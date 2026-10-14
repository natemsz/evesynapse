package app

import (
	"context"
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
//     leaves the key alone for swarmWarmGrace; only after that does it
//     spend its own call, so a node that cannot deliver costs
//     freshness lag, never a missing record.
//
// The wire format is unchanged for older peers: a node says what it
// warms on its own endpoint (GET /swarm/warms, a 404 on older
// instances, read as warming nothing), and demand is the pull request
// it already received. Nothing is pushed into another instance.
// ---------------------------------------------------------------------------

const (
	// swarmWarmGrace is how long a main leaves a covered miss to the
	// swarm before spending its own ESI call on it: a dedicated
	// node's cycle to fetch it plus the main's next pull to bring it
	// back, with room to spare.
	swarmWarmGrace = 15 * time.Minute
	// swarmCoverRefresh is how often a peer is asked what it warms.
	swarmCoverRefresh = time.Hour
	// maxSwarmCoverAsks bounds the coverage questions one cycle sends.
	maxSwarmCoverAsks = 5
	// maxSwarmWarmPeers bounds the dedicated nodes every pull adds on
	// top of the rotation, so demand reaches them each cycle.
	maxSwarmWarmPeers = 3
)

// swarmWarmKinds are the kinds a dedicated node can warm for others:
// every shared kind. A structure name needs a character with docking
// access, so a node only covers it when its operator says so.
var swarmWarmKinds = []string{swarmKindCorporation, swarmKindAlliance, swarmKindTypeName, swarmKindHistory, swarmKindStructure}

// parseSwarmWarm reads SHARED_CACHE_WARM: "all", or a comma list of
// shared kinds. Anything else is dropped with a warning.
func parseSwarmWarm(raw string) map[string]bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "off" || raw == "none" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "all":
			for _, k := range swarmWarmKinds {
				out[k] = true
			}
		case validSwarmKind(part):
			out[part] = true
		case part != "":
			logging.Warnf("evesynapse: SHARED_CACHE_WARM: %q is not a shared kind; ignored", part)
		}
	}
	return out
}

// swarmSelfWarms reports whether this instance is a dedicated node for
// kind: it fetches that kind for the swarm, so it never defers it.
func (app *Application) swarmSelfWarms(kind string) bool {
	return app.cfg.swarm.enabled() && app.cfg.swarm.warms[kind]
}

// handleSwarmWarms serves GET /swarm/warms: the kinds this instance
// warms for the swarm. Bearer-gated like every swarm endpoint.
func (app *Application) handleSwarmWarms(w http.ResponseWriter, r *http.Request) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !swarmAuth(app.cfg.swarm.secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	kinds := []string{}
	for _, k := range swarmWarmKinds {
		if app.cfg.swarm.warms[k] {
			kinds = append(kinds, k)
		}
	}
	writeSwarmJSON(w, map[string]any{"kinds": kinds})
}

// warmsOf asks one peer what it warms. An older peer answers 404,
// which reads as nothing.
func (c *swarmClient) warmsOf(ctx context.Context, baseURL string) ([]string, error) {
	var body struct {
		Kinds []string `json:"kinds"`
	}
	if err := c.authedGET(ctx, strings.TrimSuffix(baseURL, "/")+"/swarm/warms", &body); err != nil {
		return nil, err
	}
	return body.Kinds, nil
}

// swarmCoverage is what each peer said it warms, and when it said so.
type swarmCoverage struct {
	mu     sync.Mutex
	byPeer map[string]swarmCover
	// asked is when a covered miss was first left to the swarm,
	// by kind/key; the grace runs from it.
	asked map[string]time.Time
}

type swarmCover struct {
	baseURL string
	kinds   map[string]bool
	at      time.Time
}

// swarmRefreshCoverage asks the approved, trusted peers whose answer
// is older than swarmCoverRefresh what they warm, a few a cycle.
func (app *Application) swarmRefreshCoverage(ctx context.Context, now time.Time) {
	peers, err := app.queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: coverage peers: %v", err)
		return
	}
	asked := 0
	for _, p := range peers {
		if asked >= maxSwarmCoverAsks || ctx.Err() != nil {
			break
		}
		if !p.Approved || p.Excluded || !swarmPeerPullable(p.GoodChecks, p.BadChecks) {
			app.swarmForgetCover(p.InstanceID)
			continue
		}
		app.swarmCover.mu.Lock()
		prev, known := app.swarmCover.byPeer[p.InstanceID]
		app.swarmCover.mu.Unlock()
		if known && now.Sub(prev.at) < swarmCoverRefresh {
			continue
		}
		asked++
		kinds, err := app.swarmCli.warmsOf(ctx, p.BaseUrl)
		cover := swarmCover{baseURL: p.BaseUrl, kinds: map[string]bool{}, at: now}
		if err == nil {
			for _, k := range kinds {
				if validSwarmKind(k) {
					cover.kinds[k] = true
				}
			}
		}
		app.swarmCover.mu.Lock()
		if app.swarmCover.byPeer == nil {
			app.swarmCover.byPeer = map[string]swarmCover{}
		}
		app.swarmCover.byPeer[p.InstanceID] = cover
		app.swarmCover.mu.Unlock()
	}
}

