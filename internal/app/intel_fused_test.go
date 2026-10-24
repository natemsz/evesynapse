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

	// The chat-lines path (Axon and pasted lines): the fused
	// roster arrives as ONE line naming no system the chat
	// parser can find, and was dropped at classification --
	// never stored, so the re-read above never saw it. The
	// rescue reads it there, on the poster's system, in a
	// second channel so its set stands apart from the first.
	cid2, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Fused Lines", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	rowsOf2 := func() []db.ListIntelReportsRow {
		t.Helper()
		rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid2, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	axonLine := now.Add(-time.Minute).Format("[ 2006.01.02 15:04:05 ] ") + "Burzrujat > " + fusedBlob
	base2 := db.InsertIntelReportParams{ChannelID: cid2, AuthorUserID: u.ID,
		AuthorCharacterID: 91000301, Reporter: "Hostile One", CreatedAt: now, SeenAt: now}

	// (b) No location on file for the poster: skipped, as such
	// lines always were, and nothing is stored.
	if res := app.postChatLines(ctx, base2, axonLine, "axon"); res.Posted != 0 || res.Duplicates != 0 || res.Skipped != 1 {
		t.Errorf("fused line, no known location: %+v, want it skipped", res)
	} else if len(res.SkippedLines) != 1 || res.SkippedLines[0] != axonLine {
		t.Errorf("skipped lines kept as written: %q", res.SkippedLines)
	}
	if n := len(rowsOf2()); n != 0 {
		t.Fatalf("the lines channel holds %d reports, want 0", n)
	}

	// (a) The poster located (the snapshot seeded above): the
	// line is stored as the roster it is, on the poster's
	// system, the sentence saying where the system came from.
	base2.AuthorCharacterID = poster
	base2.Reporter = "Burzrujat"
	if res := app.postChatLines(ctx, base2, axonLine, "axon"); res.Posted != 1 || res.Skipped != 0 {
		t.Fatalf("fused line on the poster's system: %+v, want one posted", res)
	}
	rows2 := rowsOf2()
	if len(rows2) != 1 {
		t.Fatalf("the lines channel holds %d reports, want 1", len(rows2))
	}
	rowA2 := rows2[0]
	if rowA2.SystemID != sysID || rowA2.Kind != intelanalyze.KindLocal || rowA2.ScanID == "" {
		t.Errorf("rescued report: system %d kind %q scan %q, want a local reading on the poster's system", rowA2.SystemID, rowA2.Kind, rowA2.ScanID)
	}
	if !strings.HasPrefix(rowA2.Text, "28 in local: 0 hostiles, 2 friendly (incl. you), 26 neutral") ||
		!strings.Contains(rowA2.Text, "no scan attached, ships unknown") ||
		!strings.Contains(rowA2.Text, "system from the reporter's current location") ||
		strings.Contains(rowA2.Text, "AltorrAstria") {
		t.Errorf("rescued report headline: %q", rowA2.Text)
	}
	if got := pilotIDs(rowA2); !sameIDs(got, wantA) {
		t.Errorf("rescued report pilots:\n got %v\nwant %v", got, wantA)
	}
	// The same line again is the same roster: one report.
	if res := app.postChatLines(ctx, base2, axonLine, "axon"); res.Posted != 0 || res.Duplicates != 1 {
		t.Errorf("fused line re-sent: %+v, want a duplicate", res)
	}

	// (c) Plain chatter naming no system still skips.
	if res := app.postChatLines(ctx, base2, "anyone got eyes on the in-gate tonight", "chat"); res.Posted != 0 || res.Skipped != 1 {
		t.Errorf("chatter line: %+v, want it skipped", res)
	}

	// (d) The blob with a system named in the line is read as
	// it always was: a roster on that system, no fallback note.
	if res := app.postChatLines(ctx, base2, "Jita "+fusedBlob, "axon"); res.Posted != 1 {
		t.Fatalf("fused line naming its system: %+v, want one posted", res)
	}
	rows2 = rowsOf2()
	if len(rows2) != 2 {
		t.Fatalf("the lines channel holds %d reports, want 2", len(rows2))
	}
	for _, r := range rows2 {
		if r.SystemID == 30000142 {
			if r.ScanID == "" || !strings.HasPrefix(r.Text, "28 in local:") || strings.Contains(r.Text, "reporter's current location") {
				t.Errorf("named-system report: scan %q text %q", r.ScanID, r.Text)
			}
		}
	}
}

