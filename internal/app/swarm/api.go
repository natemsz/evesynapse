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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
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

// SwarmPullSignatureWindow is how far a signed pull's timestamp
// may sit from this clock, either way. Pulls are made on demand,
// never queued for later, so five minutes is drift and delay
// together with room to spare — and a captured signature is
// useless outside it.
const SwarmPullSignatureWindow = 5 * time.Minute

// SwarmPullMessage is the exact byte string a puller signs:
// "pull\n" + its instance ID + "\n" + the decimal unix timestamp
// it sends + "\n" + the lowercase hex SHA-256 of the exact
// request body bytes. The body hash binds the signature to this
// one request (the keys it asks for), the timestamp bounds its
// life, and the instance ID is the signer's own — a signature
// lifted onto another instance's header names another message.
// The storm node builds the same bytes; changing them is a flag
// day for every puller and server.
func SwarmPullMessage(instanceID string, timestamp int64, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte("pull\n" + instanceID + "\n" + strconv.FormatInt(timestamp, 10) + "\n" + hex.EncodeToString(sum[:]))
}

// SignSwarmPull signs one pull request body as id at timestamp.
func SignSwarmPull(id *SwarmIdentity, timestamp int64, body []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(id.Private, SwarmPullMessage(id.ID, timestamp, body)))
}

// VerifySwarmPullSignature checks one pull signature against
// the claimed instance's pinned public key. Every failure is
// false, with no reason given — the caller logs that a signature
// failed, never which part of it.
func VerifySwarmPullSignature(publicKey, instanceID string, timestamp int64, body []byte, signature string) bool {
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, SwarmPullMessage(instanceID, timestamp, body), sig)
}

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
	// Build is the member's self-reported build claim
	// (build.go), carried for the Admin page only. It is not
	// part of the signed address tuple — the signature
	// covers ID, address and IssuedAt — so a relay passes on
	// the newest claim it heard beside a tuple it relayed,
	// and a receiver files it as display text. Nothing in
	// the trust model reads it.
	Build string `json:"build,omitempty"`
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

// The two ways a pull request proves it may be served. The
// signed way names the puller and is checked against the key
// pinned for it here; the legacy way is the shared secret every
// swarm endpoint still accepts. Signed is the future, bearer is
// the transition: a puller dual-sends both, and the server takes
// the signed way when it verifies and the bearer when it does
// not, so old and new instances pull from each other throughout.
const (
	SwarmPullAuthSigned = "signed"
	SwarmPullAuthLegacy = "legacy"
)

// swarmPullAuth decides how one pull request is authenticated,
// over the exact body bytes received. The signed path is valid
// only when all three headers are present, the claimed instance
// is a peer this instance knows and has not excluded, the
// timestamp is inside SwarmPullSignatureWindow of this clock,
// and the signature verifies under the peer's pinned key.
//
// A signature that fails is logged — a peer whose clock or key
// has drifted should be visible in the log, not mysterious — but
// it is never anything more: no trust verdict, no shape reject.
// Authentication is not data, and a broken signature proves a
// broken peer setup, not a lying one. The request falls back to
// the bearer check like a pull that never signed at all; only
// when neither path passes is the pull refused.
func (app *Mod) swarmPullAuth(r *http.Request, body []byte) string {
	instance := r.Header.Get("X-Swarm-Instance")
	stamp := r.Header.Get("X-Swarm-Timestamp")
	signature := r.Header.Get("X-Swarm-Signature")
	if instance != "" || stamp != "" || signature != "" {
		if instance != "" && stamp != "" && signature != "" &&
			app.verifySwarmPull(r.Context(), instance, stamp, signature, body) {
			return SwarmPullAuthSigned
		}
		logging.Warnf("swarm: pull signature from %s did not verify; falling back to bearer auth", ShortenSwarmID(instance))
	}
	if SwarmAuth(app.Cfg.Secret, r) {
		return SwarmPullAuthLegacy
	}
	return ""
}

