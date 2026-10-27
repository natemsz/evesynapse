package app

// ---------------------------------------------------------------------------
// Pilot intel: what EveSynapse knows of a pilot's doings, on their public
// page (/pilot/), and the known-hostiles lists intel channels keep.
//
// From the killboard, public kills only (posted to zKillboard), the last
// 90 days: kills, solo kills, losses, ISK destroyed; where and when they
// were last seen and in what; the ships they fly; where they hunt; the
// hours (EVE time) they are active; who they fly with; their latest kills
// and losses. Once the pilot has a stored activity profile
// (pilot_activity.go), the section's aggregates are the profile's
// instead: the whole recorded record, with their usual hull and the
// gang size they fly with. Anyone can see this: it is all public.
//
// For a signed-in reader, from what is theirs to read: reports in their
// intel channels that name the pilot (the last 7 days), their own local
// scans with the pilot in them, and the channels that watch the pilot.
// Anyone who may post to a channel adds a pilot to its known hostiles
// (schema 080) with a note, or takes one off; the channel page lists them
// with where each was last seen, and the map's "Watched pilots" layer
// pins them (map_mine.go).
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	pilotIntelWindow   = 90 * 24 * time.Hour
	pilotReportsWindow = 7 * 24 * time.Hour
	watchSeenWindow    = 48 * time.Hour
	maxWatchPerChannel = 500
)

type pilotCount struct {
	ID      int64
	Name    string
	N       int64
	Text    string // the count as prose, when one bare number is not the story (a profile's ship rows: flown, final blows, lost)
	Pct     int
	SecText string
	SecTier int
	Ago     string
}

type pilotHour struct {
	Hour int
	N    int64
	Pct  int
}

type pilotCompanion struct {
	ID           int64
	Name         string
	CorpID       int64
	CorpName     string
	AllianceID   int64
	AllianceName string
	N            int64
}

type pilotKill struct {
	KillmailID int64
	Lost       bool
	Ship       string
	ShipTypeID int64
	Flew       string
	System     string
	SecText    string
	SecTier    int
	Value      string
	Ago        string
}

type pilotReport struct {
	Channel string
	System  string
	SecText string
	SecTier int
	Text    string
	By      string
	Ago     string
}

// pilotScan is one of the reader's local scans with the pilot in it.
type pilotScan struct {
	ID      string
	System  string
	SecText string
	SecTier int
	Ago     string
}

type pilotWatch struct {
	ChannelID int64
	Channel   string
	Note      string
}

// pilotIntelView is the intel part of the pilot page.
type pilotIntelView struct {
	CharacterID int64
	Name        string
	Any         bool
	// HasIntel: the section holds actual intel on this pilot —
	// killboard activity, or the reader's channel reports, scans or
	// watched lists naming them. The page gives such a section the
	// rail placement (pilot.html); an empty section keeps the plain
	// stacked placement it has always had.
	HasIntel   bool
	Kills      int64
	Solo       int64
	Losses     int64
	ISK        string
	LastSeen   *pilotKill
	Ships      []pilotCount
	Systems    []pilotCount
	Hours      []pilotHour
	Peak       string
	Companions []pilotCompanion
	Recent     []pilotKill
	// ProfileBacked: the aggregates above come from the pilot's
	// stored activity profile (pilot_activity.go), the whole of
	// their recorded kills and losses, rather than the last 90
	// days of the local board. Set only when a profile with a
	// record exists; the template labels the section with the
	// wider scope and the profile's own freshness (UpdatedAgo).
	ProfileBacked bool
	UpdatedAgo    string
	UsualShip     string
	Gang          string
	LastActive    string

	SignedIn  bool
	Reports   []pilotReport
	Scans     []pilotScan
	Watched   []pilotWatch
	CanWatch  []channelRow // channels the reader may add the pilot to
	WatchNote string
}

func pctOf(n, top int64) int {
	if top <= 0 {
		return 0
	}
	return max(3, int(n*100/top))
}

