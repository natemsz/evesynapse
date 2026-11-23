package srp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Replacement policies: how an owner replaces ships.
//
// A fleet uses the policy it names, else its owner's default. With
// neither, a handler decides each request from the loss value. Whatever
// applied when a claim was filed is stored on it, so a later change to
// a policy never changes a claim already made.
//
// Fit evidence comes only from the pilot's loss mail, because EVE does
// not show anyone else's fit. It is worked out once, when the claim is
// filed, and says what the mail had. It never says a fit was right.
// ---------------------------------------------------------------------------

// Stored in srp_policies: never rename.
const (
	srpModeManual  = "manual"
	srpModePercent = "percent_of_loss"
	srpModeFixed   = "fixed_by_hull"

	srpLedgerHandlers = "handlers"
	srpLedgerMembers  = "members"
)

const (
	srpPolicyNameMax = 60
	srpPoliciesMax   = 25
	srpHullsMax      = 100
	srpMissingShown  = 6
)

// srpPolicyFor is the policy a fleet runs under: the one it names, else
// its owner's default, else none.
func srpPolicyFor(op db.Op, policies []db.SrpPolicy) *db.SrpPolicy {
	var fallback *db.SrpPolicy
	for i := range policies {
		p := &policies[i]
		if op.SrpPolicyID.Valid && p.ID == op.SrpPolicyID.Int64 {
			return p
		}
		if p.IsDefault {
			fallback = p
		}
	}
	return fallback
}

func srpWindow(p *db.SrpPolicy) time.Duration {
	if p == nil {
		return srpClaimWindow
	}
	return time.Duration(p.ClaimWindowHours) * time.Hour
}

// srpSuggested is what the policy says to pay for a loss.
func (app *mod) srpSuggested(ctx context.Context, p *db.SrpPolicy, shipTypeID int64, value float64) float64 {
	if p == nil {
		return value
	}
	out := value
	switch p.Mode {
	case srpModePercent:
		out = value * float64(p.Percent) / 100
	case srpModeFixed:
		out = 0
		payout, err := app.queries.GetSRPPolicyHull(ctx, db.GetSRPPolicyHullParams{PolicyID: p.ID, ShipTypeID: shipTypeID})
		if err == nil {
			out = payout
		} else if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("srp: hull payout of policy %d: %v", p.ID, err)
		}
	}
	if p.MaxPayout > 0 && out > p.MaxPayout {
		out = p.MaxPayout
	}
	return out
}

// srpSightingGrace is how far outside a pilot's sightings in a fleet a
// loss still counts. The fleet is read about once a minute, and a pilot
// can drop out of it right after their ship dies.
const srpSightingGrace = 10 * time.Minute

// srpAttendedAt: the attendance row puts the pilot on the fleet when the
// loss happened. A row read from the fleet says when they were seen. A
// row marked by hand is the handler's own word for the whole fleet.
func srpAttendedAt(source string, first, last, at time.Time) bool {
	if source != module.AttendanceFleet {
		return true
	}
	return !at.Before(first.Add(-srpSightingGrace)) && !at.After(last.Add(srpSightingGrace))
}

// srpRefusal says why a loss cannot be claimed on a fleet, or nothing
// when it can. visible: the claimant can see the fleet; attended: they
// were on it when the ship died. A fleet they can neither see nor were
// on answers as if the loss had nothing to do with it. A fleet outside a
// corporation can be joined by anyone it lets in, so there a pilot has
// to have been on it: being in the same system at the same time is not
// enough. The claim window runs from when the fleet closes, because
// claims cannot open before that.
func srpRefusal(op db.Op, p *db.SrpPolicy, loss srpLoss, visible, attended, inDoctrine bool, now time.Time) string {
	closes := module.OpEnd(op).Add(module.OpCaptureTail)
	switch {
	case !srpDuring(op, loss.at), !visible && !attended:
		return "That loss was not during that fleet."
	case now.Before(closes):
		// A claim names a pilot on the fleet, and who is in a running
		// fleet is only for the people in it.
		return "Claims open once the fleet is over."
	case now.Sub(closes) > srpWindow(p):
		return "That loss is too old to claim."
	case op.OwnerKind == standing.OwnerCorporation && op.CorporationID != loss.km.Victim.CorporationID:
		return "That loss was not during that fleet."
	case op.OwnerKind != standing.OwnerCorporation && !attended,
		p != nil && p.RequiresAttendance && !attended:
		return "The fleet's attendance does not have this pilot at the time of the loss. Ask whoever runs the fleet to add them."
	case p != nil && p.RequiresDoctrine && !inDoctrine:
		return "This fleet's policy replaces doctrine ships only."
	}
	return ""
}

