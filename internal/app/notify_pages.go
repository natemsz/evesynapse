package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Notifications, as the user meets them: the icon in the top bar with
// its short list, and the page that lists them all. Everything here
// reads the records the worker wrote (notify.go); nothing computes
// news at render time.
// ---------------------------------------------------------------------------

// Top-bar icons. A kind with a picture of its own shows it when it is
// the only kind unread; otherwise the generic one stands for the lot.
const (
	notifyIconNone    = "none"
	notifyIconGeneric = "generic"
)

// notifyKindIcons: the kinds that have a picture of their own.
var notifyKindIcons = map[string]bool{notifyMail: true, notifyPI: true}

// notifyKindWording is how a count of each kind reads in the list:
// "1 new mail", "3 new mails".
var notifyKindWording = map[string][2]string{
	notifyWatch:      {"watch list alert", "watch list alerts"},
	notifySkill:      {"skill finished", "skills finished"},
	notifyMail:       {"new mail", "new mails"},
	notifyCalendar:   {"new calendar event", "new calendar events"},
	notifyKillmail:   {"new killmail", "new killmails"},
	notifyPI:         {"planet with stopped extractors", "planets with stopped extractors"},
	notifyIndustry:   {"industry job finished", "industry jobs finished"},
	notifyOp:         {"new op", "new ops"},
	notifyOpReminder: {"op starting soon", "ops starting soon"},
}

// notifyBadge is the top-bar icon and the summarized list behind it.
type notifyBadge struct {
	Unread int64
	Icon   string // notifyIconNone, notifyIconGeneric, or a kind id
	Rows   []notifyBadgeRow
}

// notifyBadgeRow is one line of the list: a kind, how many are
// unread, and the newest one's text.
type notifyBadgeRow struct {
	Kind   string
	Label  string // "3 new mails"
	Latest string // the newest notification of the kind
}

// Label names the icon for assistive technology and the tooltip.
func (b *notifyBadge) Label() string {
	if b.Unread == 0 {
		return "Notifications: nothing new"
	}
	return fmt.Sprintf("Notifications: %d unread", b.Unread)
}

// notifyBadgeFor builds the top-bar badge for an account: one indexed
// read of its unread notifications, grouped by kind.
func (app *Application) notifyBadgeFor(ctx context.Context, userID int64) *notifyBadge {
	badge := &notifyBadge{Icon: notifyIconNone}
	if userID == 0 {
		return badge
	}
	rows, err := app.queries.ListUnreadNotificationSummary(ctx, userID)
	if err != nil {
		logging.Errorf("notify: unread summary for user %d: %v", userID, err)
		return badge
	}
	byKind := make(map[string]db.ListUnreadNotificationSummaryRow, len(rows))
	for _, row := range rows {
		byKind[row.Kind] = row
	}
	for _, kind := range notifyKindList() {
		row, ok := byKind[kind.ID]
		if !ok {
			continue
		}
		words := notifyKindWording[kind.ID]
		word := words[1]
		if row.Unread == 1 {
			word = words[0]
		}
		badge.Unread += row.Unread
		badge.Rows = append(badge.Rows, notifyBadgeRow{
			Kind:   kind.ID,
			Label:  fmt.Sprintf("%d %s", row.Unread, word),
			Latest: row.Title,
		})
	}
	switch {
	case len(badge.Rows) == 1 && notifyKindIcons[badge.Rows[0].Kind]:
		badge.Icon = badge.Rows[0].Kind
	case len(badge.Rows) > 0:
		badge.Icon = notifyIconGeneric
	}
	return badge
}

// notificationsView is the /notifications/ page.
type notificationsView struct {
	Rows   []notificationRow
	Unread int
}

type notificationRow struct {
	Kind   string // the kind's name, as the settings list it
	Title  string
	URL    string
	When   string // "Oct 8, 14:02 UTC"
	Unread bool
}

// notificationsPageSize is how many notifications the page lists.
// Older ones are still stored until the worker's clean-up drops them.
const notificationsPageSize = 100

