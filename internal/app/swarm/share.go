package swarm

// ---------------------------------------------------------------------------
// What the swarm shares, and how entries land. Six public kinds,
// nothing else:
//
//   market_history      region/type daily aggregates
//   type_name           one EVE type's name
//   corporation_record  one corporation's public record
//   alliance_record     one alliance's public record
//   structure_name      one player structure's name (the only kind
//                       fetched with a token; the name itself is
//                       game data, never account data)
//   public_contract     one public contract's header and its full
//                       item list (the contents are the expensive
//                       fetch: one ESI Call per contract, ever)
//
// Two rules keep poison out of the tables. First, an instance
// serves only what its own worker fetched from ESI and still
// considers fresh — the same gates the worker fetches by. A row
// a peer supplied is never served onward: this instance would
// be signing another's data as its own, and the row would look
// newly fetched at every hop. An applied row also keeps the
// fetch time its signer signed, so it is as old here as it is
// at its source (SwarmSupplied, ApplySwarmEntry). Second,
// nothing applies to the
// real tables without passing through staging: signature
// verified against the signer's pinned key, payload
// strictly decoded into the row shape, and the IDs inside the
// payload checked against the signed key, so a valid signature
// cannot be replayed onto another key. Spot checks (swarm/trust.go)
// re-fetch against ESI before staged rows apply; renders read
// the real tables only, never staging.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"evesynapse/internal/app/module"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// The shared kinds (plus the membership list, which signs with
// the same envelope but never stages).
const (
	SwarmKindMembers     = "swarm_members"
	SwarmKindHistory     = "market_history"
	SwarmKindTypeName    = "type_name"
	SwarmKindCorporation = "corporation_record"
	SwarmKindAlliance    = "alliance_record"
	SwarmKindStructure   = "structure_name"
	SwarmKindContract    = "public_contract"
)

// ValidSwarmKind reports whether kind is data the swarm carries
// (not the membership list, which is gossip, not data).
func ValidSwarmKind(kind string) bool {
	switch kind {
	case SwarmKindHistory, SwarmKindTypeName, SwarmKindCorporation, SwarmKindAlliance, SwarmKindStructure, SwarmKindContract:
		return true
	default:
		return false
	}
}

// SwarmURL is a parsed peer URL: scheme and host only, so
// validation never depends on paths or query strings.
type SwarmURL struct {
	Scheme string
	Host   string
}

// ParseSwarmURL parses a peer URL strictly: a scheme, a host,
// nothing else trusted.
func ParseSwarmURL(raw string) (SwarmURL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return SwarmURL{}, fmt.Errorf("not a URL")
	}
	return SwarmURL{Scheme: strings.ToLower(u.Scheme), Host: u.Hostname()}, nil
}

