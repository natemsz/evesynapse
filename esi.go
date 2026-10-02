package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Minimal ESI client. ESI is at https://esi.evetech.net; every request
// carries a descriptive User-Agent (CCP asks third parties to identify
// themselves).
const (
	esiBaseURL   = "https://esi.evetech.net"
	esiUserAgent = "EveSynapse/0.2 (dev)"
)

// Snapshot kinds stored in character_snapshots.
const (
	snapSkills     = "skills"
	snapSkillqueue = "skillqueue"
	snapWallet     = "wallet"
)

// errESIErrorLimit marks ESI's error-limit responses (420/429): callers
// (notably the worker) should back off rather than keep hammering.
var errESIErrorLimit = errors.New("ESI error limit")

// esiCharacter is the public character sheet: GET /characters/{id}/
// (returned fields EveSynapse currently consumes).
type esiCharacter struct {
	Name           string  `json:"name"`
	CorporationID  int64   `json:"corporation_id"`
	Birthday       string  `json:"birthday"` // RFC3339
	SecurityStatus float64 `json:"security_status"`
}

// esiCorporation is GET /corporations/{id}/ (public, unauthenticated).
// Corporation IDs are stable, so responses are cached per the ESI
// Expires header (see corporation.go).
type esiCorporation struct {
	Name          string  `json:"name"`
	Ticker        string  `json:"ticker"`
	MemberCount   int64   `json:"member_count"`
	CEOID         int64   `json:"ceo_id"`
	CreatorID     int64   `json:"creator_id"`
	AllianceID    int64   `json:"alliance_id"` // 0 when not in an alliance
	HomeStationID int64   `json:"home_station_id"`
	TaxRate       float64 `json:"tax_rate"`     // fraction: 0.10 = 10%
	DateFounded   string  `json:"date_founded"` // RFC3339
	Description   string  `json:"description"`  // contains EVE-flavored HTML
	URL           string  `json:"url"`
}

// esiAlliance is GET /alliances/{id}/ (the slice we consume).
type esiAlliance struct {
	Name   string `json:"name"`
	Ticker string `json:"ticker"`
}

// esiStation is GET /universe/stations/{id}/ (the slice we consume).
type esiStation struct {
	Name string `json:"name"`
}

// esiSkill is one entry of GET /characters/{id}/skills/.
type esiSkill struct {
	SkillID            int64 `json:"skill_id"`
	SkillpointsInSkill int64 `json:"skillpoints_in_skill"`
	ActiveSkillLevel   int   `json:"active_skill_level"`
	TrainedSkillLevel  int   `json:"trained_skill_level"`
}

// esiSkills is GET /characters/{id}/skills/.
type esiSkills struct {
	TotalSP       int64      `json:"total_sp"`
	UnallocatedSP int64      `json:"unallocated_sp"`
	Skills        []esiSkill `json:"skills"`
}

// esiSkillqueueEntry is one entry of GET /characters/{id}/skillqueue/.
// training_start_sp can be null (e.g. for injected skills); the rest are
// always present on live entries.
type esiSkillqueueEntry struct {
	SkillID         int64  `json:"skill_id"`
	FinishedLevel   int    `json:"finished_level"`
	QueuePosition   int    `json:"queue_position"`
	StartDate       string `json:"start_date"`  // RFC3339, may be empty
	FinishDate      string `json:"finish_date"` // RFC3339, may be empty
	TrainingStartSP *int64 `json:"training_start_sp"`
	LevelStartSP    int64  `json:"level_start_sp"`
	LevelEndSP      int64  `json:"level_end_sp"`
}

// esiSkillqueue is GET /characters/{id}/skillqueue/.
type esiSkillqueue []esiSkillqueueEntry

// esiType is the slice of GET /universe/types/{id}/ we consume.
type esiType struct {
	Name string `json:"name"`
}

// esiGet performs a GET against ESI and JSON-decodes the response into
// out. The access token is sent as a Bearer header when non-empty and
// is never logged. Errors carry the path and status, nothing sensitive.
func esiGet(ctx context.Context, accessToken, path string, out any) error {
	body, _, err := esiFetchRaw(ctx, accessToken, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ESI GET %s: decode: %w", path, err)
	}
	return nil
}