func (app *Application) handleNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	rows, err := app.queries.ListNotifications(ctx, db.ListNotificationsParams{UserID: userID, Limit: notificationsPageSize})
	if err != nil {
		logging.Errorf("notifications: list for user %d: %v", userID, err)
		data.Error = "Could not load notifications; check the server log."
		app.render(ctx, w, http.StatusOK, "notifications.html", data)
		return
	}
	view := &notificationsView{}
	for _, row := range rows {
		name := row.Kind
		if kind, ok := notifyKindByID(row.Kind); ok {
			name = kind.Title
		}
		if !row.ReadAt.Valid {
			view.Unread++
		}
		view.Rows = append(view.Rows, notificationRow{
			Kind:   name,
			Title:  row.Title,
			URL:    notifyTarget(row.Url),
			When:   row.CreatedAt.UTC().Format("Jan 2, 15:04") + " UTC",
			Unread: !row.ReadAt.Valid,
		})
	}
	data.Notifications = view
	app.render(ctx, w, http.StatusOK, "notifications.html", data)
}

// handleNotificationsRead marks the account's notifications read:
// all of them, or one kind when the form names it.
func (app *Application) handleNotificationsRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if err := r.ParseForm(); err == nil && userID != 0 {
		app.markNotificationsRead(ctx, userID, r.FormValue("kind"))
	}
	http.Redirect(w, r, "/notifications/", http.StatusSeeOther)
}

