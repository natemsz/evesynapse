package appraise

// The appraisal flow: parse a paste, resolve its names against
// the item database, price the lines through core's appraisal
// seam, and shape the snapshot every surface renders from —
// the fresh result, a saved appraisal, and the shared view are
// the same snapshot, so a shared page can never drift from
// what its owner saw. Stored reads only: pricing is the core's
// batched stored-table pass, name resolution is two batched
// lookups however long the list, and the buy total is one more
// batched read of the same typicals table.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// typicalFreshAge mirrors the appraisal core's own fresh
// window for a swept typical: past it, the line's as-of is
// shown with its age (the floor rule — a stale own row still
// speaks, and says how old it is).
const typicalFreshAge = 3 * time.Hour

// maxPasteBytes caps the posted paste itself, well past what
// the line cap can hold.
const maxPasteBytes = 512 << 10

// suggestionCap bounds the nearest-name lookups for unresolved
// lines: one search per unresolved line would turn a bad paste
// into a query fan-out.
const suggestionCap = 10

// --- the snapshot ----------------------------------------------------------

// snapLine is one priced (or honestly unpriced) line of a
// snapshot, display-complete: the page renders it verbatim.
type snapLine struct {
	TypeID     int64     `json:"type_id"`
	Name       string    `json:"name"`
	Qty        int64     `json:"qty"`
	UnitPrice  float64   `json:"unit_price"`
	LineValue  float64   `json:"line_value"`
	Tier       string    `json:"tier"`
	AsOf       time.Time `json:"as_of"`
	Priced     bool      `json:"priced"`
	Note       string    `json:"note,omitempty"`
	UnitVolume float64   `json:"unit_volume"`
	LineVolume float64   `json:"line_volume"`
}

// snapUnresolved is one pasted line no name candidate could
// resolve, kept verbatim with the reason and, where the item
// database offers one, the nearest real name.
type snapUnresolved struct {
	Text       string `json:"text"`
	Qty        int64  `json:"qty"`
	Reason     string `json:"reason"`
	Suggestion string `json:"suggestion,omitempty"`
}

// snapshot is one appraisal, exactly as computed.
type snapshot struct {
	Basis       snapshotBasis    `json:"basis"`
	ExcludeText string           `json:"exclude_text,omitempty"`
	Multiplier  int64            `json:"multiplier,omitempty"`
	Lines       []snapLine       `json:"lines"`
	Unresolved  []snapUnresolved `json:"unresolved,omitempty"`
	Bad         []string         `json:"bad,omitempty"`
	Notes       []string         `json:"notes,omitempty"`
	TotalValue  float64          `json:"total_value"`
	TotalVolume float64          `json:"total_volume"`
	BuyValue    float64          `json:"buy_value"`
	BuyLines    int              `json:"buy_lines"`
	PricedLines int              `json:"priced_lines"`
	TotalLines  int              `json:"total_lines"`
}

// resolvedLine is a paste line after name resolution, before
// aggregation and pricing.
type resolvedLine struct {
	TypeID      int64
	Name        string // the type's canonical name
	Qty         int64
	AbyssalBase bool
	// ItemID is the unique item a showinfo item link named,
	// set only when the abyssal corpus holds a stored
	// estimate for it: the line then prices from that
	// estimate through the core, and Abyssal is the corpus
	// row the line's note words its range and basis from.
	ItemID     int64
	Abyssal    *db.AbyssalItem
	LinkMiss   bool // link named an item whose rolls are not on record
	NoEstimate bool // link named a recorded item with no estimate yet
	Facts      pasteLine
}

// parseMultiplier reads the quantity multiplier, clamped to
// a sane range: at least 1, at most 1000.
func parseMultiplier(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "×")), 10, 64)
	if err != nil || n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// computeAppraisal runs the whole flow over one paste under