// likeEscape makes s a literal inside an ILIKE pattern.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (app *Application) loadPilotIntel(ctx context.Context, id int64, name string) *pilotIntelView {
	v := &pilotIntelView{CharacterID: id, Name: name}
	now := time.Now()
	since := now.Add(-pilotIntelWindow)
	names := app.newKillNames(ctx)
	if v.Name == "" {
		v.Name = names.character(id)
	}
	// The stored profile first: when it backs the page, its
	// aggregates replace the 90-day ones below, so computing
	// those only to throw them away is waste on every view of
	// a profiled pilot.
	act := app.loadPilotActivity(ctx, id, names, now)
	profileBacked := act != nil && act.Any
	if !profileBacked {
		if st, err := app.queries.PilotKillStats(ctx, db.PilotKillStatsParams{CharacterID: id, Since: since}); err == nil {
			v.Kills, v.Solo, v.Losses = st.Kills, st.Solo, st.Losses
			if st.IskKilled > 0 {
				v.ISK = iskShortGo(st.IskKilled)
			}
		} else {
			logging.Warnf("pilot intel %d: stats: %v", id, err)
		}
	}
	if rows, err := app.queries.ListPilotRecentKills(ctx, db.ListPilotRecentKillsParams{CharacterID: id, MaxRows: 10}); err == nil {
		for _, r := range rows {
			sys := names.system(r.SystemID)
			k := pilotKill{KillmailID: r.KillmailID, Lost: r.Lost, ShipTypeID: r.ShipTypeID, Ship: names.typeName(r.ShipTypeID),
				System: sys.Name, Ago: ageText(now, r.KillmailTime)}
			k.SecText, k.SecTier = secRound(sys.Security)
			if r.Flew > 0 {
				k.Flew = names.typeName(r.Flew)
			}
			if r.Value > 0 {
				k.Value = iskShortGo(r.Value)
			}
			v.Recent = append(v.Recent, k)
		}
		if len(v.Recent) > 0 {
			last := v.Recent[0]
			v.LastSeen = &last
		}
	}
	if !profileBacked {
		if rows, err := app.queries.ListPilotShips(ctx, db.ListPilotShipsParams{CharacterID: id, Since: since}); err == nil && len(rows) > 0 {
			for _, r := range rows {
				v.Ships = append(v.Ships, pilotCount{ID: r.ShipTypeID, Name: names.typeName(r.ShipTypeID), N: r.N, Pct: pctOf(r.N, rows[0].N)})
			}
		}
		if rows, err := app.queries.ListPilotSystems(ctx, db.ListPilotSystemsParams{CharacterID: id, Since: since}); err == nil && len(rows) > 0 {
			for _, r := range rows {
				sys := names.system(r.SystemID)
				c := pilotCount{ID: r.SystemID, Name: sys.Name, N: r.N, Pct: pctOf(r.N, rows[0].N), Ago: ageText(now, r.LastAt)}
				c.SecText, c.SecTier = secRound(sys.Security)
				v.Systems = append(v.Systems, c)
			}
		}
		if rows, err := app.queries.ListPilotHours(ctx, db.ListPilotHoursParams{CharacterID: id, Since: since}); err == nil && len(rows) > 0 {
			v.Hours = make([]pilotHour, 24)
			var top int64
			for h := range v.Hours {
				v.Hours[h].Hour = h
			}
			for _, r := range rows {
				if r.Hour >= 0 && r.Hour < 24 {
					v.Hours[r.Hour].N = r.N
					top = max(top, r.N)
				}
			}
			for h := range v.Hours {
				if v.Hours[h].N > 0 {
					v.Hours[h].Pct = pctOf(v.Hours[h].N, top)
				}
			}
			v.Peak = peakHours(v.Hours)
		}
	}
	if rows, err := app.queries.ListPilotCompanions(ctx, db.ListPilotCompanionsParams{CharacterID: id, Since: since}); err == nil {
		for _, r := range rows {
			v.Companions = append(v.Companions, pilotCompanion{ID: r.CharacterID, Name: names.character(r.CharacterID), N: r.N,
				CorpID: r.CorporationID, CorpName: names.corp(r.CorporationID), AllianceID: r.AllianceID, AllianceName: names.alliance(r.AllianceID)})
		}
	}
	// The stored profile, when the pilot has one with a record:
	// its aggregates cover the whole of their kill history and
	// are kept current at ingest, so they take over the section's
	// numbers from the 90-day board read above. The 90-day-only
	// figures (solo kills, ISK destroyed) do not carry over; the
	// latest kills and the companions stay board reads either way.
	if profileBacked {
		v.Kills, v.Losses = act.Kills, act.Losses
		v.Solo, v.ISK = 0, ""
		v.Ships = nil
		for _, s := range act.Ships {
			v.Ships = append(v.Ships, pilotCount{ID: s.ID, Name: s.Name, Pct: s.Pct, Text: s.Text})
		}
		v.Systems = act.Systems
		v.Hours = act.Hours
		v.Peak = act.Peak
		v.ProfileBacked = true
		v.UpdatedAgo = act.UpdatedAgo
		v.UsualShip = act.UsualShip
		v.Gang = act.Gang
		v.LastActive = act.LastActive
	}
	v.Any = v.Kills+v.Losses > 0 || len(v.Recent) > 0
	v.HasIntel = v.Any

	// The reader's own: channel reports naming the pilot, their scans
	// with the pilot in them, the channels watching the pilot.
	ident := app.intelIdentity(ctx)
	if ident.userID == 0 {
		return v
	}
	v.SignedIn = true
	chans := app.accessibleChannels(ctx, ident)
	chanName := map[int64]string{}
	var chanIDs []int64
	for _, c := range chans {
		chanName[c.ID] = c.Name
		chanIDs = append(chanIDs, c.ID)
	}
	if len(chanIDs) > 0 && utf8.RuneCountInString(v.Name) >= 3 && !strings.HasPrefix(v.Name, "Pilot #") {
		if rows, err := app.queries.ListReportsNaming(ctx, db.ListReportsNamingParams{ChannelIds: chanIDs, Since: now.Add(-pilotReportsWindow), Pattern: likeEscape(v.Name)}); err == nil {
			for _, r := range rows {
				sys := names.system(r.SystemID)
				p := pilotReport{Channel: chanName[r.ChannelID], System: sys.Name, Text: r.Text, By: r.Reporter, Ago: ageText(now, r.SeenAt)}
				p.SecText, p.SecTier = secRound(sys.Security)
				v.Reports = append(v.Reports, p)
			}
		}
	}
	ids := app.sessionScanIDs(ctx)
	if ids == nil {
		ids = []string{}
	}
	if rows, err := app.queries.ListScansWithPilot(ctx, db.ListScansWithPilotParams{UserID: ident.userID, Ids: ids, CharacterID: id}); err == nil {
		for _, r := range rows {
			s := pilotScan{ID: r.ID, Ago: ageText(now, r.CreatedAt)}
			if r.SystemID > 0 {
				sys := names.system(r.SystemID)
				s.System = sys.Name
				s.SecText, s.SecTier = secRound(sys.Security)
			}
			v.Scans = append(v.Scans, s)
		}
	}
	watching := map[int64]bool{}
	if len(chanIDs) > 0 {
		if rows, err := app.queries.ListIntelWatch(ctx, chanIDs); err == nil {
			for _, r := range rows {
				if r.CharacterID == id {
					watching[r.ChannelID] = true
					v.Watched = append(v.Watched, pilotWatch{ChannelID: r.ChannelID, Channel: chanName[r.ChannelID], Note: r.Note})
				}
			}
		}
	}
	for _, c := range chans {
		if c.CanPost && !watching[c.ID] {
			v.CanWatch = append(v.CanWatch, channelRow{ID: c.ID, Name: c.Name})
		}
	}
	v.HasIntel = v.HasIntel || len(v.Reports) > 0 || len(v.Scans) > 0 || len(v.Watched) > 0
	return v
}

