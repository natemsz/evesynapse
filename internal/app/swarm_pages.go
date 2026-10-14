package app

// ---------------------------------------------------------------------------
// The swarm on the Sync and Admin pages. Sync shows what sharing
// costs and earns (entries applied, ESI calls saved, spot checks
// for and against); Admin shows the swarm itself — mode, peers,
// trust, and the approve/exclude/admit actions. The secret never
// appears anywhere: the page says only whether one is set.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// swarmPeerRow is one swarm member as the pages show it. ID is
// the full instance ID (form posts); Display is what readers
// see: the real address for instances the operator added by
// hand or configured as seeds, and a stable alias
// (adjective.noun.number) for everyone learned through gossip
// or seeds — member addresses are not shown to casual readers.
type swarmPeerRow struct {
	ID          string
	Display     string
	URL         string
	ShowAddress bool   // the operator added it or seeded it: the address shows
	Source      string // seed, gossip (including pushed registrations), manual
	Approved    bool
	Excluded    bool
	Role        string // Seed, Cache, Seed+Cache: what the peer functions as
	State       string // Approved, Waiting, Excluded (+ auto note)
	KeyConflict string // "another key was refused on 2026-10-13", empty when none was
	Trust       string // trust percent
	Good        string
	Bad         string
}

// swarmMetricsView is the Sync page's "Swarm Cache" card.
type swarmMetricsView struct {
	Status  string // Off, or Open/Allowlist with peer counts
	Enabled bool
	Rows    []swarmMetricRow
	Peers   []swarmPeerRow
}

// swarmMetricRow is one label/value line in the card.
type swarmMetricRow struct {
	Label string
	Value string
}

// swarmAdminView is the Admin page's "Swarm Cache" section.
type swarmAdminView struct {
	Mode        string // Off, Open, Allowlist
	Enabled     bool
	SecretSet   bool
	Seeds       string
	GossipEvery string
	Peers       []swarmPeerRow
}

// shortenSwarmID shows the first characters of an instance ID:
// enough to tell members apart, short enough for typed
// confirmations and log lines. IDs are random, so prefixes do
// not collide in practice; the full ID stays in the database.
func shortenSwarmID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// swarmDisplayPeer names one peer for the pages: the real
// address when the operator added it by hand or configured it
// as a seed (evesynapse.app) — both already known to the
// operator — and the stable alias otherwise, so gossip-learned
// members never show their addresses to casual readers.
func (app *Application) swarmDisplayPeer(instanceID, baseURL, source string) string {
	if source == "manual" || app.swarmIsSeed(baseURL) {
		return baseURL
	}
	return swarmAlias(app.cfg.swarm.secret, instanceID)
}

// swarmIsSeed reports whether a peer address is one of the
// configured seed origins (trailing slashes ignored).
func (app *Application) swarmIsSeed(baseURL string) bool {
	want := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if want == "" {
		return false
	}
	for _, seed := range app.cfg.swarm.seeds {
		if strings.TrimSuffix(strings.TrimSpace(seed), "/") == want {
			return true
		}
	}
	return false
}

