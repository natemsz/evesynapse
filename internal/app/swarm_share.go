package app

// ---------------------------------------------------------------------------
// What the swarm shares, and how entries land. Five public kinds,
// nothing else:
//
//   market_history      region/type daily aggregates
//   type_name           one EVE type's name
//   corporation_record  one corporation's public record
//   alliance_record     one alliance's public record
//   structure_name      one player structure's name (the only kind
//                       fetched with a token; the name itself is
//                       game data, never account data)
//
// Two rules keep poison out of the tables. First, an instance
// serves only data its own worker considers fresh — the same
// gates the worker fetches by, so the swarm never launders
// stale rows into fresh ones. Second, nothing applies to the
// real tables without passing through staging: signature
// verified against the signer's pinned key, payload
// strictly decoded into the row shape, and the IDs inside the
// payload checked against the signed key, so a valid signature
// cannot be replayed onto another key. Spot checks (swarm_trust.go)
// re-fetch against ESI before staged rows apply; renders read
// the real tables only, never staging.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
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
	swarmKindMembers     = "swarm_members"
	swarmKindHistory     = "market_history"
	swarmKindTypeName    = "type_name"
	swarmKindCorporation = "corporation_record"
	swarmKindAlliance    = "alliance_record"
	swarmKindStructure   = "structure_name"
)

// validSwarmKind reports whether kind is data the swarm carries
// (not the membership list, which is gossip, not data).
func validSwarmKind(kind string) bool {
	switch kind {
	case swarmKindHistory, swarmKindTypeName, swarmKindCorporation, swarmKindAlliance, swarmKindStructure:
		return true
	default:
		return false
	}
}

// swarmURL is a parsed peer URL: scheme and host only, so
// validation never depends on paths or query strings.
type swarmURL struct {
	scheme string
	host   string
}

// parseSwarmURL parses a peer URL strictly: a scheme, a host,
// nothing else trusted.
func parseSwarmURL(raw string) (swarmURL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return swarmURL{}, fmt.Errorf("not a URL")
	}
	return swarmURL{scheme: strings.ToLower(u.Scheme), host: u.Hostname()}, nil
}

// parseSwarmID reads one positive EVE ID from a key part.
// Malformed IDs refuse; a shared key must name its entity.
func parseSwarmID(part string) (int64, bool) {
	if strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(part, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// serveSwarmKey reads one shared key from the local tables for a
// peer pull. It serves only rows the worker holds fresh: history
// inside its refetch gate, records in the ready state, resolved
// structure names, any held type name. Anything else is a miss —
// the peer fetches it itself, and the swarm never moves stale
// data sideways.
func (app *Application) serveSwarmKey(ctx context.Context, kind, key string) (payload string, fetchedAt time.Time, ok bool) {
	switch kind {
	case swarmKindHistory:
		return app.serveSwarmHistory(ctx, key)
	case swarmKindTypeName:
		return app.serveSwarmTypeName(ctx, key)
	case swarmKindCorporation:
		return app.serveSwarmCorporation(ctx, key)
	case swarmKindAlliance:
		return app.serveSwarmAlliance(ctx, key)
	case swarmKindStructure:
		return app.serveSwarmStructure(ctx, key)
	default:
		return "", time.Time{}, false
	}
}

// swarmHistoryPayload is market_history on the wire: the pair,
// then its day rows newest first, bounded.
type swarmHistoryPayload struct {
	RegionID int64             `json:"region_id"`
	TypeID   int64             `json:"type_id"`
	Days     []swarmHistoryDay `json:"days"`
}

// swarmHistoryDay is one daily aggregate.
type swarmHistoryDay struct {
	Date       string  `json:"date"`
	Average    float64 `json:"average"`
	Highest    float64 `json:"highest"`
	Lowest     float64 `json:"lowest"`
	Volume     int64   `json:"volume"`
	OrderCount int64   `json:"order_count"`
}

// maxSwarmHistoryDays caps one history entry: a year of daily
// rows plus slack, never an unbounded table dump.
const maxSwarmHistoryDays = 400

func (app *Application) serveSwarmHistory(ctx context.Context, key string) (string, time.Time, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return "", time.Time{}, false
	}
	regionID, ok := parseSwarmID(parts[0])
	if !ok {
		return "", time.Time{}, false
	}
	typeID, ok := parseSwarmID(parts[1])
	if !ok {
		return "", time.Time{}, false
	}
	fetchKind := marketFetchKind("history", marketKey{RegionID: regionID, TypeID: typeID})
	state, err := app.queries.GetMarketFetchState(ctx, fetchKind)
	if err != nil || state.State != fetchStateOK {
		return "", time.Time{}, false
	}
	if time.Since(state.AttemptedAt) >= historyRefetchGate {
		return "", time.Time{}, false
	}
	rows, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{
		RegionID: regionID, TypeID: typeID, RowLimit: maxSwarmHistoryDays,
	})
	if err != nil || len(rows) == 0 {
		return "", time.Time{}, false
	}
	out := swarmHistoryPayload{RegionID: regionID, TypeID: typeID}
	for _, row := range rows {
		out.Days = append(out.Days, swarmHistoryDay{
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

// swarmTypeNamePayload is type_name on the wire.
type swarmTypeNamePayload struct {
	TypeID int64  `json:"type_id"`
	Name   string `json:"name"`
}

func (app *Application) serveSwarmTypeName(ctx context.Context, key string) (string, time.Time, bool) {
	typeID, ok := parseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	name, err := app.queries.GetTypeName(ctx, typeID)
	if err != nil || name == "" {
		return "", time.Time{}, false
	}
	// Type names carry no stored timestamp (they are effectively
	// immutable); the serve time travels instead, and receivers
	// apply names only where they hold none.
	raw, err := json.Marshal(swarmTypeNamePayload{TypeID: typeID, Name: name})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), time.Now().UTC(), true
}

// swarmOrgPayload is a corporation or alliance record on the
// wire: the stored ESI payload verbatim, so the receiver
// decodes exactly what the fetcher stored.
type swarmOrgPayload struct {
	ID      int64  `json:"id"`
	Payload string `json:"payload"`
}

func (app *Application) serveSwarmCorporation(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := parseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	rec, err := app.queries.GetCorporationRecord(ctx, id)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" || !rec.FetchedAt.Valid {
		return "", time.Time{}, false
	}
	raw, err := json.Marshal(swarmOrgPayload{ID: id, Payload: rec.Payload})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), rec.FetchedAt.Time, true
}

func (app *Application) serveSwarmAlliance(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := parseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	rec, err := app.queries.GetAllianceRecord(ctx, id)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" || !rec.FetchedAt.Valid {
		return "", time.Time{}, false
	}
	raw, err := json.Marshal(swarmOrgPayload{ID: id, Payload: rec.Payload})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), rec.FetchedAt.Time, true
}

