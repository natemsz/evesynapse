package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
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
	notifySkill      = "skill"
	notifyMail       = "mail"
	notifyCalendar   = "calendar"
	notifyKillmail   = "killmail"
	notifyPI         = "pi"
	notifyIndustry   = "industry"
	notifyOp         = "op"
	notifyOpReminder = "op_reminder"
)

// notifyKind describes one kind of notification.
type notifyKind struct {
	ID     string
	Title  string // as the settings page names it
	Module string // the manifest module whose data it reads
	// Standing is a state rather than an event: it is announced when
	// it starts to hold and again if it clears and comes back.
	Standing bool
	// Account: the kind belongs to the account, not to one of its
	// characters, so it cannot be switched per character.
	Account bool
}

// notifyKinds is every kind, in the order the settings page lists them.
var notifyKinds = []notifyKind{
	{ID: notifyWatch, Title: "Watch list alerts", Module: "market_public", Standing: true, Account: true},
	{ID: notifySkill, Title: "Skill complete", Module: "skills"},
	{ID: notifyMail, Title: "New mail", Module: "mail"},
	{ID: notifyCalendar, Title: "New in-game calendar event", Module: "calendar"},
	// Ops are EveSynapse's own (ops.go): any character sees its
	// corporation's, with no access to grant.
	{ID: notifyOp, Title: "New op on the calendar"},
	{ID: notifyOpReminder, Title: "Op starting soon"},
	{ID: notifyKillmail, Title: "New killmail", Module: "killmails"},
	{ID: notifyPI, Title: "Stopped planetary extractors", Module: "planets"},
	{ID: notifyIndustry, Title: "Industry job complete", Module: "industry"},
}

// notifyKindByID returns the kind for id.
func notifyKindByID(id string) (notifyKind, bool) {
	for _, k := range notifyKinds {
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
		ev.Title = strings.ToValidUTF8(ev.Title[:notifyTitleMax], "") + "…"
	}
	c.events = append(c.events, ev)
}

// collectNotifyEvents lists what is true right now across one
// account's characters, from stored snapshots only.
func (app *Application) collectNotifyEvents(ctx context.Context, userID int64, bundles []*charSnaps, now time.Time) *notifyCollector {
	c := newNotifyCollector()
	for _, b := range bundles {
		app.notifySkillEvents(ctx, c, b, now)
		app.notifyMailEvents(c, b)
		app.notifyCalendarEvents(c, b, now)
		app.notifyKillmailEvents(ctx, c, b)
		app.notifyPIEvents(ctx, c, b, now)
		app.notifyIndustryEvents(ctx, c, b, now)
	}
	app.notifyWatchEvents(ctx, c, userID)
	return c
}

// notifySkillEvents: queue entries whose finish time has passed. The
// queue keeps a finished entry until the character next logs in, so
// time passing is enough; no refetch is needed to notice.
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
			URL:   fmt.Sprintf("/character/?character=%d", id),
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
			URL:   fmt.Sprintf("/mail/?character=%d", id),
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
			URL:   fmt.Sprintf("/calendar/?character=%d", id),
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
		if !errors.Is(err, sql.ErrNoRows) {
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
			URL:   fmt.Sprintf("/killmails/?character=%d", id),
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
				URL:   fmt.Sprintf("/planets/?character=%d", id),
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
			URL:   fmt.Sprintf("/industry/?character=%d", id),
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
		rows := app.recentHistoryRows(ctx, e.RegionID, e.TypeID, markethistory.ChartRows)
		pct, ok := markethistory.ChangePct(rows, 7)
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
			Key: fmt.Sprintf("watch|%d|%d|%s", e.TypeID, e.RegionID, way),
			Title: fmt.Sprintf("%s moved %s over 7 days in %s, past your %s watch",
				app.typeNameOrID(ctx, e.TypeID), markethistory.ChangeDirection(pct), app.marketRegionLabel(ctx, e.RegionID),
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
	return prefs
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
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("notify: settings for user %d: %v", userID, err)
		}
		return notifyPrefs{}
	}
	return parseNotifyPrefs(blob)
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
	}
	prefs := app.notifyPrefsFor(ctx, userID)
	app.notifyOpEvents(ctx, c, userID, prefs, now)
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

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	q := app.queries.WithTx(tx)
	if len(record) > 0 {
		if err := q.InsertNotificationSeen(ctx, db.InsertNotificationSeenParams{UserID: userID, EventKeys: record, SeenAt: now}); err != nil {
			return 0, fmt.Errorf("record seen keys: %w", err)
		}
	}
	for _, ev := range announce {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			UserID: userID, CharacterID: ev.CharacterID, Kind: ev.Kind, Title: ev.Title, Url: ev.URL, CreatedAt: now,
		}); err != nil {
			return 0, fmt.Errorf("create notification: %w", err)
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
	for _, kind := range notifyKinds {
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
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	// Saved; now the same news goes to any browser the account has
	// subscribed (notify_push.go). A failed send loses nothing: the
	// notification is already in the top bar.
	app.pushToUser(ctx, userID, pushMessagesFor(announce))
	app.discordToUser(ctx, userID, announce)
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
func opRecipients(rows []db.ListCharacterCorporationsByUserRow, prefs notifyPrefs, kind string) map[int64]int64 {
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
	rows, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
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