// a pricing basis, which the snapshot records. A non-empty
// exclude paste subtracts its items and quantities from the
// result (list A minus list B), and a multiplier above one
// appraises that many copies of what remains.
func (app *page) computeAppraisal(ctx context.Context, input string, basis pricingBasis, exclude string, multiplier int64) (*snapshot, error) {
	if multiplier < 1 {
		multiplier = 1
	}
	snap := &snapshot{Basis: basis.snapshot(), ExcludeText: exclude, Multiplier: multiplier}
	var resolved []resolvedLine
	if moon, ok := moonScanLines(input); ok {
		parsed := parsePaste(moon)
		lines, unresolved, err := app.resolveList(ctx, parsed)
		if err != nil {
			return nil, err
		}
		resolved = lines
		snap.Unresolved = unresolved
		snap.Bad = parsed.Bad
		snap.Notes = append(snap.Notes, "Moon survey: each ore is appraised at its survey share of 10,000 units, so the total is the value of 10,000 units of this moon's ore at the selected basis.")
	} else if first, ok := firstContentLine(input); ok && fitview.EftHeaderRe.MatchString(first) {
		lines, unresolved, notes, err := app.resolveEFT(ctx, input)
		if err != nil {
			return nil, err
		}
		resolved = lines
		snap.Unresolved = unresolved
		snap.Notes = notes
	} else {
		parsed := parsePaste(input)
		lines, unresolved, err := app.resolveList(ctx, parsed)
		if err != nil {
			return nil, err
		}
		resolved = lines
		snap.Unresolved = unresolved
		snap.Bad = parsed.Bad
		if parsed.Truncated {
			snap.Notes = append(snap.Notes, fmt.Sprintf("Only the first %s lines were appraised.", esi.FormatInt(maxPasteLines)))
		}
	}
	if len(resolved) == 0 && len(snap.Unresolved) == 0 && len(snap.Bad) == 0 {
		snap.Notes = append(snap.Notes, "Nothing in that paste could be read as an item list.")
	}

	agg := aggregateLines(resolved)
	if multiplier > 1 {
		for i := range agg {
			agg[i].Qty *= multiplier
		}
		snap.Notes = append(snap.Notes, fmt.Sprintf("Quantities multiplied ×%s.", esi.FormatInt(multiplier)))
	}
	if strings.TrimSpace(exclude) != "" {
		var notes []string
		var err error
		agg, notes, err = app.subtractExclusions(ctx, agg, exclude)
		if err != nil {
			return nil, err
		}
		snap.Notes = append(snap.Notes, notes...)
	}
	priceLines := make([]module.AppraisalLine, 0, len(agg))
	for _, ln := range agg {
		pl := module.AppraisalLine{TypeID: ln.TypeID, Quantity: ln.Qty, ItemID: ln.ItemID, Included: true}
		if ln.Facts.HasFacts && ln.Facts.Copy {
			// A pasted copy prices as a copy or not at all:
			// its facts ride to the core, whose copy branch
			// never touches the original's type-level prices.
			// Runs is 0 when the paste stated none, which
			// keys no stored median.
			pl.BlueprintCopy = true
			pl.Runs = ln.Facts.Runs
			pl.MaterialEfficiency = ln.Facts.ME
			pl.TimeEfficiency = ln.Facts.TE
		}
		priceLines = append(priceLines, pl)
	}
	var res module.Appraisal
	if len(priceLines) > 0 {
		var err error
		res, err = app.deps.AppraiseLinesWithBasis(ctx, priceLines, basis.toModule())
		if err != nil {
			return nil, err
		}
	}
	// The stored medians behind any copy-priced lines, in one
	// batched read, so a copy line's note can say what its
	// price stands on -- the same basis wording habit as the
	// abyssal estimate's note.
	copySamples := map[string]int64{}
	{
		var copyTypes []int64
		seenType := map[int64]bool{}
		for _, ln := range agg {
			if ln.Facts.HasFacts && ln.Facts.Copy && !seenType[ln.TypeID] {
				seenType[ln.TypeID] = true
				copyTypes = append(copyTypes, ln.TypeID)
			}
		}
		if len(copyTypes) > 0 {
			rows, err := app.queries.ListBlueprintCopyMediansForTypes(ctx, copyTypes)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				copySamples[fmt.Sprintf("%d/%d", row.TypeID, row.Runs)] = row.SampleCount
			}
		}
	}
	// Unit volumes from the SDE physics rows, in one batched
	// read: the line and total volumes ride the snapshot.
	volumes := map[int64]float64{}
	{
		var typeIDs []int64
		seenVol := map[int64]bool{}
		for _, ln := range agg {
			if !seenVol[ln.TypeID] {
				seenVol[ln.TypeID] = true
				typeIDs = append(typeIDs, ln.TypeID)
			}
		}
		if len(typeIDs) > 0 {
			rows, err := app.queries.ListSDETypePhysicsByIDs(ctx, typeIDs)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				volumes[row.TypeID] = row.Volume
			}
		}
	}
	buys, err := app.typicalBuys(ctx, agg, res, basis)
	if err != nil {
		return nil, err
	}
	for i, ln := range agg {
		sl := snapLine{TypeID: ln.TypeID, Name: ln.Name, Qty: ln.Qty, Tier: module.AppraisalTierUnpriced, Note: ln.Note}
		sl.UnitVolume = volumes[ln.TypeID]
		sl.LineVolume = sl.UnitVolume * float64(ln.Qty)
		snap.TotalVolume += sl.LineVolume
		if i < len(res.Lines) {
			al := res.Lines[i]
			sl.UnitPrice, sl.LineValue, sl.Tier, sl.AsOf, sl.Priced = al.UnitPrice, al.LineValue, al.Tier, al.AsOf, al.Priced
		}
		if sl.Priced && sl.Tier == module.AppraisalTierCopyMedian {
			if n, ok := copySamples[fmt.Sprintf("%d/%d", ln.TypeID, ln.Facts.Runs)]; ok {
				sl.Note += fmt.Sprintf(" · median of %s copy listings", esi.FormatInt(n))
			}
		}
		if sl.Priced {
			snap.PricedLines++
			snap.TotalValue += sl.LineValue
			if b, ok := buys[ln.TypeID]; ok && isTypicalTier(sl.Tier) {
				snap.BuyValue += b * float64(ln.Qty)
				snap.BuyLines++
			}
		}
		snap.Lines = append(snap.Lines, sl)
	}
	snap.TotalLines = len(snap.Lines) + len(snap.Unresolved) + len(snap.Bad)
	return snap, nil
}

// firstContentLine is the paste's first non-empty line, the
// EFT detection point.
func firstContentLine(input string) (string, bool) {
	for _, raw := range strings.Split(input, "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			return line, true
		}
	}
	return "", false
}

