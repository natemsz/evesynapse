package srp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Changing an owner's replacement policies. A policy decides what gets
// paid, so it stays with the people who run the owner by position; being
// handed the handling of requests does not include it.
// ---------------------------------------------------------------------------

func (app *mod) srpKeeps(r *http.Request, owner standing.Owner) bool {
	ctx := r.Context()
	st, err := app.deps.StandingFor(ctx, app.deps.UserID(ctx))
	return err == nil && app.deps.HoldsProgramme(ctx, st, owner, permSRP)
}

// srpPolicyRows lists an owner's policies for the page.
func srpPolicyRows(owner standing.Owner, policies []db.SrpPolicy) []srpPolicyRow {
	rows := make([]srpPolicyRow, 0, len(policies))
	for _, p := range policies {
		row := srpPolicyRowOf(p)
		row.EditHref = standing.Address(srpPath, owner.Key()) + "&policy=" + strconv.FormatInt(p.ID, 10) + "#srp-policy"
		rows = append(rows, row)
	}
	return rows
}

func srpPolicyRowOf(p db.SrpPolicy) srpPolicyRow {
	row := srpPolicyRow{
		ID: p.ID, Name: p.Name, Mode: p.Mode, Percent: p.Percent, ClaimDays: max(p.ClaimWindowHours/24, 1),
		Doctrine: p.RequiresDoctrine, Attendance: p.RequiresAttendance, Ledger: p.LedgerVisibility,
		Text: p.PolicyText, Default: p.IsDefault, Summary: srpPolicySummary(p),
	}
	if p.MaxPayout > 0 {
		row.MaxPayout = esi.FormatInt(int64(p.MaxPayout + 0.5))
	}
	return row
}

// srpPolicyForm is the form's starting point: the policy the address
// names, or a blank one.
func (app *mod) srpPolicyForm(ctx context.Context, r *http.Request, policies []db.SrpPolicy) *srpPolicyRow {
	blank := &srpPolicyRow{Mode: srpModeManual, Percent: 100, ClaimDays: 30, Ledger: srpLedgerHandlers, Default: len(policies) == 0}
	id, err := strconv.ParseInt(r.URL.Query().Get("policy"), 10, 64)
	if err != nil {
		return blank
	}
	for _, p := range policies {
		if p.ID != id {
			continue
		}
		row := srpPolicyRowOf(p)
		hulls, err := app.queries.ListSRPPolicyHulls(ctx, p.ID)
		if err != nil {
			logging.Errorf("srp: hulls of policy %d: %v", p.ID, err)
		}
		lines := make([]string, 0, len(hulls))
		for _, h := range hulls {
			lines = append(lines, h.Name+": "+esi.FormatInt(int64(h.Payout+0.5)))
		}
		row.Hulls = strings.Join(lines, "\n")
		return &row
	}
	return blank
}

// srpHullPayout is one line of a policy's hull list.
type srpHullPayout struct {
	typeID int64
	payout float64
}

// parseSRPHulls reads the hull list: one "Ship: amount" per line. It
// answers with what to tell the person when a line cannot be read.
func (app *mod) parseSRPHulls(ctx context.Context, raw string) ([]srpHullPayout, string) {
	type line struct {
		name   string
		payout float64
	}
	var lines []line
	var names []string
	for _, text := range strings.Split(raw, "\n") {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		at := strings.LastIndexAny(text, ":\t ")
		if at <= 0 {
			return nil, "Write each hull as Ship: amount. Could not read: " + humanize.Clip(text, 60)
		}
		name := strings.ToLower(strings.TrimSpace(strings.TrimRight(text[:at], ": \t")))
		payout, ok := parseISK(text[at+1:])
		if name == "" || !ok || payout <= 0 {
			return nil, "Write each hull as Ship: amount. Could not read: " + humanize.Clip(text, 60)
		}
		lines = append(lines, line{name: name, payout: payout})
		names = append(names, name)
	}
	if len(lines) > srpHullsMax {
		return nil, "A policy can list at most " + strconv.Itoa(srpHullsMax) + " hulls."
	}
	if len(lines) == 0 {
		return nil, "List at least one hull and what it pays."
	}
	rows, err := app.queries.ListShipTypesByLowerNames(ctx, names)
	if err != nil {
		logging.Errorf("srp: hull names: %v", err)
		return nil, "The hull list could not be read. Try again in a moment."
	}
	ids := make(map[string]int64, len(rows))
	for _, row := range rows {
		ids[strings.ToLower(row.Name)] = row.TypeID
	}
	out := make([]srpHullPayout, 0, len(lines))
	for _, l := range lines {
		id, known := ids[l.name]
		if !known {
			return nil, "No ship is called " + humanize.Clip(l.name, 60) + "."
		}
		out = append(out, srpHullPayout{typeID: id, payout: l.payout})
	}
	return out, ""
}