// ParseSwarmID reads one positive EVE ID from a key part.
// Malformed IDs refuse; a shared key must name its entity.
func ParseSwarmID(part string) (int64, bool) {
	if strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(part, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ServeSwarmKey reads one shared key from the local tables for a
// peer pull. It serves only rows this instance fetched itself
// and holds fresh: history inside its refetch gate, records in
// the ready state, resolved structure names, any held type
// name. A row a peer supplied is a miss like anything else — the
// asker pulls it from its source or fetches it itself — so the
// swarm never moves stale or second-hand data sideways.
func (app *Mod) ServeSwarmKey(ctx context.Context, kind, key string) (payload string, fetchedAt time.Time, ok bool) {
	switch kind {
	case SwarmKindHistory:
		return app.ServeSwarmHistory(ctx, key)
	case SwarmKindTypeName:
		return app.ServeSwarmTypeName(ctx, key)
	case SwarmKindCorporation:
		return app.ServeSwarmCorporation(ctx, key)
	case SwarmKindAlliance:
		return app.ServeSwarmAlliance(ctx, key)
	case SwarmKindStructure:
		return app.ServeSwarmStructure(ctx, key)
	case SwarmKindContract:
		return app.ServeSwarmContract(ctx, key)
	default:
		return "", time.Time{}, false
	}
}

// SwarmHistoryPayload is market_history on the wire: the pair,
// then its day rows newest first, bounded.
type SwarmHistoryPayload struct {
	RegionID int64             `json:"region_id"`
	TypeID   int64             `json:"type_id"`
	Days     []SwarmHistoryDay `json:"days"`
}

// SwarmHistoryDay is one daily aggregate.
type SwarmHistoryDay struct {
	Date       string  `json:"date"`
	Average    float64 `json:"average"`
	Highest    float64 `json:"highest"`
	Lowest     float64 `json:"lowest"`
	Volume     int64   `json:"volume"`
	OrderCount int64   `json:"order_count"`
}

// MaxSwarmHistoryDays caps one history entry: a year of daily
// rows plus slack, never an unbounded table dump.
const MaxSwarmHistoryDays = 400

func (app *Mod) ServeSwarmHistory(ctx context.Context, key string) (string, time.Time, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return "", time.Time{}, false
	}
	regionID, ok := ParseSwarmID(parts[0])
	if !ok {
		return "", time.Time{}, false
	}
	typeID, ok := ParseSwarmID(parts[1])
	if !ok {
		return "", time.Time{}, false
	}
	fetchKind := module.MarketFetchKind("history", module.MarketKey{RegionID: regionID, TypeID: typeID})
	state, err := app.Queries.GetMarketFetchState(ctx, fetchKind)
	if err != nil || state.State != module.FetchStateOK || state.Detail == SwarmFetchDetail {
		return "", time.Time{}, false
	}
	if time.Since(state.AttemptedAt) >= module.HistoryRefetchGate {
		return "", time.Time{}, false
	}
	rows, err := app.Queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{
		RegionID: regionID, TypeID: typeID, RowLimit: MaxSwarmHistoryDays,
	})
	if err != nil || len(rows) == 0 {
		return "", time.Time{}, false
	}
	out := SwarmHistoryPayload{RegionID: regionID, TypeID: typeID}
	for _, row := range rows {
		out.Days = append(out.Days, SwarmHistoryDay{
			Date: row.Date, Average: row.Average, Highest: row.Highest,
			Lowest: row.Lowest, Volume: row.Volume, OrderCount: row.OrderCount,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), state.AttemptedAt, true
}

// SwarmTypeNamePayload is type_name on the wire.
type SwarmTypeNamePayload struct {
	TypeID int64  `json:"type_id"`
	Name   string `json:"name"`
}

func (app *Mod) ServeSwarmTypeName(ctx context.Context, key string) (string, time.Time, bool) {
	typeID, ok := ParseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	name, err := app.Queries.GetTypeName(ctx, typeID)
	if err != nil || name == "" || app.SwarmSupplied(ctx, SwarmKindTypeName, key, sql.NullTime{}) {
		return "", time.Time{}, false
	}
	// Type names carry no stored timestamp (they are effectively
	// immutable); the serve time travels instead, and receivers
	// apply names only where they hold none.
	raw, err := json.Marshal(SwarmTypeNamePayload{TypeID: typeID, Name: name})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), time.Now().UTC(), true
}

// SwarmOrgPayload is a corporation or alliance record on the
// wire: the stored ESI payload verbatim, so the receiver
// decodes exactly what the fetcher stored.
type SwarmOrgPayload struct {
	ID      int64  `json:"id"`
	Payload string `json:"payload"`
}

func (app *Mod) ServeSwarmCorporation(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	rec, err := app.Queries.GetCorporationRecord(ctx, id)
	if err != nil || rec.State != module.OrgStateReady || rec.Payload == "" || !rec.FetchedAt.Valid {
		return "", time.Time{}, false
	}
	if app.SwarmSupplied(ctx, SwarmKindCorporation, key, rec.FetchedAt) {
		return "", time.Time{}, false
	}
	raw, err := json.Marshal(SwarmOrgPayload{ID: id, Payload: rec.Payload})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), rec.FetchedAt.Time, true
}

func (app *Mod) ServeSwarmAlliance(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	rec, err := app.Queries.GetAllianceRecord(ctx, id)
	if err != nil || rec.State != module.OrgStateReady || rec.Payload == "" || !rec.FetchedAt.Valid {
		return "", time.Time{}, false
	}
	if app.SwarmSupplied(ctx, SwarmKindAlliance, key, rec.FetchedAt) {
		return "", time.Time{}, false
	}
	raw, err := json.Marshal(SwarmOrgPayload{ID: id, Payload: rec.Payload})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), rec.FetchedAt.Time, true
}

// SwarmSupplied reports whether the live row for a key is still
// the copy a peer supplied, by its provenance row: stamp is the
// live row's own timestamp (unset for a kind that has none, where
// any provenance row answers yes). A row this instance has
// fetched from ESI since carries a different stamp and is its
// own. Trouble reading provenance answers yes: when unsure, the
// row is not served.
func (app *Mod) SwarmSupplied(ctx context.Context, kind, key string, stamp sql.NullTime) bool {
	row, err := app.Queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: kind, Key: key})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		logging.Errorf("swarm: provenance of %s %s: %v", kind, key, err)
		return true
	}
	if !row.RowStamp.Valid || !stamp.Valid {
		return true
	}
	return stamp.Time.Equal(row.RowStamp.Time)
}

// SwarmStructurePayload is structure_name on the wire.
type SwarmStructurePayload struct {
	StructureID int64  `json:"structure_id"`
	Name        string `json:"name"`
}

func (app *Mod) ServeSwarmStructure(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	row, err := app.Queries.GetStructureName(ctx, id)
	if err != nil || row.State != esi.StructureResolved || row.Name == "" || !row.ResolvedAt.Valid {
		return "", time.Time{}, false
	}
	if app.SwarmSupplied(ctx, SwarmKindStructure, key, row.ResolvedAt) {
		return "", time.Time{}, false
	}
	raw, err := json.Marshal(SwarmStructurePayload{StructureID: id, Name: row.Name})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), row.ResolvedAt.Time, true
}

