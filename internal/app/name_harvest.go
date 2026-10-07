package app

// The first half of the name warm-up (warmCharacterNames in
// worker.go): reading one character's stored snapshots and
// collecting every ID in them that a page will want a name for.
// Nothing here fetches anything. The second half resolves what
// the local caches still lack.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// nameWants is what one character's snapshots refer to by ID.
type nameWants struct {
	types      map[int64]bool
	places     map[int64]string // location id -> "station"|"solar_system"
	characters map[int64]bool   // killmail people, corp rosters, wallet parties, mail, contacts
	planets    map[int64]bool   // colony planets
	schematics map[int64]bool   // PI schematic names + cycle times
	structures map[int64]bool   // for the structure-name queue; never fetched by the warm-up
}

func newNameWants() *nameWants {
	return &nameWants{
		types:      make(map[int64]bool),
		places:     make(map[int64]string),
		characters: make(map[int64]bool),
		planets:    make(map[int64]bool),
		schematics: make(map[int64]bool),
		structures: make(map[int64]bool),
	}
}

// nameHarvest reads one character's snapshots into a nameWants.
type nameHarvest struct {
	app         *Application
	characterID int64
	wants       *nameWants
}

// decode reads a snapshot payload into v, reporting whether it
// could. A payload that does not decode simply contributes nothing.
func (h *nameHarvest) decode(payload string, v any) bool {
	return json.Unmarshal([]byte(payload), v) == nil
}

// decodeOrLog is decode for the datasets whose failure is worth a
// log line (what is the label in it).
func (h *nameHarvest) decodeOrLog(payload string, v any, what string) bool {
	if err := json.Unmarshal([]byte(payload), v); err != nil {
		logging.Errorf("worker: warm names for character %d: decode %s snapshot: %v", h.characterID, what, err)
		return false
	}
	return true
}

// snapshot collects the IDs one snapshot refers to.
func (h *nameHarvest) snapshot(ctx context.Context, snap db.CharacterSnapshot) {
	switch snap.Kind {
	case esi.SnapSkills:
		h.skills(snap.Payload)
	case esi.SnapAssets:
		h.assets(snap.Payload, "assets")
	case esi.SnapCorpMembers:
		h.corpMembers(snap.Payload)
	case esi.SnapCorpMemberTracking:
		h.corpMemberTracking(snap.Payload)
	case esi.SnapCorpAssets:
		// Same payload shape as character assets.
		h.assets(snap.Payload, "corp assets")
	case esi.SnapCorpOrders:
		h.corpOrders(snap.Payload)
	case esi.SnapCorpStructures:
		h.corpStructures(ctx, snap.Payload)
	case esi.SnapWalletJournal:
		h.walletJournal(ctx, snap.Payload)
	case esi.SnapWalletTxns:
		h.walletTransactions(snap.Payload)
	case esi.SnapOrders:
		h.orders(snap.Payload)
	case esi.SnapOrdersHistory:
		h.orderHistory(snap.Payload)
	case esi.SnapContracts:
		h.contracts(snap.Payload)
	case esi.SnapIndustryJobs:
		h.industryJobs(snap.Payload)
	case esi.SnapBlueprints:
		h.blueprints(snap.Payload)
	case esi.SnapMining:
		h.miningLedger(snap.Payload)
	case esi.SnapPlanets:
		h.colonies(snap.Payload)
	case esi.SnapMail:
		h.mail(snap.Payload)
	case esi.SnapContacts:
		h.contacts(snap.Payload)
	default:
		// The suffix-keyed kinds: one snapshot per wallet
		// division, colony or calendar event.
		if strings.HasPrefix(snap.Kind, esi.SnapCorpTxnsPrefix) {
			h.corpWalletTransactions(snap.Payload)
		}
		if strings.HasPrefix(snap.Kind, esi.SnapCorpJournalPrefix) {
			h.corpWalletJournal(ctx, snap.Payload)
		}
		if strings.HasPrefix(snap.Kind, esi.SnapPlanetLayoutPrefix) {
			h.planetLayout(snap.Payload)
		}
		if strings.HasPrefix(snap.Kind, esi.SnapCalendarAttPrefix) {
			h.calendarAttendees(snap.Payload)
		}
	}
}

