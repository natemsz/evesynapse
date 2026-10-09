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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ESI is at https://esi.evetech.net; every request carries a
// descriptive User-Agent (CCP asks third parties to identify
// themselves).
const (
	baseURL = "https://esi.evetech.net"
	// defaultUserAgent identifies a client nobody told its build
	// (tests, mostly). The app sets the real one with SetUserAgent.
	defaultUserAgent = "EveSynapse (+https://github.com/natemsz/evesynapse)"
)

// SetUserAgent sets the User-Agent sent with every ESI request.
// CCP asks third-party applications to say what they are and how
// to reach whoever runs them, so that a misbehaving client gets a
// message rather than a block. Call it once, before the client is
// used.
func (c *Client) SetUserAgent(ua string) {
	if ua = strings.TrimSpace(ua); ua != "" {
		c.ua = ua
	}
}

func (c *Client) userAgent() string {
	if c.ua != "" {
		return c.ua
	}
	return defaultUserAgent
}

// Snapshot kinds stored in character_snapshots.
const (
	SnapSkills     = "skills"
	SnapSkillqueue = "skillqueue"
	SnapWallet     = "wallet"
	SnapAssets     = "assets"

	// SnapProfile is the public character record (name, birthday,
	// security status, corporation) warmed like every other snapshot so
	// no page ever fetches identity live at render. The endpoint is
	// public, so the payload needs no token; it is still stored per
	// character like the other kinds.
	SnapProfile = "profile"

	// Character: live-state endpoints.
	SnapLocation  = "location"
	SnapShip      = "ship"
	SnapOnline    = "online"
	SnapClones    = "clones"
	SnapImplants  = "implants"
	SnapFittings  = "fittings"
	SnapFatigue   = "fatigue"
	SnapKillmails = "killmails" // recent list (id+hash pairs); details live in killmail_details

	// Corporation: corporation endpoints,
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

	// Economy: character economy
	// endpoints. Journal/transactions are bounded recent windows
	// the worker merges (see fetchJournalWindow/fetchTxnsWindow);
	// contract *items* are not a snapshot at all — they warm into
	// the contract_details table (schema 006), like killmail
	// details.
	SnapWalletJournal = "wallet_journal"
	SnapWalletTxns    = "wallet_txns"
	SnapOrders        = "orders"
	SnapOrdersHistory = "orders_history"
	SnapContracts     = "contracts"
	SnapIndustryJobs  = "industry_jobs"
	SnapBlueprints    = "blueprints"
	SnapMining        = "mining"

	// Planetary industry: the colonies list, plus one
	// layout snapshot per planet keyed by planet id in the kind
	// (planet_layout_<planet id>) — the same suffix keying the
	// corporation wallet divisions use (corp_journal_3).
	SnapPlanets            = "planets"
	SnapPlanetLayoutPrefix = "planet_layout_"

	// Mail: the header list (ESI's 50 most recent), the
	// label set with per-label and total unread counts, the
	// character's mailing lists, and one body snapshot per mail
	// keyed by mail id (mail_body_<mail id>). Mail bodies are
	// immutable once delivered: the worker warms each once.
	SnapMail           = "mail"
	SnapMailLabels     = "mail_labels"
	SnapMailLists      = "mail_lists"
	SnapMailBodyPrefix = "mail_body_"

	// Calendar + contacts: event summaries (the next
	// 50 chronological from now), one detail snapshot and one
	// attendee-list snapshot per event (suffix-keyed like the
	// planet layouts), and the character's contact list.
	SnapCalendar            = "calendar"
	SnapCalendarEventPrefix = "calendar_event_"
	SnapCalendarAttPrefix   = "calendar_attendees_"
	SnapContacts            = "contacts"

	// The corporation roles the character holds (Director and the
	// rest). The ops calendar reads them to decide who may create
	// an op.
	SnapCorpRoles = "corp_roles"

	// Skill plans: the character's five attributes,
	// warmed alongside the skills snapshots — the plan engine
	// times every step against these, so the plans pages must
	// read them from the cache like everything else.
	SnapAttributes = "attributes"
)

