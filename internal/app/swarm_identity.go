package app

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
	swarmSeedPrefix = "sw:v1:"
	swarmKDFSalt    = "evesynapse/swarm-seed/v1"
	swarmKDFInfo    = "swarm entry signing"
)

// swarmIdentity is this instance: its ID and its signing key.
type swarmIdentity struct {
	id      string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

// publicString is the key as it travels in membership lists.
func (id *swarmIdentity) publicString() string {
	return base64.StdEncoding.EncodeToString(id.public)
}

// loadSwarmIdentity reads this instance's identity, minting and
// storing one on first enable. A seal that stops opening (the
// secret changed) mints a fresh identity: the old key is gone
// with the old secret, and signing on with an unreadable key is
// impossible by construction.
func loadSwarmIdentity(ctx context.Context, q *db.Queries, secret string) (*swarmIdentity, error) {
	if row, err := q.GetSwarmIdentity(ctx); err == nil {
		if id, ok := openSwarmIdentity(row.InstanceID, row.PublicKey, row.SealedSeed, secret); ok {
			return id, nil
		}
		logging.Warnf("swarm: the sealed signing key does not open with the configured secret; minting a fresh instance identity")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("swarm: read identity: %w", err)
	}
	id, err := mintSwarmIdentity()
	if err != nil {
		return nil, err
	}
	sealed, err := sealSwarmSeed(id.private.Seed(), secret)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := q.PutSwarmIdentity(ctx, db.PutSwarmIdentityParams{
		InstanceID: id.id,
		PublicKey:  id.publicString(),
		SealedSeed: sealed,
		CreatedAt:  now,
	}); err != nil {
		return nil, fmt.Errorf("swarm: store identity: %w", err)
	}
	return id, nil
}

// mintSwarmIdentity generates one fresh instance ID and keypair.
func mintSwarmIdentity() (*swarmIdentity, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("swarm: mint identity: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("swarm: mint identity: %w", err)
	}
	return &swarmIdentity{
		id:      base64.RawURLEncoding.EncodeToString(raw[:]),
		public:  pub,
		private: priv,
	}, nil
}

// openSwarmIdentity unseals a stored identity. ok is false on any
// failure: a wrong key, a wrong secret, or a malformed row all
// refuse identically, with nothing about which check failed.
func openSwarmIdentity(instanceID, publicKey, sealed, secret string) (id *swarmIdentity, ok bool) {
	if instanceID == "" {
		return nil, false
	}
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, false
	}
	seed, err := unsealSwarmSeed(sealed, secret)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if !ed25519.PublicKey(pub).Equal(priv.Public()) {
		return nil, false
	}
	return &swarmIdentity{id: instanceID, public: pub, private: priv}, true
}

// swarmSealKey derives the seed-sealing key from the shared
// secret (HKDF-SHA-256, the token box's construction with the
// swarm's own salt and label).
func swarmSealKey(secret string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(secret), []byte(swarmKDFSalt), swarmKDFInfo, 32)
}

// sealSwarmSeed seals an Ed25519 seed under the shared secret.
func sealSwarmSeed(seed []byte, secret string) (string, error) {
	key, err := swarmSealKey(secret)
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
	return swarmSeedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// unsealSwarmSeed opens a sealed seed. Any failure is one error:
// callers must not learn whether the prefix, the encoding, the
// secret, or the key was wrong.
func unsealSwarmSeed(sealed, secret string) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, swarmSeedPrefix)
	if !ok {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return nil, errors.New("swarm: sealed seed does not open")
	}
	key, err := swarmSealKey(secret)
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

// swarmEnvelope is the canonical signed document: struct field
// order makes encoding/json deterministic, so signer and
// verifier hash the same bytes without a canonicalizer.
type swarmEnvelope struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Payload   string `json:"payload"`
	FetchedAt int64  `json:"fetched_at"`
	Signer    string `json:"signer"`
}

// swarmSignedEntry is an envelope with its signature, as it
// travels over the swarm API and sits in staging.
type swarmSignedEntry struct {
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Payload   string `json:"payload"`
	FetchedAt int64  `json:"fetched_at"`
	Signer    string `json:"signer"`
	Signature string `json:"signature"`
}

// signSwarmEntry signs one entry. Payload size is capped: a peer
// asked to store megabytes per entry could fill the staging
// table one entry at a time.
const maxSwarmPayloadBytes = 256 * 1024

func signSwarmEntry(id *swarmIdentity, instanceID, kind, key, payload string, fetchedAt time.Time) (swarmSignedEntry, error) {
	entry := swarmSignedEntry{Kind: kind, Key: key, Payload: payload, FetchedAt: fetchedAt.UTC().Unix(), Signer: instanceID}
	if len(payload) > maxSwarmPayloadBytes {
		return swarmSignedEntry{}, fmt.Errorf("swarm: sign %s %s: payload %d bytes exceeds %d", kind, key, len(payload), maxSwarmPayloadBytes)
	}
	doc, err := json.Marshal(swarmEnvelope{V: 1, Kind: kind, Key: key, Payload: payload, FetchedAt: entry.FetchedAt, Signer: instanceID})
	if err != nil {
		return swarmSignedEntry{}, fmt.Errorf("swarm: sign %s %s: %w", kind, key, err)
	}
	entry.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(id.private, doc))
	return entry, nil
}

// verifySwarmEntry checks one entry against the signer's pinned
// public key. Every field the entry carries is covered; every
// failure is false, with no reason given (the reason is a
// logging decision at the call site, never a parsing oracle).
func verifySwarmEntry(entry swarmSignedEntry, publicKey string) bool {
	if entry.Kind == "" || entry.Key == "" || entry.Signer == "" {
		return false
	}
	if len(entry.Payload) > maxSwarmPayloadBytes {
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
	doc, err := json.Marshal(swarmEnvelope{V: 1, Kind: entry.Kind, Key: entry.Key, Payload: entry.Payload, FetchedAt: entry.FetchedAt, Signer: entry.Signer})
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, doc, sig)
}

// swarmMemberMessage is what a member signs to own its address:
// "member\n" + instance ID + "\n" + base URL. The seed worker
// builds the same bytes; changing them is a flag day for every
// instance and seed.
func swarmMemberMessage(instanceID, baseURL string) []byte {
	return []byte("member\n" + instanceID + "\n" + baseURL)
}

// signSwarmMember signs this instance's own address.
func signSwarmMember(id *swarmIdentity, baseURL string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(id.private, swarmMemberMessage(id.id, baseURL)))
}

// verifySwarmMember checks a member tuple's address signature
// against the public key in the same tuple. Whether that key is
// the one pinned for the instance is the caller's question.
func verifySwarmMember(m swarmMember) bool {
	pub, err := base64.StdEncoding.DecodeString(m.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(m.URLSig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, swarmMemberMessage(m.InstanceID, m.BaseURL), sig)
}