func (h *nameHarvest) skills(payload string) {
	var skills esi.Skills
	if !h.decodeOrLog(payload, &skills, "skills") {
		return
	}
	for _, s := range skills.Skills {
		h.wants.types[s.SkillID] = true
	}
}

// assets reads a character's or a corporation's asset list (the
// payloads have the same shape).
func (h *nameHarvest) assets(payload, what string) {
	var items []esi.Asset
	if !h.decodeOrLog(payload, &items, what) {
		return
	}
	for _, it := range items {
		h.wants.types[it.TypeID] = true
		if it.LocationType == "station" || it.LocationType == "solar_system" {
			h.wants.places[it.LocationID] = it.LocationType
		}
		if it.LocationType == "structure" {
			h.wants.structures[it.LocationID] = true
		}
	}
}

// corpMembers: the roster's names resolve through the same cache.
func (h *nameHarvest) corpMembers(payload string) {
	var members esi.CorpMembers
	if !h.decodeOrLog(payload, &members, "corp members") {
		return
	}
	for _, id := range members {
		if id > 0 {
			h.wants.characters[id] = true
		}
	}
}

func (h *nameHarvest) corpMemberTracking(payload string) {
	var tracking esi.CorpMemberTrackings
	if !h.decodeOrLog(payload, &tracking, "corp membertracking") {
		return
	}
	for _, t := range tracking {
		if t.CharacterID > 0 {
			h.wants.characters[t.CharacterID] = true
		}
		if t.ShipTypeID > 0 {
			h.wants.types[t.ShipTypeID] = true
		}
	}
}

func (h *nameHarvest) corpOrders(payload string) {
	var orders esi.CorpOrders
	if !h.decodeOrLog(payload, &orders, "corp orders") {
		return
	}
	for _, o := range orders {
		h.wants.types[o.TypeID] = true
	}
}

// corpStructures is the one harvester that also writes: the
// structures a corporation owns arrive with everything known about
// them, so that is stored as it goes by.
func (h *nameHarvest) corpStructures(ctx context.Context, payload string) {
	var structures esi.CorpStructures
	if !h.decodeOrLog(payload, &structures, "corp structures") {
		return
	}
	// Owner/system/type facts ride along into the
	// structure_context store behind the structure page.
	h.app.persistStructureContexts(ctx, structures)
	for _, s := range structures {
		if s.TypeID > 0 {
			h.wants.types[s.TypeID] = true
		}
		// The corp's own structures arrive already named:
		// seed the structure-name cache for free (tier 2,
		// provenance 'corp' — ESI truth, below only the
		// per-structure lookup itself).
		if s.Name != "" {
			if _, ok := h.app.esi.CachedStructureName(ctx, s.StructureID); !ok {
				if h.app.storeStructureName(ctx, s.StructureID, s.Name,
					esi.StructureResolved, esi.StructureSourceCorp, time.Now().UTC()) {
					h.app.esi.StoreStructureName(s.StructureID, s.Name)
				}
			}
		}
	}
}

// walletJournal: journal parties route by ESI's party_type (see
// harvestJournalParty): characters join the character-name
// harvest; corporations and alliances note org wants instead of
// 404ing the character endpoint every cycle.
func (h *nameHarvest) walletJournal(ctx context.Context, payload string) {
	var journal esi.WalletJournal
	if !h.decode(payload, &journal) {
		return
	}
	for _, e := range journal {
		h.app.harvestJournalParty(ctx, h.wants.characters, e.FirstPartyID, e.FirstPartyType)
		h.app.harvestJournalParty(ctx, h.wants.characters, e.SecondPartyID, e.SecondPartyType)
	}
}

func (h *nameHarvest) walletTransactions(payload string) {
	var txns esi.WalletTransactions
	if !h.decode(payload, &txns) {
		return
	}
	for _, t := range txns {
		if t.TypeID > 0 {
			h.wants.types[t.TypeID] = true
		}
		if t.ClientID >= 90_000_000 {
			h.wants.characters[t.ClientID] = true
		}
	}
}

