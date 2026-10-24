package app

// Journal party routing: journal counterparties arrive with an
// ESI party_type, and the worker harvests must honor it --
// corporations and alliances note org wants, only characters
// join the character-name/pilot harvests. These tests pin that
// split for both harvests (the name warmer and the pilot orbit),
// proving a typed corporation never reaches /characters/{id}/.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"evesynapse/internal/esi"
)

// journalPartyTransport records every request path and answers
// the one character endpoint with a name payload, so a test can
// see exactly which IDs the harvest tried to resolve as pilots.
type journalPartyTransport struct {
	mu    sync.Mutex
	paths []string
}

func (s *journalPartyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.paths = append(s.paths, req.URL.Path)
	s.mu.Unlock()
	body := `{}`
	if strings.HasPrefix(req.URL.Path, "/characters/") {
		body = `{"name":"Fixture Counterparty"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *journalPartyTransport) asked(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.paths {
		if p == path {
			return true
		}
	}
	return false
}

const (
	journalCharParty     = int64(1000052)
	journalCorpParty     = int64(613933833) // a modern corporation ID: passes the old >= 90M guess
	journalAllianceParty = int64(99000001)
)

// TestWarmCharacterNamesJournalPartyRouting seeds a wallet
// journal whose parties are a character, a corporation, and an
// alliance, then runs the name warmer: the character warms, the
// other two note org wants and are never asked about as pilots.
func TestWarmCharacterNamesJournalPartyRouting(t *testing.T) {
	transport := &journalPartyTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{
		{ID: 1, RefType: "player_trading", FirstPartyID: journalCharParty, FirstPartyType: "character", SecondPartyID: journalCorpParty, SecondPartyType: "corporation", Amount: 100},
		{ID: 2, RefType: "bounty_prizes", FirstPartyID: journalAllianceParty, FirstPartyType: "alliance", SecondPartyID: journalCharParty, SecondPartyType: "character", Amount: 50},
	})

	app.warmCharacterNames(ctx, ch, &warmBudget{left: 10})

	if name, ok := app.esi.CachedCharacterName(journalCharParty); !ok || name == "" {
		t.Error("character journal party was not warmed")
	}
	if transport.asked("/characters/613933833/") {
		t.Error("corporation journal party was fetched through the character endpoint")
	}
	if transport.asked("/characters/99000001/") {
		t.Error("alliance journal party was fetched through the character endpoint")
	}
	if _, err := q.GetCorporationRecord(ctx, journalCorpParty); err != nil {
		t.Errorf("corporation journal party noted no corporation want: %v", err)
	}
	if _, err := q.GetAllianceRecord(ctx, journalAllianceParty); err != nil {
		t.Errorf("alliance journal party noted no alliance want: %v", err)
	}
}

// TestPilotCounterpartyJournalRouting runs the pilot-orbit
// harvest over character and corporation journals: typed
// corporations/alliances stay out of the pilot set (and note org
// wants), typed characters land in it.
func TestPilotCounterpartyJournalRouting(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{
		{ID: 1, RefType: "player_trading", FirstPartyID: journalCharParty, FirstPartyType: "character", SecondPartyID: journalCorpParty, SecondPartyType: "corporation", Amount: 100},
		{ID: 2, RefType: "bounty_prizes", FirstPartyID: journalAllianceParty, FirstPartyType: "alliance", SecondPartyID: journalCharParty, SecondPartyType: "character", Amount: 50},
	})
	const corpJournalCorp = int64(613933834)
	const corpJournalChar = int64(1000053)
	seedSnapshot(t, q, fixtureCharA, esi.CorpJournalKind(1), esi.CorpJournal{
		{ID: 3, RefType: "corporation_account_withdrawal", FirstPartyID: corpJournalCorp, FirstPartyType: "corporation", SecondPartyID: corpJournalChar, SecondPartyType: "character", Amount: 25},
	})

	out := make(map[int64]bool)
	app.pilotCounterpartyIDs(ctx, fixtureCharA, out)

	if !out[journalCharParty] || !out[corpJournalChar] {
		t.Errorf("character journal parties missing from pilot set: %v", out)
	}
	for _, id := range []int64{journalCorpParty, journalAllianceParty, corpJournalCorp} {
		if out[id] {
			t.Errorf("non-character journal party %d landed in the pilot set", id)
		}
	}
	if _, err := q.GetCorporationRecord(ctx, journalCorpParty); err != nil {
		t.Errorf("character-journal corporation noted no corporation want: %v", err)
	}
	if _, err := q.GetCorporationRecord(ctx, corpJournalCorp); err != nil {
		t.Errorf("corp-journal corporation noted no corporation want: %v", err)
	}
	if _, err := q.GetAllianceRecord(ctx, journalAllianceParty); err != nil {
		t.Errorf("alliance journal party noted no alliance want: %v", err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("pilot counterparty harvest made %d outbound calls, want 0", got)
	}
}

// placeWarmTransport records request counts per path and answers
// the station/system endpoints with per-path statuses, so a test
// can see exactly which place IDs the warm pass asked ESI about.
type placeWarmTransport struct {
	mu       sync.Mutex
	counts   map[string]int
	statuses map[string]int // path -> status; absent = 200
}

func newPlaceWarmTransport() *placeWarmTransport {
	return &placeWarmTransport{counts: map[string]int{}, statuses: map[string]int{}}
}

func (s *placeWarmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	s.mu.Lock()
	s.counts[path]++
	status := s.statuses[path]
	s.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	body := `{"error":"fixture failure"}`
	if status == http.StatusOK {
		body = `{"name":"Fixture Place"}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *placeWarmTransport) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[path]
}