// loadSwarmPeers reads the swarm table into display rows,
// newest-heard first (the query's order).
func (app *Application) loadSwarmPeers(ctx context.Context) []swarmPeerRow {
	rows, err := app.queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: list peers for pages: %v", err)
		return nil
	}
	var out []swarmPeerRow
	for _, p := range rows {
		row := swarmPeerRow{
			ID: p.InstanceID, Display: app.swarmDisplayPeer(p.InstanceID, p.BaseUrl, p.Source),
			URL: p.BaseUrl, Source: p.Source,
			Approved: p.Approved, Excluded: p.Excluded,
			Trust: percentOf(swarmTrustPercent(p.GoodChecks, p.BadChecks), 100),
			Good:  esi.FormatInt(p.GoodChecks), Bad: esi.FormatInt(p.BadChecks),
		}
		row.ShowAddress = row.Display == p.BaseUrl
		// Role is what the peer functions as. The wire carries
		// no role flag: the seed worker's fixed identity is its
		// report — it signs as "seed" and holds no data — while
		// anything serving under its own identity is a cache,
		// even when it also answers at a directory address.
		row.Role = "Cache"
		switch {
		case p.InstanceID == swarmSeedInstanceID:
			row.Role = "Seed"
		case app.swarmIsSeed(p.BaseUrl):
			row.Role = "Seed+Cache"
		}
		switch {
		case p.Excluded:
			row.State = "Excluded"
			if strings.HasPrefix(p.ExcludeNote, "auto:") {
				row.State = "Excluded (auto)"
			}
		case app.swarmIsSeed(p.BaseUrl):
			// A configured seed URL is a directory: approval is
			// meaningless and the state says what it is. Pulls
			// are decided by identity, not address, so a
			// directory that also serves stays pullable.
			row.State = "Seed"
		case p.Approved:
			row.State = "Approved"
		default:
			row.State = "Waiting"
		}
		if p.KeyConflictAt.Valid {
			row.KeyConflict = "another key was refused on " + p.KeyConflictAt.Time.UTC().Format("2006-01-02")
		}
		out = append(out, row)
	}
	return out
}

// swarmTrustPercent renders a trust score 0-100 for percentOf.
func swarmTrustPercent(good, bad int64) int64 {
	return int64(swarmTrust(good, bad)*100 + 0.5)
}

// swarmCounterMap reads the Sync counters into a map.
func (app *Application) swarmCounterMap(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	rows, err := app.queries.GetSwarmCounters(ctx)
	if err != nil {
		logging.Errorf("swarm: counters: %v", err)
		return out
	}
	for _, row := range rows {
		out[row.Name] = row.Value
	}
	return out
}

// loadSwarmMetrics builds the Sync card. Rendered reads only,
// bounded (100 peers), no fetches.
func (app *Application) loadSwarmMetrics(ctx context.Context) *swarmMetricsView {
	view := &swarmMetricsView{}
	if !app.cfg.swarm.enabled() {
		view.Status = "Off — this instance fetches everything itself (SHARED_CACHE)."
		return view
	}
	view.Enabled = true
	peers := app.loadSwarmPeers(ctx)
	view.Peers = peers
	approved, excluded := 0, 0
	for _, p := range peers {
		if p.Excluded {
			excluded++
		} else if p.Approved {
			approved++
		}
	}
	mode := "Open"
	if app.cfg.swarm.allowlisted() {
		mode = "Allowlist"
	}
	view.Status = mode + " — " + pluralPeer(approved) + " sharing, " + pluralPeer(excluded) + " excluded"
	counters := app.swarmCounterMap(ctx)
	applied := counters[swarmCounterAppliedHistory] + counters[swarmCounterAppliedNames] + counters[swarmCounterAppliedRecords]
	// Every applied entry is one ESI fetch this instance did not
	// make: the worker settles the fetch state on apply, so the
	// datum is never fetched twice.
	view.Rows = []swarmMetricRow{
		{Label: "Entries applied from peers", Value: esi.FormatInt(applied)},
		{Label: "ESI calls saved", Value: esi.FormatInt(applied)},
		{Label: "Spot checks passed", Value: esi.FormatInt(counters[swarmCounterSpotPassed])},
		{Label: "Spot checks failed (discarded, signer flagged)", Value: esi.FormatInt(counters[swarmCounterSpotFailed])},
		{Label: "Entries taken back (signer excluded)", Value: esi.FormatInt(counters[swarmCounterRolledBack])},
		{Label: "Peer pulls succeeded", Value: esi.FormatInt(counters[swarmCounterPullsOK])},
		{Label: "Peer pulls failed", Value: esi.FormatInt(counters[swarmCounterPullsFailed])},
	}
	return view
}

