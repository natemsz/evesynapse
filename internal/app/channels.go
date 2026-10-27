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
// as they age; a route names the reported systems it crosses but never
// steers around them, since a report is not danger. Reports are
// kept 7 days.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
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
	"evesynapse/internal/esi"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/intelchat"
	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
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
	// standing is the account's standing (internal/standing), for
	// organisation channels' rules; managed, the organisations whose
	// channels it manages (channels_orgs.go).
	standing orgStanding
	managed  []orgOwner
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
	if st, err := app.standingFor(ctx, id.userID); err == nil {
		id.standing = st
		id.managed = app.intelManagedOrgs(ctx, st)
	} else {
		logging.Warnf("channels: standing of user %d: %v", id.userID, err)
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

// channelAccess finds one channel among those the account can read
// (accessibleChannels, channels_orgs.go).
func (app *Application) channelAccess(ctx context.Context, id intelIdentity, channelID int64) (accessChannel, bool) {
	for _, c := range app.accessibleChannels(ctx, id) {
		if c.ID == channelID {
			return c, true
		}
	}
	return accessChannel{}, false
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
	// Owners are who a new channel can belong to: just the reader,
	// or an organisation whose channels they manage.
	Owners []channelOwnerOption
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
	Owner       bool // the reader manages it
	CanPost     bool
	Org         string // the owner organisation's name, "" for a personal channel
}

type grantView struct {
	Kind    string
	ID      int64
	Name    string
	CanPost bool
	Logo    string // image URL, "" for groups
	Who     string // a rule grant's "Who" choice (channels_orgs.go)
}

type groupChoice struct {
	ID   int64
	Name string
}

// reportView is one report as the feed (and the map) shows it.
type reportView struct {
	ID         int64  `json:"id"`
	Channel    int64  `json:"ch"`
	ChName     string `json:"chn,omitempty"` // the channel's name: set on the cross-channel feed only
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
	GateID    int64         `json:"gateId,omitempty"`
	// Split is the report's standing split as proportional
	// segments for the stats bar, from the counts stored with
	// the report at ingest.
	Split []reportSeg `json:"split,omitempty"`
	// Gang is the tracked gang this report's group stands in,
	// when fusion tracks one sighted beyond this group alone
	// (intel_gangs.go): the card's gang event line.
	Gang *reportGang `json:"gang,omitempty"`
	// NoDoors is set when the card is shown on the report's own
	// system page, where its system doors would point at the
	// page itself.
	NoDoors bool `json:"-"`
	// Time is At for the templates.
	Time time.Time `json:"-"`
}

// reportSeg is one segment of a report's standing-split bar:
// its name, its count, its share of the whole in percent, and
// its class, which names its colour in channels.css.
type reportSeg struct {
	Name  string `json:"name"`
	N     int    `json:"n"`
	Pct   int    `json:"pct"`
	Class string `json:"class"`
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
	// Org is the owner organisation's name ("" for a personal
	// channel); RuleChoices, who in it the channel can be shared with.
	Org         string
	RuleChoices []whoChoice
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
			v.Channels = append(v.Channels, app.channelRowOf(ctx, c))
		}
		v.Owners = app.channelOwnerOptions(ctx, id)
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
	// Personal, or an organisation's the reader manages channels for.
	var org orgOwner
	isOrg := false
	if raw := r.PostFormValue("owner"); raw != "" {
		o, ok := parseOrgOwner(raw)
		if !ok || !ownersInclude(id.managed, o) {
			back("You can't make channels for that corporation or alliance.")
			return
		}
		org, isOrg = o, true
	}
	owned := 0
	for _, c := range app.accessibleChannels(ctx, id) {
		if o, ok := c.Org(); (isOrg && ok && o == org) || (!isOrg && !ok && c.OwnerUserID == id.userID) {
			owned++
		}
	}
	if owned >= intelMaxChannels {
		if isOrg {
			back(fmt.Sprintf("A corporation or alliance can have up to %d channels.", intelMaxChannels))
		} else {
			back(fmt.Sprintf("You can own up to %d channels.", intelMaxChannels))
		}
		return
	}
	cid, err := app.queries.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: id.userID, Name: name, Description: desc,
		CreatedAt: time.Now().UTC(), OwnerKind: org.Kind, OwnerID: org.ID})
	if err != nil {
		logging.Errorf("channels: create: %v", err)
		back("That did not go through — please try again.")
		return
	}
	if isOrg {
		// An organisation's channel starts open to its members.
		app.addChannelRule(ctx, org, cid, ruleMember, true)
		app.flash(ctx, "Channel created. Every member of "+app.ownerName(ctx, org)+" can read and post; change who below.")
	} else {
		app.flash(ctx, "Channel created. Grant access below to share it.")
	}
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
	v := &channelView{ID: c.ID, Name: c.Name, Description: c.Description, Owner: c.Manage, CanPost: c.CanPost, Poster: id.posterName}
	org, isOrg := c.Org()
	if isOrg {
		v.Org = app.ownerName(ctx, org)
	}
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
		if isOrg {
			v.Grants = append(app.channelRuleGrants(ctx, org, c.ID), v.Grants...)
			v.RuleChoices = app.whoChoices(ctx, org)
		}
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
	var groupIDs []int64
	for _, r := range rows {
		if r.GroupID > 0 {
			groupIDs = append(groupIDs, r.GroupID)
		}
	}
	comps := app.groupCompositesFor(ctx, groupIDs)
	gangs := app.gangsForGroups(ctx, groupIDs)
	out := make([]reportView, 0, len(rows))
	for _, r := range rows {
		rv := reportView{ID: r.ID, Channel: r.ChannelID, SystemID: r.SystemID, Reporter: r.Reporter, Text: r.Text,
			Hostiles: r.Hostiles, Clear: r.Clear, ScanID: r.ScanID, Source: r.Source, At: r.SeenAt.Unix(), Time: r.SeenAt,
			Ago: ageText(now, r.SeenAt), Mine: r.AuthorUserID == id.userID}
		sys := names.system(r.SystemID)
		rv.SystemName = sys.Name
		rv.SecText, rv.SecTier = secRound(sys.Security)
		// A grouped report's card speaks for its group: the
		// composite's headline and counts, not its own alone.
		if comp, ok := comps[r.GroupID]; ok && len(comp.Members) > 1 {
			app.applyCompositeCard(ctx, &rv, r.Kind, comp, names)
		} else {
			applyAnalysis(&rv, r.Kind, r.Analysis, names)
		}
		if g := gangs[r.GroupID]; g.showsAsEvent() {
			rv.Gang = app.reportGangView(ctx, g, r.SystemID, now)
		}
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

// --- The cross-channel feed ---

// intelFeedView is the feed page: the newest reports across every
// channel its reader can reach, each card naming its channel.
type intelFeedView struct {
	Reports  []reportView
	Older    string
	LastID   int64
	Channels int // how many channels feed it
	// Systems is the front page's systems at a glance, busiest
	// first: which systems the reports are about, how many
	// reports and the largest hostile count among them.
	Systems []feedSystem
}

// feedSystem is one system's line in the feed's summary: its
// name and security (for the map link), how many of the page's
// reports name it, the largest hostile count among them, and
// the newest report's age.
type feedSystem struct {
	ID       int64
	Name     string
	SecText  string
	SecTier  int
	Reports  int
	Hostiles int32
	Ago      string
}

// feedSystems summarises the reports a feed page already holds
// by system: no extra read, the page's own sixty at most.
func feedSystems(reports []reportView) []feedSystem {
	byID := map[int64]*feedSystem{}
	var order []*feedSystem
	for _, r := range reports {
		if r.SystemID == 0 {
			continue
		}
		s := byID[r.SystemID]
		if s == nil {
			s = &feedSystem{ID: r.SystemID, Name: r.SystemName, SecText: r.SecText, SecTier: r.SecTier, Ago: r.Ago}
			byID[r.SystemID] = s
			order = append(order, s)
		}
		s.Reports++
		if !r.Clear && r.Hostiles > s.Hostiles {
			s.Hostiles = r.Hostiles
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Reports != order[j].Reports {
			return order[i].Reports > order[j].Reports
		}
		return order[i].Hostiles > order[j].Hostiles
	})
	if len(order) > 6 {
		order = order[:6]
	}
	out := make([]feedSystem, 0, len(order))
	for _, s := range order {
		out = append(out, *s)
	}
	return out
}

// feedReports lists reports across channels as the feed shows them:
// channelReports across more than one channel, each view naming
// its channel.
func (app *Application) feedReports(ctx context.Context, id intelIdentity, channels []accessChannel, after int64, beforeAt time.Time, beforeID int64, limit int) []reportView {
	if len(channels) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(channels))
	namesByID := make(map[int64]string, len(channels))
	for _, c := range channels {
		ids = append(ids, c.ID)
		namesByID[c.ID] = c.Name
	}
	rows, err := app.queries.ListIntelReportsAcrossChannels(ctx, db.ListIntelReportsAcrossChannelsParams{ChannelIds: ids, AfterID: after, BeforeAt: beforeAt, BeforeID: beforeID, MaxRows: int32(limit)})
	if err != nil {
		logging.Warnf("channels: feed reports: %v", err)
		return nil
	}
	names := app.newKillNames(ctx)
	now := time.Now()
	var groupIDs []int64
	for _, r := range rows {
		if r.GroupID > 0 {
			groupIDs = append(groupIDs, r.GroupID)
		}
	}
	comps := app.groupCompositesFor(ctx, groupIDs)
	gangs := app.gangsForGroups(ctx, groupIDs)
	out := make([]reportView, 0, len(rows))
	for _, r := range rows {
		rv := reportView{ID: r.ID, Channel: r.ChannelID, ChName: namesByID[r.ChannelID], SystemID: r.SystemID, Reporter: r.Reporter, Text: r.Text,
			Hostiles: r.Hostiles, Clear: r.Clear, ScanID: r.ScanID, Source: r.Source, At: r.SeenAt.Unix(), Time: r.SeenAt,
			Ago: ageText(now, r.SeenAt), Mine: r.AuthorUserID == id.userID}
		sys := names.system(r.SystemID)
		rv.SystemName = sys.Name
		rv.SecText, rv.SecTier = secRound(sys.Security)
		if comp, ok := comps[r.GroupID]; ok && len(comp.Members) > 1 {
			app.applyCompositeCard(ctx, &rv, r.Kind, comp, names)
		} else {
			applyAnalysis(&rv, r.Kind, r.Analysis, names)
		}
		if g := gangs[r.GroupID]; g.showsAsEvent() {
			rv.Gang = app.reportGangView(ctx, g, r.SystemID, now)
		}
		out = append(out, rv)
	}
	return out
}