// handleNotificationsOpen is a row of the top-bar list being
// followed: that kind is marked read and the visitor is sent to
// where its newest notification points. A POST, because it changes
// what is unread.
func (app *Application) handleNotificationsOpen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	target := "/notifications/"
	if err := r.ParseForm(); err == nil && userID != 0 {
		if kind, ok := notifyKindByID(r.FormValue("kind")); ok {
			if rows, err := app.queries.ListUnreadNotificationSummary(ctx, userID); err == nil {
				for _, row := range rows {
					if row.Kind == kind.ID {
						target = notifyTarget(row.Url)
					}
				}
			}
			app.markNotificationsRead(ctx, userID, kind.ID)
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// markNotificationsRead marks one kind read, or every kind when kind
// is not one that exists.
func (app *Application) markNotificationsRead(ctx context.Context, userID int64, kind string) {
	now := timeSet(time.Now().UTC())
	var err error
	if _, ok := notifyKindByID(kind); ok {
		err = app.queries.MarkNotificationsReadByKind(ctx, db.MarkNotificationsReadByKindParams{ReadAt: now, UserID: userID, Kind: kind})
	} else {
		err = app.queries.MarkNotificationsRead(ctx, db.MarkNotificationsReadParams{ReadAt: now, UserID: userID})
	}
	if err != nil {
		logging.Errorf("notifications: mark read for user %d: %v", userID, err)
	}
}

// safeLocalNext reports whether n is safe to hand to http.Redirect
// as an on-site target: a path beginning with a single slash. The
// second byte matters as much as the first — browsers treat "//"
// and "/\" as scheme-relative addresses and leave the site — so a
// backslash is rejected anywhere in the value, and CR/LF would let
// the value break out of the Location header entirely. Every
// handler that redirects to a user-supplied "next" goes through
// here rather than re-checking by hand.
func safeLocalNext(n string) bool {
	if !strings.HasPrefix(n, "/") {
		return false
	}
	if len(n) > 1 && (n[1] == '/' || n[1] == '\\') {
		return false
	}
	return !strings.ContainsAny(n, "\\\r\n")
}

// notifyTarget is where a notification leads. Stored addresses are
// the app's own paths; anything else falls back to the list, so a
// stored value can never send the visitor to another site.
func notifyTarget(url string) string {
	if safeLocalNext(url) {
		return url
	}
	return "/notifications/"
}

// ---------------------------------------------------------------------------
// Settings: which kinds the account wants. One switch per kind, and
// beside each how many of the account's characters can produce it,
// since a kind reads a module's data and a character that has not
// granted that module's scopes has none.
// ---------------------------------------------------------------------------

// notifySettingsView is the /notifications/settings page.
type notifySettingsView struct {
	Rows []notifySettingRow
	// The delivery channels, the same switches the first-run
	// prompt offers: InApp always, Desktop when this server can
	// push (its per-browser panel sits with the channels now).
	InApp   bool
	Desktop bool
	// Discord direct messages: the switch in the channels list,
	// naming the linked account. It is the one on the account's
	// Discord link — the same value the prompt's switch writes, so
	// the two never disagree about what is stored. The Discord
	// section below the form connects the account; it carries no
	// switch of its own. Offered follows discordChannelState:
	// a link, or an install that offers the linking.
	DiscordOffered bool
	DiscordLinked  bool
	Discord        bool
	DiscordName    string
	// Browser push: whether this server offers it, the key a browser
	// needs to subscribe, and how many browsers the account has
	// subscribed so far.
	PushConfigured bool
	PushKey        string
	PushDevices    int64
	// ChannelSections and AccountSections are what the modules that
	// deliver notifications add to the page: a box inside the form, and
	// a section below it (module.SettingsSection).
	ChannelSections []template.HTML
	AccountSections []template.HTML
}

type notifySettingRow struct {
	ID    string
	Title string
	On    bool
	// Characters is who the kind can be switched for, one by one; nil
	// for a kind that belongs to the account. Summary says how many of
	// them it is on for ("On for 3 of 5 characters").
	Characters []notifyCharacterOption
	Summary    string
	// Minutes is the choice of lead for the op reminder, with the
	// current one marked; nil for every other kind.
	Minutes []notifyMinutesOption
}

// notifyMinutesOption is one lead the op reminder can be set to.
type notifyMinutesOption struct {
	Value    int
	Label    string // "30 minutes", "2 hours"
	Selected bool
}

// notifyMinutesOptions lists the leads, the account's own marked.
func notifyMinutesOptions(prefs notifyPrefs) []notifyMinutesOption {
	current := int(prefs.reminderLead() / time.Minute)
	out := make([]notifyMinutesOption, 0, len(notifyReminderMinutes))
	for _, n := range notifyReminderMinutes {
		label := fmt.Sprintf("%d minutes", n)
		switch {
		case n == 60:
			label = "1 hour"
		case n > 60 && n%60 == 0:
			label = fmt.Sprintf("%d hours", n/60)
		}
		out = append(out, notifyMinutesOption{Value: n, Label: label, Selected: n == current})
	}
	return out
}

// notifyCharacterOption is one character under one kind.
type notifyCharacterOption struct {
	ID   int64
	Name string
	On   bool
	// Locked: the character has not granted the access this kind
	// reads, so there is nothing to switch; RelinkURL is the sign-in
	// that asks for just that.
	Locked    bool
	RelinkURL string
}

// notifyCharactersSummary is the line shown on a kind's character
// list: how many characters it is on for, out of those that can get
// it.
func notifyCharactersSummary(options []notifyCharacterOption) string {
	on, able := 0, 0
	for _, o := range options {
		if o.Locked {
			continue
		}
		able++
		if o.On {
			on++
		}
	}
	noun := "characters"
	if len(options) == 1 {
		noun = "character"
	}
	switch {
	case able == 0:
		return fmt.Sprintf("No access granted on %d %s", len(options), noun)
	case able < len(options):
		return fmt.Sprintf("On for %d of %d %s (%d without access)", on, len(options), noun, len(options)-able)
	}
	return fmt.Sprintf("On for %d of %d %s", on, len(options), noun)
}

// notifySettingRows builds the page's rows from the stored settings
// and the account's characters.
func notifySettingRows(prefs notifyPrefs, characters []db.Character) []notifySettingRow {
	rows := make([]notifySettingRow, 0, len(notifyKindList()))
	for _, kind := range notifyKindList() {
		row := notifySettingRow{ID: kind.ID, Title: kind.Title, On: !prefs.off(kind.ID)}
		if kind.ID == notifyOpReminder {
			row.Minutes = notifyMinutesOptions(prefs)
		}
		if !kind.Account && len(characters) > 0 {
			module, scoped := moduleByID(kind.Module)
			scoped = scoped && module.Layer != layerPublic
			for _, ch := range characters {
				option := notifyCharacterOption{ID: ch.CharacterID, Name: ch.Name, On: !prefs.offFor(kind.ID, ch.CharacterID)}
				if scoped && module.statusFor(scopeSet(ch.Scopes)).State == moduleLocked {
					option.Locked, option.On = true, false
					option.RelinkURL = relinkURL(module.ID, ch.CharacterID)
				}
				row.Characters = append(row.Characters, option)
			}
			row.Summary = notifyCharactersSummary(row.Characters)
		}
		rows = append(rows, row)
	}
	return rows
}

// settingsPath is the one settings page; its Notifications tab is
// where the notification settings live.
const settingsPath = "/settings#notifications"

// handleNotificationSettingsRedirect sends the old notification
// settings address at the settings page's Notifications tab.
func (app *Application) handleNotificationSettingsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, settingsPath, http.StatusSeeOther)
}