// srpInDoctrine: the hull is in what the fleet flew. A fleet that has
// started is judged by its stored copy, not by later edits.
func (app *mod) srpInDoctrine(ctx context.Context, op db.Op, shipTypeID int64) bool {
	if op.DoctrineSnapshotAt.Valid {
		return slices.Contains(op.DoctrineSnapshotShips, shipTypeID)
	}
	if op.DoctrineID == 0 {
		return false
	}
	in, err := app.queries.DoctrineHasShip(ctx, db.DoctrineHasShipParams{DoctrineID: op.DoctrineID, ShipTypeID: shipTypeID})
	if err != nil {
		logging.Errorf("srp: doctrine %d: %v", op.DoctrineID, err)
	}
	return err == nil && in
}

// srpDoctrineFit is one fit of the hull the fleet flew.
type srpDoctrineFit struct {
	Name       string `json:"name"`
	ShipTypeID int64  `json:"ship_type_id"`
	ItemsJSON  string `json:"items_json"`
}

func (app *mod) srpDoctrineFits(ctx context.Context, op db.Op, shipTypeID int64) []srpDoctrineFit {
	var fits []srpDoctrineFit
	if op.DoctrineSnapshotAt.Valid && op.DoctrineSnapshot.Valid {
		var snapshot struct {
			Fits []srpDoctrineFit `json:"fits"`
		}
		if err := json.Unmarshal([]byte(op.DoctrineSnapshot.String), &snapshot); err != nil {
			logging.Errorf("srp: doctrine copy of fleet %d: %v", op.ID, err)
			return nil
		}
		fits = snapshot.Fits
	} else if op.DoctrineID != 0 {
		rows, err := app.queries.ListDoctrineFits(ctx, []int64{op.DoctrineID})
		if err != nil {
			logging.Errorf("srp: fits of doctrine %d: %v", op.DoctrineID, err)
			return nil
		}
		for _, row := range rows {
			fits = append(fits, srpDoctrineFit{Name: row.Name, ShipTypeID: row.ShipTypeID, ItemsJSON: row.ItemsJson})
		}
	}
	return slices.DeleteFunc(fits, func(f srpDoctrineFit) bool { return f.ShipTypeID != shipTypeID })
}

// srpFittedFlag: the item was on the hull, in a low, mid or high slot, a
// rig slot or a subsystem slot. EVE's inventory flags.
func srpFittedFlag(flag int64) bool {
	return (flag >= 11 && flag <= 34) || (flag >= 92 && flag <= 99) || (flag >= 125 && flag <= 132)
}

// srpFitNote is what a loss mail showed against the closest doctrine fit.
type srpFitNote struct {
	Fit     string   `json:"fit"`
	Wanted  int      `json:"wanted"`
	Matched int      `json:"matched"`
	Missing []string `json:"missing,omitempty"`
}

