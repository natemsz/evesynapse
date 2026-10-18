package swarm

import (
	"context"
	"evesynapse/internal/app/module"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Swarm-first warming: spend the swarm's fetch capacity before this
// instance's own.
//
// A dedicated warming node (SHARED_CACHE_WARM) fetches the swarm's
// misses on its own ESI budget and serves them like any other peer
// does. Mains spend their own budget on a shared kind only as the last
// resort:
//
//  1. a miss of a shared kind (an anonymous page view, a signed-in one,
//     a hop from another record) is queued as usual and goes out with
//     the next pull as demand;
//  2. a dedicated node that warms that kind and does not hold the key
//     takes the request as demand: the key joins its own queue at
//     viewed priority, its worker fetches it, and a later pull brings
//     it back to the asking instance through the unchanged trust
//     pipeline (staging, spot checks, apply);
//  3. while a dedicated node covers the kind, the main's own worker
//     leaves the key alone for SwarmWarmGrace; only after that does it
//     spend its own Call, so a node that cannot deliver costs
//     freshness lag, never a missing record.
//
// The wire format is unchanged for older peers: a node says what it
// warms on its own endpoint (GET /swarm/warms, a 404 on older
// instances, read as warming nothing), and demand is the pull request
// it already received. Nothing is pushed into another instance.
// ---------------------------------------------------------------------------

const (
	// SwarmWarmGrace is how long a main leaves a covered miss to the
	// swarm before spending its own ESI Call on it: a dedicated
	// node's cycle to fetch it plus the main's next pull to bring it
	// back, with room to spare.
	SwarmWarmGrace = 15 * time.Minute
	// SwarmCoverRefresh is how often a peer is asked what it warms.
	SwarmCoverRefresh = time.Hour
	// MaxSwarmCoverAsks bounds the coverage questions one cycle sends.
	MaxSwarmCoverAsks = 5
	// MaxSwarmWarmPeers bounds the dedicated nodes every pull adds on
	// top of the rotation, so demand reaches them each cycle.
	MaxSwarmWarmPeers = 3
)

// SwarmWarmKinds are the kinds a dedicated node can warm for others:
// every shared kind. A structure name needs a character with docking
// access, so a node only covers it when its operator says so.
var SwarmWarmKinds = []string{SwarmKindCorporation, SwarmKindAlliance, SwarmKindTypeName, SwarmKindHistory, SwarmKindStructure, SwarmKindContract}

// ParseSwarmWarm reads SHARED_CACHE_WARM: "all", or a comma list of
// shared kinds. Anything else is dropped with a warning.
func ParseSwarmWarm(raw string) map[string]bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "off" || raw == "none" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "all":
			for _, k := range SwarmWarmKinds {
				out[k] = true
			}
		case ValidSwarmKind(part):
			out[part] = true
		case part != "":
			logging.Warnf("evesynapse: SHARED_CACHE_WARM: %q is not a shared kind; ignored", part)
		}
	}
	return out
}

// SwarmSelfWarms reports whether this instance is a dedicated node for
// kind: it fetches that kind for the swarm, so it never defers it.
func (app *Mod) SwarmSelfWarms(kind string) bool {
	return app.Cfg.Enabled() && app.Cfg.Warms[kind]
}

// HandleSwarmWarms serves GET /swarm/warms: the kinds this instance
// warms for the swarm. Bearer-gated like every swarm endpoint.
func (app *Mod) HandleSwarmWarms(w http.ResponseWriter, r *http.Request) {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !SwarmAuth(app.Cfg.Secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	kinds := []string{}
	for _, k := range SwarmWarmKinds {
		if app.Cfg.Warms[k] {
			kinds = append(kinds, k)
		}
	}
	WriteSwarmJSON(w, map[string]any{"kinds": kinds})
}

// WarmsOf asks one peer what it warms. An older peer answers 404,
// which reads as nothing.
func (c *SwarmClient) WarmsOf(ctx context.Context, baseURL string) ([]string, error) {
	var body struct {
		Kinds []string `json:"kinds"`
	}
	if err := c.AuthedGET(ctx, strings.TrimSuffix(baseURL, "/")+"/swarm/warms", &body); err != nil {
		return nil, err
	}
	return body.Kinds, nil
}

// SwarmCoverage is what each peer said it warms, and when it said so.
type SwarmCoverage struct {
	Mu     sync.Mutex
	ByPeer map[string]SwarmCover
	// asked is when a covered miss was first left to the swarm,
	// by kind/key; the grace runs from it.
	Asked map[string]time.Time
}

type SwarmCover struct {
	BaseURL string
	Kinds   map[string]bool
	At      time.Time
}

// SwarmRefreshCoverage asks the approved, trusted peers whose answer
// is older than SwarmCoverRefresh what they warm, a few a cycle.
func (app *Mod) SwarmRefreshCoverage(ctx context.Context, now time.Time) {
	peers, err := app.Queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: coverage peers: %v", err)
		return
	}
	asked := 0
	for _, p := range peers {
		if asked >= MaxSwarmCoverAsks || ctx.Err() != nil {
			break
		}
		if !p.Approved || p.Excluded || !SwarmPeerPullable(p.GoodChecks, p.BadChecks) {
			app.SwarmForgetCover(p.InstanceID)
			continue
		}
		app.SwarmCover.Mu.Lock()
		prev, known := app.SwarmCover.ByPeer[p.InstanceID]
		app.SwarmCover.Mu.Unlock()
		if known && now.Sub(prev.At) < SwarmCoverRefresh {
			continue
		}
		asked++
		kinds, err := app.SwarmCli.WarmsOf(ctx, p.BaseUrl)
		cover := SwarmCover{BaseURL: p.BaseUrl, Kinds: map[string]bool{}, At: now}
		if err == nil {
			for _, k := range kinds {
				if ValidSwarmKind(k) {
					cover.Kinds[k] = true
				}
			}
		}
		app.SwarmCover.Mu.Lock()
		if app.SwarmCover.ByPeer == nil {
			app.SwarmCover.ByPeer = map[string]SwarmCover{}
		}
		app.SwarmCover.ByPeer[p.InstanceID] = cover
		app.SwarmCover.Mu.Unlock()
	}
}

