package app

// Tests for the sign-up policy (auth_signup.go): who may create an
// account when EVE_ALLOWED_*_IDS are set, who is never asked, and
// that a restriction which cannot be read or checked refuses rather
// than opening the door.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// affiliationTransport answers ESI's character affiliation lookup
// with one fixed corporation and alliance, and counts the calls.
type affiliationTransport struct {
	corporationID int64
	allianceID    int64
	fail          bool
	calls         atomic.Int64
}

func (s *affiliationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	if s.fail || req.URL.Path != "/characters/affiliation/" {
		return respond(http.StatusInternalServerError, `{"error":"unavailable"}`)
	}
	raw, _ := io.ReadAll(req.Body)
	id := strings.Trim(string(raw), "[] \n")
	body := `[{"character_id":` + id + `,"corporation_id":` + strconv.FormatInt(s.corporationID, 10)
	if s.allianceID != 0 {
		body += `,"alliance_id":` + strconv.FormatInt(s.allianceID, 10)
	}
	return respond(http.StatusOK, body+`}]`)
}

func TestLoadSignUpPolicy(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	open := loadSignUpPolicy(env(nil))
	if open.err != nil || open.restricted() {
		t.Fatalf("no lists set: err %v, restricted %v; want open and no error", open.err, open.restricted())
	}

	p := loadSignUpPolicy(env(map[string]string{
		"EVE_ALLOWED_CHARACTER_IDS":   "90000001, 90000002",
		"EVE_ALLOWED_CORPORATION_IDS": "98000001",
		"EVE_ALLOWED_ALLIANCE_IDS":    " 99000001 ,",
	}))
	if p.err != nil {
		t.Fatalf("valid lists: %v", p.err)
	}
	if !p.restricted() || !p.characterIDs[90000001] || !p.characterIDs[90000002] ||
		!p.corporationIDs[98000001] || !p.allianceIDs[99000001] {
		t.Fatalf("valid lists parsed as %+v", p)
	}

	// A list that is set but unreadable must be an error, never an
	// empty list: an empty list would mean "anyone may sign up".
	for _, bad := range []string{"98000001;98000002", "my-corp", "98000001,0", "-5", "98000001x"} {
		p := loadSignUpPolicy(env(map[string]string{"EVE_ALLOWED_CORPORATION_IDS": bad}))
		if p.err == nil {
			t.Errorf("EVE_ALLOWED_CORPORATION_IDS=%q was accepted", bad)
		} else if !strings.Contains(p.err.Error(), "EVE_ALLOWED_CORPORATION_IDS") {
			t.Errorf("error %q does not name the variable", p.err)
		}
	}
}

