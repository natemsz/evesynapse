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
	"net/http"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// swarmPeerRow is one swarm member as the pages show it. ID is
// the full instance ID (form posts); Short is the display
// prefix (narrow tables).
type swarmPeerRow struct {
	ID       string
	Short    string
	URL      string
	Source   string // seed, gossip, manual
	Approved bool
	Excluded bool
	State    string // Approved, Waiting, Excluded (+ auto note)
	Trust    string // trust percent
	Good     string
	Bad      string
}

// swarmMetricsView is the Sync page's "Shared cache" card.
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

// swarmAdminView is the Admin page's "Shared cache" section.
type swarmAdminView struct {
	Mode        string // Off, Open, Allowlist
	Enabled     bool
	SecretSet   bool
	Seeds       string
	GossipEvery string
	Peers       []swarmPeerRow
}

// shortenSwarmID shows the first characters of an instance ID:
// enough to tell members apart, short enough for a narrow
// table. IDs are random, so prefixes do not collide in
// practice; the full ID stays in the database.
func shortenSwarmID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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
			ID: p.InstanceID, Short: shortenSwarmID(p.InstanceID),
			URL: p.BaseUrl, Source: p.Source,
			Approved: p.Approved, Excluded: p.Excluded,
			Trust: percentOf(swarmTrustPercent(p.GoodChecks, p.BadChecks), 100),
			Good:  esi.FormatInt(p.GoodChecks), Bad: esi.FormatInt(p.BadChecks),
		}
		switch {
		case p.Excluded:
			row.State = "Excluded"
			if strings.HasPrefix(p.ExcludeNote, "auto:") {
				row.State = "Excluded (auto)"
			}
		case p.Approved:
			row.State = "Approved"
		default:
			row.State = "Waiting"
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