// The production d-scan, as stored from intel_reports 18-20 on
// 2026-10-24: one scan sent three times in a flush, the copies
// differing in the first structure's name and where they clip.
// The fused roster of intel_report 17 rides along: names are
// production, IDs and corporations fixture values.
const prodScanA = "35825  - Al's Logistics Agency HQ  Raitaru  1,048,257 km35825  Teonusude - Eagle's Perch  Raitaru  7.4 AU22544  H2  Hulk  -22544  Vespa II  Hulk  -22544  Vespa II  Hulk  -22544  Share 1  Hulk  -22544  H1  Hulk  -22548  Blowjob queen  Mackinaw  -22544  monster Hamalias Hulk  Hulk  -22544  Vespa II  H"

const prodScanB = "35825  Teonusude - Al's Logistics Agency HQ  Raitaru  1,048,257 km35825  Teonusude - Eagle's Perch  Raitaru  7.4 AU22544  H2  Hulk  -22544  Vespa II  Hulk  -22544  Vespa II  Hulk  -22544  Share 1  Hulk  -22544  H1  Hulk  -22548  Blowjob queen  Mackinaw  -22544  monster Hamalias Hulk  Hulk  -22544  V"

const prodRoster17 = "AurikulAurithosAurixaAzarian VespilloBurzrujatCaere DorestadCaldari Citizen 92702902Calvin TarkaneanCharnan WhorntonclosetomeFelicia Tayber EnginseerFrodo BeutlingGonjeri MusteanHarry DavahamLambo OttelLaura The BoraLexikdarklonggen2Michael190MinerZeemonster Hamaliamonsterchens schwesterMR JAZZMOghe"

var roster17Names = []fusedPilot{
	{"Aurikul", 2124000401, 98000999},
	{"Aurithos", 2124000402, 98000777},
	{"Aurixa", 2124000403, 98000999},
	{"Azarian Vespillo", 2124000404, 98000777},
	{"Caere Dorestad", 2124000405, 98000999},
	{"Calvin Tarkanean", 2124000406, 98000777},
	{"Charnan Whornton", 2124000407, 98000999},
	{"Felicia Tayber", 2124000408, 98000777},
	{"Frodo Beutling", 2124000409, 98000999},
	{"Gonjeri Mustean", 2124000410, 98000777},
	{"Harry Davaham", 2124000411, 98000999},
	{"Lambo Ottel", 2124000412, 98000777},
	{"Lexikdark", 2124000413, 98000999},
	{"longgen2", 2124000414, 98000777},
	{"Michael190", 2124000415, 98000999},
	{"Miner", 2124000416, 98000777},
	{"Zeemonster", 2124000417, 98000999},
	{"Hamalia", 2124000418, 98000777},
	{"monsterchens schwester", 2124000419, 98000999},
}