// peakHours names the busiest stretch of the day: the three-hour
// window (wrapping midnight) with the most kills.
func peakHours(hours []pilotHour) string {
	best, at := int64(0), 0
	for h := 0; h < 24; h++ {
		s := hours[h].N + hours[(h+1)%24].N + hours[(h+2)%24].N
		if s > best {
			best, at = s, h
		}
	}
	if best == 0 {
		return ""
	}
	return fmt.Sprintf("Most active %02d:00–%02d:00 EVE", at, (at+3)%24)
}

func iskShortGo(v float64) string {
	switch {
	case v >= 1e12:
		return fmt.Sprintf("%.1ft", v/1e12)
	case v >= 1e9:
		return fmt.Sprintf("%.1fb", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.0fm", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

// handlePilotWatch is POST /pilot/watch: add (or, with remove, take
// off) a pilot on a channel's known hostiles.
func (app *Application) handlePilotWatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PostFormValue("character"), 10, 64)
	cid, _ := strconv.ParseInt(r.PostFormValue("channel"), 10, 64)
	back := r.PostFormValue("back")
	if !safeLocalNext(back) {
		back = "/pilot/?character=" + strconv.FormatInt(id, 10)
	}
	flash := app.flashBack(w, r, back)
	if id <= 0 {
		flash("No pilot named.")
		return
	}
	ident := app.intelIdentity(ctx)
	c, ok := app.channelAccess(ctx, ident, cid)
	if !ok || !c.CanPost {
		flash("You cannot change that channel's known hostiles.")
		return
	}
	if r.PostFormValue("remove") != "" {
		if err := app.queries.RemoveIntelWatch(ctx, db.RemoveIntelWatchParams{ChannelID: c.ID, CharacterID: id}); err != nil {
			flash("That did not go through — please try again.")
			return
		}
		flash("Off " + c.Name + "'s known hostiles.")
		return
	}
	if rows, err := app.queries.ListIntelWatch(ctx, []int64{c.ID}); err == nil && len(rows) >= maxWatchPerChannel {
		flash(fmt.Sprintf("A channel watches up to %d pilots.", maxWatchPerChannel))
		return
	}
	note := clipRunes(strings.TrimSpace(r.PostFormValue("note")), 140)
	if err := app.queries.AddIntelWatch(ctx, db.AddIntelWatchParams{ChannelID: c.ID, CharacterID: id, Note: note, AddedBy: ident.userID, AddedAt: time.Now().UTC()}); err != nil {
		logging.Errorf("pilot watch: %v", err)
		flash("That did not go through — please try again.")
		return
	}
	flash("On " + c.Name + "'s known hostiles.")
}

// watchRow is one known hostile on a channel page.
type watchRow struct {
	ID      int64
	Name    string
	Note    string
	Seen    string // "Jita · 3 h ago", "" when not lately
	SecText string
	SecTier int
	SysID   int64
}

// channelWatch lists a channel's known hostiles with where each was
// last seen on a public kill (the last 48 hours).
func (app *Application) channelWatch(ctx context.Context, channelID int64) []watchRow {
	rows, err := app.queries.ListIntelWatch(ctx, []int64{channelID})
	if err != nil || len(rows) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.CharacterID)
	}
	seen := map[int64]db.ListPilotsLastSeenRow{}
	if s, err := app.queries.ListPilotsLastSeen(ctx, db.ListPilotsLastSeenParams{CharacterIds: ids, Since: time.Now().Add(-watchSeenWindow)}); err == nil {
		for _, x := range s {
			seen[x.CharacterID] = x
		}
	}
	names := app.newKillNames(ctx)
	now := time.Now()
	out := make([]watchRow, 0, len(rows))
	for _, r := range rows {
		w := watchRow{ID: r.CharacterID, Name: names.character(r.CharacterID), Note: r.Note}
		if s, ok := seen[r.CharacterID]; ok {
			sys := names.system(s.SystemID)
			w.SysID = s.SystemID
			w.Seen = sys.Name + " · " + ageText(now, s.KillmailTime)
			w.SecText, w.SecTier = secRound(sys.Security)
		}
		out = append(out, w)
	}
	// The recently seen first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seen != "" && out[j].Seen == "" })
	return out
}

