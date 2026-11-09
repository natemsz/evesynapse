package app

import (
	"context"
	json "github.com/goccy/go-json"
	"errors"
	"evesynapse/internal/app/module"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Notifications: telling the user what the worker already sees.
//
// After each refresh cycle the worker looks at every account's stored
// data (never ESI) and lists the events that are true right now: this
// skill has finished, that mail is unread, this extractor has stopped.
// Each event has a key. A key the worker has not seen before becomes a
// notification; one it has seen is left alone. That is the whole
// mechanism, and it is why nothing here compares one cycle's data with
// the last.
//
// Two things keep it quiet:
//
//   - The first time a source is readable (a character's mail, one
//     planet's layout) everything already in it is recorded as seen
//     and nothing is announced. Linking a character does not replay
//     its history.
//   - A kind the user has switched off is still recorded as seen, so
//     switching it on later announces only what happens from then on.
//
// A notification is a module's news on a delivery channel: each kind
// names the module (module_manifest.go) whose data it reads, and a character
// that has not granted that module's scopes simply has no data to
// produce events from.
// ---------------------------------------------------------------------------

// Notification kinds. These strings are stored (notifications.kind,
// the keys in notification_seen, the user's settings); never rename
// one.
const (
	notifyWatch      = "watch"
	notifyMarket     = "market_change"
	notifySkill      = "skill"
	notifyMail       = "mail"
	notifyCalendar   = "calendar"
	notifyKillmail   = "killmail"
	notifyPI         = "pi"
	notifyIndustry   = "industry"
	notifyOp         = "op"
	notifyOpReminder = "op_reminder"
	notifySRPNew     = "srp_new"
	notifySRPDone    = "srp_done"
	// notifyCamp fires while a chosen system shows a gate camp and
	// one of the account's characters is online.
	notifyCamp = "camp"
	// notifyContract fires when a contract the character is party
	// to is accepted, finished or expired.
	notifyContract = "contract"
	// notifyRelink fires when a character's saved sign-in stops
	// working, once per transition to token-dead.
	notifyRelink = "relogin"
	// notifyUndercut is the undercut digest: a briefing card only,
	// never one notification per beaten order.
	notifyUndercut = "undercut"
	// notifyNotice is an administrator's urgent global notice
	// (notices.go); it is never produced by the worker pass.
	notifyNotice = "notice"
)

// notifyKind describes one kind of notification.
type notifyKind struct {
	ID     string
	Title  string // as the settings page names it
	Module string // the feature (manifest entry or module declaration) whose data it reads
	// Standing is a state rather than an event: it is announced when
	// it starts to hold and again if it clears and comes back.
	Standing bool
	// Account: the kind belongs to the account, not to one of its
	// characters, so it cannot be switched per character.
	Account bool
	// DigestOnly: the kind never notifies; its row carries a
	// briefing card switch (and its own settings) but no
	// notification switch.
	DigestOnly bool
}

// notifyKinds is every kind, in the order the settings page lists them.
var coreNotifyKinds = []notifyKind{
	{ID: notifyWatch, Title: "Watch list alerts", Module: "market_public", Standing: true, Account: true},
	{ID: notifyMarket, Title: "Market changes on watched items and baskets", Module: "market_public", Account: true},
	{ID: notifySkill, Title: "Skill complete", Module: "skills"},
	// Ops are EveSynapse's own (ops.go): any character sees its
	// corporation's, with no access to grant.
	{ID: notifyOp, Title: "New op on the calendar"},
	{ID: notifyOpReminder, Title: "Op starting soon"},
	// Ship replacement (corp_srp.go) is EveSynapse's own too.
	{ID: notifyKillmail, Title: "New killmail", Module: "killmails"},
	{ID: notifyPI, Title: "Stopped planetary extractors", Module: "planets"},
	{ID: notifyIndustry, Title: "Industry job complete", Module: "industry"},
	{ID: notifyCamp, Title: "Gate camp alerts", Account: true},
	{ID: notifyContract, Title: "Contract accepted, finished or expired", Module: "contracts"},
	{ID: notifyRelink, Title: "Character needs re-login"},
	{ID: notifyUndercut, Title: "Undercut digest", Account: true, DigestOnly: true},
	// Urgent service notices from the EveSynapse team (notices.go).
	{ID: notifyNotice, Title: "Urgent service notices", Account: true},
}

// notifyKindList is every kind this build has, in the order the settings
// page lists them: core's own, then the ones the built-in modules declare,
// each after the kind it names.
func notifyKindList() []notifyKind {
	out := append([]notifyKind(nil), coreNotifyKinds...)
	for _, m := range registeredModules {
		for _, k := range m.NotifyKinds {
			kind := notifyKind{ID: k.ID, Title: k.Title, Module: k.Feature, Standing: k.Standing, Account: k.Account}
			if kind.Module == "" {
				kind.Module = m.ID
			}
			at := len(out)
			for i, existing := range out {
				if k.After != "" && existing.ID == k.After {
					at = i + 1
					break
				}
			}
			out = append(out, notifyKind{})
			copy(out[at+1:], out[at:])
			out[at] = kind
		}
	}
	return out
}

// notifyKindByID returns the kind for id.
func notifyKindByID(id string) (notifyKind, bool) {
	for _, k := range notifyKindList() {
		if k.ID == id {
			return k, true
		}
	}
	return notifyKind{}, false
}

// notifyEvent is one thing that is true right now and worth telling
// the user once.
type notifyEvent struct {
	Kind        string
	CharacterID int64 // 0 for an account-level event
	Source      string
	Key         string
	Title       string
	URL         string
}

// notifyTitleMax bounds a stored title; a mail subject is the one
// part of a title the user does not control.
const notifyTitleMax = 200

// notifyCollector gathers one account's events, and the sources they
// were read from. A source that was readable but held no events is
// still recorded, so that its baseline is taken now and not when its
// first event arrives.
type notifyCollector struct {
	events  []notifyEvent
	sources map[string]bool
	keys    map[string]bool
}

func newNotifyCollector() *notifyCollector {
	return &notifyCollector{sources: map[string]bool{}, keys: map[string]bool{}}
}

// source names one readable source and returns its baseline key.
func (c *notifyCollector) source(kind string, parts ...any) string {
	key := "base|" + kind
	for _, p := range parts {
		key += fmt.Sprintf("|%v", p)
	}
	c.sources[key] = true
	return key
}

func (c *notifyCollector) add(ev notifyEvent) {
	if c.keys[ev.Key] {
		return
	}
	c.keys[ev.Key] = true
	if len(ev.Title) > notifyTitleMax {
		ev.Title = clip(ev.Title, notifyTitleMax) + "…"
	}
	c.events = append(c.events, ev)
}

// collectNotifyEvents lists what is true right now across one
// account's characters, from stored snapshots only.
func (app *Application) collectNotifyEvents(ctx context.Context, userID int64, bundles []*charSnaps, now time.Time) *notifyCollector {
	c := newNotifyCollector()
	for _, b := range bundles {
		app.notifySkillEvents(ctx, c, b, now)
		if _, ok := notifyKindByID(notifyMail); ok {
			app.notifyMailEvents(c, b)
		}
		if _, ok := notifyKindByID(notifyCalendar); ok {
			app.notifyCalendarEvents(c, b, now)
		}
		app.notifyKillmailEvents(ctx, c, b)
		app.notifyPIEvents(ctx, c, b, now)
		app.notifyIndustryEvents(ctx, c, b, now)
		if _, ok := notifyKindByID(notifyContract); ok {
			app.notifyContractEvents(c, b, now)
		}
		if _, ok := notifyKindByID(notifyRelink); ok {
			app.notifyRelinkEvents(c, b)
		}
	}
	app.notifyWatchEvents(ctx, c, userID)
	app.notifyMarketEvents(ctx, c, userID)
	if _, ok := notifyKindByID(notifyCamp); ok {
		app.notifyCampEvents(ctx, c, userID, bundles, now)
	}
	return c
}

// notifySkillEvents: queue entries whose finish time has passed. The
// queue keeps a finished entry until the character next logs in, so
// time passing is enough; no refetch is needed to notice.
//
// It reads the raw queue on purpose: its events are the finished
// entries views leave out (skillqueue_live.go).
func (app *Application) notifySkillEvents(ctx context.Context, c *notifyCollector, b *charSnaps, now time.Time) {
	if !b.queueKnown {
		return
	}
	id := b.ch.CharacterID
	src := c.source(notifySkill, id)
	for _, e := range b.queue {
		finish, ok := parseRFC3339(e.FinishDate)
		if !ok || finish.After(now) {
			continue
		}
		c.add(notifyEvent{
			Kind: notifySkill, CharacterID: id, Source: src,
			Key:   fmt.Sprintf("skill|%d|%d|%d", id, e.SkillID, e.FinishedLevel),
			Title: fmt.Sprintf("%s finished training %s %s", b.ch.Name, app.typeNameOrID(ctx, e.SkillID), esi.RomanLevel(e.FinishedLevel)),
			URL:   fmt.Sprintf("/character/?character=%d&view=1", id),
		})
	}
}

// notifyMailEvents: unread mail from someone else.
func (app *Application) notifyMailEvents(c *notifyCollector, b *charSnaps) {
	if !b.mailKnown {
		return
	}
	id := b.ch.CharacterID
	src := c.source(notifyMail, id)
	for _, h := range b.mail {
		if h.IsRead || h.From == id {
			continue
		}
		title := b.ch.Name + " has new mail"
		if sender, found := app.esi.CachedCharacterName(h.From); found && sender != "" {
			title += " from " + sender
		}
		if subject := strings.TrimSpace(h.Subject); subject != "" {
			title += ": " + subject
		}
		c.add(notifyEvent{
			Kind: notifyMail, CharacterID: id, Source: src,
			Key:   fmt.Sprintf("mail|%d|%d", id, h.MailID),
			Title: title,
			URL:   fmt.Sprintf("/mail/?character=%d&view=1", id),
		})
	}
}

// notifyCalendarEvents: events still ahead. One already on the
// calendar when it first becomes readable is part of the baseline.
func (app *Application) notifyCalendarEvents(c *notifyCollector, b *charSnaps, now time.Time) {
	if !b.calendarKnown {
		return
	}
	id := b.ch.CharacterID
	src := c.source(notifyCalendar, id)
	for _, ev := range b.calendar {
		at, ok := parseRFC3339(ev.EventDate)
		if !ok || at.Before(now) {
			continue
		}
		name := strings.TrimSpace(ev.Title)
		if name == "" {
			name = "Calendar event"
		}
		c.add(notifyEvent{
			Kind: notifyCalendar, CharacterID: id, Source: src,
			Key:   fmt.Sprintf("calendar|%d|%d", id, ev.EventID),
			Title: fmt.Sprintf("%s has a new calendar event: %s, %s", b.ch.Name, name, at.UTC().Format("Jan 2 15:04")),
			URL:   fmt.Sprintf("/calendar/?character=%d&view=1", id),
		})
	}
}

// notifyKillmailEvents: kills and losses on the character's recent
// list. The list is read straight from its stored snapshot; it is not
// part of the bundle the home page loads.
func (app *Application) notifyKillmailEvents(ctx context.Context, c *notifyCollector, b *charSnaps) {
	id := b.ch.CharacterID
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: id, Kind: esi.SnapKillmails})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("notify: killmails for character %d: %v", id, err)
		}
		return
	}
	var refs []esi.KillmailRef
	if json.Unmarshal([]byte(snap.Payload), &refs) != nil {
		return
	}
	src := c.source(notifyKillmail, id)
	for _, ref := range refs {
		c.add(notifyEvent{
			Kind: notifyKillmail, CharacterID: id, Source: src,
			Key:   fmt.Sprintf("killmail|%d|%d", id, ref.KillmailID),
			Title: b.ch.Name + " has a new killmail",
			URL:   fmt.Sprintf("/killmails/?character=%d&view=1", id),
		})
	}
}