func (h *nameHarvest) orders(payload string) {
	var orders esi.CharOrders
	if !h.decode(payload, &orders) {
		return
	}
	for _, o := range orders {
		if o.TypeID > 0 {
			h.wants.types[o.TypeID] = true
		}
		if isStructureID(o.LocationID) {
			h.wants.structures[o.LocationID] = true
		}
	}
}

func (h *nameHarvest) orderHistory(payload string) {
	var history esi.CharOrderHistory
	if !h.decode(payload, &history) {
		return
	}
	for _, o := range history {
		if o.TypeID > 0 {
			h.wants.types[o.TypeID] = true
		}
	}
}

func (h *nameHarvest) contracts(payload string) {
	var contracts esi.Contracts
	if !h.decode(payload, &contracts) {
		return
	}
	for _, c := range contracts {
		for _, id := range []int64{c.IssuerID, c.AssigneeID, c.AcceptorID} {
			if id >= 90_000_000 {
				h.wants.characters[id] = true
			}
		}
	}
}

func (h *nameHarvest) industryJobs(payload string) {
	var jobs esi.IndustryJobs
	if !h.decode(payload, &jobs) {
		return
	}
	for _, j := range jobs {
		if j.BlueprintTypeID > 0 {
			h.wants.types[j.BlueprintTypeID] = true
		}
		if j.ProductTypeID > 0 {
			h.wants.types[j.ProductTypeID] = true
		}
		if isStructureID(j.FacilityID) {
			h.wants.structures[j.FacilityID] = true
		}
	}
}

func (h *nameHarvest) blueprints(payload string) {
	var blueprints esi.Blueprints
	if !h.decode(payload, &blueprints) {
		return
	}
	for _, bp := range blueprints {
		if bp.TypeID > 0 {
			h.wants.types[bp.TypeID] = true
		}
	}
}

func (h *nameHarvest) miningLedger(payload string) {
	var ledger esi.MiningLedger
	if !h.decode(payload, &ledger) {
		return
	}
	for _, m := range ledger {
		if m.TypeID > 0 {
			h.wants.types[m.TypeID] = true
		}
	}
}

// colonies: colony planets resolve through the place-name cache
// (their names come from /universe/planets/); their systems ride
// the station/system pass.
func (h *nameHarvest) colonies(payload string) {
	var colonies esi.Colonies
	if !h.decode(payload, &colonies) {
		return
	}
	for _, c := range colonies {
		if c.PlanetID > 0 {
			h.wants.planets[c.PlanetID] = true
		}
		if c.SolarSystemID > 0 {
			h.wants.places[c.SolarSystemID] = "solar_system"
		}
	}
}

// mail: senders and character recipients resolve through the
// character-name cache (senders are characters by construction;
// recipients by their recorded kind).
func (h *nameHarvest) mail(payload string) {
	var headers esi.MailHeaders
	if !h.decode(payload, &headers) {
		return
	}
	for _, m := range headers {
		if m.From >= 90_000_000 {
			h.wants.characters[m.From] = true
		}
		for _, rcpt := range m.Recipients {
			if rcpt.RecipientType == "character" && rcpt.RecipientID >= 90_000_000 {
				h.wants.characters[rcpt.RecipientID] = true
			}
		}
	}
}

// contacts: contact kind is explicit in the payload, so the >= 90M
// harvest rule does not apply: pre-90M character contacts (the
// oldest pilots) warm their names here too.
func (h *nameHarvest) contacts(payload string) {
	var contacts esi.Contacts
	if !h.decode(payload, &contacts) {
		return
	}
	for _, c := range contacts {
		if c.ContactType == "character" && c.ContactID > 0 {
			h.wants.characters[c.ContactID] = true
		}
	}
}

// corpWalletTransactions reads one division's ledger. Transaction
// clients carry no kind, so they keep the >= 90M harvest threshold
// and the client's negative cache bounds a wrong guess.
func (h *nameHarvest) corpWalletTransactions(payload string) {
	var txns esi.CorpWalletTransactions
	if !h.decode(payload, &txns) {
		return
	}
	for _, t := range txns {
		if t.ClientID >= 90_000_000 {
			h.wants.characters[t.ClientID] = true
		}
	}
}

