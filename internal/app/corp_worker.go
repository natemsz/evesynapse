package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Corporation worker pass: keeps every corp_* snapshot warm for one
// character, keyed by that character's corporation (module sweep,
// cluster 2).
//
// Corporation endpoints are role-gated in-game: member tracking,
// assets and corp killmails need Director; orders need Accountant
// or Trader; wallets need Accountant or Junior Accountant;
// structures need Station Manager. A 403 is therefore an expected
// state, not a failure: it is recorded in snapshot_fetch_state
// (state "role_missing" plus the role label), the kind backs off
// for roleMissingBackoff instead of retrying every minute, the
// snapshot cache is left untouched, and the pages render a plain
// "needs the role" state from the record. Other errors are
// recorded as "error" and the pass moves on; only CCP's error
// limit (420/429) stops it, like every other worker pass.
// ---------------------------------------------------------------------------

const (
	// corpMappingMaxAge bounds how long the cached character to
	// corporation mapping is trusted before the worker re-reads
	// the public character sheet (characters move corps; pages key
	// corp data by the mapping).
	corpMappingMaxAge = 24 * time.Hour
	// roleMissingBackoff is how long a recorded 403 keeps a kind
	// out of the fetch rotation. Roles change slowly, and a
	// character granted a role in-game sees data within hours,
	// not a minute.
	roleMissingBackoff = 6 * time.Hour
	// maxCorpAssetNameItemsPerCycle caps the singleton-name POST
	// batches per character per cycle (ESI accepts 1,000 IDs per
	// call, so one cycle is at most one call).
	maxCorpAssetNameItemsPerCycle = 1000
)

// corpFetchOutcome is one corporation kind's result for a cycle.
type corpFetchOutcome int

const (
	corpFetchOK          corpFetchOutcome = iota
	corpFetchSkipped                      // fresh snapshot, or role-missing backoff active
	corpFetchRoleMissing                  // ESI answered 403
	corpFetchFailed                       // transient error, recorded; the pass continues
	corpFetchLimited                      // ESI error limit: the caller must stop
)

// refreshCorpSnapshots runs the corporation pass for one character
// and reports how many snapshots it stored.
func (app *Application) refreshCorpSnapshots(ctx context.Context, ch db.Character) int {
	corpID, ok := app.corpIDForCharacter(ctx, ch)
	if !ok {
		return 0
	}
	return app.refreshCorpSnapshotsFor(ctx, ch, corpID, true)
}

// refreshCorpSnapshotsFor is refreshCorpSnapshots against a known
// corporation. mayRetryMembers allows one stale-mapping retry (see
// the members case below).
func (app *Application) refreshCorpSnapshotsFor(ctx context.Context, ch db.Character, corpID int64, mayRetryMembers bool) int {
	refreshed := 0
	for _, kind := range corpSnapshotKinds() {
		if ctx.Err() != nil {
			return refreshed
		}
		outcome := app.fetchCorpKind(ctx, ch, corpID, kind)
		switch outcome {
		case corpFetchOK:
			refreshed++
			if kind == esi.SnapCorpStructures {
				// The structures payload names the structures:
				// seed the place cache so assets/orders elsewhere
				// in the corp can title them.
				app.seedStructurePlaceNames(ctx, ch)
			}
		case corpFetchLimited:
			return refreshed
		case corpFetchRoleMissing:
			// The members list takes no role. A 403 there means
			// the recorded corporation is stale, not that a role
			// is missing: re-resolve the mapping once and, if the
			// corporation changed, restart against the new one.
			if kind == esi.SnapCorpMembers && mayRetryMembers {
				if newID, ok := app.refreshCorpMapping(ctx, ch); ok && newID != corpID {
					log.Printf("worker: character %d moved from corporation %d to %d; restarting corporation pass", ch.CharacterID, corpID, newID)
					return refreshed + app.refreshCorpSnapshotsFor(ctx, ch, newID, false)
				}
			}
		}
	}

	// Singleton names for the assets we just (or earlier) stored.
	app.warmCorpAssetNames(ctx, ch, corpID)
	return refreshed
}