// notifyPIEvents: extractors past their expiry, one event per planet
// and stop (extractors set up together stop together). Each planet's
// layout arrives on its own, so each is its own source.
func (app *Application) notifyPIEvents(ctx context.Context, c *notifyCollector, b *charSnaps, now time.Time) {
	if !b.planetsKnown {
		return
	}
	id := b.ch.CharacterID
	for _, colony := range b.colonies {
		layout := b.layouts[colony.PlanetID]
		if layout == nil {
			continue
		}
		src := c.source(notifyPI, id, colony.PlanetID)
		for _, pin := range layout.Pins {
			if pin.ExtractorDetails == nil {
				continue
			}
			expiry, ok := parseRFC3339(pin.ExpiryTime)
			if !ok || expiry.After(now) {
				continue
			}
			c.add(notifyEvent{
				Kind: notifyPI, CharacterID: id, Source: src,
				Key:   fmt.Sprintf("pi|%d|%d|%d", id, colony.PlanetID, expiry.Unix()/3600),
				Title: fmt.Sprintf("%s: extractors have stopped on %s", b.ch.Name, app.planetDisplayName(ctx, colony.PlanetID)),
				URL:   fmt.Sprintf("/planets/?character=%d&view=1", id),
			})
		}
	}
}

// notifyIndustryEvents: jobs that are ready, or still marked active
// past their end (ESI only says "ready" once it is asked again).
func (app *Application) notifyIndustryEvents(ctx context.Context, c *notifyCollector, b *charSnaps, now time.Time) {
	if !b.jobsKnown {
		return
	}
	id := b.ch.CharacterID
	src := c.source(notifyIndustry, id)
	for _, j := range b.jobs {
		done := j.Status == "ready"
		if j.Status == "active" {
			if end, ok := parseRFC3339(j.EndDate); ok && !end.After(now) {
				done = true
			}
		}
		if !done {
			continue
		}
		nameID := j.ProductTypeID
		if nameID == 0 {
			nameID = j.BlueprintTypeID
		}
		c.add(notifyEvent{
			Kind: notifyIndustry, CharacterID: id, Source: src,
			Key:   fmt.Sprintf("industry|%d|%d", id, j.JobID),
			Title: fmt.Sprintf("%s: the %s job has finished", b.ch.Name, app.typeNameOrID(ctx, nameID)),
			URL:   fmt.Sprintf("/industry/jobs/?character=%d&view=1", id),
		})
	}
}

