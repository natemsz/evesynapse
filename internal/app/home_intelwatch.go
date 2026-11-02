package app

// ---------------------------------------------------------------------------
// Intel watch: the latest filed intel for the account's watched
// systems (notifyPrefs.CampSystems, the same list the gate camp
// alerts read, in its stored order), plus recent kills involving
// the account's corporations.
//
// Reports come through the one intel read path every surface
// uses: the channels the reader may access (accessibleChannels
// over the reader's intel identity), the per-system and gate
// queries, merged and phrased the way the system page merges and
// phrases them. Kills come from the kills table's corporation
// read, which only ever returns public kills. Nothing here is
// computed from anything else, and nothing is stated that the
// stored reports and kills do not say.
//
// Size is information, as everywhere on the home grid: the
// smallest sizes show the newest reports only, the default sizes
// add each watched system's gate states, and the largest add the
// corporation kills. The counts are this widget's own, keyed off
// its stored size.
// ---------------------------------------------------------------------------

import (
	"context"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

// intelWatchReportsFetch caps the stored reports read per watched
// system and direction (the system's own and its gate-linked),
// the same bound the system page reads under.
const intelWatchReportsFetch = systemReportsMax

// intelWatchKillsFetch caps the kills read per corporation.
const intelWatchKillsFetch = 5

// intelWatchGate is one gate's latest reading for a watched
// system: the system on the far side of the gate and what the
// newest report about that gate says, in the kind's own words.
type intelWatchGate struct {
	Gate  string
	State string
	Ago   string
}

// intelWatchSystem is one watched system's place in the widget:
// its name and, at the sizes that show them, its gate states.
type intelWatchSystem struct {
	ID    int64
	Name  string
	Gates []intelWatchGate
}

// intelWatchWidget is the Intel watch module's model.
type intelWatchWidget struct {
	Empty   bool // no watched systems chosen yet
	Systems []intelWatchSystem
	Reports []reportView
	Kills   []killRow
	Watched *watchedSystemsView
}

// intelWatchBands resolves the widget's size into what it shows:
// how many of the newest reports, whether gate states ride along,
// and whether the corporation kills do.
func intelWatchBands(gw, gh int) (reports int, gates, kills bool) {
	switch area := gw * gh; {
	case area <= 2:
		return 2, false, false
	case area <= 4:
		return 5, true, false
	default:
		return 8, true, true
	}
}

func (app *Application) buildIntelWatch(ctx context.Context, userID int64, gw, gh int) *intelWatchWidget {
	prefs := app.notifyPrefsFor(ctx, userID)
	view := &intelWatchWidget{Watched: app.watchedSystemsViewFor(ctx, prefs, "/")}
	if len(prefs.CampSystems) == 0 {
		view.Empty = true
		return view
	}
	reportsCap, showGates, showKills := intelWatchBands(gw, gh)
	names := app.newKillNames(ctx)
	id := app.intelIdentity(ctx)
	chans := app.accessibleChannels(ctx, id)
	channelIDs := make([]int64, len(chans))
	for i, c := range chans {
		channelIDs[i] = c.ID
	}
	now := time.Now()
	var feed []reportView
	seenReports := map[int64]bool{}
	for _, systemID := range prefs.CampSystems {
		system := intelWatchSystem{ID: systemID, Name: app.campSystemName(ctx, systemID)}
		reports := app.intelWatchSystemReports(ctx, names, id, channelIDs, systemID, now)
		if showGates {
			system.Gates = intelWatchGateStates(reports, systemID)
		}
		view.Systems = append(view.Systems, system)
		// A report about the gate between two watched systems
		// stands in both systems' lists; the merged feed shows
		// it once, in the earlier watched system's phrasing.
		for _, rv := range reports {
			if seenReports[rv.ID] {
				continue
			}
			seenReports[rv.ID] = true
			feed = append(feed, rv)
		}
	}
	sort.SliceStable(feed, func(i, j int) bool {
		if !feed[i].Time.Equal(feed[j].Time) {
			return feed[i].Time.After(feed[j].Time)
		}
		return feed[i].ID > feed[j].ID
	})
	if len(feed) > reportsCap {
		feed = feed[:reportsCap]
	}
	view.Reports = feed
	if showKills {
		view.Kills = app.intelWatchCorpKills(ctx, names, userID, now)
	}
	return view
}

// intelWatchSystemReports reads one watched system's reports the
// way the system page does: its own reports and the gate-linked
// ones filed under the far system, merged newest first, each
// phrased by the shared analysis. A gate-linked clear is phrased
// from this side, also as the system page phrases it.
func (app *Application) intelWatchSystemReports(ctx context.Context, names *killNames, id intelIdentity, channelIDs []int64, systemID int64, now time.Time) []reportView {
	if len(channelIDs) == 0 {
		return nil
	}
	rows, err := app.queries.ListIntelReportsBySystem(ctx, db.ListIntelReportsBySystemParams{
		ChannelIds: channelIDs, SystemID: systemID, MaxRows: intelWatchReportsFetch,
	})
	if err != nil {
		logging.Errorf("intel watch: reports for system %d: %v", systemID, err)
		return nil
	}
	gateRows, err := app.queries.ListIntelReportsByGateSystem(ctx, db.ListIntelReportsByGateSystemParams{
		ChannelIds: channelIDs, SystemID: systemID, MaxRows: intelWatchReportsFetch,
	})
	if err != nil {
		logging.Errorf("intel watch: gate reports for system %d: %v", systemID, err)
	}
	type sysReport struct {
		row db.ListIntelReportsBySystemRow
		far bool
	}
	all := make([]sysReport, 0, len(rows)+len(gateRows))
	for _, r := range rows {
		all = append(all, sysReport{row: r})
	}
	for _, r := range gateRows {
		all = append(all, sysReport{row: db.ListIntelReportsBySystemRow(r), far: true})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].row.SeenAt.Equal(all[j].row.SeenAt) {
			return all[i].row.SeenAt.After(all[j].row.SeenAt)
		}
		return all[i].row.ID > all[j].row.ID
	})
	if len(all) > intelWatchReportsFetch {
		all = all[:intelWatchReportsFetch]
	}
	out := make([]reportView, 0, len(all))
	for _, sr := range all {
		r := sr.row
		rv := reportView{ID: r.ID, Channel: r.ChannelID, SystemID: r.SystemID, Reporter: r.Reporter, Text: r.Text,
			Hostiles: r.Hostiles, Clear: r.Clear, ScanID: r.ScanID, Source: r.Source, At: r.SeenAt.Unix(), Time: r.SeenAt,
			Ago: ageText(now, r.SeenAt), Mine: r.AuthorUserID == id.userID, NoDoors: true}
		sys := names.system(r.SystemID)
		rv.SystemName = sys.Name
		rv.SecText, rv.SecTier = secRound(sys.Security)
		applyAnalysis(&rv, r.Kind, r.Analysis, names)
		if sr.far && r.Kind == intelanalyze.KindClear && rv.GateID == systemID {
			rv.Text = "The " + rv.SystemName + " side is clear"
		}
		out = append(out, rv)
	}
	return out
}