// resolveList resolves a parsed line list against the item
// database in one batched lookup.
func (app *page) resolveList(ctx context.Context, parsed pasteResult) ([]resolvedLine, []snapUnresolved, error) {
	lowers := map[string]bool{}
	for _, pl := range parsed.Lines {
		for _, c := range pl.Candidates {
			lowers[strings.ToLower(c.Name)] = true
		}
	}
	byLower := map[string]db.ListSDETypesByLowerNamesRow{}
	byTypeID := map[int64]db.ListSDETypesByLowerNamesRow{}
	if len(lowers) > 0 {
		names := make([]string, 0, len(lowers))
		for n := range lowers {
			names = append(names, n)
		}
		rows, err := app.queries.ListSDETypesByLowerNames(ctx, names)
		if err != nil {
			return nil, nil, fmt.Errorf("appraisal: resolve names: %w", err)
		}
		for _, row := range rows {
			byLower[strings.ToLower(row.Name)] = row
			byTypeID[row.TypeID] = row
		}
	}
	// The abyssal corpus rows behind any item links, in one
	// batched read: a linked line prices its own item when
	// the corpus holds an estimate for it.
	corpus := map[int64]*db.AbyssalItem{}
	var itemIDs []int64
	for _, pl := range parsed.Lines {
		if pl.LinkItemID > 0 {
			itemIDs = append(itemIDs, pl.LinkItemID)
		}
	}
	if len(itemIDs) > 0 {
		rows, err := app.queries.ListAbyssalItemsByIDs(ctx, itemIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("appraisal: abyssal items: %w", err)
		}
		for i := range rows {
			corpus[rows[i].ItemID] = &rows[i]
		}
	}
	findHit := func(pl pasteLine) (*db.ListSDETypesByLowerNamesRow, nameCandidate) {
		for _, c := range pl.Candidates {
			if row, ok := byLower[strings.ToLower(c.Name)]; ok {
				row := row
				return &row, c
			}
		}
		return nil, nameCandidate{}
	}
	canonName := func(typeID int64, fallback string) string {
		if row, ok := byTypeID[typeID]; ok && row.Name != "" {
			return row.Name
		}
		if n := app.deps.TypeName(ctx, typeID); n != "" && !strings.HasPrefix(n, "Type #") {
			return n
		}
		return fallback
	}
	var resolved []resolvedLine
	var unresolved []snapUnresolved
	for _, pl := range parsed.Lines {
		if pl.LinkItemID > 0 {
			rl, u := resolveLinked(pl, corpus[pl.LinkItemID], findHit, canonName)
			if u != nil {
				unresolved = append(unresolved, *u)
			} else {
				resolved = append(resolved, rl)
			}
			continue
		}
		hit, hitCand := findHit(pl)
		if hit == nil {
			u := snapUnresolved{Text: pl.Candidates[0].Name, Qty: pl.Candidates[0].Qty,
				Reason: "That name is not in the item database."}
			if _, isMutated := mutationBase(pl.Candidates[0].Name); isMutated {
				u.Reason = "This mutated item's base item could not be named."
			}
			unresolved = append(unresolved, u)
			continue
		}
		resolved = append(resolved, resolvedLine{
			TypeID: hit.TypeID, Name: hit.Name, Qty: hitCand.Qty,
			AbyssalBase: hitCand.AbyssalBase, Facts: pl,
		})
	}
	// Nearest-name suggestions for the first few unresolved.
	for i := range unresolved {
		if i >= suggestionCap {
			break
		}
		unresolved[i].Suggestion = app.suggestName(ctx, unresolved[i].Text)
	}
	return resolved, unresolved, nil
}

// resolveLinked resolves one showinfo item-link line, and
// the abyssal corpus decides how: an item with a stored
// estimate prices from that estimate — its item id rides to
// the appraisal core, which reads the same stored figure the
// contract pages show; a recorded item with no estimate yet
// prices its known source (base) type exactly; an item the
// corpus has never seen falls back to name resolution
// against its base, flagged so the line says its rolls are
// not on record. Exactly one return is meaningful: a nil
// unresolved means the line resolved.
func resolveLinked(pl pasteLine, row *db.AbyssalItem,
	findHit func(pasteLine) (*db.ListSDETypesByLowerNamesRow, nameCandidate),
	canonName func(int64, string) string) (resolvedLine, *snapUnresolved) {
	if row != nil && row.EstValue.Valid {
		ab := *row
		return resolvedLine{
			TypeID: row.TypeID, Name: canonName(row.TypeID, pl.LinkName), Qty: 1,
			ItemID: row.ItemID, Abyssal: &ab, Facts: pl,
		}, nil
	}
	if row != nil {
		return resolvedLine{
			TypeID: row.SourceTypeID, Name: canonName(row.SourceTypeID, pl.LinkName), Qty: 1,
			NoEstimate: true, Facts: pl,
		}, nil
	}
	hit, hitCand := findHit(pl)
	if hit == nil {
		text := pl.LinkName
		if text == "" {
			text = strings.TrimSpace(pl.Raw)
		}
		return resolvedLine{}, &snapUnresolved{Text: text, Qty: 1,
			Reason: "This item's rolls are not in our records, and its base item could not be named."}
	}
	return resolvedLine{
		TypeID: hit.TypeID, Name: hit.Name, Qty: 1,
		AbyssalBase: hitCand.AbyssalBase, LinkMiss: true, Facts: pl,
	}, nil
}

// suggestName finds the nearest real type name to a missed
// one: a substring search, preferring a name that starts with
// the text, then the shortest container. "" when nothing is
// close.
func (app *page) suggestName(ctx context.Context, name string) string {
	esc := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(name)
	rows, err := app.queries.SearchSDETypes(ctx, "%"+esc+"%")
	if err != nil || len(rows) == 0 {
		return ""
	}
	best := ""
	for _, row := range rows {
		if strings.EqualFold(row.Name, name) {
			continue
		}
		if best == "" || len(row.Name) < len(best) {
			best = row.Name
		}
	}
	return best
}

