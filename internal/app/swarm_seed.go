package app

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
	"io"
	"net/http"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// seedAnnounceKind is the entry kind a heartbeat carries. It
// never enters staging: the seed endpoint is its only reader.
const seedAnnounceKind = "announce"

// seedAnnouncePayload is what the entry seals: where to dial,
// and the key the signature already proved.
type seedAnnouncePayload struct {
	BaseURL   string `json:"base_url"`
	PublicKey string `json:"public_key"`
	// URLSig is the sender's member signature over its ID,
	// BaseURL and IssuedAt: with the fields beside it, it is the
	// tuple the receiver stores and relays verbatim.
	URLSig   string `json:"url_sig"`
	IssuedAt int64  `json:"issued_at"`
}

// buildSeedAnnounce signs this instance's heartbeat. False when
// there is nothing worth announcing (sharing off, or no public
// address to be dialed at).
func (app *Application) buildSeedAnnounce(now time.Time) (swarmSignedEntry, bool) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		return swarmSignedEntry{}, false
	}
	origin := app.cfg.publicOrigin()
	if origin == "" {
		return swarmSignedEntry{}, false
	}
	if !strings.HasPrefix(origin, "https://") {
		// Loopback and plain-http origins are undialable: the
		// seed would refuse the record, so do not send it.
		return swarmSignedEntry{}, false
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(seedAnnouncePayload{
		BaseURL:   origin,
		PublicKey: app.swarmID.publicString(),
		URLSig:    signSwarmMember(app.swarmID, origin, now.Unix()),
		IssuedAt:  now.Unix(),
	}); err != nil {
		return swarmSignedEntry{}, false
	}
	entry, err := signSwarmEntry(app.swarmID, app.swarmID.id, seedAnnounceKind, app.swarmID.id, strings.TrimSpace(buf.String()), now)
	if err != nil {
		logging.Errorf("swarm: sign seed announce: %v", err)
		return swarmSignedEntry{}, false
	}
	return entry, true
}

// sendSeedAnnounce POSTs one heartbeat and reports whether the
// seed kept it. Strict-decode, size-capped, like every swarm
// call; the secret travels in the bearer header only.
func (c *swarmClient) sendSeedAnnounce(ctx context.Context, seedURL string, entry swarmSignedEntry) error {
	if !validSwarmBaseURL(seedURL) {
		return fmt.Errorf("swarm: refusing to announce to %q (https, or http on loopback, only)", seedURL)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]any{"entry": entry}); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(seedURL, "/")+"/swarm/seed/register", &buf)
	if err != nil {
		return fmt.Errorf("swarm: announce %s: %w", seedURL, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("swarm: announce %s: %w", seedURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("swarm: announce %s: status %d", seedURL, resp.StatusCode)
	}
	var body struct {
		OK bool `json:"ok"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxSwarmBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return fmt.Errorf("swarm: announce %s: %w", seedURL, err)
	}
	if !body.OK {
		return fmt.Errorf("swarm: announce %s: seed refused the heartbeat", seedURL)
	}
	return nil
}

// swarmAnnounceToSeeds ends the membership round with one
// heartbeat per configured seed: this instance's self-signed
// announce entry, the same one the seed worker files with a
// 48h TTL. Best-effort by design: a refused heartbeat only
// logs, never fails the round; and silent when there is
// nothing to announce (no seeds, or no dialable origin).
func (app *Application) swarmAnnounceToSeeds(ctx context.Context, now time.Time) {
	entry, ok := app.buildSeedAnnounce(now)
	if !ok {
		return
	}
	kept := 0
	for _, seed := range app.cfg.swarm.seeds {
		if err := app.swarmCli.sendSeedAnnounce(ctx, seed, entry); err != nil {
			logging.Warnf("swarm: announce %s: %v", seed, err)
			continue
		}
		kept++
	}
	if kept > 0 {
		logging.Infof("swarm: announced to %d seed(s)", kept)
	}
}
