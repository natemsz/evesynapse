package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/intelanalyze"
)

// The fixture pilots: the production fused blob's roster, the
// spaced roster's extra names, the fragment pilots the plain
// reading used to tag inside longer names, and a mini roster
// nobody has looked up yet. IDs and corporations are fixture
// values; the names are the production ones.
type fusedPilot struct {
	name string
	id   int64
	corp int64
}

var fusedRoster = []fusedPilot{
	{"Altorr", 1056608666, 98000999},
	{"Astria Starbourne", 2124784878, 98000999},
	{"Benedict Socrates", 94134438, 98000777},
	{"BOB Lexikdark", 2124000101, 98000999},
	{"Burzrujat", 519225918, 98000301},
	{"Caldari Citizen 92702902", 92702902, 98000777},
	{"Ebony Shift", 2124617317, 98000999},
	{"Galliane Dacella", 2124000102, 98000999},
	{"Goosius Tal", 2124000103, 98000777},
	{"H8t3R Baboli", 2124000104, 98000999},
	{"H8T3R Duffes", 2124000105, 98000999},
	{"Madeveda", 2124000106, 98000777},
	{"H8T3R Visteen", 2124000107, 98000999},
	{"Ken117", 2124000108, 98000777},
	{"Laura The Bora", 2121842534, 98000999},
	{"Mineritie", 2124000109, 98000999},
	{"MR JAZZM", 2124000110, 98000777},
	{"Nassendra Starbourne", 2124000111, 98000999},
	{"natebo", 2124000112, 98000301},
	{"M2Ray Dainy", 2124000113, 98000999},
	{"Red Ice", 480609586, 98000999},
	{"Khamez", 2124000114, 98000777},
	{"Stevinio", 2124000115, 98000999},
	{"Strokes", 2124000116, 98000999},
	{"Arbahd", 2124000117, 98000777},
	{"URKILLA", 2124000118, 98000999},
	{"Vacker", 2124000119, 98000999},
	{"Vild", 2124000120, 98000777},
}

var spacedExtra = []fusedPilot{
	{"FrankLinSHein", 2124000201, 98000999},
	{"Locutus Harry", 2124000202, 98000777},
	{"Davaham", 2124000203, 98000999},
	{"HYC-0824", 2124388820, 98000777},
	{"JuicyFruits043", 2124000204, 98000999},
	{"Lexikdark", 2124000205, 98000777},
	{"longgen2", 2124000206, 98000999},
	{"TheRealKOrel", 2124000207, 98000777},
	{"XYZ-1978", 2122517217, 98000999},
	{"Zrebenar", 2124000208, 98000777},
}

var fragmentPilots = []fusedPilot{
	{"Citizen", 95498378, 98000777},
	{"Ice", 611348333, 98000777},
	{"Red", 263160602, 98000777},
}

var miniRoster = []fusedPilot{
	{"Pavel Tsukov", 2124000301, 98000999},
	{"Rho Banu", 2124000302, 98000777},
	{"Sef Amari", 2124000303, 98000999},
	{"Tovak Lin", 2124000304, 98000777},
	{"Uma Ferro", 2124000305, 98000999},
	{"Wex Talbot", 2124000306, 98000777},
}

// The production texts, verbatim: the fused blob an Axon report
// carried, and the same roster class pasted spaced by hand.
const fusedBlob = "AltorrAstria StarbourneBenedict SocratesBOB LexikdarkBurzrujatCaldari Citizen 92702902Ebony ShiftGalliane DacellaGoosius TalH8t3R BaboliH8T3R Duffes MadevedaH8T3R VisteenKen117Laura The BoraMineritieMR JAZZMNassendra StarbournenateboM2Ray DainyRED ICE KhamezStevinioStrokes ArbahdURKILLAVacker VildXY"

const spacedRoster = "Altorr Benedict Socrates Burzrujat Caldari Citizen 92702902 FrankLinSHein Goosius Tal Locutus Harry Davaham HYC-0824 JuicyFruits043 Ken117 Laura The Bora Lexikdark longgen2 Mineritie MR JAZZM RED ICE Khamez Strokes Arbahd TheRealKOrel URKILLA Vacker Vild XYZ-1978 Zrebenar"