// resolveEFT prices a pasted fit through the fitting tool's
// own EFT parser: the ship, every fitted item, the loaded
// charges and the cargo and drones the export lists.
func (app *page) resolveEFT(ctx context.Context, input string) ([]resolvedLine, []snapUnresolved, []string, error) {
	doc, notes := app.deps.FitBuilder().ParseEFT(ctx, input)
	var resolved []resolvedLine
	add := func(typeID, qty int64) {
		if typeID > 0 && qty > 0 {
			resolved = append(resolved, resolvedLine{TypeID: typeID, Name: app.deps.TypeName(ctx, typeID), Qty: qty})
		}
	}
	add(doc.ShipTypeID, 1)
	for _, it := range doc.Items {
		add(it.TypeID, int64(it.Qty))
	}
	chargeQty := map[int64]int64{}
	for _, chargeID := range doc.Charges {
		chargeQty[chargeID]++
	}
	for chargeID, n := range chargeQty {
		add(chargeID, n)
	}
	var unresolved []snapUnresolved
	var kept []string
	for _, note := range notes {
		switch {
		case strings.HasPrefix(note, "Couldn't place: "):
			unresolved = append(unresolved, snapUnresolved{
				Text: strings.TrimPrefix(note, "Couldn't place: "), Qty: 1,
				Reason: "That name is not in the item database.",
			})
		case strings.HasPrefix(note, "Couldn't find a ship called "):
			name := strings.Trim(strings.TrimPrefix(note, "Couldn't find a ship called "), "\".")
			unresolved = append(unresolved, snapUnresolved{
				Text: name, Qty: 1, Reason: "That ship name is not in the item database.",
			})
		default:
			kept = append(kept, note)
		}
	}
	return resolved, unresolved, kept, nil
}

// aggLine is a resolved line after same-type aggregation.
type aggLine struct {
	TypeID int64
	ItemID int64 // a unique item's id; unique lines never merge across items
	Name   string
	Qty    int64
	Note   string
	// Facts are the line's parsed blueprint facts, carried
	// so a copy line reaches the appraisal core AS a copy:
	// without them it would price at its original's figure.
	Facts pasteLine
}

// aggregateLines merges same-type lines that carry the same
// facts (two stacks of one mineral become one line; two
// different blueprint copies never do), keeping first-seen
// order. A line priced from one unique item's rolls carries
// that item's id in its key, so two different mutated items
// of one type stay two lines.
func aggregateLines(resolved []resolvedLine) []aggLine {
	var out []aggLine
	seen := map[string]int{}
	for _, ln := range resolved {
		note := lineNote(ln)
		key := fmt.Sprintf("%d|%d|%s", ln.TypeID, ln.ItemID, note)
		if i, ok := seen[key]; ok {
			out[i].Qty += ln.Qty
			continue
		}
		seen[key] = len(out)
		out = append(out, aggLine{TypeID: ln.TypeID, ItemID: ln.ItemID, Name: ln.Name, Qty: ln.Qty, Note: note, Facts: ln.Facts})
	}
	return out
}

// subtractExclusions removes an exclusion paste's quantities
// from the aggregated lines, by type: what remains is the
// appraisal. Lines reduced to zero go away; exclusion lines
// that cannot be read are reported, never silently applied.
func (app *page) subtractExclusions(ctx context.Context, agg []aggLine, exclude string) ([]aggLine, []string, error) {
	parsed := parsePaste(exclude)
	resolved, unresolved, err := app.resolveList(ctx, parsed)
	if err != nil {
		return nil, nil, err
	}
	sub := map[int64]int64{}
	for _, ln := range aggregateLines(resolved) {
		sub[ln.TypeID] += ln.Qty
	}
	var notes []string
	if len(unresolved) > 0 || len(parsed.Bad) > 0 {
		notes = append(notes, fmt.Sprintf("%d exclusion line(s) could not be read and were ignored.", len(unresolved)+len(parsed.Bad)))
	}
	var removed int64
	out := make([]aggLine, 0, len(agg))
	for _, ln := range agg {
		if n, ok := sub[ln.TypeID]; ok && n > 0 {
			if n >= ln.Qty {
				removed += ln.Qty
				delete(sub, ln.TypeID)
				continue
			}
			removed += n
			ln.Qty -= n
			delete(sub, ln.TypeID)
		}
		out = append(out, ln)
	}
	if removed > 0 {
		notes = append(notes, fmt.Sprintf("Excluded %s unit(s) from the second list.", esi.FormatInt(removed)))
	} else if len(notes) == 0 {
		notes = append(notes, "Nothing in the second list matched the first.")
	}
	return out, notes, nil
}

// abyssalEstimateNote words a stored abyssal estimate's
// range and basis for one appraised line, in the same words
// the contract pages use for the same stored figure.
func abyssalEstimateNote(ab *db.AbyssalItem) string {
	basis := ""
	switch ab.EstBasis {
	case "corpus":
		if ab.EstScope == "module" {
			basis = fmt.Sprintf("from %d listings of this module", ab.EstComps)
		} else {
			basis = fmt.Sprintf("from %d listings of similar modules", ab.EstComps)
		}
	case "curve":
		basis = "score curve (few listings)"
	case "prior":
		basis = "base module price and rolls only"
	}
	if ab.EstLow.Valid && ab.EstHigh.Valid {
		rng := fmt.Sprintf("likely %s to %s", esi.FormatISK(ab.EstLow.Float64), esi.FormatISK(ab.EstHigh.Float64))
		if basis != "" {
			return rng + " · " + basis
		}
		return rng
	}
	return basis
}

// lineNote words a line's special facts: blueprint facts the
// paste carried (or, for a bare blueprint name, that it prices
// as an original), and the mutated-item labels — a linked
// item priced from its stored estimate carries the estimate's
// range and basis; a mutation priced without its rolls
// prices the base item and says why.
func lineNote(ln resolvedLine) string {
	var parts []string
	f := ln.Facts
	switch {
	case f.HasFacts:
		kind := "BPO"
		if f.Copy {
			kind = "BPC"
		}
		note := kind
		if f.HasME {
			note += fmt.Sprintf(" · ME %d", f.ME)
		}
		if f.HasTE {
			note += fmt.Sprintf(" · TE %d", f.TE)
		}
		if f.HasRuns && f.Runs > 0 {
			note += fmt.Sprintf(" · %d runs", f.Runs)
		}
		parts = append(parts, note)
	case strings.HasSuffix(ln.Name, " Blueprint"):
		parts = append(parts, "Blueprint — priced as an original")
	}
	switch {
	case ln.Abyssal != nil:
		if note := abyssalEstimateNote(ln.Abyssal); note != "" {
			parts = append(parts, note)
		}
	case ln.NoEstimate:
		parts = append(parts, "Base item price — we hold this item's rolls but have no estimate for it yet")
	case ln.LinkMiss:
		parts = append(parts, "Base item price — this item's rolls are not in our records")
	case ln.AbyssalBase:
		parts = append(parts, "Base item price — this mutated item was pasted without its link, so its own rolls were not priced")
	default:
		if _, mutated := mutationBase(ln.Name); mutated {
			parts = append(parts, "Base item price — this mutated item was pasted without its link, so its own rolls were not priced")
		}
	}
	return strings.Join(parts, " · ")
}