// contractFinished reports whether a contract status means done
// from either side's point of view.
func contractFinished(status string) bool {
	switch status {
	case "finished", "finished_issuer", "finished_contractor":
		return true
	}
	return false
}

// contractName names a contract for a notification or digest line:
// its title when it has one, "courier run" for an untitled
// courier, otherwise its number.
func contractName(c esi.Contract) string {
	if title := strings.TrimSpace(c.Title); title != "" {
		return title
	}
	if c.Type == "courier" {
		return "courier run"
	}
	return fmt.Sprintf("contract %d", c.ContractID)
}

// contractEventWhat phrases one contract transition the same way
// in a notification and on the digest card: the item's name and
// what happened to it.
func contractEventWhat(name, event string) string {
	return name + " " + event
}

// notifyContractEvents: contracts the character is party to that
// were accepted, finished or expired. A contract new to the pass is
// its own baseline (its source unseen), so only transitions after
// that announce once each: the key carries the contract and the
// event, and repeat polls find the key already seen.
func (app *Application) notifyContractEvents(c *notifyCollector, b *charSnaps, now time.Time) {
	if !b.contractsKnown {
		return
	}
	id := b.ch.CharacterID
	for _, contract := range b.contracts {
		if contract.IssuerID != id && contract.AcceptorID != id {
			continue
		}
		src := c.source(notifyContract, id, contract.ContractID)
		url := fmt.Sprintf("/contracts/?character=%d&view=1", id)
		name := contractName(contract)
		switch {
		case contract.Status == "in_progress":
			c.add(notifyEvent{
				Kind: notifyContract, CharacterID: id, Source: src,
				Key:   fmt.Sprintf("contract|%d|%d|accepted", id, contract.ContractID),
				Title: fmt.Sprintf("%s: %s", b.ch.Name, contractEventWhat(name, "was accepted")),
				URL:   url,
			})
		case contractFinished(contract.Status):
			c.add(notifyEvent{
				Kind: notifyContract, CharacterID: id, Source: src,
				Key:   fmt.Sprintf("contract|%d|%d|finished", id, contract.ContractID),
				Title: fmt.Sprintf("%s: %s", b.ch.Name, contractEventWhat(name, "finished")),
				URL:   url,
			})
		case contract.Status == "failed" || contractLapsed(contract, now):
			c.add(notifyEvent{
				Kind: notifyContract, CharacterID: id, Source: src,
				Key:   fmt.Sprintf("contract|%d|%d|expired", id, contract.ContractID),
				Title: fmt.Sprintf("%s: %s", b.ch.Name, contractEventWhat(name, "expired")),
				URL:   url,
			})
		}
	}
}

// contractLapsed reports whether an outstanding contract with nobody
// holding it has run past its expiry: the pass's "expired" for
// contracts ESI leaves outstanding instead of closing.
func contractLapsed(c esi.Contract, now time.Time) bool {
	if c.Status != "outstanding" || c.AcceptorID != 0 {
		return false
	}
	expired, ok := parseRFC3339(c.DateExpired)
	return ok && !expired.After(now)
}