const miniBlob = "Pavel TsukovRho BanuSef AmariTovak LinUma FerroWex Talbot"

// fusedESI is the fixture EVE: exact name resolution and
// affiliations for the fixture pilots, nothing else.
type fusedESI struct {
	byName map[string]fusedPilot
	byID   map[int64]fusedPilot
}

func newFusedESI() *fusedESI {
	s := &fusedESI{byName: map[string]fusedPilot{}, byID: map[int64]fusedPilot{}}
	for _, group := range [][]fusedPilot{fusedRoster, spacedExtra, fragmentPilots, miniRoster} {
		for _, p := range group {
			s.byName[strings.ToLower(p.name)] = p
			s.byID[p.id] = p
		}
	}
	return s
}

func (s *fusedESI) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(status int, v any) (*http.Response, error) {
		b, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	}
	body, _ := io.ReadAll(req.Body)
	switch req.URL.Path {
	case "/universe/ids/":
		var names []string
		_ = json.Unmarshal(body, &names)
		chars := []map[string]any{}
		for _, n := range names {
			if p, ok := s.byName[strings.ToLower(n)]; ok {
				chars = append(chars, map[string]any{"id": p.id, "name": p.name})
			}
		}
		return respond(http.StatusOK, map[string]any{"characters": chars})
	case "/characters/affiliation/":
		var ids []int64
		_ = json.Unmarshal(body, &ids)
		rows := []map[string]any{}
		for _, id := range ids {
			corp := int64(98000777)
			if p, ok := s.byID[id]; ok {
				corp = p.corp
			}
			rows = append(rows, map[string]any{"character_id": id, "corporation_id": corp, "alliance_id": 0})
		}
		return respond(http.StatusOK, rows)
	}
	return respond(http.StatusInternalServerError, map[string]any{"error": "unexpected path " + req.URL.Path})
}