// isTypicalTier reports whether a tier is one of the swept
// typical family, in any mode.
func isTypicalTier(tier string) bool {
	switch tier {
	case module.AppraisalTierTypical, module.AppraisalTierTypicalBuy,
		module.AppraisalTierTypicalSplit, module.AppraisalTierTypicalSplitSell, module.AppraisalTierTypicalSplitBuy:
		return true
	}
	return false
}

// typicalBuys reads the stored typical buys at the basis hub
// for the priced set in one batched lookup, for the buy total.
func (app *page) typicalBuys(ctx context.Context, agg []aggLine, res module.Appraisal, basis pricingBasis) (map[int64]float64, error) {
	out := map[int64]float64{}
	var typeIDs []int64
	seen := map[int64]bool{}
	for i, ln := range agg {
		if i < len(res.Lines) && res.Lines[i].Priced && isTypicalTier(res.Lines[i].Tier) && !seen[ln.TypeID] {
			seen[ln.TypeID] = true
			typeIDs = append(typeIDs, ln.TypeID)
		}
	}
	if len(typeIDs) == 0 {
		return out, nil
	}
	regionID := basis.RegionID
	if regionID == 0 {
		regionID = app.deps.DefaultMarketRegion()
	}
	rows, err := app.queries.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{
		RegionID: regionID, Column2: typeIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("appraisal: typical buys: %w", err)
	}
	for _, row := range rows {
		if row.TypicalBuy > 0 {
			out[row.TypeID] = row.TypicalBuy
		}
	}
	return out, nil
}

// --- display ---------------------------------------------------------------

// resultLine is one display row of a rendered snapshot.
type resultLine struct {
	TypeID    int64
	Name      string
	Qty       string
	UnitPrice string
	LineValue string
	TierLabel string
	Note      string
	AsOf      string
	Priced    bool
	UnitVol   string // unit volume in m³, "" when unknown
	LineVol   string // line volume in m³, "" when unknown
}

// resultView is a snapshot shaped for the template.
type resultView struct {
	Lines      []resultLine
	Unresolved []snapUnresolved
	Bad        []string
	Notes      []string
	TotalValue string
	TotalVol   string // total volume in m³, "" when unknown
	ISKPerM3   string // total value per m³, "" without a volume
	BuyValue   string
	BuyLines   int
	Priced     int
	Total      int
	TierMix    string
	BasisText  string // the pricing basis, worded for the page
	Basis      pricingBasis
	Hubs       []hubOption // hub choices for the reprice form
	RepriceURL string      // where the reprice form posts, when offered
	Created    string      // "Oct 17, 2026, 6:52 PM" for a stored snapshot
	Shared     bool        // rendered from a share link
	OwnID      int64       // the signed-in owner's appraisal id, when it is theirs
	ShareURL   string
	CanDelete  bool   // the viewer created this appraisal and may delete it
	DeleteURL  string // where the delete form posts, when CanDelete
}

// appraiseView is the page body.
type appraiseView struct {
	Result    *resultView
	Reprocess *reprocessView
	Saved     []savedRow
	SignedIn  bool
	Input     string
	Basis     pricingBasis
	Hubs      []hubOption
}

// savedRow is one entry of the owner's appraisal list.
type savedRow struct {
	ID       int64
	Created  string
	Total    string
	Priced   int64
	TotalLn  int64
	ShareURL string
	Anon     bool // an anonymous appraisal, opened by its share link
}

// buildResult shapes a snapshot for display. age is the deps'
// age wording for a stale typical's as-of.
func (app *page) buildResult(snap *snapshot) *resultView {
	v := &resultView{
		Unresolved: snap.Unresolved, Bad: snap.Bad, Notes: snap.Notes,
		TotalValue: esi.FormatISK(snap.TotalValue),
		BuyLines:   snap.BuyLines, Priced: snap.PricedLines, Total: snap.TotalLines,
	}
	if snap.BuyLines > 0 {
		v.BuyValue = esi.FormatISK(snap.BuyValue)
	}
	if snap.TotalVolume > 0 {
		v.TotalVol = formatVolume(snap.TotalVolume)
		if snap.TotalValue > 0 {
			v.ISKPerM3 = esi.FormatISK(snap.TotalValue / snap.TotalVolume)
		}
	}
	basis := snap.Basis.basis()
	if basis.HubName == "" {
		if name, ok := app.deps.MarketRegionName(basis.RegionID); ok {
			basis.HubName = name
		}
	}
	v.Basis = basis
	v.BasisText = basis.basisText()
	v.Hubs = app.hubOptions(basis.RegionID)
	counts := map[string]int{}
	for _, ln := range snap.Lines {
		rl := resultLine{
			TypeID: ln.TypeID, Name: ln.Name, Qty: esi.FormatInt(ln.Qty),
			TierLabel: tierLabelWithBasis(ln.Tier, basis), Note: ln.Note, Priced: ln.Priced,
		}
		if ln.UnitVolume > 0 {
			rl.UnitVol = formatVolume(ln.UnitVolume)
			rl.LineVol = formatVolume(ln.LineVolume)
		}
		if ln.Priced {
			counts[ln.Tier]++
			rl.UnitPrice = esi.FormatISK(ln.UnitPrice)
			rl.LineValue = esi.FormatISK(ln.LineValue)
			rl.AsOf = ln.AsOf.Format("Jan 2, 3:04 PM")
			if isTypicalTier(ln.Tier) && time.Since(ln.AsOf) > typicalFreshAge {
				rl.AsOf += " (" + app.deps.AgeText(ln.AsOf) + ")"
			}
		}
		v.Lines = append(v.Lines, rl)
	}
	var parts []string
	for _, tier := range []string{module.AppraisalTierTypical, module.AppraisalTierTypicalBuy, module.AppraisalTierTypicalSplit, module.AppraisalTierTypicalSplitSell, module.AppraisalTierTypicalSplitBuy, module.AppraisalTierEstimate, module.AppraisalTierAbyssal, module.AppraisalTierCopyMedian, module.AppraisalTierFallback} {
		if n := counts[tier]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d × %s", n, tierLabelWithBasis(tier, basis)))
		}
	}
	v.TierMix = strings.Join(parts, ", ")
	return v
}