func (app *Application) handleIntelFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	channels := app.accessibleChannels(ctx, id)
	data := app.publicPage(ctx)
	v := &intelFeedView{Channels: len(channels)}
	// Pages go back by (seen, id), as a channel's do: ?before=<unix>.<id>.
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
	v.Reports = app.feedReports(ctx, id, channels, 0, beforeAt, beforeID, intelFeedPage)
	if !paged {
		v.Systems = feedSystems(v.Reports)
	}
	if len(v.Reports) == intelFeedPage {
		last := v.Reports[len(v.Reports)-1]
		v.Older = fmt.Sprintf("/intel/feed/?before=%d.%d", last.At, last.ID)
	}
	if !paged {
		for _, rep := range v.Reports {
			v.LastID = max(v.LastID, rep.ID)
		}
	}
	data.IntelFeed = v
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: "Intel Feed", Description: "The newest reports across your intel channels.", CanonicalPath: "/intel/feed/"})
	app.render(ctx, w, http.StatusOK, "feed.html", data)
}

// handleIntelFeedJSON is GET /intel/feed.json: the feed page's live
// poll, the reports across the reader's channels posted after
// after_id.
func (app *Application) handleIntelFeedJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	reports := app.feedReports(ctx, id, app.accessibleChannels(ctx, id), after, intelFeedTop(), 1<<62, intelFeedPage)
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
	// postedReportID, once a post below lands one report, rides
	// the JSON reply: the workbench's paste drawer navigates to
	// the report it just made.
	var postedReportID int64
	reply := func(status int, msg string, n int) {
		if wantsJSON(r) {
			body := map[string]any{"ok": status < 300, "message": msg, "posted": n}
			if postedReportID > 0 {
				body["reportId"] = postedReportID
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
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
		// linesReply is reply, with the lines that named no system
		// named in the answer: shown to the poster as written, never
		// silently dropped.
		linesReply := func(status int, msg string, n int, skipped []string) {
			if wantsJSON(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": status < 300, "message": msg, "posted": n, "skippedLines": skipped})
				return
			}
			if len(skipped) > 0 {
				shown := skipped
				if len(shown) > 3 {
					shown = shown[:3]
				}
				quoted := make([]string, len(shown))
				for i, s := range shown {
					quoted[i] = "“" + clipRunes(s, 80) + "”"
				}
				msg += " Not read: " + strings.Join(quoted, " · ")
				if len(skipped) > len(shown) {
					msg += fmt.Sprintf(" and %d more.", len(skipped)-len(shown))
				}
			}
			app.flashBack(w, r, page)(msg)
		}
		res := app.postChatLines(ctx, base, lines, "chat", "")
		posted := res.Posted
		msg := fmt.Sprintf("Posted %s from the pasted lines.", plural(posted, "report"))
		if res.Duplicates > 0 {
			msg += fmt.Sprintf(" %d already in the channel.", res.Duplicates)
		}
		if res.Skipped > 0 {
			msg += fmt.Sprintf(" %s skipped (no system named, or a question).", plural(res.Skipped, "line"))
		}
		if posted == 0 && res.Duplicates > 0 {
			linesReply(http.StatusOK, msg, 0, res.SkippedLines)
			return
		}
		if posted == 0 {
			linesReply(http.StatusUnprocessableEntity, "No report found in those lines: each needs a system's name.", 0, res.SkippedLines)
			return
		}
		linesReply(http.StatusOK, msg, posted, res.SkippedLines)
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
	base.Source = "form"
	base.Clear = r.PostForm.Get("clear") != ""
	text := strings.TrimSpace(r.PostForm.Get("text"))
	base.Text = clipRunes(text, intelMaxText)
	base.Pilots = base.Text
	if n, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("hostiles")), 10, 32); err == nil && n > 0 && !base.Clear {
		base.Hostiles = int32(min(n, 999)) // n fits 32 bits: ParseInt above
	}
	if !base.Clear && base.Text == "" && base.Hostiles == 0 {
		reply(http.StatusUnprocessableEntity, "Say what was seen: pilots, a count, or mark it clear.", 0)
		return
	}
	// A scan pasted into the report box is a scan, not prose:
	// the report is the one its reading makes, on the system
	// named here -- or, naming none, on the one the paste names
	// or the poster's own current system (scanReportOf's
	// precedence). (Classified on the text as pasted, before the
	// clip above shortened it.)
	if !base.Clear {
		if p, counts, ok := app.scanReportOf(ctx, base, text, base.SystemID, ""); ok {
			id, err := app.insertIntelReport(ctx, p, false, counts)
			switch {
			case err == nil:
				postedReportID = id
			case errors.Is(err, errIntelDuplicate):
				// The reading already stands in the channel:
				// the drawer opens the standing report.
				if standing, gerr := app.queries.GetIntelReportByScanID(ctx, p.ScanID); gerr == nil {
					postedReportID = standing.ID
				}
			default:
				logging.Errorf("channels: insert report: %v", err)
				reply(http.StatusInternalServerError, "That did not go through — please try again.", 0)
				return
			}
			reply(http.StatusOK, "Reported.", 1)
			return
		}
	}
	if base.SystemID == 0 {
		reply(http.StatusUnprocessableEntity, "Name the system the report is about.", 0)
		return
	}
	if id, err := app.insertIntelReport(ctx, base, false, nil); err == nil {
		postedReportID = id
	} else if !errors.Is(err, errIntelDuplicate) {
		logging.Errorf("channels: insert report: %v", err)
		reply(http.StatusInternalServerError, "That did not go through — please try again.", 0)
		return
	}
	reply(http.StatusOK, "Reported.", 1)
}

