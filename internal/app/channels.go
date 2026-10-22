package app

// ---------------------------------------------------------------------------
// Intel channels (Intel): owned streams of intel reports -- "hostiles in
// X" -- shared by grant and drawn on the map.
//
// A channel belongs to the account that made it, and only that account
// manages it: renames it, deletes it, grants and revokes access. A grant
// names one character, a corporation, an alliance, or a director-kept
// group (org_groups), and is read-only or read and post. An account
// reaches a channel through any of its linked characters: the character
// itself, its corporation (the worker's character -> corporation map),
// that corporation's alliance (the corporation record), and the groups
// it is in.
//
// Reports come in three ways: the web form (one system, a note, a count,
// or "clear"), pasted lines from an in-game intel channel (internal/
// intelchat picks out each line's system, count and clear), and a scan
// from the scan tool posted into a channel. The map draws the last 30
// minutes of every channel its reader can see (/map/intel.json), fading
// as they age, and its routes steer around fresh hostiles. Reports are
// kept 7 days.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelchat"
	"evesynapse/internal/logging"
)

const (
	intelRetention   = 7 * 24 * time.Hour
	intelLive        = 30 * time.Minute
	intelFeedPage    = 60
	intelMaxChannels = 20
	intelMaxGrants   = 200
	intelMaxText     = 300
	intelMaxPaste    = 64 << 10
	intelMaxLines    = 200
)

// --- Who can see what ---

// intelIdentity is everything an account matches grants by.
type intelIdentity struct {
	userID       int64
	characters   []int64
	corporations []int64
	alliances    []int64
	groups       []int64
	// poster is the character a report is posted as: the session
	// character when it is one of the account's, else the first.
	posterID   int64
	posterName string
}

func (app *Application) intelIdentity(ctx context.Context) intelIdentity {
	return app.intelIdentityFor(ctx, app.userID(ctx), app.SessionCharacterID(ctx))
}

// intelIdentityFor is an account's identity by its user ID: a page's
// reader (posting as their session character), or a posting token's
// account (session 0: its first character).
func (app *Application) intelIdentityFor(ctx context.Context, userID, session int64) intelIdentity {
	id := intelIdentity{userID: userID}
	if id.userID == 0 {
		return id
	}
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, id.userID)
	if err != nil {
		logging.Warnf("channels: characters of user %d: %v", id.userID, err)
	}
	seenCorp, seenAlliance := map[int64]bool{}, map[int64]bool{}
	for _, r := range rows {
		id.characters = append(id.characters, r.CharacterID)
		if id.posterID == 0 || r.CharacterID == session {
			id.posterID, id.posterName = r.CharacterID, r.Name
		}
		if r.CorporationID > 0 && !seenCorp[r.CorporationID] {
			seenCorp[r.CorporationID] = true
			id.corporations = append(id.corporations, r.CorporationID)
			if a, _ := app.corpAlliance(ctx, r.CorporationID); a > 0 && !seenAlliance[a] {
				seenAlliance[a] = true
				id.alliances = append(id.alliances, a)
			}
		}
	}
	if gs, err := app.queries.ListOrgGroupsByUser(ctx, id.userID); err == nil {
		seen := map[int64]bool{}
		for _, g := range gs {
			if !seen[g.GroupID] {
				seen[g.GroupID] = true
				id.groups = append(id.groups, g.GroupID)
			}
		}
	}
	if id.posterID == 0 {
		// A character still settling (no corporation known yet)
		// can post too.
		chars, _ := app.queries.ListCharactersByUser(ctx, id.userID)
		for _, ch := range chars {
			if id.posterID == 0 || ch.CharacterID == session {
				id.posterID, id.posterName = ch.CharacterID, ch.Name
			}
		}
	}
	return id
}