func (app *Application) swarmForgetCover(instanceID string) {
	app.swarmCover.mu.Lock()
	delete(app.swarmCover.byPeer, instanceID)
	app.swarmCover.mu.Unlock()
}

// swarmCovered reports whether a trusted peer said lately that it
// warms kind.
func (app *Application) swarmCovered(kind string, now time.Time) bool {
	app.swarmCover.mu.Lock()
	defer app.swarmCover.mu.Unlock()
	for _, c := range app.swarmCover.byPeer {
		if c.kinds[kind] && now.Sub(c.at) < 2*swarmCoverRefresh {
			return true
		}
	}
	return false
}

// swarmWarmPeerIDs are the peers that warm anything, for the pull to
// ask every cycle.
func (app *Application) swarmWarmPeerIDs(now time.Time) map[string]bool {
	out := map[string]bool{}
	for id := range app.swarmWarmPeerURLs(now) {
		out[id] = true
	}
	return out
}

// swarmWarmPeerURLs maps each peer that warms anything to its base URL.
func (app *Application) swarmWarmPeerURLs(now time.Time) map[string]string {
	app.swarmCover.mu.Lock()
	defer app.swarmCover.mu.Unlock()
	out := map[string]string{}
	for id, c := range app.swarmCover.byPeer {
		if len(c.kinds) > 0 && now.Sub(c.at) < 2*swarmCoverRefresh {
			out[id] = c.baseURL
		}
	}
	return out
}

// swarmDefersFetch reports whether this instance leaves one due key to
// the swarm instead of spending its own ESI call on it now:
//
//   - a fresh staged entry for it is moments from a spot decision
//     (the pull already brought it; fetching it again would spend
//     the call the pull saved);
//   - or a dedicated node warms the kind and the key has been left
//     to it for less than swarmWarmGrace.
//
// A dedicated node never defers a kind it warms itself: it is the one
// that fetches. Deferring only delays a fetch, never cancels it.
func (app *Application) swarmDefersFetch(ctx context.Context, kind, key string, now time.Time) bool {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		return false
	}
	if app.swarmSuppressFetch(ctx, kind, key, now) {
		return true
	}
	if app.swarmSelfWarms(kind) || !app.swarmCovered(kind, now) {
		return false
	}
	id := kind + "|" + key
	app.swarmCover.mu.Lock()
	defer app.swarmCover.mu.Unlock()
	if app.swarmCover.asked == nil {
		app.swarmCover.asked = map[string]time.Time{}
	}
	first, ok := app.swarmCover.asked[id]
	if !ok {
		app.swarmCover.asked[id] = now
		return true
	}
	return now.Sub(first) < swarmWarmGrace
}

// swarmPruneAsked drops grace marks long past their window, so the
// book holds only keys still in play.
func (app *Application) swarmPruneAsked(now time.Time) {
	app.swarmCover.mu.Lock()
	defer app.swarmCover.mu.Unlock()
	for id, at := range app.swarmCover.asked {
		if now.Sub(at) > 4*swarmWarmGrace {
			delete(app.swarmCover.asked, id)
		}
	}
}

// swarmNoteDemand is a dedicated node taking a pull's miss as demand:
// the key joins its own queue at viewed priority, so its worker
// fetches it on its own budget and a later pull finds it served.
func (app *Application) swarmNoteDemand(ctx context.Context, kind, key string) {
	if !app.swarmSelfWarms(kind) {
		return
	}
	var err error
	switch kind {
	case swarmKindCorporation, swarmKindAlliance, swarmKindTypeName, swarmKindStructure:
		id, perr := strconv.ParseInt(key, 10, 64)
		if perr != nil || id <= 0 {
			return
		}
		switch kind {
		case swarmKindCorporation:
			err = app.wantCorporation(ctx, id, wantViewed)
		case swarmKindAlliance:
			err = app.wantAlliance(ctx, id, wantViewed)
		case swarmKindTypeName:
			app.esi.NoteTypeNameMiss(id)
		case swarmKindStructure:
			if isStructureID(id) {
				app.noteStructureIDs(ctx, id)
			}
		}
	case swarmKindHistory:
		var regionID, typeID int64
		if _, serr := fmt.Sscanf(key, "%d/%d", &regionID, &typeID); serr != nil || regionID <= 0 || typeID <= 0 {
			return
		}
		err = app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: regionID, TypeID: typeID, LastRequestedAt: time.Now().UTC(), Priority: wantViewed,
		})
	}
	if err != nil {
		logging.Errorf("swarm: note %s demand for %s: %v", kind, key, err)
	}
}