// chatPostResult is what posting chat lines came to. SkippedLines
// are the lines that named no system, kept as written so the poster
// can be shown what was not read.
type chatPostResult struct {
	Posted       int      `json:"posted"`
	Duplicates   int      `json:"duplicates"`
	Skipped      int      `json:"skipped"`
	SkippedLines []string `json:"skippedLines,omitempty"`
}

// posterSystemID is the system the poster's character is in, as
// the site last synced it: their stored location snapshot. It is
// the fallback system for a paste that names none -- the site
// knows where its reporter is, and a roster from there is about
// there. 0 when no location is on file: no location is ever
// invented, and the paste is refused exactly as before.
func (app *Application) posterSystemID(ctx context.Context, characterID int64) int64 {
	if characterID <= 0 {
		return 0
	}
	var loc esi.Location
	if !app.loadCorpSnapshot(ctx, characterID, esi.SnapLocation, &loc) {
		return 0
	}
	return loc.SolarSystemID
}

// speakerSystemID is the system a chat line's speaker is in, as
// the site last synced it: the speaker's name resolved through
// the verified pilot names to their character, then that
// character's stored location, read as posterSystemID reads the
// poster's. 0 when the speaker is no pilot the site has
// verified, or has no location on file. A relayed log's lines
// belong to their speaker, not to the character the relay posts
// as: the roster the speaker pasted is about where the speaker
// sits.
func (app *Application) speakerSystemID(ctx context.Context, speaker string) int64 {
	if speaker == "" {
		return 0
	}
	row, err := app.queries.GetIntelPilotName(ctx, strings.ToLower(speaker))
	if err != nil || row.CharacterID <= 0 {
		return 0
	}
	return app.posterSystemID(ctx, row.CharacterID)
}

