package app

import (
	"context"
	"fmt"
	"net/http"
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
	notifyWatch:    {"watch list alert", "watch list alerts"},
	notifySkill:    {"skill finished", "skills finished"},
	notifyMail:     {"new mail", "new mails"},
	notifyCalendar: {"new calendar event", "new calendar events"},
	notifyKillmail: {"new killmail", "new killmails"},
	notifyPI:       {"planet with stopped extractors", "planets with stopped extractors"},
	notifyIndustry: {"industry job finished", "industry jobs finished"},
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
	for _, kind := range notifyKinds {
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
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
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
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
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
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
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

// notifyTarget is where a notification leads. Stored addresses are
// the app's own paths; anything else falls back to the list, so a
// stored value can never send the visitor to another site.
func notifyTarget(url string) string {
	if strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//") && !strings.ContainsAny(url, "\\\r\n") {
		return url
	}
	return "/notifications/"
}