// notifyRelinkEvents: a character whose saved sign-in stopped
// working. The key carries the moment it was parked, so the event
// fires on the transition, not on every worker cycle.
func (app *Application) notifyRelinkEvents(c *notifyCollector, b *charSnaps) {
	id := b.ch.CharacterID
	src := c.source(notifyRelink, id)
	if b.ch.LinkState != linkStateTokenDead {
		return
	}
	var parked int64
	if b.ch.LinkStateAt.Valid {
		parked = b.ch.LinkStateAt.Time.Unix()
	}
	c.add(notifyEvent{
		Kind: notifyRelink, CharacterID: id, Source: src,
		Key:   fmt.Sprintf("relogin|%d|%d", id, parked),
		Title: fmt.Sprintf("%s needs to sign in again. The saved EVE sign-in stopped working.", b.ch.Name),
		URL:   "/characters/",
	})
}

// campOnlineFresh is how recently an online snapshot must have been
// fetched to count the character as online for a camp alert: a stale
// "online" from yesterday must not fire it.
const campOnlineFresh = 30 * time.Minute

// campSystemName names a watched system from the static data.
func (app *Application) campSystemName(ctx context.Context, systemID int64) string {
	if sys, err := app.queries.GetSDESystem(ctx, systemID); err == nil && sys.Name != "" {
		return sys.Name
	}
	return fmt.Sprintf("system %d", systemID)
}

// notifyCampEvents: gate camps in the account's watched systems,
// but only while one of its characters is online. A camp new to the
// pass is baselined like any other source; a continuing camp keeps
// its key, so it announces once, and a fresh camp after it announces
// again.
func (app *Application) notifyCampEvents(ctx context.Context, c *notifyCollector, userID int64, bundles []*charSnaps, now time.Time) {
	prefs := app.notifyPrefsFor(ctx, userID)
	if len(prefs.CampSystems) == 0 {
		return
	}
	// Sources register before the online gate, so a camp that
	// stood while the account was offline reads as known when a
	// character comes online, and only a new camp announces.
	for _, systemID := range prefs.CampSystems {
		c.source(notifyCamp, systemID)
	}
	online := false
	for _, b := range bundles {
		var on esi.Online
		if at, ok := app.snapAt(ctx, b.ch.CharacterID, esi.SnapOnline, &on); ok && on.Online && now.Sub(at) <= campOnlineFresh {
			online = true
			break
		}
	}
	if !online {
		return
	}
	names := app.newKillNames(ctx)
	for _, systemID := range prefs.CampSystems {
		// The source is registered on every pass, camp or not, so
		// a camp that forms reads as a transition from a known
		// past, not as history.
		src := c.source(notifyCamp, systemID)
		stream, err := app.queries.ListKillsBySystem(ctx, db.ListKillsBySystemParams{
			SystemID: systemID, BeforeTime: now.Add(time.Hour), BeforeID: 1 << 62, MaxRows: systemKillsFetch,
		})
		if err != nil {
			logging.Errorf("notify: camp kills for system %d: %v", systemID, err)
			continue
		}
		camp := app.mapSystemCamp(ctx, names, systemID, now, stream)
		if camp == nil {
			continue
		}
		name := app.campSystemName(ctx, systemID)
		c.add(notifyEvent{
			Kind: notifyCamp, Source: src,
			Key:   fmt.Sprintf("camp|%d|%d", systemID, camp.Since),
			Title: fmt.Sprintf("Gate camp in %s: %d kills in the last hour", name, camp.Kills),
			URL:   fmt.Sprintf("/system/?system=%d", systemID),
		})
	}
}

// notifyWatchEvents: watched prices past their threshold, the same
// test the home briefing applies. A standing state: its key carries
// the direction, and it is forgotten when the price comes back inside.
func (app *Application) notifyWatchEvents(ctx context.Context, c *notifyCollector, userID int64) {
	entries, err := app.queries.ListWatchlistByUser(ctx, userID)
	if err != nil {
		logging.Errorf("notify: watch list for user %d: %v", userID, err)
		return
	}
	src := c.source(notifyWatch, userID)
	for _, e := range entries {
		regionID := marketRegionForType(e.TypeID, e.RegionID)
		m := app.watchMove(ctx, e.TypeID, regionID)
		pct, ok := m.Chg7, m.Has7
		if !ok || absFloat(pct) < e.ThresholdPct {
			continue
		}
		// The key carries only which way it moved. The size of the move
		// changes daily and must not make the same alert look new.
		way := "up"
		if pct < 0 {
			way = "down"
		}
		c.add(notifyEvent{
			Kind: notifyWatch, Source: src,
			Key: fmt.Sprintf("watch|%d|%d|%s", e.TypeID, regionID, way),
			Title: fmt.Sprintf("%s moved %s over 7 days in %s, past your %s watch",
				app.typeNameOrID(ctx, e.TypeID), markethistory.ChangeDirection(pct), app.marketRegionLabel(ctx, regionID),
				markethistory.FormatChangePct(e.ThresholdPct)),
			URL: "/market/",
		})
	}
}

// ---------------------------------------------------------------------------
// Settings: which kinds the user wants. Stored with the other per-user
// module settings (widget_configs), under one id.
// ---------------------------------------------------------------------------

// notifyConfigID is the widget_configs id the settings live under.
const notifyConfigID = "notifications"

// Delivery channels, the ids notifyPrefs.OffChannels stores. Discord
// is not one of them: it has its own switch, on the account's
// Discord link (discord_links.dm_notifications), which the settings
// page and the first-run prompt both read and write.
const (
	notifyChannelInApp   = "inapp"
	notifyChannelDesktop = "desktop"
)

