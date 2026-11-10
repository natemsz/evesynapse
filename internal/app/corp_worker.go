package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Corporation worker pass: keeps every corp_* snapshot warm for one
// character, keyed by that character's corporation.
//
// Corporation endpoints are role-gated in-game: member tracking,
// assets and corp killmails need Director; orders need Accountant
// or Trader; wallets need Accountant or Junior Accountant;
// structures need Station Manager. A 403 is therefore an expected
// state, not a failure: it is recorded in snapshot_fetch_state
// (state "role_missing" plus the role label), the kind backs off
// for roleMissingBackoff instead of retrying every minute, the
// snapshot cache is left untouched, and the pages render a plain
// "needs the role" state from the record. A refusal that names
// the login instead (a 401, or a 403 naming the scope) settles
// under the shared refusal policy (fetch_refusal.go). Other
// errors are recorded as "error" and the pass moves on; only
// CCP's error limit (420/429) stops it, like every other
// worker pass.
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
	// corpOwnFetchMaxAge bounds how long a member's own successful
	// fetch of a corporation kind lets it copy a fellow member's
	// fresh answer instead of asking ESI: past it the member asks
	// ESI itself, which re-checks its in-game role. It is also the
	// longest a role revoked in-game can keep showing that
	// member's corporation data, so it stays at fifteen minutes.
	corpOwnFetchMaxAge = 15 * time.Minute
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
	granted := scopeSet(ch.Scopes)
	for _, kind := range corpSnapshotKinds() {
		if ctx.Err() != nil {
			return refreshed
		}
		if kindExcludedByGrants(granted, kind) {
			continue // the login never granted this kind's scope: not planned
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
					logging.Infof("worker: character %d moved from corporation %d to %d; restarting corporation pass", ch.CharacterID, corpID, newID)
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
// due, recording the outcome. A fresh snapshot or a recorded
// refusal still in its window skips the ESI call entirely.
func (app *Application) fetchCorpKind(ctx context.Context, ch db.Character, corpID int64, kind string) corpFetchOutcome {
	snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	switch {
	case serr == nil && esi.SnapshotFresh(snap):
		return corpFetchSkipped
	case serr != nil && !errors.Is(serr, pgx.ErrNoRows):
		logging.Errorf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	// A recorded refusal backs this kind off instead of retrying
	// every minute; the pages keep showing the refusal state from
	// the record in the meantime (fetch_refusal.go).
	if app.refusalSettled(ctx, ch.CharacterID, kind) {
		return corpFetchSkipped
	}

	// Another member of the corporation may already hold this
	// window's answer: copy it rather than ask ESI again.
	if app.copySiblingCorpSnapshot(ctx, ch, corpID, kind) {
		return corpFetchOK
	}

	if err := app.esi.FetchAndStoreCorpSnapshot(ctx, ch, corpID, kind); err != nil {
		switch {
		case errors.Is(err, esi.ErrErrorLimit):
			logging.Warnf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
			return corpFetchLimited
		case scopeNamedRefusal(err):
			// A 401 (or a 403 naming the scope) is the login's
			// grant, not an in-game role: it settles under the
			// shared refusal policy.
			app.recordFetchRefusal(ctx, ch.CharacterID, kind, err)
			return corpFetchFailed
		case esi.IsForbidden(err):
			role := corpKindRole(kind)
			if role == "" {
				role = "corporation membership"
			}
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateRoleMissing, corpKindRole(kind))
			logging.Infof("worker: %s for character %d refused by ESI (403); recorded as needing the %s role in-game", kind, ch.CharacterID, role)
			return corpFetchRoleMissing
		default:
			logging.Errorf("worker: refresh %s for corporation %d (character %d): %v", kind, corpID, ch.CharacterID, err)
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, err.Error())
			return corpFetchFailed
		}
	}
	app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateOK, "")
	return corpFetchOK
}