// --- storage ---------------------------------------------------------------

// errNoItemLines is the refusal to store an appraisal whose
// paste resolved to no item lines at all: prose, a URL, or
// comment-only text is not an appraisal, and nothing is
// written for it.
var errNoItemLines = errors.New("appraisal: paste contained no recognised item lines")

// snapshotHasItemLines reports whether a snapshot carries at
// least one resolved item line.
func snapshotHasItemLines(snap *snapshot) bool {
	return snap != nil && len(snap.Lines) > 0
}

func inputHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func newShareToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// saveAppraisal stores one computed appraisal for its owner
// and returns the row id and its share token. A snapshot with
// no resolved item lines is refused here as well as in the
// handler, so no caller can store an empty appraisal.
func (app *page) saveAppraisal(ctx context.Context, userID int64, anonymous bool, input string, snap *snapshot) (int64, string, error) {
	if !snapshotHasItemLines(snap) {
		return 0, "", errNoItemLines
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, "", err
	}
	token, err := newShareToken()
	if err != nil {
		return 0, "", err
	}
	id, err := app.queries.InsertUserAppraisal(ctx, db.InsertUserAppraisalParams{
		UserID: userID, InputText: input, InputHash: inputHash(input), ShareToken: token,
		Snapshot: raw, TotalValue: snap.TotalValue,
		PricedLines: int64(snap.PricedLines), TotalLines: int64(snap.TotalLines),
		IsAnonymous: anonymous,
	})
	return id, token, err
}

// storedResult rebuilds a stored row's result view: the
// snapshot renders verbatim, never repriced.
func (app *page) storedResult(row db.UserAppraisal) (*resultView, error) {
	var snap snapshot
	if err := json.Unmarshal(row.Snapshot, &snap); err != nil {
		return nil, err
	}
	v := app.buildResult(&snap)
	v.Created = row.CreatedAt.Format("Jan 2, 2006, 3:04 PM")
	return v, nil
}

func (app *page) savedList(ctx context.Context, userID int64) []savedRow {
	rows, err := app.queries.ListUserAppraisals(ctx, db.ListUserAppraisalsParams{UserID: userID, Limit: 20})
	if err != nil {
		logging.Errorf("appraisal: list saved: %v", err)
		return nil
	}
	out := make([]savedRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, savedRow{
			ID: row.ID, Created: row.CreatedAt.Format("Jan 2, 2006, 3:04 PM"),
			Total: esi.FormatISK(row.TotalValue), Priced: row.PricedLines, TotalLn: row.TotalLines,
			ShareURL: "/market/appraise/s/" + row.ShareToken + "/",
		})
	}
	return out
}

// anonSavedList shapes the cookie's anonymous appraisals in
// cookie order (newest first), skipping tokens whose rows are
// gone.
func (app *page) anonSavedList(ctx context.Context, tokens []string) []savedRow {
	if len(tokens) == 0 {
		return nil
	}
	rows, err := app.queries.ListUserAppraisalsByShareTokens(ctx, tokens)
	if err != nil {
		logging.Errorf("appraisal: list anonymous saved: %v", err)
		return nil
	}
	byToken := make(map[string]db.UserAppraisal, len(rows))
	for _, row := range rows {
		if row.IsAnonymous {
			byToken[row.ShareToken] = row
		}
	}
	out := make([]savedRow, 0, len(tokens))
	for _, tok := range tokens {
		row, ok := byToken[tok]
		if !ok {
			continue
		}
		out = append(out, savedRow{
			ID: row.ID, Created: row.CreatedAt.Format("Jan 2, 2006, 3:04 PM"),
			Total: esi.FormatISK(row.TotalValue), Priced: row.PricedLines, TotalLn: row.TotalLines,
			ShareURL: "/market/appraise/s/" + row.ShareToken + "/", Anon: true,
		})
	}
	return out
}

// --- handlers --------------------------------------------------------------

// handleAppraisePage renders the tool: the paste form, the
// owner's saved appraisals, and ?a=<id>'s stored result.
func (app *page) handleAppraisePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	view := &appraiseView{SignedIn: data.LoggedIn}
	userID := app.deps.UserID(ctx)
	basis := app.basisFromValues(app.rememberedBasis(ctx, r, userID), r.URL.Query())
	view.Basis = basis
	view.Hubs = app.hubOptions(basis.RegionID)
	if userID > 0 {
		view.Saved = app.savedList(ctx, userID)
		if raw := r.URL.Query().Get("a"); raw != "" {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
				if row, err := app.queries.GetUserAppraisal(ctx, db.GetUserAppraisalParams{ID: id, UserID: userID}); err == nil {
					if res, err := app.storedResult(row); err == nil {
						res.OwnID = row.ID
						res.ShareURL = "/market/appraise/s/" + row.ShareToken + "/"
						res.RepriceURL = "/market/appraise/" + strconv.FormatInt(row.ID, 10) + "/reprice"
						res.CanDelete = true
						res.DeleteURL = "/market/appraise/" + strconv.FormatInt(row.ID, 10) + "/delete"
						view.Result = res
					}
				}
			}
			if view.Result == nil {
				data.Error = "That appraisal could not be found."
			}
		}
	} else {
		view.Saved = app.anonSavedList(ctx, app.anonTokens(ctx, r))
	}
	data.Appraise = view
	app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
}