// SwarmContractPayload is public_contract on the wire: the
// header facts as plain values (dates as unix seconds, the way
// the envelope carries time), then the full item list. The
// storm node copies this struct field for field — names and
// order are the protocol.
type SwarmContractPayload struct {
	ContractID          int64               `json:"contract_id"`
	RegionID            int64               `json:"region_id"`
	Type                string              `json:"type"`
	IssuerID            int64               `json:"issuer_id"`
	IssuerCorporationID int64               `json:"issuer_corporation_id"`
	ForCorporation      bool                `json:"for_corporation"`
	Title               string              `json:"title"`
	Price               float64             `json:"price"`
	Buyout              float64             `json:"buyout"`
	Reward              float64             `json:"reward"`
	Collateral          float64             `json:"collateral"`
	Volume              float64             `json:"volume"`
	StartLocationID     int64               `json:"start_location_id"`
	EndLocationID       int64               `json:"end_location_id"`
	DateIssued          int64               `json:"date_issued"`
	DateExpired         int64               `json:"date_expired"`
	DaysToComplete      int64               `json:"days_to_complete"`
	Items               []SwarmContractItem `json:"items"`
}

// SwarmContractItem is one line of a contract's item list. A
// nil ItemID is no singleton ID (the database's NULL, ESI's 0);
// the blueprint fields are pointers because absence is the
// signal, exactly as on the ESI route.
type SwarmContractItem struct {
	RecordID           int64  `json:"record_id"`
	TypeID             int64  `json:"type_id"`
	ItemID             *int64 `json:"item_id"`
	Quantity           int64  `json:"quantity"`
	IsIncluded         bool   `json:"is_included"`
	IsBlueprintCopy    bool   `json:"is_blueprint_copy"`
	Runs               *int64 `json:"runs"`
	MaterialEfficiency *int64 `json:"material_efficiency"`
	TimeEfficiency     *int64 `json:"time_efficiency"`
}

// MaxSwarmContractItems caps one contract entry's item list:
// real contracts run to hundreds of lines, and a list past this
// is a dump, not a contract.
const MaxSwarmContractItems = 1000

