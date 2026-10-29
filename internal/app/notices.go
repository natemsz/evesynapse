package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/htmlsanitize"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Global notices: what an administrator posts to every visitor. A
// service announcement, an in-game promotion or giveaway, a security
// warning. Each shows as a banner on every page until it ends, is
// retired, or the visitor dismisses it; one with a detail post opens
// it in a dialog.
//
// The Notices tab of the Admin page writes them. It is built for the
// day an administrator's session is in the wrong hands:
//
//   - The detail post is Markdown, rendered and sanitized on the
//     server at save time (htmlsanitize.Markdown). Only that HTML is
//     stored for display, and it is sanitized once more when the
//     banner cache loads it. The editor's preview asks the same
//     renderer, so what the admin sees is what visitors get.
//   - Titles and posts are capped, publishing is rate-limited per
//     account and for everyone, and only a few notices can be live at
//     once.
//   - App notifications (the bell and browser push) go out only when
//     the admin asks, only for warnings and critical notices, and at
//     most once a day. Notices never go to Discord.
//   - Every publish, edit, retire and send is written to an audit
//     trail with the account and character that made it.
//
// Pages read the live notices from a short in-memory cache, so a
// render costs at most one small indexed read per cache period plus
// the signed-in reader's dismissals.
// ---------------------------------------------------------------------------

// Notice limits.
const (
	noticeTitleMax    = 120  // characters
	noticeBodyMax     = 8000 // characters of Markdown
	noticeLiveMax     = 3    // live at once
	noticeBannerLimit = 5    // rows a page read takes, above the live cap
	noticeListLimit   = 50   // rows on the admin tab
	noticeAuditLimit  = 30

	noticeMinDuration = 10 * time.Minute
	noticeMaxDuration = 30 * 24 * time.Hour

	// Publishing: per account and for everyone, per hour. Edits
	// have their own per-account allowance; retiring is never
	// limited, so a bad notice can always be taken down.
	noticePublishPerAccount = 3
	noticePublishGlobal     = 6
	noticeEditPerAccount    = 20
	noticeRateWindow        = time.Hour

	// App notifications: once a day, for everyone.
	noticeUrgentWindow = 24 * time.Hour

	noticeCacheTTL = 30 * time.Second

	// noticeHiddenCookie carries an anonymous visitor's dismissals
	// (and a signed-in one's, so a swap hides them at once): notice
	// ids joined by dots. notices.js writes it.
	noticeHiddenCookie = "evesynapse-notices-hidden"
	noticeHiddenMax    = 20
)

// noticeSeverity is one of the fixed severities a notice is posted as.
type noticeSeverity struct {
	ID    string
	Label string
}

var noticeSeverities = []noticeSeverity{
	{"info", "Info"},
	{"promo", "Promotion"},
	{"warning", "Warning"},
	{"critical", "Critical"},
}

func noticeSeverityLabel(id string) (string, bool) {
	for _, s := range noticeSeverities {
		if s.ID == id {
			return s.Label, true
		}
	}
	return "", false
}

// noticeUrgentAllowed: only these severities may send app notifications.
func noticeUrgentAllowed(severity string) bool {
	return severity == "warning" || severity == "critical"
}

// noticeState is the Application's notice bookkeeping.
type noticeState struct {
	// write serializes the admin writes, so that the rate limits'
	// count and the insert they guard cannot interleave.
	write sync.Mutex

	cacheMu  sync.Mutex
	cached   []liveNotice
	cachedAt time.Time
}

// liveNotice is one banner as pages show it.
type liveNotice struct {
	ID       int64
	Title    string
	Severity string
	Label    string
	Body     template.HTML // sanitized; empty when there is no post
	Critical bool
}

// noticeBanners is what base.html renders above the page body.
type noticeBanners struct {
	Account bool // signed in: dismissals are saved to the account too
	Items   []liveNotice
}