// verifySwarmPull checks one signed pull's claims against the
// peer table: a known, non-excluded instance, a fresh
// timestamp, a signature under the pinned key.
func (app *Mod) verifySwarmPull(ctx context.Context, instance, stamp, signature string, body []byte) bool {
	at, err := strconv.ParseInt(strings.TrimSpace(stamp), 10, 64)
	if err != nil {
		return false
	}
	diff := time.Now().Unix() - at
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(SwarmPullSignatureWindow/time.Second) {
		return false
	}
	peer, err := app.Queries.GetSwarmPeer(ctx, instance)
	if err != nil || peer.Excluded {
		return false
	}
	return VerifySwarmPullSignature(peer.PublicKey, instance, at, body, signature)
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
	self := SwarmMember{InstanceID: app.SwarmID.ID, BaseURL: app.Deps.PublicOrigin(), PublicKey: app.SwarmID.PublicString(), IssuedAt: time.Now().Unix(), Build: BuildClaim()}
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
		members = append(members, SwarmMember{InstanceID: p.InstanceID, BaseURL: p.BaseUrl, PublicKey: p.PublicKey, URLSig: p.UrlSig, IssuedAt: p.UrlIssuedAt, Build: p.Build})
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
	tuple := SwarmMember{InstanceID: entry.Signer, BaseURL: payload.BaseURL, PublicKey: payload.PublicKey, URLSig: payload.URLSig, IssuedAt: payload.IssuedAt, Build: payload.Build}
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
	// The body is read once, before authentication: a signed
	// pull's signature covers the exact bytes, so the bytes are
	// what both the check and the decode below consume.
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxSwarmBodyBytes))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	auth := app.swarmPullAuth(r, raw)
	if auth == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req SwarmPullRequest
	if err := DecodeSwarmStrict(bytes.NewReader(raw), &req); err != nil {
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
	// Counted here, once per request actually served — never
	// per key, and a refused pull counts nowhere.
	switch auth {
	case SwarmPullAuthSigned:
		app.AddSwarmCounter(ctx, SwarmCounterPullsAuthSigned, 1)
	case SwarmPullAuthLegacy:
		app.AddSwarmCounter(ctx, SwarmCounterPullsAuthLegacy, 1)
	}
	served := WriteSwarmJSON(w, map[string]any{"entries": entries})
	// The bytes this pull served, for the Admin page's Swarm
	// panel: the marshaled payload length, before any
	// compression the outer middleware applies.
	app.Deps.NoteActivityN(module.ActivitySwarmBytes, int64(served))
}

// ReadSwarmJSON strictly decodes one size-capped request body.
// Unknown fields reject the request: the wire format is fixed,
// and anything else is a probe or a version skew to notice.
func ReadSwarmJSON(r *http.Request, into any) error {
	defer r.Body.Close()
	return DecodeSwarmStrict(io.LimitReader(r.Body, MaxSwarmBodyBytes), into)
}

// WriteSwarmJSON writes one response envelope and returns the
// payload length in bytes (0 when the body would not encode).
func WriteSwarmJSON(w http.ResponseWriter, body any) int {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return 0
	}
	n := buf.Len()
	w.Header().Set("Content-Type", "application/json")
	_, _ = buf.WriteTo(w)
	return n
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
	// ID is this instance's signing identity, set when the
	// client is built for a running instance (module.go). With
	// it, pulls are signed as well as bearer-authed (dual-send
	// through the transition); without it — tests and any path
	// built outside an instance — pulls send the bearer only.
	ID *SwarmIdentity
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
// header, the body (if any) sent as JSON, a non-200 answer an
// error, and the answer size-capped and strictly decoded into into.
func (c *SwarmClient) Call(ctx context.Context, method, url string, body, into any) error {
	var raw []byte
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return fmt.Errorf("swarm: %s %s: %w", method, url, err)
		}
		raw = buf.Bytes()
	}
	return c.do(ctx, method, url, raw, nil, into)
}

// do sends one already-marshaled body (nil for none) with the
// bearer header and any extra headers the caller computed over
// those exact bytes — a pull signature must cover the bytes that
// go on the wire, so the marshaling happens once, in the caller.
func (c *SwarmClient) do(ctx context.Context, method, url string, raw []byte, extra map[string]string, into any) error {
	if !ValidSwarmBaseURL(url) {
		return fmt.Errorf("swarm: refusing to dial %q (https, or http on loopback, only)", url)
	}
	var reader io.Reader
	if raw != nil {
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("swarm: %s %s: %w", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Secret)
	req.Header.Set("Accept", "application/json")
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range extra {
		req.Header.Set(name, value)
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
//
// The request itself is signed when the client carries this
// instance's identity, and the bearer goes with it either way
// (dual-send): a peer that verifies signatures takes the signed
// path, a peer from before signatures takes the bearer, and this
// instance can pull from both while the swarm transitions. The
// body is marshaled once, here, because the signature covers
// the exact bytes sent.
func (c *SwarmClient) PullEntries(ctx context.Context, baseURL string, keys []SwarmPullKey) ([]SwarmSignedEntry, error) {
	if len(keys) > MaxSwarmPullKeys {
		return nil, fmt.Errorf("swarm: %d keys exceeds the pull cap of %d", len(keys), MaxSwarmPullKeys)
	}
	raw, err := json.Marshal(SwarmPullRequest{Keys: keys})
	if err != nil {
		return nil, fmt.Errorf("swarm: pull: %w", err)
	}
	var headers map[string]string
	if c.ID != nil {
		at := time.Now().Unix()
		headers = map[string]string{
			"X-Swarm-Instance":  c.ID.ID,
			"X-Swarm-Timestamp": strconv.FormatInt(at, 10),
			"X-Swarm-Signature": SignSwarmPull(c.ID, at, raw),
		}
	}
	var body struct {
		Entries []SwarmSignedEntry `json:"entries"`
	}
	if err := c.do(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/swarm/pull", raw, headers, &body); err != nil {
		return nil, err
	}
	return body.Entries, nil
}
