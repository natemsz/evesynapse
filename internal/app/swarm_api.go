package app

// ---------------------------------------------------------------------------
// The swarm's HTTP API: how instances talk to each other. Two
// endpoints, both bearer-gated by the shared secret, both dark
// (404) while the shared cache is off so a disabled instance
// offers no surface to probe.
//
//   GET  /swarm/members  — this instance's signed membership list
//   POST /swarm/pull     — signed public-data entries for wanted keys
//
// The API is pull-only: instances fetch from peers they trust;
// no instance can push data (or wants, or queue hints) into
// another. Membership spreads peer to peer (gossip); reputation
// never does — every instance scores signers from its own spot
// checks, so a liar cannot talk others into distrusting someone.
//
// Transport security is the standard library throughout: the
// server side is the app's own TLS terminator (or its loopback
// listener), the client verifies certificates by default, and
// the bearer check compares SHA-256 hashes in constant time.
// Cleartext to a non-loopback host is refused on dial, so a
// gossip-learned URL can never downgrade the secret onto the
// wire. No handrolled crypto, no custom framing: JSON over
// HTTPS, strict-decoded, size-capped.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
	members := []swarmMember{}
	if origin := app.cfg.publicOrigin(); origin != "" {
		members = append(members, swarmMember{InstanceID: app.swarmID.id, BaseURL: origin, PublicKey: app.swarmID.publicString()})
	}
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
	entry, err := signSwarmEntry(app.swarmID, app.swarmID.id, swarmKindMembers, app.swarmID.id, string(raw), time.Now())
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	writeSwarmJSON(w, map[string]any{"entry": entry})
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
// and strictly decoded.
type swarmClient struct {
	secret string
	http   *http.Client
}

func newSwarmClient(secret string) *swarmClient {
	return &swarmClient{secret: secret, http: &http.Client{Timeout: swarmHTTPTimeout}}
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

// fetchMembers asks one peer for its signed membership list and
// verifies the signature against the peer's pinned key before
// returning a single member. An unsigned or mis-signed list is
// worthless: gossip content is untrusted until checked.
func (c *swarmClient) fetchMembers(ctx context.Context, baseURL, peerID, peerKey string) ([]swarmMember, error) {
	var body struct {
		Entry swarmSignedEntry `json:"entry"`
	}
	if err := c.authedGET(ctx, strings.TrimSuffix(baseURL, "/")+"/swarm/members", &body); err != nil {
		return nil, err
	}
	entry := body.Entry
	if entry.Kind != swarmKindMembers || entry.Signer != peerID {
		return nil, fmt.Errorf("swarm: %s served a member list signed by %q", baseURL, entry.Signer)
	}
	if !verifySwarmEntry(entry, peerKey) {
		return nil, fmt.Errorf("swarm: %s served a member list that does not verify", baseURL)
	}
	var members []swarmMember
	dec := json.NewDecoder(strings.NewReader(entry.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&members); err != nil {
		return nil, fmt.Errorf("swarm: %s served an unreadable member list", baseURL)
	}
	return members, nil
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
