package app

// ---------------------------------------------------------------------------
// The swarm's HTTP API: how instances talk to each other. Three
// endpoints, all bearer-gated by the shared secret, all dark
// (404) while the shared cache is off so a disabled instance
// offers no surface to probe.
//
//   GET  /swarm/members: this instance's signed membership list
//   POST /swarm/pull: signed public-data entries for wanted keys
//   POST /swarm/seed/register: a peer's self-signed heartbeat (see below)
//
// Data moves pull-only: instances fetch from peers they trust;
// no instance can push data (or wants, or queue hints) into
// another. The one push is membership, not data: a heartbeat
// carries only the sender's own URL and public key, self-signed,
// and files through the same trust-on-first-use path as a
// gossip sighting, approval mode and all. That is what makes
// every instance a usable seed: register here and this
// instance's member list teaches you the swarm, the same as
// the Cloudflare seed worker's does. Membership spreads peer
// to peer (gossip); reputation never does: every instance
// scores signers from its own spot checks, so a liar cannot
// talk others into distrusting someone.
//
// Transport security is the standard library throughout: the
// server side is the app's own TLS terminator (or its loopback
// listener), the client verifies certificates by default, and
// the bearer check compares SHA-256 hashes in constant time.
// The client refuses cleartext to anything but a loopback URL
// before a request is built, and at the point of connection it
// refuses any address that is not public and any redirect off
// the host it asked (swarm_dial.go), so a gossip-learned URL
// can neither downgrade the secret onto the wire nor aim a
// request inward. No handrolled crypto, no custom framing:
// JSON over HTTPS, strict-decoded, size-capped.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Bounds on the swarm API: one pull names at most this many keys,
// and no request or response body past this size is read.
const (
	maxSwarmPullKeys  = 50
	maxSwarmBodyBytes = 1 << 20
)

// swarmHTTPTimeout bounds one peer round trip. Peers are few and
// slow gossip is fine; a hung peer must not hold a worker pass.
const swarmHTTPTimeout = 15 * time.Second

// swarmMember is one instance as the membership list carries it:
// identity material only, never reputation judgments.
type swarmMember struct {
	InstanceID string `json:"instance_id"`
	BaseURL    string `json:"base_url"`
	PublicKey  string `json:"public_key"`
}

// swarmAuth checks the request's bearer token against the shared
// secret. Hashes are compared (not the secret itself) so a
// length difference leaks nothing, in constant time so a prefix
// match learns nothing. False on any failure, with no reason.
func swarmAuth(secret string, r *http.Request) bool {
	if secret == "" {
		return false
	}
	present, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || present == "" {
		return false
	}
	want := sha256.Sum256([]byte(secret))
	got := sha256.Sum256([]byte(strings.TrimSpace(present)))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// handleSwarmMembers serves GET /swarm/members: the signed
// membership list — this instance plus every non-excluded peer
// it knows, so the caller learns the swarm one hop at a time.
func (app *Application) handleSwarmMembers(w http.ResponseWriter, r *http.Request) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !swarmAuth(app.cfg.swarm.secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	// Self first, always: the key is the attestation (it
	// verifies the list), even when this instance has no
	// public address configured to advertise beside it.
	// Callers file the signer under the URL that answered.
	members := []swarmMember{{
		InstanceID: app.swarmID.id, BaseURL: app.cfg.publicOrigin(),
		PublicKey: app.swarmID.publicString(),
	}}
	peers, err := app.queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: list peers: %v", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	for _, p := range peers {
		if p.Excluded {
			continue
		}
		members = append(members, swarmMember{InstanceID: p.InstanceID, BaseURL: p.BaseUrl, PublicKey: p.PublicKey})
	}
	raw, err := json.Marshal(members)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	// Sealed under the shared secret before signing: possession
	// of the secret decides who may fetch; the seal decides who
	// may read. A response that leaks anywhere else stays opaque.
	sealed, err := sealSwarmMembers(raw, app.cfg.swarm.secret)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	entry, err := signSwarmEntry(app.swarmID, app.swarmID.id, swarmKindMembers, app.swarmID.id, sealed, time.Now())
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	writeSwarmJSON(w, map[string]any{"entry": entry})
}