// esiFetchRaw GETs path from ESI and returns the raw body and response
// headers (Expires drives snapshot bookkeeping). Non-200 statuses are
// errors; 420/429 wrap errESIErrorLimit.
func esiFetchRaw(ctx context.Context, accessToken, path string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, esiBaseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", esiUserAgent)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := loginHTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: read body: %w", path, err)
	}
	if resp.StatusCode == 420 || resp.StatusCode == http.StatusTooManyRequests {
		return nil, resp.Header, fmt.Errorf("ESI GET %s: status %d: %w", path, resp.StatusCode, errESIErrorLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, fmt.Errorf("ESI GET %s: status %d", path, resp.StatusCode)
	}
	return body, resp.Header, nil
}

// snapshotPath maps a snapshot kind to its ESI path for a character.
func snapshotPath(characterID int64, kind string) string {
	switch kind {
	case snapSkills:
		return fmt.Sprintf("/characters/%d/skills/", characterID)
	case snapSkillqueue:
		return fmt.Sprintf("/characters/%d/skillqueue/", characterID)
	case snapWallet:
		return fmt.Sprintf("/characters/%d/wallet/", characterID)
	}
	return ""
}

// snapshotFresh reports whether the snapshot's cached_until is still in
// the future. A missing/unparseable expiry counts as stale.
func snapshotFresh(snap db.CharacterSnapshot) bool {
	if !snap.CachedUntil.Valid || snap.CachedUntil.String == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, snap.CachedUntil.String)
	return err == nil && time.Now().Before(until)
}

// fetchAndStoreSnapshot fetches the kind's ESI path with a valid token
// and stores the raw payload, honoring the response Expires header as
// cached_until (fallback: now + 5 minutes when ESI doesn't send one).
func (app *application) fetchAndStoreSnapshot(ctx context.Context, ch db.Character, kind string) ([]byte, error) {
	path := snapshotPath(ch.CharacterID, kind)
	if path == "" {
		return nil, fmt.Errorf("unknown snapshot kind %q", kind)
	}

	token, err := app.validAccessToken(ctx, ch)
	if err != nil {
		return nil, err
	}

	body, header, err := esiFetchRaw(ctx, token, path)
	if err != nil {
		return nil, err
	}

	cachedUntil := time.Now().Add(5 * time.Minute)
	if exp := header.Get("Expires"); exp != "" {
		if t, perr := http.ParseTime(exp); perr == nil {
			cachedUntil = t
		}
	}

	now := time.Now().UTC()
	if err := app.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: ch.CharacterID,
		Kind:        kind,
		Payload:     string(body),
		FetchedAt:   now.Format(time.RFC3339),
		CachedUntil: sql.NullString{String: cachedUntil.UTC().Format(time.RFC3339), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("store %s snapshot for character %d: %w", kind, ch.CharacterID, err)
	}
	return body, nil
}