// accountSpeaker checks a batch's claimed speaker against the
// account posting it: the name resolves through the verified
// pilot names, the way a chat line's speaker resolves, and the
// character it names is one of the account's own. It returns
// the name as verified, for the batch to be read as that
// pilot's, or "" when the claim names no verified pilot, or a
// pilot of another account: a batch files under the posting
// account's own pilots only, and a claim that fails the check
// leaves the batch exactly as if it named no speaker.
func (app *Application) accountSpeaker(ctx context.Context, userID int64, speaker string) string {
	speaker = strings.TrimSpace(speaker)
	if speaker == "" || userID <= 0 {
		return ""
	}
	row, err := app.queries.GetIntelPilotName(ctx, strings.ToLower(speaker))
	if err != nil || row.CharacterID <= 0 {
		return ""
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return ""
	}
	for _, ch := range chars {
		if ch.CharacterID == row.CharacterID {
			return row.Name
		}
	}
	return ""
}

// scanReportOf reads raw as a scan paste, when it is one, and
// makes the report a reading of it makes: the reading is stored
// (as the scan tool stores it) and the report carries the
// reading's interpreted sentence, its system and its headline
// count -- never the raw paste. Which system, in order: the one
// the reading finds in the paste, the one the poster named
// (namedSystemID), and -- a paste needing a system and naming
// none -- the line's speaker's own current system when the
// paste is a chat line with one, else the poster's, from their
// stored locations; the sentence then says so. ok is false when
// the paste is no scan the tool knows, or no system is had any
// of those ways: a local list on its own cannot be a report,
// reports being about a system.
func (app *Application) scanReportOf(ctx context.Context, base db.InsertIntelReportParams, raw string, namedSystemID int64, speaker string) (db.InsertIntelReportParams, *intelanalyze.StandingCounts, bool) {
	return app.scanReportAs(ctx, base, raw, submitOverride{}, namedSystemID, speaker)
}

// scanReportAs is scanReportOf under a submission's overrides
// (submitOverride): a kind read the paste as that kind, and a
// system files the reading there, over anything the paste says.
func (app *Application) scanReportAs(ctx context.Context, base db.InsertIntelReportParams, raw string, ov submitOverride, namedSystemID int64, speaker string) (db.InsertIntelReportParams, *intelanalyze.StandingCounts, bool) {
	kind := ov.scanKind()
	if kind == "" && scan.Detect(raw) == "" {
		return base, nil, false
	}
	p, err := scan.ParseAs(raw, kind)
	if err != nil {
		return base, nil, false
	}
	if ov.SystemID > 0 {
		namedSystemID = ov.SystemID
	}
	res := app.readScan(ctx, p, namedSystemID)
	if ov.SystemID > 0 {
		res.SystemID = ov.SystemID
	}
	return app.readingReportOf(ctx, base, res, &p, raw, speaker)
}

// fusedScanReportOf is scanReportOf for a message whose scan the
// client fused into one line: Detect cannot see it, so the rows
// are reconstructed (reconstructFusedDScan) and the reading must
// clear the roster re-read's standard -- it accounts for most
// of the message, and more of it than the plain reading's tags
// do -- before it is stored the same way as any scan report.
func (app *Application) fusedScanReportOf(ctx context.Context, base db.InsertIntelReportParams, raw string, namedSystemID int64, speaker string) (db.InsertIntelReportParams, *intelanalyze.StandingCounts, bool) {
	parsed, covered, total := app.reconstructFusedDScan(ctx, raw)
	if len(parsed.DScan) == 0 || total == 0 || covered*100 < total*intelanalyze.FusedMinCoverPct {
		return base, nil, false
	}
	plain := intelanalyze.Analyze(intelanalyze.Input{Text: raw}, app.intelDict(ctx, base.ChannelID, map[int64]bool{}))
	if covered <= taggedCoverage(raw, plain.Pilots) {
		return base, nil, false
	}
	return app.readingReportOf(ctx, base, app.readScan(ctx, parsed, namedSystemID), &parsed, raw, speaker)
}