// liveNotices returns the live notices, from the cache when it is fresh.
func (app *Application) liveNotices(ctx context.Context) []liveNotice {
	st := &app.notices
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	now := time.Now()
	if !st.cachedAt.IsZero() && now.Sub(st.cachedAt) < noticeCacheTTL {
		return st.cached
	}
	rows, err := app.queries.ListLiveGlobalNotices(ctx, db.ListLiveGlobalNoticesParams{Now: now.UTC(), RowLimit: noticeBannerLimit})
	if err != nil {
		logging.Errorf("notices: list live: %v", err)
		// Keep serving what was there; try again next period.
		st.cachedAt = now
		return st.cached
	}
	out := make([]liveNotice, 0, len(rows))
	for _, row := range rows {
		label, _ := noticeSeverityLabel(row.Severity)
		n := liveNotice{ID: row.ID, Title: row.Title, Severity: row.Severity, Label: label, Critical: row.Severity == "critical"}
		if strings.TrimSpace(row.BodyHtml) != "" {
			// Stored sanitized; sanitized again here, so a row
			// written around the app still cannot carry markup.
			n.Body = htmlsanitize.Sanitize(row.BodyHtml)
		}
		out = append(out, n)
	}
	st.cached, st.cachedAt = out, now
	return out
}

// dropNoticeCache makes the next page read the notices afresh.
func (app *Application) dropNoticeCache() {
	app.notices.cacheMu.Lock()
	app.notices.cachedAt = time.Time{}
	app.notices.cacheMu.Unlock()
}

type noticeHiddenKey struct{}