// notifyPrefs is an account's notification settings. The zero value,
// and any missing or damaged stored value, means every kind is on.
type notifyPrefs struct {
	Off []string `json:"off,omitempty"` // kinds switched off
	// OffFor: per kind, the characters it is switched off for. A
	// character not listed gets the kind, so one linked later starts
	// with everything on.
	OffFor map[string][]int64 `json:"off_for,omitempty"`
	// OpReminderMinutes is how long before an op starts the account
	// is reminded of it (notifyOpReminder); 0 means the default.
	OpReminderMinutes int `json:"op_reminder_minutes,omitempty"`
	// OffChannels: delivery channels switched off, by id
	// (notifyChannelInApp, notifyChannelDesktop). A channel off
	// stops only that channel: the news is still recorded as seen,
	// and the other channels still carry it.
	OffChannels []string `json:"off_channels,omitempty"`
	// CardsOff: kinds whose briefing card is switched off, for the
	// kinds that have one (briefingCardKindFor). The notification
	// switch and the card switch are independent: a kind can arrive
	// as a notification, a card, both, or neither.
	CardsOff []string `json:"cards_off,omitempty"`
	// CampSystems: the solar systems the gate camp alerts watch,
	// chosen on the settings page, at most campSystemsMax.
	CampSystems []int64 `json:"camp_systems,omitempty"`
	// UndercutDigestHours: how often the undercut digest may appear
	// at most, in hours; 0 means every time. One of
	// undercutDigestHours.
	UndercutDigestHours int `json:"undercut_digest_hours,omitempty"`
	// UndercutDigestShown: when the undercut digest last appeared,
	// unix seconds. The digest builder reads it to enforce the
	// frequency above and writes it when the digest appears, at
	// most once per frequency window.
	UndercutDigestShown int64 `json:"undercut_digest_shown,omitempty"`
	// PromptAnswered: the account has answered the first-run prompt
	// (notify_prompt.go), or waved it away. Either way the prompt is
	// not shown unasked again; it stays one click away in the bell.
	PromptAnswered bool `json:"prompt_answered,omitempty"`
}

// parseNotifyPrefs decodes stored settings, keeping only kinds that
// exist.
func parseNotifyPrefs(blob string) notifyPrefs {
	var raw, prefs notifyPrefs
	if strings.TrimSpace(blob) == "" || json.Unmarshal([]byte(blob), &raw) != nil {
		return prefs
	}
	for _, id := range raw.Off {
		if _, ok := notifyKindByID(id); ok && !prefs.off(id) {
			prefs.Off = append(prefs.Off, id)
		}
	}
	if validReminderMinutes(raw.OpReminderMinutes) {
		prefs.OpReminderMinutes = raw.OpReminderMinutes
	}
	for id, characters := range raw.OffFor {
		kind, ok := notifyKindByID(id)
		if !ok || kind.Account {
			continue
		}
		for _, characterID := range characters {
			if characterID > 0 && !prefs.offFor(id, characterID) {
				if prefs.OffFor == nil {
					prefs.OffFor = map[string][]int64{}
				}
				prefs.OffFor[id] = append(prefs.OffFor[id], characterID)
			}
		}
	}
	for _, id := range raw.OffChannels {
		if (id == notifyChannelInApp || id == notifyChannelDesktop) && !prefs.channelOff(id) {
			prefs.OffChannels = append(prefs.OffChannels, id)
		}
	}
	for _, id := range raw.CardsOff {
		if _, ok := briefingCardKindFor(id); ok && !prefs.cardOff(id) {
			prefs.CardsOff = append(prefs.CardsOff, id)
		}
	}
	for _, id := range raw.CampSystems {
		if id > 0 && len(prefs.CampSystems) < campSystemsMax && !prefs.watchesCampSystem(id) {
			prefs.CampSystems = append(prefs.CampSystems, id)
		}
	}
	if validUndercutDigestHours(raw.UndercutDigestHours) {
		prefs.UndercutDigestHours = raw.UndercutDigestHours
	}
	prefs.UndercutDigestShown = raw.UndercutDigestShown
	prefs.PromptAnswered = raw.PromptAnswered
	return prefs
}

// briefingCardKindFor names the briefing card kind a notification
// kind's news also appears as, for the kinds that have a card: the
// settings page offers those kinds a briefing card switch beside
// the notification one. Kinds not listed have no card today.
func briefingCardKindFor(kind string) (string, bool) {
	switch kind {
	case notifyMail:
		return "mail", true
	case notifySkill:
		return "skills", true
	case notifyPI:
		return "pi", true
	case notifyIndustry:
		return "industry", true
	case notifyCalendar:
		return "calendar", true
	case notifyWatch:
		return "market", true
	case notifyContract:
		return "contract", true
	case notifyRelink:
		return "link", true
	case notifyCamp:
		return "camp", true
	case notifyUndercut:
		return "orders", true
	}
	return "", false
}

// notifyKindForBriefingCard is briefingCardKindFor read the other
// way: the notification kind whose card switch gates a card kind.
func notifyKindForBriefingCard(cardKind string) (string, bool) {
	switch cardKind {
	case "mail":
		return notifyMail, true
	case "skills":
		return notifySkill, true
	case "pi":
		return notifyPI, true
	case "industry":
		return notifyIndustry, true
	case "calendar":
		return notifyCalendar, true
	case "market":
		return notifyWatch, true
	case "contract":
		return notifyContract, true
	case "link":
		return notifyRelink, true
	case "camp":
		return notifyCamp, true
	case "orders":
		return notifyUndercut, true
	}
	return "", false
}

// cardOff reports whether a kind's briefing card is switched off.
func (p notifyPrefs) cardOff(kind string) bool {
	for _, id := range p.CardsOff {
		if id == kind {
			return true
		}
	}
	return false
}