// handleAppraiseSubmit computes one paste. A paste that
// resolves to no item lines is rejected with an error and
// nothing is stored. Otherwise the appraisal is saved — for
// a signed-in user under their account, for an anonymous
// visitor under the anonymous owner with its token remembered
// in the history cookie — and the page redirects to the
// stored snapshot, so a reload shows what was saved.
func (app *page) handleAppraiseSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxPasteBytes)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
		return
	}
	input := r.FormValue("items")
	userID := app.deps.UserID(ctx)
	if strings.TrimSpace(input) == "" {
		http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
		return
	}
	basis := app.basisFromValues(app.rememberedBasis(ctx, r, userID), r.Form)
	snap, err := app.computeAppraisal(ctx, input, basis, r.FormValue("exclude"), parseMultiplier(r.FormValue("multiplier")))
	if err != nil {
		logging.Errorf("appraisal: compute: %v", err)
		data := &PageData{ShellData: app.deps.PublicShell(ctx)}
		data.Error = "That appraisal could not be run. Please try again."
		data.Appraise = &appraiseView{SignedIn: data.LoggedIn, Input: input}
		app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
		return
	}
	if !snapshotHasItemLines(snap) {
		data := &PageData{ShellData: app.deps.PublicShell(ctx)}
		data.Error = "Nothing in that paste could be read as an item list, so no appraisal was saved. Paste at least one item, one per line."
		view := &appraiseView{SignedIn: data.LoggedIn, Input: input}
		if userID > 0 {
			view.Saved = app.savedList(ctx, userID)
		} else {
			view.Saved = app.anonSavedList(ctx, app.anonTokens(ctx, r))
		}
		data.Appraise = view
		app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
		return
	}
	if userID > 0 {
		id, _, err := app.saveAppraisal(ctx, userID, false, input, snap)
		if err != nil {
			logging.Errorf("appraisal: save: %v", err)
			data := &PageData{ShellData: app.deps.PublicShell(ctx)}
			data.Error = "Your appraisal was priced, but it could not be saved."
			view := &appraiseView{SignedIn: true, Input: input, Result: app.buildResult(snap)}
			data.Appraise = view
			app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
			return
		}
		app.rememberBasis(ctx, w, r, userID, basis)
		http.Redirect(w, r, "/market/appraise/?a="+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	ownerID, err := app.anonymousOwnerID(ctx)
	if err != nil {
		logging.Errorf("appraisal: anonymous owner: %v", err)
		data := &PageData{ShellData: app.deps.PublicShell(ctx)}
		data.Error = "Your appraisal was priced, but it could not be saved."
		data.Appraise = &appraiseView{SignedIn: false, Input: input, Result: app.buildResult(snap)}
		app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
		return
	}
	_, token, err := app.saveAppraisal(ctx, ownerID, true, input, snap)
	if err != nil {
		logging.Errorf("appraisal: anonymous save: %v", err)
		data := &PageData{ShellData: app.deps.PublicShell(ctx)}
		data.Error = "Your appraisal was priced, but it could not be saved."
		data.Appraise = &appraiseView{SignedIn: false, Input: input, Result: app.buildResult(snap)}
		app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
		return
	}
	app.rememberBasis(ctx, w, r, 0, basis)
	app.rememberAnonToken(ctx, w, r, token)
	http.Redirect(w, r, "/market/appraise/s/"+token+"/", http.StatusSeeOther)
}

// handleAppraiseShare renders one saved snapshot read-only
// from its share token: stored values verbatim, no recompute.
// Viewing an anonymous appraisal refreshes its last-viewed
// time, which is what its 180-day expiry counts from. Its
// creator (account owner, or the history-cookie holder for an
// anonymous appraisal) also gets the delete control.
func (app *page) handleAppraiseShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	token := chi.URLParam(r, "token")
	row, err := app.queries.GetUserAppraisalByShareToken(ctx, token)
	if err != nil {
		data.Error = "That shared appraisal could not be found."
		data.Appraise = &appraiseView{SignedIn: data.LoggedIn}
		app.deps.Render(ctx, w, http.StatusNotFound, "market_appraise.html", data)
		return
	}
	res, err := app.storedResult(row)
	if err != nil {
		logging.Errorf("appraisal: share snapshot: %v", err)
		data.Error = "That shared appraisal could not be read."
		data.Appraise = &appraiseView{SignedIn: data.LoggedIn}
		app.deps.Render(ctx, w, http.StatusInternalServerError, "market_appraise.html", data)
		return
	}
	res.Shared = true
	res.ShareURL = "/market/appraise/s/" + row.ShareToken + "/"
	res.RepriceURL = "/market/appraise/s/" + row.ShareToken + "/reprice"
	if row.IsAnonymous {
		if err := app.queries.TouchUserAppraisalByToken(ctx, token); err != nil {
			logging.Errorf("appraisal: touch %s: %v", token, err)
		}
		if app.hasAnonToken(ctx, r, token) {
			res.CanDelete = true
			res.DeleteURL = "/market/appraise/s/" + token + "/delete"
		}
	} else if userID := app.deps.UserID(ctx); userID > 0 && row.UserID == userID {
		res.CanDelete = true
		res.DeleteURL = "/market/appraise/" + strconv.FormatInt(row.ID, 10) + "/delete"
	}
	data.Appraise = &appraiseView{SignedIn: data.LoggedIn, Result: res}
	app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
}

