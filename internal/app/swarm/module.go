// Package swarm is the Shared Cache module: instances of EveSynapse share
// the public data each has fetched (market history, type names, corporation
// and alliance records, player structure names) with signed, spot-checked
// entries, so a fetch is made once for the whole swarm. It reaches core by
// three explicit hooks: a FetchGate (core asks before spending ESI, and any
// gate failure means fetch normally), two worker-cycle jobs that keep the
// cycle's timing names ("swarm fetch" and "swarm"), and the admin and sync
// page sections. Configuration is read from the environment at start; the
// tables and the fetch pipeline stay core's.
package swarm

import (
	"context"
	"html/template"
	"net/http"
	"os"

	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"fmt"
	"github.com/go-chi/chi/v5"
	"strconv"
	"strings"
	"sync"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Package swarm is the Shared Cache module: instances of EveSynapse share
// the public data each has fetched (market history, type names, corporation
// and alliance records, player structure names) with signed, spot-checked
// entries, so a fetch is made once for the whole swarm. It reaches core by
// three explicit hooks: a FetchGate (core asks before spending ESI, and any
// gate failure means fetch normally), two worker-cycle jobs that keep the
// cycle's timing names ("swarm fetch" and "swarm"), and the admin and sync
// page sections. Configuration is read from the environment at start; the
// tables and the fetch pipeline stay core's.
//
// Mod is the swarm's state and its view of the foundation: the identity,
// the peer client, the gossip clock, the failure streaks and the coverage
// peers have promised. One lives per application (StateFor).
type Mod struct {
	Deps    module.Deps
	Queries *db.Queries

	// Cfg is the swarm's settings, read from the environment at startup.
	Cfg SwarmConfig

	// The signing identity (identity.go): minted on first start.
	SwarmID  *SwarmIdentity
	SwarmCli *SwarmClient
	// SwarmMu guards the next gossip time only; the identity is
	// immutable after start.
	SwarmMu         sync.Mutex
	SwarmNextGossip time.Time
	SwarmNextLoaded bool
	// SwarmStreakMu guards the consecutive-failure streaks.
	SwarmStreakMu sync.Mutex
	SwarmStreak   map[string]int
	// SwarmCover is what peers warm for the swarm and which misses
	// have been left to them (warm.go).
	SwarmCover SwarmCoverage
}

var states sync.Map // module.Deps -> *Mod

// StateFor returns the swarm state of one application, creating it on
// first use.
func StateFor(deps module.Deps) *Mod {
	if v, ok := states.Load(deps); ok {
		return v.(*Mod)
	}
	v, _ := states.LoadOrStore(deps, &Mod{Deps: deps, Queries: deps.DB()})
	return v.(*Mod)
}

// timeSet is a valid timestamp for a nullable column.
func timeSet(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }

// characterSyncs reports whether a character is fetched: only healthy
// links are; token_dead and owner_changed characters wait for a sign-in.
func characterSyncs(ch db.Character) bool {
	return ch.LinkState == "" || ch.LinkState == module.LinkStateOK
}

// gcmForKey is AES-256-GCM over a key.
func gcmForKey(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func percentOf(part, whole int64) string {
	if whole <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", (part*100+whole/2)/whole)
}

// intSetting reads a whole-number setting: the default when it is unset
// or not a positive whole number (which the log says), else the number
// kept between lo and hi.
func intSetting(name, raw string, lo, hi, unset int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return unset
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logging.Warnf("evesynapse: %s=%q is not a whole number; using the default", name, raw)
		return unset
	}
	return min(max(n, lo), hi)
}

// oneFetch is a fetch allowance of a fixed size, for a check that is not
// part of a worker cycle.
type oneFetch struct {
	Mu   sync.Mutex
	Left int
}

func (b *oneFetch) Take() bool {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	if b.Left <= 0 {
		return false
	}
	b.Left--
	return true
}

func (b *oneFetch) Exhausted() bool {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	return b.Left <= 0
}

// Module is the Shared Cache module's registration.
var Module = module.Module{
	ID:       "swarm",
	Title:    "Shared Cache",
	Requires: []string{},
	// Sharing needs no scope of its own.
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: module.StructureScope, Unlocks: "Shared structure names are spot-checked with a character that can read the structure."},
	},
	Init:       initSwarm,
	FetchGates: []module.FetchGate{gate{}},
	CycleJobs: []module.CycleJob{
		{Name: "swarm fetch", Describe: "asking the swarm first", At: module.BeforePublicData, Run: func(ctx context.Context, deps module.Deps, c module.Cycle) (int, bool) {
			StateFor(deps).SwarmFetchPhase(ctx, c.Now)
			return 0, false
		}},
		{Name: "swarm", Describe: "sharing with the swarm", At: module.AfterPublicData, Run: func(ctx context.Context, deps module.Deps, c module.Cycle) (int, bool) {
			return StateFor(deps).SwarmCycle(ctx, c.Budget, c.Characters, c.Now)
		}},
	},
	AdminSections: []module.PageSection{{Anchor: "admin-swarm", Title: "Swarm Cache", Render: func(ctx context.Context, deps module.Deps) template.HTML {
		html, err := deps.Fragment("swarm.html", "swarm-admin", StateFor(deps).LoadSwarmAdmin(ctx))
		if err != nil {
			logging.Errorf("swarm: admin section: %v", err)
			return ""
		}
		return html
	}}},
	SyncSections: []module.PageSection{{Anchor: "sync-swarm", Title: "Swarm cache", Render: func(ctx context.Context, deps module.Deps) template.HTML {
		view := StateFor(deps).LoadSwarmMetrics(ctx)
		if view == nil {
			return ""
		}
		html, err := deps.Fragment("swarm.html", "swarm-sync", view)
		if err != nil {
			logging.Errorf("swarm: sync section: %v", err)
			return ""
		}
		return html
	}}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*Mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(StateFor(deps), w, req) }
		}
		// The members of the swarm, by the administrators.
		admin := r.With(deps.RequireAdmin())
		admin.Post("/admin/swarm/add", h((*Mod).HandleSwarmAddPeer))
		admin.Post("/admin/swarm/approve", h((*Mod).HandleSwarmApprove))
		admin.Post("/admin/swarm/exclude", h((*Mod).HandleSwarmExclude))
		admin.Post("/admin/swarm/admit", h((*Mod).HandleSwarmAdmit))
		admin.Post("/admin/swarm/remove", h((*Mod).HandleSwarmRemove))
		// The machine API: bearer-gated by the shared secret, dark (404)
		// while sharing is off. No session middleware: instances are not
		// browsers.
		r.Get("/swarm/members", h((*Mod).HandleSwarmMembers))
		r.Post("/swarm/pull", h((*Mod).HandleSwarmPull))
		r.Get("/swarm/warms", h((*Mod).HandleSwarmWarms))
		r.Post("/swarm/seed/register", h((*Mod).HandleSwarmSeedRegister))
	},
}