// TestWarmPlaceNamesSkipsInvalidAndSettlesDead pins the place
// warm pass's junk filter: an ID outside its kind's space is
// never fetched, a definitive 404 settles the ID so the next
// pass asks nothing, and a transient failure settles nothing
// and is retried.
func TestWarmPlaceNamesSkipsInvalidAndSettlesDead(t *testing.T) {
	transport := newPlaceWarmTransport()
	transport.statuses["/universe/stations/60000001/"] = http.StatusNotFound
	transport.statuses["/universe/stations/60000002/"] = http.StatusInternalServerError
	app, _, _ := buildCorpTestApp(t, transport)
	ctx := context.Background()

	places := map[int64]string{
		0:        "station", // unresolved location: never enqueue
		30000142: "station", // a system ID wearing the station kind
		60000001: "station", // definitive 404: settles
		60000002: "station", // transient 500: settles nothing
		30000143: "solar_system",
	}
	budget := &warmBudget{left: 100}
	app.warmMissingPlaceNames(ctx, places, budget)

	if got := transport.count("/universe/stations/0/"); got != 0 {
		t.Fatalf("station 0 fetched %d times, want 0", got)
	}
	if got := transport.count("/universe/stations/30000142/"); got != 0 {
		t.Fatalf("system ID fetched as a station %d times, want 0", got)
	}
	if got := transport.count("/universe/stations/60000001/"); got != 1 {
		t.Fatalf("dead station fetched %d times, want 1", got)
	}
	if got := transport.count("/universe/systems/30000143/"); got != 1 {
		t.Fatalf("system fetched %d times, want 1", got)
	}
	flakyBefore := transport.count("/universe/stations/60000002/")
	if flakyBefore == 0 {
		t.Fatal("transient station was never fetched")
	}

	// Next cycle, same wants re-derived from the stored
	// snapshots: the settled 404 and the resolved system are not
	// re-asked; the transient failure is.
	app.warmMissingPlaceNames(ctx, places, budget)
	if got := transport.count("/universe/stations/60000001/"); got != 1 {
		t.Fatalf("settled 404 re-asked: %d fetches, want 1", got)
	}
	if got := transport.count("/universe/systems/30000143/"); got != 1 {
		t.Fatalf("resolved system re-asked: %d fetches, want 1", got)
	}
	if got := transport.count("/universe/stations/60000002/"); got <= flakyBefore {
		t.Fatalf("transient failure not retried: %d fetches, want more than %d", got, flakyBefore)
	}
}
