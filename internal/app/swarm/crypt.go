package swarm

// ---------------------------------------------------------------------------
// Swarm member privacy. The member list is topology: who is in
// the swarm and where they listen. Two layers keep it from
// leaking:
//
// 1. The /swarm/members endpoint is bearer-gated (swarm/api.go),
//    so only secret-holders can fetch it at all. On top of that
//    the list payload itself is sealed under a key derived from
//    the shared secret — a leaked, cached, or logged response
//    still decodes for members alone.
//
// 2. Pages never show raw instance IDs or addresses for peers
//    learned through gossip or seeds. Each peer displays under a
//    stable alias (adjective.noun.number, derived from the ID
//    and the secret, so every member computes the same one).
//    The exceptions are addresses the operator added by hand
//    and the configured seeds (evesynapse.app): those show
//    plainly, because the operator already knows them.
//
// All crypto is the standard library (HKDF-SHA-256, AES-256-GCM,
// HMAC-SHA-256) with the same envelope discipline as the token
// box and the sealed identity. No handrolled anything.
// ---------------------------------------------------------------------------

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// The sealed member-list envelope: "swm:v1:" +
// base64(nonce | ciphertext). Salt and label are part of the
// format, as with the token box and the sealed seed.
const (
	SwarmMembersPrefix = "swm:v1:"
	SwarmMembersSalt   = "evesynapse/swarm-members/v1"
	SwarmMembersInfo   = "swarm member list"
)

// SwarmMembersKey derives the list-sealing key from the shared
// secret. Members hold the secret, so members alone decode.
func SwarmMembersKey(secret string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(secret), []byte(SwarmMembersSalt), SwarmMembersInfo, 32)
}

// SealSwarmMembers seals one marshalled member list under the
// shared secret. The signature still goes over the sealed blob
// (encrypt-then-sign): authenticity is checked before anything
// decrypts.
func SealSwarmMembers(raw []byte, secret string) (string, error) {
	key, err := SwarmMembersKey(secret)
	if err != nil {
		return "", err
	}
	aead, err := gcmForKey(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("swarm: seal members: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, raw, nil)
	return SwarmMembersPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// OpenSwarmMembers opens a sealed member list. Any failure is
// one error: callers must not learn whether the prefix, the
// encoding, the secret, or the ciphertext was wrong.
func OpenSwarmMembers(sealed, secret string) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, SwarmMembersPrefix)
	if !ok {
		return nil, errors.New("swarm: member list does not open")
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return nil, errors.New("swarm: member list does not open")
	}
	key, err := SwarmMembersKey(secret)
	if err != nil {
		return nil, errors.New("swarm: member list does not open")
	}
	aead, err := gcmForKey(key)
	if err != nil {
		return nil, errors.New("swarm: member list does not open")
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("swarm: member list does not open")
	}
	opened, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("swarm: member list does not open")
	}
	return opened, nil
}

// swarmAliasWords are the alias pools (adjective.noun.number).
// Plain neutral words: an alias identifies a row, never a
// machine, owner, or place.
var (
	SwarmAliasAdjectives = []string{
		"amber", "arctic", "ashen", "birch", "brisk", "calm", "cedar",
		"clear", "cobalt", "copper", "coral", "crisp", "dawn", "delta",
		"ember", "even", "fern", "flint", "frost", "garnet", "gentle",
		"glacier", "granite", "harbor", "hazel", "indigo", "iron",
		"ivory", "juniper", "kelp", "linen", "lunar", "maple", "marble",
		"meadow", "moss", "nimbus", "north", "ochre", "onyx", "opal",
		"pebble", "pine", "quartz", "quiet", "river", "sable", "sage",
	}
	SwarmAliasNouns = []string{
		"alder", "anchor", "antler", "arc", "basin", "beacon", "birch",
		"brook", "burrow", "cairn", "cedar", "channel", "cinder", "cliff",
		"comet", "cove", "crane", "crest", "drift", "dune", "elm",
		"falcon", "fjord", "flute", "fox", "frost", "grove", "gull",
		"harbor", "hawk", "heath", "heron", "hollow", "inlet", "islet",
		"lagoon", "lark", "ledge", "lotus", "meadow", "merlin", "nest",
		"otter", "owl", "ridge", "robin", "spruce", "wren",
	}
)

// SwarmAlias derives a peer's display alias from its instance ID
// and the shared secret: stable per swarm (every member
// computes the same one), meaningless without the list it came
// with. HMAC keyed by the secret, so aliases differ per swarm
// and reveal nothing across swarms.
func SwarmAlias(secret, instanceID string) string {
	mac := hmac.New(sha256.New, []byte("swarm-alias/v1\x00"+secret))
	mac.Write([]byte(instanceID))
	sum := mac.Sum(nil)
	adj := SwarmAliasAdjectives[int(sum[0])%len(SwarmAliasAdjectives)]
	noun := SwarmAliasNouns[int(sum[1])%len(SwarmAliasNouns)]
	num := (int(sum[2])<<8 | int(sum[3])) % 1000
	return fmt.Sprintf("%s.%s.%d", adj, noun, num)
}