// campSystemsMax bounds how many systems the gate camp alerts watch:
// each watched system costs the pass its own bounded kill reads.
const campSystemsMax = 10

// watchesCampSystem reports whether a system is already watched.
func (p notifyPrefs) watchesCampSystem(id int64) bool {
	for _, watched := range p.CampSystems {
		if watched == id {
			return true
		}
	}
	return false
}

// undercutDigestHours are the frequencies the undercut digest can
// be capped at, in hours; 0 means every time.
var undercutDigestHours = []int{0, 1, 6, 24}

func validUndercutDigestHours(n int) bool {
	for _, allowed := range undercutDigestHours {
		if n == allowed {
			return true
		}
	}
	return false
}

// channelOff reports whether a delivery channel is switched off.
func (p notifyPrefs) channelOff(id string) bool {
	for _, off := range p.OffChannels {
		if off == id {
			return true
		}
	}
	return false
}

// offChannels builds the stored channel list from two switches, in a
// fixed order so the stored value is stable.
func offChannels(inAppOn, desktopOn bool) []string {
	var out []string
	if !inAppOn {
		out = append(out, notifyChannelInApp)
	}
	if !desktopOn {
		out = append(out, notifyChannelDesktop)
	}
	return out
}

func (p notifyPrefs) off(kind string) bool {
	for _, id := range p.Off {
		if id == kind {
			return true
		}
	}
	return false
}

// offFor reports whether kind is switched off for one character.
func (p notifyPrefs) offFor(kind string, characterID int64) bool {
	for _, id := range p.OffFor[kind] {
		if id == characterID {
			return true
		}
	}
	return false
}

// silent reports whether an event is one the user asked not to hear
// about: its kind is off, or off for its character.
func (p notifyPrefs) silent(ev notifyEvent) bool {
	return p.off(ev.Kind) || (ev.CharacterID != 0 && p.offFor(ev.Kind, ev.CharacterID))
}

// notifyPrefsFor reads an account's settings; a read that fails
// leaves everything on.
func (app *Application) notifyPrefsFor(ctx context.Context, userID int64) notifyPrefs {
	blob, err := app.queries.GetWidgetConfig(ctx, db.GetWidgetConfigParams{UserID: userID, WidgetID: notifyConfigID})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("notify: settings for user %d: %v", userID, err)
		}
		return notifyPrefs{}
	}
	return parseNotifyPrefs(blob)
}

// storeNotifyPrefs saves an account's settings.
func (app *Application) storeNotifyPrefs(ctx context.Context, userID int64, prefs notifyPrefs) error {
	blob, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	return app.queries.UpsertWidgetConfig(ctx, db.UpsertWidgetConfigParams{
		UserID: userID, WidgetID: notifyConfigID, Config: string(blob), UpdatedAt: time.Now().UTC(),
	})
}

// ---------------------------------------------------------------------------
// The worker pass.
// ---------------------------------------------------------------------------

const (
	// notifySeenTouch: a key still current has its seen time renewed
	// at most this often, and notifySeenKeep is how long a key that
	// is no longer current is remembered. Renewal is what stops a
	// long-lived event (an unread mail kept for months) from being
	// forgotten and announced a second time.
	notifySeenTouch = 24 * time.Hour
	notifySeenKeep  = 45 * 24 * time.Hour
	// Read notifications are kept briefly, unread ones longer.
	notifyReadKeep   = 14 * 24 * time.Hour
	notifyUnreadKeep = 60 * 24 * time.Hour
	// notifyPruneEvery spaces out the clean-up; the tables are small
	// and nothing depends on it being prompt.
	notifyPruneEvery = time.Hour
)

// notifyPrunedAt is when the clean-up last ran (unix seconds).
var notifyPrunedAt atomic.Int64

// notifyPass runs after a refresh cycle: for every account with
// characters, turn what is newly true into notifications.
func (app *Application) notifyPass(ctx context.Context, characters []db.Character, now time.Time) (created int) {
	return app.notifyAccounts(ctx, characters, now, nil)
}

// notifyAccounts is the pass itself. quiet, when given, says which
// accounts need only their ops looked at this time (notify_quiet.go).
func (app *Application) notifyAccounts(ctx context.Context, characters []db.Character, now time.Time, quiet func(userID int64, chars []db.Character) bool) (created int) {
	byUser := map[int64][]db.Character{}
	var order []int64
	for _, ch := range characters {
		if _, ok := byUser[ch.UserID]; !ok {
			order = append(order, ch.UserID)
		}
		byUser[ch.UserID] = append(byUser[ch.UserID], ch)
	}
	for _, userID := range order {
		if ctx.Err() != nil {
			return created
		}
		opsOnly := quiet != nil && quiet(userID, byUser[userID])
		n, err := app.notifyUser(ctx, userID, byUser[userID], now, opsOnly)
		if err != nil {
			logging.Errorf("notify: user %d: %v", userID, err)
			app.notifyQuiet.forget(userID)
			continue
		}
		created += n
	}
	if last := notifyPrunedAt.Load(); now.Unix()-last >= int64(notifyPruneEvery/time.Second) && notifyPrunedAt.CompareAndSwap(last, now.Unix()) {
		if err := app.queries.PruneNotificationSeen(ctx, now.Add(-notifySeenKeep)); err != nil {
			logging.Errorf("notify: prune seen keys: %v", err)
		}
		if err := app.queries.PruneNotifications(ctx, db.PruneNotificationsParams{
			ReadBefore:    timeSet(now.Add(-notifyReadKeep)),
			CreatedBefore: now.Add(-notifyUnreadKeep),
		}); err != nil {
			logging.Errorf("notify: prune notifications: %v", err)
		}
	}
	return created
}