// handleSRPPolicySave creates or changes a policy (POST /srp/policies).
func (app *mod) handleSRPPolicySave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	owner, ok := srpFormOwner(r)
	if !ok || !app.srpKeeps(r, owner) {
		app.deps.FlashBack(w, r, srpPath)("Only the people who run it can change that.")
		return
	}
	back := app.deps.FlashBack(w, r, standing.Address(srpPath, owner.Key())+"#srp-policy")
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" || len([]rune(name)) > srpPolicyNameMax {
		back("Give the policy a name of up to 60 characters.")
		return
	}
	mode := r.Form.Get("mode")
	if mode != srpModeManual && mode != srpModePercent && mode != srpModeFixed {
		back("Choose how the policy pays.")
		return
	}
	percent := 100
	if mode == srpModePercent {
		n, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("percent")))
		if err != nil || n < 1 || n > 100 {
			back("Enter a percentage from 1 to 100.")
			return
		}
		percent = n
	}
	maxPayout := 0.0
	if raw := strings.TrimSpace(r.Form.Get("max_payout")); raw != "" {
		v, valid := parseISK(raw)
		if !valid {
			back("Enter the most it pays in ISK, or leave it empty.")
			return
		}
		maxPayout = v
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("claim_days")))
	if err != nil || days < 1 || days > 30 {
		back("Enter how many days pilots have to claim, from 1 to 30.")
		return
	}
	ledger := r.Form.Get("ledger")
	if ledger != srpLedgerHandlers && ledger != srpLedgerMembers {
		back("Choose who can read the ledger.")
		return
	}
	var hulls []srpHullPayout
	if mode == srpModeFixed {
		var problem string
		if hulls, problem = app.parseSRPHulls(ctx, r.Form.Get("hulls")); problem != "" {
			back(problem)
			return
		}
	}
	id, _ := strconv.ParseInt(r.Form.Get("id"), 10, 64)
	existing, err := app.queries.ListSRPPolicies(ctx, db.ListSRPPoliciesParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("srp: policies of %s: %v", owner.Key(), err)
		back("The policy could not be saved. Try again in a moment.")
		return
	}
	if id == 0 && len(existing) >= srpPoliciesMax {
		back("An owner can keep at most " + strconv.Itoa(srpPoliciesMax) + " policies.")
		return
	}
	isDefault := r.Form.Get("default") == "1" || len(existing) == 0
	by := app.deps.OwnerActorFor(ctx, app.deps.UserID(ctx), owner)
	now := time.Now().UTC()
	text := humanize.Clip(strings.TrimSpace(r.Form.Get("policy_text")), srpPolicyMax)
	doctrine, attendance := r.Form.Get("requires_doctrine") == "1", r.Form.Get("requires_attendance") == "1"

	tx, err := app.deps.PGPool().Begin(ctx)
	if err != nil {
		logging.Errorf("srp: save policy of %s: %v", owner.Key(), err)
		back("The policy could not be saved. Try again in a moment.")
		return
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if isDefault {
		if err := q.ClearDefaultSRPPolicy(ctx, db.ClearDefaultSRPPolicyParams{OwnerKind: owner.Kind, OwnerID: owner.ID}); err != nil {
			logging.Errorf("srp: save policy of %s: %v", owner.Key(), err)
			back("The policy could not be saved. Try again in a moment.")
			return
		}
	}
	if id == 0 {
		id, err = q.CreateSRPPolicy(ctx, db.CreateSRPPolicyParams{
			OwnerKind: owner.Kind, OwnerID: owner.ID, Name: name, Mode: mode, Percent: int32(percent), MaxPayout: maxPayout,
			RequiresDoctrine: doctrine, RequiresAttendance: attendance, ClaimWindowHours: int32(days * 24), LedgerVisibility: ledger,
			PolicyText: text, IsDefault: isDefault, UpdatedBy: by, UpdatedAt: now,
		})
	} else {
		var n int64
		n, err = q.UpdateSRPPolicy(ctx, db.UpdateSRPPolicyParams{
			ID: id, OwnerKind: owner.Kind, OwnerID: owner.ID, Name: name, Mode: mode, Percent: int32(percent), MaxPayout: maxPayout,
			RequiresDoctrine: doctrine, RequiresAttendance: attendance, ClaimWindowHours: int32(days * 24), LedgerVisibility: ledger,
			PolicyText: text, IsDefault: isDefault, UpdatedBy: by, UpdatedAt: now,
		})
		if err == nil && n == 0 {
			back("That policy is gone.")
			return
		}
	}
	if err == nil {
		err = q.DeleteSRPPolicyHulls(ctx, id)
	}
	for _, h := range hulls {
		if err != nil {
			break
		}
		err = q.InsertSRPPolicyHull(ctx, db.InsertSRPPolicyHullParams{PolicyID: id, ShipTypeID: h.typeID, Payout: h.payout})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		back("There is already a policy with that name.")
		return
	}
	if err != nil {
		logging.Errorf("srp: save policy of %s: %v", owner.Key(), err)
		back("The policy could not be saved. Try again in a moment.")
		return
	}
	logging.Infof("srp: user %d saved policy %d of %s", app.deps.UserID(ctx), id, owner.Key())
	back("Saved " + name + ". Claims already made keep the terms they were filed under.")
}

// handleSRPPolicyDelete removes a policy (POST /srp/policies/{id}/delete).
// Requests filed under it keep their amounts, and fleets that named it
// fall back to the owner's default.
func (app *mod) handleSRPPolicyDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	owner, ok := srpFormOwner(r)
	if !ok || !app.srpKeeps(r, owner) {
		app.deps.FlashBack(w, r, srpPath)("Only the people who run it can change that.")
		return
	}
	back := app.deps.FlashBack(w, r, standing.Address(srpPath, owner.Key())+"#srp-policy")
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	n, err := app.queries.DeleteSRPPolicy(ctx, db.DeleteSRPPolicyParams{ID: id, OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("srp: delete policy %d: %v", id, err)
		back("The policy could not be deleted. Try again in a moment.")
		return
	}
	if n == 0 {
		back("That policy is gone.")
		return
	}
	logging.Infof("srp: user %d deleted policy %d of %s", app.deps.UserID(ctx), id, owner.Key())
	back("Policy deleted. Claims already made keep their amounts.")
}