func swarmPtrFromNull(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func swarmNullFromPtr(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func (app *Mod) ServeSwarmContract(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	rec, err := app.Queries.GetPublicContract(ctx, id)
	if err != nil || rec.ItemsState != "stored" || !rec.DateExpired.After(time.Now()) {
		return "", time.Time{}, false
	}
	// Contract items are immutable once issued, so the header's
	// own last_seen is the entry's freshness stamp — and the
	// stamp the provenance check binds to, as for records.
	if app.SwarmSupplied(ctx, SwarmKindContract, key, timeSet(rec.LastSeen)) {
		return "", time.Time{}, false
	}
	rows, err := app.Queries.ListPublicContractItems(ctx, id)
	if err != nil {
		return "", time.Time{}, false
	}
	out := SwarmContractPayload{
		ContractID: rec.ContractID, RegionID: rec.RegionID, Type: rec.Type,
		IssuerID: rec.IssuerID, IssuerCorporationID: rec.IssuerCorporationID,
		ForCorporation: rec.ForCorporation, Title: rec.Title,
		Price: rec.Price, Buyout: rec.Buyout, Reward: rec.Reward,
		Collateral: rec.Collateral, Volume: rec.Volume,
		StartLocationID: rec.StartLocationID, EndLocationID: rec.EndLocationID,
		DateIssued: rec.DateIssued.Unix(), DateExpired: rec.DateExpired.Unix(),
		DaysToComplete: rec.DaysToComplete,
		Items:          []SwarmContractItem{},
	}
	for _, row := range rows {
		out.Items = append(out.Items, SwarmContractItem{
			RecordID: row.RecordID, TypeID: row.TypeID, ItemID: swarmPtrFromNull(row.ItemID),
			Quantity: row.Quantity, IsIncluded: row.IsIncluded, IsBlueprintCopy: row.IsBlueprintCopy,
			Runs: swarmPtrFromNull(row.Runs), MaterialEfficiency: swarmPtrFromNull(row.MaterialEfficiency),
			TimeEfficiency: swarmPtrFromNull(row.TimeEfficiency),
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), rec.LastSeen, true
}

// SwarmClockDrift is how far ahead of this clock a signed fetch
// time may sit and still be put down to drift; MaxSwarmEntryAge
// is how old one may be and still be staged.
const (
	SwarmClockDrift  = 5 * time.Minute
	MaxSwarmEntryAge = 90 * 24 * time.Hour
)

// SwarmHistoryWindowDays is how far back from its fetch a
// history entry's days are taken: the most one entry carries
// (MaxSwarmHistoryDays) plus a month of slack. Older rows are
// left out of the apply; nothing a single fetch returns reaches
// that far.
const SwarmHistoryWindowDays = MaxSwarmHistoryDays + 31

// SwarmHistoryDayAfter finds a day in a history payload dated
// after the fetch that is said to have returned it.
func SwarmHistoryDayAfter(payload string, fetchedAt time.Time) (string, bool) {
	var body SwarmHistoryPayload
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return "", false
	}
	last := fetchedAt.UTC().Format(time.DateOnly)
	for _, day := range body.Days {
		if day.Date > last {
			return day.Date, true
		}
	}
	return "", false
}

// StageSwarmEntry verifies one pulled entry and stages it for a
// spot check. The signature is checked against the signer's
// pinned key, the payload is strictly decoded into its row
// shape, and the IDs inside must match the signed key — a valid
// signature replayed onto another key dies here. Attribution is
// split where it belongs: transport faults (unknown signer, bad
// signature) count against the peer that served the entry, which
// is the only identity verified so far — counting them against
// the claimed signer would let a peer frame anyone by naming
// them. Content lies (a valid signature over a lying payload)
// count against the signer, whose key proved authorship.
// Nothing touches the real tables at this step.
func (app *Mod) StageSwarmEntry(ctx context.Context, entry SwarmSignedEntry, from string, now time.Time) error {
	if !ValidSwarmKind(entry.Kind) || entry.Key == "" {
		return fmt.Errorf("swarm: stage %s %s: unknown kind", entry.Kind, entry.Key)
	}
	peer, err := app.Queries.GetSwarmPeer(ctx, entry.Signer)
	if err != nil {
		app.CountSwarmShapeReject(ctx, from)
		return fmt.Errorf("swarm: stage %s %s: unknown signer", entry.Kind, entry.Key)
	}
	if !VerifySwarmEntry(entry, peer.PublicKey) {
		app.CountSwarmShapeReject(ctx, from)
		return fmt.Errorf("swarm: stage %s %s: signature does not verify", entry.Kind, entry.Key)
	}
	if err := CheckSwarmPayload(entry.Kind, entry.Key, entry.Payload); err != nil {
		app.CountSwarmShapeReject(ctx, entry.Signer)
		return err
	}
	// The signed fetch time is the signer's clock, and it decides
	// which of two staged answers is kept and how old the applied
	// row reads. It is only believed inside a window around this
	// instance's own receipt: a time further ahead than clocks
	// drift is a lie under a good signature (and would outrank
	// every honest answer for the key), so it counts against the
	// signer; within drift it is held to now. An entry older than
	// the window is too stale to be worth a check and is simply
	// not staged.
	fetchedAt := time.Unix(entry.FetchedAt, 0).UTC()
	switch {
	case fetchedAt.After(now.Add(SwarmClockDrift)):
		app.CountSwarmShapeReject(ctx, entry.Signer)
		return fmt.Errorf("swarm: stage %s %s: fetch time is %s ahead of this clock", entry.Kind, entry.Key, fetchedAt.Sub(now).Round(time.Second))
	case fetchedAt.After(now):
		fetchedAt = now.UTC()
	case now.Sub(fetchedAt) > MaxSwarmEntryAge:
		return fmt.Errorf("swarm: stage %s %s: fetched %s ago, too old to stage", entry.Kind, entry.Key, now.Sub(fetchedAt).Round(time.Hour))
	}
	if entry.Kind == SwarmKindHistory {
		// No fetch returns a day later than the day it ran: one
		// in the entry is made up, under a good signature.
		if day, bad := SwarmHistoryDayAfter(entry.Payload, fetchedAt); bad {
			app.CountSwarmShapeReject(ctx, entry.Signer)
			return fmt.Errorf("swarm: stage %s %s: day %s is after its own fetch on %s", entry.Kind, entry.Key, day, fetchedAt.Format(time.DateOnly))
		}
	}
	if err := app.Queries.StageSwarmEntry(ctx, db.StageSwarmEntryParams{
		Kind: entry.Kind, Key: entry.Key, Payload: entry.Payload,
		Signer: entry.Signer, Signature: entry.Signature,
		FetchedAt: fetchedAt, ReceivedAt: now, Source: from,
	}); err != nil {
		return fmt.Errorf("swarm: stage %s %s: %w", entry.Kind, entry.Key, err)
	}
	return nil
}

// CheckSwarmPayload strictly decodes one entry payload into its
// row shape and binds it to the signed key: every ID inside
// must match, every number must be in range, every day row must
// be shaped like the table row it would become. This is format
// validation against the existing row shapes, not sanitizing —
// the values stay exactly what the signer sent, for the spot
// check to judge.
func CheckSwarmPayload(kind, key, payload string) error {
	dec := SwarmStrictDecoder(strings.NewReader(payload))
	switch kind {
	case SwarmKindHistory:
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		regionID, ok := ParseSwarmID(parts[0])
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		typeID, ok := ParseSwarmID(parts[1])
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body SwarmHistoryPayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.RegionID != regionID || body.TypeID != typeID {
			return fmt.Errorf("swarm: stage %s %s: payload names another pair", kind, key)
		}
		if len(body.Days) == 0 || len(body.Days) > MaxSwarmHistoryDays {
			return fmt.Errorf("swarm: stage %s %s: %d day rows", kind, key, len(body.Days))
		}
		// Every day is a real calendar date, written the one way
		// the table writes it, and appears once: the date is the
		// row's key, and everything later orders by it as text.
		seen := make(map[string]bool, len(body.Days))
		for _, day := range body.Days {
			if day.Average < 0 || day.Highest < 0 || day.Lowest < 0 || day.Volume < 0 || day.OrderCount < 0 {
				return fmt.Errorf("swarm: stage %s %s: malformed day row", kind, key)
			}
			if at, err := time.Parse(time.DateOnly, day.Date); err != nil || at.Format(time.DateOnly) != day.Date || seen[day.Date] {
				return fmt.Errorf("swarm: stage %s %s: malformed or repeated day %q", kind, key, day.Date)
			}
			seen[day.Date] = true
		}
	case SwarmKindTypeName:
		typeID, ok := ParseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body SwarmTypeNamePayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.TypeID != typeID || body.Name == "" || len(body.Name) > 256 {
			return fmt.Errorf("swarm: stage %s %s: payload names another type", kind, key)
		}
	case SwarmKindCorporation, SwarmKindAlliance:
		id, ok := ParseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body SwarmOrgPayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.ID != id || body.Payload == "" || len(body.Payload) > MaxSwarmPayloadBytes {
			return fmt.Errorf("swarm: stage %s %s: payload names another record", kind, key)
		}
		// The record body must itself be JSON (it is stored
		// verbatim and decoded at render): a non-JSON body
		// would poison the render path, not the swarm table.
		var probe any
		if err := json.Unmarshal([]byte(body.Payload), &probe); err != nil {
			return fmt.Errorf("swarm: stage %s %s: record body is not JSON", kind, key)
		}
		// It must also name the record it claims to be, in
		// either dialect the app reads — this app's wrapped
		// shape or a bare ESI record. A body naming nothing
		// under either dialect is shape, not data: refused
		// here, it never reaches a spot check that could
		// only convict its signer for it.
		switch kind {
		case SwarmKindCorporation:
			if _, ok := module.DecodeCorpPayload(body.Payload); !ok {
				return fmt.Errorf("swarm: stage %s %s: record body names no corporation", kind, key)
			}
		case SwarmKindAlliance:
			if _, ok := module.DecodeAlliancePayload(body.Payload); !ok {
				return fmt.Errorf("swarm: stage %s %s: record body names no alliance", kind, key)
			}
		}
	case SwarmKindStructure:
		id, ok := ParseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body SwarmStructurePayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.StructureID != id || body.Name == "" || len(body.Name) > 256 {
			return fmt.Errorf("swarm: stage %s %s: payload names another structure", kind, key)
		}
	case SwarmKindContract:
		id, ok := ParseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body SwarmContractPayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.ContractID != id {
			return fmt.Errorf("swarm: stage %s %s: payload names another contract", kind, key)
		}
		switch body.Type {
		case "item_exchange", "auction", "courier":
		default:
			return fmt.Errorf("swarm: stage %s %s: unknown contract type %q", kind, key, body.Type)
		}
		if body.RegionID <= 0 || body.IssuerID <= 0 || body.IssuerCorporationID <= 0 ||
			body.Price < 0 || body.Buyout < 0 || body.Reward < 0 || body.Collateral < 0 || body.Volume < 0 ||
			body.DaysToComplete < 0 || body.DateExpired <= body.DateIssued ||
			len(body.Title) > 1024 || len(body.Items) > MaxSwarmContractItems {
			return fmt.Errorf("swarm: stage %s %s: malformed contract", kind, key)
		}
		// Record IDs are the item rows' keys: each appears once,
		// or one insert would silently swallow another line.
		seen := make(map[int64]bool, len(body.Items))
		for _, item := range body.Items {
			if item.RecordID <= 0 || seen[item.RecordID] || item.TypeID <= 0 || item.Quantity <= 0 {
				return fmt.Errorf("swarm: stage %s %s: malformed or repeated item row", kind, key)
			}
			seen[item.RecordID] = true
		}
	default:
		return fmt.Errorf("swarm: stage %s %s: unknown kind", kind, key)
	}
	return nil
}

// SwarmOrigin is where an applied entry came from: the peer it
// was pulled from, the instance whose key signed it, the fetch
// time that signature covers, and whether ESI confirmed it
// before it was applied.
type SwarmOrigin struct {
	Source    string
	Signer    string
	FetchedAt time.Time
	Verified  bool
}

// ApplySwarmEntry writes one entry to the real tables through
// the existing upserts, and settles the fetch state the worker
// gates on — so a swarm-supplied row stops the worker fetching
// it itself, which is the saved ESI Call. Only the trust pass
// calls this, on a passed check or a trust-earned skip.
//
// Every apply leaves a provenance row (swarm_applied): who
// served the entry, who signed it, and the stamp written into
// the live row. That row is what lets SwarmRollBack take the
// data back if its signer is later excluded.
func (app *Mod) ApplySwarmEntry(ctx context.Context, kind, key, payload string, origin SwarmOrigin, now time.Time) error {
	// The live row carries the fetch time its signer signed, not
	// the moment it arrived: a row is as old as its fetch, here
	// as at its source, so the worker's freshness gates see its
	// true age and nothing is made fresher by changing hands. A
	// time ahead of this clock is held to now. The same stamp
	// goes on the provenance row, at the database's precision,
	// so the two compare equal later.
	appliedAt := now.UTC().Truncate(time.Microsecond)
	stamp := origin.FetchedAt.UTC().Truncate(time.Microsecond)
	if origin.FetchedAt.IsZero() || stamp.After(appliedAt) {
		stamp = appliedAt
	}
	applied := db.PutSwarmAppliedParams{
		Kind: kind, Key: key, Source: origin.Source, Signer: origin.Signer,
		FetchedAt: stamp, AppliedAt: appliedAt, Verified: origin.Verified,
		RowStamp: timeSet(stamp),
	}
	if applied.Source == "" {
		// Staged before the serving peer was recorded: a pull
		// only ever staged entries its server signed.
		applied.Source = origin.Signer
	}
	dec := SwarmStrictDecoder(strings.NewReader(payload))
	switch kind {
	case SwarmKindHistory:
		var body SwarmHistoryPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		// Only days inside the window its fetch could have
		// returned are written: none after the fetch, none
		// further back than one fetch reaches.
		first := stamp.AddDate(0, 0, -SwarmHistoryWindowDays).Format(time.DateOnly)
		last := stamp.Format(time.DateOnly)
		for _, day := range body.Days {
			if day.Date < first || day.Date > last {
				continue
			}
			if err := app.Queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
				RegionID: body.RegionID, TypeID: body.TypeID, Date: day.Date,
				Average: day.Average, Highest: day.Highest, Lowest: day.Lowest,
				Volume: day.Volume, OrderCount: day.OrderCount,
			}); err != nil {
				return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
			}
			if applied.FirstDate == "" || day.Date < applied.FirstDate {
				applied.FirstDate = day.Date
			}
			if day.Date > applied.LastDate {
				applied.LastDate = day.Date
			}
		}
		// The sold-per-day figure follows, as after a direct fetch
		// (fetchAndStoreHistory).
		if err := app.Queries.RefreshMarketRegionStatAvgVolume(ctx, db.RefreshMarketRegionStatAvgVolumeParams{
			RegionID: body.RegionID, TypeID: body.TypeID,
		}); err != nil {
			logging.Errorf("swarm: refresh sold-per-day for %s: %v", key, err)
		}
		app.Deps.RecordMarketFetchAt(ctx, module.MarketFetchKind("history", module.MarketKey{RegionID: body.RegionID, TypeID: body.TypeID}), module.FetchStateOK, SwarmFetchDetail, stamp)
		app.AddSwarmCounter(ctx, SwarmCounterAppliedHistory, 1)
	case SwarmKindTypeName:
		var body SwarmTypeNamePayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		if err := app.Queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: body.TypeID, Name: body.Name}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		// Readers see it at once, and the demand for it is settled.
		app.Deps.LearnTypeName(body.TypeID, body.Name)
		// Type names carry no timestamp in their table.
		applied.RowStamp = sql.NullTime{}
		app.AddSwarmCounter(ctx, SwarmCounterAppliedNames, 1)
	case SwarmKindCorporation:
		var body SwarmOrgPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		// Stored in this app's shape whatever shape the peer sent; a
		// payload that names no corporation is not stored at all.
		normalized, ok := module.DecodeCorpPayload(body.Payload)
		if !ok {
			return fmt.Errorf("swarm: apply %s %s: unreadable corporation record", kind, key)
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		if err := app.Queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
			CorporationID: body.ID, Payload: string(encoded), State: module.OrgStateReady, FetchedAt: timeSet(stamp),
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.AddSwarmCounter(ctx, SwarmCounterAppliedRecords, 1)
	case SwarmKindAlliance:
		var body SwarmOrgPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		normalized, ok := module.DecodeAlliancePayload(body.Payload)
		if !ok {
			return fmt.Errorf("swarm: apply %s %s: unreadable alliance record", kind, key)
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		if err := app.Queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
			AllianceID: body.ID, Payload: string(encoded), State: module.OrgStateReady, FetchedAt: timeSet(stamp),
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.AddSwarmCounter(ctx, SwarmCounterAppliedRecords, 1)
	case SwarmKindStructure:
		var body SwarmStructurePayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		// A name this instance resolved from a better source
		// (its own ESI Call, a corporation list) stays: the
		// peer's copy is consumed without being written, and no
		// provenance points at a row it never touched.
		if existing, err := app.Queries.GetStructureName(ctx, body.StructureID); err == nil &&
			existing.State == esi.StructureResolved &&
			module.StructureSourceRank(existing.Source) > module.StructureSourceRank(esi.StructureSourceCommunity) {
			if err := app.Queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
				logging.Errorf("swarm: apply %s %s: unstaged: %v", kind, key, err)
			}
			return nil
		}
		if err := app.Queries.SetStructureName(ctx, db.SetStructureNameParams{
			StructureID: body.StructureID, Name: body.Name, State: esi.StructureResolved,
			ResolvedAt: timeSet(stamp), Source: esi.StructureSourceCommunity,
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.AddSwarmCounter(ctx, SwarmCounterAppliedNames, 1)
	case SwarmKindContract:
		var body SwarmContractPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		// A contract whose item list is already settled here —
		// stored by this instance's own drain, or marked
		// private, gone or parked — takes no writes at all:
		// the entry is consumed, and the header refresh stays
		// the instance's own sweep's job, the same precedent as
		// a structure name a better source already resolved.
		// Only a pending row (header swept, contents not yet
		// fetched — the exact gap this kind exists to close)
		// or no row at all is written.
		if existing, err := app.Queries.GetPublicContract(ctx, body.ContractID); err == nil {
			if existing.ItemsState != "pending" {
				if err := app.Queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
					logging.Errorf("swarm: apply %s %s: unstaged: %v", kind, key, err)
				}
				return nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		// The header lands under the region's current sweep
		// cycle (0 when the region has no sweep state yet), with
		// the entry stamp as both first and last seen — the
		// $19 the upsert writes twice — so the row reads exactly
		// as old as its source, and the provenance stamp below
		// compares equal to the last_seen written here.
		var cycle int64
		if state, err := app.Queries.GetPublicContractSweepState(ctx, body.RegionID); err == nil {
			cycle = state.Cycle
		}
		if err := app.Queries.UpsertPublicContract(ctx, db.UpsertPublicContractParams{
			ContractID: body.ContractID, RegionID: body.RegionID, Type: body.Type,
			IssuerID: body.IssuerID, IssuerCorporationID: body.IssuerCorporationID,
			ForCorporation: body.ForCorporation, Title: body.Title,
			Price: body.Price, Buyout: body.Buyout, Reward: body.Reward,
			Collateral: body.Collateral, Volume: body.Volume,
			StartLocationID: body.StartLocationID, EndLocationID: body.EndLocationID,
			DateIssued: time.Unix(body.DateIssued, 0).UTC(), DateExpired: time.Unix(body.DateExpired, 0).UTC(),
			DaysToComplete: body.DaysToComplete,
			LastCycle:      cycle, FirstSeen: stamp,
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		for _, item := range body.Items {
			if err := app.Queries.InsertPublicContractItem(ctx, db.InsertPublicContractItemParams{
				RecordID: item.RecordID, ContractID: body.ContractID, TypeID: item.TypeID,
				ItemID: swarmNullFromPtr(item.ItemID), Quantity: item.Quantity,
				IsIncluded: item.IsIncluded, IsBlueprintCopy: item.IsBlueprintCopy,
				Runs: swarmNullFromPtr(item.Runs), MaterialEfficiency: swarmNullFromPtr(item.MaterialEfficiency),
				TimeEfficiency: swarmNullFromPtr(item.TimeEfficiency),
			}); err != nil {
				return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
			}
		}
		if err := app.Queries.SetPublicContractItemsState(ctx, db.SetPublicContractItemsStateParams{
			ContractID: body.ContractID, ItemsState: "stored",
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		// As when the drain stores a list itself: a known
		// abyssal instance on the list opens its sighting now.
		if err := app.Queries.OpenAbyssalSightingsForContract(ctx, db.OpenAbyssalSightingsForContractParams{
			ContractID: body.ContractID, LastSeenAt: time.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		if err := app.Queries.StampAbyssalSightingAttribution(ctx, db.StampAbyssalSightingAttributionParams{
			ContractID: sql.NullInt64{Int64: body.ContractID, Valid: true},
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.AddSwarmCounter(ctx, SwarmCounterAppliedContracts, 1)
	default:
		return fmt.Errorf("swarm: apply %s %s: unknown kind", kind, key)
	}
	if err := app.Queries.PutSwarmApplied(ctx, applied); err != nil {
		// The data is in; without its provenance it could never
		// be taken back, so say so loudly.
		logging.Errorf("swarm: apply %s %s: provenance not recorded: %v", kind, key, err)
	}
	if err := app.Queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
		logging.Errorf("swarm: apply %s %s: unstaged: %v", kind, key, err)
	}
	return nil
}

// SwarmFetchDetail marks a market fetch state the swarm settled,
// not this instance's own fetch.
const SwarmFetchDetail = "swarm"

// MaxSwarmRollBackPerCycle bounds one cycle's rollback work: an
// excluded signer may have supplied thousands of rows, and the
// worker cycle is not the place to undo them all at once.
const MaxSwarmRollBackPerCycle = 200

// SwarmRollBack takes back what excluded signers supplied.
// Excluding a signer stops its entries arriving; this is the
// other half, for the ones already applied (most of which went
// in unchecked, on the signer's reputation). Each provenance row
// under an excluded signer is undone where the live row is
// still the swarm's copy — a record goes back to pending, a
// name is cleared, history loses the days the entry supplied
// and its fetch state, so the worker fetches the truth from ESI
// like any other missing datum. A row this instance has fetched
// itself since is its own and stays. Local writes only; it
// returns how many live rows were taken back.
func (app *Mod) SwarmRollBack(ctx context.Context) int {
	rows, err := app.Queries.ListSwarmAppliedToRollBack(ctx, MaxSwarmRollBackPerCycle)
	if err != nil {
		logging.Errorf("swarm: roll back: list: %v", err)
		return 0
	}
	undone := 0
	for _, row := range rows {
		took, err := app.SwarmRollBackOne(ctx, row)
		if err != nil {
			// The provenance row stays, so the next cycle tries again.
			logging.Errorf("swarm: roll back %s %s: %v", row.Kind, row.Key, err)
			continue
		}
		if err := app.Queries.DeleteSwarmApplied(ctx, db.DeleteSwarmAppliedParams{Kind: row.Kind, Key: row.Key}); err != nil {
			logging.Errorf("swarm: roll back %s %s: clear provenance: %v", row.Kind, row.Key, err)
		}
		if took {
			undone++
		}
	}
	if undone > 0 {
		app.AddSwarmCounter(ctx, SwarmCounterRolledBack, int64(undone))
		logging.Warnf("swarm: took back %d entries supplied by excluded signers", undone)
	}
	return undone
}

// SwarmRollBackOne undoes one applied entry. took is false when
// there was nothing left to undo (the row has been refetched, or
// the key no longer parses): the provenance row goes either way.
func (app *Mod) SwarmRollBackOne(ctx context.Context, row db.SwarmApplied) (took bool, err error) {
	var n int64
	switch row.Kind {
	case SwarmKindHistory:
		parts := strings.Split(row.Key, "/")
		if len(parts) != 2 || !row.RowStamp.Valid {
			return false, nil
		}
		regionID, okRegion := ParseSwarmID(parts[0])
		typeID, okType := ParseSwarmID(parts[1])
		if !okRegion || !okType {
			return false, nil
		}
		// The fetch state decides: gone or restamped means this
		// instance has fetched the pair itself since.
		n, err = app.Queries.RollBackSwarmHistoryState(ctx, db.RollBackSwarmHistoryStateParams{
			Kind:     module.MarketFetchKind("history", module.MarketKey{RegionID: regionID, TypeID: typeID}),
			RowStamp: row.RowStamp.Time,
		})
		if err != nil || n == 0 {
			return false, err
		}
		if _, err := app.Queries.RollBackSwarmHistoryDays(ctx, db.RollBackSwarmHistoryDaysParams{
			RegionID: regionID, TypeID: typeID, FirstDate: row.FirstDate, LastDate: row.LastDate,
		}); err != nil {
			return false, err
		}
		return true, nil
	case SwarmKindTypeName:
		id, ok := ParseSwarmID(row.Key)
		if !ok {
			return false, nil
		}
		n, err = app.Queries.RollBackSwarmTypeName(ctx, id)
		// The in-process cache holds the name too.
		app.Deps.ForgetTypeName(id)
	case SwarmKindCorporation:
		id, ok := ParseSwarmID(row.Key)
		if !ok || !row.RowStamp.Valid {
			return false, nil
		}
		n, err = app.Queries.RollBackSwarmCorporation(ctx, db.RollBackSwarmCorporationParams{CorporationID: id, RowStamp: row.RowStamp.Time})
	case SwarmKindAlliance:
		id, ok := ParseSwarmID(row.Key)
		if !ok || !row.RowStamp.Valid {
			return false, nil
		}
		n, err = app.Queries.RollBackSwarmAlliance(ctx, db.RollBackSwarmAllianceParams{AllianceID: id, RowStamp: row.RowStamp.Time})
	case SwarmKindStructure:
		id, ok := ParseSwarmID(row.Key)
		if !ok || !row.RowStamp.Valid {
			return false, nil
		}
		n, err = app.Queries.RollBackSwarmStructure(ctx, db.RollBackSwarmStructureParams{StructureID: id, RowStamp: row.RowStamp.Time})
	case SwarmKindContract:
		id, ok := ParseSwarmID(row.Key)
		if !ok || !row.RowStamp.Valid {
			return false, nil
		}
		// The header delete is stamp-guarded: a row the
		// instance's own sweep has re-seen since carries a
		// newer last_seen and is its own now, so nothing is
		// taken. Bids are the instance's own fetches and stay
		// either way; the items go explicitly with the header
		// (no cascade joins them).
		n, err = app.Queries.DeletePublicContractIfStamped(ctx, db.DeletePublicContractIfStampedParams{ContractID: id, LastSeen: row.RowStamp.Time})
		if err != nil || n == 0 {
			return false, err
		}
		if err := app.Queries.DeletePublicContractItemsByContracts(ctx, []int64{id}); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
	return n > 0, err
}

// Why a staged entry left staging without applying, for the log
// line. A discard is a failed check: ESI was asked and the entry
// was wrong. A drop is everything else, and blames nobody.
const (
	SwarmDiscardMismatch = "it did not match ESI"
	SwarmDropAgedOut     = "it waited in staging past the age cap"
	SwarmDropLowTrust    = "its signer is under the trust floor"
	SwarmDropUndecidable = "it could not be checked after several passes"
)

// DropSwarmEntry removes a staged entry that was never compared
// with ESI: too old, from a signer no longer read, or impossible
// to check from here. It is not a failed check, so it costs the
// signer nothing (no mark, no streak) and has its own counter.
// If the datum is still wanted, a later pull asks for it again.
func (app *Mod) DropSwarmEntry(ctx context.Context, kind, key, signer, reason string) {
	logging.Infof("swarm: dropped %s %s from %s unchecked: %s", kind, key, ShortenSwarmID(signer), reason)
	if err := app.Queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
		logging.Errorf("swarm: drop %s %s: %v", kind, key, err)
		return
	}
	app.AddSwarmCounter(ctx, SwarmCounterDroppedUnchecked, 1)
}

// DiscardSwarmEntry drops one staged entry that failed its spot
// check — ESI was asked and the entry was wrong — and counts it
// against the signer. The row never reaches the real tables:
// failed checks are discarded, never served. Nothing else comes
// through here (see DropSwarmEntry).
// Every discard is logged with its entry, signer and reason.
func (app *Mod) DiscardSwarmEntry(ctx context.Context, kind, key, signer, reason string) {
	logging.Warnf("swarm: discarded %s %s from %s: %s", kind, key, ShortenSwarmID(signer), reason)
	if err := app.Queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
		logging.Errorf("swarm: discard %s %s: %v", kind, key, err)
	}
	app.CountSwarmCheck(ctx, signer, false)
	app.AddSwarmCounter(ctx, SwarmCounterSpotFailed, 1)
}
