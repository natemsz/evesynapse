package swarm

// ---------------------------------------------------------------------------
// Seed announcements: how the mesh tells the Cloudflare seed
// worker (swarm-seed/) who is listening where. Discovery needs
// no new code path — seeds are dialed like peers — but the
// worker cannot discover anyone by itself, so every gossip
// round ends with one heartbeat per seed: this instance's
// self-signed announce entry (kind "announce", key and signer
// the instance id, payload its base URL and public key). The
// worker verifies the signature against the claimed key, the
// same trust-on-first-use as mesh gossip, and files the record
// with a 48h TTL.
//
// Instances with no public address configured stay silent: a
// URL nobody can dial would only pollute the directory.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// SeedAnnounceKind is the entry kind a heartbeat carries. It
// never enters staging: the seed endpoint is its only reader.
const SeedAnnounceKind = "announce"

// SeedAnnouncePayload is what the entry seals: where to dial,
// and the key the signature already proved.
type SeedAnnouncePayload struct {
	BaseURL   string `json:"base_url"`
	PublicKey string `json:"public_key"`
	// URLSig is the sender's member signature over its ID,
	// BaseURL and IssuedAt: with the fields beside it, it is the
	// tuple the receiver stores and relays verbatim.
	URLSig   string `json:"url_sig"`
	IssuedAt int64  `json:"issued_at"`
}

// BuildSeedAnnounce signs this instance's heartbeat. False when
// there is nothing worth announcing (sharing off, or no public
// address to be dialed at).
func (app *Mod) BuildSeedAnnounce(now time.Time) (SwarmSignedEntry, bool) {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		return SwarmSignedEntry{}, false
	}
	origin := app.Deps.PublicOrigin()
	if origin == "" {
		return SwarmSignedEntry{}, false
	}
	if !strings.HasPrefix(origin, "https://") {
		// Loopback and plain-http origins are undialable: the
		// seed would refuse the record, so do not send it.
		return SwarmSignedEntry{}, false
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(SeedAnnouncePayload{
		BaseURL:   origin,
		PublicKey: app.SwarmID.PublicString(),
		URLSig:    SignSwarmMember(app.SwarmID, origin, now.Unix()),
		IssuedAt:  now.Unix(),
	}); err != nil {
		return SwarmSignedEntry{}, false
	}
	entry, err := SignSwarmEntry(app.SwarmID, app.SwarmID.ID, SeedAnnounceKind, app.SwarmID.ID, strings.TrimSpace(buf.String()), now)
	if err != nil {
		logging.Errorf("swarm: sign seed announce: %v", err)
		return SwarmSignedEntry{}, false
	}
	return entry, true
}

// SendSeedAnnounce POSTs one heartbeat and reports whether the
// seed kept it, through the one swarm request helper (Call).
func (c *SwarmClient) SendSeedAnnounce(ctx context.Context, seedURL string, entry SwarmSignedEntry) error {
	var body struct {
		OK bool `json:"ok"`
	}
	if err := c.Call(ctx, http.MethodPost, strings.TrimSuffix(seedURL, "/")+"/swarm/seed/register", map[string]any{"entry": entry}, &body); err != nil {
		return err
	}
	if !body.OK {
		return fmt.Errorf("swarm: announce %s: seed refused the heartbeat", seedURL)
	}
	return nil
}

// SwarmAnnounceToSeeds ends the membership round with one
// heartbeat per configured seed: this instance's self-signed
// announce entry, the same one the seed worker files with a
// 48h TTL. Best-effort by design: a refused heartbeat only
// logs, never fails the round; and silent when there is
// nothing to announce (no seeds, or no dialable origin).
func (app *Mod) SwarmAnnounceToSeeds(ctx context.Context, now time.Time) {
	entry, ok := app.BuildSeedAnnounce(now)
	if !ok {
		return
	}
	seeds := app.Cfg.Seeds
	errs := make([]error, len(seeds))
	SwarmFanOut(ctx, len(seeds), func(rctx context.Context, i int) {
		errs[i] = app.SwarmCli.SendSeedAnnounce(rctx, seeds[i], entry)
	})
	kept := 0
	for i, err := range errs {
		if err != nil {
			logging.Warnf("swarm: announce %s: %v", seeds[i], err)
			continue
		}
		kept++
	}
	if kept > 0 {
		logging.Infof("swarm: announced to %d seed(s)", kept)
	}
}
