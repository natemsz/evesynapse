package app

// Hermetic tests for the Phase 1A multi-character foundation:
// the SSO link policy (create / move / owner-hash flagging), the
// character management page (tags, unlink guardrails, acting
// fallback), the header switcher, and token-dead parking in the
// worker. All network access is stubbed; renders must stay at
// zero outbound calls as everywhere else.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
)

// doReq drives the router and returns status, body, and any
// cookies the response set (session flows rotate the cookie).
func doReq(t *testing.T, app *Application, method, path string, form url.Values, cookies ...*http.Cookie) (int, string, []*http.Cookie) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Result().Cookies()
}

func linkFor(t *testing.T, app *Application, userID, characterID int64, ownerHash string) linkResult {
	t.Helper()
	res, err := app.linkVerifiedCharacter(context.Background(), linkCharacterInput{
		UserID:       userID,
		CharacterID:  characterID,
		Name:         "Fixture Pilot",
		Scopes:       "esi-skills.read_skills.v1",
		OwnerHash:    ownerHash,
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  mustNullTime("2999-01-01T00:00:00Z"),
	})
	if err != nil {
		t.Fatalf("link character %d: %v", characterID, err)
	}
	return res
}

// TestLinkVerifiedCharacterLifecycle covers the link policy end to
// end: first link, same-account re-login, account move, owner-hash
// change flagging, and re-verification.
func TestLinkVerifiedCharacterLifecycle(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	userA, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}

	// First link: stored, healthy, owned by A.
	res := linkFor(t, app, userA.ID, fixtureCharA, "hash-one")
	if res.Moved || res.OwnerChanged {
		t.Fatalf("first link: got %+v, want no move/owner change", res)
	}
	ch, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get character: %v", err)
	}
	if ch.UserID != userA.ID || ch.OwnerHash != "hash-one" || ch.LinkState != linkStateOK {
		t.Fatalf("first link stored %+v", ch)
	}

	// Same account, same hash: a plain re-login.
	res = linkFor(t, app, userA.ID, fixtureCharA, "hash-one")
	if res.Moved || res.OwnerChanged {
		t.Fatalf("re-login: got %+v, want no move/owner change", res)
	}

	// Fresh sign-in while signed into B: the character moves.
	res = linkFor(t, app, userB.ID, fixtureCharA, "hash-one")
	if !res.Moved || res.OwnerChanged {
		t.Fatalf("move: got %+v, want moved only", res)
	}
	ch, _ = q.GetCharacter(ctx, fixtureCharA)
	if ch.UserID != userB.ID {
		t.Fatalf("after move: user_id = %d, want %d", ch.UserID, userB.ID)
	}
	if chars, _ := q.ListCharactersByUser(ctx, userA.ID); len(chars) != 0 {
		t.Fatalf("account A still holds %d characters after move", len(chars))
	}

	// The character changed EVE accounts: flag, don't trust.
	res = linkFor(t, app, userB.ID, fixtureCharA, "hash-two")
	if !res.OwnerChanged {
		t.Fatalf("owner change: got %+v, want flagged", res)
	}
	ch, _ = q.GetCharacter(ctx, fixtureCharA)
	if ch.LinkState != linkStateOwnerChanged || ch.OwnerHash != "hash-two" {
		t.Fatalf("after owner change: state %q hash %q", ch.LinkState, ch.OwnerHash)
	}
	if characterSyncs(ch) {
		t.Fatal("owner-changed character still syncs")
	}

	// A fresh sign-in with the now-stored hash re-verifies.
	res = linkFor(t, app, userB.ID, fixtureCharA, "hash-two")
	if res.OwnerChanged {
		t.Fatalf("re-verify: got %+v, want clean", res)
	}
	ch, _ = q.GetCharacter(ctx, fixtureCharA)
	if ch.LinkState != linkStateOK || !characterSyncs(ch) {
		t.Fatalf("after re-verify: state %q", ch.LinkState)
	}
}