// readingReportOf stores one scan reading as a report in the
// base's channel: the scan row under a fresh ID, the sentence
// scanReportText writes for the reading, and the reading's own
// counts. The system is the reading's, and -- a reading with
// none -- the line's speaker's own current system when the
// paste is a chat line with one, else the poster's, from their
// stored locations; the sentence then says so. ok is false
// when no system is had any of those ways.
func (app *Application) readingReportOf(ctx context.Context, base db.InsertIntelReportParams, res scanResult, p *scan.Result, raw string, speaker string) (db.InsertIntelReportParams, *intelanalyze.StandingCounts, bool) {
	fromPoster := false
	if res.SystemID == 0 {
		// The speaker before the poster: a chat line is its
		// speaker's report, and a relay posts it as another
		// character entirely.
		res.SystemID = app.speakerSystemID(ctx, speaker)
		if res.SystemID == 0 {
			res.SystemID = app.posterSystemID(ctx, base.AuthorCharacterID)
		}
		fromPoster = res.SystemID != 0
	}
	if res.SystemID == 0 {
		return base, nil, false
	}
	body, err := json.Marshal(res)
	if err != nil {
		return base, nil, false
	}
	id, err := newScanID()
	if err == nil {
		err = app.queries.InsertScan(ctx, db.InsertScanParams{ID: id, Kind: string(res.Kind), SystemID: res.SystemID, UserID: base.AuthorUserID, Raw: raw, Result: string(body), CreatedAt: time.Now().UTC()})
	}
	if err != nil {
		logging.Errorf("channels: store scan from a paste: %v", err)
		return base, nil, false
	}
	out := base
	out.SystemID = res.SystemID
	text, split := app.scanReportText(ctx, base.ChannelID, res, p, base.AuthorCharacterID)
	if fromPoster {
		// The system came from the reporter's own location,
		// not the paste: the sentence says so, quietly.
		text = strings.TrimSuffix(text, ".") + " — system from the reporter's current location."
	}
	out.Text = text
	counts := &intelanalyze.StandingCounts{Hostile: split[bucketHostile], Friendly: split[bucketFriendly], Neutral: split[bucketNeutral], Unclassified: split[bucketUnknown]}
	out.Pilots = out.Text
	// A count the poster typed is theirs and survives
	// classification; without one (chat lines, the scan tool),
	// the report carries the reading's count. An incoming
	// Hostiles is only ever set by the form's count field.
	if base.Hostiles == 0 {
		out.Hostiles = scanHostiles(res)
	}
	out.Clear = false
	out.ScanID = id
	out.Source = "scan"
	return out, counts, true
}

// A chat message's fate when offered the scan readings.
const (
	msgNotScan = iota
	msgPosted
	msgDuplicate
	msgFailed // a scan, but the store failed: handled, never prose
)

// postScanMessage offers one chat message the scan readings
// before it is read as chat. A message carrying tabs gets the
// scan tool's plain reading of it; a scan the client fused into
// the message gets the fused re-reading; either is stored as
// the scan report it is, not as prose. A message that is
// neither falls through as msgNotScan, to be read as chat
// exactly as before -- without tabs a single message cannot be
// a tabbed paste, and the plain reading's one-line kinds (a
// local list of one name, above all) must not swallow ordinary
// chat. The speaker and moment are the message's own, as for
// any chat report.
func (app *Application) postScanMessage(ctx context.Context, base db.InsertIntelReportParams, raw string, systemID int64, speaker string, at time.Time) int {
	b := base
	if speaker != "" {
		b.Reporter = clipRunes(speaker, 40)
	}
	if !at.IsZero() && at.Before(base.CreatedAt.Add(time.Minute)) && at.After(base.CreatedAt.Add(-intelRetention)) {
		b.SeenAt = at
	}
	try := func(p db.InsertIntelReportParams, counts *intelanalyze.StandingCounts, ok bool) int {
		if !ok {
			return msgNotScan
		}
		if _, err := app.insertIntelReport(ctx, p, false, counts); err != nil {
			if errors.Is(err, errIntelDuplicate) {
				return msgDuplicate
			}
			logging.Errorf("channels: insert scan report from a chat message: %v", err)
			return msgFailed
		}
		return msgPosted
	}
	if strings.Contains(raw, "\t") {
		if fate := try(app.scanReportOf(ctx, b, raw, systemID, speaker)); fate != msgNotScan {
			return fate
		}
	}
	return try(app.fusedScanReportOf(ctx, b, raw, systemID, speaker))
}

// postChatLines reads intel chat lines into reports on base's channel,
// as base's author. A line's speaker and timestamp, when it has them,
// are the report's; a report already in the channel (the same moment,
// system, speaker and words) is not stored twice, so lines sent again
// -- a pasted log overlapping the last, an Axon restarting, two
// characters logging the same channel -- land once. batchSpeaker is
// the speaker the batch as a whole claims (the overlay posting as the
// character at the keyboard, whose lines carry no speaker of their
// own): it counts only as the posting account's own pilot
// (accountSpeaker), and everywhere else a line's own speaker stands.
func (app *Application) postChatLines(ctx context.Context, base db.InsertIntelReportParams, lines, source, batchSpeaker string) chatPostResult {
	return app.postChatLinesWith(ctx, base, lines, source, batchSpeaker, submitOverride{})
}

