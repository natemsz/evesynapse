package swarm

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
// the host it asked (swarm/dial.go), so a gossip-learned URL
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
	MaxSwarmPullKeys  = 50
	MaxSwarmBodyBytes = 1 << 20
)

// SwarmHTTPTimeout bounds one peer round trip. Peers are few and
// slow gossip is fine; a hung peer must not hold a worker pass.
const SwarmHTTPTimeout = 15 * time.Second

// SwarmMember is one instance as the membership list carries it:
// identity material only, never reputation judgments. URLSig is
// the member's own signature over its ID, address and IssuedAt
// (SwarmMemberMessage), made with the key beside it: the tuple
// is relayed exactly as its owner issued it, by instances and
// seeds alike, and nobody signs an address for anyone else.
// IssuedAt is the owner's clock when it signed; of two tuples
// for one member the later-issued is the current one.
type SwarmMember struct {
	InstanceID string `json:"instance_id"`
	BaseURL    string `json:"base_url"`
	PublicKey  string `json:"public_key"`
	URLSig     string `json:"url_sig"`
	IssuedAt   int64  `json:"issued_at"`
}

// SwarmAuth checks the request's bearer token against the shared
// secret. Hashes are compared (not the secret itself) so a
// length difference leaks nothing, in constant time so a prefix
// match learns nothing. False on any failure, with no reason.
func SwarmAuth(secret string, r *http.Request) bool {
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

// HandleSwarmMembers serves GET /swarm/members: the signed
// membership list — this instance plus every non-excluded peer
// it knows, so the caller learns the swarm one hop at a time.
func (app *Mod) HandleSwarmMembers(w http.ResponseWriter, r *http.Request) {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !SwarmAuth(app.Cfg.Secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	// Self first, always: the key is the attestation (it
	// verifies the list), even when this instance has no
	// public address configured to advertise beside it.
	// Callers file the signer under the URL that answered.
	self := SwarmMember{InstanceID: app.SwarmID.ID, BaseURL: app.Deps.PublicOrigin(), PublicKey: app.SwarmID.PublicString(), IssuedAt: time.Now().Unix()}
	self.URLSig = SignSwarmMember(app.SwarmID, self.BaseURL, self.IssuedAt)
	members := []SwarmMember{self}
	peers, err := app.Queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: list peers: %v", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	for _, p := range peers {
		// A peer known only by an address this instance dialed
		// has no signed address to pass on.
		if p.Excluded || p.UrlSig == "" {
			continue
		}
		members = append(members, SwarmMember{InstanceID: p.InstanceID, BaseURL: p.BaseUrl, PublicKey: p.PublicKey, URLSig: p.UrlSig, IssuedAt: p.UrlIssuedAt})
	}
	raw, err := json.Marshal(members)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	// Sealed under the shared secret before signing: possession
	// of the secret decides who may fetch; the seal decides who
	// may read. A response that leaks anywhere else stays opaque.
	sealed, err := SealSwarmMembers(raw, app.Cfg.Secret)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	entry, err := SignSwarmEntry(app.SwarmID, app.SwarmID.ID, SwarmKindMembers, app.SwarmID.ID, sealed, time.Now())
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	WriteSwarmJSON(w, map[string]any{"entry": entry})
}

// HandleSwarmSeedRegister serves POST /swarm/seed/register: a
// peer's self-signed heartbeat, the same entry a seed worker
// files. The body holds only the sender's own member tuple (URL,
// public key, and its signature over the URL); the signature must verify under the payload's key with
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
func (app *Mod) HandleSwarmSeedRegister(w http.ResponseWriter, r *http.Request) {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !SwarmAuth(app.Cfg.Secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Entry SwarmSignedEntry `json:"entry"`
	}
	if err := ReadSwarmJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	entry := body.Entry
	if entry.Kind != SeedAnnounceKind || entry.Signer == "" || entry.Key != entry.Signer {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var payload SeedAnnouncePayload
	if err := DecodeSwarmStrict(strings.NewReader(entry.Payload), &payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !ValidSwarmBaseURL(payload.BaseURL) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(payload.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !VerifySwarmEntry(entry, payload.PublicKey) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// The tuple this instance will store and relay: the sender's
	// own signature over its address, under the same key.
	tuple := SwarmMember{InstanceID: entry.Signer, BaseURL: payload.BaseURL, PublicKey: payload.PublicKey, URLSig: payload.URLSig, IssuedAt: payload.IssuedAt}
	if !VerifySwarmMember(tuple) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	now := time.Now()
	if entry.Signer != app.SwarmID.ID {
		if err := app.SwarmLearnMembers(ctx, []SwarmMember{tuple}, entry.Signer, payload.PublicKey, "", "gossip", now); err != nil {
			if errors.Is(err, ErrSwarmKeyChanged) {
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
		if err := app.Queries.TouchSwarmPeerGossip(ctx, db.TouchSwarmPeerGossipParams{InstanceID: entry.Signer, LastGossip: timeSet(now)}); err != nil {
			logging.Errorf("swarm: touch registration %s: %v", entry.Signer, err)
		}
	}
	WriteSwarmJSON(w, map[string]any{"ok": true})
}

// SwarmPullRequest is one pull: the keys the caller already
// wants (demand-driven — a peer only ever serves what the caller
// asked for, never what it chose to send).
type SwarmPullRequest struct {
	Keys []SwarmPullKey `json:"keys"`
}

// SwarmPullKey names one public record.
type SwarmPullKey struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

// HandleSwarmPull serves POST /swarm/pull: the signed entries
// for the requested keys this instance holds. Unknown kinds and
// keys it does not hold are skipped silently, so the response
// never says what is missing — only what is shared.
func (app *Mod) HandleSwarmPull(w http.ResponseWriter, r *http.Request) {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		http.NotFound(w, r)
		return
	}
	if !SwarmAuth(app.Cfg.Secret, r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req SwarmPullRequest
	if err := ReadSwarmJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Keys) > MaxSwarmPullKeys {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	entries := []SwarmSignedEntry{}
	for _, k := range req.Keys {
		if !ValidSwarmKind(k.Kind) || k.Key == "" || len(k.Key) > 128 {
			continue
		}
		payload, fetchedAt, ok := app.ServeSwarmKey(ctx, k.Kind, k.Key)
		if !ok {
			// A dedicated node takes the miss as demand and
			// fetches it on its own budget (swarm/warm.go).
			app.SwarmNoteDemand(ctx, k.Kind, k.Key)
			continue
		}
		entry, err := SignSwarmEntry(app.SwarmID, app.SwarmID.ID, k.Kind, k.Key, payload, fetchedAt)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	WriteSwarmJSON(w, map[string]any{"entries": entries})
}

// ReadSwarmJSON strictly decodes one size-capped request body.
// Unknown fields reject the request: the wire format is fixed,
// and anything else is a probe or a version skew to notice.
func ReadSwarmJSON(r *http.Request, into any) error {
	defer r.Body.Close()
	return DecodeSwarmStrict(io.LimitReader(r.Body, MaxSwarmBodyBytes), into)
}

// WriteSwarmJSON writes one response envelope.
func WriteSwarmJSON(w http.ResponseWriter, body any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = buf.WriteTo(w)
}

// ValidSwarmBaseURL reports whether a peer URL may be dialed:
// https anywhere, http on loopback only (local development and
// tests, where it never leaves the machine).
func ValidSwarmBaseURL(raw string) bool {
	u, err := ParseSwarmURL(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return SwarmHostIsLoopback(u.Host)
	default:
		return false
	}
}

// SwarmClient fetches from one peer: membership lists and
// entries, over verifying TLS, with every response size-capped
// and strictly decoded. NewSwarmClient (swarm/dial.go) builds
// it with the guarded dialer and the same-host redirect rule.
type SwarmClient struct {
	Secret string
	Http   *http.Client
}

// SwarmStrictDecoder decodes swarm JSON strictly: an unknown field
// is an error, because the wire format is fixed and anything else is
// a probe or a version skew to notice. Every swarm decode goes
// through it (DecodeSwarmStrict for one value).
func SwarmStrictDecoder(r io.Reader) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec
}

// DecodeSwarmStrict strictly decodes one value from r.
func DecodeSwarmStrict(r io.Reader, into any) error {
	return SwarmStrictDecoder(r).Decode(into)
}

// Call is every outbound swarm request: the address checked before
// dialing (https, or http on loopback), the secret in the bearer
// header only, the body (if any) sent as JSON, a non-200 answer an
// error, and the answer size-capped and strictly decoded into into.
func (c *SwarmClient) Call(ctx context.Context, method, url string, body, into any) error {
	if !ValidSwarmBaseURL(url) {
		return fmt.Errorf("swarm: refusing to dial %q (https, or http on loopback, only)", url)
	}
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return fmt.Errorf("swarm: %s %s: %w", method, url, err)
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("swarm: %s %s: %w", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Http.Do(req)
	if err != nil {
		return fmt.Errorf("swarm: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("swarm: %s %s: status %d", method, url, resp.StatusCode)
	}
	if err := DecodeSwarmStrict(io.LimitReader(resp.Body, MaxSwarmBodyBytes), into); err != nil {
		return fmt.Errorf("swarm: %s %s: %w", method, url, err)
	}
	return nil
}

// AuthedGET is one GET through Call.
func (c *SwarmClient) AuthedGET(ctx context.Context, url string, into any) error {
	return c.Call(ctx, http.MethodGet, url, nil, into)
}

// PullEntries asks one peer for signed entries for the given
// keys. Verification happens at staging, not here: the caller
// checks every entry against the signer's pinned key before
// anything is stored.
func (c *SwarmClient) PullEntries(ctx context.Context, baseURL string, keys []SwarmPullKey) ([]SwarmSignedEntry, error) {
	if len(keys) > MaxSwarmPullKeys {
		return nil, fmt.Errorf("swarm: %d keys exceeds the pull cap of %d", len(keys), MaxSwarmPullKeys)
	}
	var body struct {
		Entries []SwarmSignedEntry `json:"entries"`
	}
	if err := c.Call(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/swarm/pull", SwarmPullRequest{Keys: keys}, &body); err != nil {
		return nil, err
	}
	return body.Entries, nil
}