// Global snapshot kinds stored in global_snapshots (schema 007):
// the Intel pages' data. Everything here is public
// ESI data — no character token — so it lives in a global store
// rather than the per-character snapshot table. War *details*
// are not a global snapshot; they warm into the war_details
// table like killmail details (see the worker).
const (
	GlobalStatus     = "status"     // GET /status/
	GlobalWars       = "wars"       // GET /wars/ (war ID list)
	GlobalIncursions = "incursions" // GET /incursions/
	GlobalFWSystems  = "fw_systems" // GET /fw/systems/
	GlobalFWStats    = "fw_stats"   // GET /fw/stats/
	GlobalFactions   = "factions"   // GET /universe/factions/
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

// PlanetLayoutKind is the snapshot kind holding one planet's
// colony layout (the planet id keying the layout endpoint).
func PlanetLayoutKind(planetID int64) string {
	return SnapPlanetLayoutPrefix + strconv.FormatInt(planetID, 10)
}

// PlanetLayoutPlanetID extracts the planet id from a planet-layout
// snapshot kind; ok=false for any other kind.
func PlanetLayoutPlanetID(kind string) (int64, bool) {
	return kindSuffixID(kind, SnapPlanetLayoutPrefix)
}

// MailBodyKind is the snapshot kind holding one mail's body.
func MailBodyKind(mailID int64) string {
	return SnapMailBodyPrefix + strconv.FormatInt(mailID, 10)
}

// CalendarEventKind is the snapshot kind holding one calendar
// event's detail.
func CalendarEventKind(eventID int64) string {
	return SnapCalendarEventPrefix + strconv.FormatInt(eventID, 10)
}

// CalendarAttendeesKind is the snapshot kind holding one calendar
// event's attendee list.
func CalendarAttendeesKind(eventID int64) string {
	return SnapCalendarAttPrefix + strconv.FormatInt(eventID, 10)
}

// kindSuffixID parses the trailing integer of a suffix-keyed
// snapshot kind ("planet_layout_40123456" → 40123456); anything
// without a positive trailing integer is not a keyed kind.
func kindSuffixID(kind, prefix string) (int64, bool) {
	if !strings.HasPrefix(kind, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(kind, prefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
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
	Detail string // the "error" text ESI sent with the status, when it sent one
}

func (e *StatusError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("ESI %s %s: status %d: %s", e.Method, e.Path, e.Code, e.Detail)
	}
	return fmt.Sprintf("ESI %s %s: status %d", e.Method, e.Path, e.Code)
}

// IsForbidden reports whether err is an ESI 403.
func IsForbidden(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

// StatusCode returns the HTTP status of a failed ESI call when the
// error carries one (0 otherwise). Lets callers distinguish a
// definitive answer about the request itself (404/422: this ID is
// not that kind of entity) from transient failures worth retrying.
func StatusCode(err error) (int, bool) {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code, true
	}
	return 0, false
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
	ua      string // User-Agent; see SetUserAgent

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

	// IDs ESI has definitively said are not characters (404/422
	// from GET /characters/{id}/): corporation and alliance IDs
	// share the numeric space and get harvested from ledger
	// payloads, and without this negative cache the worker would
	// re-ask about them every cycle. In-process only — a restart
	// re-proves a handful of IDs once.
	charNameMisses map[int64]bool

	// In-process caches of corporation and alliance ID → name,
	// warmed by the worker from war details so the Intel pages
	// can label war parties. Two maps on purpose: corporation
	// and alliance IDs share EVE's numeric space, and the app
	// never compares them cross-wise (see the corp cluster's
	// keying rule). Public data.
	corpNamesMu sync.RWMutex
	corpNames   map[int64]string

	allianceNamesMu sync.RWMutex
	allianceNames   map[int64]string

	// In-process cache of constellation ID → name for the
	// incursions page (constellations aren't in the SDE tables;
	// the IDs sit in their own range, clear of stations/systems).
	constellationNamesMu sync.RWMutex
	constellationNames   map[int64]string

	// In-process cache of PI schematic ID → schematic (name +
	// cycle time) for the planetary-industry pages. Schematics
	// are stable public data; the worker warms the cache from the
	// factory pins in colony layouts, and renders read it
	// cache-only. In-process only — the full schematic universe
	// is ~90 entries, so a restart re-warms in a cycle or two.
	schematicsMu sync.RWMutex
	schematics   map[int64]Schematic

	// In-process cache of player structure ID → name. Unlike the
	// public caches above, structure names live behind an
	// authenticated endpoint, so the durable copy sits in the
	// structure_names table (schema 014) and this map is only a
	// warm tier over it; the worker resolves, pages only read.
	structNamesMu sync.RWMutex
	structNames   map[int64]string

	// ESI error budget: X-Esi-Error-Limit-Remain and
	// X-Esi-Error-Limit-Reset from every response. Updated
	// atomically on each request; workers check ErrorBudgetLow()
	// before spending budget instead of discovering 420s.
	errBudgetMu     sync.RWMutex
	errBudgetRemain int
	errBudgetReset  int64 // unix seconds when the budget resets

	// rates is what is known of ESI's per-character rate limits
	// (ratelimit.go).
	rates rateLimits

	// changes is which characters have had new data stored
	// (changed.go).
	changes changeLog
}

// New builds a Client. httpClient performs every ESI request (the
// caller shares it with the SSO/JWKS calls); queries is the sqlc
// handle for the snapshot and type-name tables; tokens supplies
// access tokens for authenticated character endpoints.
func New(httpClient *http.Client, queries *db.Queries, tokens TokenFunc) *Client {
	return &Client{
		http:               httpClient,
		queries:            queries,
		tokens:             tokens,
		typeNames:          make(map[int64]string),
		typeGroups:         make(map[int64]int64),
		groupNames:         make(map[int64]string),
		placeNames:         make(map[int64]string),
		charNames:          make(map[int64]string),
		charNameMisses:     make(map[int64]bool),
		corpNames:          make(map[int64]string),
		allianceNames:      make(map[int64]string),
		constellationNames: make(map[int64]string),
		schematics:         make(map[int64]Schematic),
		structNames:        make(map[int64]string),
	}
}

// ---------------------------------------------------------------------------
// ESI payload types (the slices EveSynapse consumes).
// ---------------------------------------------------------------------------

// Character is the public character sheet: GET /characters/{id}/
// (returned fields EveSynapse consumes). AllianceID/FactionID are
// 0 when unset; Description is EVE-flavored HTML and is only ever
// rendered sanitized (see sanitizeMailHTML); Title is the
// character's in-corp title, "" for most pilots.
type Character struct {
	Name           string  `json:"name"`
	CorporationID  int64   `json:"corporation_id"`
	AllianceID     int64   `json:"alliance_id"`
	FactionID      int64   `json:"faction_id"`
	Birthday       string  `json:"birthday"` // RFC3339
	SecurityStatus float64 `json:"security_status"`
	Description    string  `json:"description"`
	Title          string  `json:"title"`
}

// CorpHistoryEntry is one entry of GET
// /characters/{id}/corporationhistory/ (public): a stint in one
// corporation starting at StartDate.
type CorpHistoryEntry struct {
	CorporationID int64  `json:"corporation_id"`
	StartDate     string `json:"start_date"` // RFC3339
	RecordID      int64  `json:"record_id"`
}

// CorpHistory is the character's employment history, newest
// record last as ESI returns it (callers sort for display).
type CorpHistory []CorpHistoryEntry

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
	Name                  string `json:"name"`
	Ticker                string `json:"ticker"`
	CreatorID             int64  `json:"creator_id"`
	CreatorCorporationID  int64  `json:"creator_corporation_id"`
	ExecutorCorporationID int64  `json:"executor_corporation_id"` // 0 for NPC-run alliances
	DateFounded           string `json:"date_founded"`            // RFC3339
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

// Attributes is GET /characters/{id}/attributes/: the five
// training attributes as final values (base + implants, after any
// remap) plus the remap bookkeeping the plan page reports.
// BonusRemaps and the dates are optional in ESI, hence pointers.
type Attributes struct {
	Charisma                 int    `json:"charisma"`
	Intelligence             int    `json:"intelligence"`
	Memory                   int    `json:"memory"`
	Perception               int    `json:"perception"`
	Willpower                int    `json:"willpower"`
	BonusRemaps              *int   `json:"bonus_remaps,omitempty"`
	LastRemapDate            string `json:"last_remap_date,omitempty"`
	AccruedRemapCooldownDate string `json:"accrued_remap_cooldown_date,omitempty"`
}

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
	Name        string `json:"name"`
	GroupID     int64  `json:"group_id"`
	Description string `json:"description"` // EVE-flavored text; rendered escaped, never raw
}

// Group is the slice of GET /universe/groups/{id}/ we consume.
type Group struct {
	Name string `json:"name"`
}

// UniverseIDEntry is one group entry of POST /universe/ids/;
// the response carries several groups (characters, corporations,
// inventory_types, ...), of which we consume inventory_types and
// characters.
type UniverseIDEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// UniverseIDs is the slice of POST /universe/ids/ we consume.
type UniverseIDs struct {
	Characters     []UniverseIDEntry `json:"characters"`
	InventoryTypes []UniverseIDEntry `json:"inventory_types"`
}

// MarketPrice is one row of GET /markets/prices/ (CCP's market
// guide: recent average and the adjusted price used for taxes).
type MarketPrice struct {
	TypeID        int64   `json:"type_id"`
	AveragePrice  float64 `json:"average_price"`
	AdjustedPrice float64 `json:"adjusted_price"`
}

// MarketHistoryDay is one row of GET /markets/{region_id}/history/
// — CCP's daily aggregate for a type in a region. Date is the ESI
// day ("2006-01-02").
type MarketHistoryDay struct {
	Date       string  `json:"date"`
	Average    float64 `json:"average"`
	Highest    float64 `json:"highest"`
	Lowest     float64 `json:"lowest"`
	Volume     int64   `json:"volume"`
	OrderCount int64   `json:"order_count"`
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
// Character live-state payloads.
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
// Corporation payloads. Shapes verified
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
	ID              int64   `json:"id"`
	Date            string  `json:"date"` // RFC3339
	RefType         string  `json:"ref_type"`
	Amount          float64 `json:"amount"` // + into the wallet, - out of it
	Balance         float64 `json:"balance"`
	Description     string  `json:"description"`
	Reason          string  `json:"reason"`
	FirstPartyID    int64   `json:"first_party_id"`
	FirstPartyType  string  `json:"first_party_type"` // "character" | "corporation" | "alliance" | …
	SecondPartyID   int64   `json:"second_party_id"`
	SecondPartyType string  `json:"second_party_type"`
	ContextID       int64   `json:"context_id"`
	ContextType     string  `json:"context_id_type"`
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

// CharacterFleet is GET /characters/{id}/fleet/: the fleet a
// character is in right now. ESI answers 404 when it is in none.
type CharacterFleet struct {
	FleetID     int64  `json:"fleet_id"`
	FleetBossID int64  `json:"fleet_boss_id"`
	Role        string `json:"role"`
}

// FleetMember is one entry of GET /fleets/{id}/members/, which only
// the fleet's boss may read.
type FleetMember struct {
	CharacterID   int64  `json:"character_id"`
	ShipTypeID    int64  `json:"ship_type_id"`
	SolarSystemID int64  `json:"solar_system_id"`
	Role          string `json:"role"`
	JoinTime      string `json:"join_time"`
}

// CharacterRoles is GET /characters/{id}/roles/: the corporation
// roles a character holds. Only the corporation-wide list is read.
type CharacterRoles struct {
	Roles []string `json:"roles"`
}

// AssetName is one entry of POST /corporations/{id}/assets/names/:
// the player-given name of a singleton item (a fitted ship, a
// renamed container). CCP reports "None" for items nobody named.
type AssetName struct {
	ItemID int64  `json:"item_id"`
	Name   string `json:"name"`
}

// ---------------------------------------------------------------------------
// Character economy payloads. Shapes
// verified against CCP's ESI OpenAPI document (components/schemas
// CharactersCharacterId*). Dates are RFC3339 strings.
// ---------------------------------------------------------------------------

// WalletJournalEntry is one entry of GET
// /characters/{id}/wallet/journal/ (30 days back, paged by
// ?page=). The stored snapshot is a bounded newest-first window
// merged across pages (see fetchJournalWindow), not one raw page.
type WalletJournalEntry struct {
	ID              int64   `json:"id"`
	Date            string  `json:"date"` // RFC3339
	RefType         string  `json:"ref_type"`
	Amount          float64 `json:"amount"` // + into the wallet, - out of it
	Balance         float64 `json:"balance"`
	Description     string  `json:"description"`
	Reason          string  `json:"reason"`
	FirstPartyID    int64   `json:"first_party_id"`
	FirstPartyType  string  `json:"first_party_type"` // "character" | "corporation" | "alliance" | …
	SecondPartyID   int64   `json:"second_party_id"`
	SecondPartyType string  `json:"second_party_type"`
	ContextID       int64   `json:"context_id"`
	ContextType     string  `json:"context_id_type"`
}

// WalletJournal is the stored journal window.
type WalletJournal []WalletJournalEntry

// WalletTransaction is one entry of GET
// /characters/{id}/wallet/transactions/ (steps backward via
// ?from_id=). The stored snapshot is a bounded newest-first window.
type WalletTransaction struct {
	TransactionID int64   `json:"transaction_id"`
	Date          string  `json:"date"` // RFC3339
	TypeID        int64   `json:"type_id"`
	LocationID    int64   `json:"location_id"`
	UnitPrice     float64 `json:"unit_price"`
	Quantity      int64   `json:"quantity"`
	ClientID      int64   `json:"client_id"` // counterparty character/corporation
	IsBuy         bool    `json:"is_buy"`
	IsPersonal    bool    `json:"is_personal"`
	JournalRefID  int64   `json:"journal_ref_id"`
}

// WalletTransactions is the stored transactions window.
type WalletTransactions []WalletTransaction

// CharOrder is one entry of GET /characters/{id}/orders/ (open).
type CharOrder struct {
	OrderID       int64   `json:"order_id"`
	TypeID        int64   `json:"type_id"`
	LocationID    int64   `json:"location_id"`
	RegionID      int64   `json:"region_id"`
	IsBuyOrder    bool    `json:"is_buy_order"`
	IsCorporation bool    `json:"is_corporation"`
	Price         float64 `json:"price"`
	VolumeTotal   int64   `json:"volume_total"`
	VolumeRemain  int64   `json:"volume_remain"`
	MinVolume     int64   `json:"min_volume"`
	Range         string  `json:"range"`  // "station"|"solarsystem"|"region"|"1".."40"
	Issued        string  `json:"issued"` // RFC3339
	Duration      int64   `json:"duration"`
	Escrow        float64 `json:"escrow"`
}

// CharOrders is GET /characters/{id}/orders/.
type CharOrders []CharOrder

// CharOrderHistoryEntry is one entry of GET
// /characters/{id}/orders/history/: a closed order. State is
// "cancelled"/"expired" per the spec (filled orders appear only
// while CCP retains them, with their final volume_remain).
type CharOrderHistoryEntry struct {
	CharOrder
	State string `json:"state"`
}

// CharOrderHistory is GET /characters/{id}/orders/history/.
type CharOrderHistory []CharOrderHistoryEntry

// Contract is one entry of GET /characters/{id}/contracts/ (all
// pages merged). AssigneeID is 0 for public contracts; AcceptorID
// is 0 until accepted.
type Contract struct {
	ContractID          int64   `json:"contract_id"`
	IssuerID            int64   `json:"issuer_id"`
	IssuerCorporationID int64   `json:"issuer_corporation_id"`
	AssigneeID          int64   `json:"assignee_id"`
	AcceptorID          int64   `json:"acceptor_id"`
	Type                string  `json:"type"` // item_exchange|auction|courier|loan|unknown
	Status              string  `json:"status"`
	Availability        string  `json:"availability"`
	Title               string  `json:"title"`
	ForCorporation      bool    `json:"for_corporation"`
	Price               float64 `json:"price"`
	Reward              float64 `json:"reward"`
	Buyout              float64 `json:"buyout"`
	Collateral          float64 `json:"collateral"`
	Volume              float64 `json:"volume"` // m3
	StartLocationID     int64   `json:"start_location_id"`
	EndLocationID       int64   `json:"end_location_id"`
	DateIssued          string  `json:"date_issued"`  // RFC3339
	DateExpired         string  `json:"date_expired"` // RFC3339
	DateAccepted        string  `json:"date_accepted"`
	DateCompleted       string  `json:"date_completed"`
	DaysToComplete      int64   `json:"days_to_complete"`
}

// Contracts is GET /characters/{id}/contracts/.
type Contracts []Contract

// ContractItem is one entry of GET
// /characters/{id}/contracts/{contract_id}/items/. A negative
// RawQuantity flags a singleton blueprint: -1 original, -2 copy.
type ContractItem struct {
	RecordID    int64 `json:"record_id"`
	TypeID      int64 `json:"type_id"`
	Quantity    int64 `json:"quantity"`
	RawQuantity int64 `json:"raw_quantity"`
	IsIncluded  bool  `json:"is_included"`
	IsSingleton bool  `json:"is_singleton"`
}

// ContractItems is GET /characters/{id}/contracts/{id}/items/.
type ContractItems []ContractItem

// IndustryJob is one entry of GET /characters/{id}/industry/jobs/
// (fetched with include_completed=true).
type IndustryJob struct {
	JobID                int64   `json:"job_id"`
	ActivityID           int64   `json:"activity_id"`
	BlueprintTypeID      int64   `json:"blueprint_type_id"`
	BlueprintID          int64   `json:"blueprint_id"`
	BlueprintLocationID  int64   `json:"blueprint_location_id"`
	OutputLocationID     int64   `json:"output_location_id"`
	FacilityID           int64   `json:"facility_id"`
	StationID            int64   `json:"station_id"` // facility's NPC station, when one
	InstallerID          int64   `json:"installer_id"`
	Runs                 int64   `json:"runs"`
	LicensedRuns         int64   `json:"licensed_runs"`
	SuccessfulRuns       int64   `json:"successful_runs"`
	Status               string  `json:"status"`     // active|paused|ready|delivered|cancelled|reverted
	StartDate            string  `json:"start_date"` // RFC3339
	EndDate              string  `json:"end_date"`   // RFC3339
	PauseDate            string  `json:"pause_date"`
	CompletedDate        string  `json:"completed_date"`
	Duration             int64   `json:"duration"` // seconds
	Cost                 float64 `json:"cost"`
	ProductTypeID        int64   `json:"product_type_id"`
	Probability          float64 `json:"probability"`
	CompletedCharacterID int64   `json:"completed_character_id"`
}

// IndustryJobs is GET /characters/{id}/industry/jobs/.
type IndustryJobs []IndustryJob

// Blueprint is one entry of GET /characters/{id}/blueprints/ (all
// pages merged). ESI semantics: Quantity -1 marks an original
// (BPO), -2 a copy (BPC); Runs -1 means unlimited (originals).
type Blueprint struct {
	ItemID             int64  `json:"item_id"`
	TypeID             int64  `json:"type_id"`
	LocationID         int64  `json:"location_id"`
	LocationFlag       string `json:"location_flag"`
	MaterialEfficiency int64  `json:"material_efficiency"`
	TimeEfficiency     int64  `json:"time_efficiency"`
	Quantity           int64  `json:"quantity"`
	Runs               int64  `json:"runs"`
}

// Blueprints is GET /characters/{id}/blueprints/.
type Blueprints []Blueprint

// MiningEntry is one entry of GET /characters/{id}/mining/ (all
// pages merged): one ore type, one system, one day.
type MiningEntry struct {
	Date          string `json:"date"` // YYYY-MM-DD
	TypeID        int64  `json:"type_id"`
	SolarSystemID int64  `json:"solar_system_id"`
	Quantity      int64  `json:"quantity"`
}

// MiningLedger is GET /characters/{id}/mining/.
type MiningLedger []MiningEntry

// ---------------------------------------------------------------------------
// Intel payloads (all public ESI).
// Shapes verified against CCP's ESI OpenAPI document
// (components/schemas WarsWarIdGet, IncursionsGet, FwSystemsGet,
// FwStatsGet, UniverseFactionsGet, Status,
// UniverseConstellationsConstellationIdGet). Dates are RFC3339
// strings.
// ---------------------------------------------------------------------------

// ServerStatus is GET /status/: Tranquility's vital signs.
type ServerStatus struct {
	Players       int64  `json:"players"`
	ServerVersion string `json:"server_version"`
	StartTime     string `json:"start_time"` // RFC3339
	VIP           bool   `json:"vip"`
}

// WarParty is one side (aggressor/defender) of a war: exactly one
// of CorporationID/AllianceID is set.
type WarParty struct {
	CorporationID int64   `json:"corporation_id"`
	AllianceID    int64   `json:"alliance_id"`
	ISKDestroyed  float64 `json:"isk_destroyed"`
	ShipsKilled   int64   `json:"ships_killed"`
}

// WarAlly is one entry of a war's allies list: exactly one of
// CorporationID/AllianceID is set.
type WarAlly struct {
	CorporationID int64 `json:"corporation_id"`
	AllianceID    int64 `json:"alliance_id"`
}

// War is GET /wars/{war_id}/. Started/Retracted/Finished are
// empty until the war reaches that point; a non-empty Finished
// freezes the payload (the worker never refetches those).
type War struct {
	ID            int64     `json:"id"`
	Aggressor     WarParty  `json:"aggressor"`
	Defender      WarParty  `json:"defender"`
	Allies        []WarAlly `json:"allies"`
	Declared      string    `json:"declared"`  // RFC3339
	Started       string    `json:"started"`   // RFC3339
	Retracted     string    `json:"retracted"` // RFC3339
	Finished      string    `json:"finished"`  // RFC3339
	Mutual        bool      `json:"mutual"`
	OpenForAllies bool      `json:"open_for_allies"`
}

// WarList is GET /wars/: war IDs, most recent first (the worker
// warms details for a bounded head of the list).
type WarList []int64

// Incursion is one entry of GET /incursions/.
type Incursion struct {
	ConstellationID int64   `json:"constellation_id"`
	FactionID       int64   `json:"faction_id"`
	HasBoss         bool    `json:"has_boss"`
	InfestedSystems []int64 `json:"infested_solar_systems"`
	Influence       float64 `json:"influence"` // 0..1
	StagingSystemID int64   `json:"staging_solar_system_id"`
	State           string  `json:"state"` // established|mobilizing|withdrawing
	Type            string  `json:"type"`
}

// Incursions is GET /incursions/.
type Incursions []Incursion

// FWSystem is one entry of GET /fw/systems/. Contested is CCP's
// state label (captured|contested|uncontested|vulnerable); the
// contested percentage pages show is VictoryPoints over
// VictoryPointsThreshold, the ratio the in-game display tracks.
type FWSystem struct {
	SolarSystemID          int64  `json:"solar_system_id"`
	OccupierFactionID      int64  `json:"occupier_faction_id"`
	OwnerFactionID         int64  `json:"owner_faction_id"`
	Contested              string `json:"contested"`
	VictoryPoints          int64  `json:"victory_points"`
	VictoryPointsThreshold int64  `json:"victory_points_threshold"`
}

// FWSystems is GET /fw/systems/.
type FWSystems []FWSystem

// FWPeriod is a yesterday/last-week/total counter block (kills
// and victory points share the shape in /fw/stats/).
type FWPeriod struct {
	Yesterday int64 `json:"yesterday"`
	LastWeek  int64 `json:"last_week"`
	Total     int64 `json:"total"`
}

// FWStat is one faction's entry of GET /fw/stats/.
type FWStat struct {
	FactionID         int64    `json:"faction_id"`
	Pilots            int64    `json:"pilots"`
	SystemsControlled int64    `json:"systems_controlled"`
	Kills             FWPeriod `json:"kills"`
	VictoryPoints     FWPeriod `json:"victory_points"`
}

// FWStats is GET /fw/stats/.
type FWStats []FWStat

// Faction is one entry of GET /universe/factions/ (the slice
// EveSynapse consumes; the list is nearly static).
type Faction struct {
	FactionID     int64  `json:"faction_id"`
	Name          string `json:"name"`
	CorporationID int64  `json:"corporation_id"` // the faction's NPC corporation, 0 when none
}

// Factions is GET /universe/factions/.
type Factions []Faction

// Constellation is GET /universe/constellations/{id}/ (the slice
// EveSynapse consumes).
type Constellation struct {
	ConstellationID int64  `json:"constellation_id"`
	Name            string `json:"name"`
	RegionID        int64  `json:"region_id"`
}

// ---------------------------------------------------------------------------
// Planetary industry payloads (auth scope
// esi-planets.manage_planets.v1 — the only scope CCP publishes for
// the two colony GETs). Shapes verified against CCP's ESI OpenAPI
// document (components/schemas CharactersCharacterIdPlanets*,
// UniverseSchematicsSchematicIdGet). Dates are RFC3339 strings.
// ---------------------------------------------------------------------------

// Colony is one entry of GET /characters/{id}/planets/.
type Colony struct {
	PlanetID      int64  `json:"planet_id"`
	OwnerID       int64  `json:"owner_id"` // the owning character
	SolarSystemID int64  `json:"solar_system_id"`
	PlanetType    string `json:"planet_type"` // temperate|barren|oceanic|ice|gas|lava|storm|plasma
	NumPins       int64  `json:"num_pins"`
	UpgradeLevel  int64  `json:"upgrade_level"` // command-center upgrade level 0..5
	LastUpdate    string `json:"last_update"`   // RFC3339
}

// Colonies is GET /characters/{id}/planets/.
type Colonies []Colony

// PlanetExtractorHead is one extraction head of an extractor pin.
type PlanetExtractorHead struct {
	HeadID    int64   `json:"head_id"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// PlanetExtractor is the extractor_details object of an extractor
// pin. CycleTime, QtyPerCycle and the head layout are fixed at pin
// install time, so they stay true for the pin's life.
type PlanetExtractor struct {
	ProductTypeID int64                 `json:"product_type_id"`
	CycleTime     int64                 `json:"cycle_time"` // seconds
	QtyPerCycle   int64                 `json:"qty_per_cycle"`
	Heads         []PlanetExtractorHead `json:"heads"`
}

// PlanetFactory is the factory_details object of a factory pin.
type PlanetFactory struct {
	SchematicID int64 `json:"schematic_id"`
}

// PlanetPinContent is one stored commodity of a pin. ESI only
// recalculates amounts when the colony is viewed through the game
// client, so readers must not present them as live truth — the PI
// pages omit them entirely (see planets.go).
type PlanetPinContent struct {
	TypeID int64 `json:"type_id"`
	Amount int64 `json:"amount"`
}

// PlanetPin is one pin of GET /characters/{id}/planets/{planet_id}/.
// ExpiryTime rides the pin itself (extractor pins carry it); it is
// fixed when the extractor program is installed, so expiry
// countdowns computed from it are reliable. LastCycleStart shares
// the contents staleness caveat above.
type PlanetPin struct {
	PinID            int64              `json:"pin_id"`
	TypeID           int64              `json:"type_id"`
	SchematicID      int64              `json:"schematic_id"` // factory pins; mirrors FactoryDetails
	ExpiryTime       string             `json:"expiry_time"`  // RFC3339, extractor pins
	InstallTime      string             `json:"install_time"` // RFC3339
	LastCycleStart   string             `json:"last_cycle_start"`
	ExtractorDetails *PlanetExtractor   `json:"extractor_details"`
	FactoryDetails   *PlanetFactory     `json:"factory_details"`
	Contents         []PlanetPinContent `json:"contents"`
}

// PlanetLink is one link of a colony layout.
type PlanetLink struct {
	SourcePinID      int64 `json:"source_pin_id"`
	DestinationPinID int64 `json:"destination_pin_id"`
	LinkLevel        int64 `json:"link_level"`
}

// PlanetRoute is one commodity route of a colony layout.
type PlanetRoute struct {
	RouteID          int64   `json:"route_id"`
	SourcePinID      int64   `json:"source_pin_id"`
	DestinationPinID int64   `json:"destination_pin_id"`
	ContentTypeID    int64   `json:"content_type_id"`
	Quantity         float64 `json:"quantity"`
	Waypoints        []int64 `json:"waypoints"`
}

// PlanetLayout is GET /characters/{id}/planets/{planet_id}/.
type PlanetLayout struct {
	Pins   []PlanetPin   `json:"pins"`
	Links  []PlanetLink  `json:"links"`
	Routes []PlanetRoute `json:"routes"`
}

// Schematic is GET /universe/schematics/{schematic_id}/ (public).
// This is the complete ESI surface for a schematic: its name and
// cycle time. ESI exposes no input/output bill of materials (that
// lives only in the SDE), so factory pins display the schematic
// name and never a guessed input list.
type Schematic struct {
	SchematicName string `json:"schematic_name"`
	CycleTime     int64  `json:"cycle_time"` // seconds per run
}

// ---------------------------------------------------------------------------
// Mail payloads (auth scope esi-mail.read_mail.v1, the
// only mail scope this app requests — no organize/send).
// Shapes verified against CCP's ESI OpenAPI document
// (components/schemas CharactersCharacterIdMail*).
// ---------------------------------------------------------------------------

// MailRecipient is one addressee of a mail header/body.
type MailRecipient struct {
	RecipientID   int64  `json:"recipient_id"`
	RecipientType string `json:"recipient_type"` // alliance|character|corporation|mailing_list
}

// MailHeader is one entry of GET /characters/{id}/mail/ (the 50
// most recent matching headers).
type MailHeader struct {
	MailID     int64           `json:"mail_id"`
	From       int64           `json:"from"` // sender character id
	Subject    string          `json:"subject"`
	Timestamp  string          `json:"timestamp"` // RFC3339
	IsRead     bool            `json:"is_read"`
	Labels     []int64         `json:"labels"` // label ids
	Recipients []MailRecipient `json:"recipients"`
}

// MailHeaders is GET /characters/{id}/mail/.
type MailHeaders []MailHeader

// Mail is GET /characters/{id}/mail/{mail_id}/: one mail with
// body. Body is EVE-flavored HTML; it is never rendered unsanitized
// (see sanitizeMailHTML in mail.go).
type Mail struct {
	From       int64           `json:"from"`
	Subject    string          `json:"subject"`
	Body       string          `json:"body"`
	Timestamp  string          `json:"timestamp"` // RFC3339
	Read       bool            `json:"read"`
	Labels     []int64         `json:"labels"`
	Recipients []MailRecipient `json:"recipients"`
}

// MailLabel is one entry of the label set: a user label with its
// unread count.
type MailLabel struct {
	LabelID     int64  `json:"label_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	UnreadCount int64  `json:"unread_count"`
}

// MailLabels is GET /characters/{id}/mail/labels/.
type MailLabels struct {
	Labels           []MailLabel `json:"labels"`
	TotalUnreadCount int64       `json:"total_unread_count"`
}

// MailList is one entry of GET /characters/{id}/mail/lists/: a
// mailing list the character belongs to.
type MailList struct {
	MailingListID int64  `json:"mailing_list_id"`
	Name          string `json:"name"`
}

// MailLists is GET /characters/{id}/mail/lists/.
type MailLists []MailList

// ---------------------------------------------------------------------------
// Calendar + contacts payloads (auth scopes
// esi-calendar.read_calendar_events.v1 and
// esi-characters.read_contacts.v1, both long held). Shapes
// verified against CCP's ESI OpenAPI document
// (components/schemas CharactersCharacterIdCalendar* /
// CharactersCharacterIdContactsGet). Dates are RFC3339 strings.
// ---------------------------------------------------------------------------

// CalendarEventSummary is one entry of GET
// /characters/{id}/calendar/ (the next 50 chronological event
// summaries from now).
type CalendarEventSummary struct {
	EventID       int64  `json:"event_id"`
	EventDate     string `json:"event_date"` // RFC3339
	Title         string `json:"title"`
	Importance    int64  `json:"importance"`
	EventResponse string `json:"event_response"` // accepted|declined|tentative|not_responded
}

// CalendarEventSummaries is GET /characters/{id}/calendar/.
type CalendarEventSummaries []CalendarEventSummary

// CalendarEvent is GET /characters/{id}/calendar/{event_id}/.
type CalendarEvent struct {
	EventID    int64  `json:"event_id"`
	Date       string `json:"date"`     // RFC3339
	Duration   int64  `json:"duration"` // minutes
	Importance int64  `json:"importance"`
	OwnerID    int64  `json:"owner_id"`
	OwnerName  string `json:"owner_name"`
	OwnerType  string `json:"owner_type"` // eve_server|corporation|faction|character|alliance
	Response   string `json:"response"`
	Title      string `json:"title"`
	Text       string `json:"text"`
}

// CalendarAttendee is one entry of GET
// /characters/{id}/calendar/{event_id}/attendees/.
type CalendarAttendee struct {
	CharacterID   int64  `json:"character_id"`
	EventResponse string `json:"event_response"` // accepted|declined|tentative|not_responded
}

// CalendarAttendees is GET /characters/{id}/calendar/{event_id}/attendees/.
type CalendarAttendees []CalendarAttendee

// Contact is one entry of GET /characters/{id}/contacts/ (paged;
// the stored snapshot merges every page).
type Contact struct {
	ContactID   int64   `json:"contact_id"`
	ContactType string  `json:"contact_type"` // character|corporation|alliance|faction
	Standing    float64 `json:"standing"`     // -10..10
	IsBlocked   bool    `json:"is_blocked"`
	IsWatched   bool    `json:"is_watched"`
	LabelIDs    []int64 `json:"label_ids"`
}

// Contacts is GET /characters/{id}/contacts/.
type Contacts []Contact

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

// request is the one place an ESI request is made. Every helper
// below goes through it, so they all identify themselves to CCP
// the same way, all feed the error budget, and all read ESI's
// answers the same way: 420/429 wrap ErrErrorLimit, a status
// outside want is a StatusError, anything else is the body.
//
// payload nil sends no body; accessToken "" sends no Authorization
// header. Token values are never logged; errors carry the method,
// path and status only.
func (c *Client) request(ctx context.Context, method, accessToken, path string, payload any, want ...int) ([]byte, http.Header, error) {
	body, header, _, err := c.send(ctx, method, accessToken, path, payload, "", want...)
	return body, header, err
}

// send is request with two additions for conditional fetches: it
// sends ifNoneMatch (when not empty) as If-None-Match, and it
// reports which of the wanted statuses came back.
//
// Idempotent reads (GET, HEAD) are retried twice on transport failures
// and 5xx answers, so a transient ESI failure does not cost the dataset
// a whole worker cycle. The wait honors ESI's Retry-After header
// (capped), else backs off 500ms, 1s. Writes are never retried (a
// repeated POST could send a mail twice), and neither are 420/429:
// those wrap ErrErrorLimit, and the worker backs off until the next
// cycle on those by contract.
func (c *Client) send(ctx context.Context, method, accessToken, path string, payload any, ifNoneMatch string, want ...int) (body []byte, header http.Header, status int, err error) {
	var raw []byte
	if payload != nil {
		raw, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("ESI %s %s: encode: %w", method, path, err)
		}
	}
	retryable := method == http.MethodGet || method == http.MethodHead
	backoffs := sendRetryBackoffs
	var attempt int
	for {
		body, header, status, err = c.sendOnce(ctx, method, accessToken, path, raw, ifNoneMatch, want)
		if err == nil || !retryable || attempt >= len(backoffs) {
			return body, header, status, err
		}
		wait, ok := retryWait(err, header, backoffs[attempt])
		if !ok {
			return body, header, status, err
		}
		attempt++
		select {
		case <-ctx.Done():
			return nil, nil, 0, fmt.Errorf("ESI %s %s: %w", method, path, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// retryWait reports whether a failed read is worth another attempt
// and how long to wait first: transport failures and 500/502/503/504
// answers are, anything else (4xx, the error limit, a canceled
// context) is not. ESI's Retry-After answer, when present, sets the
// wait (capped); without one the caller's backoff stands.
func retryWait(err error, header http.Header, backoff time.Duration) (time.Duration, bool) {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		default:
			return 0, false
		}
	} else if !isTransportError(err) {
		return 0, false
	}
	if wait, ok := parseRetryAfter(header.Get("Retry-After")); ok {
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
		return wait, true
	}
	return backoff, true
}

// maxRetryAfter caps the wait ESI can ask for before a retry: the
// worker cycle has its own deadlines, and a minute-plus sleep on
// one dataset would stall everything behind it.
const maxRetryAfter = 10 * time.Second

// sendRetryBackoffs is the wait before each read retry when ESI
// sends no Retry-After. A var so tests can run retries without
// sleeping (the suite is sequential, so overriding with a cleanup
// restore is safe).
var sendRetryBackoffs = []time.Duration{500 * time.Millisecond, time.Second}

// isTransportError reports whether err came from the HTTP transport
// itself (dial, TLS, reset) rather than from ESI answering: only
// then is the attempt known not to have happened.
func isTransportError(err error) bool {
	var se *StatusError
	if errors.As(err, &se) || errors.Is(err, ErrErrorLimit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// parseRetryAfter reads a Retry-After header value: seconds, or an
// HTTP date. It reports false when the value is missing or junk.
func parseRetryAfter(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		wait := time.Until(when)
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}
	return 0, false
}

// sendOnce makes one ESI request and reads its answer the one way:
// 420/429 wrap ErrErrorLimit, a status outside want is a StatusError,
// anything else is the body.
func (c *Client) sendOnce(ctx context.Context, method, accessToken, path string, raw []byte, ifNoneMatch string, want []int) (body []byte, header http.Header, status int, err error) {
	var reqBody io.Reader
	if raw != nil {
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("ESI %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.trackErrorBudget(resp.Header)
	c.trackRateHeaders(characterFrom(ctx), resp.Header, time.Now())
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("ESI %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// One character's budget, not everybody's (ratelimit.go).
		c.noteRateLimited(characterFrom(ctx), resp.Header, time.Now())
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("ESI %s %s: status %d: %w (%w)", method, path, resp.StatusCode, ErrErrorLimit, ErrRateLimited)
	}
	if resp.StatusCode == 420 {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("ESI %s %s: status %d: %w", method, path, resp.StatusCode, ErrErrorLimit)
	}
	for _, code := range want {
		if resp.StatusCode == code {
			return body, resp.Header, resp.StatusCode, nil
		}
	}
	return nil, resp.Header, resp.StatusCode, &StatusError{Method: method, Path: path, Code: resp.StatusCode, Detail: esiErrorDetail(body)}
}

// esiErrorDetail pulls the human-readable "error" text out of an ESI
// error body ({"error": "..."}), shortened, or "" when there is none.
func esiErrorDetail(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	detail := strings.TrimSpace(e.Error)
	if r := []rune(detail); len(r) > 200 {
		detail = string(r[:200]) + "…"
	}
	return detail
}

// fetchIfChanged GETs path, offering ESI the ETag of the copy
// already stored (etag; "" when there is none, which makes this an
// ordinary fetch). When that copy is still current ESI answers 304
// Not Modified with no body and notModified is true: the caller
// keeps what it has and only renews its cache window from the
// headers. A 304 is not an error and does not count against the
// error limit.
func (c *Client) fetchIfChanged(ctx context.Context, accessToken, path, etag string) (body []byte, header http.Header, notModified bool, err error) {
	want := []int{http.StatusOK}
	if etag != "" {
		want = append(want, http.StatusNotModified)
	}
	body, header, status, err := c.send(ctx, http.MethodGet, accessToken, path, nil, etag, want...)
	if err != nil {
		return nil, header, false, err
	}
	if status == http.StatusNotModified {
		return nil, header, true, nil
	}
	return body, header, false, nil
}

// cacheWindowEnd is when a response stops being current: its
// Expires header, or five minutes from now when ESI sent none.
func cacheWindowEnd(header http.Header) time.Time {
	if exp := header.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			return t
		}
	}
	return time.Now().Add(5 * time.Minute)
}

// FetchRaw GETs path from ESI and returns the raw body and response
// headers (Expires drives snapshot bookkeeping). Non-200 statuses are
// errors; 420/429 wrap ErrErrorLimit.
func (c *Client) FetchRaw(ctx context.Context, accessToken, path string) ([]byte, http.Header, error) {
	return c.request(ctx, http.MethodGet, accessToken, path, nil, http.StatusOK)
}

// trackErrorBudget records ESI's X-Esi-Error-Limit-Remain/Reset
// headers. Called on every response, in request;
// workers consult ErrorBudgetLow before spending budget.
func (c *Client) trackErrorBudget(h http.Header) {
	remainStr := h.Get("X-Esi-Error-Limit-Remain")
	resetStr := h.Get("X-Esi-Error-Limit-Reset")
	if remainStr == "" && resetStr == "" {
		return
	}
	c.errBudgetMu.Lock()
	defer c.errBudgetMu.Unlock()
	if remainStr != "" {
		if n, err := strconv.Atoi(remainStr); err == nil {
			c.errBudgetRemain = n
		}
	}
	if resetStr != "" {
		if n, err := strconv.Atoi(resetStr); err == nil {
			c.errBudgetReset = time.Now().Unix() + int64(n)
		}
	}
}

// ErrorBudgetLow reports whether ESI's error budget is exhausted or
// nearly so. Workers should check this before spending fetch budget
// and back off until the reset time instead of discovering 420s.
func (c *Client) ErrorBudgetLow() bool {
	c.errBudgetMu.RLock()
	defer c.errBudgetMu.RUnlock()
	// Never observed a budget header yet: not low.
	if c.errBudgetReset == 0 {
		return false
	}
	// Budget already reset: not low.
	if time.Now().Unix() >= c.errBudgetReset {
		return false
	}
	return c.errBudgetRemain <= 5
}

// ErrorBudgetStatus returns the last observed remain count and reset
// time for status displays. Zero values mean no header seen yet.
func (c *Client) ErrorBudgetStatus() (remain int, resetUnix int64) {
	c.errBudgetMu.RLock()
	defer c.errBudgetMu.RUnlock()
	return c.errBudgetRemain, c.errBudgetReset
}

// PostJSON POSTs payload as JSON to a public ESI endpoint and
// decodes the 200 response into out. Used by POST /universe/ids/
// for exact name → ID resolution and POST /characters/affiliation/;
// no token — the endpoints are public.
func (c *Client) PostJSON(ctx context.Context, path string, payload any, out any) error {
	body, _, err := c.request(ctx, http.MethodPost, "", path, payload, http.StatusOK)
	if err != nil {
		return err
	}
	return decodeESI(http.MethodPost, path, body, out)
}

// PostJSONAuthed is the authenticated-write path: POST with a
// bearer token to an endpoint that answers 201 Created (saving a
// fitting, sending a mail). A 403 surfaces as StatusError so
// callers can tell a missing scope from a bad payload.
//
// The response is decoded into out; pass nil when the caller has
// no use for it. What a 201 carries differs by endpoint — an object
// for a new fitting, a bare number for a new mail — and a caller
// that does not need it should not fail on its shape.
func (c *Client) PostJSONAuthed(ctx context.Context, accessToken, path string, payload any, out any) error {
	body, _, err := c.request(ctx, http.MethodPost, accessToken, path, payload, http.StatusCreated)
	if err != nil {
		return err
	}
	return decodeESI(http.MethodPost, path, body, out)
}

// PutJSONAuthed is the PUT counterpart of PostJSONAuthed (Issue 26:
// PUT /characters/{id}/mail/{mail_id}/ to mark mail read). Any 2xx
// is success; a 403 surfaces as StatusError so callers can tell a
// missing scope from a bad payload.
func (c *Client) PutJSONAuthed(ctx context.Context, accessToken, path string, payload any) error {
	_, _, err := c.request(ctx, http.MethodPut, accessToken, path, payload,
		http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent)
	return err
}

// decodeESI unmarshals a response body into out (nil: the caller
// does not want it).
func decodeESI(method, path string, body []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ESI %s %s: decode: %w", method, path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Per-character snapshot cache.
// ---------------------------------------------------------------------------

// snapshotPath maps a snapshot kind to its ESI path for a character.
func snapshotPath(characterID int64, kind string) string {
	switch kind {
	case SnapProfile:
		return fmt.Sprintf("/characters/%d/", characterID)
	case SnapSkills:
		return fmt.Sprintf("/characters/%d/skills/", characterID)
	case SnapSkillqueue:
		return fmt.Sprintf("/characters/%d/skillqueue/", characterID)
	case SnapAttributes:
		return fmt.Sprintf("/characters/%d/attributes/", characterID)
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
	case SnapCorpRoles:
		return fmt.Sprintf("/characters/%d/roles/", characterID)
	case SnapFatigue:
		return fmt.Sprintf("/characters/%d/fatigue/", characterID)
	case SnapKillmails:
		return fmt.Sprintf("/characters/%d/killmails/recent/", characterID)
	case SnapWalletJournal:
		return fmt.Sprintf("/characters/%d/wallet/journal/", characterID)
	case SnapWalletTxns:
		return fmt.Sprintf("/characters/%d/wallet/transactions/", characterID)
	case SnapOrders:
		return fmt.Sprintf("/characters/%d/orders/", characterID)
	case SnapOrdersHistory:
		return fmt.Sprintf("/characters/%d/orders/history/", characterID)
	case SnapContracts:
		return fmt.Sprintf("/characters/%d/contracts/", characterID)
	case SnapIndustryJobs:
		return fmt.Sprintf("/characters/%d/industry/jobs/?include_completed=true", characterID)
	case SnapBlueprints:
		return fmt.Sprintf("/characters/%d/blueprints/", characterID)
	case SnapMining:
		return fmt.Sprintf("/characters/%d/mining/", characterID)
	case SnapPlanets:
		return fmt.Sprintf("/characters/%d/planets/", characterID)
	case SnapMail:
		return fmt.Sprintf("/characters/%d/mail/", characterID)
	case SnapMailLabels:
		return fmt.Sprintf("/characters/%d/mail/labels/", characterID)
	case SnapMailLists:
		return fmt.Sprintf("/characters/%d/mail/lists/", characterID)
	case SnapCalendar:
		return fmt.Sprintf("/characters/%d/calendar/", characterID)
	case SnapContacts:
		return fmt.Sprintf("/characters/%d/contacts/", characterID)
	}
	// Suffix-keyed kinds: the entity id travels in the kind
	// (planet_layout_<planet id>, mail_body_<mail id>,
	// calendar_event_<event id>, calendar_attendees_<event id>).
	if planetID, ok := kindSuffixID(kind, SnapPlanetLayoutPrefix); ok {
		return fmt.Sprintf("/characters/%d/planets/%d/", characterID, planetID)
	}
	if mailID, ok := kindSuffixID(kind, SnapMailBodyPrefix); ok {
		return fmt.Sprintf("/characters/%d/mail/%d/", characterID, mailID)
	}
	if eventID, ok := kindSuffixID(kind, SnapCalendarEventPrefix); ok {
		return fmt.Sprintf("/characters/%d/calendar/%d/", characterID, eventID)
	}
	if eventID, ok := kindSuffixID(kind, SnapCalendarAttPrefix); ok {
		return fmt.Sprintf("/characters/%d/calendar/%d/attendees/", characterID, eventID)
	}
	return ""
}

// SnapshotFresh reports whether the snapshot's cached_until is still in
// the future. A missing expiry counts as stale.
func SnapshotFresh(snap db.CharacterSnapshot) bool {
	return CacheWindowOpen(snap.CachedUntil)
}

// CacheWindowOpen reports whether a stored cached_until is still in
// the future. It is SnapshotFresh for readers that hold only a
// snapshot's bookkeeping, not the snapshot.
func CacheWindowOpen(cachedUntil sql.NullTime) bool {
	return cachedUntil.Valid && time.Now().Before(cachedUntil.Time)
}

// FetchAndStoreSnapshot fetches the kind's ESI path with a valid token
// and stores the raw payload, honoring the response Expires header as
// cached_until (fallback: now + 5 minutes when ESI doesn't send one).
//
// A dataset that comes in one response is fetched conditionally:
// ESI is offered the ETag the stored copy came with, and when
// nothing has changed it answers 304 with no body. The payload then
// stays as it is and only its cache window is renewed. Datasets
// that span several pages are downloaded in full, as before.
func (c *Client) FetchAndStoreSnapshot(ctx context.Context, ch db.Character, kind string) error {
	_, _, err := c.refreshSnapshot(ctx, ch, kind, true)
	return err
}

// refreshSnapshot is FetchAndStoreSnapshot for callers that want
// the outcome: the fresh payload, or notModified when ESI said the
// stored one is still current (the payload is then not returned;
// the caller already has it or does not need it). conditional false
// forces a full download — for a stored copy that turned out to be
// unusable, which a 304 would only confirm.
func (c *Client) refreshSnapshot(ctx context.Context, ch db.Character, kind string, conditional bool) (body []byte, notModified bool, err error) {
	path := snapshotPath(ch.CharacterID, kind)
	if path == "" {
		return nil, false, fmt.Errorf("unknown snapshot kind %q", kind)
	}

	token, err := c.tokens(ctx, ch)
	if err != nil {
		return nil, false, err
	}

	var header http.Header
	singleResponse := false
	switch kind {
	case SnapAssets, SnapContracts, SnapBlueprints, SnapMining, SnapContacts:
		// Paginated; the stored snapshot is the merged array so
		// downstream code sees one flat list.
		body, header, err = c.fetchAllPages(ctx, token, path)
	case SnapWalletJournal:
		// Paginated, but only a bounded recent window is stored.
		body, header, err = c.fetchJournalWindow(ctx, token, path)
	case SnapWalletTxns:
		// Steps backward via from_id; bounded recent window.
		body, header, err = c.fetchTxnsWindow(ctx, token, path)
	default:
		singleResponse = true
		etag := ""
		if conditional {
			etag = c.storedSnapshotETag(ctx, ch.CharacterID, kind)
		}
		body, header, notModified, err = c.fetchIfChanged(ctx, token, path, etag)
	}
	if err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	cachedUntil := cacheWindowEnd(header).UTC()
	if notModified {
		if err := c.keepSnapshot(ctx, ch.CharacterID, kind, now, cachedUntil); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}

	// Only a single-response dataset has one ETag that stands for
	// all of it; a merged multi-page payload is stored without.
	etag := ""
	if singleResponse {
		etag = header.Get("ETag")
	}
	if err := c.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: ch.CharacterID,
		Kind:        kind,
		Payload:     string(body),
		FetchedAt:   now,
		CachedUntil: sql.NullTime{Time: cachedUntil, Valid: true},
		Etag:        etag,
	}); err != nil {
		return nil, false, fmt.Errorf("store %s snapshot for character %d: %w", kind, ch.CharacterID, err)
	}
	c.changes.note(ch.CharacterID)
	return body, false, nil
}

// storedSnapshotETag is the ETag the stored snapshot came with, or
// "" when there is no snapshot, it has none, or it cannot be read
// — every one of which simply means "fetch it in full".
func (c *Client) storedSnapshotETag(ctx context.Context, characterID int64, kind string) string {
	etag, err := c.queries.GetSnapshotETag(ctx, db.GetSnapshotETagParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		return ""
	}
	return etag
}

// keepSnapshot renews the cache window of a snapshot ESI reported
// unchanged, leaving its payload and ETag as they are.
func (c *Client) keepSnapshot(ctx context.Context, characterID int64, kind string, now, cachedUntil time.Time) error {
	n, err := c.queries.TouchSnapshot(ctx, db.TouchSnapshotParams{
		FetchedAt:   now,
		CachedUntil: sql.NullTime{Time: cachedUntil, Valid: true},
		CharacterID: characterID,
		Kind:        kind,
	})
	if err != nil {
		return fmt.Errorf("renew %s snapshot for character %d: %w", kind, characterID, err)
	}
	if n == 0 {
		// The row went away between the request and the answer (the
		// character was unlinked). There is nothing to renew.
		return fmt.Errorf("renew %s snapshot for character %d: the snapshot is gone", kind, characterID)
	}
	return nil
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
func (c *Client) FetchAndStoreCorpSnapshot(ctx context.Context, ch db.Character, corporationID int64, kind string) error {
	path, paginated, known := corpSnapshotPath(corporationID, kind)
	if !known {
		return fmt.Errorf("unknown corp snapshot kind %q", kind)
	}

	token, err := c.tokens(ctx, ch)
	if err != nil {
		return err
	}

	// Single-response datasets are fetched conditionally, like the
	// character ones (see FetchAndStoreSnapshot).
	var body []byte
	var header http.Header
	notModified := false
	if paginated {
		body, header, err = c.fetchAllPages(ctx, token, path)
	} else {
		body, header, notModified, err = c.fetchIfChanged(ctx, token, path, c.storedSnapshotETag(ctx, ch.CharacterID, kind))
	}
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	cachedUntil := cacheWindowEnd(header).UTC()
	if notModified {
		return c.keepSnapshot(ctx, ch.CharacterID, kind, now, cachedUntil)
	}

	etag := ""
	if !paginated {
		etag = header.Get("ETag")
	}
	if err := c.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: ch.CharacterID,
		Kind:        kind,
		Payload:     string(body),
		FetchedAt:   now,
		CachedUntil: sql.NullTime{Time: cachedUntil, Valid: true},
		Etag:        etag,
	}); err != nil {
		return fmt.Errorf("store %s snapshot for character %d: %w", kind, ch.CharacterID, err)
	}
	c.changes.note(ch.CharacterID)
	return nil
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
	body, _, err := c.request(ctx, http.MethodPost, token, path, itemIDs, http.StatusOK)
	if err != nil {
		return nil, err
	}
	if err := decodeESI(http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FetchCharacterAssetNames resolves player-given names for a
// character's own asset items (ships, renamed containers) via
// POST /characters/{id}/assets/names/. ESI accepts at most 1,000 item
// IDs per call and rejects the whole batch when one of them is not
// the character's.
func (c *Client) FetchCharacterAssetNames(ctx context.Context, ch db.Character, itemIDs []int64) ([]AssetName, error) {
	token, err := c.tokens(ctx, ch)
	if err != nil {
		return nil, err
	}
	var out []AssetName
	path := fmt.Sprintf("/characters/%d/assets/names/", ch.CharacterID)
	body, _, err := c.request(ctx, http.MethodPost, token, path, itemIDs, http.StatusOK)
	if err != nil {
		return nil, err
	}
	if err := decodeESI(http.MethodPost, path, body, &out); err != nil {
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

	// Each page arrived capped at 8MB, but the merge itself is
	// capped too: without a total, a runaway page count
	// (or a lying X-Pages) would balloon memory for one dataset.
	const maxMergedBytes = 64 << 20
	merged := []json.RawMessage{}
	total := 0
	appendPage := func(b []byte) error {
		var entries []json.RawMessage
		if err := json.Unmarshal(b, &entries); err != nil {
			return fmt.Errorf("ESI GET %s: decode page: %w", path, err)
		}
		total += len(b)
		if total > maxMergedBytes {
			return fmt.Errorf("ESI GET %s: merged pages exceed %d bytes", path, maxMergedBytes)
		}
		merged = append(merged, entries...)
		return nil
	}
	if err := appendPage(body); err != nil {
		return nil, nil, err
	}
	for page := 2; page <= pages; page++ {
		b, _, err := c.FetchRaw(ctx, token, withPageParam(path, page))
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

// withPageParam sets the page query parameter on an ESI path,
// keeping any parameters already there. Appending "?page=" blindly
// breaks the day a paginated kind carries its own query string.
func withPageParam(path string, page int) string {
	if strings.Contains(path, "?") {
		return path + "&page=" + strconv.Itoa(page)
	}
	return path + "?page=" + strconv.Itoa(page)
}

// ---------------------------------------------------------------------------
// Windowed fetches. The wallet journal pages forward via
// X-Pages; wallet transactions step backward via from_id. Both are
// stored as one bounded newest-first window so the snapshot stays
// small and the page renders from one flat list.
// ---------------------------------------------------------------------------

const (
	// maxJournalEntries bounds the stored journal window.
	maxJournalEntries = 300
	// maxJournalPages bounds one journal refresh (each page holds
	// up to 1,000 entries, so the window normally fills on page 1).
	maxJournalPages = 3
	// maxTxnEntries bounds the stored transactions window.
	maxTxnEntries = 300
	// maxTxnRequests bounds the from_id steps in one refresh.
	maxTxnRequests = 3
)

// fetchJournalWindow GETs journal pages (newest first) until the
// window is full or the endpoint runs out of pages, and returns
// the merged array plus the first response's headers.
func (c *Client) fetchJournalWindow(ctx context.Context, token, path string) ([]byte, http.Header, error) {
	merged := []json.RawMessage{}
	var firstHeader http.Header
	for page := 1; page <= maxJournalPages; page++ {
		p := path
		if page > 1 {
			p = withPageParam(path, page)
		}
		body, header, err := c.FetchRaw(ctx, token, p)
		if err != nil {
			return nil, nil, err
		}
		if page == 1 {
			firstHeader = header
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, nil, fmt.Errorf("ESI GET %s: decode page: %w", path, err)
		}
		merged = append(merged, entries...)
		if len(merged) >= maxJournalEntries {
			break
		}
		pages := 1
		if xp := header.Get("X-Pages"); xp != "" {
			if n, aerr := strconv.Atoi(xp); aerr == nil && n > 1 {
				pages = n
			}
		}
		if page >= pages {
			break
		}
	}
	if len(merged) > maxJournalEntries {
		merged = merged[:maxJournalEntries]
	}
	combined, err := json.Marshal(merged)
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: merge pages: %w", path, err)
	}
	return combined, firstHeader, nil
}

// fetchTxnsWindow GETs wallet transactions newest-first, stepping
// backward with from_id (each response covers the transactions
// older than the given one) until the window is full or the
// endpoint runs dry, at most maxTxnRequests calls.
func (c *Client) fetchTxnsWindow(ctx context.Context, token, path string) ([]byte, http.Header, error) {
	merged := []json.RawMessage{}
	var firstHeader http.Header
	var fromID int64
	for call := 0; call < maxTxnRequests; call++ {
		p := path
		if fromID > 0 {
			p = fmt.Sprintf("%s?from_id=%d", path, fromID)
		}
		body, header, err := c.FetchRaw(ctx, token, p)
		if err != nil {
			return nil, nil, err
		}
		if call == 0 {
			firstHeader = header
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, nil, fmt.Errorf("ESI GET %s: decode page: %w", path, err)
		}
		if len(entries) == 0 {
			break
		}
		merged = append(merged, entries...)
		if len(merged) >= maxTxnEntries {
			break
		}
		// Step from the oldest transaction just seen.
		var last struct {
			TransactionID int64 `json:"transaction_id"`
		}
		if err := json.Unmarshal(entries[len(entries)-1], &last); err != nil || last.TransactionID <= 0 {
			break
		}
		fromID = last.TransactionID
	}
	if len(merged) > maxTxnEntries {
		merged = merged[:maxTxnEntries]
	}
	combined, err := json.Marshal(merged)
	if err != nil {
		return nil, nil, fmt.Errorf("ESI GET %s: merge pages: %w", path, err)
	}
	return combined, firstHeader, nil
}

// FetchContractItems GETs one contract's item list with the
// character's token. Contract items are immutable once posted, so
// the worker warms them into the contract_details store once and
// pages render from there (see internal/app/schema 006).
func (c *Client) FetchContractItems(ctx context.Context, ch db.Character, contractID int64) ([]byte, error) {
	token, err := c.tokens(ctx, ch)
	if err != nil {
		return nil, err
	}
	body, _, err := c.FetchRaw(ctx, token, fmt.Sprintf("/characters/%d/contracts/%d/items/", ch.CharacterID, contractID))
	return body, err
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
		logging.Errorf("esi: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	// A stored copy that does not decode is unusable: it has to be
	// downloaded again in full, not merely confirmed as unchanged.
	storedUsable := haveSnap
	if haveSnap && SnapshotFresh(snap) {
		if err := json.Unmarshal([]byte(snap.Payload), out); err == nil {
			return nil
		} else {
			logging.Warnf("esi: decode cached %s for character %d: %v (refetching)", kind, ch.CharacterID, err)
			storedUsable = false
		}
	}

	body, notModified, err := c.refreshSnapshot(ctx, ch, kind, storedUsable)
	if err != nil {
		if haveSnap {
			logging.Warnf("esi: %s fetch for character %d failed (%v); serving stale snapshot", kind, ch.CharacterID, err)
			if derr := json.Unmarshal([]byte(snap.Payload), out); derr == nil {
				return nil
			}
		}
		return err
	}
	if notModified {
		// ESI confirmed the copy read above is still current.
		body = []byte(snap.Payload)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ESI %s for character %d: decode: %w", kind, ch.CharacterID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Global public-data store (Intel cluster). The datasets here are
// public ESI — no character token — so they live in the
// global_snapshots table under the same cache contract as the
// per-character snapshots (raw payload + Expires as cached_until,
// 5-minute fallback). Handlers read the table directly and never
// call this: only the worker fetches.
// ---------------------------------------------------------------------------

// globalSnapshotPath maps a global snapshot kind to its ESI path.
func globalSnapshotPath(kind string) string {
	switch kind {
	case GlobalStatus:
		return "/status/"
	case GlobalWars:
		return "/wars/"
	case GlobalIncursions:
		return "/incursions/"
	case GlobalFWSystems:
		return "/fw/systems/"
	case GlobalFWStats:
		return "/fw/stats/"
	case GlobalFactions:
		return "/universe/factions/"
	}
	return ""
}

// GlobalSnapshotFresh reports whether the stored global payload
// is still inside its ESI cache window.
func GlobalSnapshotFresh(snap db.GlobalSnapshot) bool {
	return time.Now().Before(snap.CachedUntil)
}

// FetchAndStoreGlobalSnapshot fetches the kind's public ESI path
// (no token) and stores the raw payload, honoring the response
// Expires header as cached_until (fallback: now + 5 minutes when
// ESI doesn't send one). It mirrors FetchAndStoreSnapshot's
// bookkeeping for the per-character store.
//
// Like the character datasets, it is fetched conditionally: when
// ESI answers that the stored copy (by its ETag) is still current,
// only the cache window is renewed.
func (c *Client) FetchAndStoreGlobalSnapshot(ctx context.Context, kind string) error {
	path := globalSnapshotPath(kind)
	if path == "" {
		return fmt.Errorf("unknown global snapshot kind %q", kind)
	}

	etag := ""
	if stored, err := c.queries.GetGlobalSnapshot(ctx, kind); err == nil {
		etag = stored.Etag
	}
	body, header, notModified, err := c.fetchIfChanged(ctx, "", path, etag)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	cachedUntil := cacheWindowEnd(header).UTC()
	if notModified {
		n, err := c.queries.TouchGlobalSnapshot(ctx, db.TouchGlobalSnapshotParams{
			FetchedAt:   now,
			CachedUntil: cachedUntil,
			Kind:        kind,
		})
		if err != nil {
			return fmt.Errorf("renew global snapshot %s: %w", kind, err)
		}
		if n == 0 {
			return fmt.Errorf("renew global snapshot %s: the snapshot is gone", kind)
		}
		return nil
	}

	if err := c.queries.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
		Kind:        kind,
		Payload:     string(body),
		FetchedAt:   now,
		CachedUntil: cachedUntil,
		Etag:        header.Get("ETag"),
	}); err != nil {
		return fmt.Errorf("store global snapshot %s: %w", kind, err)
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
			logging.Errorf("esi: type name lookup %d: %v", id, err)
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
			logging.Errorf("esi: persist type name %d: %v", id, err)
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
			logging.Errorf("esi: type group lookup %d: %v", id, err)
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
			logging.Errorf("skills: group name lookup %d: %v", id, err)
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
		logging.Errorf("assets: place lookup %s: %v", path, err)
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
// Player structure names. GET /universe/structures/{id}/ is
// authenticated-only (an unauthenticated call answers 401), so the
// two tiers differ from the public caches above: FetchStructure is
// the network tier and runs in the worker with a linked character's
// token; CachedStructureName is the render tier, in-process map
// then the durable structure_names table, and never fetches.
// ---------------------------------------------------------------------------

// UniverseStructure is GET /universe/structures/{structure_id}/
// (authenticated; needs esi-universe.read_structures.v1).
type UniverseStructure struct {
	Name          string `json:"name"`
	SolarSystemID int64  `json:"solar_system_id"`
	TypeID        int64  `json:"type_id"`
}

// StructureState values for the structure_names table (schema
// 014), shared with the app layer's queue bookkeeping.
const (
	StructurePending  = "pending"
	StructureResolved = "resolved"
	StructureMissing  = "missing"
)

// StructureSource values for the structure_names.source column
// (schema 023): where a cached name came from. ESI truth — the
// authenticated lookup or a corporation structure list — always
// outranks the community tier; the app layer enforces that order
// when storing. Community is plumbed but inactive: no dataset
// ships yet, and no name is ever invented to fill the tier.
const (
	StructureSourceESI       = "esi"
	StructureSourceCorp      = "corp"
	StructureSourceCommunity = "community"
)

// FetchStructure resolves one structure id with a character's
// token (network tier). A 403 means the token can see the
// structure exists but not its name (no docking access); 404
// means it is gone. One character's 403 is not the final word —
// callers try every eligible character and record a negative
// answer only once the whole set has answered no.
func (c *Client) FetchStructure(ctx context.Context, ch db.Character, structureID int64) (UniverseStructure, error) {
	token, err := c.tokens(ctx, ch)
	if err != nil {
		return UniverseStructure{}, err
	}
	var out UniverseStructure
	if err := c.Get(ctx, token, fmt.Sprintf("/universe/structures/%d/", structureID), &out); err != nil {
		return UniverseStructure{}, err
	}
	return out, nil
}

// CachedStructureName answers from the in-process cache, then the
// durable structure_names table. Render tier: never fetches.
func (c *Client) CachedStructureName(ctx context.Context, structureID int64) (string, bool) {
	c.structNamesMu.RLock()
	name, ok := c.structNames[structureID]
	c.structNamesMu.RUnlock()
	if ok {
		return name, true
	}
	row, err := c.queries.GetStructureName(ctx, structureID)
	if err != nil || row.State != StructureResolved || row.Name == "" {
		return "", false
	}
	c.StoreStructureName(structureID, row.Name)
	return row.Name, true
}

// StoreStructureName records a resolved name in the in-process
// cache (the app layer persists it in structure_names).
func (c *Client) StoreStructureName(structureID int64, name string) {
	c.structNamesMu.Lock()
	c.structNames[structureID] = name
	c.structNamesMu.Unlock()
}

// ---------------------------------------------------------------------------
// Planet names. GET /universe/planets/{planet_id}/ is public (no
// token), and planet names never change, so resolution is cheap
// and durable: FetchPlanet is the network tier (worker only),
// CachedPlanetName is the render tier — in-process place cache,
// then the durable planet_names table — and never fetches.
// ---------------------------------------------------------------------------

// UniversePlanet is GET /universe/planets/{planet_id}/ (public).
// Only the name feeds the app today; system_id and type_id ride
// along for future surfaces.
type UniversePlanet struct {
	Name     string `json:"name"`
	PlanetID int64  `json:"planet_id"`
	SystemID int64  `json:"system_id"`
	TypeID   int64  `json:"type_id"`
}

// PlanetState values for the planet_names table (schema 025),
// shared with the app layer's queue bookkeeping.
const (
	PlanetPending  = "pending"
	PlanetResolved = "resolved"
	PlanetMissing  = "missing"
)

// FetchPlanet resolves one planet id (network tier; the endpoint
// is public, so no token is involved). A 404 means the id is not
// a planet — colony planet ids always exist, so callers treat a
// 404 as a rare negative and back it off rather than failing.
func (c *Client) FetchPlanet(ctx context.Context, planetID int64) (UniversePlanet, error) {
	var out UniversePlanet
	if err := c.Get(ctx, "", fmt.Sprintf("/universe/planets/%d/", planetID), &out); err != nil {
		return UniversePlanet{}, err
	}
	return out, nil
}

// CachedPlanetName answers from the in-process place cache (the
// worker's warm passes store planet names there), then the
// durable planet_names table. Render tier: never fetches.
func (c *Client) CachedPlanetName(ctx context.Context, planetID int64) (string, bool) {
	c.placeMu.Lock()
	name, ok := c.placeNames[planetID]
	c.placeMu.Unlock()
	if ok {
		return name, true
	}
	row, err := c.queries.GetPlanetName(ctx, planetID)
	if err != nil || row.State != PlanetResolved || row.Name == "" {
		return "", false
	}
	c.StorePlaceName(planetID, row.Name)
	return row.Name, true
}

// StorePlanetName records a resolved planet name in the
// in-process place cache (the app layer persists it in
// planet_names).
func (c *Client) StorePlanetName(planetID int64, name string) {
	c.StorePlaceName(planetID, name)
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
	if c.CharacterNameMissed(id) {
		return "", fmt.Errorf("character %d: not a character (remembered answer)", id)
	}
	var ch Character
	if err := c.Get(ctx, "", fmt.Sprintf("/characters/%d/", id), &ch); err != nil {
		// A 404/422 here is definitive — the ID belongs to some
		// other entity kind (or nothing) — so remember it instead
		// of letting the caller re-ask every cycle.
		if code, ok := StatusCode(err); ok && (code == http.StatusNotFound || code == 422) {
			c.StoreCharacterNameMiss(id)
		}
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

// CharacterNameMissed reports whether ESI has already definitively
// answered that this ID is not a character (see charNameMisses).
func (c *Client) CharacterNameMissed(id int64) bool {
	c.charNamesMu.RLock()
	defer c.charNamesMu.RUnlock()
	return c.charNameMisses[id]
}

// StoreCharacterNameMiss records a definitive "not a character"
// answer for an ID so the warm-up pass skips it from now on.
func (c *Client) StoreCharacterNameMiss(id int64) {
	c.charNamesMu.Lock()
	c.charNameMisses[id] = true
	c.charNamesMu.Unlock()
}

// ---------------------------------------------------------------------------
// Organization and constellation names: war parties and
// incursion constellations. Same two tiers as character names —
// the Cached* accessors never touch the network; the worker warms
// the caches from war details and the incursions snapshot.
// ---------------------------------------------------------------------------

// CachedCorpName resolves a corporation ID from the in-process
// cache only. Zero network.
func (c *Client) CachedCorpName(id int64) (string, bool) {
	c.corpNamesMu.RLock()
	defer c.corpNamesMu.RUnlock()
	name, ok := c.corpNames[id]
	return name, ok
}

// StoreCorpName records a corporation name in the in-process
// cache (the worker's warm-up pass stores fetched names this way).
func (c *Client) StoreCorpName(id int64, name string) {
	c.corpNamesMu.Lock()
	c.corpNames[id] = name
	c.corpNamesMu.Unlock()
}

// CachedAllianceName resolves an alliance ID from the in-process
// cache only. Zero network.
func (c *Client) CachedAllianceName(id int64) (string, bool) {
	c.allianceNamesMu.RLock()
	defer c.allianceNamesMu.RUnlock()
	name, ok := c.allianceNames[id]
	return name, ok
}

// StoreAllianceName records an alliance name in the in-process
// cache.
func (c *Client) StoreAllianceName(id int64, name string) {
	c.allianceNamesMu.Lock()
	c.allianceNames[id] = name
	c.allianceNamesMu.Unlock()
}

// CachedConstellationName resolves a constellation ID from the
// in-process cache only. Zero network.
func (c *Client) CachedConstellationName(id int64) (string, bool) {
	c.constellationNamesMu.RLock()
	defer c.constellationNamesMu.RUnlock()
	name, ok := c.constellationNames[id]
	return name, ok
}

// StoreConstellationName records a constellation name in the
// in-process cache.
func (c *Client) StoreConstellationName(id int64, name string) {
	c.constellationNamesMu.Lock()
	c.constellationNames[id] = name
	c.constellationNamesMu.Unlock()
}

// ---------------------------------------------------------------------------
// PI schematics: same two tiers as the name caches — FetchSchematic
// is the network tier (public GET /universe/schematics/{id}/, no
// token) for the worker's warm-up pass; CachedSchematic is the
// render tier and never touches the network.
// ---------------------------------------------------------------------------

// FetchSchematic GETs one schematic (public endpoint) and caches
// it. Errors carry the path and status, nothing sensitive.
func (c *Client) FetchSchematic(ctx context.Context, id int64) (Schematic, error) {
	var s Schematic
	if err := c.Get(ctx, "", fmt.Sprintf("/universe/schematics/%d/", id), &s); err != nil {
		return Schematic{}, err
	}
	c.StoreSchematic(id, s)
	return s, nil
}

// CachedSchematic resolves a schematic ID from the in-process
// cache only. Zero network.
func (c *Client) CachedSchematic(id int64) (Schematic, bool) {
	c.schematicsMu.RLock()
	defer c.schematicsMu.RUnlock()
	s, ok := c.schematics[id]
	return s, ok
}

// StoreSchematic records a fetched schematic in the in-process
// cache (the worker's warm-up pass stores schematics this way).
func (c *Client) StoreSchematic(id int64, s Schematic) {
	c.schematicsMu.Lock()
	c.schematics[id] = s
	c.schematicsMu.Unlock()
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
