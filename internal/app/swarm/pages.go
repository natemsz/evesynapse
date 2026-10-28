package swarm

// ---------------------------------------------------------------------------
// The swarm on the Sync and Admin pages. Sync shows what the
// shared cache's own checks did (spot checks for and against,
// entries dropped unchecked or taken back); Admin shows the
// swarm itself: mode, peers, trust, and the approve/exclude/admit
// actions, with the summary figures on its analytics tab. The
// secret never appears anywhere: the page says only whether one
// is set.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"evesynapse/internal/humanize"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// SwarmPeerRow is one swarm member as the pages show it. ID is
// the full instance ID (form posts); Display is what readers
// see: the real address for instances the operator added by
// hand or configured as seeds, and a stable alias
// (adjective.noun.number) for everyone learned through gossip
// or seeds — member addresses are not shown to casual readers.
type SwarmPeerRow struct {
	ID          string
	Display     string
	URL         string
	ShowAddress bool   // the operator added it or seeded it: the address shows
	Source      string // seed, gossip (including pushed registrations), manual
	Approved    bool
	Excluded    bool
	Role        string // Seed, Cache, Seed+Cache: what the peer functions as
	Build       string // rendered build claim: "Mainline · <release>", "Fork", "—" (build.go)
	State       string // Approved, Waiting, Excluded (+ auto note)
	KeyConflict string // "another key was refused on 2026-10-13", empty when none was
	Trust       string // trust percent
	Good        string
	Bad         string
}

// SwarmMetricsView is the Sync page's "Swarm Cache" card.
type SwarmMetricsView struct {
	Status  string // Off, or Open/Allowlist with peer counts
	Enabled bool
	Rows    []SwarmMetricRow
}

// SwarmMetricRow is one label/value line in the card.
type SwarmMetricRow struct {
	Label string
	Value string
}

// SwarmAdminView is the Admin page's "Swarm Cache" section.
type SwarmAdminView struct {
	Mode        string // Off, Open, Allowlist
	Enabled     bool
	SecretSet   bool
	Seeds       string
	GossipEvery string
	Peers       []SwarmPeerRow
}

// ShortenSwarmID shows the first characters of an instance ID:
// enough to tell members apart, short enough for typed
// confirmations and log lines. IDs are random, so prefixes do
// not collide in practice; the full ID stays in the database.
func ShortenSwarmID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// SwarmDisplayPeer names one peer for the pages: the real
// address when the operator added it by hand or configured it
// as a seed (evesynapse.app) — both already known to the
// operator — and the stable alias otherwise, so gossip-learned
// members never show their addresses to casual readers.
func (app *Mod) SwarmDisplayPeer(instanceID, baseURL, source string) string {
	if source == "manual" || app.SwarmIsSeed(baseURL) {
		return baseURL
	}
	return SwarmAlias(app.Cfg.Secret, instanceID)
}

// SwarmIsSeed reports whether a peer address is one of the
// configured seed origins (trailing slashes ignored).
func (app *Mod) SwarmIsSeed(baseURL string) bool {
	want := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if want == "" {
		return false
	}
	for _, seed := range app.Cfg.Seeds {
		if strings.TrimSuffix(strings.TrimSpace(seed), "/") == want {
			return true
		}
	}
	return false
}