func (app *Application) accessibleChannels(ctx context.Context, id intelIdentity) []db.ListAccessibleChannelsRow {
	if id.userID == 0 {
		return nil
	}
	nz := func(s []int64) []int64 {
		if s == nil {
			return []int64{}
		}
		return s
	}
	rows, err := app.queries.ListAccessibleChannels(ctx, db.ListAccessibleChannelsParams{
		UserID: id.userID, CharacterIds: nz(id.characters), CorporationIds: nz(id.corporations),
		AllianceIds: nz(id.alliances), GroupIds: nz(id.groups),
	})
	if err != nil {
		logging.Warnf("channels: accessible for user %d: %v", id.userID, err)
		return nil
	}
	return rows
}

// channelAccess finds one channel among those the account can read.
func (app *Application) channelAccess(ctx context.Context, id intelIdentity, channelID int64) (db.ListAccessibleChannelsRow, bool) {
	for _, c := range app.accessibleChannels(ctx, id) {
		if c.ID == channelID {
			return c, true
		}
	}
	return db.ListAccessibleChannelsRow{}, false
}

// --- The system dictionary for pasted chat lines ---

type systemDict struct {
	mu     sync.Mutex
	names  map[string]db.ListSDESystemNamesRow
	loaded time.Time
}

func (app *Application) systemLookup(ctx context.Context) intelchat.Lookup {
	d := &app.intelSystems
	d.mu.Lock()
	if d.names == nil || time.Since(d.loaded) > 6*time.Hour {
		if rows, err := app.queries.ListSDESystemNames(ctx); err == nil && len(rows) > 0 {
			d.names = make(map[string]db.ListSDESystemNamesRow, len(rows))
			for _, r := range rows {
				d.names[strings.ToLower(r.Name)] = r
			}
			d.loaded = time.Now()
		}
	}
	names := d.names
	d.mu.Unlock()
	return func(lower string) (int64, string, bool) {
		r, ok := names[lower]
		return r.SystemID, r.Name, ok
	}
}

// --- Views ---

type channelListView struct {
	SignedIn bool
	Channels []channelRow
	Error    string
	// EveSynapse Axon (intel_tokens.go): the account's paired
	// devices, the downloads, and a pairing code just made.
	Tokens      []intelTokenRow
	Downloads   []intelDownload
	PairingCode string
	// SiteAddr is this site's address, which Axon asks for when
	// pairing.
	SiteAddr string
}

type channelRow struct {
	ID          int64
	Name        string
	Description string
	Owner       bool
	CanPost     bool
}

type grantView struct {
	Kind    string
	ID      int64
	Name    string
	CanPost bool
	Logo    string // image URL, "" for groups
}

type groupChoice struct {
	ID   int64
	Name string
}

// reportView is one report as the feed (and the map) shows it.
type reportView struct {
	ID         int64  `json:"id"`
	Channel    int64  `json:"ch"`
	SystemID   int64  `json:"sys"`
	SystemName string `json:"sysName"`
	SecText    string `json:"sec"`
	SecTier    int    `json:"tier"`
	Reporter   string `json:"by"`
	Text       string `json:"text"`
	Hostiles   int32  `json:"n"`
	Clear      bool   `json:"clear"`
	ScanID     string `json:"scan,omitempty"`
	Source     string `json:"src,omitempty"`
	At         int64  `json:"at"`
	Ago        string `json:"ago"`
	Mine       bool   `json:"mine,omitempty"`
	// What the report says (intel_analysis.go): its kind in words,
	// the pilots (each linking to their page), ships and gate.
	Kind      string        `json:"kind,omitempty"`
	KindLabel string        `json:"kindLabel,omitempty"`
	KindTone  string        `json:"tone,omitempty"` // the badge's class: loss, wait or kill
	Pilots    []reportPilot `json:"pilots,omitempty"`
	Ships     []reportShip  `json:"ships,omitempty"`
	Gate      string        `json:"gate,omitempty"`
	// Time is At for the templates.
	Time time.Time `json:"-"`
}

type channelView struct {
	ID          int64
	Name        string
	Description string
	Owner       bool
	CanPost     bool
	Poster      string
	Reports     []reportView
	Older       string
	LastID      int64
	Grants      []grantView
	Groups      []groupChoice
	PrefillSys  string
	// Watch is the channel's known hostiles (pilot_intel.go).
	Watch []watchRow
	Error string
}

