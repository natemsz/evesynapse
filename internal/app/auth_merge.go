package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Merging accounts: a player who ended up with characters on two
// accounts moves everything from one (the source) into the other
// (the kept account). Characters and everything saved against the
// source account move; where both accounts hold the same thing (a
// watchlist row, a widget's settings, the theme), the kept account's
// copy wins. The source is left empty and removed after
// mergeDeleteAfter by the worker.

// mergeDeleteAfter is how long an emptied account waits before it
// is removed.
const mergeDeleteAfter = 30 * 24 * time.Hour

// accountMergeRule says what a merge does with one account column.
// Key nil moves every row. Key set moves the rows the kept account
// has no row for (matched on Key); the rest stay with the source and
// go when it is removed. Stay leaves the rows on the source.
type accountMergeRule struct {
	Table, Column string
	Key           []string
	Stay          bool
}

// accountMergeRules covers every column that holds an account id
// (TestAccountMergeRulesCoverEveryAccountColumn keeps it complete).
var accountMergeRules = []accountMergeRule{
	{Table: "characters", Column: "user_id"},
	{Table: "api_keys", Column: "user_id"},
	{Table: "appraisal_anonymous_owner", Column: "user_id"},
	{Table: "battle_fights", Column: "user_id"},
	{Table: "blueprint_buildable", Column: "user_id"},
	{Table: "fitting_anonymous_owner", Column: "user_id"},
	{Table: "local_fittings", Column: "user_id"},
	{Table: "market_baskets", Column: "user_id"},
	{Table: "notifications", Column: "user_id"},
	{Table: "op_signups", Column: "user_id"},
	{Table: "push_subscriptions", Column: "user_id"},
	{Table: "skill_plans", Column: "user_id"},
	{Table: "srp_requests", Column: "user_id"},
	{Table: "user_appraisals", Column: "user_id"},
	{Table: "intel_channels", Column: "owner_user_id"},
	{Table: "intel_reports", Column: "author_user_id"},
	{Table: "intel_pairing_codes", Column: "user_id"},
	{Table: "intel_submit_labels", Column: "user_id"},
	{Table: "intel_tokens", Column: "user_id"},
	{Table: "pi_plans", Column: "user_id"},
	{Table: "scans", Column: "user_id"},

	{Table: "account_link_choices", Column: "user_id", Key: []string{}},
	{Table: "blueprint_buildable_folds", Column: "user_id", Key: []string{}},
	{Table: "discord_links", Column: "user_id", Key: []string{}},
	{Table: "user_appraisal_prefs", Column: "user_id", Key: []string{}},
	{Table: "user_profit_prefs", Column: "user_id", Key: []string{}},
	{Table: "asset_location_values", Column: "user_id", Key: []string{"character_id", "location_id"}},
	{Table: "blueprint_pins", Column: "user_id", Key: []string{"blueprint_type_id"}},
	{Table: "briefing_dismissals", Column: "user_id", Key: []string{"line_key"}},
	{Table: "clone_names", Column: "user_id", Key: []string{"character_id", "clone_id"}},
	{Table: "fit_ratings", Column: "user_id", Key: []string{"fit_id"}},
	{Table: "foresight_targets", Column: "user_id", Key: []string{"event_key", "target_kind", "subject_id", "region_id", "detail"}},
	{Table: "global_notice_dismissals", Column: "user_id", Key: []string{"notice_id"}},
	{Table: "industry_facility_bonuses", Column: "user_id", Key: []string{"facility_id"}},
	{Table: "market_watchlist", Column: "user_id", Key: []string{"type_id", "region_id"}},
	{Table: "market_watchlist_baskets", Column: "user_id", Key: []string{"basket_id"}},
	{Table: "notification_seen", Column: "user_id", Key: []string{"event_key"}},
	{Table: "restock_targets", Column: "user_id", Key: []string{"type_id"}},
	{Table: "trained_banner_dismissals", Column: "user_id", Key: []string{"character_id", "entry_key"}},
	{Table: "wallet_history", Column: "user_id", Key: []string{"character_id", "day"}},
	{Table: "widget_configs", Column: "user_id", Key: []string{"widget_id"}},

	// Passkeys are registered to their account and stay with it; the
	// kept account keeps its own second factor. Audit rows stay put.
	{Table: "webauthn_credentials", Column: "user_id", Stay: true},
	{Table: "webauthn_recovery_codes", Column: "user_id", Stay: true},
	{Table: "webauthn_events", Column: "user_id", Stay: true},
	{Table: "global_notice_audit", Column: "user_id", Stay: true},
}