// pluralPeer counts peers in words.
func pluralPeer(n int) string {
	if n == 1 {
		return "1 peer"
	}
	return strconv.Itoa(n) + " peers"
}

// loadSwarmAdmin builds the Admin section: the settings (from
// the environment — changing them restarts the instance) and
// the swarm itself with its actions.
func (app *Application) loadSwarmAdmin(ctx context.Context) *swarmAdminView {
	view := &swarmAdminView{SecretSet: app.cfg.swarm.secret != ""}
	switch {
	case !app.cfg.swarm.enabled():
		view.Mode = "Off"
	case app.cfg.swarm.allowlisted():
		view.Mode = "Allowlist"
	default:
		view.Mode = "Open"
	}
	view.Enabled = app.cfg.swarm.enabled()
	view.Seeds = strings.Join(app.cfg.swarm.seeds, ", ")
	view.GossipEvery = "every " + strconv.Itoa(app.cfg.swarm.gossipHours) + " hours"
	view.Peers = app.loadSwarmPeers(ctx)
	return view
}

// handleSwarmApprove approves one gossip-learned peer (POST
// /admin/swarm/approve). In open mode everything the secret
// lets in is already approved; this is the allowlist's door.
func (app *Application) handleSwarmApprove(w http.ResponseWriter, r *http.Request) {
	back := app.flashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	if id == "" {
		back("Nothing was approved: no instance was named.")
		return
	}
	peer, err := app.queries.GetSwarmPeer(r.Context(), id)
	if err != nil {
		back("Nothing was approved: that instance is not known.")
		return
	}
	if peer.Excluded {
		back("Nothing was approved: that instance is excluded. Re-admit it first.")
		return
	}
	if err := app.queries.SetSwarmPeerApproved(r.Context(), db.SetSwarmPeerApprovedParams{InstanceID: id, Approved: true}); err != nil {
		logging.Errorf("admin: swarm approve %s: %v", id, err)
		back("The instance could not be approved; check the server log.")
		return
	}
	logging.Warnf("admin: account %d approved swarm instance %s", app.userID(r.Context()), shortenSwarmID(id))
	back("Instance " + shortenSwarmID(id) + " was approved: its entries may now be pulled and checked.")
}

// handleSwarmExclude excludes one peer by hand (POST
// /admin/swarm/exclude). Excluded peers are never contacted,
// never pulled from, and their staged entries are dropped. The
// reputation ledger stays, so re-admitting keeps the history.
func (app *Application) handleSwarmExclude(w http.ResponseWriter, r *http.Request) {
	back := app.flashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	if id == "" {
		back("Nothing was excluded: no instance was named.")
		return
	}
	if _, err := app.queries.GetSwarmPeer(r.Context(), id); err != nil {
		back("Nothing was excluded: that instance is not known.")
		return
	}
	note := "manual: excluded by the operator"
	if err := app.queries.SetSwarmPeerExcluded(r.Context(), db.SetSwarmPeerExcludedParams{InstanceID: id, Excluded: true, ExcludeNote: note}); err != nil {
		logging.Errorf("admin: swarm exclude %s: %v", id, err)
		back("The instance could not be excluded; check the server log.")
		return
	}
	logging.Warnf("admin: account %d excluded swarm instance %s", app.userID(r.Context()), shortenSwarmID(id))
	back("Instance " + shortenSwarmID(id) + " was excluded: nothing more is read from it.")
}

