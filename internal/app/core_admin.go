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
// It used to print every account, every character with its full scope
// list, and every stored dataset of every character: three tables that
// grew with the site until the page was too long to load or read.
// ---------------------------------------------------------------------------

// adminNewest is how many of the newest accounts are listed.
const adminNewest = 10

// adminView is the Admin page body.
type adminView struct {
	Accounts     string // counts, formatted
	SeenToday    string
	SeenThisWeek string
	Characters   string
	Parked       string
	AnyParked    bool
	Newest       []adminAccountRow
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
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
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
			account := &adminAccountView{ID: user.ID, Created: rfc3339(user.CreatedAt), LastSeen: rfc3339(user.LastSeenAt)}
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