// --- Pages ---

func (app *Application) handleChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	v := &channelListView{}
	id := app.intelIdentity(ctx)
	if id.userID > 0 {
		v.SignedIn = true
		for _, c := range app.accessibleChannels(ctx, id) {
			v.Channels = append(v.Channels, channelRow{ID: c.ID, Name: c.Name, Description: c.Description, Owner: c.OwnerUserID == id.userID, CanPost: c.CanPost})
		}
		app.fillAxon(ctx, v, id.userID)
	}
	data.Channels = v
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: "Intel Channels", Description: "Shared intel channels: report hostiles, paste intel chat, and see it all on the map.", CanonicalPath: "/intel/channels/"})
	app.render(ctx, w, http.StatusOK, "channels.html", data)
}

func (app *Application) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, "/intel/channels/")
	id := app.intelIdentity(ctx)
	name := strings.TrimSpace(r.PostFormValue("name"))
	desc := strings.TrimSpace(r.PostFormValue("description"))
	if name == "" || utf8.RuneCountInString(name) > 60 || utf8.RuneCountInString(desc) > 200 {
		back("A channel needs a name of up to 60 characters (and a description of up to 200).")
		return
	}
	owned := 0
	for _, c := range app.accessibleChannels(ctx, id) {
		if c.OwnerUserID == id.userID {
			owned++
		}
	}
	if owned >= intelMaxChannels {
		back(fmt.Sprintf("You can own up to %d channels.", intelMaxChannels))
		return
	}
	cid, err := app.queries.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: id.userID, Name: name, Description: desc, CreatedAt: time.Now().UTC()})
	if err != nil {
		logging.Errorf("channels: create: %v", err)
		back("That did not go through — please try again.")
		return
	}
	app.flash(ctx, "Channel created. Grant access below to share it.")
	http.Redirect(w, r, fmt.Sprintf("/intel/channels/%d/", cid), http.StatusSeeOther)
}

func channelIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "channelID"), 10, 64)
	return id
}

func (app *Application) handleChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	c, ok := app.channelAccess(ctx, id, channelIDParam(r))
	data := app.publicPage(ctx)
	if !ok {
		data.Channel = &channelView{Error: "This channel does not exist, or you have no access to it."}
		app.render(ctx, w, http.StatusNotFound, "channel.html", data)
		return
	}
	v := &channelView{ID: c.ID, Name: c.Name, Description: c.Description, Owner: c.OwnerUserID == id.userID, CanPost: c.CanPost, Poster: id.posterName}
	if sys, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64); err == nil && sys > 0 {
		if s, err := app.queries.GetSDESystem(ctx, sys); err == nil {
			v.PrefillSys = s.Name
		}
	}
	// Pages go back by (seen, id): ?before=<unix>.<id>.
	beforeAt, beforeID, paged := intelFeedTop(), int64(1<<62), false
	if b := r.URL.Query().Get("before"); b != "" {
		if dot := strings.IndexByte(b, '.'); dot > 0 {
			sec, err1 := strconv.ParseInt(b[:dot], 10, 64)
			bid, err2 := strconv.ParseInt(b[dot+1:], 10, 64)
			if err1 == nil && err2 == nil {
				beforeAt, beforeID, paged = time.Unix(sec, 0).UTC(), bid, true
			}
		}
	}
	v.Reports = app.channelReports(ctx, id, c.ID, 0, beforeAt, beforeID, intelFeedPage)
	v.Watch = app.channelWatch(ctx, c.ID)
	if len(v.Reports) == intelFeedPage {
		last := v.Reports[len(v.Reports)-1]
		v.Older = fmt.Sprintf("/intel/channels/%d/?before=%d.%d", c.ID, last.At, last.ID)
	}
	// The live feed asks for what was posted after the newest post
	// shown (a pasted line can be seen earlier than it was posted).
	if !paged {
		for _, rep := range v.Reports {
			v.LastID = max(v.LastID, rep.ID)
		}
	}
	if v.Owner {
		v.Grants = app.channelGrants(ctx, c.ID)
		v.Groups = app.grantableGroups(ctx, id)
	}
	data.Channel = v
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: c.Name + " · Intel channel"})
	app.render(ctx, w, http.StatusOK, "channel.html", data)
}