// postChatLinesWith is postChatLines under a submission's overrides
// (submitOverride). A scan kind reads the batch as that scan or not
// at all; "chat" reads it as chat lines only, never as a scan; a
// system files every report the batch makes there, the lines that
// named none included.
func (app *Application) postChatLinesWith(ctx context.Context, base db.InsertIntelReportParams, lines, source, batchSpeaker string, ov submitOverride) chatPostResult {
	var res chatPostResult
	now := base.CreatedAt
	batchSpeaker = app.accountSpeaker(ctx, base.AuthorUserID, batchSpeaker)
	asChat := ov.Kind == submitKindChat
	// A scan pasted as chat lines is a scan, not chat: the same
	// reading the scan tool makes of it, reported as one report.
	if !asChat {
		if p, counts, ok := app.scanReportAs(ctx, base, lines, ov, 0, batchSpeaker); ok {
			if _, err := app.insertIntelReport(ctx, p, false, counts); err != nil {
				if errors.Is(err, errIntelDuplicate) {
					res.Duplicates = 1
					return res
				}
				logging.Errorf("channels: insert scan report: %v", err)
				return res
			}
			res.Posted = 1
			return res
		}
	}
	if ov.scanKind() != "" {
		// The player said which scan this is, and it does not
		// read as one: it is not read as chat behind their back.
		for _, l := range strings.Split(lines, "\n") {
			if strings.TrimSpace(l) != "" {
				res.Skipped++
			}
		}
		return res
	}
	o := intelchat.Parse(lines, app.systemLookup(ctx), now)
	res.Skipped = o.Questions
	if ov.SystemID > 0 {
		// The player named the system: every line is about it,
		// the ones that named none as much as the rest.
		o.Reports = append(o.Reports, o.NoSystemRead...)
		o.NoSystemRead, o.NoSystemLines = nil, nil
		for i := range o.Reports {
			o.Reports[i].SystemID = ov.SystemID
		}
	}
	// A line that named no system is offered the readings the
	// chat parser cannot make before it counts as skipped: a
	// scan it fused, read as a scan (postScanMessage), and a
	// local roster fused the same way, stored by the roster
	// rescue -- the analysis pipeline only ever re-reads
	// reports that were stored, so a line dropped here is
	// never read at all. A scan that failed to store counts
	// neither way: it was a scan, never prose.
	for i, nr := range o.NoSystemRead {
		fate := msgNotScan
		if !asChat {
			fate = app.postScanMessage(ctx, base, nr.Raw, 0, nr.Speaker, nr.At)
		}
		switch fate {
		case msgPosted:
			res.Posted++
			continue
		case msgDuplicate:
			res.Duplicates++
			continue
		case msgFailed:
			continue
		}
		// A line naming no speaker of its own is the batch
		// speaker's line: the rescue reads it with that
		// speaker's location before the poster's, as it does
		// a spoken line.
		rnr := nr
		if rnr.Speaker == "" {
			rnr.Speaker = batchSpeaker
		}
		switch app.rescueRosterLine(ctx, base, rnr, source) {
		case rosterStored:
			res.Posted++
		case rosterDuplicate:
			res.Duplicates++
		default:
			res.Skipped++
			res.SkippedLines = append(res.SkippedLines, o.NoSystemLines[i])
		}
	}
	for _, rep := range o.Reports {
		// A message that is a scan is stored as one, not as the
		// prose the chat parser made of its remains.
		fate := msgNotScan
		if !asChat {
			fate = app.postScanMessage(ctx, base, rep.Raw, rep.SystemID, rep.Speaker, rep.At)
		}
		switch fate {
		case msgPosted:
			res.Posted++
			continue
		case msgDuplicate:
			res.Duplicates++
			continue
		case msgFailed:
			continue
		}
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
		if _, err := app.insertIntelReport(ctx, p, rep.NoVisual, nil); err != nil {
			if errors.Is(err, errIntelDuplicate) {
				res.Duplicates++
				continue
			}
			logging.Errorf("channels: insert chat report: %v", err)
			continue
		}
		res.Posted++
	}
	return res
}

// A roster line's fate in rescueRosterLine.
const (
	rosterSkipped = iota
	rosterStored
	rosterDuplicate
)

