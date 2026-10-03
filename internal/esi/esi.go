// Package esi is EveSynapse's ESI client: the HTTP layer for CCP's
// ESI API, the per-character snapshot cache (raw ESI payloads stored
// in character_snapshots, served inside ESI's own cache window), and
// the type/group/place name-resolution caches the page handlers read
// from.
//
// The two-tier name-resolution design lives here intact: a network
// tier (ResolveTypeNames, ResolveTypeGroups, ResolveGroupNames,
// PlaceName) that fetches and caches what the local caches lack,
// capped per call, and a cache-only tier (CachedTypeNames,
// CachedTypeGroups, CachedGroupNames, CachedPlaceName) that never
// touches the network, so page renders can never block on ESI.
//
// "Local" now means, in order: the in-process maps, the SDE static
// data tables (sde_types/sde_groups/sde_stations/sde_systems — the
// app's local copy of CCP's data dump, the primary source), then
// the type_names drip-feed table. The network tier still exists
// below all three for anything the SDE lacks (notably
// player-structure names).
//
// The client performs no account logic of its own: turning a
// character row into a usable access token is the caller's job,
// injected at construction (see TokenFunc), so this package never
// imports the web application that hosts it.
package esi

import (
	"bytes"
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
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// ESI is at https://esi.evetech.net; every request carries a
// descriptive User-Agent (CCP asks third parties to identify
// themselves).
const (
	baseURL   = "https://esi.evetech.net"
	userAgent = "EveSynapse/0.2 (dev)"
)

// Snapshot kinds stored in character_snapshots.
const (
	SnapSkills     = "skills"
	SnapSkillqueue = "skillqueue"
	SnapWallet     = "wallet"
	SnapAssets     = "assets"

	// Module sweep, cluster 1 (character): live-state endpoints.
	SnapLocation  = "location"
	SnapShip      = "ship"
	SnapOnline    = "online"
	SnapClones    = "clones"
	SnapImplants  = "implants"
	SnapFittings  = "fittings"
	SnapFatigue   = "fatigue"
	SnapKillmails = "killmails" // recent list (id+hash pairs); details live in killmail_details

	// Module sweep, cluster 2 (corporation): corporation endpoints,
	// stored per viewing character (the character whose token
	// fetched them, against that character's corporation) under
	// corp_* kinds. Journal/transaction kinds are per wallet
	// division: corp_journal_1..7 and corp_txns_1..7 (see
	// CorpJournalKind / CorpTxnsKind).
	SnapCorpInfo           = "corp_info" // GET /corporations/{id}/ (public payload)
	SnapCorpMembers        = "corp_members"
	SnapCorpMemberTracking = "corp_membertracking"
	SnapCorpWallets        = "corp_wallets"
	SnapCorpOrders         = "corp_orders"
	SnapCorpAssets         = "corp_assets"
	SnapCorpStructures     = "corp_structures"
	SnapCorpKillmails      = "corp_killmails" // recent list (id+hash pairs); details share killmail_details

	// Snapshot-kind prefixes for the per-division wallet kinds.
	SnapCorpJournalPrefix = "corp_journal_"
	SnapCorpTxnsPrefix    = "corp_txns_"
)

// CorpJournalKind is the snapshot kind holding wallet division d's
// journal (d in 1..7).
func CorpJournalKind(division int64) string {
	return SnapCorpJournalPrefix + strconv.FormatInt(division, 10)
}

// CorpTxnsKind is the snapshot kind holding wallet division d's
// transactions (d in 1..7).
func CorpTxnsKind(division int64) string {
	return SnapCorpTxnsPrefix + strconv.FormatInt(division, 10)
}

// ErrErrorLimit marks ESI's error-limit responses (420/429): callers
// (notably the worker) should back off rather than keep hammering.
var ErrErrorLimit = errors.New("ESI error limit")

// StatusError is a non-200 ESI response that isn't the error limit:
// the worker distinguishes 403 (an in-game role or a scope the
// character hasn't granted) from other failures so role-gated
// corporation endpoints can record a "role missing" state instead
// of retrying forever. The message matches the plain format the
// client has always produced, so log output is unchanged.
type StatusError struct {
	Method string // "GET" or "POST"
	Path   string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("ESI %s %s: status %d", e.Method, e.Path, e.Code)
}

// IsForbidden reports whether err is an ESI 403.
func IsForbidden(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

// TokenFunc returns an access token for the character that is safe
// to use right now (refreshing and persisting a rotated pair first
// when needed). Token values must never be logged.
type TokenFunc func(ctx context.Context, ch db.Character) (string, error)

// Client is the ESI client: HTTP access, the snapshot cache, and the
// in-process name caches (type names, type→group links, group
// names, place names). Construct with New; the zero value is not
// usable.
type Client struct {
	http    *http.Client
	queries *db.Queries
	tokens  TokenFunc

	// In-process cache of EVE type ID → name (backed by the
	// type_names table).
	typeNamesMu sync.RWMutex
	typeNames   map[int64]string

	// In-process cache of EVE type ID → group ID, populated as a
	// side effect of type fetches; feeds skill-sheet grouping.
	typeGroupsMu sync.RWMutex
	typeGroups   map[int64]int64

	// In-process cache of EVE group ID → name for skill-sheet
	// section headers; group names are stable public data.
	groupNamesMu sync.Mutex
	groupNames   map[int64]string

	// In-process cache of station/system ID → name for asset
	// location titles; both are stable public data.
	placeMu    sync.Mutex
	placeNames map[int64]string

	// In-process cache of character ID → name, warmed by the
	// worker from killmail details so the killmail list can label
	// victims and final-blow attackers. Public data.
	charNamesMu sync.RWMutex
	charNames   map[int64]string
}

// New builds a Client. httpClient performs every ESI request (the
// caller shares it with the SSO/JWKS calls); queries is the sqlc
// handle for the snapshot and type-name tables; tokens supplies
// access tokens for authenticated character endpoints.
func New(httpClient *http.Client, queries *db.Queries, tokens TokenFunc) *Client {
	return &Client{
		http:       httpClient,
		queries:    queries,
		tokens:     tokens,
		typeNames:  make(map[int64]string),
		typeGroups: make(map[int64]int64),
		groupNames: make(map[int64]string),
		placeNames: make(map[int64]string),
		charNames:  make(map[int64]string),
	}
}

// ---------------------------------------------------------------------------
// ESI payload types (the slices EveSynapse consumes).
// ---------------------------------------------------------------------------

// Character is the public character sheet: GET /characters/{id}/
// (returned fields EveSynapse currently consumes).
type Character struct {
	Name           string  `json:"name"`
	CorporationID  int64   `json:"corporation_id"`
	Birthday       string  `json:"birthday"` // RFC3339
	SecurityStatus float64 `json:"security_status"`
}

// Corporation is GET /corporations/{id}/ (public, unauthenticated).
// Corporation IDs are stable, so responses are cached per the ESI
// Expires header (see the corporation page handler).
type Corporation struct {
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

// Alliance is GET /alliances/{id}/ (the slice we consume).
type Alliance struct {
	Name   string `json:"name"`
	Ticker string `json:"ticker"`
}

// Station is GET /universe/stations/{id}/ (the slice we consume).
type Station struct {
	Name string `json:"name"`
}

// Skill is one entry of GET /characters/{id}/skills/.
type Skill struct {
	SkillID            int64 `json:"skill_id"`
	SkillpointsInSkill int64 `json:"skillpoints_in_skill"`
	ActiveSkillLevel   int   `json:"active_skill_level"`
	TrainedSkillLevel  int   `json:"trained_skill_level"`
}

// Skills is GET /characters/{id}/skills/.
type Skills struct {
	TotalSP       int64   `json:"total_sp"`
	UnallocatedSP int64   `json:"unallocated_sp"`
	Skills        []Skill `json:"skills"`
}

// SkillqueueEntry is one entry of GET /characters/{id}/skillqueue/.
// training_start_sp can be null (e.g. for injected skills); the rest are
// always present on live entries.
type SkillqueueEntry struct {
	SkillID         int64  `json:"skill_id"`
	FinishedLevel   int    `json:"finished_level"`
	QueuePosition   int    `json:"queue_position"`
	StartDate       string `json:"start_date"`  // RFC3339, may be empty
	FinishDate      string `json:"finish_date"` // RFC3339, may be empty
	TrainingStartSP *int64 `json:"training_start_sp"`
	LevelStartSP    int64  `json:"level_start_sp"`
	LevelEndSP      int64  `json:"level_end_sp"`
}

// Skillqueue is GET /characters/{id}/skillqueue/.
type Skillqueue []SkillqueueEntry

// Asset is one entry of GET /characters/{id}/assets/. is_blueprint_copy
// is only present on blueprint items; absent decodes as false.
type Asset struct {
	ItemID          int64  `json:"item_id"`
	TypeID          int64  `json:"type_id"`
	Quantity        int64  `json:"quantity"`
	LocationID      int64  `json:"location_id"`
	LocationType    string `json:"location_type"` // station|solar_system|structure|other|item
	LocationFlag    string `json:"location_flag"`
	IsSingleton     bool   `json:"is_singleton"`
	IsBlueprintCopy bool   `json:"is_blueprint_copy"`
}

// Type is the slice of GET /universe/types/{id}/ we consume.
// GroupID feeds skill grouping on the skill sheet; name-only
// consumers simply ignore it.
type Type struct {
	Name    string `json:"name"`
	GroupID int64  `json:"group_id"`
}

// Group is the slice of GET /universe/groups/{id}/ we consume.
type Group struct {
	Name string `json:"name"`
}

// UniverseIDEntry is one group entry of POST /universe/ids/;
// the response carries several groups (characters, corporations,
// inventory_types, ...), of which we consume inventory_types.
type UniverseIDEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// UniverseIDs is the slice of POST /universe/ids/ we consume.
type UniverseIDs struct {
	InventoryTypes []UniverseIDEntry `json:"inventory_types"`
}

// MarketPrice is one row of GET /markets/prices/ (CCP's market
// guide: recent average and the adjusted price used for taxes).
type MarketPrice struct {
	TypeID        int64   `json:"type_id"`
	AveragePrice  float64 `json:"average_price"`
	AdjustedPrice float64 `json:"adjusted_price"`
}

// MarketOrder is one order of GET /markets/{region}/orders/.
type MarketOrder struct {
	OrderID      int64   `json:"order_id"`
	TypeID       int64   `json:"type_id"`
	LocationID   int64   `json:"location_id"`
	SystemID     int64   `json:"system_id"`
	IsBuyOrder   bool    `json:"is_buy_order"`
	Price        float64 `json:"price"`
	VolumeTotal  int64   `json:"volume_total"`
	VolumeRemain int64   `json:"volume_remain"`
	Range        string  `json:"range"`
	Issued       string  `json:"issued"`
	Duration     int     `json:"duration"`
}

// ---------------------------------------------------------------------------
// Module sweep, cluster 1: character live-state payloads.
// ---------------------------------------------------------------------------

// Location is GET /characters/{id}/location/. station_id and
// structure_id are mutually exclusive and both absent in space.
type Location struct {
	SolarSystemID int64 `json:"solar_system_id"`
	StationID     int64 `json:"station_id"`
	StructureID   int64 `json:"structure_id"`
}

// Ship is GET /characters/{id}/ship/.
type Ship struct {
	ShipTypeID int64  `json:"ship_type_id"`
	ShipItemID int64  `json:"ship_item_id"`
	ShipName   string `json:"ship_name"`
}

// Online is GET /characters/{id}/online/.
type Online struct {
	Online     bool   `json:"online"`
	LastLogin  string `json:"last_login"`  // RFC3339
	LastLogout string `json:"last_logout"` // RFC3339
	Logins     int64  `json:"logins"`
}

// CloneHome is the home_location object of GET /characters/{id}/clones/.
type CloneHome struct {
	LocationID   int64  `json:"location_id"`
	LocationType string `json:"location_type"` // station|structure
}

// JumpClone is one jump clone of GET /characters/{id}/clones/.
// Implants are type IDs in slot order.
type JumpClone struct {
	JumpCloneID  int64   `json:"jump_clone_id"`
	LocationID   int64   `json:"location_id"`
	LocationType string  `json:"location_type"` // station|structure
	Name         string  `json:"name"`          // pilot-given clone name, may be empty
	Implants     []int64 `json:"implants"`
}

// Clones is GET /characters/{id}/clones/.
type Clones struct {
	HomeLocation      CloneHome   `json:"home_location"`
	JumpClones        []JumpClone `json:"jump_clones"`
	LastCloneJumpDate string      `json:"last_clone_jump_date"` // RFC3339, may be empty
}

// Implants is GET /characters/{id}/implants/: active implant type
// IDs in slot order.
type Implants []int64

// Fatigue is GET /characters/{id}/fatigue/. All dates RFC3339;
// LastJumpDate may be empty for a character that never jumped.
type Fatigue struct {
	LastJumpDate          string `json:"last_jump_date"`
	JumpFatigueExpireDate string `json:"jump_fatigue_expire_date"`
	LastUpdateDate        string `json:"last_update_date"`
}

// FittingItem is one fitted module/charge of a Fitting.
type FittingItem struct {
	TypeID   int64  `json:"type_id"`
	Quantity int64  `json:"quantity"`
	Flag     string `json:"flag"` // HiSlot0, MedSlot2, DroneBay, Cargo, ...
}

// Fitting is one entry of GET /characters/{id}/fittings/.
type Fitting struct {
	FittingID  int64         `json:"fitting_id"`
	Name       string        `json:"name"`
	ShipTypeID int64         `json:"ship_type_id"`
	Items      []FittingItem `json:"items"`
}

// Fittings is GET /characters/{id}/fittings/.
type Fittings []Fitting

// KillmailRef is one entry of GET /characters/{id}/killmails/recent/:
// an ID plus the hash that unlocks the public detail endpoint.
type KillmailRef struct {
	KillmailID   int64  `json:"killmail_id"`
	KillmailHash string `json:"killmail_hash"`
}

// KillmailVictimItem is one destroyed/dropped item on a killmail
// victim. Quantities are optional in ESI (singletons omit both).
type KillmailVictimItem struct {
	ItemTypeID        int64 `json:"item_type_id"`
	QuantityDestroyed int64 `json:"quantity_destroyed"`
	QuantityDropped   int64 `json:"quantity_dropped"`
}

// KillmailVictim is the victim block of a killmail detail.
type KillmailVictim struct {
	CharacterID   int64                `json:"character_id"` // 0 for NPC victims
	CorporationID int64                `json:"corporation_id"`
	ShipTypeID    int64                `json:"ship_type_id"`
	DamageTaken   int64                `json:"damage_taken"`
	Items         []KillmailVictimItem `json:"items"`
}

// KillmailAttacker is one attacker on a killmail detail.
// CharacterID is 0 for NPC attackers.
type KillmailAttacker struct {
	CharacterID   int64 `json:"character_id"`
	CorporationID int64 `json:"corporation_id"`
	ShipTypeID    int64 `json:"ship_type_id"`
	FinalBlow     bool  `json:"final_blow"`
	DamageDone    int64 `json:"damage_done"`
}

// Killmail is GET /killmails/{id}/{hash}/ (the detail payload).
type Killmail struct {
	KillmailID    int64              `json:"killmail_id"`
	KillmailTime  string             `json:"killmail_time"` // RFC3339
	SolarSystemID int64              `json:"solar_system_id"`
	Victim        KillmailVictim     `json:"victim"`
	Attackers     []KillmailAttacker `json:"attackers"`
}

// ---------------------------------------------------------------------------
// Module sweep, cluster 2: corporation payloads. Shapes verified
// against CCP's ESI OpenAPI document (components/schemas
// CorporationsCorporationId*). Dates are RFC3339 strings, matching
// the rest of this package.
// ---------------------------------------------------------------------------

// CorpMembers is GET /corporations/{id}/members/: the member
// character IDs (names resolve through the character-name cache).
type CorpMembers []int64

// CorpMemberTracking is one entry of GET
// /corporations/{id}/membertracking/ (Director role in-game).
// Only character_id is guaranteed; the rest fill in as CCP tracks
// the member.
type CorpMemberTracking struct {
	CharacterID int64  `json:"character_id"`
	BaseID      int64  `json:"base_id"`      // home station, 0 when unset
	LocationID  int64  `json:"location_id"`  // current station/system/structure, 0 when unknown
	LogoffDate  string `json:"logoff_date"`  // RFC3339
	LogonDate   string `json:"logon_date"`   // RFC3339, last login
	ShipTypeID  int64  `json:"ship_type_id"` // current ship type, 0 when unknown
	StartDate   string `json:"start_date"`   // RFC3339, when the member joined
}

// CorpMemberTrackings is GET /corporations/{id}/membertracking/.
type CorpMemberTrackings []CorpMemberTracking

// CorpWalletDivision is one entry of GET /corporations/{id}/wallets/.
type CorpWalletDivision struct {
	Division int64   `json:"division"` // 1..7
	Balance  float64 `json:"balance"`
}

// CorpWallets is GET /corporations/{id}/wallets/.
type CorpWallets []CorpWalletDivision

// CorpJournalEntry is one entry of GET
// /corporations/{id}/wallets/{division}/journal/. Only id, date,
// ref_type and description are guaranteed by the spec.
type CorpJournalEntry struct {
	ID            int64   `json:"id"`
	Date          string  `json:"date"` // RFC3339
	RefType       string  `json:"ref_type"`
	Amount        float64 `json:"amount"` // + into the wallet, - out of it
	Balance       float64 `json:"balance"`
	Description   string  `json:"description"`
	Reason        string  `json:"reason"`
	FirstPartyID  int64   `json:"first_party_id"`
	SecondPartyID int64   `json:"second_party_id"`
	ContextID     int64   `json:"context_id"`
	ContextType   string  `json:"context_id_type"`
}

// CorpJournal is one page of a division journal.
type CorpJournal []CorpJournalEntry

// CorpWalletTransaction is one entry of GET
// /corporations/{id}/wallets/{division}/transactions/.
type CorpWalletTransaction struct {
	TransactionID int64   `json:"transaction_id"`
	Date          string  `json:"date"` // RFC3339
	TypeID        int64   `json:"type_id"`
	LocationID    int64   `json:"location_id"`
	UnitPrice     float64 `json:"unit_price"`
	Quantity      int64   `json:"quantity"`
	ClientID      int64   `json:"client_id"` // counterparty character/corporation
	IsBuy         bool    `json:"is_buy"`
	JournalRefID  int64   `json:"journal_ref_id"`
}

// CorpWalletTransactions is one fetch of a division's transactions.
type CorpWalletTransactions []CorpWalletTransaction

// CorpOrder is one entry of GET /corporations/{id}/orders/ (open
// orders only).
type CorpOrder struct {
	OrderID      int64   `json:"order_id"`
	TypeID       int64   `json:"type_id"`
	LocationID   int64   `json:"location_id"`
	RegionID     int64   `json:"region_id"`
	IsBuyOrder   bool    `json:"is_buy_order"`
	Price        float64 `json:"price"`
	VolumeTotal  int64   `json:"volume_total"`
	VolumeRemain int64   `json:"volume_remain"`
	MinVolume    int64   `json:"min_volume"`
	Range        string  `json:"range"`
	Issued       string  `json:"issued"` // RFC3339
	Duration     int64   `json:"duration"`
	Escrow       float64 `json:"escrow"`
	WalletDiv    int64   `json:"wallet_division"`
	IssuedBy     int64   `json:"issued_by"`
}

// CorpOrders is GET /corporations/{id}/orders/ (all pages merged).
type CorpOrders []CorpOrder

// CorpStructureService is one service module of a CorpStructure.
type CorpStructureService struct {
	Name  string `json:"name"`
	State string `json:"state"` // online|offline|cleanup
}

// CorpStructure is one entry of GET /corporations/{id}/structures/
// (Station Manager role in-game). The payload carries the
// structure's name directly. Optional timers (fuel, reinforce
// transitions, unanchoring) are empty strings when absent; the
// reinforce hours are pointers because 0 is a real hour.
type CorpStructure struct {
	StructureID       int64                  `json:"structure_id"`
	TypeID            int64                  `json:"type_id"`
	SystemID          int64                  `json:"system_id"`
	CorporationID     int64                  `json:"corporation_id"`
	Name              string                 `json:"name"`
	FuelExpires       string                 `json:"fuel_expires"` // RFC3339
	State             string                 `json:"state"`
	StateTimerStart   string                 `json:"state_timer_start"` // RFC3339
	StateTimerEnd     string                 `json:"state_timer_end"`   // RFC3339
	ReinforceHour     *int64                 `json:"reinforce_hour"`    // 0..23, nil when omitted
	NextReinforceHour *int64                 `json:"next_reinforce_hour"`
	NextReinforceAt   string                 `json:"next_reinforce_apply"` // RFC3339
	UnanchorsAt       string                 `json:"unanchors_at"`         // RFC3339
	Services          []CorpStructureService `json:"services"`
}

// CorpStructures is GET /corporations/{id}/structures/ (all pages
// merged).
type CorpStructures []CorpStructure

// AssetName is one entry of POST /corporations/{id}/assets/names/:
// the player-given name of a singleton item (a fitted ship, a
// renamed container). CCP reports "None" for items nobody named.
type AssetName struct {
	ItemID int64  `json:"item_id"`
	Name   string `json:"name"`
}

// ---------------------------------------------------------------------------
// HTTP layer.
// ---------------------------------------------------------------------------

// Get performs a GET against ESI and JSON-decodes the response into
// out. The access token is sent as a Bearer header when non-empty and
// is never logged. Errors carry the path and status, nothing sensitive.
func (c *Client) Get(ctx context.Context, accessToken, path string, out any) error {
	body, _, err := c.FetchRaw(ctx, accessToken, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ESI GET %s: decode: %w", path, err)
	}
	return nil
}

// FetchRaw GETs path from ESI and returns the raw body and response
// headers (Expires drives snapshot bookkeeping). Non-200 statuses are
// errors; 420/429 wrap ErrErrorLimit.
func (c *Client) FetchRaw(ctx context.Context, accessToken, path string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: read body: %w", path, err)
	}
	if resp.StatusCode == 420 || resp.StatusCode == http.StatusTooManyRequests {
		return nil, resp.Header, fmt.Errorf("ESI GET %s: status %d: %w", path, resp.StatusCode, ErrErrorLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, &StatusError{Method: http.MethodGet, Path: path, Code: resp.StatusCode}
	}
	return body, resp.Header, nil
}

// PostJSON POSTs payload as JSON to ESI and decodes the response
// into out, following FetchRaw's conventions (User-Agent header,
// 420/429 wrapped as ErrErrorLimit, other non-200 statuses as
// StatusError). Used by POST /universe/ids/ for exact name → ID
// resolution; no auth token — the endpoint is public.
func (c *Client) PostJSON(ctx context.Context, path string, payload any, out any) error {
	return c.postJSON(ctx, "", path, payload, out)
}

// postJSON is PostJSON with an optional Bearer token (sent only
// when non-empty; token values are never logged).
func (c *Client) postJSON(ctx context.Context, accessToken, path string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ESI POST %s: encode: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ESI POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("ESI POST %s: read body: %w", path, err)
	}
	if resp.StatusCode == 420 || resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("ESI POST %s: status %d: %w", path, resp.StatusCode, ErrErrorLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Method: http.MethodPost, Path: path, Code: resp.StatusCode}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ESI POST %s: decode: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Per-character snapshot cache.
// ---------------------------------------------------------------------------

// snapshotPath maps a snapshot kind to its ESI path for a character.
func snapshotPath(characterID int64, kind string) string {
	switch kind {
	case SnapSkills:
		return fmt.Sprintf("/characters/%d/skills/", characterID)
	case SnapSkillqueue:
		return fmt.Sprintf("/characters/%d/skillqueue/", characterID)
	case SnapWallet:
		return fmt.Sprintf("/characters/%d/wallet/", characterID)
	case SnapAssets:
		return fmt.Sprintf("/characters/%d/assets/", characterID)
	case SnapLocation:
		return fmt.Sprintf("/characters/%d/location/", characterID)
	case SnapShip:
		return fmt.Sprintf("/characters/%d/ship/", characterID)
	case SnapOnline:
		return fmt.Sprintf("/characters/%d/online/", characterID)
	case SnapClones:
		return fmt.Sprintf("/characters/%d/clones/", characterID)
	case SnapImplants:
		return fmt.Sprintf("/characters/%d/implants/", characterID)
	case SnapFittings:
		return fmt.Sprintf("/characters/%d/fittings/", characterID)
	case SnapFatigue:
		return fmt.Sprintf("/characters/%d/fatigue/", characterID)
	case SnapKillmails:
		return fmt.Sprintf("/characters/%d/killmails/recent/", characterID)
	}
	return ""
}

// SnapshotFresh reports whether the snapshot's cached_until is still in
// the future. A missing/unparseable expiry counts as stale.
func SnapshotFresh(snap db.CharacterSnapshot) bool {
	if !snap.CachedUntil.Valid || snap.CachedUntil.String == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, snap.CachedUntil.String)
	return err == nil && time.Now().Before(until)
}

// FetchAndStoreSnapshot fetches the kind's ESI path with a valid token
// and stores the raw payload, honoring the response Expires header as
// cached_until (fallback: now + 5 minutes when ESI doesn't send one).
func (c *Client) FetchAndStoreSnapshot(ctx context.Context, ch db.Character, kind string) ([]byte, error) {
	path := snapshotPath(ch.CharacterID, kind)
	if path == "" {
		return nil, fmt.Errorf("unknown snapshot kind %q", kind)
	}

	token, err := c.tokens(ctx, ch)
	if err != nil {
		return nil, err
	}

	var body []byte
	var header http.Header
	if kind == SnapAssets {
		// Assets are paginated; the stored snapshot is the merged
		// array so downstream code sees one flat list.
		body, header, err = c.fetchAllPages(ctx, token, path)
	} else {
		body, header, err = c.FetchRaw(ctx, token, path)
	}
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
	if err := c.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
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

// corpSnapshotPath maps a corporation snapshot kind to its ESI
// path for a corporation, reporting whether the endpoint paginates
// (assets/orders/structures merge every page into one stored
// array, exactly like character assets) and whether the kind is
// known at all. The journal/transactions kinds carry their wallet
// division as a suffix (corp_journal_3). Everything else is a
// single fetch: the killmail list page shows the most recent
// entries first, and journal/transactions pages render "recent"
// views, so page 1 is the whole story for both.
func corpSnapshotPath(corporationID int64, kind string) (path string, paginated bool, ok bool) {
	switch kind {
	case SnapCorpInfo:
		return fmt.Sprintf("/corporations/%d/", corporationID), false, true
	case SnapCorpMembers:
		return fmt.Sprintf("/corporations/%d/members/", corporationID), false, true
	case SnapCorpMemberTracking:
		return fmt.Sprintf("/corporations/%d/membertracking/", corporationID), false, true
	case SnapCorpWallets:
		return fmt.Sprintf("/corporations/%d/wallets/", corporationID), false, true
	case SnapCorpOrders:
		return fmt.Sprintf("/corporations/%d/orders/", corporationID), true, true
	case SnapCorpAssets:
		return fmt.Sprintf("/corporations/%d/assets/", corporationID), true, true
	case SnapCorpStructures:
		return fmt.Sprintf("/corporations/%d/structures/", corporationID), true, true
	case SnapCorpKillmails:
		return fmt.Sprintf("/corporations/%d/killmails/recent/", corporationID), false, true
	}
	if division, found := parseDivisionKind(kind, SnapCorpJournalPrefix); found {
		return fmt.Sprintf("/corporations/%d/wallets/%d/journal/", corporationID, division), false, true
	}
	if division, found := parseDivisionKind(kind, SnapCorpTxnsPrefix); found {
		return fmt.Sprintf("/corporations/%d/wallets/%d/transactions/", corporationID, division), false, true
	}
	return "", false, false
}

// parseDivisionKind extracts the wallet division (1..7) from a
// per-division snapshot kind ("corp_journal_3"); anything outside
// 1..7 or unparseable is not a division kind.
func parseDivisionKind(kind, prefix string) (int64, bool) {
	if !strings.HasPrefix(kind, prefix) {
		return 0, false
	}
	d, err := strconv.ParseInt(strings.TrimPrefix(kind, prefix), 10, 64)
	if err != nil || d < 1 || d > 7 {
		return 0, false
	}
	return d, true
}

// FetchAndStoreCorpSnapshot is FetchAndStoreSnapshot for the
// corporation endpoints: the payload is fetched with ch's access
// token against ch's corporation and stored under ch's character
// ID + kind (corp data rides the per-character snapshot table —
// see the SnapCorp* kind comments). A 403 comes back as a
// StatusError (see IsForbidden): the caller records the missing
// in-game role and leaves the cache untouched.
func (c *Client) FetchAndStoreCorpSnapshot(ctx context.Context, ch db.Character, corporationID int64, kind string) ([]byte, error) {
	path, paginated, known := corpSnapshotPath(corporationID, kind)
	if !known {
		return nil, fmt.Errorf("unknown corp snapshot kind %q", kind)
	}

	token, err := c.tokens(ctx, ch)
	if err != nil {
		return nil, err
	}

	var body []byte
	var header http.Header
	if paginated {
		body, header, err = c.fetchAllPages(ctx, token, path)
	} else {
		body, header, err = c.FetchRaw(ctx, token, path)
	}
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
	if err := c.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
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

// FetchCorpAssetNames resolves player-given names for corporation
// asset items via POST /corporations/{id}/assets/names/ (ESI
// accepts at most 1,000 item IDs per call; the worker bounds its
// per-cycle batches). The endpoint needs the Director role,
// like the corporation assets list itself.
func (c *Client) FetchCorpAssetNames(ctx context.Context, ch db.Character, corporationID int64, itemIDs []int64) ([]AssetName, error) {
	token, err := c.tokens(ctx, ch)
	if err != nil {
		return nil, err
	}
	var out []AssetName
	path := fmt.Sprintf("/corporations/%d/assets/names/", corporationID)
	if err := c.postJSON(ctx, token, path, itemIDs, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchAllPages GETs every page of a paginated ESI endpoint (page
// count from the X-Pages header of the first response) and returns
// the entries merged into a single JSON array, plus the first
// response's headers (whose Expires drives snapshot bookkeeping).
func (c *Client) fetchAllPages(ctx context.Context, token, path string) ([]byte, http.Header, error) {
	body, header, err := c.FetchRaw(ctx, token, path)
	if err != nil {
		return nil, nil, err
	}

	pages := 1
	if xp := header.Get("X-Pages"); xp != "" {
		if n, aerr := strconv.Atoi(xp); aerr == nil && n > 1 {
			pages = n
		}
	}
	if pages == 1 {
		return body, header, nil
	}

	merged := []json.RawMessage{}
	appendPage := func(b []byte) error {
		var entries []json.RawMessage
		if err := json.Unmarshal(b, &entries); err != nil {
			return fmt.Errorf("ESI GET %s: decode page: %w", path, err)
		}
		merged = append(merged, entries...)
		return nil
	}
	if err := appendPage(body); err != nil {
		return nil, nil, err
	}
	for page := 2; page <= pages; page++ {
		b, _, err := c.FetchRaw(ctx, token, fmt.Sprintf("%s?page=%d", path, page))
		if err != nil {
			return nil, nil, err
		}
		if err := appendPage(b); err != nil {
			return nil, nil, err
		}
	}

	combined, err := json.Marshal(merged)
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: merge pages: %w", path, err)
	}
	return combined, header, nil
}

// GetCached returns ESI data for (character, kind), decoded into out.
// A snapshot whose cached_until is in the future is served as-is;
// otherwise a live fetch refreshes it. If the fetch fails but a
// snapshot exists, the stale snapshot is served (and logged). The
// fresh fetch goes through the token func, so expired access tokens
// are renewed transparently.
func (c *Client) GetCached(ctx context.Context, ch db.Character, kind string, out any) error {
	if snapshotPath(ch.CharacterID, kind) == "" {
		return fmt.Errorf("unknown snapshot kind %q", kind)
	}

	snap, serr := c.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	haveSnap := serr == nil
	if serr != nil && !errors.Is(serr, sql.ErrNoRows) {
		log.Printf("esi: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	if haveSnap && SnapshotFresh(snap) {
		if err := json.Unmarshal([]byte(snap.Payload), out); err == nil {
			return nil
		} else {
			log.Printf("esi: decode cached %s for character %d: %v (refetching)", kind, ch.CharacterID, err)
		}
	}

	body, err := c.FetchAndStoreSnapshot(ctx, ch, kind)
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
// Type-name resolution: skill/item IDs -> display names. The network
// tier (ResolveTypeNames, ResolveTypeGroups, ResolveGroupNames,
// TypeName, PlaceName) fetches and caches what the local caches
// lack, capped per call; the cache-only tier (CachedTypeNames,
// CachedTypeGroups, CachedGroupNames, CachedPlaceName) consults the
// in-process maps and the type_names table ONLY — never the network
// — so page renders can't block on ESI. Misses are simply absent
// (or reported unknown), and the worker's warm-up pass fills the
// gaps.
// ---------------------------------------------------------------------------

// maxTypeNameLookups bounds the number of ESI /universe/types lookups a
// single ResolveTypeNames call will make; anything beyond is left as
// "Type #<id>" until a later render.
const maxTypeNameLookups = 60

// TypeName returns the display name for one EVE type ID, consulting
// the in-process map, then the SDE tables, then the type_names
// table, then ESI (public endpoint, unauthenticated). Falls back
// to "Type #<id>".
func (c *Client) TypeName(ctx context.Context, id int64) string {
	names := c.ResolveTypeNames(ctx, []int64{id})
	if name, ok := names[id]; ok {
		return name
	}
	return fmt.Sprintf("Type #%d", id)
}

// ResolveTypeNames resolves a batch of type IDs. Cached names
// (memory, then the SDE tables, then the type_names table) are
// returned for everything requested; uncached IDs are fetched from
// ESI, at most maxTypeNameLookups per call, and persisted. Names
// that can't be resolved are simply absent from the result.
func (c *Client) ResolveTypeNames(ctx context.Context, ids []int64) map[int64]string {
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

	c.typeNamesMu.RLock()
	var missing []int64
	for _, id := range todo {
		if name, ok := c.typeNames[id]; ok {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	c.typeNamesMu.RUnlock()

	// The SDE tables are the primary source; the type_names
	// drip-feed table is only a fallback overlay now.
	var unstored []int64
	for _, id := range missing {
		if row, ok := c.sdeType(ctx, id); ok && row.Name != "" {
			out[id] = row.Name
		} else {
			unstored = append(unstored, id)
		}
	}

	var fetch []int64
	for _, id := range unstored {
		if name, err := c.queries.GetTypeName(ctx, id); err == nil && name != "" {
			out[id] = name
			c.typeNamesMu.Lock()
			c.typeNames[id] = name
			c.typeNamesMu.Unlock()
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
		var t Type
		if err := c.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t); err != nil {
			log.Printf("esi: type name lookup %d: %v", id, err)
			continue
		}
		if t.Name == "" {
			continue
		}
		out[id] = t.Name
		c.StoreTypeName(ctx, id, t)
	}
	return out
}

// StoreTypeName records a fetched type in the in-process caches and
// the type_names table. The group ID rides along on the same
// /universe/types payload, so name and group are stored together.
func (c *Client) StoreTypeName(ctx context.Context, id int64, t Type) {
	if t.Name != "" {
		c.typeNamesMu.Lock()
		c.typeNames[id] = t.Name
		c.typeNamesMu.Unlock()
		if err := c.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: id, Name: t.Name}); err != nil {
			log.Printf("esi: persist type name %d: %v", id, err)
		}
	}
	if t.GroupID > 0 {
		c.typeGroupsMu.Lock()
		c.typeGroups[id] = t.GroupID
		c.typeGroupsMu.Unlock()
	}
}

// cacheType records a name (and its group link, when known) in the
// in-process caches only. Unlike StoreTypeName it writes nothing to
// the type_names table — SDE-derived names already live in
// sde_types.
func (c *Client) cacheType(id int64, name string, groupID int64) {
	if name != "" {
		c.typeNamesMu.Lock()
		c.typeNames[id] = name
		c.typeNamesMu.Unlock()
	}
	if groupID > 0 {
		c.typeGroupsMu.Lock()
		c.typeGroups[id] = groupID
		c.typeGroupsMu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// SDE lookups: the local static-data tables are the primary name
// source now. Every helper treats a miss (or no import yet) as a
// plain "not found" — the type_names table and the network tier
// remain below as fallback.
// ---------------------------------------------------------------------------

// sdeType looks one type up in sde_types, filling the in-process
// caches (name + group link) on a hit.
func (c *Client) sdeType(ctx context.Context, id int64) (db.SdeType, bool) {
	row, err := c.queries.GetSDEType(ctx, id)
	if err != nil {
		return db.SdeType{}, false
	}
	c.cacheType(row.TypeID, row.Name, row.GroupID)
	return row, true
}

// sdeGroupName looks one group up in sde_groups, filling the
// in-process group-name cache on a hit.
func (c *Client) sdeGroupName(ctx context.Context, id int64) (string, bool) {
	row, err := c.queries.GetSDEGroup(ctx, id)
	if err != nil || row.Name == "" {
		return "", false
	}
	c.StoreGroupName(id, row.Name)
	return row.Name, true
}

// sdePlaceName looks a station or system ID up in the SDE tables
// (stations first — asset locations are usually stations; the ID
// spaces don't collide in practice), filling the place cache on a
// hit.
func (c *Client) sdePlaceName(ctx context.Context, id int64) (string, bool) {
	if row, err := c.queries.GetSDEStation(ctx, id); err == nil && row.Name != "" {
		c.StorePlaceName(id, row.Name)
		return row.Name, true
	}
	if row, err := c.queries.GetSDESystem(ctx, id); err == nil && row.Name != "" {
		c.StorePlaceName(id, row.Name)
		return row.Name, true
	}
	return "", false
}

// maxTypeGroupLookups bounds the number of ESI /universe/types
// lookups a single ResolveTypeGroups call will make.
const maxTypeGroupLookups = 60

// ResolveTypeGroups resolves type ID → group ID for a batch of
// types, from the in-process cache first (populated as a side
// effect of ResolveTypeNames fetches), then the SDE tables, then
// ESI, bounded per call. Types whose group can't be resolved are
// absent from the result.
func (c *Client) ResolveTypeGroups(ctx context.Context, ids []int64) map[int64]int64 {
	out := make(map[int64]int64, len(ids))

	seen := make(map[int64]bool, len(ids))
	var todo []int64
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		todo = append(todo, id)
	}

	c.typeGroupsMu.RLock()
	var missing []int64
	for _, id := range todo {
		if gid, ok := c.typeGroups[id]; ok {
			out[id] = gid
		} else {
			missing = append(missing, id)
		}
	}
	c.typeGroupsMu.RUnlock()

	var fetch []int64
	for _, id := range missing {
		if row, ok := c.sdeType(ctx, id); ok && row.GroupID > 0 {
			out[id] = row.GroupID
		} else {
			fetch = append(fetch, id)
		}
	}

	lookups := 0
	for _, id := range fetch {
		if lookups >= maxTypeGroupLookups {
			break
		}
		lookups++
		var t Type
		if err := c.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t); err != nil {
			log.Printf("esi: type group lookup %d: %v", id, err)
			continue
		}
		if t.GroupID <= 0 {
			continue
		}
		out[id] = t.GroupID
		c.typeGroupsMu.Lock()
		c.typeGroups[id] = t.GroupID
		c.typeGroupsMu.Unlock()
	}
	return out
}

// maxGroupNameLookups bounds the number of ESI /universe/groups
// lookups a single ResolveGroupNames call will make.
const maxGroupNameLookups = 40

// ResolveGroupNames resolves skill-group ID → display name from the
// in-process cache, then the SDE tables, then GET
// /universe/groups/{id}/ (public), capped per call. This is the
// network tier — page renders use CachedGroupNames instead;
// unresolved groups are absent from the result and the caller
// falls back to "Group #<id>".
func (c *Client) ResolveGroupNames(ctx context.Context, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))

	c.groupNamesMu.Lock()
	var missing []int64
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if name, ok := c.groupNames[id]; ok {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	c.groupNamesMu.Unlock()

	var fetch []int64
	for _, id := range missing {
		if name, ok := c.sdeGroupName(ctx, id); ok {
			out[id] = name
		} else {
			fetch = append(fetch, id)
		}
	}

	lookups := 0
	for _, id := range fetch {
		if lookups >= maxGroupNameLookups {
			break
		}
		lookups++
		var g Group
		if err := c.Get(ctx, "", fmt.Sprintf("/universe/groups/%d/", id), &g); err != nil {
			log.Printf("skills: group name lookup %d: %v", id, err)
			continue
		}
		if g.Name == "" {
			continue
		}
		out[id] = g.Name
		c.groupNamesMu.Lock()
		c.groupNames[id] = g.Name
		c.groupNamesMu.Unlock()
	}
	return out
}

// PlaceName resolves a station or solar-system ID to its name,
// consulting the in-process cache, then the SDE tables, then
// public ESI (successes cached in-process — the data is stable).
// This is the network tier: renders use CachedPlaceName instead;
// the market page's interactive order lookups still come here.
func (c *Client) PlaceName(ctx context.Context, path string, id int64, fallback string) string {
	c.placeMu.Lock()
	name, ok := c.placeNames[id]
	c.placeMu.Unlock()
	if ok {
		return name
	}

	// The SDE table matching the path's collection.
	if strings.Contains(path, "/stations/") {
		if row, err := c.queries.GetSDEStation(ctx, id); err == nil && row.Name != "" {
			c.StorePlaceName(id, row.Name)
			return row.Name
		}
	} else if strings.Contains(path, "/systems/") {
		if row, err := c.queries.GetSDESystem(ctx, id); err == nil && row.Name != "" {
			c.StorePlaceName(id, row.Name)
			return row.Name
		}
	}

	var place Station
	if err := c.Get(ctx, "", path, &place); err != nil {
		log.Printf("assets: place lookup %s: %v", path, err)
		return fallback
	}
	if place.Name == "" {
		return fallback
	}

	c.placeMu.Lock()
	c.placeNames[id] = place.Name
	c.placeMu.Unlock()
	return place.Name
}

// ---------------------------------------------------------------------------
// Cache-only resolution: the render-path tier. These consult the
// in-process maps, the SDE tables, and the type_names table ONLY —
// never the network.
// ---------------------------------------------------------------------------

// CachedTypeNames resolves type IDs from the in-process map, then
// the SDE tables, then the type_names table. Zero network.
// Unresolved IDs are absent.
func (c *Client) CachedTypeNames(ctx context.Context, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))

	seen := make(map[int64]bool, len(ids))
	var todo []int64
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		todo = append(todo, id)
	}

	c.typeNamesMu.RLock()
	var missing []int64
	for _, id := range todo {
		if name, ok := c.typeNames[id]; ok {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	c.typeNamesMu.RUnlock()

	var unstored []int64
	for _, id := range missing {
		if row, ok := c.sdeType(ctx, id); ok && row.Name != "" {
			out[id] = row.Name
		} else {
			unstored = append(unstored, id)
		}
	}

	for _, id := range unstored {
		if name, err := c.queries.GetTypeName(ctx, id); err == nil && name != "" {
			out[id] = name
			c.typeNamesMu.Lock()
			c.typeNames[id] = name
			c.typeNamesMu.Unlock()
		}
	}
	return out
}

// CachedTypeName is the single-ID form of CachedTypeNames; it
// returns "" when the ID isn't cached yet.
func (c *Client) CachedTypeName(ctx context.Context, id int64) string {
	if names := c.CachedTypeNames(ctx, []int64{id}); names != nil {
		return names[id]
	}
	return ""
}

// CachedTypeGroups resolves type ID → group ID from the in-process
// cache, then the SDE tables (populated by network fetches the
// worker performs, for anything the SDE lacks).
func (c *Client) CachedTypeGroups(ctx context.Context, ids []int64) map[int64]int64 {
	out := make(map[int64]int64, len(ids))
	c.typeGroupsMu.RLock()
	var missing []int64
	for _, id := range ids {
		if gid, ok := c.typeGroups[id]; ok {
			out[id] = gid
		} else {
			missing = append(missing, id)
		}
	}
	c.typeGroupsMu.RUnlock()
	for _, id := range missing {
		if row, ok := c.sdeType(ctx, id); ok && row.GroupID > 0 {
			out[id] = row.GroupID
		}
	}
	return out
}

// CachedGroupNames resolves group ID → name from the in-process
// cache, then the SDE tables.
func (c *Client) CachedGroupNames(ctx context.Context, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	c.groupNamesMu.Lock()
	var missing []int64
	for _, id := range ids {
		if name, ok := c.groupNames[id]; ok {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	c.groupNamesMu.Unlock()
	for _, id := range missing {
		if name, ok := c.sdeGroupName(ctx, id); ok {
			out[id] = name
		}
	}
	return out
}

// CachedPlaceName resolves a station/system ID from the in-process
// place-name cache, then the SDE tables.
func (c *Client) CachedPlaceName(ctx context.Context, id int64) (string, bool) {
	c.placeMu.Lock()
	name, ok := c.placeNames[id]
	c.placeMu.Unlock()
	if ok {
		return name, true
	}
	return c.sdePlaceName(ctx, id)
}

// StoreGroupName records a group name in the in-process cache (the
// worker's warm-up pass stores fetched group names this way).
func (c *Client) StoreGroupName(id int64, name string) {
	c.groupNamesMu.Lock()
	c.groupNames[id] = name
	c.groupNamesMu.Unlock()
}

// StorePlaceName records a station/system name in the in-process
// cache (the worker's warm-up pass stores fetched place names this
// way).
func (c *Client) StorePlaceName(id int64, name string) {
	c.placeMu.Lock()
	c.placeNames[id] = name
	c.placeMu.Unlock()
}

// ---------------------------------------------------------------------------
// Character-name resolution: killmail victims and final-blow
// attackers. Same two tiers as places: CharacterName is the network
// tier (public GET /characters/{id}/), CachedCharacterName is the
// render tier and never touches the network — the worker warms the
// cache from killmail details.
// ---------------------------------------------------------------------------

// CharacterName resolves a character ID to its name, consulting the
// in-process cache then public ESI (successes cached in-process).
// This is the network tier; renders use CachedCharacterName.
func (c *Client) CharacterName(ctx context.Context, id int64) (string, error) {
	if name, ok := c.CachedCharacterName(id); ok {
		return name, nil
	}
	var ch Character
	if err := c.Get(ctx, "", fmt.Sprintf("/characters/%d/", id), &ch); err != nil {
		return "", err
	}
	if ch.Name == "" {
		return "", fmt.Errorf("character %d: empty name", id)
	}
	c.StoreCharacterName(id, ch.Name)
	return ch.Name, nil
}

// CachedCharacterName resolves a character ID from the in-process
// cache only. Zero network.
func (c *Client) CachedCharacterName(id int64) (string, bool) {
	c.charNamesMu.RLock()
	defer c.charNamesMu.RUnlock()
	name, ok := c.charNames[id]
	return name, ok
}

// StoreCharacterName records a character name in the in-process
// cache (the worker's warm-up pass stores fetched names this way).
func (c *Client) StoreCharacterName(id int64, name string) {
	c.charNamesMu.Lock()
	c.charNames[id] = name
	c.charNamesMu.Unlock()
}

// ---------------------------------------------------------------------------
// Display helpers shared by the page handlers.
// ---------------------------------------------------------------------------

// SortedSkillIDs returns the skill IDs ordered by skillpoints
// (heaviest first, ties by ascending ID); used by the home page to
// pick the heaviest skills first.
func SortedSkillIDs(skills []Skill) []int64 {
	sorted := make([]Skill, len(skills))
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

// FormatISK renders a wallet balance with thousands separators and two
// decimals, e.g. 1234567.891 -> "1,234,567.89".
func FormatISK(balance float64) string {
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

// FormatInt renders an integer with thousands separators.
func FormatInt(n int64) string {
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

// RomanLevel renders a skill level the EVE way (I..V).
func RomanLevel(level int) string {
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