// channelReports lists a channel's reports as the feed shows them.
// intelFeedTop is a key past every report: the first page.
func intelFeedTop() time.Time { return time.Now().Add(time.Hour).UTC() }

func (app *Application) channelReports(ctx context.Context, id intelIdentity, channelID, after int64, beforeAt time.Time, beforeID int64, limit int) []reportView {
	rows, err := app.queries.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: channelID, AfterID: after, BeforeAt: beforeAt, BeforeID: beforeID, MaxRows: int32(limit)})
	if err != nil {
		logging.Warnf("channels: reports of %d: %v", channelID, err)
		return nil
	}
	names := app.newKillNames(ctx)
	now := time.Now()
	out := make([]reportView, 0, len(rows))
	for _, r := range rows {
		rv := reportView{ID: r.ID, Channel: r.ChannelID, SystemID: r.SystemID, Reporter: r.Reporter, Text: r.Text,
			Hostiles: r.Hostiles, Clear: r.Clear, ScanID: r.ScanID, Source: r.Source, At: r.SeenAt.Unix(), Time: r.SeenAt,
			Ago: ageText(now, r.SeenAt), Mine: r.AuthorUserID == id.userID}
		sys := names.system(r.SystemID)
		rv.SystemName = sys.Name
		rv.SecText, rv.SecTier = secRound(sys.Security)
		applyAnalysis(&rv, r.Kind, r.Analysis, names)
		out = append(out, rv)
	}
	return out
}