// rescueRosterLine is the chat-lines path's roster rescue: a
// line the chat parser could not place, for naming no system,
// is read as the local roster its text holds -- the same
// analysis pipeline reading (analyzeIntel and its roster
// re-read) that re-reads a stored report, so the segmentation,
// the bar and the reading are the one implementation. The
// system is scanReportOf's precedence as far as chat lines
// reach: the line names none the chat parser could find (that
// is why it is here) and a chat line names none in a form, so
// what remains is the speaker's own current system, and
// failing that the poster's, and the sentence says so, as it
// does for a scan paste. A rescued report is stored as the re-read is stored
// (insertIntelReport): the reading as its scan with the line
// kept as the paste, the sentence for a headline, recognised
// again by its pilot set. A roster the re-read cannot make --
// its names are pilots the site has not verified yet, so the
// segmentation cannot clear the bar however real they are --
// is stored anyway by storeVerifiedRosterLine: storage does
// not stand on the reading's bar. No known location is the
// one refusal left, and every skip here logs its reason and
// the line's first fragment.
func (app *Application) rescueRosterLine(ctx context.Context, base db.InsertIntelReportParams, nr intelchat.Report, source string) int {
	frag := clipRunes(nr.Text, 60)
	sysID := app.speakerSystemID(ctx, nr.Speaker)
	if sysID == 0 {
		sysID = app.posterSystemID(ctx, base.AuthorCharacterID)
	}
	if sysID == 0 {
		logging.Warnf("channels: roster line skipped, no location on file for its speaker or poster: %q", frag)
		return rosterSkipped
	}
	plain, roster, _ := app.analyzeIntel(ctx, base.ChannelID, intelanalyze.Input{
		System: sysID, Text: nr.Text, Hostiles: nr.Hostiles, Clear: nr.Clear, NoVisual: nr.NoVisual,
	}, base.AuthorCharacterID, false)
	if roster == nil {
		return app.storeVerifiedRosterLine(ctx, base, nr, source, sysID, plain, frag)
	}
	scanID, err := app.storeRosterScan(ctx, base.AuthorUserID, roster, nr.Text)
	if err != nil {
		logging.Errorf("channels: store rescued roster reading: %v: %q", err, frag)
		return rosterSkipped
	}
	p := base
	p.Source = source
	p.SystemID = sysID
	p.ScanID = scanID
	p.Text = strings.TrimSuffix(roster.sentence, ".") + " — system from the reporter's current location."
	p.Pilots = p.Text
	if p.Hostiles == 0 {
		p.Hostiles = scanHostiles(roster.result)
	}
	p.Clear = false
	if nr.Speaker != "" {
		p.Reporter = clipRunes(nr.Speaker, 40)
	}
	if !nr.At.IsZero() && nr.At.Before(base.CreatedAt.Add(time.Minute)) && nr.At.After(base.CreatedAt.Add(-intelRetention)) {
		p.SeenAt = nr.At
	}
	a := roster.analysis
	p.Kind = a.Kind
	if raw, err := json.Marshal(a); err == nil {
		p.Analysis = string(raw)
	}
	p.AnalysisPending = false
	if set := app.rosterSetOf(ctx, p.Text, p.Analysis, p.ScanID); set != nil {
		if app.rosterDuplicate(ctx, p.ChannelID, p.SystemID, 0, p.SeenAt, set) > 0 {
			return rosterDuplicate
		}
	}
	if _, err := app.storeIntelReport(ctx, p); err != nil {
		logging.Errorf("channels: insert rescued roster report: %v: %q", err, frag)
		return rosterSkipped
	}
	return rosterStored
}