// getCached returns ESI data for (character, kind), decoded into out.
// A snapshot whose cached_until is in the future is served as-is;
// otherwise a live fetch refreshes it. If the fetch fails but a
// snapshot exists, the stale snapshot is served (and logged). The
// fresh fetch goes through validAccessToken, so expired access tokens
// are renewed transparently.
func (app *application) getCached(ctx context.Context, ch db.Character, kind string, out any) error {
	if snapshotPath(ch.CharacterID, kind) == "" {
		return fmt.Errorf("unknown snapshot kind %q", kind)
	}

	snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	haveSnap := serr == nil
	if serr != nil && !errors.Is(serr, sql.ErrNoRows) {
		log.Printf("esi: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	if haveSnap && snapshotFresh(snap) {
		if err := json.Unmarshal([]byte(snap.Payload), out); err == nil {
			return nil
		} else {
			log.Printf("esi: decode cached %s for character %d: %v (refetching)", kind, ch.CharacterID, err)
		}
	}

	body, err := app.fetchAndStoreSnapshot(ctx, ch, kind)
	if err != nil {
		if haveSnap {
			log.Printf("esi: %s fetch for character %d failed (%v); serving stale snapshot", kind, ch.CharacterID, err)
			if derr := json.Unmarshal([]byte(snap.Payload), out); derr == nil {
				return nil
			}
		}
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ESI %s for character %d: decode: %w", kind, ch.CharacterID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Type-name resolution: skill/item IDs -> display names.
// ---------------------------------------------------------------------------

// maxTypeNameLookups bounds the number of ESI /universe/types lookups a
// single resolveTypeNames call will make; anything beyond is left as
// "Type #<id>" until a later render.
const maxTypeNameLookups = 60

// typeName returns the display name for one EVE type ID, consulting
// the in-process map, then the type_names table, then ESI (public
// endpoint, unauthenticated). Falls back to "Type #<id>".
func (app *application) typeName(ctx context.Context, id int64) string {
	names := app.resolveTypeNames(ctx, []int64{id})
	if name, ok := names[id]; ok {
		return name
	}
	return fmt.Sprintf("Type #%d", id)
}

// resolveTypeNames resolves a batch of type IDs. Cached names (memory,
// then DB) are returned for everything requested; uncached IDs are
// fetched from ESI, at most maxTypeNameLookups per call, and persisted.
// Names that can't be resolved are simply absent from the result.
func (app *application) resolveTypeNames(ctx context.Context, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))

	// Dedupe, keep order for the lookup budget.
	seen := make(map[int64]bool, len(ids))
	var todo []int64
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		todo = append(todo, id)
	}

	app.typeNamesMu.RLock()
	var missing []int64
	for _, id := range todo {
		if name, ok := app.typeNames[id]; ok {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	app.typeNamesMu.RUnlock()

	var fetch []int64
	for _, id := range missing {
		if name, err := app.queries.GetTypeName(ctx, id); err == nil && name != "" {
			out[id] = name
			app.typeNamesMu.Lock()
			app.typeNames[id] = name
			app.typeNamesMu.Unlock()
		} else {
			fetch = append(fetch, id)
		}
	}

	lookups := 0
	for _, id := range fetch {
		if lookups >= maxTypeNameLookups {
			break
		}
		lookups++
		var t esiType
		if err := esiGet(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t); err != nil {
			log.Printf("esi: type name lookup %d: %v", id, err)
			continue
		}
		if t.Name == "" {
			continue
		}
		out[id] = t.Name
		app.typeNamesMu.Lock()
		app.typeNames[id] = t.Name
		app.typeNamesMu.Unlock()
		if err := app.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: id, Name: t.Name}); err != nil {
			log.Printf("esi: persist type name %d: %v", id, err)
		}
	}
	return out
}

// sortedSkillIDs is a small helper used by the home page to pick the
// heaviest skills first.
func sortedSkillIDs(skills []esiSkill) []int64 {
	sorted := make([]esiSkill, len(skills))
	copy(sorted, skills)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SkillpointsInSkill != sorted[j].SkillpointsInSkill {
			return sorted[i].SkillpointsInSkill > sorted[j].SkillpointsInSkill
		}
		return sorted[i].SkillID < sorted[j].SkillID
	})
	ids := make([]int64, len(sorted))
	for i, s := range sorted {
		ids[i] = s.SkillID
	}
	return ids
}

// formatISK renders a wallet balance with thousands separators and two
// decimals, e.g. 1234567.891 -> "1,234,567.89".
func formatISK(balance float64) string {
	neg := balance < 0
	if neg {
		balance = -balance
	}
	whole := int64(balance)
	cents := int64((balance-float64(whole))*100 + 0.5)
	if cents == 100 {
		whole++
		cents = 0
	}
	s := groupThousands(strconv.FormatInt(whole, 10))
	if neg {
		s = "-" + s
	}
	return fmt.Sprintf("%s.%02d", s, cents)
}

// formatInt renders an integer with thousands separators.
func formatInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := groupThousands(strconv.FormatInt(n, 10))
	if neg {
		return "-" + s
	}
	return s
}

func groupThousands(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b []byte
	lead := n % 3
	if lead > 0 {
		b = append(b, digits[:lead]...)
	}
	for i := lead; i < n; i += 3 {
		if len(b) > 0 {
			b = append(b, ',')
		}
		b = append(b, digits[i:i+3]...)
	}
	return string(b)
}

// romanLevel renders a skill level the EVE way (I..V).
func romanLevel(level int) string {
	switch level {
	case 1:
		return "I"
	case 2:
		return "II"
	case 3:
		return "III"
	case 4:
		return "IV"
	case 5:
		return "V"
	default:
		return strconv.Itoa(level)
	}
}
