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