// TestIntelChatScanMessages: classification runs per chat
// message, so a d-scan the client fused into a message is
// stored as the scan it is -- one report for the three copies
// production sent, its reading the scan tool's -- and the
// fused roster of the same evening is one roster report.
func TestIntelChatScanMessages(t *testing.T) {
	// Report 17's names resolve through EVE like any pilot's:
	// the fixture ESI knows them beside the standing fixture
	// pilots (whose IDs it keeps).
	transport := newFusedESI()
	for _, p := range roster17Names {
		if _, ok := transport.byName[strings.ToLower(p.name)]; !ok {
			transport.byName[strings.ToLower(p.name)] = p
			transport.byID[p.id] = p
		}
	}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30002385, 'Teonusude', 10000060, -0.38) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (543, 'Exhumer', 6), (1406, 'Engineering Complex', 65) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id) VALUES (35825, 'Raitaru', 1406), (22544, 'Hulk', 543), (22548, 'Mackinaw', 543) ON CONFLICT DO NOTHING`)
	const (
		teoID  = int64(30002385)
		poster = int64(519225918) // Burzrujat
	)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, poster, "Burzrujat")
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: poster, CorporationID: 98000301, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Teonusude", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The verified dictionary: report 17's resolvable names,
	// plus the names report 13's roster already pinned.
	for _, group := range [][]fusedPilot{roster17Names, fusedRoster} {
		for _, p := range group {
			if err := q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: strings.ToLower(p.name), CharacterID: p.id, Name: p.name, CheckedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{CharacterID: p.id, CorporationID: p.corp, CheckedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedSnapshot(t, q, poster, esi.SnapLocation, esi.Location{SolarSystemID: teoID})
	base := db.InsertIntelReportParams{ChannelID: cid, AuthorUserID: u.ID,
		AuthorCharacterID: poster, Reporter: "Burzrujat", CreatedAt: now, SeenAt: now}
	stamp := func(d time.Duration) string { return now.Add(d).Format("[ 2006.01.02 15:04:05 ] ") }

	// The d-scan, three copies in one flush: one report.
	flush := stamp(-3*time.Minute) + "Burzrujat > " + prodScanA + "\n" +
		stamp(-3*time.Minute+time.Second) + "Burzrujat > " + prodScanB + "\n" +
		stamp(-3*time.Minute+2*time.Second) + "Burzrujat > " + prodScanA
	if res := app.postChatLines(ctx, base, flush, "axon"); res.Posted != 1 || res.Duplicates != 2 {
		t.Fatalf("the three copies of one scan: %+v, want one posted and two duplicates", res)
	}
	rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the channel holds %d reports, want 1", len(rows))
	}
	row := rows[0]
	if row.SystemID != teoID || row.ScanID == "" {
		t.Fatalf("scan report: system %d scan %q, want a scan reading on Teonusude", row.SystemID, row.ScanID)
	}
	if strings.Contains(row.Text, "35825") || !strings.Contains(row.Text, "Teonusude") || !strings.Contains(row.Text, "on grid") {
		t.Errorf("scan report headline is not the d-scan sentence: %q", row.Text)
	}
	scanRow, err := q.GetScan(ctx, row.ScanID)
	if err != nil {
		t.Fatalf("the report's scan: %v", err)
	}
	var reading scanResult
	if err := json.Unmarshal([]byte(scanRow.Result), &reading); err != nil {
		t.Fatalf("the scan's reading: %v", err)
	}
	if reading.Kind != "dscan" || reading.DScan == nil {
		t.Fatalf("the reading is not a d-scan: %+v", reading)
	}
	d := reading.DScan
	if len(d.Structures) != 1 || d.Structures[0].Name != "Raitaru" || d.Structures[0].N != 2 {
		t.Errorf("structures: %+v, want the two Raitarus", d.Structures)
	}
	types := map[string]int{}
	for _, c := range d.Types {
		types[c.Name] = c.N
	}
	if types["Hulk"] != 6 || types["Mackinaw"] != 1 || d.Ships != 7 {
		t.Errorf("ship types: %v (ships %d), want six Hulks and a Mackinaw", types, d.Ships)
	}

	// Report 17's roster, fused the same way: one roster
	// report on the poster's system, the blob nowhere in it.
	line := stamp(-2*time.Minute) + "Burzrujat > " + prodRoster17
	if res := app.postChatLines(ctx, base, line, "axon"); res.Posted != 1 {
		t.Fatalf("the fused roster line: %+v, want one posted", res)
	}
	rows, err = q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("the channel holds %d reports, want 2", len(rows))
	}
	var rosterRow *db.ListIntelReportsRow
	for i := range rows {
		if rows[i].ScanID != "" && rows[i].ID != row.ID {
			rosterRow = &rows[i]
		}
	}
	if rosterRow == nil {
		t.Fatal("no roster report stands beside the scan")
	}
	if rosterRow.SystemID != teoID || rosterRow.Kind != intelanalyze.KindLocal ||
		!strings.Contains(rosterRow.Text, "in local:") ||
		!strings.Contains(rosterRow.Text, "system from the reporter's current location") ||
		strings.Contains(rosterRow.Text, "AurikulAurithos") {
		t.Errorf("roster report: system %d kind %q text %q", rosterRow.SystemID, rosterRow.Kind, rosterRow.Text)
	}
	var a intelanalyze.Analysis
	if err := json.Unmarshal([]byte(rosterRow.Analysis), &a); err != nil {
		t.Fatalf("roster analysis: %v", err)
	}
	if len(a.Pilots) < 20 {
		t.Errorf("roster analysis names %d pilots, want at least 20 of report 17's names", len(a.Pilots))
	}
}

// TestIntelStoredRosterBackfill: a report stored before the
// roster re-read existed -- the blob filed as prose, its
// analysis reading no pilots from it, settled on arrival --
// is converted by the drain into the roster reading, the blob
// kept as the reading's paste. A second drain changes nothing.
func TestIntelStoredRosterBackfill(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, newFusedESI())
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004759, '1DQ1-A', 10000060, -0.38) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	const (
		sysID   = int64(30004759)
		poster  = int64(519225918) // Burzrujat
		ownCorp = int64(98000301)
	)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, poster, "Burzrujat")
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: poster, CorporationID: ownCorp, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Stored", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, p := range fusedRoster {
		if err := q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: strings.ToLower(p.name), CharacterID: p.id, Name: p.name, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{CharacterID: p.id, CorporationID: p.corp, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	// The old way in: the blob as prose, settled on arrival,
	// two hours past the pending window.
	old := now.Add(-2 * time.Hour)
	plain, _ := json.Marshal(intelanalyze.Analysis{Kind: intelanalyze.KindSighting})
	id, err := q.InsertIntelReport(ctx, db.InsertIntelReportParams{ChannelID: cid, SystemID: sysID,
		AuthorUserID: u.ID, AuthorCharacterID: poster, Reporter: "Burzrujat",
		Text: fusedBlob, Pilots: fusedBlob, Source: "axon", SeenAt: old, CreatedAt: old,
		Kind: intelanalyze.KindSighting, Analysis: string(plain), AnalysisPending: false})
	if err != nil {
		t.Fatalf("store the old-way report: %v", err)
	}
	stored := func() db.ListIntelReportsRow {
		t.Helper()
		rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("the channel holds %d reports, want 1", len(rows))
		}
		return rows[0]
	}
	if row := stored(); row.ID != id || row.ScanID != "" || row.Text != fusedBlob {
		t.Fatalf("the report did not go in the old way: %+v", row)
	}

	if limited := app.drainIntelAnalysis(ctx); limited {
		t.Fatal("the drain hit a limit")
	}
	row := stored()
	if row.Kind != intelanalyze.KindLocal || row.ScanID == "" {
		t.Fatalf("the stored blob was not re-read: kind %q scan %q", row.Kind, row.ScanID)
	}
	if !strings.HasPrefix(row.Text, "28 in local: 0 hostiles, 2 friendly (incl. you), 26 neutral") ||
		!strings.Contains(row.Text, "no scan attached, ships unknown") ||
		strings.Contains(row.Text, "AltorrAstria") {
		t.Errorf("backfilled headline: %q", row.Text)
	}
	var a intelanalyze.Analysis
	if err := json.Unmarshal([]byte(row.Analysis), &a); err != nil {
		t.Fatalf("backfilled analysis: %v", err)
	}
	if len(a.Pilots) != len(fusedRoster) {
		t.Errorf("backfilled analysis names %d pilots, want %d", len(a.Pilots), len(fusedRoster))
	}
	scanRow, err := q.GetScan(ctx, row.ScanID)
	if err != nil {
		t.Fatalf("the reading's scan: %v", err)
	}
	if scanRow.Raw != fusedBlob {
		t.Errorf("the scan's paste is not the blob as stored")
	}

	// The drain again: nothing changes.
	if limited := app.drainIntelAnalysis(ctx); limited {
		t.Fatal("the drain hit a limit")
	}
	if again := stored(); again.ScanID != row.ScanID || again.Text != row.Text || again.Analysis != row.Analysis {
		t.Errorf("the second drain changed the report:\nfirst  scan %q text %q\nsecond scan %q text %q", row.ScanID, row.Text, again.ScanID, again.Text)
	}
}