// storeVerifiedRosterLine stores a no-system roster line the
// roster re-read could not make: the names in its text are
// verified with EVE first (verifyRosterNames), and the report
// stands on the ones EVE backs, however few of them the site
// knew before. The line itself is the report's text, kept as
// written, with the location note the fallback carries; the
// system is the speaker's or poster's the rescue settled. A
// line no roster can be verified in is skipped, with the
// reason logged like every skip on this path.
func (app *Application) storeVerifiedRosterLine(ctx context.Context, base db.InsertIntelReportParams, nr intelchat.Report, source string, sysID int64, plain intelanalyze.Analysis, frag string) int {
	pilots, counts, ok := app.verifyRosterNames(ctx, base.ChannelID, nr.Text, plain.Unknown)
	if !ok {
		logging.Warnf("channels: roster line skipped, no roster EVE verifies in it: %q", frag)
		return rosterSkipped
	}
	p := base
	p.Source = source
	p.SystemID = sysID
	p.Text = nr.Text + " — system from the reporter's current location."
	p.Pilots = p.Text
	p.Clear = false
	if nr.Speaker != "" {
		p.Reporter = clipRunes(nr.Speaker, 40)
	}
	if !nr.At.IsZero() && nr.At.Before(base.CreatedAt.Add(time.Minute)) && nr.At.After(base.CreatedAt.Add(-intelRetention)) {
		p.SeenAt = nr.At
	}
	a := intelanalyze.Analysis{Kind: intelanalyze.KindLocal, Count: nr.Hostiles, Pilots: pilots, Counts: counts}
	p.Kind = a.Kind
	if raw, err := json.Marshal(a); err == nil {
		p.Analysis = string(raw)
	}
	p.AnalysisPending = false
	set := make(map[int64]bool, len(pilots))
	for _, pl := range pilots {
		set[pl.ID] = true
	}
	if app.rosterDuplicate(ctx, p.ChannelID, p.SystemID, 0, p.SeenAt, set) > 0 {
		return rosterDuplicate
	}
	if _, err := app.storeIntelReport(ctx, p); err != nil {
		logging.Errorf("channels: insert verified roster report: %v: %q", err, frag)
		return rosterSkipped
	}
	return rosterStored
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
	// The paste the reading was made from, for the sentence's
	// attribution; a reading whose paste no longer parses is
	// reported from its tallies alone.
	var parsed *scan.Result
	if pp, err := scan.Parse(row.Raw); err == nil {
		parsed = &pp
	}
	text, split := app.scanReportText(ctx, c.ID, res, parsed, id.posterID)
	counts := &intelanalyze.StandingCounts{Hostile: split[bucketHostile], Friendly: split[bucketFriendly], Neutral: split[bucketNeutral], Unclassified: split[bucketUnknown]}
	p := db.InsertIntelReportParams{ChannelID: c.ID, SystemID: row.SystemID, AuthorUserID: id.userID, AuthorCharacterID: id.posterID,
		Reporter: id.posterName, Text: text, ScanID: scanID, Source: "scan", SeenAt: row.CreatedAt, CreatedAt: now}
	p.Hostiles = scanHostiles(res)
	if _, err := app.insertIntelReport(ctx, p, false, counts); err != nil && !errors.Is(err, errIntelDuplicate) {
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
	rep, err := app.queries.GetIntelReportFull(ctx, rid)
	if !ok || err != nil || rep.ChannelID != c.ID || (rep.AuthorUserID != id.userID && !c.Manage) {
		back("You cannot remove that report.")
		return
	}
	if err := app.queries.DeleteIntelReport(ctx, rid); err != nil {
		back("That did not go through — please try again.")
		return
	}
	// The group the report belonged to recomputes without it.
	if rep.GroupID > 0 {
		app.refreshGroupComposite(ctx, rep.GroupID)
	}
	back("Report removed.")
}

// --- Managing (the owner only) ---

// ownedChannel finds a channel the account manages: its own, or one
// of an organisation it manages channels for.
func (app *Application) ownedChannel(ctx context.Context, r *http.Request) (db.IntelChannel, intelIdentity, bool) {
	id := app.intelIdentity(ctx)
	c, err := app.queries.GetIntelChannel(ctx, channelIDParam(r))
	if err != nil || !id.managesChannel(c.OwnerKind, c.OwnerID, c.OwnerUserID) {
		return db.IntelChannel{}, id, false
	}
	return c, id, true
}

func (app *Application) handleChannelEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, _, ok := app.ownedChannel(ctx, r)
	back := app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", channelIDParam(r)))
	if !ok {
		back("Only whoever runs the channel can change it.")
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
		app.flashBack(w, r, fmt.Sprintf("/intel/channels/%d/", channelIDParam(r)))("Only whoever runs the channel can delete it.")
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
		back("Only whoever runs the channel can grant access.")
		return
	}
	kind := r.PostFormValue("kind")
	canPost := r.PostFormValue("post") != ""
	nGrants, _ := app.queries.CountIntelChannelGrants(ctx, c.ID)
	nRules, _ := app.queries.CountIntelChannelRules(ctx, c.ID)
	if nGrants+nRules >= intelMaxGrants {
		back(fmt.Sprintf("A channel holds up to %d grants.", intelMaxGrants))
		return
	}
	// An organisation's channel shared by rule (channels_orgs.go).
	if kind == "rule" {
		if c.OwnerKind == "" {
			back("Only a corporation's or alliance's channel can be shared by role or rank.")
			return
		}
		label, ok := app.addChannelRule(ctx, orgOwner{Kind: c.OwnerKind, ID: c.OwnerID}, c.ID, r.PostFormValue("who"), canPost)
		if !ok {
			back("Choose who in the corporation or alliance to share with.")
			return
		}
		how := "can read"
		if canPost {
			how = "can read and post"
		}
		back(label + ": " + how + ".")
		return
	}
	if !grantKinds[kind] {
		back("Choose who to grant: a pilot, a corporation, an alliance or a group.")
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
		back("Only whoever runs the channel can change access.")
		return
	}
	if r.PostFormValue("kind") == "rule" {
		kind, ref, _ := strings.Cut(r.PostFormValue("who"), ":")
		if err := app.queries.DeleteIntelChannelRule(ctx, db.DeleteIntelChannelRuleParams{ChannelID: c.ID, RuleKind: kind, RuleRef: ref}); err != nil {
			back("That did not go through — please try again.")
			return
		}
		back("Access removed.")
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
		// Gate is the system the report's gate leads to, when
		// its reading names one: a gate reading is intel about
		// both systems, and the map pins it under each.
		Gate int64 `json:"gate,omitempty"`
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
		names := app.newKillNames(ctx)
		for _, r := range rows {
			rep := report{ID: r.ID, Ch: r.ChannelID, Sys: r.SystemID, By: r.Reporter, Text: r.Text, N: r.Hostiles, Clear: r.Clear, Scan: r.ScanID, At: r.SeenAt.Unix(), Kind: r.Kind}
			// The reading travels with the row already; the
			// gate is read off it, never looked up per report.
			var a intelanalyze.Analysis
			if r.Analysis != "" && json.Unmarshal([]byte(r.Analysis), &a) == nil && a.Gate > 0 {
				rep.Gate = a.Gate
				if r.ScanID == "" {
					if s, ok := gateReadingSentence(r.Kind, a, names.system(r.SystemID).Name, names.system(a.Gate).Name); ok {
						rep.Text = s
					}
				}
			}
			out.Reports = append(out.Reports, rep)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// pruneIntelReports drops reports past their 7 days.
func (app *Application) pruneIntelReports(ctx context.Context) {
	// Seven-day retention changes its answer at most once a
	// day; the kill prune's daily stamp is the shape to copy,
	// not four DELETEs a minute.
	if time.Since(app.intelLastPruned) < 24*time.Hour {
		return
	}
	app.intelLastPruned = time.Now()
	n, err := app.queries.PruneIntelReports(ctx, time.Now().Add(-intelRetention))
	if err != nil {
		logging.Warnf("channels: prune: %v", err)
		return
	}
	if n > 0 {
		logging.Infof("channels: pruned %d reports past 7 days", n)
	}
	if _, err := app.queries.PruneIntelGroupComposites(ctx); err != nil {
		logging.Warnf("channels: prune composites: %v", err)
	}
	if _, err := app.queries.PruneIntelGangGroups(ctx); err != nil {
		logging.Warnf("channels: prune gang groups: %v", err)
	}
	if _, err := app.queries.PruneIntelGangs(ctx); err != nil {
		logging.Warnf("channels: prune gangs: %v", err)
	}
}
