package app

// The worker's side of the public pilot page (pilot.go): draining the
// pilot_records queue from ESI's public endpoints, resolving the
// pilot names the search box is waiting on, and noting a record for
// everyone the deployment's own data mentions so their page is warm
// before anyone follows the name. Nothing here renders anything.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// refreshPilotRecords drains the pilot queue inside the cycle
// budget: 'pending' rows first, then ready rows gone stale.
// Returns how many records it settled and whether ESI's error
// limit stopped the pass. Called from refreshCycle.
func (app *Application) refreshPilotRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()

	now := time.Now().UTC()
	ids, err := app.queries.ListPilotDrains(ctx, db.ListPilotDrainsParams{
		StaleCutoff: now.Add(-pilotStaleAfter),
		DrainLimit:  maxPilotDrainsPerCycle,
	})
	if err != nil {
		logging.Errorf("worker: pilot records: list drains: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		settled, ltd := app.drainPilotRecord(ctx, id, allowance, now)
		if ltd {
			return drained, true
		}
		if settled {
			drained++
		}
	}
	return drained, false
}

// refreshPilotNameWants resolves due pilot name wants (topbar
// searches for names no local tier knew) inside the cycle
// allowance. Each due name costs one public POST /universe/ids/
// lookup; an exact character match queues that character's pilot
// record at viewed priority, so the ordinary pilot drain — in
// the same pass — fills the record the search is waiting on. A
// name with no exact character match settles as 'missing' and is
// never asked about again; transient failures back off instead
// of being re-asked on every cycle. Holds the shared fetch lock
// like the other pilot passes.
func (app *Application) refreshPilotNameWants(ctx context.Context, allowance *fetchBudget) (resolved int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()
	return app.drainPilotNameWants(ctx, allowance)
}

// drainPilotNameWants is refreshPilotNameWants for callers that
// already hold the shared fetch lock (the urgent drain).
func (app *Application) drainPilotNameWants(ctx context.Context, allowance *fetchBudget) (resolved int, limited bool) {
	now := time.Now().UTC()
	wants, err := app.queries.ListDuePilotNameWants(ctx, db.ListDuePilotNameWantsParams{
		Now: now, Lim: maxPilotNameResolutions,
	})
	if err != nil {
		logging.Errorf("worker: pilot name wants: list due: %v", err)
		return 0, false
	}
	for _, want := range wants {
		if ctx.Err() != nil || !allowance.take() {
			break
		}
		var ids esi.UniverseIDs
		err := app.esi.PostJSON(ctx, "/universe/ids/", []string{want.DisplayName}, &ids)
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: pilot name wants: ESI error limit resolving %q; backing off", want.DisplayName)
			return resolved, true
		}
		if err != nil {
			if code, has := esi.StatusCode(err); has && (code == http.StatusBadRequest || code == http.StatusNotFound) {
				if serr := app.queries.SetPilotNameWantMissing(ctx, db.SetPilotNameWantMissingParams{
					ResolvedAt: timeSet(now), NormalizedName: want.NormalizedName,
				}); serr != nil {
					logging.Errorf("worker: pilot name wants: settle miss %q: %v", want.DisplayName, serr)
					continue
				}
				resolved++
				continue
			}
			if serr := app.queries.SetPilotNameWantError(ctx, db.SetPilotNameWantErrorParams{
				ResolvedAt:     timeSet(now),
				NextTryAt:      timeSet(now.Add(pilotNameWantRetryDelay)),
				NormalizedName: want.NormalizedName,
			}); serr != nil {
				logging.Errorf("worker: pilot name wants: record error %q: %v", want.DisplayName, serr)
			}
			logging.Errorf("worker: pilot name wants: resolve %q: %v", want.DisplayName, err)
			continue
		}
		var match *esi.UniverseIDEntry
		for i := range ids.Characters {
			if strings.EqualFold(ids.Characters[i].Name, want.DisplayName) {
				match = &ids.Characters[i]
				break
			}
		}
		if match == nil {
			if serr := app.queries.SetPilotNameWantMissing(ctx, db.SetPilotNameWantMissingParams{
				ResolvedAt: timeSet(now), NormalizedName: want.NormalizedName,
			}); serr != nil {
				logging.Errorf("worker: pilot name wants: settle miss %q: %v", want.DisplayName, serr)
				continue
			}
			resolved++
			continue
		}
		app.esi.StoreCharacterName(match.ID, match.Name)
		if qerr := app.queries.UpsertPilotWant(ctx, match.ID); qerr != nil {
			logging.Errorf("worker: pilot name wants: queue pilot %d for %q: %v", match.ID, want.DisplayName, qerr)
			continue
		}
		if serr := app.queries.SetPilotNameWantReady(ctx, db.SetPilotNameWantReadyParams{
			CharacterID: match.ID, ResolvedAt: timeSet(now), NormalizedName: want.NormalizedName,
		}); serr != nil {
			logging.Errorf("worker: pilot name wants: settle %q: %v", want.DisplayName, serr)
			continue
		}
		resolved++
	}
	return resolved, false
}