func TestSignUpPolicy(t *testing.T) {
	const (
		listedChar  = int64(90000001)
		corpMember  = int64(90000002)
		outsider    = int64(90000003)
		adminChar   = int64(90000004)
		allowedCorp = int64(98000001)
		otherCorp   = int64(98000002)
		allowedAlly = int64(99000001)
	)
	ctx := context.Background()

	build := func(t *testing.T, transport *affiliationTransport, policy signUpPolicy) *Application {
		t.Helper()
		app, _, _ := buildCorpTestApp(t, transport)
		app.cfg.signUp = policy
		app.cfg.adminCharIDs = map[int64]bool{adminChar: true}
		return app
	}
	userCount := func(t *testing.T, app *Application) int {
		t.Helper()
		users, err := app.queries.ListUsers(ctx)
		if err != nil {
			t.Fatalf("list users: %v", err)
		}
		return len(users)
	}

	t.Run("open by default", func(t *testing.T) {
		transport := &affiliationTransport{corporationID: otherCorp}
		app := build(t, transport, signUpPolicy{})
		if _, err := app.resolveSignInUser(ctx, 0, outsider, "hash"); err != nil {
			t.Fatalf("sign-up with no lists set: %v", err)
		}
		if got := transport.calls.Load(); got != 0 {
			t.Fatalf("an open instance made %d affiliation lookup(s)", got)
		}
	})

	t.Run("character list", func(t *testing.T) {
		transport := &affiliationTransport{corporationID: otherCorp}
		app := build(t, transport, signUpPolicy{characterIDs: map[int64]bool{listedChar: true}})
		if _, err := app.resolveSignInUser(ctx, 0, listedChar, "hash"); err != nil {
			t.Fatalf("listed character: %v", err)
		}
		// The named administrators are always welcome.
		if _, err := app.resolveSignInUser(ctx, 0, adminChar, "hash"); err != nil {
			t.Fatalf("admin character: %v", err)
		}
		before := userCount(t, app)
		if _, err := app.resolveSignInUser(ctx, 0, outsider, "hash"); !errors.Is(err, errSignUpNotAllowed) {
			t.Fatalf("unlisted character: err = %v, want errSignUpNotAllowed", err)
		}
		if after := userCount(t, app); after != before {
			t.Fatalf("a refused sign-up created an account (%d → %d)", before, after)
		}
		// With only a character list there is nothing to look up.
		if got := transport.calls.Load(); got != 0 {
			t.Fatalf("a character-only list made %d affiliation lookup(s)", got)
		}
	})

	t.Run("corporation and alliance lists", func(t *testing.T) {
		policy := signUpPolicy{
			corporationIDs: map[int64]bool{allowedCorp: true},
			allianceIDs:    map[int64]bool{allowedAlly: true},
		}
		inCorp := build(t, &affiliationTransport{corporationID: allowedCorp}, policy)
		if _, err := inCorp.resolveSignInUser(ctx, 0, corpMember, "hash"); err != nil {
			t.Fatalf("member of an allowed corporation: %v", err)
		}
		inAlliance := build(t, &affiliationTransport{corporationID: otherCorp, allianceID: allowedAlly}, policy)
		if _, err := inAlliance.resolveSignInUser(ctx, 0, corpMember, "hash"); err != nil {
			t.Fatalf("member of an allowed alliance: %v", err)
		}
		elsewhere := build(t, &affiliationTransport{corporationID: otherCorp}, policy)
		if _, err := elsewhere.resolveSignInUser(ctx, 0, outsider, "hash"); !errors.Is(err, errSignUpNotAllowed) {
			t.Fatalf("member of neither: err = %v, want errSignUpNotAllowed", err)
		}
	})

	t.Run("a lookup that fails refuses", func(t *testing.T) {
		app := build(t, &affiliationTransport{fail: true}, signUpPolicy{corporationIDs: map[int64]bool{allowedCorp: true}})
		before := userCount(t, app)
		if _, err := app.resolveSignInUser(ctx, 0, corpMember, "hash"); !errors.Is(err, errSignUpUnverified) {
			t.Fatalf("lookup failure: err = %v, want errSignUpUnverified", err)
		}
		if after := userCount(t, app); after != before {
			t.Fatalf("an unverified sign-up created an account (%d → %d)", before, after)
		}
	})

	t.Run("existing accounts are not asked", func(t *testing.T) {
		transport := &affiliationTransport{corporationID: otherCorp}
		app := build(t, transport, signUpPolicy{characterIDs: map[int64]bool{listedChar: true}})
		member, err := app.queries.CreateUser(ctx)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		linkFor(t, app, member.ID, outsider, "hash")

		// Returning with a character already linked: its account.
		if got, err := app.resolveSignInUser(ctx, 0, outsider, "hash"); err != nil || got != member.ID {
			t.Fatalf("returning character: user %d, %v; want %d", got, err, member.ID)
		}
		// Linking an alt from inside a signed-in account.
		if got, err := app.resolveSignInUser(ctx, member.ID, corpMember, "hash"); err != nil || got != member.ID {
			t.Fatalf("alt linked from a signed-in account: user %d, %v; want %d", got, err, member.ID)
		}
		// But a transferred character starts a new account, and
		// that one is asked.
		if _, err := app.resolveSignInUser(ctx, 0, outsider, "new-owner-hash"); !errors.Is(err, errSignUpNotAllowed) {
			t.Fatalf("transferred character with an unlisted new owner: err = %v, want errSignUpNotAllowed", err)
		}
	})
}

func TestSignUpErrorsHaveFriendlyMessages(t *testing.T) {
	for _, code := range []string{"notallowed", "allowcheck"} {
		if friendlyLoginError(code) == "" {
			t.Errorf("sign-in error code %q has no message for the home page", code)
		}
	}
}