// srpFitEvidence compares the loss mail's fitted modules with each
// doctrine fit of the hull and keeps the closest. It is empty when the
// mail has no item list, or the fleet has no fit of that hull.
func (app *mod) srpFitEvidence(ctx context.Context, op db.Op, payload string, shipTypeID int64) string {
	fits := app.srpDoctrineFits(ctx, op, shipTypeID)
	if len(fits) == 0 {
		return ""
	}
	var mail struct {
		Victim struct {
			Items []struct {
				ItemTypeID        int64 `json:"item_type_id"`
				Flag              int64 `json:"flag"`
				QuantityDestroyed int64 `json:"quantity_destroyed"`
				QuantityDropped   int64 `json:"quantity_dropped"`
			} `json:"items"`
		} `json:"victim"`
	}
	if json.Unmarshal([]byte(payload), &mail) != nil || len(mail.Victim.Items) == 0 {
		return ""
	}
	have := map[int64]int{}
	for _, item := range mail.Victim.Items {
		if srpFittedFlag(item.Flag) {
			have[item.ItemTypeID] += max(1, int(item.QuantityDestroyed+item.QuantityDropped))
		}
	}

	docs := make([]fitview.FitDoc, len(fits))
	var typeIDs []int64
	for i, fit := range fits {
		if json.Unmarshal([]byte(fit.ItemsJSON), &docs[i]) != nil {
			continue
		}
		for _, item := range docs[i].Items {
			if !item.Cargo && !slices.Contains(typeIDs, item.TypeID) {
				typeIDs = append(typeIDs, item.TypeID)
			}
		}
	}
	// Drones and fighters can be out in space when a ship dies, so a
	// mail without them says nothing. Only modules are compared.
	modules, err := app.queries.ListFittableTypeIDs(ctx, typeIDs)
	if err != nil {
		logging.Errorf("srp: module types for fleet %d: %v", op.ID, err)
		return ""
	}

	var best *srpFitNote
	var bestMissing []int64
	for i, fit := range fits {
		note := srpFitNote{Fit: fit.Name}
		var missing []int64
		for _, item := range docs[i].Items {
			if item.Cargo || item.Qty <= 0 || !slices.Contains(modules, item.TypeID) {
				continue
			}
			note.Wanted += item.Qty
			got := min(item.Qty, have[item.TypeID])
			note.Matched += got
			if got < item.Qty {
				missing = append(missing, item.TypeID)
			}
		}
		if note.Wanted == 0 {
			continue
		}
		if best == nil || note.Matched*best.Wanted > best.Matched*note.Wanted {
			kept := note
			best, bestMissing = &kept, missing
		}
	}
	if best == nil {
		return ""
	}
	for _, id := range bestMissing[:min(len(bestMissing), srpMissingShown)] {
		best.Missing = append(best.Missing, app.deps.TypeNameOrID(ctx, id))
	}
	raw, err := json.Marshal(best)
	if err != nil {
		return ""
	}
	return string(raw)
}

// srpFitSentence words stored fit evidence for a handler.
func srpFitSentence(raw string) string {
	if raw == "" {
		return ""
	}
	var note srpFitNote
	if json.Unmarshal([]byte(raw), &note) != nil || note.Wanted == 0 {
		return ""
	}
	out := fmt.Sprintf("The loss mail has %d of the %d modules in %s.", note.Matched, note.Wanted, note.Fit)
	if len(note.Missing) > 0 {
		out += " Not on the mail: " + strings.Join(note.Missing, ", ") + "."
	}
	return out
}