// drainPilotRecord assembles and stores one pilot's public record
// from ESI's public endpoints (no token involved anywhere). A 404
// on the profile settles the record as 'missing'.
func (app *Application) drainPilotRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {

	if !allowance.take() {
		return false, false
	}
	var profile esi.Character
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", id), &profile); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: pilot records: ESI error limit hit resolving pilot %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
			if serr := app.queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
				CharacterID: id, Payload: "", State: pilotStateMissing, FetchedAt: timeSet(now),
			}); serr != nil {
				logging.Errorf("worker: pilot records: record miss for %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		logging.Errorf("worker: pilot records: fetch profile %d: %v", id, err)
		return false, false
	}
	app.esi.StoreCharacterName(id, profile.Name)

	if !allowance.take() {
		return false, false
	}
	history := esi.CorpHistory{}
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/corporationhistory/", id), &history); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: pilot records: ESI error limit hit fetching history for %d; backing off until next cycle", id)
			return false, true
		}
		// History failing is not fatal to the record: store the
		// profile with whatever history we could not get as empty.
		logging.Warnf("worker: pilot records: fetch history %d: %v (storing profile only)", id, err)
		history = esi.CorpHistory{}
	}

	payload := pilotPayload{Profile: profile}

	// Current corporation (name + ticker), alliance, faction —
	// each is one public GET, skipped when the profile has none.
	if profile.CorporationID > 0 && allowance.take() {
		var corp esi.Corporation
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", profile.CorporationID), &corp); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			logging.Errorf("worker: pilot records: fetch corp %d for pilot %d: %v", profile.CorporationID, id, err)
		} else {
			payload.Corp = corp
			app.esi.StoreCorpName(profile.CorporationID, corp.Name)
		}
	}
	if profile.AllianceID > 0 && allowance.take() {
		var alliance esi.Alliance
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", profile.AllianceID), &alliance); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			logging.Errorf("worker: pilot records: fetch alliance %d for pilot %d: %v", profile.AllianceID, id, err)
		} else {
			payload.Alliance = alliance
			app.esi.StoreAllianceName(profile.AllianceID, alliance.Name)
		}
	}
	if profile.FactionID > 0 && allowance.take() {
		var factions []esi.Faction
		if err := app.esi.Get(ctx, "", "/universe/factions/", &factions); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			logging.Errorf("worker: pilot records: fetch factions for pilot %d: %v", id, err)
		} else {
			for _, f := range factions {
				if f.FactionID == profile.FactionID {
					payload.FactionName = f.Name
					break
				}
			}
		}
	}

	// Employment history: oldest first, each stint ending where
	// the next begins. Corp names resolve from the name cache
	// first; the worker spends a few extra fetches on the rest.
	stints := make([]esi.CorpHistoryEntry, len(history))
	copy(stints, history)
	sort.SliceStable(stints, func(i, j int) bool { return stints[i].StartDate < stints[j].StartDate })
	historyNameFetches := 0
	for i, stint := range stints {
		row := pilotHistoryRow{CorpID: stint.CorporationID, Start: stint.StartDate}
		if i+1 < len(stints) {
			row.End = stints[i+1].StartDate
		}
		if name, ok := app.esi.CachedCorpName(stint.CorporationID); ok {
			row.CorpName = name
		} else if stint.CorporationID > 0 && historyNameFetches < maxHistoryCorpNamesPerDrain && allowance.take() {
			historyNameFetches++
			var corp esi.Corporation
			if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", stint.CorporationID), &corp); err != nil {
				if errors.Is(err, esi.ErrErrorLimit) {
					return false, true
				}
				logging.Errorf("worker: pilot records: fetch history corp %d for pilot %d: %v", stint.CorporationID, id, err)
			} else {
				row.CorpName = corp.Name
				app.esi.StoreCorpName(stint.CorporationID, corp.Name)
			}
		}
		payload.History = append(payload.History, row)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		logging.Errorf("worker: pilot records: encode payload for %d: %v", id, err)
		return false, false
	}
	if err := app.queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: id, Payload: string(encoded), State: pilotStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("worker: pilot records: store record for %d: %v", id, err)
		return false, false
	}
	return true, false
}