// corpWalletJournal reads one division's journal: parties route by
// ESI's party_type like the character-side journal.
func (h *nameHarvest) corpWalletJournal(ctx context.Context, payload string) {
	var journal esi.CorpJournal
	if !h.decode(payload, &journal) {
		return
	}
	for _, e := range journal {
		h.app.harvestJournalParty(ctx, h.wants.characters, e.FirstPartyID, e.FirstPartyType)
		h.app.harvestJournalParty(ctx, h.wants.characters, e.SecondPartyID, e.SecondPartyType)
	}
}

// planetLayout reads one colony's layout: pin and product types
// resolve through the type caches, factory schematics through the
// schematic cache.
func (h *nameHarvest) planetLayout(payload string) {
	var layout esi.PlanetLayout
	if !h.decode(payload, &layout) {
		return
	}
	for _, pin := range layout.Pins {
		if pin.TypeID > 0 {
			h.wants.types[pin.TypeID] = true
		}
		if pin.ExtractorDetails != nil && pin.ExtractorDetails.ProductTypeID > 0 {
			h.wants.types[pin.ExtractorDetails.ProductTypeID] = true
		}
		if pin.FactoryDetails != nil && pin.FactoryDetails.SchematicID > 0 {
			h.wants.schematics[pin.FactoryDetails.SchematicID] = true
		}
		if pin.SchematicID > 0 {
			h.wants.schematics[pin.SchematicID] = true
		}
	}
}

// calendarAttendees reads one event's attendee list. The payloads
// carry character ids only, and they resolve through the
// character-name cache.
func (h *nameHarvest) calendarAttendees(payload string) {
	var attendees esi.CalendarAttendees
	if !h.decode(payload, &attendees) {
		return
	}
	for _, a := range attendees {
		if a.CharacterID >= 90_000_000 {
			h.wants.characters[a.CharacterID] = true
		}
	}
}

// killmailPeople adds the victims and final-blow attackers of the
// character's stored killmail details, so the killmail list can
// label people instead of raw IDs.
func (h *nameHarvest) killmailPeople(ctx context.Context) {
	rows, err := h.app.queries.ListKillmailDetailsByCharacter(ctx, h.characterID)
	if err != nil {
		logging.Errorf("worker: warm names for character %d: list killmail details: %v", h.characterID, err)
		return
	}
	for _, row := range rows {
		var km esi.Killmail
		if err := json.Unmarshal([]byte(row.Payload), &km); err != nil {
			continue // undecodable payload: nothing to derive
		}
		if km.Victim.CharacterID > 0 {
			h.wants.characters[km.Victim.CharacterID] = true
		}
		for _, a := range km.Attackers {
			if a.FinalBlow && a.CharacterID > 0 {
				h.wants.characters[a.CharacterID] = true
			}
		}
	}
}

// harvestJournalParty routes one journal counterparty into the
// name pipeline its party_type names: characters join the
// character-name harvest, corporations and alliances note org
// record wants so their names resolve through the org endpoints
// -- never the character endpoint, which could only 404 on them.
// A party with no recorded type is left alone here; the render
// paths still resolve it on demand (journalParty). Called from
// the worker harvests, so the corp/alliance notes are plain
// queue writes, like a page noting a want.
func (app *Application) harvestJournalParty(ctx context.Context, charIDs map[int64]bool, id int64, partyType string) {
	if id <= 0 {
		return
	}
	switch partyType {
	case "character":
		charIDs[id] = true
	case "corporation":
		if err := app.queries.UpsertCorporationWant(ctx, id); err != nil {
			logging.Errorf("worker: note corporation want for journal party %d: %v", id, err)
		}
	case "alliance":
		if err := app.queries.UpsertAllianceWant(ctx, id); err != nil {
			logging.Errorf("worker: note alliance want for journal party %d: %v", id, err)
		}
	}
}
