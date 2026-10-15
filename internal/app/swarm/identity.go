package swarm

// ---------------------------------------------------------------------------
// Swarm identity and entry signatures (the paper: "every shared
// entry is signed by the instance that fetched it, the signature
// is verified on every read").
//
// Each instance owns an Ed25519 keypair (crypto/ed25519 from the
// standard library — no handrolled crypto anywhere in this
// module). The seed is sealed at rest under a key derived from
// the operator's shared secret (HKDF-SHA-256, AES-256-GCM, the
// same construction as the token box), so a copy of the database
// alone cannot sign as this instance. The public key travels in
// the membership lists; peers pin the key they first saw for an
// instance ID and verify every entry against it.
//
// A signed envelope covers kind, key, payload, fetch time and
// signer in one canonical JSON document. Anything outside the
// envelope (transport framing, gossip metadata) is never trusted
// for the entry's content.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// The sealed-seed envelope: "sw:v1:" + base64(nonce | ciphertext).
// The salt and label are part of the format: changing either
// stops every sealed seed opening, which mints a fresh identity
// rather than reusing a key that cannot be read.
const (
	SwarmSeedPrefix = "sw:v1:"
	SwarmKDFSalt    = "evesynapse/swarm-seed/v1"
	SwarmKDFInfo    = "swarm entry signing"
)

// SwarmIdentity is this instance: its ID and its signing key.
type SwarmIdentity struct {
	ID      string
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// PublicString is the key as it travels in membership lists.
func (id *SwarmIdentity) PublicString() string {
	return base64.StdEncoding.EncodeToString(id.Public)
}

// LoadSwarmIdentity reads this instance's identity, minting and
// storing one on first enable. A seal that stops opening (the
// secret changed) mints a fresh identity: the old key is gone
// with the old secret, and signing on with an unreadable key is
// impossible by construction.
func LoadSwarmIdentity(ctx context.Context, q *db.Queries, secret string) (*SwarmIdentity, error) {
	if row, err := q.GetSwarmIdentity(ctx); err == nil {
		if id, ok := OpenSwarmIdentity(row.InstanceID, row.PublicKey, row.SealedSeed, secret); ok {
			return id, nil
		}
		logging.Warnf("swarm: the sealed signing key does not open with the configured secret; minting a fresh instance identity")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("swarm: read identity: %w", err)
	}
	id, err := MintSwarmIdentity()
	if err != nil {
		return nil, err
	}
	sealed, err := SealSwarmSeed(id.Private.Seed(), secret)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := q.PutSwarmIdentity(ctx, db.PutSwarmIdentityParams{
		InstanceID: id.ID,
		PublicKey:  id.PublicString(),
		SealedSeed: sealed,
		CreatedAt:  now,
	}); err != nil {
		return nil, fmt.Errorf("swarm: store identity: %w", err)
	}
	return id, nil
}

// MintSwarmIdentity generates one fresh instance ID and keypair.
func MintSwarmIdentity() (*SwarmIdentity, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("swarm: mint identity: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("swarm: mint identity: %w", err)
	}
	return &SwarmIdentity{
		ID:      base64.RawURLEncoding.EncodeToString(raw[:]),
		Public:  pub,
		Private: priv,
	}, nil
}

// OpenSwarmIdentity unseals a stored identity. ok is false on any
// failure: a wrong key, a wrong secret, or a malformed row all
// refuse identically, with nothing about which check failed.
func OpenSwarmIdentity(instanceID, publicKey, sealed, secret string) (id *SwarmIdentity, ok bool) {
	if instanceID == "" {
		return nil, false
	}
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, false
	}
	seed, err := UnsealSwarmSeed(sealed, secret)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if !ed25519.PublicKey(pub).Equal(priv.Public()) {
		return nil, false
	}
	return &SwarmIdentity{ID: instanceID, Public: pub, Private: priv}, true
}

// SwarmSealKey derives the seed-sealing key from the shared
// secret (HKDF-SHA-256, the token box's construction with the
// swarm's own salt and label).
func SwarmSealKey(secret string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(secret), []byte(SwarmKDFSalt), SwarmKDFInfo, 32)
}