// handleAppraiseDelete deletes one of the signed-in owner's
// appraisals. Its share link 404s from then on.
func (app *page) handleAppraiseDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 || userID <= 0 {
		http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
		return
	}
	if _, err := app.queries.DeleteUserAppraisal(ctx, db.DeleteUserAppraisalParams{ID: id, UserID: userID}); err != nil {
		logging.Errorf("appraisal: delete %d: %v", id, err)
	}
	http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
}

// handleAppraiseDeleteAnon deletes an anonymous appraisal for
// the history-cookie holder who created it.
func (app *page) handleAppraiseDeleteAnon(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")
	if app.hasAnonToken(ctx, r, token) {
		if _, err := app.queries.DeleteAnonymousAppraisalByToken(ctx, token); err != nil {
			logging.Errorf("appraisal: delete anonymous: %v", err)
		}
		app.forgetAnonToken(ctx, w, r, token)
	}
	http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
}

// storedBasis reads a stored row's recorded basis, default
// when its snapshot predates basis recording.
func (app *page) storedBasis(row db.UserAppraisal) pricingBasis {
	var snap snapshot
	if err := json.Unmarshal(row.Snapshot, &snap); err == nil {
		b := snap.Basis.basis()
		if b.HubName == "" {
			if name, ok := app.deps.MarketRegionName(b.RegionID); ok {
				b.HubName = name
			}
		}
		if b.RegionID != 0 {
			return b
		}
	}
	return app.defaultBasis()
}

// storedExclude reads a stored row's recorded exclusion
// paste, empty when it had none.
func (app *page) storedExclude(row db.UserAppraisal) string {
	var snap snapshot
	if err := json.Unmarshal(row.Snapshot, &snap); err == nil {
		return snap.ExcludeText
	}
	return ""
}

// storedMultiplier reads a stored row's recorded quantity
// multiplier, 1 when it had none.
func (app *page) storedMultiplier(row db.UserAppraisal) int64 {
	var snap snapshot
	if err := json.Unmarshal(row.Snapshot, &snap); err == nil && snap.Multiplier > 1 {
		return snap.Multiplier
	}
	return 1
}

// handleAppraiseReprice re-runs one of the owner's saved
// pastes at current prices, under the basis the form carries
// (the stored basis when it carries none): a new appraisal
// beside the old one, which is never mutated.
func (app *page) handleAppraiseReprice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 || userID <= 0 {
		http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
		return
	}
	row, err := app.queries.GetUserAppraisal(ctx, db.GetUserAppraisalParams{ID: id, UserID: userID})
	if err != nil {
		http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	basis := app.basisFromValues(app.storedBasis(row), r.Form)
	snap, err := app.computeAppraisal(ctx, row.InputText, basis, app.storedExclude(row), app.storedMultiplier(row))
	if err != nil {
		logging.Errorf("appraisal: reprice %d: %v", id, err)
		http.Redirect(w, r, "/market/appraise/?a="+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	newID, _, err := app.saveAppraisal(ctx, userID, false, row.InputText, snap)
	if err != nil {
		logging.Errorf("appraisal: reprice save %d: %v", id, err)
		http.Redirect(w, r, "/market/appraise/?a="+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	app.rememberBasis(ctx, w, r, userID, basis)
	http.Redirect(w, r, "/market/appraise/?a="+strconv.FormatInt(newID, 10), http.StatusSeeOther)
}

// handleAppraiseRepriceToken re-runs a shared appraisal's
// paste under the form's basis as a new appraisal owned by
// whoever asked (their account, or the anonymous owner with
// the history cookie updated). The original is never mutated.
func (app *page) handleAppraiseRepriceToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")
	row, err := app.queries.GetUserAppraisalByShareToken(ctx, token)
	if err != nil {
		http.Redirect(w, r, "/market/appraise/", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	basis := app.basisFromValues(app.storedBasis(row), r.Form)
	snap, err := app.computeAppraisal(ctx, row.InputText, basis, app.storedExclude(row), app.storedMultiplier(row))
	if err != nil {
		logging.Errorf("appraisal: reprice share: %v", err)
		http.Redirect(w, r, "/market/appraise/s/"+token+"/", http.StatusSeeOther)
		return
	}
	userID := app.deps.UserID(ctx)
	if userID > 0 {
		newID, _, err := app.saveAppraisal(ctx, userID, false, row.InputText, snap)
		if err != nil {
			logging.Errorf("appraisal: reprice share save: %v", err)
			http.Redirect(w, r, "/market/appraise/s/"+token+"/", http.StatusSeeOther)
			return
		}
		app.rememberBasis(ctx, w, r, userID, basis)
		http.Redirect(w, r, "/market/appraise/?a="+strconv.FormatInt(newID, 10), http.StatusSeeOther)
		return
	}
	ownerID, err := app.anonymousOwnerID(ctx)
	if err != nil {
		http.Redirect(w, r, "/market/appraise/s/"+token+"/", http.StatusSeeOther)
		return
	}
	_, newToken, err := app.saveAppraisal(ctx, ownerID, true, row.InputText, snap)
	if err != nil {
		logging.Errorf("appraisal: reprice share save: %v", err)
		http.Redirect(w, r, "/market/appraise/s/"+token+"/", http.StatusSeeOther)
		return
	}
	app.rememberBasis(ctx, w, r, 0, basis)
	app.rememberAnonToken(ctx, w, r, newToken)
	http.Redirect(w, r, "/market/appraise/s/"+newToken+"/", http.StatusSeeOther)
}