// fileClaim checks a loss against a fleet and its policy and stores the
// request. It reads everything about the loss from the stored killmail;
// mine says whether a character is the claimant's. It answers with what
// to tell the pilot.
func (app *mod) fileClaim(ctx context.Context, op db.Op, killmailID, userID int64, mine func(int64) bool, note string, now time.Time) (string, bool) {
	payload, hash, ok := app.deps.KillmailPayload(ctx, killmailID)
	var km esi.Killmail
	if !ok || json.Unmarshal([]byte(payload), &km) != nil {
		return "Pick the loss to claim.", false
	}
	if km.Victim.CharacterID == 0 || !mine(km.Victim.CharacterID) {
		return "That loss is not one of your characters'.", false
	}
	at, err := time.Parse(time.RFC3339, km.KillmailTime)
	if err != nil {
		return "Pick the loss to claim.", false
	}
	policies, err := app.queries.ListSRPPolicies(ctx, db.ListSRPPoliciesParams{OwnerKind: op.OwnerKind, OwnerID: op.OwnerID})
	if err != nil {
		logging.Errorf("srp: policies of fleet %d: %v", op.ID, err)
		return "The request could not be saved; check the server log.", false
	}
	policy := srpPolicyFor(op, policies)
	seen, seenErr := app.queries.GetOpAttendance(ctx, db.GetOpAttendanceParams{OpID: op.ID, CharacterID: km.Victim.CharacterID})
	if seenErr != nil && !errors.Is(seenErr, pgx.ErrNoRows) {
		logging.Errorf("srp: attendance on fleet %d: %v", op.ID, seenErr)
		return "The request could not be saved; check the server log.", false
	}
	loss := srpLoss{km: km, hash: hash, at: at}
	inDoctrine := app.srpInDoctrine(ctx, op, km.Victim.ShipTypeID)
	attended := seenErr == nil && srpAttendedAt(seen.Source, seen.FirstSeenAt, seen.LastSeenAt, at)
	visible := app.deps.FleetsVisible(ctx, []db.Op{op})[op.ID]
	if refusal := srpRefusal(op, policy, loss, visible, attended, inDoctrine, now); refusal != "" {
		return refusal, false
	}
	value := app.deps.KillmailLossValue(ctx, km.KillmailID, payload)
	params := db.CreateSRPRequestParams{
		CorporationID: op.CorporationID, OwnerKind: op.OwnerKind, OwnerID: op.OwnerID,
		OpID: op.ID, KillmailID: km.KillmailID, KillmailHash: hash,
		CharacterID: km.Victim.CharacterID, UserID: userID, ShipTypeID: km.Victim.ShipTypeID, SolarSystemID: km.SolarSystemID,
		LostAt: at, LossValue: value, SuggestedPayout: app.srpSuggested(ctx, policy, km.Victim.ShipTypeID, value),
		FitEvidence: app.srpFitEvidence(ctx, op, payload, km.Victim.ShipTypeID),
		Note:        note, CreatedAt: now,
	}
	if policy != nil {
		params.PolicyID = sql.NullInt64{Int64: policy.ID, Valid: true}
	}
	id, err := app.queries.CreateSRPRequest(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return "That loss has already been claimed.", false
	}
	if err != nil {
		logging.Errorf("srp: save request for killmail %d: %v", km.KillmailID, err)
		return "The request could not be saved; check the server log.", false
	}
	logging.Infof("srp: character %d asked for killmail %d on fleet %d (request %d)", km.Victim.CharacterID, km.KillmailID, op.ID, id)
	return "Request sent. You will be told here when it is answered.", true
}

// srpPolicyRow is one policy on the page, or the policy being edited.
type srpPolicyRow struct {
	ID         int64
	Name       string
	Mode       string
	Percent    int32
	MaxPayout  string
	ClaimDays  int32
	Doctrine   bool
	Attendance bool
	Ledger     string
	Text       string
	Default    bool
	Hulls      string
	Summary    string
	EditHref   string
	// Owner is the owner key the policy's form posts with.
	Owner string
}

// srpPolicySummary says what a policy does in one sentence.
func srpPolicySummary(p db.SrpPolicy) string {
	var parts []string
	switch p.Mode {
	case srpModePercent:
		parts = append(parts, "Pays "+strconv.Itoa(int(p.Percent))+"% of the loss")
	case srpModeFixed:
		parts = append(parts, "Pays a set amount per hull")
	default:
		parts = append(parts, "The handler decides the amount")
	}
	if p.MaxPayout > 0 {
		parts = append(parts, "up to "+srpISK(p.MaxPayout))
	}
	if p.RequiresDoctrine {
		parts = append(parts, "doctrine ships only")
	}
	if p.RequiresAttendance {
		parts = append(parts, "the pilot must be on attendance")
	}
	if days := int(p.ClaimWindowHours / 24); days >= 1 {
		parts = append(parts, "claim within "+humanize.Plural(days, "day"))
	} else {
		parts = append(parts, "claim within "+humanize.Plural(int(p.ClaimWindowHours), "hour"))
	}
	return strings.Join(parts, ", ") + "."
}