// copySiblingCorpSnapshot stores, as ch's own, a corporation dataset
// another member of ch's corporation fetched inside its current ESI
// cache window: every member's corporation endpoints answer the same
// corp-level data, so asking once per window is enough. Access stays
// per member, and ESI keeps deciding it: ch may copy only while its
// own last ESI fetch of the kind succeeded within corpOwnFetchMaxAge,
// so a member who lost the role in-game, or left the corporation,
// hits ESI's 403 on its next own fetch within that bound and stops
// seeing the data. The public corporation sheet (SnapCorpInfo)
// carries no access to prove and copies freely. Reports whether it
// copied.
func (app *Application) copySiblingCorpSnapshot(ctx context.Context, ch db.Character, corpID int64, kind string) bool {
	// The corporation's public sheet is readable by anyone, so no
	// access needs proving to copy it. Every other kind is read by
	// role or by membership, which leaving the corporation revokes.
	if kind != esi.SnapCorpInfo {
		state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: kind})
		if err != nil || state.State != fetchStateOK || time.Since(state.AttemptedAt) >= corpOwnFetchMaxAge {
			return false
		}
	}
	now := time.Now().UTC()
	sib, err := app.queries.GetFreshSiblingCorpSnapshot(ctx, db.GetFreshSiblingCorpSnapshotParams{
		CorporationID: corpID, Kind: kind, CharacterID: ch.CharacterID, Now: now,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("worker: read sibling %s snapshot for corporation %d: %v", kind, corpID, err)
		}
		return false
	}
	if err := app.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: ch.CharacterID, Kind: kind, Payload: sib.Payload,
		FetchedAt: sib.FetchedAt, CachedUntil: sib.CachedUntil, Etag: sib.Etag,
	}); err != nil {
		logging.Errorf("worker: copy %s snapshot to character %d: %v", kind, ch.CharacterID, err)
		return false
	}
	app.esi.NoteCharacterChanged(ch.CharacterID)
	return true
}

// recordCorpFetchState upserts one fetch-outcome row (best-effort:
// a logging failure must not break the pass).
func (app *Application) recordCorpFetchState(ctx context.Context, characterID int64, kind, state, detail string) {
	if err := app.queries.UpsertSnapshotFetchState(ctx, db.UpsertSnapshotFetchStateParams{
		CharacterID: characterID,
		Kind:        kind,
		State:       state,
		Detail:      detail,
		AttemptedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("worker: record fetch state %s/%s for character %d: %v", kind, state, characterID, err)
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
		if time.Since(row.UpdatedAt) < corpMappingMaxAge {
			return row.CorporationID, true
		}
		if newID, ok := app.corpMappingFromProfile(ctx, ch); ok {
			return newID, true
		}
		if newID, ok := app.refreshCorpMapping(ctx, ch); ok {
			return newID, true
		}
		return row.CorporationID, true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("worker: read corporation mapping for character %d: %v", ch.CharacterID, err)
	}
	return app.refreshCorpMapping(ctx, ch)
}

// corpMappingFromProfile renews the mapping from the character's
// stored profile snapshot while it is inside its ESI cache window:
// the core pass already fetched the same /characters/{id}/ sheet,
// so the mapping needs no call of its own.
func (app *Application) corpMappingFromProfile(ctx context.Context, ch db.Character) (int64, bool) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapProfile})
	if err != nil || !esi.SnapshotFresh(snap) {
		return 0, false
	}
	var profile esi.Character
	if json.Unmarshal([]byte(snap.Payload), &profile) != nil || profile.CorporationID <= 0 {
		return 0, false
	}
	if err := app.queries.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID: ch.CharacterID, CorporationID: profile.CorporationID, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("worker: store corporation mapping for character %d: %v", ch.CharacterID, err)
	}
	return profile.CorporationID, true
}

// refreshCorpMapping re-reads the character's corporation from the
// public character sheet (no token needed) and stores it. It
// reports the corporation ID and whether it is trustworthy.
func (app *Application) refreshCorpMapping(ctx context.Context, ch db.Character) (int64, bool) {
	var pub esi.Character
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", ch.CharacterID), &pub); err != nil {
		if !errors.Is(err, esi.ErrErrorLimit) && ctx.Err() == nil {
			logging.Errorf("worker: resolve corporation for character %d: %v", ch.CharacterID, err)
		}
		return 0, false
	}
	if pub.CorporationID <= 0 {
		return 0, false
	}
	if err := app.queries.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID:   ch.CharacterID,
		CorporationID: pub.CorporationID,
		UpdatedAt:     time.Now().UTC(),
	}); err != nil {
		logging.Errorf("worker: store corporation mapping for character %d: %v", ch.CharacterID, err)
	}
	return pub.CorporationID, true
}