// handleSwarmAdmit re-admits one excluded peer (POST
// /admin/swarm/admit). Trust starts where the ledger left it —
// admitting does not forgive, it only re-opens the door.
func (app *Application) handleSwarmAdmit(w http.ResponseWriter, r *http.Request) {
	back := app.flashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	if id == "" {
		back("Nothing was re-admitted: no instance was named.")
		return
	}
	if _, err := app.queries.GetSwarmPeer(r.Context(), id); err != nil {
		back("Nothing was re-admitted: that instance is not known.")
		return
	}
	if err := app.queries.SetSwarmPeerExcluded(r.Context(), db.SetSwarmPeerExcludedParams{InstanceID: id, Excluded: false, ExcludeNote: ""}); err != nil {
		logging.Errorf("admin: swarm admit %s: %v", id, err)
		back("The instance could not be re-admitted; check the server log.")
		return
	}
	logging.Warnf("admin: account %d re-admitted swarm instance %s", app.userID(r.Context()), shortenSwarmID(id))
	back("Instance " + shortenSwarmID(id) + " was re-admitted with its trust history intact.")
}

// handleSwarmAddPeer learns one instance by address (POST
// /admin/swarm/add): the operator types a bare origin
// (https://evesynapse.app), and the instance asks it for its
// membership list — the same round as gossip, on demand. The
// address joins under trust on first use like any seed; in
// allowlist mode it still waits for approval. Refused while
// sharing is off, and refused for anything but an https origin
// (http on loopback, for local testing).
func (app *Application) handleSwarmAddPeer(w http.ResponseWriter, r *http.Request) {
	back := app.flashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	if !app.cfg.swarm.enabled() || app.swarmID == nil || app.swarmCli == nil {
		back("Nothing was added: the swarm cache is off. Set SHARED_CACHE and restart first.")
		return
	}
	raw := strings.TrimSpace(r.Form.Get("url"))
	if !validSwarmBaseURL(raw) {
		back("Nothing was added: that is not an https address (http only on loopback).")
		return
	}
	switch err := app.swarmContactPeer(r.Context(), strings.TrimSuffix(raw, "/"), "", "", "manual", time.Now().UTC()); {
	case err == nil:
		logging.Warnf("admin: account %d added swarm address %s", app.userID(r.Context()), raw)
		back("That address answered: its instances are listed below" + approvalNote(app) + ".")
	case errors.Is(err, errSwarmKeyChanged):
		back("Nothing was added: that address answered as an instance already listed under a different key. Remove that instance first if its key really changed.")
	default:
		back("Nothing was added: that address did not answer as a swarm member.")
	}
}

// approvalNote reminds where newly learned instances wait in
// allowlist mode.
func approvalNote(app *Application) string {
	if app.cfg.swarm.allowlisted() {
		return ", waiting for approval"
	}
	return ""
}

// handleSwarmRemove forgets one peer entirely (POST
// /admin/swarm/remove): address, pinned key, approval,
// exclusion, and the ledger. Removing is how a peer's key is
// rotated — the next gossip pins the new one from scratch —
// and how dead instances leave the list. Like account removal
// it asks twice: the instance in the form, and typed out by
// hand. Never the instance itself.
func (app *Application) handleSwarmRemove(w http.ResponseWriter, r *http.Request) {
	back := app.flashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	// The confirmation names what the page shows (the alias, or
	// the address for hand-added and seed instances).
	peer, err := app.queries.GetSwarmPeer(r.Context(), id)
	if err != nil || id == "" {
		back("Nothing was removed: that instance is not known.")
		return
	}
	if strings.TrimSpace(r.Form.Get("confirm")) != app.swarmDisplayPeer(peer.InstanceID, peer.BaseUrl, peer.Source) {
		back("Nothing was removed: type the instance's shown name to confirm.")
		return
	}
	if app.swarmID != nil && id == app.swarmID.id {
		back("Nothing was removed: that is this instance.")
		return
	}
	if err := app.queries.DeleteSwarmPeer(r.Context(), id); err != nil {
		logging.Errorf("admin: swarm remove %s: %v", id, err)
		back("The instance could not be removed; check the server log.")
		return
	}
	logging.Warnf("admin: account %d removed swarm instance %s", app.userID(r.Context()), shortenSwarmID(id))
	back("Instance " + shortenSwarmID(id) + " was removed, with its key and trust history.")
}