// swarmStructurePayload is structure_name on the wire.
type swarmStructurePayload struct {
	StructureID int64  `json:"structure_id"`
	Name        string `json:"name"`
}

func (app *Application) serveSwarmStructure(ctx context.Context, key string) (string, time.Time, bool) {
	id, ok := parseSwarmID(key)
	if !ok {
		return "", time.Time{}, false
	}
	row, err := app.queries.GetStructureName(ctx, id)
	if err != nil || row.State != esi.StructureResolved || row.Name == "" || !row.ResolvedAt.Valid {
		return "", time.Time{}, false
	}
	raw, err := json.Marshal(swarmStructurePayload{StructureID: id, Name: row.Name})
	if err != nil {
		return "", time.Time{}, false
	}
	return string(raw), row.ResolvedAt.Time, true
}

// stageSwarmEntry verifies one pulled entry and stages it for a
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
func (app *Application) stageSwarmEntry(ctx context.Context, entry swarmSignedEntry, from string, now time.Time) error {
	if !validSwarmKind(entry.Kind) || entry.Key == "" {
		return fmt.Errorf("swarm: stage %s %s: unknown kind", entry.Kind, entry.Key)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, entry.Signer)
	if err != nil {
		app.countSwarmShapeReject(ctx, from)
		return fmt.Errorf("swarm: stage %s %s: unknown signer", entry.Kind, entry.Key)
	}
	if !verifySwarmEntry(entry, peer.PublicKey) {
		app.countSwarmShapeReject(ctx, from)
		return fmt.Errorf("swarm: stage %s %s: signature does not verify", entry.Kind, entry.Key)
	}
	if err := checkSwarmPayload(entry.Kind, entry.Key, entry.Payload); err != nil {
		app.countSwarmShapeReject(ctx, entry.Signer)
		return err
	}
	fetchedAt := time.Unix(entry.FetchedAt, 0).UTC()
	if err := app.queries.StageSwarmEntry(ctx, db.StageSwarmEntryParams{
		Kind: entry.Kind, Key: entry.Key, Payload: entry.Payload,
		Signer: entry.Signer, Signature: entry.Signature,
		FetchedAt: fetchedAt, ReceivedAt: now,
	}); err != nil {
		return fmt.Errorf("swarm: stage %s %s: %w", entry.Kind, entry.Key, err)
	}
	return nil
}