// withHiddenNotices puts the request's dismissal cookie into ctx
// (navStateMiddleware calls it), so the shell can leave those
// banners out.
func withHiddenNotices(ctx context.Context, r *http.Request) context.Context {
	c, err := r.Cookie(noticeHiddenCookie)
	if err != nil || c.Value == "" {
		return ctx
	}
	hidden := map[int64]bool{}
	for _, part := range strings.Split(c.Value, ".") {
		if len(hidden) >= noticeHiddenMax {
			break
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil && id > 0 {
			hidden[id] = true
		}
	}
	return context.WithValue(ctx, noticeHiddenKey{}, hidden)
}

// noticeBannersFor is the banners one render shows: the live notices
// less the ones this visitor dismissed. Nil when there are none.
func (app *Application) noticeBannersFor(ctx context.Context, loggedIn bool) *noticeBanners {
	live := app.liveNotices(ctx)
	if len(live) == 0 {
		return nil
	}
	hidden, _ := ctx.Value(noticeHiddenKey{}).(map[int64]bool)
	userID := int64(0)
	if loggedIn {
		userID = app.userID(ctx)
	}
	if userID != 0 {
		ids := make([]int64, len(live))
		for i, n := range live {
			ids[i] = n.ID
		}
		if dismissed, err := app.queries.ListGlobalNoticeDismissals(ctx, db.ListGlobalNoticeDismissalsParams{UserID: userID, NoticeIds: ids}); err != nil {
			logging.Errorf("notices: dismissals for user %d: %v", userID, err)
		} else if len(dismissed) > 0 {
			merged := make(map[int64]bool, len(hidden)+len(dismissed))
			for id := range hidden {
				merged[id] = true
			}
			for _, id := range dismissed {
				merged[id] = true
			}
			hidden = merged
		}
	}
	out := &noticeBanners{Account: userID != 0}
	for _, n := range live {
		if !hidden[n.ID] {
			out.Items = append(out.Items, n)
		}
	}
	if len(out.Items) == 0 {
		return nil
	}
	return out
}

// handleNoticeDismiss saves a signed-in visitor's dismissal (POST
// /notices/dismiss). The browser has already hidden the banner and
// set the cookie; this makes it stick on the account's other devices.
func (app *Application) handleNoticeDismiss(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	userID := app.userID(ctx)
	if err != nil || id <= 0 || userID == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := app.queries.InsertGlobalNoticeDismissal(ctx, db.InsertGlobalNoticeDismissalParams{UserID: userID, NoticeID: id, Now: time.Now().UTC()}); err != nil {
		logging.Errorf("notices: dismiss %d for user %d: %v", id, userID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// The Notices tab.
// ---------------------------------------------------------------------------

// noticesAdminView is the Notices tab body.
type noticesAdminView struct {
	Severities []noticeSeverity
	Form       noticeFormView
	Rows       []noticeAdminRow
	Audit      []noticeAuditRow
	LiveCount  int
	LiveMax    int
	TitleMax   int
	BodyMax    int
}

// noticeFormView is the publish or edit form.
type noticeFormView struct {
	ID         int64 // 0 publishes a new notice
	Title      string
	Severity   string
	Body       string
	Ends       string // datetime-local, UTC
	UrgentSent string // when app notifications went out; empty when never
}

type noticeAdminRow struct {
	ID       int64
	Title    string
	Severity string
	Label    string
	State    string // Live, Scheduled, Ended, Retired
	Live     bool
	Ends     string
	Urgent   string
	Created  string
	By       int64
	HasPost  bool
}

type noticeAuditRow struct {
	At        string
	Action    string
	NoticeID  int64
	Title     string
	Account   int64
	Character string
}

// noticeEndsLayout is the datetime-local value the form uses.
const noticeEndsLayout = "2006-01-02T15:04"

// noticeAdminScripts is the Notices tab's own script.
var noticeAdminScripts = []string{"/static/notices_admin.js"}

// handleAdminNotices renders the Notices tab (GET /admin/notices/):
// the form, the notices posted lately and the audit trail.
// ?edit= opens one for editing.
func (app *Application) handleAdminNotices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	data.AdminTab = "notices"
	data.Scripts = noticeAdminScripts
	now := time.Now().UTC()
	view := &noticesAdminView{
		Severities: noticeSeverities,
		LiveMax:    noticeLiveMax,
		TitleMax:   noticeTitleMax,
		BodyMax:    noticeBodyMax,
		Form:       noticeFormView{Severity: "info", Ends: now.Add(72 * time.Hour).Format(noticeEndsLayout)},
	}
	data.NoticeAdmin = view

	if id, err := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); err == nil && id > 0 {
		row, err := app.queries.GetGlobalNotice(ctx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			data.Error = "No notice has that id."
		case err != nil:
			logging.Errorf("notices: get %d: %v", id, err)
			data.Error = "Could not load that notice; check the server log."
		case row.RetiredAt.Valid:
			data.Error = "That notice is retired and can no longer be edited."
		default:
			view.Form = noticeFormView{ID: row.ID, Title: row.Title, Severity: row.Severity, Body: row.BodyMd, Ends: row.EndsAt.UTC().Format(noticeEndsLayout)}
			if row.UrgentSentAt.Valid {
				view.Form.UrgentSent = row.UrgentSentAt.Time.UTC().Format("Jan 2, 15:04") + " UTC"
			}
		}
	}

	if rows, err := app.queries.ListRecentGlobalNotices(ctx, noticeListLimit); err != nil {
		logging.Errorf("notices: list recent: %v", err)
		data.Error = "Could not load notices; check the server log."
	} else {
		for _, row := range rows {
			label, _ := noticeSeverityLabel(row.Severity)
			ar := noticeAdminRow{
				ID: row.ID, Title: row.Title, Severity: row.Severity, Label: label,
				Ends: row.EndsAt.UTC().Format("Jan 2, 15:04") + " UTC", Created: row.CreatedAt.UTC().Format("Jan 2, 15:04") + " UTC",
				By: row.CreatedBy, HasPost: strings.TrimSpace(row.BodyMd) != "",
			}
			switch {
			case row.RetiredAt.Valid:
				ar.State = "Retired"
			case !row.EndsAt.After(now):
				ar.State = "Ended"
			case row.StartsAt.After(now):
				ar.State = "Scheduled"
			default:
				ar.State, ar.Live = "Live", true
				view.LiveCount++
			}
			if row.UrgentSentAt.Valid {
				ar.Urgent = "Sent " + row.UrgentSentAt.Time.UTC().Format("Jan 2, 15:04") + " UTC"
			}
			view.Rows = append(view.Rows, ar)
		}
	}
	if rows, err := app.queries.ListGlobalNoticeAudit(ctx, noticeAuditLimit); err != nil {
		logging.Errorf("notices: list audit: %v", err)
	} else {
		for _, row := range rows {
			view.Audit = append(view.Audit, noticeAuditRow{
				At: row.At.UTC().Format("Jan 2, 15:04") + " UTC", Action: noticeAuditWords[row.Action],
				NoticeID: row.NoticeID, Title: row.Title, Account: row.UserID, Character: row.CharacterName,
			})
		}
	}
	app.render(ctx, w, http.StatusOK, "admin_notices.html", data)
}

var noticeAuditWords = map[string]string{
	"publish": "Published",
	"edit":    "Edited",
	"retire":  "Retired",
	"urgent":  "Sent app notifications",
}

// noticeInput is a validated form.
type noticeInput struct {
	Title, Severity, Body string
	BodyHTML              template.HTML
	EndsAt                time.Time
	Urgent                bool
}

// parseNoticeForm reads and checks the publish/edit form. The message
// is what to tell the admin when it does not pass.
func parseNoticeForm(r *http.Request, now time.Time) (noticeInput, string) {
	in := noticeInput{
		Title:    strings.TrimSpace(r.PostForm.Get("title")),
		Severity: r.PostForm.Get("severity"),
		Body:     strings.TrimSpace(strings.ReplaceAll(r.PostForm.Get("body"), "\r\n", "\n")),
		Urgent:   r.PostForm.Get("urgent") == "on",
	}
	switch {
	case in.Title == "":
		return in, "A notice needs a title."
	case utf8.RuneCountInString(in.Title) > noticeTitleMax:
		return in, fmt.Sprintf("Keep the title to %d characters.", noticeTitleMax)
	case strings.ContainsAny(in.Title, "\n\r"):
		return in, "The title is one line."
	case utf8.RuneCountInString(in.Body) > noticeBodyMax:
		return in, fmt.Sprintf("Keep the post to %d characters.", noticeBodyMax)
	}
	if _, ok := noticeSeverityLabel(in.Severity); !ok {
		return in, "Pick a severity from the list."
	}
	ends, err := time.ParseInLocation(noticeEndsLayout, r.PostForm.Get("ends"), time.UTC)
	switch {
	case err != nil:
		return in, "The end time could not be read."
	case ends.Before(now.Add(noticeMinDuration)):
		return in, "The end time has to be at least 10 minutes from now."
	case ends.After(now.Add(noticeMaxDuration)):
		return in, "A notice can run for 30 days at most."
	}
	in.EndsAt = ends
	if in.Urgent && !noticeUrgentAllowed(in.Severity) {
		return in, "App notifications are for warnings and critical notices only."
	}
	if in.Body != "" {
		in.BodyHTML = htmlsanitize.Markdown(in.Body)
	}
	return in, ""
}

// noticeActor is who an audit row names.
func (app *Application) noticeActor(ctx context.Context) (userID, characterID int64, name string) {
	return app.userID(ctx), sessionCharID(app.sessions, ctx), app.sessions.GetString(ctx, sessionCharacterName)
}

// noticeRateLimited reports why an account may not take action now,
// or "" when it may. The caller holds notices.write.
func (app *Application) noticeRateLimited(ctx context.Context, action string, userID int64, now time.Time) (string, error) {
	since := now.Add(-noticeRateWindow)
	count := func(user int64) (int64, error) {
		return app.queries.CountGlobalNoticeAuditSince(ctx, db.CountGlobalNoticeAuditSinceParams{Action: action, Since: since, UserID: user})
	}
	switch action {
	case "publish":
		mine, err := count(userID)
		if err != nil {
			return "", err
		}
		if mine >= noticePublishPerAccount {
			return fmt.Sprintf("You have published %d notices in the last hour, the most allowed. Try again later.", mine), nil
		}
		all, err := count(0)
		if err != nil {
			return "", err
		}
		if all >= noticePublishGlobal {
			return "Administrators have published as many notices as allowed in the last hour. Try again later.", nil
		}
	case "edit":
		mine, err := count(userID)
		if err != nil {
			return "", err
		}
		if mine >= noticeEditPerAccount {
			return "You have made as many edits as allowed in the last hour. Try again later.", nil
		}
	}
	return "", nil
}

// handleAdminNoticeSave publishes a notice, or saves an edit to one
// (POST /admin/notices/save).
func (app *Application) handleAdminNoticeSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, "/admin/notices/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	now := time.Now().UTC()
	in, problem := parseNoticeForm(r, now)
	id, _ := strconv.ParseInt(r.PostForm.Get("id"), 10, 64)
	if problem != "" {
		if id > 0 {
			back = app.flashBack(w, r, fmt.Sprintf("/admin/notices/?edit=%d", id))
		}
		back("Nothing was saved: " + problem)
		return
	}
	userID, charID, charName := app.noticeActor(ctx)
	action := "publish"
	if id > 0 {
		action = "edit"
	}

	app.notices.write.Lock()
	defer app.notices.write.Unlock()
	if why, err := app.noticeRateLimited(ctx, action, userID, now); err != nil {
		logging.Errorf("notices: rate check: %v", err)
		back("Nothing was saved: the limits could not be checked.")
		return
	} else if why != "" {
		logging.Warnf("notices: account %d refused (%s): %s", userID, action, why)
		back("Nothing was saved. " + why)
		return
	}
	if action == "publish" {
		live, err := app.queries.CountLiveGlobalNotices(ctx, now)
		if err != nil {
			logging.Errorf("notices: count live: %v", err)
			back("Nothing was saved: the live notices could not be counted.")
			return
		}
		if live >= noticeLiveMax {
			back(fmt.Sprintf("Nothing was saved: %d notices are live, the most at once. Retire one first.", live))
			return
		}
	}
	sendUrgent := false
	if in.Urgent {
		if action == "edit" {
			// An edit never sends; the toggle belongs to publishing.
			in.Urgent = false
		} else {
			sent, err := app.queries.CountGlobalNoticeUrgentSince(ctx, timeSet(now.Add(-noticeUrgentWindow)))
			if err != nil {
				logging.Errorf("notices: count urgent: %v", err)
				back("Nothing was saved: the app notification limit could not be checked.")
				return
			}
			if sent > 0 {
				back("Nothing was saved: app notifications already went out in the last 24 hours. Turn that off to publish this as a banner only.")
				return
			}
			sendUrgent = true
		}
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("notices: begin: %v", err)
		back("Nothing was saved; check the server log.")
		return
	}
	defer tx.Rollback()
	q := app.queries.WithTx(tx)
	if action == "publish" {
		id, err = q.InsertGlobalNotice(ctx, db.InsertGlobalNoticeParams{
			Title: in.Title, Severity: in.Severity, BodyMd: in.Body, BodyHtml: string(in.BodyHTML),
			Urgent: in.Urgent, Now: now, EndsAt: in.EndsAt, CreatedBy: userID,
		})
	} else {
		var n int64
		n, err = q.UpdateGlobalNotice(ctx, db.UpdateGlobalNoticeParams{
			ID: id, Title: in.Title, Severity: in.Severity, BodyMd: in.Body, BodyHtml: string(in.BodyHTML), EndsAt: in.EndsAt, Now: now,
		})
		if err == nil && n == 0 {
			back("Nothing was saved: that notice is retired or gone.")
			return
		}
	}
	audit := db.InsertGlobalNoticeAuditParams{
		NoticeID: id, Action: action, UserID: userID, CharacterID: charID, CharacterName: charName,
		Title: in.Title, Severity: in.Severity, BodyMd: in.Body, EndsAt: in.EndsAt, At: now,
	}
	if err == nil {
		err = q.InsertGlobalNoticeAudit(ctx, audit)
	}
	if err == nil && sendUrgent {
		audit.Action = "urgent"
		if err = q.MarkGlobalNoticeUrgentSent(ctx, db.MarkGlobalNoticeUrgentSentParams{ID: id, Now: timeSet(now)}); err == nil {
			err = q.InsertGlobalNoticeAudit(ctx, audit)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		logging.Errorf("notices: save (%s): %v", action, err)
		back("Nothing was saved; check the server log.")
		return
	}
	app.dropNoticeCache()
	logging.Warnf("notices: account %d (%s) %s notice %d: %q", userID, charName, action, id, in.Title)
	if sendUrgent {
		title := in.Title
		app.Background(func() { app.sendNoticeNotifications(id, title) })
		back("Published. App notifications are going out now.")
		return
	}
	if action == "edit" {
		back("Saved.")
		return
	}
	back("Published.")
}

// handleAdminNoticeRetire takes a notice down (POST
// /admin/notices/retire). Never rate-limited.
func (app *Application) handleAdminNoticeRetire(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, "/admin/notices/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id, err := strconv.ParseInt(r.PostForm.Get("id"), 10, 64)
	if err != nil || id <= 0 {
		back("Nothing changed: no notice was named.")
		return
	}
	now := time.Now().UTC()
	userID, charID, charName := app.noticeActor(ctx)
	app.notices.write.Lock()
	defer app.notices.write.Unlock()
	row, err := app.queries.GetGlobalNotice(ctx, id)
	if err != nil {
		back("Nothing changed: no notice has that id.")
		return
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("notices: begin: %v", err)
		back("Nothing changed; check the server log.")
		return
	}
	defer tx.Rollback()
	q := app.queries.WithTx(tx)
	n, err := q.RetireGlobalNotice(ctx, db.RetireGlobalNoticeParams{ID: id, Now: timeSet(now)})
	if err == nil && n == 0 {
		back("Nothing changed: that notice was already retired.")
		return
	}
	if err == nil {
		err = q.InsertGlobalNoticeAudit(ctx, db.InsertGlobalNoticeAuditParams{
			NoticeID: id, Action: "retire", UserID: userID, CharacterID: charID, CharacterName: charName,
			Title: row.Title, Severity: row.Severity, BodyMd: row.BodyMd, EndsAt: row.EndsAt, At: now,
		})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		logging.Errorf("notices: retire %d: %v", id, err)
		back("Nothing changed; check the server log.")
		return
	}
	app.dropNoticeCache()
	logging.Warnf("notices: account %d (%s) retired notice %d", userID, charName, id)
	back("Retired. The banner is gone from every page.")
}

// handleAdminNoticePreview renders a draft post with the same renderer
// and sanitizer the save uses (POST /admin/notices/preview), as JSON:
// {"html": "..."}.
func (app *Application) handleAdminNoticePreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*noticeBodyMax+1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	body := r.PostForm.Get("body")
	resp := struct {
		HTML    string `json:"html"`
		TooLong bool   `json:"too_long,omitempty"`
	}{}
	if utf8.RuneCountInString(body) > noticeBodyMax {
		resp.TooLong = true
	} else {
		resp.HTML = string(htmlsanitize.Markdown(body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// sendNoticeNotifications delivers one urgent notice as an app
// notification to every account: the bell, and browser push where
// the account has it on. Each account's own switches decide; Discord
// and the other module channels are never used.
func (app *Application) sendNoticeNotifications(noticeID int64, title string) {
	ctx := app.workerCtx
	if ctx == nil {
		ctx = context.Background()
	}
	users, err := app.queries.ListGlobalNoticeRecipients(ctx)
	if err != nil {
		logging.Errorf("notices: recipients for %d: %v", noticeID, err)
		return
	}
	ev := notifyEvent{Kind: notifyNotice, Key: fmt.Sprintf("%s|%d", notifyNotice, noticeID), Title: clip(title, notifyTitleMax), URL: "/"}
	sent := 0
	for _, userID := range users {
		if ctx.Err() != nil {
			return
		}
		if app.isDemoUser(ctx, userID) {
			continue
		}
		prefs := app.notifyPrefsFor(ctx, userID)
		if prefs.off(notifyNotice) {
			continue
		}
		if !prefs.channelOff(notifyChannelInApp) {
			if _, err := app.queries.InsertNotification(ctx, db.InsertNotificationParams{
				UserID: userID, Kind: ev.Kind, Title: ev.Title, Url: ev.URL, CreatedAt: time.Now().UTC(),
			}); err != nil {
				logging.Errorf("notices: notify user %d: %v", userID, err)
				continue
			}
		}
		if !prefs.channelOff(notifyChannelDesktop) {
			app.pushToUser(ctx, userID, pushMessagesFor([]notifyEvent{ev}))
		}
		sent++
	}
	logging.Infof("notices: notice %d sent to %d accounts", noticeID, sent)
}