// intelWatchGateStates reduces one watched system's reports
// (newest first) to one state per gate: for a report filed under
// the watched system the gate is the system its analysis names;
// for a gate-linked report filed under the far system, the gate
// is that far system. The newest reading of each gate stands.
func intelWatchGateStates(reports []reportView, systemID int64) []intelWatchGate {
	seen := map[string]bool{}
	var gates []intelWatchGate
	for _, rv := range reports {
		gate := rv.Gate
		if rv.SystemID != systemID {
			gate = rv.SystemName
		}
		if gate == "" || seen[gate] {
			continue
		}
		seen[gate] = true
		gates = append(gates, intelWatchGate{Gate: gate, State: intelWatchGateState(rv), Ago: rv.Ago})
	}
	return gates
}

// intelWatchGateState says what one gate reading reports, in
// words: the kind's own label where it has one, a hostile count
// where the report carries one.
func intelWatchGateState(rv reportView) string {
	if rv.KindLabel != "" {
		return rv.KindLabel
	}
	if rv.Hostiles > 0 {
		return "Hostiles"
	}
	return "Reported"
}

// intelWatchCorpKills reads the recent public kills involving
// the account's corporations: the kills table's corporation
// read per corporation, merged newest first.
func (app *Application) intelWatchCorpKills(ctx context.Context, names *killNames, userID int64, now time.Time) []killRow {
	memberships, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("intel watch: corporations for user %d: %v", userID, err)
		return nil
	}
	seen := map[int64]bool{}
	var corps []int64
	for _, m := range memberships {
		if m.CorporationID > 0 && !seen[m.CorporationID] {
			seen[m.CorporationID] = true
			corps = append(corps, m.CorporationID)
		}
	}
	sort.Slice(corps, func(i, j int) bool { return corps[i] < corps[j] })
	var kills []db.Kill
	for _, corpID := range corps {
		rows, err := app.queries.ListKillsByCorporation(ctx, db.ListKillsByCorporationParams{
			EntityID: corpID, BeforeTime: now.Add(time.Hour), BeforeID: 1 << 62, MaxRows: intelWatchKillsFetch,
		})
		if err != nil {
			logging.Errorf("intel watch: kills for corporation %d: %v", corpID, err)
			continue
		}
		kills = append(kills, rows...)
	}
	sort.SliceStable(kills, func(i, j int) bool {
		if !kills[i].KillmailTime.Equal(kills[j].KillmailTime) {
			return kills[i].KillmailTime.After(kills[j].KillmailTime)
		}
		return kills[i].KillmailID > kills[j].KillmailID
	})
	if len(kills) > intelWatchKillsFetch {
		kills = kills[:intelWatchKillsFetch]
	}
	out := make([]killRow, 0, len(kills))
	for _, k := range kills {
		out = append(out, names.row(k))
	}
	return out
}