// checkSwarmPayload strictly decodes one entry payload into its
// row shape and binds it to the signed key: every ID inside
// must match, every number must be in range, every day row must
// be shaped like the table row it would become. This is format
// validation against the existing row shapes, not sanitizing —
// the values stay exactly what the signer sent, for the spot
// check to judge.
func checkSwarmPayload(kind, key, payload string) error {
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	switch kind {
	case swarmKindHistory:
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		regionID, ok := parseSwarmID(parts[0])
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		typeID, ok := parseSwarmID(parts[1])
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body swarmHistoryPayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.RegionID != regionID || body.TypeID != typeID {
			return fmt.Errorf("swarm: stage %s %s: payload names another pair", kind, key)
		}
		if len(body.Days) == 0 || len(body.Days) > maxSwarmHistoryDays {
			return fmt.Errorf("swarm: stage %s %s: %d day rows", kind, key, len(body.Days))
		}
		for _, day := range body.Days {
			if day.Date == "" || day.Average < 0 || day.Highest < 0 || day.Lowest < 0 || day.Volume < 0 || day.OrderCount < 0 {
				return fmt.Errorf("swarm: stage %s %s: malformed day row", kind, key)
			}
		}
	case swarmKindTypeName:
		typeID, ok := parseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body swarmTypeNamePayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.TypeID != typeID || body.Name == "" || len(body.Name) > 256 {
			return fmt.Errorf("swarm: stage %s %s: payload names another type", kind, key)
		}
	case swarmKindCorporation, swarmKindAlliance:
		id, ok := parseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body swarmOrgPayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.ID != id || body.Payload == "" || len(body.Payload) > maxSwarmPayloadBytes {
			return fmt.Errorf("swarm: stage %s %s: payload names another record", kind, key)
		}
		// The record body must itself be JSON (it is stored
		// verbatim and decoded at render): a non-JSON body
		// would poison the render path, not the swarm table.
		var probe any
		if err := json.Unmarshal([]byte(body.Payload), &probe); err != nil {
			return fmt.Errorf("swarm: stage %s %s: record body is not JSON", kind, key)
		}
	case swarmKindStructure:
		id, ok := parseSwarmID(key)
		if !ok {
			return fmt.Errorf("swarm: stage %s %s: malformed key", kind, key)
		}
		var body swarmStructurePayload
		if err := dec.Decode(&body); err != nil {
			return fmt.Errorf("swarm: stage %s %s: unreadable payload", kind, key)
		}
		if body.StructureID != id || body.Name == "" || len(body.Name) > 256 {
			return fmt.Errorf("swarm: stage %s %s: payload names another structure", kind, key)
		}
	default:
		return fmt.Errorf("swarm: stage %s %s: unknown kind", kind, key)
	}
	return nil
}

// applySwarmEntry writes one spot-checked entry to the real
// tables through the existing upserts, and settles the fetch
// state the worker gates on — so a swarm-supplied row stops the
// worker fetching it itself, which is the saved ESI call. Only
// the trust pass calls this, after a spot check passed.
func (app *Application) applySwarmEntry(ctx context.Context, kind, key, payload string, now time.Time) error {
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	switch kind {
	case swarmKindHistory:
		var body swarmHistoryPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		for _, day := range body.Days {
			if err := app.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
				RegionID: body.RegionID, TypeID: body.TypeID, Date: day.Date,
				Average: day.Average, Highest: day.Highest, Lowest: day.Lowest,
				Volume: day.Volume, OrderCount: day.OrderCount,
			}); err != nil {
				return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
			}
		}
		app.recordMarketFetch(ctx, marketFetchKind("history", marketKey{RegionID: body.RegionID, TypeID: body.TypeID}), fetchStateOK, "swarm")
		app.addSwarmCounter(ctx, swarmCounterAppliedHistory, 1)
	case swarmKindTypeName:
		var body swarmTypeNamePayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		if err := app.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: body.TypeID, Name: body.Name}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.addSwarmCounter(ctx, swarmCounterAppliedNames, 1)
	case swarmKindCorporation:
		var body swarmOrgPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		if err := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
			CorporationID: body.ID, Payload: body.Payload, State: orgStateReady, FetchedAt: timeSet(now),
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.addSwarmCounter(ctx, swarmCounterAppliedRecords, 1)
	case swarmKindAlliance:
		var body swarmOrgPayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		if err := app.queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
			AllianceID: body.ID, Payload: body.Payload, State: orgStateReady, FetchedAt: timeSet(now),
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.addSwarmCounter(ctx, swarmCounterAppliedRecords, 1)
	case swarmKindStructure:
		var body swarmStructurePayload
		if err := dec.Decode(&body); err != nil {
			return err
		}
		if err := app.queries.SetStructureName(ctx, db.SetStructureNameParams{
			StructureID: body.StructureID, Name: body.Name, State: esi.StructureResolved,
			ResolvedAt: timeSet(now), Source: esi.StructureSourceCommunity,
		}); err != nil {
			return fmt.Errorf("swarm: apply %s %s: %w", kind, key, err)
		}
		app.addSwarmCounter(ctx, swarmCounterAppliedNames, 1)
	default:
		return fmt.Errorf("swarm: apply %s %s: unknown kind", kind, key)
	}
	if err := app.queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
		logging.Errorf("swarm: apply %s %s: unstaged: %v", kind, key, err)
	}
	return nil
}

// discardSwarmEntry drops one staged entry that failed its spot
// check, and counts it against the signer. The row never reaches
// the real tables: failed checks are discarded, never served.
func (app *Application) discardSwarmEntry(ctx context.Context, kind, key, signer string) {
	if err := app.queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: kind, Key: key}); err != nil {
		logging.Errorf("swarm: discard %s %s: %v", kind, key, err)
	}
	app.countSwarmCheck(ctx, signer, false)
	app.addSwarmCounter(ctx, swarmCounterSpotFailed, 1)
}