// TestCharactersPageSwitcherAndTags renders the management page
// (with the header switcher around it) and saves tags through the
// real form handler.
func TestCharactersPageSwitcherAndTags(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateTokenDead,
		LinkStateAt: mustNullTime("2026-10-03T00:00:00Z"),
		CharacterID: fixtureCharB,
	}); err != nil {
		t.Fatalf("park character B: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("GET /characters/: status %d", code)
	}
	mustContain(t, "/characters/", body,
		"Fixture Ceo", "Second Pilot",
		"Linked", "Re-link needed",
		"Switch character", "Manage characters",
		"/characters/switch?character=",
		"Characters", // nav menu entry
	)

	// Tag save round-trip through the form handler.
	form := url.Values{
		"character_id": {fmt.Sprint(fixtureCharA)},
		"tags":         {"  PI   alt,  Trader "},
	}
	code, _, _ = doReq(t, app, http.MethodPost, "/characters/tags", form, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("POST /characters/tags: status %d", code)
	}
	ch, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get character: %v", err)
	}
	if ch.Tags != "PI alt, Trader" {
		t.Fatalf("stored tags %q, want normalized %q", ch.Tags, "PI alt, Trader")
	}
	code, body = getPage(t, app, cookie, "/characters/")
	if code != http.StatusOK || !strings.Contains(body, "PI alt, Trader") {
		t.Fatalf("tags not rendered after save (status %d)", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestCharacterSwitchFlow switches the acting character through
// the switcher endpoint and checks the session follows.
func TestCharacterSwitchFlow(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// The management page marks the session character active.
	_, body := getPage(t, app, cookie, "/characters/")
	activeA := fmt.Sprintf("href=\"/characters/switch?character=%d\" class=\"active\"", fixtureCharA)
	if !strings.Contains(body, activeA) {
		t.Fatal("acting character not marked in the switcher")
	}

	// Switch to B (plain GET, like the switcher links). Session
	// data lives server-side under the same token, so the same
	// cookie carries the new pick.
	code, _, _ := doReq(t, app, http.MethodGet, "/characters/switch?character="+fmt.Sprint(fixtureCharB), nil, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("switch: status %d", code)
	}

	// The topbar now greets B and the switcher marks it active.
	code, body = getPage(t, app, cookie, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("GET /characters/ after switch: status %d", code)
	}
	activeB := fmt.Sprintf("href=\"/characters/switch?character=%d\" class=\"active\"", fixtureCharB)
	if !strings.Contains(body, "Signed in as Second Pilot") || !strings.Contains(body, activeB) {
		t.Fatal("session did not follow the switch to Second Pilot")
	}

	// Switching to a character the account doesn't own is a no-op.
	code, _, _ = doReq(t, app, http.MethodGet, "/characters/switch?character=123456789", nil, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("foreign switch: status %d", code)
	}
	_, body = getPage(t, app, cookie, "/characters/")
	if !strings.Contains(body, activeB) {
		t.Fatal("foreign switch changed the acting character")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestCharacterUnlinkFlow exercises the confirm + delete path,
// the acting-character fallback, and the last-character empty
// state.
func TestCharacterUnlinkFlow(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{TotalSP: 42})
	seedSnapshot(t, q, fixtureCharB, esi.SnapSkills, esi.Skills{TotalSP: 7})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// Confirm step renders before anything destructive.
	code, body := getPage(t, app, cookie, "/characters/?confirm_unlink="+fmt.Sprint(fixtureCharA))
	if code != http.StatusOK || !strings.Contains(body, "Unlink Fixture Ceo?") {
		t.Fatalf("confirm step missing (status %d)", code)
	}
	if _, err := q.GetCharacter(ctx, fixtureCharA); err != nil {
		t.Fatal("character deleted by the confirm step")
	}

	// The delete itself (form POST, as the confirm button sends).
	form := url.Values{"character_id": {fmt.Sprint(fixtureCharA)}}
	code, _, _ = doReq(t, app, http.MethodPost, "/characters/unlink", form, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("POST unlink: status %d", code)
	}
	if _, err := q.GetCharacter(ctx, fixtureCharA); err == nil {
		t.Fatal("character row survived unlink")
	}
	if snaps, _ := q.ListSnapshotsByCharacter(ctx, fixtureCharA); len(snaps) != 0 {
		t.Fatalf("snapshots survived unlink: %d rows", len(snaps))
	}
	if _, err := q.GetCharacter(ctx, fixtureCharB); err != nil {
		t.Fatal("other character lost in unlink")
	}

	// Acting fell back to the remaining character.
	code, body = getPage(t, app, cookie, "/characters/")
	activeB := fmt.Sprintf("href=\"/characters/switch?character=%d\" class=\"active\"", fixtureCharB)
	if code != http.StatusOK || !strings.Contains(body, "Signed in as Second Pilot") || !strings.Contains(body, activeB) {
		t.Fatalf("acting fallback failed (status %d)", code)
	}

	// Unlink the last character: clean empty state, still 200.
	form = url.Values{"character_id": {fmt.Sprint(fixtureCharB)}}
	code, _, _ = doReq(t, app, http.MethodPost, "/characters/unlink", form, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("POST unlink last: status %d", code)
	}
	code, body = getPage(t, app, cookie, "/characters/")
	if code != http.StatusOK || !strings.Contains(body, "No characters linked yet") {
		t.Fatalf("empty state missing (status %d)", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// pathTransport records request paths and answers 500 to all.
type pathTransport struct {
	mu    sync.Mutex
	paths []string
}

func (s *pathTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.paths = append(s.paths, req.URL.Path)
	s.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"fixture"}`)),
	}, nil
}

func (s *pathTransport) saw(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.paths {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// TestTokenDeadClassificationAndWorkerSkip: definitive credential
// failures park a character; the worker then never fetches for it,
// while healthy characters keep cycling.
func TestTokenDeadClassificationAndWorkerSkip(t *testing.T) {
	if !isDefinitiveTokenFailure(&oauth2.RetrieveError{ErrorCode: "invalid_grant"}) {
		t.Error("invalid_grant not classified as definitive")
	}
	if !isDefinitiveTokenFailure(fmt.Errorf("wrap: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"})) {
		t.Error("wrapped invalid_grant not classified as definitive")
	}
	if !isDefinitiveTokenFailure(&esi.StatusError{Method: "GET", Path: "/x/", Code: http.StatusUnauthorized}) {
		t.Error("ESI 401 not classified as definitive")
	}
	if isDefinitiveTokenFailure(fmt.Errorf("dial tcp: connection refused")) {
		t.Error("network error misclassified as definitive")
	}
	if isDefinitiveTokenFailure(&esi.StatusError{Method: "GET", Path: "/x/", Code: http.StatusForbidden}) {
		t.Error("ESI 403 (scope/role) misclassified as token death")
	}

	transport := &pathTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Healthy Pilot")
	dead := seedCharacter(t, q, user.ID, fixtureCharB, "Dead Pilot")

	// markCharacterTokenDead only parks healthy links.
	app.markCharacterTokenDead(ctx, dead.CharacterID)
	ch, _ := q.GetCharacter(ctx, fixtureCharB)
	if ch.LinkState != linkStateTokenDead || characterSyncs(ch) {
		t.Fatalf("dead character state %q, syncs=%v", ch.LinkState, characterSyncs(ch))
	}

	// An owner_changed flag is stronger and is not overwritten.
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateOwnerChanged,
		LinkStateAt: mustNullTime("2026-10-03T00:00:00Z"),
		CharacterID: fixtureCharA,
	}); err != nil {
		t.Fatalf("flag owner change: %v", err)
	}
	app.markCharacterTokenDead(ctx, fixtureCharA)
	ch, _ = q.GetCharacter(ctx, fixtureCharA)
	if ch.LinkState != linkStateOwnerChanged {
		t.Fatalf("owner flag overwritten: %q", ch.LinkState)
	}
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateOK,
		LinkStateAt: sql.NullTime{},
		CharacterID: fixtureCharA,
	}); err != nil {
		t.Fatalf("restore healthy: %v", err)
	}

	// One full cycle: the dead character must not appear in any
	// request path; the healthy one is fetched as usual.
	app.refreshCycle(ctx)
	deadPath := fmt.Sprintf("/characters/%d/", fixtureCharB)
	if transport.saw(deadPath) {
		t.Fatalf("worker fetched for parked character: %v", transport.paths)
	}
	if !transport.saw(fmt.Sprintf("/characters/%d/", fixtureCharA)) {
		t.Fatal("worker never fetched for the healthy character")
	}
}

// TestMigration009Reopen proves the schema bootstrap is
// idempotent: a second openDB over the same database applies
// nothing twice.
func TestMigration009Reopen(t *testing.T) {
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := openDB(context.Background(), dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var cols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'characters' AND column_name IN ('owner_hash', 'tags', 'link_state', 'link_state_at')`).Scan(&cols); err != nil {
		t.Fatalf("columns: %v", err)
	}
	if cols != 4 {
		t.Fatalf("foundation columns: %d, want 4", cols)
	}
	conn.Close()
	pool.Close()
	conn, pool, err = openDB(context.Background(), dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	conn.Close()
	pool.Close()
}

// TestResolveSignInUser covers which account a verified sign-in
// lands on: a fresh session adopts the account a returning
// character is already linked to (an expired session must not split
// a user's pilots onto a new account holding only that one
// character), a first-ever character creates an account, and a
// held session account always wins.
func TestResolveSignInUser(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	userA, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	linkFor(t, app, userA.ID, fixtureCharA, "hash-one")

	// Fresh session, returning character: adopt A, do not mint.
	// A sign-in whose token carries no owner hash proves nothing
	// about a transfer and adopts A all the same.
	for _, hash := range []string{"hash-one", ""} {
		got, err := app.resolveSignInUser(ctx, 0, fixtureCharA, hash)
		if err != nil {
			t.Fatalf("resolve returning (hash %q): %v", hash, err)
		}
		if got != userA.ID {
			t.Fatalf("resolve returning (hash %q): got user %d, want %d", hash, got, userA.ID)
		}
	}

	// Held session account wins (link-another-character path).
	userB, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	got, err := app.resolveSignInUser(ctx, userB.ID, fixtureCharA, "hash-one")
	if err != nil {
		t.Fatalf("resolve with session account: %v", err)
	}
	if got != userB.ID {
		t.Fatalf("resolve with session account: got user %d, want %d", got, userB.ID)
	}

	// First-ever character: a new account appears.
	got, err = app.resolveSignInUser(ctx, 0, fixtureCharB, "hash-other")
	if err != nil {
		t.Fatalf("resolve first-ever: %v", err)
	}
	if got == 0 || got == userA.ID || got == userB.ID {
		t.Fatalf("resolve first-ever: got user %d, want a fresh account", got)
	}
}

// TestTransferredCharacterNeverInheritsTheAccount: a character that
// changed EVE accounts (sold, traded) signs in from a fresh session.
// Its new owner must not land on the account the previous owner
// built — that would hand over every other character linked there.
// They get a fresh account, the character moves onto it healthy, and
// the previous owner's account keeps everything else.
func TestTransferredCharacterNeverInheritsTheAccount(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	seller, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create seller account: %v", err)
	}
	linkFor(t, app, seller.ID, fixtureCharA, "seller-hash") // the character that gets sold
	linkFor(t, app, seller.ID, fixtureCharB, "seller-hash") // one the seller keeps

	// The buyer signs the character in from a fresh session.
	buyerID, err := app.resolveSignInUser(ctx, 0, fixtureCharA, "buyer-hash")
	if err != nil {
		t.Fatalf("resolve transferred character: %v", err)
	}
	if buyerID == 0 || buyerID == seller.ID {
		t.Fatalf("transferred character resolved to user %d; the seller's account is %d and must not be adopted", buyerID, seller.ID)
	}
	res := linkFor(t, app, buyerID, fixtureCharA, "buyer-hash")
	if !res.Moved || !res.OwnerChanged {
		t.Fatalf("transfer link: got %+v, want moved and owner-changed", res)
	}

	sold, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get transferred character: %v", err)
	}
	if sold.UserID != buyerID || sold.OwnerHash != "buyer-hash" {
		t.Fatalf("transferred character stored on user %d with hash %q", sold.UserID, sold.OwnerHash)
	}
	// Nothing left to re-verify: the new owner just proved control
	// and the character has left the old account.
	if sold.LinkState != linkStateOK {
		t.Fatalf("transferred character state %q, want %q", sold.LinkState, linkStateOK)
	}

	// The buyer's account holds that one character; the seller's
	// keeps the other.
	if chars, _ := q.ListCharactersByUser(ctx, buyerID); len(chars) != 1 || chars[0].CharacterID != fixtureCharA {
		t.Fatalf("buyer's account holds %+v, want only the transferred character", chars)
	}
	if chars, _ := q.ListCharactersByUser(ctx, seller.ID); len(chars) != 1 || chars[0].CharacterID != fixtureCharB {
		t.Fatalf("seller's account holds %+v, want only the character they kept", chars)
	}

	// The buyer coming back later lands on their own account.
	again, err := app.resolveSignInUser(ctx, 0, fixtureCharA, "buyer-hash")
	if err != nil {
		t.Fatalf("resolve returning buyer: %v", err)
	}
	if again != buyerID {
		t.Fatalf("returning buyer resolved to user %d, want %d", again, buyerID)
	}
}

// TestSessionSlidingRenewal covers the sliding session: a
// signed-in session near its deadline is renewed (token rotated,
// deadline pushed back to a full sessionLifetime), so an active
// user is not signed out simply for visiting daily.
func TestSessionSlidingRenewal(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.sessions.Lifetime = sessionLifetime

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	linkFor(t, app, user.ID, fixtureCharA, "hash-one")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Char A")

	// Age the session: an hour from its deadline.
	lctx, err := app.sessions.Load(ctx, cookie.Value)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	app.sessions.SetDeadline(lctx, time.Now().Add(time.Hour))
	if _, _, err := app.sessions.Commit(lctx); err != nil {
		t.Fatalf("age session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("aged session page: got %d", rec.Code)
	}
	var rotated string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "evesynapse_session" && c.Value != "" && c.Value != cookie.Value {
			rotated = c.Value
		}
	}
	if rotated == "" {
		t.Fatalf("aged session was not renewed (cookie token unchanged)")
	}
	var daysLeft float64
	if err := conn.QueryRow(`SELECT EXTRACT(EPOCH FROM (expiry - now())) / 86400.0 FROM sessions WHERE token = $1`, rotated).Scan(&daysLeft); err != nil {
		t.Fatalf("read renewed expiry: %v", err)
	}
	if daysLeft < 29 {
		t.Fatalf("renewed session expires in %.2f days, want ~30", daysLeft)
	}
}