// LoadSwarmPeers reads the swarm table into display rows,
// newest-heard first (the query's order).
func (app *Mod) LoadSwarmPeers(ctx context.Context) []SwarmPeerRow {
	rows, err := app.Queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: list peers for pages: %v", err)
		return nil
	}
	var out []SwarmPeerRow
	for _, p := range rows {
		row := SwarmPeerRow{
			ID: p.InstanceID, Display: app.SwarmDisplayPeer(p.InstanceID, p.BaseUrl, p.Source),
			URL: p.BaseUrl, Source: p.Source,
			Approved: p.Approved, Excluded: p.Excluded,
			Trust: percentOf(SwarmTrustPercent(p.GoodChecks, p.BadChecks), 100),
			Good:  esi.FormatInt(p.GoodChecks), Bad: esi.FormatInt(p.BadChecks),
		}
		row.ShowAddress = row.Display == p.BaseUrl
		// Role is what the peer functions as, as seen from
		// here. The wire carries no role flag and a seed is a
		// member like any other: an address configured as a
		// seed is a Seed until a pull from it has staged
		// something, and a Seed+Cache from then on.
		row.Role = "Cache"
		if app.SwarmIsSeed(p.BaseUrl) {
			row.Role = "Seed"
			if p.LastServed.Valid {
				row.Role = "Seed+Cache"
			}
		}
		// The build claim is classified here, at render, and
		// nowhere else: the class is never stored, and the
		// stored claim never feeds back into the trust fields
		// computed above.
		row.Build = BuildLabel(p.Build)
		switch {
		case p.Excluded:
			row.State = "Excluded"
			if strings.HasPrefix(p.ExcludeNote, "auto:") {
				row.State = "Excluded (auto)"
			}
		case app.SwarmIsSeed(p.BaseUrl):
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

// SwarmTrustPercent renders a trust score 0-100 for percentOf.
func SwarmTrustPercent(good, bad int64) int64 {
	return int64(SwarmTrust(good, bad)*100 + 0.5)
}

// SwarmCounterMap reads the Sync counters into a map.
func (app *Mod) SwarmCounterMap(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	rows, err := app.Queries.GetSwarmCounters(ctx)
	if err != nil {
		logging.Errorf("swarm: counters: %v", err)
		return out
	}
	for _, row := range rows {
		out[row.Name] = row.Value
	}
	return out
}

// LoadSwarmMetrics builds the Sync card. Rendered reads only,
// bounded (100 peers), no fetches. The rows are the cache's own
// verification outcomes; the swarm's summary figures (the peers
// and their trust, the pulls applied and failed, the bytes
// served) are the Admin analytics page's, and are not restated
// here.
func (app *Mod) LoadSwarmMetrics(ctx context.Context) *SwarmMetricsView {
	view := &SwarmMetricsView{}
	if !app.Cfg.Enabled() {
		view.Status = "Off — this instance fetches everything itself (SHARED_CACHE)."
		return view
	}
	view.Enabled = true
	peers := app.LoadSwarmPeers(ctx)
	approved, excluded := 0, 0
	for _, p := range peers {
		if p.Excluded {
			excluded++
		} else if p.Approved {
			approved++
		}
	}
	mode := "Open"
	if app.Cfg.Allowlisted() {
		mode = "Allowlist"
	}
	view.Status = mode + " — " + humanize.Plural(approved, "peer") + " sharing, " + humanize.Plural(excluded, "peer") + " excluded"
	counters := app.SwarmCounterMap(ctx)
	view.Rows = []SwarmMetricRow{
		{Label: "Spot checks passed", Value: esi.FormatInt(counters[SwarmCounterSpotPassed])},
		{Label: "Spot checks failed (discarded, signer flagged)", Value: esi.FormatInt(counters[SwarmCounterSpotFailed])},
		{Label: "Entries dropped unchecked (expired or undecidable; nobody blamed)", Value: esi.FormatInt(counters[SwarmCounterDroppedUnchecked])},
		{Label: "Entries taken back (signer excluded)", Value: esi.FormatInt(counters[SwarmCounterRolledBack])},
	}
	return view
}

// LoadSwarmAdmin builds the Admin section: the settings (from
// the environment — changing them restarts the instance) and
// the swarm itself with its actions.
func (app *Mod) LoadSwarmAdmin(ctx context.Context) *SwarmAdminView {
	view := &SwarmAdminView{SecretSet: app.Cfg.Secret != ""}
	switch {
	case !app.Cfg.Enabled():
		view.Mode = "Off"
	case app.Cfg.Allowlisted():
		view.Mode = "Allowlist"
	default:
		view.Mode = "Open"
	}
	view.Enabled = app.Cfg.Enabled()
	view.Seeds = strings.Join(app.Cfg.Seeds, ", ")
	view.GossipEvery = "every " + strconv.Itoa(app.Cfg.GossipHours) + " hours"
	view.Peers = app.LoadSwarmPeers(ctx)
	return view
}

// HandleSwarmApprove approves one gossip-learned peer (POST
// /admin/swarm/approve). In open mode everything the secret
// lets in is already approved; this is the allowlist's door.
func (app *Mod) HandleSwarmApprove(w http.ResponseWriter, r *http.Request) {
	back := app.Deps.FlashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	if id == "" {
		back("Nothing was approved: no instance was named.")
		return
	}
	peer, err := app.Queries.GetSwarmPeer(r.Context(), id)
	if err != nil {
		back("Nothing was approved: that instance is not known.")
		return
	}
	if peer.Excluded {
		back("Nothing was approved: that instance is excluded. Re-admit it first.")
		return
	}
	if err := app.Queries.SetSwarmPeerApproved(r.Context(), db.SetSwarmPeerApprovedParams{InstanceID: id, Approved: true}); err != nil {
		logging.Errorf("admin: swarm approve %s: %v", id, err)
		back("The instance could not be approved; check the server log.")
		return
	}
	logging.Warnf("admin: account %d approved swarm instance %s", app.Deps.UserID(r.Context()), ShortenSwarmID(id))
	back("Instance " + ShortenSwarmID(id) + " was approved: its entries may now be pulled and checked.")
}

// HandleSwarmExclude excludes one peer by hand (POST
// /admin/swarm/exclude). Excluded peers are never contacted,
// never pulled from, and their staged entries are dropped. The
// reputation ledger stays, so re-admitting keeps the history.
func (app *Mod) HandleSwarmExclude(w http.ResponseWriter, r *http.Request) {
	back := app.Deps.FlashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	if id == "" {
		back("Nothing was excluded: no instance was named.")
		return
	}
	if _, err := app.Queries.GetSwarmPeer(r.Context(), id); err != nil {
		back("Nothing was excluded: that instance is not known.")
		return
	}
	note := "manual: excluded by the operator"
	if err := app.Queries.SetSwarmPeerExcluded(r.Context(), db.SetSwarmPeerExcludedParams{InstanceID: id, Excluded: true, ExcludeNote: note}); err != nil {
		logging.Errorf("admin: swarm exclude %s: %v", id, err)
		back("The instance could not be excluded; check the server log.")
		return
	}
	logging.Warnf("admin: account %d excluded swarm instance %s", app.Deps.UserID(r.Context()), ShortenSwarmID(id))
	back("Instance " + ShortenSwarmID(id) + " was excluded: nothing more is read from it.")
}

// HandleSwarmAdmit re-admits one excluded peer (POST
// /admin/swarm/admit). Trust starts where the ledger left it —
// admitting does not forgive, it only re-opens the door.
func (app *Mod) HandleSwarmAdmit(w http.ResponseWriter, r *http.Request) {
	back := app.Deps.FlashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	if id == "" {
		back("Nothing was re-admitted: no instance was named.")
		return
	}
	if _, err := app.Queries.GetSwarmPeer(r.Context(), id); err != nil {
		back("Nothing was re-admitted: that instance is not known.")
		return
	}
	if err := app.Queries.SetSwarmPeerExcluded(r.Context(), db.SetSwarmPeerExcludedParams{InstanceID: id, Excluded: false, ExcludeNote: ""}); err != nil {
		logging.Errorf("admin: swarm admit %s: %v", id, err)
		back("The instance could not be re-admitted; check the server log.")
		return
	}
	logging.Warnf("admin: account %d re-admitted swarm instance %s", app.Deps.UserID(r.Context()), ShortenSwarmID(id))
	back("Instance " + ShortenSwarmID(id) + " was re-admitted with its trust history intact.")
}

// HandleSwarmAddPeer learns one instance by address (POST
// /admin/swarm/add): the operator types a bare origin
// (https://evesynapse.app), and the instance asks it for its
// membership list — the same round as gossip, on demand. The
// address joins under trust on first use like any seed; in
// allowlist mode it still waits for approval. Refused while
// sharing is off, and refused for anything but an https origin
// (http on loopback, for local testing).
func (app *Mod) HandleSwarmAddPeer(w http.ResponseWriter, r *http.Request) {
	back := app.Deps.FlashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	if !app.Cfg.Enabled() || app.SwarmID == nil || app.SwarmCli == nil {
		back("Nothing was added: the swarm cache is off. Set SHARED_CACHE and restart first.")
		return
	}
	raw := strings.TrimSpace(r.Form.Get("url"))
	if !ValidSwarmBaseURL(raw) {
		back("Nothing was added: that is not an https address (http only on loopback).")
		return
	}
	switch err := app.SwarmContactPeer(r.Context(), strings.TrimSuffix(raw, "/"), "", "", "manual", time.Now().UTC()); {
	case err == nil:
		logging.Warnf("admin: account %d added swarm address %s", app.Deps.UserID(r.Context()), raw)
		back("That address answered: its instances are listed below" + ApprovalNote(app) + ".")
	case errors.Is(err, ErrSwarmKeyChanged):
		back("Nothing was added: that address answered as an instance already listed under a different key. Remove that instance first if its key really changed.")
	default:
		back("Nothing was added: that address did not answer as a swarm member.")
	}
}

// ApprovalNote reminds where newly learned instances wait in
// allowlist mode.
func ApprovalNote(app *Mod) string {
	if app.Cfg.Allowlisted() {
		return ", waiting for approval"
	}
	return ""
}

// HandleSwarmRemove forgets one peer entirely (POST
// /admin/swarm/remove): address, pinned key, approval,
// exclusion, and the ledger. Removing is how a peer's key is
// rotated — the next gossip pins the new one from scratch —
// and how dead instances leave the list. Like account removal
// it asks twice: the instance in the form, and typed out by
// hand. Never the instance itself.
func (app *Mod) HandleSwarmRemove(w http.ResponseWriter, r *http.Request) {
	back := app.Deps.FlashBack(w, r, "/admin/")
	if r.ParseForm() != nil {
		back("That request could not be read.")
		return
	}
	id := strings.TrimSpace(r.Form.Get("instance"))
	// The confirmation names what the page shows (the alias, or
	// the address for hand-added and seed instances).
	peer, err := app.Queries.GetSwarmPeer(r.Context(), id)
	if err != nil || id == "" {
		back("Nothing was removed: that instance is not known.")
		return
	}
	if strings.TrimSpace(r.Form.Get("confirm")) != app.SwarmDisplayPeer(peer.InstanceID, peer.BaseUrl, peer.Source) {
		back("Nothing was removed: type the instance's shown name to confirm.")
		return
	}
	if app.SwarmID != nil && id == app.SwarmID.ID {
		back("Nothing was removed: that is this instance.")
		return
	}
	if err := app.Queries.DeleteSwarmPeer(r.Context(), id); err != nil {
		logging.Errorf("admin: swarm remove %s: %v", id, err)
		back("The instance could not be removed; check the server log.")
		return
	}
	logging.Warnf("admin: account %d removed swarm instance %s", app.Deps.UserID(r.Context()), ShortenSwarmID(id))
	back("Instance " + ShortenSwarmID(id) + " was removed, with its key and trust history.")
}