// mineWatched is the map's "Watched pilots": the reader's channels'
// known hostiles, where each was last seen on a public kill.
type mineWatched struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Sys  int64  `json:"sys"`
	At   int64  `json:"at"`
	Note string `json:"note,omitempty"`
}

func (app *Application) mineWatched(ctx context.Context, userID int64) []mineWatched {
	ident := app.intelIdentityFor(ctx, userID, 0)
	var chanIDs []int64
	for _, c := range app.accessibleChannels(ctx, ident) {
		chanIDs = append(chanIDs, c.ID)
	}
	out := []mineWatched{}
	if len(chanIDs) == 0 {
		return out
	}
	rows, err := app.queries.ListIntelWatch(ctx, chanIDs)
	if err != nil || len(rows) == 0 {
		return out
	}
	note := map[int64]string{}
	var ids []int64
	for _, r := range rows {
		if _, dup := note[r.CharacterID]; !dup {
			ids = append(ids, r.CharacterID)
		}
		if note[r.CharacterID] == "" {
			note[r.CharacterID] = r.Note
		}
	}
	seen, err := app.queries.ListPilotsLastSeen(ctx, db.ListPilotsLastSeenParams{CharacterIds: ids, Since: time.Now().Add(-watchSeenWindow)})
	if err != nil {
		return out
	}
	names := app.newKillNames(ctx)
	for _, s := range seen {
		out = append(out, mineWatched{ID: s.CharacterID, Name: names.character(s.CharacterID), Sys: s.SystemID, At: s.KillmailTime.Unix(), Note: note[s.CharacterID]})
	}
	return out
}