// SealSwarmSeed seals an Ed25519 seed under the shared secret.
func SealSwarmSeed(seed []byte, secret string) (string, error) {
	key, err := SwarmSealKey(secret)
	if err != nil {
		return "", err
	}
	aead, err := gcmForKey(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("swarm: seal seed: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, seed, nil)
	return SwarmSeedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// UnsealSwarmSeed opens a sealed seed. Any failure is one error:
// callers must not learn whether the prefix, the encoding, the
// secret, or the key was wrong.
func UnsealSwarmSeed(sealed, secret string) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, SwarmSeedPrefix)
	if !ok {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	key, err := SwarmSealKey(secret)
	if err != nil {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	aead, err := gcmForKey(key)
	if err != nil {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	seed, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	return seed, nil
}

// SwarmEnvelope is the canonical signed document: struct field
// order makes encoding/json deterministic, so signer and
// verifier hash the same bytes without a canonicalizer.
type SwarmEnvelope struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Payload   string `json:"payload"`
	FetchedAt int64  `json:"fetched_at"`
	Signer    string `json:"signer"`
}

// SwarmSignedEntry is an envelope with its signature, as it
// travels over the swarm API and sits in staging.
type SwarmSignedEntry struct {
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Payload   string `json:"payload"`
	FetchedAt int64  `json:"fetched_at"`
	Signer    string `json:"signer"`
	Signature string `json:"signature"`
}

// SignSwarmEntry signs one entry. Payload size is capped: a peer
// asked to store megabytes per entry could fill the staging
// table one entry at a time.
const MaxSwarmPayloadBytes = 256 * 1024

func SignSwarmEntry(id *SwarmIdentity, instanceID, kind, key, payload string, fetchedAt time.Time) (SwarmSignedEntry, error) {
	entry := SwarmSignedEntry{Kind: kind, Key: key, Payload: payload, FetchedAt: fetchedAt.UTC().Unix(), Signer: instanceID}
	if len(payload) > MaxSwarmPayloadBytes {
		return SwarmSignedEntry{}, fmt.Errorf("swarm: sign %s %s: payload %d bytes exceeds %d", kind, key, len(payload), MaxSwarmPayloadBytes)
	}
	doc, err := json.Marshal(SwarmEnvelope{V: 1, Kind: kind, Key: key, Payload: payload, FetchedAt: entry.FetchedAt, Signer: instanceID})
	if err != nil {
		return SwarmSignedEntry{}, fmt.Errorf("swarm: sign %s %s: %w", kind, key, err)
	}
	entry.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(id.Private, doc))
	return entry, nil
}

// VerifySwarmEntry checks one entry against the signer's pinned
// public key. Every field the entry carries is covered; every
// failure is false, with no reason given (the reason is a
// logging decision at the Call site, never a parsing oracle).
func VerifySwarmEntry(entry SwarmSignedEntry, publicKey string) bool {
	if entry.Kind == "" || entry.Key == "" || entry.Signer == "" {
		return false
	}
	if len(entry.Payload) > MaxSwarmPayloadBytes {
		return false
	}
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(entry.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	doc, err := json.Marshal(SwarmEnvelope{V: 1, Kind: entry.Kind, Key: entry.Key, Payload: entry.Payload, FetchedAt: entry.FetchedAt, Signer: entry.Signer})
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, doc, sig)
}

// SwarmMemberMessage is what a member signs to own its address:
// "member\n" + instance ID + "\n" + base URL + "\n" + the unix
// time it was issued, in decimal. The issue time is inside the
// signature so that an old tuple cannot be passed off as current:
// per member, only a tuple issued later than the one held is
// taken up. It is only ever compared with that same member's
// earlier issue times, so clocks need not agree across instances.
// The seed worker builds the same bytes; changing them is a flag
// day for every instance and seed.
func SwarmMemberMessage(instanceID, baseURL string, issuedAt int64) []byte {
	return []byte("member\n" + instanceID + "\n" + baseURL + "\n" + strconv.FormatInt(issuedAt, 10))
}

// SignSwarmMember signs this instance's own address as of issuedAt.
func SignSwarmMember(id *SwarmIdentity, baseURL string, issuedAt int64) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(id.Private, SwarmMemberMessage(id.ID, baseURL, issuedAt)))
}

// VerifySwarmMember checks a member tuple's address signature
// against the public key in the same tuple. Whether that key is
// the one pinned for the instance, and whether the tuple is newer
// than the one held, are the caller's questions.
func VerifySwarmMember(m SwarmMember) bool {
	pub, err := base64.StdEncoding.DecodeString(m.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(m.URLSig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, SwarmMemberMessage(m.InstanceID, m.BaseURL, m.IssuedAt), sig)
}