func (app *Application) handleChannelFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	c, ok := app.channelAccess(ctx, id, channelIDParam(r))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"reports":[]}`))
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	reports := app.channelReports(ctx, id, c.ID, after, intelFeedTop(), 1<<62, intelFeedPage)
	if reports == nil {
		reports = []reportView{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"reports": reports})
}

// --- Posting ---

// wantsJSON: the map's quick report and the feed post by script.
func wantsJSON(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

func (app *Application) handleChannelReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	cid := channelIDParam(r)
	page := fmt.Sprintf("/intel/channels/%d/", cid)
	reply := func(status int, msg string, n int) {
		if wantsJSON(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": status < 300, "message": msg, "posted": n})
			return
		}
		app.flashBack(w, r, page)(msg)
	}
	c, ok := app.channelAccess(ctx, id, cid)
	if !ok || !c.CanPost {
		reply(http.StatusForbidden, "You cannot post to this channel.", 0)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, intelMaxPaste+8<<10)
	if err := r.ParseForm(); err != nil {
		reply(http.StatusRequestEntityTooLarge, "That paste is too large.", 0)
		return
	}
	now := time.Now().UTC()
	base := db.InsertIntelReportParams{ChannelID: c.ID, AuthorUserID: id.userID, AuthorCharacterID: id.posterID, Reporter: id.posterName, CreatedAt: now, SeenAt: now}

	if lines := r.PostForm.Get("lines"); strings.TrimSpace(lines) != "" {
		if strings.Count(lines, "\n") >= intelMaxLines {
			reply(http.StatusUnprocessableEntity, fmt.Sprintf("Paste up to %d lines at a time.", intelMaxLines), 0)
			return
		}
		res := app.postChatLines(ctx, base, lines, "chat")
		posted := res.Posted
		msg := fmt.Sprintf("Posted %s from the pasted lines.", plural(posted, "report"))
		if res.Duplicates > 0 {
			msg += fmt.Sprintf(" %d already in the channel.", res.Duplicates)
		}
		if res.Skipped > 0 {
			msg += fmt.Sprintf(" %s skipped (no system named, or a question).", plural(res.Skipped, "line"))
		}
		if posted == 0 && res.Duplicates > 0 {
			reply(http.StatusOK, msg, 0)
			return
		}
		if posted == 0 {
			reply(http.StatusUnprocessableEntity, "No report found in those lines: each needs a system's name.", 0)
			return
		}
		reply(http.StatusOK, msg, posted)
		return
	}

	sysName := strings.TrimSpace(r.PostForm.Get("system"))
	if sid, err := strconv.ParseInt(r.PostForm.Get("system_id"), 10, 64); err == nil && sid > 0 {
		if s, err := app.queries.GetSDESystem(ctx, sid); err == nil {
			base.SystemID = s.SystemID
		}
	} else if sysName != "" {
		if s, err := app.queries.GetSDESystemByName(ctx, sysName); err == nil {
			base.SystemID = s.SystemID
		}
	}
	if base.SystemID == 0 {
		reply(http.StatusUnprocessableEntity, "Name the system the report is about.", 0)
		return
	}
	base.Source = "form"
	base.Clear = r.PostForm.Get("clear") != ""
	base.Text = clipRunes(strings.TrimSpace(r.PostForm.Get("text")), intelMaxText)
	base.Pilots = base.Text
	if n, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("hostiles")), 10, 32); err == nil && n > 0 && !base.Clear {
		base.Hostiles = int32(min(n, 999)) // n fits 32 bits: ParseInt above
	}
	if !base.Clear && base.Text == "" && base.Hostiles == 0 {
		reply(http.StatusUnprocessableEntity, "Say what was seen: pilots, a count, or mark it clear.", 0)
		return
	}
	if _, err := app.insertIntelReport(ctx, base, false); err != nil {
		logging.Errorf("channels: insert report: %v", err)
		reply(http.StatusInternalServerError, "That did not go through — please try again.", 0)
		return
	}
	reply(http.StatusOK, "Reported.", 1)
}

// chatPostResult is what posting chat lines came to.
type chatPostResult struct {
	Posted     int `json:"posted"`
	Duplicates int `json:"duplicates"`
	Skipped    int `json:"skipped"`
}

// postChatLines reads intel chat lines into reports on base's channel,
// as base's author. A line's speaker and timestamp, when it has them,
// are the report's; a report already in the channel (the same moment,
// system, speaker and words) is not stored twice, so lines sent again
// -- a pasted log overlapping the last, an Axon restarting, two
// characters logging the same channel -- land once.
func (app *Application) postChatLines(ctx context.Context, base db.InsertIntelReportParams, lines, source string) chatPostResult {
	var res chatPostResult
	now := base.CreatedAt
	o := intelchat.Parse(lines, app.systemLookup(ctx), now)
	res.Skipped = o.NoSystem + o.Questions
	for _, rep := range o.Reports {
		p := base
		p.Source, p.SystemID, p.Clear, p.Hostiles = source, rep.SystemID, rep.Clear, int32(min(max(rep.Hostiles, 0), 999))
		p.Text = clipRunes(rep.Text, intelMaxText)
		p.Pilots = p.Text
		if rep.Speaker != "" {
			p.Reporter = clipRunes(rep.Speaker, 40)
		}
		if !rep.At.IsZero() && rep.At.Before(now.Add(time.Minute)) && rep.At.After(now.Add(-intelRetention)) {
			p.SeenAt = rep.At
			dup, err := app.queries.IntelReportExists(ctx, db.IntelReportExistsParams{ChannelID: p.ChannelID, SeenAt: p.SeenAt, SystemID: p.SystemID, Reporter: p.Reporter, Text: p.Text})
			if err == nil && dup {
				res.Duplicates++
				continue
			}
		}
		if _, err := app.insertIntelReport(ctx, p, rep.NoVisual); err != nil {
			logging.Errorf("channels: insert chat report: %v", err)
			continue
		}
		res.Posted++
	}
	return res
}

// handleScanPost is POST /intel/scan/{id}/post: a scan from the scan
// tool, into a channel, as a report on its system.
func (app *Application) handleScanPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scanID := chi.URLParam(r, "id")
	back := app.flashBack(w, r, "/intel/scan/"+scanID+"/")
	if !validScanID(scanID) {
		back("This scan does not exist.")
		return
	}
	row, err := app.queries.GetScan(ctx, scanID)
	if err != nil {
		back("This scan does not exist.")
		return
	}
	id := app.intelIdentity(ctx)
	cid, _ := strconv.ParseInt(r.PostFormValue("channel"), 10, 64)
	c, ok := app.channelAccess(ctx, id, cid)
	if !ok || !c.CanPost {
		back("You cannot post to that channel.")
		return
	}
	var res scanResult
	_ = json.Unmarshal([]byte(row.Result), &res)
	if row.SystemID == 0 {
		back("This scan has no system, so it cannot be a report. Paste it again naming the system.")
		return
	}
	now := time.Now().UTC()
	p := db.InsertIntelReportParams{ChannelID: c.ID, SystemID: row.SystemID, AuthorUserID: id.userID, AuthorCharacterID: id.posterID,
		Reporter: id.posterName, Text: scanTitle(res), ScanID: scanID, Source: "scan", SeenAt: row.CreatedAt, CreatedAt: now}
	switch {
	case res.DScan != nil:
		p.Hostiles = int32(res.DScan.Ships)
	case res.Local != nil:
		p.Hostiles = int32(len(res.Local.Pilots) + len(res.Local.Unknown))
	case res.Fleet != nil:
		p.Hostiles = int32(len(res.Fleet.Members))
	}
	if _, err := app.insertIntelReport(ctx, p, false); err != nil {
		logging.Errorf("channels: insert scan report: %v", err)
		back("That did not go through — please try again.")
		return
	}
	back("Posted to " + c.Name + ".")
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (app *Application) handleReportDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	cid := channelIDParam(r)
	back := app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", cid))
	c, ok := app.channelAccess(ctx, id, cid)
	rid, _ := strconv.ParseInt(chi.URLParam(r, "reportID"), 10, 64)
	rep, err := app.queries.GetIntelReport(ctx, rid)
	if !ok || err != nil || rep.ChannelID != c.ID || (rep.AuthorUserID != id.userID && c.OwnerUserID != id.userID) {
		back("You cannot remove that report.")
		return
	}
	if err := app.queries.DeleteIntelReport(ctx, rid); err != nil {
		back("That did not go through — please try again.")
		return
	}
	back("Report removed.")
}

// --- Managing (the owner only) ---

// ownedChannel finds a channel the account owns.
func (app *Application) ownedChannel(ctx context.Context, r *http.Request) (db.IntelChannel, intelIdentity, bool) {
	id := app.intelIdentity(ctx)
	c, err := app.queries.GetIntelChannel(ctx, channelIDParam(r))
	if err != nil || c.OwnerUserID != id.userID || id.userID == 0 {
		return db.IntelChannel{}, id, false
	}
	return c, id, true
}

func (app *Application) handleChannelEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, _, ok := app.ownedChannel(ctx, r)
	back := app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", channelIDParam(r)))
	if !ok {
		back("Only the channel's owner can change it.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	desc := strings.TrimSpace(r.PostFormValue("description"))
	if name == "" || utf8.RuneCountInString(name) > 60 || utf8.RuneCountInString(desc) > 200 {
		back("A channel needs a name of up to 60 characters (and a description of up to 200).")
		return
	}
	if err := app.queries.UpdateIntelChannel(ctx, db.UpdateIntelChannelParams{ID: c.ID, Name: name, Description: desc}); err != nil {
		back("That did not go through — please try again.")
		return
	}
	back("Saved.")
}

func (app *Application) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, _, ok := app.ownedChannel(ctx, r)
	if !ok {
		app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", channelIDParam(r)))("Only the channel's owner can delete it.")
		return
	}
	if r.PostFormValue("confirm") != c.Name {
		app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", c.ID))("Type the channel's name to delete it.")
		return
	}
	if err := app.queries.DeleteIntelChannel(ctx, c.ID); err != nil {
		app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", c.ID))("That did not go through — please try again.")
		return
	}
	app.flashBack(w, r, "/intel/channels/")("Deleted " + c.Name + ".")
}

var grantKinds = map[string]bool{"character": true, "corporation": true, "alliance": true, "group": true}

func (app *Application) handleGrantAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, id, ok := app.ownedChannel(ctx, r)
	back := app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/#access", channelIDParam(r)))
	if !ok {
		back("Only the channel's owner can grant access.")
		return
	}
	kind := r.PostFormValue("kind")
	canPost := r.PostFormValue("post") != ""
	if !grantKinds[kind] {
		back("Choose who to grant: a pilot, a corporation, an alliance or a group.")
		return
	}
	if n, err := app.queries.CountIntelChannelGrants(ctx, c.ID); err == nil && n >= intelMaxGrants {
		back(fmt.Sprintf("A channel holds up to %d grants.", intelMaxGrants))
		return
	}
	var target int64
	var label string
	if kind == "group" {
		gid, _ := strconv.ParseInt(r.PostFormValue("group"), 10, 64)
		for _, g := range app.grantableGroups(ctx, id) {
			if g.ID == gid {
				target, label = g.ID, g.Name
			}
		}
		if target == 0 {
			back("Choose one of your corporation's or alliance's groups.")
			return
		}
	} else {
		name := strings.TrimSpace(r.PostFormValue("name"))
		var problem string
		target, label, problem = app.resolveGrantName(ctx, kind, name)
		if problem != "" {
			back(problem)
			return
		}
	}
	if err := app.queries.UpsertIntelChannelGrant(ctx, db.UpsertIntelChannelGrantParams{ChannelID: c.ID, Kind: kind, TargetID: target, CanPost: canPost, CreatedAt: time.Now().UTC()}); err != nil {
		back("That did not go through — please try again.")
		return
	}
	how := "can read"
	if canPost {
		how = "can read and post"
	}
	back(label + " " + how + ".")
}

// resolveGrantName finds a pilot, corporation or alliance by its exact
// name through ESI. problem, when not empty, is what to tell the
// owner instead.
func (app *Application) resolveGrantName(ctx context.Context, kind, name string) (id int64, label, problem string) {
	if name == "" || utf8.RuneCountInString(name) > 50 {
		return 0, "", "Type the exact name to grant."
	}
	var ids struct {
		Characters []struct {
			ID   int64
			Name string
		} `json:"characters"`
		Corporations []struct {
			ID   int64
			Name string
		} `json:"corporations"`
		Alliances []struct {
			ID   int64
			Name string
		} `json:"alliances"`
	}
	if err := app.esi.PostJSON(ctx, "/universe/ids/", []string{name}, &ids); err != nil {
		logging.Warnf("channels: resolve %s %q: %v", kind, name, err)
		return 0, "", "EVE's servers did not answer — try again in a moment."
	}
	var list []struct {
		ID   int64
		Name string
	}
	switch kind {
	case "character":
		list = ids.Characters
	case "corporation":
		list = ids.Corporations
	case "alliance":
		list = ids.Alliances
	}
	for _, e := range list {
		if strings.EqualFold(e.Name, name) {
			switch kind {
			case "character":
				app.esi.StoreCharacterName(e.ID, e.Name)
			case "corporation":
				app.esi.StoreCorpName(e.ID, e.Name)
			case "alliance":
				app.esi.StoreAllianceName(e.ID, e.Name)
			}
			return e.ID, e.Name, ""
		}
	}
	return 0, "", fmt.Sprintf("No %s is named “%s”.", map[string]string{"character": "pilot", "corporation": "corporation", "alliance": "alliance"}[kind], name)
}

func (app *Application) handleGrantRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, _, ok := app.ownedChannel(ctx, r)
	back := app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/#access", channelIDParam(r)))
	if !ok {
		back("Only the channel's owner can change access.")
		return
	}
	target, _ := strconv.ParseInt(r.PostFormValue("target"), 10, 64)
	if err := app.queries.DeleteIntelChannelGrant(ctx, db.DeleteIntelChannelGrantParams{ChannelID: c.ID, Kind: r.PostFormValue("kind"), TargetID: target}); err != nil {
		back("That did not go through — please try again.")
		return
	}
	back("Access removed.")
}

func (app *Application) channelGrants(ctx context.Context, channelID int64) []grantView {
	rows, err := app.queries.ListIntelChannelGrants(ctx, channelID)
	if err != nil {
		return nil
	}
	names := app.newKillNames(ctx)
	out := make([]grantView, 0, len(rows))
	for _, g := range rows {
		v := grantView{Kind: g.Kind, ID: g.TargetID, CanPost: g.CanPost}
		switch g.Kind {
		case "character":
			v.Name, v.Logo = names.character(g.TargetID), fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=32", g.TargetID)
		case "corporation":
			v.Name, v.Logo = names.corp(g.TargetID), fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=32", g.TargetID)
		case "alliance":
			v.Name, v.Logo = names.alliance(g.TargetID), fmt.Sprintf("https://images.evetech.net/alliances/%d/logo?size=32", g.TargetID)
		case "group":
			v.Name = "Group #" + strconv.FormatInt(g.TargetID, 10)
			if og, err := app.queries.GetOrgGroup(ctx, g.TargetID); err == nil {
				v.Name = og.Name
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return grantOrder[out[i].Kind] < grantOrder[out[j].Kind]
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

var grantOrder = map[string]int{"alliance": 0, "corporation": 1, "group": 2, "character": 3}

// grantableGroups are the groups an owner may grant: those kept by
// their characters' corporations and alliances.
func (app *Application) grantableGroups(ctx context.Context, id intelIdentity) []groupChoice {
	var out []groupChoice
	add := func(kind string, owner int64) {
		gs, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: kind, OwnerID: owner})
		if err != nil {
			return
		}
		for _, g := range gs {
			out = append(out, groupChoice{ID: g.ID, Name: g.Name})
		}
	}
	for _, a := range id.alliances {
		add("alliance", a)
	}
	for _, c := range id.corporations {
		add("corporation", c)
	}
	return out
}

// --- The map ---

// handleMapIntel is GET /map/intel.json: the channels its reader can
// see, and their last 30 minutes of reports.
func (app *Application) handleMapIntel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type channel struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Post bool   `json:"post"`
	}
	type report struct {
		ID    int64  `json:"id"`
		Ch    int64  `json:"ch"`
		Sys   int64  `json:"sys"`
		By    string `json:"by"`
		Text  string `json:"text"`
		N     int32  `json:"n"`
		Clear bool   `json:"clear,omitempty"`
		Scan  string `json:"scan,omitempty"`
		At    int64  `json:"at"`
		Kind  string `json:"kind,omitempty"` // what the report is (intel_analysis.go)
	}
	out := struct {
		Now      int64     `json:"now"`
		Channels []channel `json:"channels"`
		Reports  []report  `json:"reports"`
	}{Now: time.Now().Unix(), Channels: []channel{}, Reports: []report{}}
	id := app.intelIdentity(ctx)
	var ids []int64
	for _, c := range app.accessibleChannels(ctx, id) {
		out.Channels = append(out.Channels, channel{ID: c.ID, Name: c.Name, Post: c.CanPost})
		ids = append(ids, c.ID)
	}
	if len(ids) > 0 {
		rows, err := app.queries.ListRecentIntelReports(ctx, db.ListRecentIntelReportsParams{ChannelIds: ids, Since: time.Now().Add(-intelLive)})
		if err != nil {
			logging.Warnf("channels: map reports: %v", err)
		}
		for _, r := range rows {
			out.Reports = append(out.Reports, report{ID: r.ID, Ch: r.ChannelID, Sys: r.SystemID, By: r.Reporter, Text: r.Text, N: r.Hostiles, Clear: r.Clear, Scan: r.ScanID, At: r.SeenAt.Unix(), Kind: r.Kind})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// pruneIntelReports drops reports past their 7 days.
func (app *Application) pruneIntelReports(ctx context.Context) {
	n, err := app.queries.PruneIntelReports(ctx, time.Now().Add(-intelRetention))
	if err != nil {
		logging.Warnf("channels: prune: %v", err)
		return
	}
	if n > 0 {
		logging.Infof("channels: pruned %d reports past 7 days", n)
	}
}