// notePilotOrbit proactively notes pilot records for every
// character who appears in the deployment's own data — wallet
// counterparties, contract parties, contacts, mail senders and
// recipients, corporation rosters, corp wallet clients and
// parties — so their /pilot/ page is already warm when a user
// follows a name. Killmail participants are deliberately absent:
// kill contexts link to zKillboard, never here. Records already
// noted or settled are skipped, the user's own characters are
// skipped (their names go to the full sheet), and noting is
// bounded per cycle; the drain order (viewed wants first, then
// the orbit) lives in the drain query. Notes only — no fetches
// here. Called from refreshCycle ahead of the pilot drain.
func (app *Application) notePilotOrbit(ctx context.Context) {
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		logging.Errorf("worker: pilot orbit: list characters: %v", err)
		return
	}
	own := make(map[int64]bool, len(characters))
	for _, ch := range characters {
		own[ch.CharacterID] = true
	}
	have := make(map[int64]bool)
	recorded, err := app.queries.ListPilotRecordIDs(ctx)
	if err != nil {
		logging.Errorf("worker: pilot orbit: list records: %v", err)
		return
	}
	for _, id := range recorded {
		have[id] = true
	}

	found := make(map[int64]bool)
	for _, ch := range characters {
		app.pilotCounterpartyIDs(ctx, ch.CharacterID, found)
	}
	ids := make([]int64, 0, len(found))
	for id := range found {
		if own[id] || have[id] {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	noted := 0
	for _, id := range ids {
		if noted >= maxOrbitPilotsPerCycle {
			break
		}
		if err := app.queries.InsertPilotOrbitWant(ctx, id); err != nil {
			logging.Errorf("worker: pilot orbit: note %d: %v", id, err)
			continue
		}
		noted++
	}
	if noted > 0 {
		logging.Infof("worker: pilot orbit: noted %d new counterparty records", noted)
	}
}

// pilotCounterpartyIDs adds the character IDs found in one
// character's ledger, contract, contact, mail, and roster
// snapshots to out. Journal parties route by ESI's party_type
// (corporations and alliances note org wants instead -- they are
// not pilots and never reach the character endpoint); sources
// that carry no kind (transaction clients, contract parties)
// keep the >= 90M plausibility guess, and the client's negative
// cache bounds a wrong guess to one lookup per ID.
func (app *Application) pilotCounterpartyIDs(ctx context.Context, characterID int64, out map[int64]bool) {
	add := func(id int64) {
		if id >= 90_000_000 {
			out[id] = true
		}
	}
	// Contacts carry an explicit kind, so a character contact is
	// a character even with a pre-90M ID (the oldest pilots) —
	// those names must warm too, not just ledger counterparties.
	addAny := func(id int64) {
		if id > 0 {
			out[id] = true
		}
	}
	corps, alliances := map[int64]bool{}, map[int64]bool{}
	if journal, ok := loadSnapshot[esi.WalletJournal](app, ctx, characterID, esi.SnapWalletJournal); ok {
		for _, e := range journal {
			routeJournalParty(out, corps, alliances, e.FirstPartyID, e.FirstPartyType)
			routeJournalParty(out, corps, alliances, e.SecondPartyID, e.SecondPartyType)
		}
	}
	if txns, ok := loadSnapshot[esi.WalletTransactions](app, ctx, characterID, esi.SnapWalletTxns); ok {
		for _, t := range txns {
			add(t.ClientID)
		}
	}
	if contracts, ok := loadSnapshot[esi.Contracts](app, ctx, characterID, esi.SnapContracts); ok {
		for _, c := range contracts {
			add(c.IssuerID)
			add(c.AssigneeID)
			add(c.AcceptorID)
		}
	}
	if contacts, ok := loadSnapshot[esi.Contacts](app, ctx, characterID, esi.SnapContacts); ok {
		for _, c := range contacts {
			if c.ContactType == "character" {
				addAny(c.ContactID)
			}
		}
	}
	if headers, ok := loadSnapshot[esi.MailHeaders](app, ctx, characterID, esi.SnapMail); ok {
		for _, h := range headers {
			add(h.From)
			for _, rcpt := range h.Recipients {
				if rcpt.RecipientType == "character" {
					add(rcpt.RecipientID)
				}
			}
		}
	}
	if members, ok := loadSnapshot[esi.CorpMembers](app, ctx, characterID, esi.SnapCorpMembers); ok {
		for _, id := range members {
			add(id)
		}
	}
	for division := int64(1); division <= 7; division++ {
		if journal, ok := loadSnapshot[esi.CorpJournal](app, ctx, characterID, esi.CorpJournalKind(division)); ok {
			for _, e := range journal {
				routeJournalParty(out, corps, alliances, e.FirstPartyID, e.FirstPartyType)
				routeJournalParty(out, corps, alliances, e.SecondPartyID, e.SecondPartyType)
			}
		}
		if txns, ok := loadSnapshot[esi.CorpWalletTransactions](app, ctx, characterID, esi.CorpTxnsKind(division)); ok {
			for _, t := range txns {
				add(t.ClientID)
			}
		}
	}
	app.flushOrgWants(ctx, corps, alliances)
}