// notifyUser handles one account. Recording the keys as seen and
// creating the notifications happen in one transaction: an event is
// never announced twice because one half was saved without the other.
//
// opsOnly leaves the account's stored data unread and looks only at
// ops (new ones, and reminders, which go by the clock). Each kind of
// event keeps its own baseline and its own keys, so a pass that looks
// at fewer sources neither announces nor forgets anything on behalf
// of the ones it skipped.
func (app *Application) notifyUser(ctx context.Context, userID int64, chars []db.Character, now time.Time, opsOnly bool) (int, error) {
	c := newNotifyCollector()
	if !opsOnly {
		bundles := app.loadCharSnaps(ctx, userID, chars, []string{widgetBriefing})
		c = app.collectNotifyEvents(ctx, userID, bundles, now)
		app.foresee(ctx, userID, bundles, now)
	}
	prefs := app.notifyPrefsFor(ctx, userID)
	app.notifyOpEvents(ctx, c, userID, prefs, now)
	if _, ok := notifyKindByID(notifySRPNew); ok {
		app.notifySRPEvents(ctx, c, userID, now)
	}
	if len(c.sources) == 0 {
		return 0, nil
	}

	current := make([]string, 0, len(c.sources)+len(c.events))
	for key := range c.sources {
		current = append(current, key)
	}
	for _, ev := range c.events {
		current = append(current, ev.Key)
	}
	seenKeys, err := app.queries.ListNotificationSeenKeys(ctx, db.ListNotificationSeenKeysParams{UserID: userID, EventKeys: current})
	if err != nil {
		return 0, fmt.Errorf("read seen keys: %w", err)
	}
	seen := make(map[string]bool, len(seenKeys))
	for _, key := range seenKeys {
		seen[key] = true
	}

	var record []string
	var announce []notifyEvent
	for key := range c.sources {
		if !seen[key] {
			record = append(record, key) // a new source: its baseline is taken below
		}
	}
	for _, ev := range c.events {
		if seen[ev.Key] {
			continue
		}
		record = append(record, ev.Key)
		if !seen[ev.Source] || prefs.silent(ev) {
			continue // part of a baseline, or switched off (for this character)
		}
		announce = append(announce, ev)
	}

	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if len(record) > 0 {
		if err := q.InsertNotificationSeen(ctx, db.InsertNotificationSeenParams{UserID: userID, EventKeys: record, SeenAt: now}); err != nil {
			return 0, fmt.Errorf("record seen keys: %w", err)
		}
	}
	// A channel switched off stops only that channel's delivery:
	// the keys above are still recorded as seen, so switching back
	// on starts from now and never replays what was missed.
	if !prefs.channelOff(notifyChannelInApp) {
		for _, ev := range announce {
			if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
				UserID: userID, CharacterID: ev.CharacterID, Kind: ev.Kind, Title: ev.Title, Url: ev.URL, CreatedAt: now,
			}); err != nil {
				return 0, fmt.Errorf("create notification: %w", err)
			}
		}
	}
	if err := q.TouchNotificationSeen(ctx, db.TouchNotificationSeenParams{
		UserID: userID, EventKeys: current, SeenAt: now, StaleBefore: now.Add(-notifySeenTouch),
	}); err != nil {
		return 0, fmt.Errorf("renew seen keys: %w", err)
	}
	// Standing kinds: a state that no longer holds is forgotten. Only
	// where the source was readable this time, so that a failed read
	// is not mistaken for everything having cleared.
	for _, kind := range notifyKindList() {
		if !kind.Standing || !c.sources[fmt.Sprintf("base|%s|%d", kind.ID, userID)] {
			continue
		}
		keep := []string{} // never nil: an empty list must still mean "forget them all"
		for _, ev := range c.events {
			if ev.Kind == kind.ID {
				keep = append(keep, ev.Key)
			}
		}
		if err := q.ForgetNotificationSeenExcept(ctx, db.ForgetNotificationSeenExceptParams{
			UserID: userID, KeyPattern: kind.ID + "|%", KeepKeys: keep,
		}); err != nil {
			return 0, fmt.Errorf("forget cleared states: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	// Saved; now the same news goes out on the channels the account
	// has on: any browser it has subscribed (notify_push.go), and the
	// module channels (deliverToModules), each of which honors its
	// own switch. A failed send loses nothing: what was announced
	// here is not news again.
	if !prefs.channelOff(notifyChannelDesktop) {
		app.pushToUser(ctx, userID, pushMessagesFor(announce))
	}
	app.deliverToModules(ctx, userID, announce)
	return len(announce), nil
}

// notifyOpWindow is how far ahead an op has to start to be announced;
// nobody needs telling about one planned for next year.
const notifyOpWindow = 90 * 24 * time.Hour

// notifyReminderMinutes are the leads a reminder can be set to, and
// defaultReminderMinutes the one used until the user picks.
var notifyReminderMinutes = []int{10, 15, 30, 60, 120}

const defaultReminderMinutes = 30

func validReminderMinutes(n int) bool {
	for _, allowed := range notifyReminderMinutes {
		if n == allowed {
			return true
		}
	}
	return false
}

// reminderLead is how long before an op starts the account is
// reminded of it.
func (p notifyPrefs) reminderLead() time.Duration {
	if validReminderMinutes(p.OpReminderMinutes) {
		return time.Duration(p.OpReminderMinutes) * time.Minute
	}
	return defaultReminderMinutes * time.Minute
}

// opRecipients picks, per corporation, the character of the account
// that is told about its ops under kind: the first that has not
// switched the kind off, else the first.
func opRecipients(rows []db.ListWorkingCharacterCorporationsByUserRow, prefs notifyPrefs, kind string) map[int64]int64 {
	recipient := map[int64]int64{}
	for _, row := range rows {
		if row.CorporationID == 0 {
			continue
		}
		current, known := recipient[row.CorporationID]
		if !known || (prefs.offFor(kind, current) && !prefs.offFor(kind, row.CharacterID)) {
			recipient[row.CorporationID] = row.CharacterID
		}
	}
	return recipient
}

// notifyOpEvents: ops ahead, planned for a corporation the account
// has a character in (ops.go). Two things are said about one:
//
// That it was planned (notifyOp). The ops already there when a
// corporation's calendar first becomes readable are its baseline. An
// op the account made itself is not news to it, and neither is one
// that was cancelled before it was seen.
//
// That it is about to start (notifyOpReminder): once the start is
// within the account's chosen lead and has not passed. The worker
// looks about once a minute, so the reminder comes within a minute or
// so of the chosen time. It goes only to an account that signed up to
// the op as coming or maybe, in the name of the character it signed up
// with; everyone else in the corporation was already told when the op
// was planned. Signing up inside the lead brings the reminder at the
// worker's next look. A start that is moved is reminded of again.
//
// Each is said once to the account. The planning is said in the name
// of one of its characters in that corporation: the first that has
// not switched that kind off. With every character concerned switched
// off, an event is still recorded as seen, so switching back on starts
// from then.
func (app *Application) notifyOpEvents(ctx context.Context, c *notifyCollector, userID int64, prefs notifyPrefs, now time.Time) {
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("notify: corporations of user %d: %v", userID, err)
		return
	}
	mine := map[int64]bool{}
	seenCorp := map[int64]bool{}
	var corps []int64
	for _, row := range rows {
		mine[row.CharacterID] = true
		if row.CorporationID != 0 && !seenCorp[row.CorporationID] {
			seenCorp[row.CorporationID] = true
			corps = append(corps, row.CorporationID)
		}
	}
	if len(corps) == 0 {
		return
	}
	ops, err := app.queries.ListOpsForCorporationsBetween(ctx, db.ListOpsForCorporationsBetweenParams{
		CorporationIds: corps, FromTime: now, ToTime: now.Add(notifyOpWindow),
	})
	if err != nil {
		logging.Errorf("notify: ops for user %d: %v", userID, err)
		return
	}
	planned := opRecipients(rows, prefs, notifyOp)
	plannedSource, remindedSource := map[int64]string{}, map[int64]string{}
	for _, corp := range corps {
		plannedSource[corp] = c.source(notifyOp, corp)
		remindedSource[corp] = c.source(notifyOpReminder, corp)
	}

	// The ops about to start, and which of the account's characters
	// signed up to each as coming or maybe: the one reminded. Where
	// several did, one that has not switched reminders off.
	lead := prefs.reminderLead()
	var soon []int64
	for _, op := range ops {
		if !op.CancelledAt.Valid && !op.StartsAt.After(now.Add(lead)) {
			soon = append(soon, op.ID)
		}
	}
	signedUp := map[int64]int64{} // op -> character
	if len(soon) > 0 {
		signups, err := app.queries.ListOpSignupsForOps(ctx, soon)
		if err != nil {
			logging.Errorf("notify: sign-ups for user %d: %v", userID, err)
			return
		}
		for _, s := range signups {
			if s.UserID != userID || s.Response == opNo {
				continue
			}
			current, known := signedUp[s.OpID]
			if !known || (prefs.offFor(notifyOpReminder, current) && !prefs.offFor(notifyOpReminder, s.CharacterID)) {
				signedUp[s.OpID] = s.CharacterID
			}
		}
	}

	for _, op := range ops {
		if op.CancelledAt.Valid {
			continue
		}
		// The corporation is named where its name is already known;
		// a title is stored, so it never carries a placeholder.
		corp := ""
		if name, settled := app.resolvedCorpName(ctx, op.CorporationID); settled && name != "" {
			corp = " for " + name
		}
		if !mine[op.CreatedByCharacter] {
			c.add(notifyEvent{
				Kind: notifyOp, CharacterID: planned[op.CorporationID], Source: plannedSource[op.CorporationID],
				Key:   fmt.Sprintf("op|%d", op.ID),
				Title: fmt.Sprintf("New op%s: %s, %s", corp, op.Title, op.StartsAt.UTC().Format("Jan 2 15:04")),
				URL:   opURL(op.ID),
			})
		}
		if character, coming := signedUp[op.ID]; coming {
			c.add(notifyEvent{
				Kind: notifyOpReminder, CharacterID: character, Source: remindedSource[op.CorporationID],
				// The start is part of the key: an op that is moved is
				// reminded of again at its new time.
				Key:   fmt.Sprintf("opremind|%d|%d", op.ID, op.StartsAt.Unix()),
				Title: fmt.Sprintf("Op%s starts at %s EVE time: %s", corp, op.StartsAt.UTC().Format("15:04"), op.Title),
				URL:   opURL(op.ID),
			})
		}
	}
}

// deliverToModules offers one batch of new notifications to every
// registered module channel. It is a fan-out: each Deliverer sees every
// notification and honors the account's own preferences for its channel;
// what a Deliverer reports is only whether THIS channel delivered it (for
// the log), and never stops the others. See module.Deliverer.
func (app *Application) deliverToModules(ctx context.Context, userID int64, events []notifyEvent) {
	if len(events) == 0 {
		return
	}
	n := module.Notification{}
	for _, ev := range events {
		n.Events = append(n.Events, module.NotifyEvent{Kind: ev.Kind, CharacterID: ev.CharacterID, Title: ev.Title, URL: ev.URL})
	}
	for _, msg := range pushMessagesFor(events) {
		n.Messages = append(n.Messages, module.NotifyMessage{Title: msg.Title, Body: msg.Body, URL: msg.URL})
	}
	for _, m := range registeredModules {
		for _, deliver := range m.Deliverers {
			func() {
				defer recoverWorkerPanic("notification delivery: " + m.ID)
				if deliver(ctx, app, userID, n) {
					logging.Debugf("notify: delivered to user %d through %s", userID, m.ID)
				}
			}()
		}
	}
}