// fetchCorpKind refreshes one corporation snapshot kind when it is
// due, recording the outcome. A fresh snapshot or an active
// role-missing backoff skips the ESI call entirely.
func (app *Application) fetchCorpKind(ctx context.Context, ch db.Character, corpID int64, kind string) corpFetchOutcome {
	snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	switch {
	case serr == nil && esi.SnapshotFresh(snap):
		return corpFetchSkipped
	case serr != nil && !errors.Is(serr, sql.ErrNoRows):
		log.Printf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	// A recorded role refusal backs this kind off instead of
	// retrying every minute; the pages keep showing the role
	// state from the record in the meantime.
	if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: kind}); err == nil && state.State == fetchStateRoleMissing {
		if attempted, perr := time.Parse(time.RFC3339, state.AttemptedAt); perr == nil && time.Since(attempted) < roleMissingBackoff {
			return corpFetchSkipped
		}
	}

	if _, err := app.esi.FetchAndStoreCorpSnapshot(ctx, ch, corpID, kind); err != nil {
		switch {
		case errors.Is(err, esi.ErrErrorLimit):
			log.Printf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
			return corpFetchLimited
		case esi.IsForbidden(err):
			role := corpKindRole(kind)
			if role == "" {
				role = "corporation membership"
			}
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateRoleMissing, corpKindRole(kind))
			log.Printf("worker: %s for character %d refused by ESI (403); recorded as needing the %s role in-game", kind, ch.CharacterID, role)
			return corpFetchRoleMissing
		default:
			log.Printf("worker: refresh %s for corporation %d (character %d): %v", kind, corpID, ch.CharacterID, err)
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, err.Error())
			return corpFetchFailed
		}
	}
	app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateOK, "")
	return corpFetchOK
}

// recordCorpFetchState upserts one fetch-outcome row (best-effort:
// a logging failure must not break the pass).
func (app *Application) recordCorpFetchState(ctx context.Context, characterID int64, kind, state, detail string) {
	if err := app.queries.UpsertSnapshotFetchState(ctx, db.UpsertSnapshotFetchStateParams{
		CharacterID: characterID,
		Kind:        kind,
		State:       state,
		Detail:      detail,
		AttemptedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("worker: record fetch state %s/%s for character %d: %v", kind, state, characterID, err)
	}
}

// corpIDForCharacter resolves the character's corporation from the
// local mapping, refreshing it from the public character sheet
// when missing or older than corpMappingMaxAge. The stored value
// survives a failed refresh (a transient error shouldn't blank the
// corporation pages).
func (app *Application) corpIDForCharacter(ctx context.Context, ch db.Character) (int64, bool) {
	row, err := app.queries.GetCharacterCorporation(ctx, ch.CharacterID)
	if err == nil {
		if updated, perr := time.Parse(time.RFC3339, row.UpdatedAt); perr == nil && time.Since(updated) < corpMappingMaxAge {
			return row.CorporationID, true
		}
		if newID, ok := app.refreshCorpMapping(ctx, ch); ok {
			return newID, true
		}
		return row.CorporationID, true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("worker: read corporation mapping for character %d: %v", ch.CharacterID, err)
	}
	return app.refreshCorpMapping(ctx, ch)
}

// refreshCorpMapping re-reads the character's corporation from the
// public character sheet (no token needed) and stores it. It
// reports the corporation ID and whether it is trustworthy.
func (app *Application) refreshCorpMapping(ctx context.Context, ch db.Character) (int64, bool) {
	var pub esi.Character
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", ch.CharacterID), &pub); err != nil {
		if !errors.Is(err, esi.ErrErrorLimit) && ctx.Err() == nil {
			log.Printf("worker: resolve corporation for character %d: %v", ch.CharacterID, err)
		}
		return 0, false
	}
	if pub.CorporationID <= 0 {
		return 0, false
	}
	if err := app.queries.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID:   ch.CharacterID,
		CorporationID: pub.CorporationID,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("worker: store corporation mapping for character %d: %v", ch.CharacterID, err)
	}
	return pub.CorporationID, true
}