// TestIntelFusedRoster: a local roster whose lines fused on the
// way in is re-read as the roster it is -- segmented against the
// verified pilot names, stored as a local reading with the
// roster sentence for a headline -- and a spaced roster tags its
// full names, never the fragments inside them. The same roster
// arriving again, in either face, is one report (its pilot set
// is its identity). A roster nobody has looked up yet waits for
// the drain and is re-read there. Plain prose is never re-read.
// A scan paste naming no system falls back to the poster's own
// current system.
func TestIntelFusedRoster(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, newFusedESI())
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004759, '1DQ1-A', 10000060, -0.38), (30000142, 'Jita', 10000002, 0.95) ON CONFLICT DO NOTHING`)
	const (
		sysID   = int64(30004759)
		poster  = int64(519225918) // Burzrujat
		ownCorp = int64(98000301)
	)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000301, "Channel Owner")
	seedCharacter(t, q, u.ID, poster, "Burzrujat")
	for _, id := range []int64{90000301, poster} {
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: id, CorporationID: ownCorp, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Fused", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The verified dictionary: every roster name and the
	// fragments -- but not the mini roster, which only EVE knows.
	for _, group := range [][]fusedPilot{fusedRoster, spacedExtra, fragmentPilots} {
		for _, p := range group {
			if err := q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: strings.ToLower(p.name), CharacterID: p.id, Name: p.name, CheckedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{CharacterID: p.id, CorporationID: p.corp, CheckedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	byName := map[string]fusedPilot{}
	for _, group := range [][]fusedPilot{fusedRoster, spacedExtra, miniRoster} {
		for _, p := range group {
			byName[p.name] = p
		}
	}
	idsOf := func(names ...string) []int64 {
		ids := make([]int64, len(names))
		for i, n := range names {
			ids[i] = byName[n].id
		}
		return ids
	}
	reportByID := func(id int64) db.ListIntelReportsRow {
		t.Helper()
		rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("report %d not in the channel", id)
		return db.ListIntelReportsRow{}
	}
	reportCount := func() int {
		t.Helper()
		rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	pilotIDs := func(r db.ListIntelReportsRow) []int64 {
		t.Helper()
		var a intelanalyze.Analysis
		if err := json.Unmarshal([]byte(r.Analysis), &a); err != nil {
			t.Fatalf("analysis of report %d: %v", r.ID, err)
		}
		ids := make([]int64, len(a.Pilots))
		for i, p := range a.Pilots {
			ids[i] = p.ID
		}
		return ids
	}
	sameIDs := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	insert := func(source, text string) (int64, error) {
		t.Helper()
		p := db.InsertIntelReportParams{ChannelID: cid, SystemID: sysID, AuthorUserID: u.ID,
			AuthorCharacterID: poster, Reporter: "Burzrujat", Text: text, Pilots: text,
			Source: source, SeenAt: now, CreatedAt: now}
		return app.insertIntelReport(ctx, p, false, nil)
	}

	// The segmentation itself, on the production blob.
	seg, covered, total := app.intelNameIndex(ctx).Segment(fusedBlob)
	t.Logf("fused blob: %d pilots, coverage %d/%d letters and digits", len(seg), covered, total)
	if len(seg) != len(fusedRoster) {
		t.Fatalf("segmentation found %d pilots, want %d", len(seg), len(fusedRoster))
	}
	if !intelanalyze.PassesRosterBar(seg, covered, total) {
		t.Fatalf("segmentation under the roster bar: %d/%d", covered, total)
	}

	// (a) The fused blob, as Axon delivered it.
	idA, err := insert("axon", fusedBlob)
	if err != nil {
		t.Fatalf("insert fused: %v", err)
	}
	rowA := reportByID(idA)
	if rowA.Kind != intelanalyze.KindLocal || rowA.ScanID == "" {
		t.Errorf("fused report: kind %q scan %q, want a local reading", rowA.Kind, rowA.ScanID)
	}
	wantA := make([]int64, len(fusedRoster))
	for i, p := range fusedRoster {
		wantA[i] = p.id
	}
	if got := pilotIDs(rowA); !sameIDs(got, wantA) {
		t.Errorf("fused report pilots:\n got %v\nwant %v", got, wantA)
	}
	if !strings.HasPrefix(rowA.Text, "28 in local: 0 hostiles, 2 friendly (incl. you), 26 neutral") ||
		!strings.Contains(rowA.Text, "no scan attached, ships unknown") ||
		strings.Contains(rowA.Text, "AltorrAstria") {
		t.Errorf("fused report headline: %q", rowA.Text)
	}

	// The same roster again, fused and spaced: one report.
	if _, err := insert("axon", fusedBlob); !errors.Is(err, errIntelDuplicate) {
		t.Errorf("fused re-send: err %v, want a duplicate", err)
	}
	spacedSame := make([]string, len(fusedRoster))
	for i, p := range fusedRoster {
		spacedSame[i] = p.name
	}
	if _, err := insert("form", strings.Join(spacedSame, " ")); !errors.Is(err, errIntelDuplicate) {
		t.Errorf("spaced face of the same roster: err %v, want a duplicate", err)
	}
	if n := reportCount(); n != 1 {
		t.Fatalf("after the re-sends the channel holds %d reports, want 1", n)
	}

	// (b) The spaced roster: full names tagged, no fragments.
	idB, err := insert("form", spacedRoster)
	if err != nil {
		t.Fatalf("insert spaced: %v", err)
	}
	rowB := reportByID(idB)
	wantB := idsOf("Altorr", "Benedict Socrates", "Burzrujat", "Caldari Citizen 92702902",
		"FrankLinSHein", "Goosius Tal", "Locutus Harry", "Davaham", "HYC-0824",
		"JuicyFruits043", "Ken117", "Laura The Bora", "Lexikdark", "longgen2",
		"Mineritie", "MR JAZZM", "Red Ice", "Khamez", "Strokes", "Arbahd",
		"TheRealKOrel", "URKILLA", "Vacker", "Vild", "XYZ-1978", "Zrebenar")
	gotB := pilotIDs(rowB)
	if !sameIDs(gotB, wantB) {
		t.Errorf("spaced report pilots:\n got %v\nwant %v", gotB, wantB)
	}
	for _, f := range fragmentPilots {
		for _, id := range gotB {
			if id == f.id {
				t.Errorf("spaced report tags the fragment %q (%d)", f.name, f.id)
			}
		}
	}

	// (d) Plain prose is never re-read.
	prose := "gate is camped, two bubbles up, need help on the gate"
	idD, err := insert("form", prose)
	if err != nil {
		t.Fatalf("insert prose: %v", err)
	}
	if rowD := reportByID(idD); rowD.ScanID != "" || rowD.Text != prose || rowD.Kind == intelanalyze.KindLocal {
		t.Errorf("prose report was re-read: kind %q scan %q text %q", rowD.Kind, rowD.ScanID, rowD.Text)
	}

	// (c) A roster nobody has looked up yet: a blob at insert,
	// re-read by the drain once EVE has verified its names.
	idC, err := insert("axon", miniBlob)
	if err != nil {
		t.Fatalf("insert mini: %v", err)
	}
	if rowC := reportByID(idC); rowC.ScanID != "" || rowC.Text != miniBlob {
		t.Fatalf("mini roster read too early: scan %q text %q", rowC.ScanID, rowC.Text)
	}
	if limited := app.drainIntelAnalysis(ctx); limited {
		t.Fatal("the drain hit a limit")
	}
	rowC := reportByID(idC)
	wantC := idsOf("Pavel Tsukov", "Rho Banu", "Sef Amari", "Tovak Lin", "Uma Ferro", "Wex Talbot")
	if rowC.ScanID == "" || !strings.HasPrefix(rowC.Text, "6 in local:") {
		t.Errorf("mini roster after the drain: scan %q text %q", rowC.ScanID, rowC.Text)
	}
	if got := pilotIDs(rowC); !sameIDs(got, wantC) {
		t.Errorf("mini roster pilots:\n got %v\nwant %v", got, wantC)
	}
	if rowD := reportByID(idD); rowD.ScanID != "" || rowD.Text != prose {
		t.Errorf("prose report changed in the drain: scan %q text %q", rowD.ScanID, rowD.Text)
	}

	// The poster's current system carries a roster paste that
	// names none; without a known location the paste is refused
	// as it always was.
	seedSnapshot(t, q, poster, esi.SnapLocation, esi.Location{SolarSystemID: sysID})
	lines := "Altorr\nKen117\nVild\nKhamez\nStrokes"
	base := db.InsertIntelReportParams{ChannelID: cid, AuthorUserID: u.ID,
		AuthorCharacterID: poster, Reporter: "Burzrujat", CreatedAt: now, SeenAt: now}
	if res := app.postChatLines(ctx, base, lines, "chat"); res.Posted != 1 {
		t.Fatalf("roster paste on the poster's system: %+v, want one posted", res)
	}
	rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.SystemID == sysID && strings.Contains(r.Text, "system from the reporter's current location") {
			found = true
			if !strings.HasPrefix(r.Text, "5 in local:") {
				t.Errorf("fallback report headline: %q", r.Text)
			}
		}
	}
	if !found {
		t.Errorf("no report carries the location note")
	}
	base.AuthorCharacterID = 91000301
	base.Reporter = "Hostile One"
	if res := app.postChatLines(ctx, base, lines, "chat"); res.Posted != 0 || res.Duplicates != 0 {
		t.Errorf("roster paste with no known location: %+v, want it refused", res)
	}

	// A system named with the paste beats the poster's location.
	p2, _, ok := app.scanReportOf(ctx, db.InsertIntelReportParams{ChannelID: cid, AuthorUserID: u.ID,
		AuthorCharacterID: poster, Reporter: "Burzrujat", CreatedAt: now, SeenAt: now}, lines, 30000142)
	if !ok || p2.SystemID != 30000142 || strings.Contains(p2.Text, "reporter's current location") {
		t.Errorf("named system precedence: ok %v system %d text %q", ok, p2.SystemID, p2.Text)
	}
}