// seedStructurePlaceNames stores the corp's structure names from
// the structures snapshot into the ESI client's place cache, so
// location titles elsewhere (orders, tracking) can use them. The
// snapshot's facts also land in the durable structure_context
// store (and its names in structure_names) behind the structure
// page.
func (app *Application) seedStructurePlaceNames(ctx context.Context, ch db.Character) {
	var structures esi.CorpStructures
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpStructures, &structures) {
		return
	}
	app.persistStructureContexts(ctx, structures)
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
// outcomes are recorded under the corp_asset_names pseudo-kind so
// a refusal, or a 404 batch rejection, backs off instead of
// retrying every minute. CCP reports "None" for unnamed items,
// those are skipped (and retried next time the item still lacks
// a name, which costs nothing while the cycle cap holds).
func (app *Application) warmCorpAssetNames(ctx context.Context, ch db.Character, corpID int64) {
	var items []esi.Asset
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpAssets, &items) {
		return // no assets snapshot yet; nothing to name
	}

	if kindExcludedByGrants(scopeSet(ch.Scopes), esi.SnapCorpAssets) {
		return // the login never granted the corp assets scope: not planned
	}
	// Respect a recent refusal or failure on the names endpoint
	// itself. A 403 means the Director role is missing; a 404
	// (ESI rejects the whole batch when even one submitted item
	// no longer resolves, e.g. a snapshot outlived its assets)
	// won't heal by retrying every minute either. Either way the
	// next attempt waits out the usual backoff, which also keeps
	// the failure log to one line per backoff window. A recorded
	// refusal (401, or a 403 naming the scope) settles under the
	// shared policy's longer window (fetch_refusal.go).
	if app.refusalSettled(ctx, ch.CharacterID, corpAssetNamesKind) {
		return
	}
	if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: corpAssetNamesKind}); err == nil &&
		(state.State == fetchStateRoleMissing || state.State == fetchStateError) {
		if time.Since(state.AttemptedAt) < roleMissingBackoff {
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
		} else if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("worker: read item name %d: %v", it.ItemID, err)
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

	splits := corpAssetNameSplitCalls
	names, invalid, err := app.fetchCorpAssetNamesIsolating(ctx, ch, corpID, missing, &splits)
	// An item ESI rejects on its own is settled with an empty name,
	// the unnamed marker every reader of item_names already skips
	// (the character pass stores unnamed items the same way): its
	// batch stops failing whole on every pass.
	for _, id := range invalid {
		if serr := app.queries.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: id, Name: ""}); serr != nil {
			logging.Errorf("worker: settle invalid item %d: %v", id, serr)
		}
	}
	if len(invalid) > 0 {
		logging.Infof("worker: corp asset names for corporation %d: %d item(s) ESI no longer resolves, settled as unnamed", corpID, len(invalid))
	}
	if err != nil {
		switch {
		case scopeNamedRefusal(err):
			app.recordFetchRefusal(ctx, ch.CharacterID, corpAssetNamesKind, err)
		case esi.IsForbidden(err):
			app.recordCorpFetchState(ctx, ch.CharacterID, corpAssetNamesKind, fetchStateRoleMissing, corpKindRole(esi.SnapCorpAssets))
			logging.Infof("worker: corp asset names for corporation %d refused by ESI (403); needs the %s role in-game", corpID, corpKindRole(esi.SnapCorpAssets))
		case errors.Is(err, esi.ErrErrorLimit):
			logging.Warnf("worker: ESI error limit hit warming corp asset names for corporation %d", corpID)
		default:
			logging.Errorf("worker: corp asset names for corporation %d: %v", corpID, err)
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
			logging.Errorf("worker: store item name %d: %v", n.ItemID, err)
			continue
		}
		stored++
	}
	if stored > 0 {
		logging.Infof("worker: stored %d corp asset name(s) for corporation %d", stored, corpID)
	}
}

// corpAssetNameSplitCalls bounds the extra names calls one pass may
// spend finding the items that fail a batch: about two per halving,
// so one bad item in a full batch of 1,000 is found well inside it.
const corpAssetNameSplitCalls = 24

// fetchCorpAssetNamesIsolating asks for the names of ids. ESI
// rejects the whole batch with a 404 when even one item no longer
// resolves, so a rejected batch is split in halves and each asked
// again, down to the items that fail alone, which come back in
// invalid. The splitting spends from *splits; once that is gone the
// rejection is returned as it came.
func (app *Application) fetchCorpAssetNamesIsolating(ctx context.Context, ch db.Character, corpID int64, ids []int64, splits *int) (names []esi.AssetName, invalid []int64, err error) {
	names, err = app.esi.FetchCorpAssetNames(ctx, ch, corpID, ids)
	if err == nil {
		return names, nil, nil
	}
	if code, ok := esi.StatusCode(err); !ok || code != http.StatusNotFound {
		return nil, nil, err
	}
	if len(ids) == 1 {
		return nil, ids, nil
	}
	if *splits < 2 {
		return nil, nil, err
	}
	*splits -= 2
	mid := len(ids) / 2
	for _, half := range [][]int64{ids[:mid], ids[mid:]} {
		n, bad, herr := app.fetchCorpAssetNamesIsolating(ctx, ch, corpID, half, splits)
		names, invalid = append(names, n...), append(invalid, bad...)
		if herr != nil {
			return names, invalid, herr
		}
	}
	return names, invalid, nil
}

// warmCorpKillmailDetails warms killmail detail payloads for the
// corporation's recent list into the shared killmail_details
// store, same bound and contract as the character pass.
func (app *Application) warmCorpKillmailDetails(ctx context.Context, ch db.Character) (fetched int, limited bool) {
	return app.warmKillmailDetailsFor(ctx, ch, esi.SnapCorpKillmails)
}