// gate is the swarm's FetchGate: peers may have the data already.
type gate struct{}

func (gate) Defer(ctx context.Context, deps module.Deps, kind, key string, now time.Time) bool {
	return StateFor(deps).SwarmDefersFetch(ctx, kind, key, now)
}

func (gate) UrgentHistory(ctx context.Context, deps module.Deps, regionID, typeID int64) bool {
	return StateFor(deps).SwarmUrgentHistory(ctx, module.MarketKey{RegionID: regionID, TypeID: typeID})
}

// initSwarm reads the settings and, when sharing is on, loads (or mints)
// the signing identity. Sharing stays off without a good one, and the
// start fails rather than sharing unsigned.
func initSwarm(deps module.Deps) error {
	cfg := LoadSwarmConfig(os.Getenv)
	if cfg.Err != nil {
		return cfg.Err
	}
	st := StateFor(deps)
	st.Cfg = cfg
	if cfg.Enabled() {
		id, err := LoadSwarmIdentity(context.Background(), deps.DB(), cfg.Secret)
		if err != nil {
			return err
		}
		st.SwarmID = id
		st.SwarmCli = NewSwarmClient(cfg.Secret)
		logging.Infof("evesynapse: shared cache enabled (%s)", cfg.Mode)
	}
	return nil
}