// handleSwarmSeedRegister serves POST /swarm/seed/register: a
// peer's self-signed heartbeat, the same entry a seed worker
// files. The body holds only the sender's own URL and public
// key; the signature must verify under the payload's key with
// key and signer all naming the sender, which is the whole
// trust check: the shared secret already proved membership,
// the key proves possession from here on. What verifies files
// through the gossip path (key pinning, approval mode,
// exclusion untouched), so a registering peer lands exactly
// where a gossip sighting would put it: a first heartbeat
// pins its key, a later one under that key may move its
// address, and one under any other key is a conflict (409),
// refused and flagged. Anything else is a bad request; a
// heartbeat from this instance itself is kept as a no-op
// answer, not a peer row.
func (app *Application) handleSwarmSeedRegister(w http.ResponseWriter, r *http.Request) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !swarmAuth(app.cfg.swarm.secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Entry swarmSignedEntry `json:"entry"`
	}
	if err := readSwarmJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	entry := body.Entry
	if entry.Kind != seedAnnounceKind || entry.Signer == "" || entry.Key != entry.Signer {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var payload seedAnnouncePayload
	dec := json.NewDecoder(strings.NewReader(entry.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validSwarmBaseURL(payload.BaseURL) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(payload.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !verifySwarmEntry(entry, payload.PublicKey) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	now := time.Now()
	if entry.Signer != app.swarmID.id {
		members := []swarmMember{{
			InstanceID: entry.Signer, BaseURL: payload.BaseURL, PublicKey: payload.PublicKey,
		}}
		if err := app.swarmLearnMembers(ctx, members, entry.Signer, payload.PublicKey, "", "gossip", now); err != nil {
			if errors.Is(err, errSwarmKeyChanged) {
				// Registered before under another key: the
				// pinned one stays, and the sender is told
				// its heartbeat was not filed.
				http.Error(w, "conflict", http.StatusConflict)
				return
			}
			logging.Errorf("swarm: learn seed registration: %v", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		if err := app.queries.TouchSwarmPeerGossip(ctx, db.TouchSwarmPeerGossipParams{InstanceID: entry.Signer, LastGossip: timeSet(now)}); err != nil {
			logging.Errorf("swarm: touch registration %s: %v", entry.Signer, err)
		}
	}
	writeSwarmJSON(w, map[string]any{"ok": true})
}

// swarmPullRequest is one pull: the keys the caller already
// wants (demand-driven — a peer only ever serves what the caller
// asked for, never what it chose to send).
type swarmPullRequest struct {
	Keys []swarmPullKey `json:"keys"`
}

// swarmPullKey names one public record.
type swarmPullKey struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

// handleSwarmPull serves POST /swarm/pull: the signed entries
// for the requested keys this instance holds. Unknown kinds and
// keys it does not hold are skipped silently, so the response
// never says what is missing — only what is shared.
func (app *Application) handleSwarmPull(w http.ResponseWriter, r *http.Request) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !swarmAuth(app.cfg.swarm.secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req swarmPullRequest
	if err := readSwarmJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Keys) > maxSwarmPullKeys {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	entries := []swarmSignedEntry{}
	for _, k := range req.Keys {
		if !validSwarmKind(k.Kind) || k.Key == "" || len(k.Key) > 128 {
			continue
		}
		payload, fetchedAt, ok := app.serveSwarmKey(ctx, k.Kind, k.Key)
		if !ok {
			continue
		}
		entry, err := signSwarmEntry(app.swarmID, app.swarmID.id, k.Kind, k.Key, payload, fetchedAt)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	writeSwarmJSON(w, map[string]any{"entries": entries})
}

// readSwarmJSON strictly decodes one size-capped request body.
// Unknown fields reject the request: the wire format is fixed,
// and anything else is a probe or a version skew to notice.
func readSwarmJSON(r *http.Request, into any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSwarmBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	return nil
}

// writeSwarmJSON writes one response envelope.
func writeSwarmJSON(w http.ResponseWriter, body any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = buf.WriteTo(w)
}

// validSwarmBaseURL reports whether a peer URL may be dialed:
// https anywhere, http on loopback only (local development and
// tests, where it never leaves the machine).
func validSwarmBaseURL(raw string) bool {
	u, err := parseSwarmURL(raw)
	if err != nil {
		return false
	}
	switch u.scheme {
	case "https":
		return true
	case "http":
		return swarmHostIsLoopback(u.host)
	default:
		return false
	}
}

// swarmClient fetches from one peer: membership lists and
// entries, over verifying TLS, with every response size-capped
// and strictly decoded. newSwarmClient (swarm_dial.go) builds
// it with the guarded dialer and the same-host redirect rule.
type swarmClient struct {
	secret string
	http   *http.Client
}

// swarmAuthedGET is one bearer-authenticated GET with a capped,
// strictly-decoded JSON response.
func (c *swarmClient) authedGET(ctx context.Context, url string, into any) error {
	if !validSwarmBaseURL(url) {
		return fmt.Errorf("swarm: refusing to dial %q (https, or http on loopback, only)", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("swarm: GET %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("swarm: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("swarm: GET %s: status %d", url, resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxSwarmBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("swarm: GET %s: %w", url, err)
	}
	return nil
}

// pullEntries asks one peer for signed entries for the given
// keys. Verification happens at staging, not here: the caller
// checks every entry against the signer's pinned key before
// anything is stored.
func (c *swarmClient) pullEntries(ctx context.Context, baseURL string, keys []swarmPullKey) ([]swarmSignedEntry, error) {
	if !validSwarmBaseURL(baseURL) {
		return nil, fmt.Errorf("swarm: refusing to dial %q (https, or http on loopback, only)", baseURL)
	}
	if len(keys) > maxSwarmPullKeys {
		return nil, fmt.Errorf("swarm: %d keys exceeds the pull cap of %d", len(keys), maxSwarmPullKeys)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(swarmPullRequest{Keys: keys}); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/swarm/pull", &buf)
	if err != nil {
		return nil, fmt.Errorf("swarm: pull %s: %w", baseURL, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("swarm: pull %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("swarm: pull %s: status %d", baseURL, resp.StatusCode)
	}
	var body struct {
		Entries []swarmSignedEntry `json:"entries"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxSwarmBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("swarm: pull %s: %w", baseURL, err)
	}
	return body.Entries, nil
}