func (app *Application) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	data.Scripts = []string{"/static/settings.js", "/static/webauthn.js"}
	userID := app.userID(ctx)
	prefs := app.notifyPrefsFor(ctx, userID)
	view := &notifySettingsView{
		Rows:    notifySettingRows(prefs, app.sessionCharacters(ctx)),
		InApp:   !prefs.channelOff(notifyChannelInApp),
		Desktop: !prefs.channelOff(notifyChannelDesktop),
	}
	view.DiscordOffered, view.DiscordLinked, view.Discord, view.DiscordName = app.discordChannelState(ctx, userID)
	if app.push != nil {
		view.PushConfigured, view.PushKey = true, app.push.PublicKey
		if n, err := app.queries.CountPushSubscriptionsByUser(ctx, userID); err == nil {
			view.PushDevices = n
		}
	}
	for _, m := range registeredModules {
		if sec := m.NotifySettings; sec != nil {
			if sec.Channel != nil {
				view.ChannelSections = append(view.ChannelSections, sec.Channel(ctx, app, r, userID))
			}
			if sec.Account != nil {
				view.AccountSections = append(view.AccountSections, sec.Account(ctx, app, r, userID))
			}
		}
	}
	data.NotifySettings = view
	data.Security = app.securityViewFor(ctx, r, userID)
	app.attachAPIKeys(ctx, &data, userID)
	app.render(ctx, w, http.StatusOK, "settings.html", data)
}