func (app *Mod) SwarmForgetCover(instanceID string) {
	app.SwarmCover.Mu.Lock()
	delete(app.SwarmCover.ByPeer, instanceID)
	app.SwarmCover.Mu.Unlock()
}

// SwarmCovered reports whether a trusted peer said lately that it
// warms kind.
func (app *Mod) SwarmCovered(kind string, now time.Time) bool {
	app.SwarmCover.Mu.Lock()
	defer app.SwarmCover.Mu.Unlock()
	for _, c := range app.SwarmCover.ByPeer {
		if c.Kinds[kind] && now.Sub(c.At) < 2*SwarmCoverRefresh {
			return true
		}
	}
	return false
}

// SwarmWarmPeerIDs are the peers that warm anything, for the pull to
// ask every cycle.
func (app *Mod) SwarmWarmPeerIDs(now time.Time) map[string]bool {
	out := map[string]bool{}
	for id := range app.SwarmWarmPeerURLs(now) {
		out[id] = true
	}
	return out
}

// SwarmWarmPeerURLs maps each peer that warms anything to its base URL.
func (app *Mod) SwarmWarmPeerURLs(now time.Time) map[string]string {
	app.SwarmCover.Mu.Lock()
	defer app.SwarmCover.Mu.Unlock()
	out := map[string]string{}
	for id, c := range app.SwarmCover.ByPeer {
		if len(c.Kinds) > 0 && now.Sub(c.At) < 2*SwarmCoverRefresh {
			out[id] = c.BaseURL
		}
	}
	return out
}

// SwarmDefersFetch reports whether this instance leaves one due key to
// the swarm instead of spending its own ESI Call on it now:
//
//   - a fresh staged entry for it is moments from a spot decision
//     (the pull already brought it; fetching it again would spend
//     the Call the pull saved);
//   - or a dedicated node warms the kind and the key has been left
//     to it for less than SwarmWarmGrace.
//
// A dedicated node never defers a kind it warms itself: it is the one
// that fetches. Deferring only delays a fetch, never cancels it.
func (app *Mod) SwarmDefersFetch(ctx context.Context, kind, key string, now time.Time) bool {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		return false
	}
	if app.SwarmSuppressFetch(ctx, kind, key, now) {
		return true
	}
	if app.SwarmSelfWarms(kind) || !app.SwarmCovered(kind, now) {
		return false
	}
	id := kind + "|" + key
	app.SwarmCover.Mu.Lock()
	defer app.SwarmCover.Mu.Unlock()
	if app.SwarmCover.Asked == nil {
		app.SwarmCover.Asked = map[string]time.Time{}
	}
	first, ok := app.SwarmCover.Asked[id]
	if !ok {
		app.SwarmCover.Asked[id] = now
		return true
	}
	return now.Sub(first) < SwarmWarmGrace
}

// SwarmPruneAsked drops grace marks long past their window, so the
// book holds only keys still in play.
func (app *Mod) SwarmPruneAsked(now time.Time) {
	app.SwarmCover.Mu.Lock()
	defer app.SwarmCover.Mu.Unlock()
	for id, at := range app.SwarmCover.Asked {
		if now.Sub(at) > 4*SwarmWarmGrace {
			delete(app.SwarmCover.Asked, id)
		}
	}
}

// SwarmNoteDemand is a dedicated node taking a pull's miss as demand:
// the key joins its own queue at viewed priority, so its worker
// fetches it on its own budget and a later pull finds it served.
func (app *Mod) SwarmNoteDemand(ctx context.Context, kind, key string) {
	if !app.SwarmSelfWarms(kind) {
		return
	}
	// public_contract deliberately has no case here: the public
	// contracts route is by region, so a node cannot fetch a
	// header from a contract ID alone, and its own sweep covers
	// discovery anyway. What a node can warm is the item list
	// once its sweep has seen the header — that demand travels
	// as its own pending queue, like every main's.
	var err error
	switch kind {
	case SwarmKindCorporation, SwarmKindAlliance, SwarmKindTypeName, SwarmKindStructure:
		id, perr := strconv.ParseInt(key, 10, 64)
		if perr != nil || id <= 0 {
			return
		}
		switch kind {
		case SwarmKindCorporation:
			err = app.Deps.WantCorporation(ctx, id, module.WantViewed)
		case SwarmKindAlliance:
			err = app.Deps.WantAlliance(ctx, id, module.WantViewed)
		case SwarmKindTypeName:
			app.Deps.NoteTypeNameMiss(id)
		case SwarmKindStructure:
			if module.IsStructureID(id) {
				app.Deps.NoteStructureIDs(ctx, id)
			}
		}
	case SwarmKindHistory:
		var regionID, typeID int64
		if _, serr := fmt.Sscanf(key, "%d/%d", &regionID, &typeID); serr != nil || regionID <= 0 || typeID <= 0 {
			return
		}
		err = app.Queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: regionID, TypeID: typeID, LastRequestedAt: time.Now().UTC(), Priority: module.WantViewed,
		})
	}
	if err != nil {
		logging.Errorf("swarm: note %s demand for %s: %v", kind, key, err)
	}
}
