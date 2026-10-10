package app

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The Admin page is about accounts and the people behind them: how
// many there are, who is new, and one account looked up through the
// search box, with its characters, their sign-in state and what they
// granted. What the worker is doing and how fresh anyone's data is
// belongs to the Sync page, and each character here links there.
//
// ---------------------------------------------------------------------------

// adminNewest is how many of the newest accounts are listed.
const adminNewest = 10

// adminEmptyListed is how many accounts with no characters are listed.
const adminEmptyListed = 25

// adminView is the Admin page body.
type adminView struct {
	Accounts     string // counts, formatted
	SeenToday    string
	SeenThisWeek string
	Characters   string
	Parked       string
	AnyParked    bool
	Newest       []adminAccountRow
	Empty        []adminAccountRow // accounts with no character linked
	EmptyMore    bool
	Lookup       characterLookupView
	NoAccount    bool              // ?account= named one that does not exist
	Account      *adminAccountView // nil until one is found
}

// adminAccountRow is one line of the newest-accounts list.
type adminAccountRow struct {
	ID         int64
	Created    string
	LastSeen   string
	Characters int64
}

// adminAccountView is the one account the page is showing.
type adminAccountView struct {
	ID         int64
	Created    string
	LastSeen   string
	Own        bool // the reader's own account
	Admin      bool // holds an administrator character
	Characters []adminCharacterRow
}

// adminCharacterRow is one of that account's characters.
type adminCharacterRow struct {
	ID          int64
	Name        string
	Found       bool   // the one the search was for
	Tier        string // how often the worker refreshes it; empty when parked
	Parked      string // why it is not synced, when it is not
	TokenExpiry string
	Scopes      []string
}

// handleAdmin renders the Admin page: the totals, the newest accounts,
// and the account of the character asked for (?q= a name or an id).
func (app *Application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	view := &adminView{}
	data.Admin = view
	now := time.Now()

	if totals, err := app.queries.AdminTotals(ctx, db.AdminTotalsParams{DayAgo: now.Add(-24 * time.Hour), WeekAgo: now.Add(-7 * 24 * time.Hour)}); err != nil {
		logging.Errorf("admin: totals: %v", err)
		data.Error = "Could not load admin data; check the server log."
	} else {
		view.Accounts, view.SeenToday, view.SeenThisWeek = esi.FormatInt(totals.Accounts), esi.FormatInt(totals.SeenToday), esi.FormatInt(totals.SeenThisWeek)
		view.Characters, view.Parked, view.AnyParked = esi.FormatInt(totals.Characters), esi.FormatInt(totals.Parked), totals.Parked > 0
	}
	if rows, err := app.queries.ListNewestUsers(ctx, adminNewest); err != nil {
		logging.Errorf("admin: newest accounts: %v", err)
		data.Error = "Could not load admin data; check the server log."
	} else {
		for _, row := range rows {
			view.Newest = append(view.Newest, adminAccountRow{ID: row.ID, Created: rfc3339(row.CreatedAt), LastSeen: rfc3339(row.LastSeenAt), Characters: row.Characters})
		}
	}

	// The account to show: the one the character searched for belongs
	// to, or the one named outright (?account=, which is what the lists
	// on this page and the Sync page link with).
	if rows, err := app.queries.ListEmptyUsers(ctx, adminEmptyListed+1); err != nil {
		logging.Errorf("admin: empty accounts: %v", err)
	} else {
		if len(rows) > adminEmptyListed {
			rows, view.EmptyMore = rows[:adminEmptyListed], true
		}
		for _, row := range rows {
			view.Empty = append(view.Empty, adminAccountRow{ID: row.ID, Created: rfc3339(row.CreatedAt), LastSeen: rfc3339(row.LastSeenAt)})
		}
	}

	found, lookup := app.lookupCharacter(ctx, r.URL.Query().Get("q"))
	view.Lookup = lookup
	var accountID, foundID int64
	if found != nil {
		accountID, foundID = found.UserID, found.CharacterID
	} else if id, err := strconv.ParseInt(r.URL.Query().Get("account"), 10, 64); err == nil && id > 0 {
		accountID = id
	}
	if accountID != 0 {
		user, uerr := app.queries.GetUser(ctx, accountID)
		characters, cerr := app.queries.ListCharactersByUser(ctx, accountID)
		switch {
		case errors.Is(uerr, sql.ErrNoRows):
			view.NoAccount = true
		case uerr != nil || cerr != nil:
			logging.Errorf("admin: account %d: %v %v", accountID, uerr, cerr)
			data.Error = "Could not load that account; check the server log."
		default:
			account := &adminAccountView{ID: user.ID, Created: rfc3339(user.CreatedAt), LastSeen: rfc3339(user.LastSeenAt), Own: user.ID == app.userID(ctx), Admin: app.adminAmong(characters)}
			for _, ch := range characters {
				row := adminCharacterRow{
					ID: ch.CharacterID, Name: ch.Name, Found: ch.CharacterID == foundID,
					TokenExpiry: rfc3339Or(ch.TokenExpiry, "—"), Scopes: strings.Fields(ch.Scopes),
				}
				if characterSyncs(ch) {
					row.Tier = app.tierOf(ch, now).String()
				} else {
					row.Parked = linkStateWords(ch.LinkState)
				}
				account.Characters = append(account.Characters, row)
			}
			view.Account = account
		}
	}
	app.render(ctx, w, http.StatusOK, "admin.html", data)
}

// handleAdminAccountRemove deletes one account and everything it owns
// (POST /admin/accounts/remove). An administrator's request only, and
// only for an account named twice: in the form, and typed out by hand.
// Never the administrator's own account, and never one that holds an
// administrator character.
func (app *Application) handleAdminAccountRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id, err := strconv.ParseInt(r.Form.Get("account"), 10, 64)
	if err != nil || id <= 0 || strings.TrimSpace(r.Form.Get("confirm")) != strconv.FormatInt(id, 10) {
		back("Nothing was removed: type the account's number to confirm.")
		return
	}
	by := app.userID(ctx)
	if id == by {
		back("Nothing was removed: that is your own account.")
		return
	}
	characters, err := app.queries.ListCharactersByUser(ctx, id)
	if err != nil {
		logging.Errorf("admin: remove account %d: %v", id, err)
		back("The account could not be read; nothing was removed.")
		return
	}
	if app.adminAmong(characters) {
		back("Nothing was removed: that account holds an administrator character. Take its id out of EVE_ADMIN_CHARACTER_IDS first.")
		return
	}
	n, err := app.queries.DeleteUser(ctx, id)
	switch {
	case err != nil:
		logging.Errorf("admin: remove account %d: %v", id, err)
		back("The account could not be removed; check the server log.")
	case n == 0:
		back("There is no account with that number.")
	default:
		logging.Warnf("admin: account %d removed account %d and its %s", by, id, plural(len(characters), "character"))
		back("Account " + strconv.FormatInt(id, 10) + " was removed, with its " + plural(len(characters), "character") + " and everything stored for it.")
	}
}