// seedStructurePlaceNames stores the corp's structure names from
// the structures snapshot into the ESI client's place cache, so
// location titles elsewhere (orders, tracking) can use them.
func (app *Application) seedStructurePlaceNames(ctx context.Context, ch db.Character) {
	var structures esi.CorpStructures
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpStructures, &structures) {
		return
	}
	for _, s := range structures {
		if s.Name != "" {
			app.esi.StorePlaceName(s.StructureID, s.Name)
		}
	}
}

// warmCorpAssetNames resolves player-given names for the corp's
// singleton asset items (fitted ships, renamed containers) via
// POST /corporations/{id}/assets/names/, at most
// maxCorpAssetNameItemsPerCycle unresolved items per cycle. The
// names POST needs the same Director role as the assets list;
// outcomes are recorded under the corp_asset_names pseudo-kind so a
// refusal backs off instead of retrying every minute. CCP reports
// "None" for unnamed items — those are skipped (and retried next
// time the item still lacks a name, which costs nothing while the
// cycle cap holds).
func (app *Application) warmCorpAssetNames(ctx context.Context, ch db.Character, corpID int64) {
	var items []esi.Asset
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpAssets, &items) {
		return // no assets snapshot yet; nothing to name
	}

	// Respect a recent role refusal on the names endpoint itself.
	if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: corpAssetNamesKind}); err == nil && state.State == fetchStateRoleMissing {
		if attempted, perr := time.Parse(time.RFC3339, state.AttemptedAt); perr == nil && time.Since(attempted) < roleMissingBackoff {
			return
		}
	}

	seen := make(map[int64]bool)
	var missing []int64
	for _, it := range items {
		if !it.IsSingleton || it.Quantity != 1 || seen[it.ItemID] {
			continue
		}
		seen[it.ItemID] = true
		if _, err := app.queries.GetItemName(ctx, it.ItemID); err == nil {
			continue // already named
		} else if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("worker: read item name %d: %v", it.ItemID, err)
			continue
		}
		missing = append(missing, it.ItemID)
	}
	if len(missing) == 0 {
		return
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	if len(missing) > maxCorpAssetNameItemsPerCycle {
		missing = missing[:maxCorpAssetNameItemsPerCycle]
	}

	names, err := app.esi.FetchCorpAssetNames(ctx, ch, corpID, missing)
	if err != nil {
		switch {
		case esi.IsForbidden(err):
			app.recordCorpFetchState(ctx, ch.CharacterID, corpAssetNamesKind, fetchStateRoleMissing, corpKindRole(esi.SnapCorpAssets))
			log.Printf("worker: corp asset names for corporation %d refused by ESI (403); needs the %s role in-game", corpID, corpKindRole(esi.SnapCorpAssets))
		case errors.Is(err, esi.ErrErrorLimit):
			log.Printf("worker: ESI error limit hit warming corp asset names for corporation %d", corpID)
		default:
			log.Printf("worker: corp asset names for corporation %d: %v", corpID, err)
			app.recordCorpFetchState(ctx, ch.CharacterID, corpAssetNamesKind, fetchStateError, err.Error())
		}
		return
	}
	app.recordCorpFetchState(ctx, ch.CharacterID, corpAssetNamesKind, fetchStateOK, "")

	stored := 0
	for _, n := range names {
		if n.Name == "" || n.Name == "None" {
			continue // CCP's placeholder for an unnamed item
		}
		if err := app.queries.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: n.ItemID, Name: n.Name}); err != nil {
			log.Printf("worker: store item name %d: %v", n.ItemID, err)
			continue
		}
		stored++
	}
	if stored > 0 {
		log.Printf("worker: stored %d corp asset name(s) for corporation %d", stored, corpID)
	}
}

// warmCorpKillmailDetails warms killmail detail payloads for the
// corporation's recent list into the shared killmail_details
// store, same bound and contract as the character pass.
func (app *Application) warmCorpKillmailDetails(ctx context.Context, ch db.Character) (fetched int, limited bool) {
	return app.warmKillmailDetailsFor(ctx, ch, esi.SnapCorpKillmails)
}