// handleNotificationSettingsSave stores the settings. The form sends
// the kinds that are ticked, and under each kind the characters that
// are ticked; everything else is off. A character that could not be
// ticked (it has not granted the access) is left as it was, so it does
// not come back switched off once it has.
func (app *Application) handleNotificationSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, settingsPath, http.StatusSeeOther)
		return
	}
	on := map[string]bool{}
	for _, id := range r.Form["kind"] {
		on[id] = true
	}
	before := app.notifyPrefsFor(ctx, userID)
	prefs := notifyPrefs{OffFor: map[string][]int64{}, OpReminderMinutes: before.OpReminderMinutes}
	// The prompt's answer and the channel switches are this form's
	// to carry, not to reset: the answer is kept as it was, and the
	// channels follow the form's own boxes when it showed them
	// (channels_shown) — a form without them changes nothing there.
	prefs.PromptAnswered = before.PromptAnswered
	prefs.OffChannels = before.OffChannels
	if r.Form.Get("channels_shown") == "1" {
		desktopOn := !before.channelOff(notifyChannelDesktop)
		if app.push != nil {
			desktopOn = r.Form.Get("channel_desktop") == "1"
		}
		prefs.OffChannels = offChannels(r.Form.Get("channel_inapp") == "1", desktopOn)
	}
	if n, err := strconv.Atoi(r.Form.Get("op_reminder_minutes")); err == nil && validReminderMinutes(n) {
		prefs.OpReminderMinutes = n
	}
	for _, row := range notifySettingRows(before, app.sessionCharacters(ctx)) {
		if !on[row.ID] {
			prefs.Off = append(prefs.Off, row.ID)
		}
		ticked := map[string]bool{}
		for _, id := range r.Form["char."+row.ID] {
			ticked[id] = true
		}
		for _, option := range row.Characters {
			off := !ticked[strconv.FormatInt(option.ID, 10)]
			if option.Locked {
				off = before.offFor(row.ID, option.ID)
			}
			if off {
				prefs.OffFor[row.ID] = append(prefs.OffFor[row.ID], option.ID)
			}
		}
	}
	err := app.storeNotifyPrefs(ctx, userID, prefs)
	// The page's one Save button also carries the modules' channel
	// boxes; each module reads its own (module.SettingsSection.Save).
	for _, m := range registeredModules {
		if sec := m.NotifySettings; err == nil && sec != nil && sec.Save != nil {
			err = sec.Save(ctx, app, userID, r.Form)
		}
	}
	if err != nil {
		logging.Errorf("notifications: save settings for user %d: %v", userID, err)
		app.flash(ctx, "The settings could not be saved; check the server log.")
	} else {
		app.flash(ctx, "Notification settings saved.")
	}
	http.Redirect(w, r, settingsPath, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// The live icon. An open page asks GET /notifications/badge every
// NOTIFY_POLL_SECONDS (app.js) and swaps the icon's contents in when
// they have changed, so new notifications show without a reload.
//
// It is kept cheap on purpose: the answer is the same one indexed read
// every page render already makes, a page only asks while it is on
// screen, and an unchanged icon is answered 304 with no body.
// ---------------------------------------------------------------------------

const (
	// defaultNotifyPoll is how often an open page asks, when
	// NOTIFY_POLL_SECONDS is not set.
	defaultNotifyPoll = 30
	// A page never asks more often than minNotifyPoll or less often
	// than maxNotifyPoll, whatever is configured.
	minNotifyPoll = 5
	maxNotifyPoll = 3600
)

// parseNotifyPoll reads NOTIFY_POLL_SECONDS: whole seconds between an
// open page's checks. Unset is the default, 0 turns the checks off
// (the icon then updates on page loads only), and anything else is
// kept between minNotifyPoll and maxNotifyPoll. ok is false when the
// value cannot be read; the default is returned then.
func parseNotifyPoll(raw string) (seconds int, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultNotifyPoll, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultNotifyPoll, false
	}
	switch {
	case n == 0:
		return 0, true
	case n < minNotifyPoll:
		return minNotifyPoll, true
	case n > maxNotifyPoll:
		return maxNotifyPoll, true
	}
	return n, true
}

// notifyBadgePath is where an open page asks for the icon's contents.
const notifyBadgePath = "/notifications/badge"

// notifyBadgeHeader marks the answer as the icon's contents. A page
// whose session has ended is sent to the sign-in page instead, and
// must not mistake that page for an icon.
const notifyBadgeHeader = "X-Notify-Badge"

// handleNotifyBadge serves GET /notifications/badge: the contents of
// the top-bar icon for the signed-in account, as the same HTML every
// page carries. X-Notify-Unread is the unread count.
func (app *Application) handleNotifyBadge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	badge := app.notifyBadgeFor(ctx, userID)
	ts, err := parsedTemplate(&fragmentTemplates, "notify-bell", "notifybell.html")
	if err != nil {
		logging.Errorf("notify: parse the badge template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := ts.ExecuteTemplate(&buf, "notify-bell", badge); err != nil {
		logging.Errorf("notify: render the badge: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(buf.Bytes())
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set(notifyBadgeHeader, "1")
	h.Set("X-Notify-Unread", strconv.FormatInt(badge.Unread, 10))
	h.Set("ETag", etag)
	// It is one account's own state: never stored by anything shared,
	// and always checked again.
	h.Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