// mergeResult is what a merge moved.
type mergeResult struct {
	Characters  int64
	DeleteAfter time.Time
}

// mergeAccounts moves everything from the source account into the
// kept one in one transaction and marks the source for removal.
func (app *Application) mergeAccounts(ctx context.Context, from, into int64) (mergeResult, error) {
	var res mergeResult
	if from == 0 || into == 0 || from == into {
		return res, fmt.Errorf("merge: accounts %d and %d", from, into)
	}
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a committed tx makes this a no-op
	for _, rule := range accountMergeRules {
		if rule.Stay {
			continue
		}
		tag, err := tx.Exec(ctx, rule.moveSQL(), into, from)
		if err != nil {
			return res, fmt.Errorf("merge %s: %w", rule.Table, err)
		}
		if rule.Table == "characters" {
			res.Characters = tag.RowsAffected()
		}
	}
	// The account's own settings: the kept account's win, the
	// source's fill what the kept account never set.
	if _, err := tx.Exec(ctx, `UPDATE users d SET
	home_layout = CASE WHEN d.home_layout = '' THEN s.home_layout ELSE d.home_layout END,
	viz_prefs = CASE WHEN d.viz_prefs = '' THEN s.viz_prefs ELSE d.viz_prefs END
FROM users s WHERE d.id = $1 AND s.id = $2`, into, from); err != nil {
		return res, fmt.Errorf("merge account settings: %w", err)
	}
	res.DeleteAfter = time.Now().UTC().Add(mergeDeleteAfter)
	if err := app.queries.WithTx(tx).MarkUserForDeletion(ctx, db.MarkUserForDeletionParams{ID: from, After: res.DeleteAfter}); err != nil {
		return res, fmt.Errorf("merge mark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	logging.Infof("merge: moved account %d into %d (%d characters); %d is removed after %s", from, into, res.Characters, from, res.DeleteAfter.Format(time.DateOnly))
	return res, nil
}

// moveSQL is the statement that moves one table's rows ($2 to $1).
func (r accountMergeRule) moveSQL() string {
	if r.Key == nil {
		return fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE %s = $2`, r.Table, r.Column, r.Column)
	}
	match := []string{fmt.Sprintf("d.%s = $1", r.Column)}
	for _, k := range r.Key {
		match = append(match, fmt.Sprintf("d.%s IS NOT DISTINCT FROM s.%s", k, k))
	}
	return fmt.Sprintf(`UPDATE %s s SET %s = $1 WHERE s.%s = $2 AND NOT EXISTS (SELECT 1 FROM %s d WHERE %s)`,
		r.Table, r.Column, r.Column, r.Table, strings.Join(match, " AND "))
}

// deleteDueAccounts removes the accounts emptied by a merge whose
// time has passed. The worker runs it every cycle (worker.go).
func (app *Application) deleteDueAccounts(ctx context.Context) {
	n, err := app.queries.DeleteDueUsers(ctx, time.Now().UTC())
	if err != nil {
		logging.Warnf("merge: delete emptied accounts: %v", err)
		return
	}
	if n > 0 {
		logging.Infof("merge: removed %d emptied accounts", n)
	}
}

// sessionMergeInto holds a merge waiting for the source account's
// second factor: the kept account, proven by this session before it
// switched to the source.
const sessionMergeInto = "merge_into"

type pendingMerge struct {
	Into int64     `json:"i"`
	At   time.Time `json:"t"`
}

func (app *Application) loadPendingMerge(ctx context.Context) (int64, bool) {
	raw := app.sessions.GetString(ctx, sessionMergeInto)
	if raw == "" {
		return 0, false
	}
	var p pendingMerge
	if json.Unmarshal([]byte(raw), &p) != nil || p.Into == 0 || time.Since(p.At) > pendingLinkLifetime {
		app.sessions.Remove(ctx, sessionMergeInto)
		return 0, false
	}
	return p.Into, true
}

// accountHasPasskey reports whether an account has a second factor
// enrolled. A failed read counts as yes: the merge then asks for it.
func (app *Application) accountHasPasskey(ctx context.Context, userID int64) bool {
	n, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("merge: count credentials for user %d: %v", userID, err)
		return true
	}
	return n > 0
}

// startMerge answers "move everything here" on the confirm page. The
// session is signed in to the kept account and has just proven a
// character of the source. A source with a passkey must pass it
// first: the session switches to the source, the factor runs, and
// /auth/merge finishes. Without one, the proven character is already
// a sign-in to the source, so the merge runs now.
func (app *Application) startMerge(w http.ResponseWriter, r *http.Request, plan signInPlan, in linkCharacterInput) {
	ctx := r.Context()
	from, into := plan.OnAccount, plan.UserID
	app.sessions.Remove(ctx, sessionPendingLink)
	if app.accountHasPasskey(ctx, from) {
		blob, _ := json.Marshal(pendingMerge{Into: into, At: time.Now().UTC()})
		app.sessions.Put(ctx, sessionMergeInto, string(blob))
		app.finishLink(w, r, from, in, "", "/auth/merge")
		return
	}
	res, err := app.mergeAccounts(ctx, from, into)
	if err != nil {
		logging.Errorf("merge: account %d into %d: %v", from, into, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	app.finishLink(w, r, into, in, mergeNotice(res), "/characters/")
}

func mergeNotice(res mergeResult) string {
	return fmt.Sprintf("Everything from your other account moved here, %s included. That account is now empty and will be deleted on %s.",
		pluralCount(res.Characters, "character", "characters"), res.DeleteAfter.Format("2 January 2006"))
}

// mergeView is the /auth/merge page, shown after the source
// account's second factor passed.
type mergeView struct {
	From *linkAccountView // this session's account, emptied by the merge
	Into *linkAccountView // the kept account
}

func (app *Application) accountView(ctx context.Context, userID int64) *linkAccountView {
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil || len(chars) == 0 {
		return &linkAccountView{}
	}
	entries := app.switcherEntriesFor(ctx, chars)
	v := &linkAccountView{Total: len(entries)}
	if len(entries) > linkAccountShown {
		v.More = len(entries) - linkAccountShown
		entries = entries[:linkAccountShown]
	}
	v.Chars = entries
	return v
}

// mergeTarget is the kept account of a merge this session may finish:
// one proven by this session, still there, and not the account the
// session is on.
func (app *Application) mergeTarget(ctx context.Context) (int64, bool) {
	into, ok := app.loadPendingMerge(ctx)
	from := app.userID(ctx)
	if !ok || from == 0 || into == from {
		return 0, false
	}
	if _, err := app.queries.GetUser(ctx, into); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("merge: read account %d: %v", into, err)
		}
		return 0, false
	}
	return into, true
}

func (app *Application) handleMerge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	into, ok := app.mergeTarget(ctx)
	if !ok {
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	data := app.page(ctx)
	data.Merge = &mergeView{From: app.accountView(ctx, app.userID(ctx)), Into: app.accountView(ctx, into)}
	app.render(ctx, w, http.StatusOK, "merge.html", data)
}

func (app *Application) handleMergePost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.PostFormValue("action") != "merge" {
		app.sessions.Remove(ctx, sessionMergeInto)
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	into, ok := app.mergeTarget(ctx)
	if !ok {
		app.flash(ctx, "That merge expired before it was confirmed. Link a character from the other account again to start over.")
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	res, err := app.mergeAccounts(ctx, app.userID(ctx), into)
	if err != nil {
		logging.Errorf("merge: account %d into %d: %v", app.userID(ctx), into, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	app.sessions.Remove(ctx, sessionMergeInto)
	// The kept account was proven by this session before the switch,
	// so the session returns to it. The acting character moved with
	// the merge and stays acting.
	if err := app.sessions.RenewToken(ctx); err != nil {
		logging.Errorf("merge: renew session token: %v", err)
	}
	app.sessions.Put(ctx, sessionUserID, int(into))
	app.flash(ctx, mergeNotice(res))
	http.Redirect(w, r, "/characters/", http.StatusSeeOther)
}
