package app

// ---------------------------------------------------------------------------
// Token encryption at rest. Every linked character's EVE SSO access
// and refresh tokens sit in the characters table, and a refresh
// token is a standing credential: with it, whoever holds a copy of
// the database (a dump, a backup, a stolen disk) can read that
// character's data and use the write scopes the app requests —
// send mail, save fittings — for as long as the character stays
// linked.
//
// With TOKEN_ENCRYPTION_KEY set, both tokens are stored encrypted
// (AES-256-GCM) and a copy of the database alone is no longer
// enough; the key lives in .env, beside the database password,
// not in the database. Without the key the tokens are stored as
// they always were.
//
// The stored form is "enc:v2:" + base64(nonce | ciphertext)
// ("enc:v1:" rows, sealed under the old single-SHA-256 KDF, still
// open and are re-sealed at boot). Each value is bound to the
// character and the column it belongs to, so a ciphertext copied
// onto another row does not decrypt there.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// The stored envelopes. v1 keyed AES-GCM off one SHA-256 of the
// operator's secret; v2 derives the key with HKDF. No EVE token
// starts either way (access tokens are JWTs, "eyJ…"). Both stay
// readable: rows sealed under v1 open with the old derivation and
// are re-sealed as v2 at boot (prepareStoredTokens).
const (
	tokenCipherPrefix   = "enc:v1:"
	tokenCipherV2Prefix = "enc:v2:"
)

// tokenKeyMinLength is the shortest TOKEN_ENCRYPTION_KEY accepted.
// The key is whatever the operator generated, not a password to
// remember; setup.sh writes 64 random hex characters.
const tokenKeyMinLength = 32

// The two token columns, as bound into each ciphertext.
const (
	tokenFieldAccess  = "access"
	tokenFieldRefresh = "refresh"
)

var (
	errTokenKeyMissing = errors.New("the stored token is encrypted and no TOKEN_ENCRYPTION_KEY is set")
	errTokenUnreadable = errors.New("the stored token does not open with the configured TOKEN_ENCRYPTION_KEY")
)

// tokenBox seals and opens stored tokens. The zero value (and a
// nil *tokenBox) has no key: it stores tokens as they are and can
// only read ones that were never encrypted.
type tokenBox struct {
	aead   cipher.AEAD // v2 (HKDF): seals everything new
	legacy cipher.AEAD // v1 (single SHA-256): opens old rows until they re-seal
}

// newTokenBox builds the box for the configured key ("" for none).
func newTokenBox(secret string) (*tokenBox, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return &tokenBox{}, nil
	}
	if len(secret) < tokenKeyMinLength {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY is %d characters long; it needs at least %d random ones (for example: openssl rand -hex 32)", len(secret), tokenKeyMinLength)
	}
	// The salt and the label are part of the stored format: change
	// either and every v2 row stops opening (tokenV2KeyPinned in the
	// tests guards this).
	key, err := hkdf.Key(sha256.New, []byte(secret), []byte(tokenKDFSalt), tokenKDFInfo, 32)
	if err != nil {
		return nil, err
	}
	aead, err := gcmForKey(key)
	if err != nil {
		return nil, err
	}
	v1 := sha256.Sum256([]byte("evesynapse/token-encryption/v1\x00" + secret))
	legacy, err := gcmForKey(v1[:])
	if err != nil {
		return nil, err
	}
	return &tokenBox{aead: aead, legacy: legacy}, nil
}

func gcmForKey(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// The v2 key derivation inputs (HKDF-SHA256 from the standard
// library): fixed salt and label, so the same TOKEN_ENCRYPTION_KEY
// always derives the same AES-256 key.
const (
	tokenKDFSalt = "evesynapse token encryption salt"
	tokenKDFInfo = "evesynapse/token-encryption/v2"
)

// enabled reports whether a key is configured.
func (b *tokenBox) enabled() bool { return b != nil && b.aead != nil }

func tokenAAD(characterID int64, field string) []byte {
	return []byte(fmt.Sprintf("character:%d:%s", characterID, field))
}

func tokenIsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, tokenCipherPrefix) ||
		strings.HasPrefix(stored, tokenCipherV2Prefix)
}

// tokenSealedCurrent reports whether a stored value already wears
// the current envelope: only those skip the boot-time re-seal.
func tokenSealedCurrent(stored string) bool {
	return strings.HasPrefix(stored, tokenCipherV2Prefix)
}

// seal returns the form of a token to store: encrypted when a key
// is configured, unchanged otherwise. An empty token stays empty.
func (b *tokenBox) seal(plain string, characterID int64, field string) (string, error) {
	if !b.enabled() || plain == "" {
		return plain, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), tokenAAD(characterID, field))
	return tokenCipherV2Prefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// open returns the usable token from its stored form. A value
// that was never encrypted (stored before a key was set) is
// returned as it is; a v1 value opens with the old derivation.
func (b *tokenBox) open(stored string, characterID int64, field string) (string, error) {
	aead := b.aeadFor(stored)
	switch {
	case aead == nil && !tokenIsEncrypted(stored):
		return stored, nil
	case aead == nil:
		return "", errTokenKeyMissing
	}
	// Both envelopes prefix the same width ("enc:vN:").
	raw, err := base64.RawStdEncoding.DecodeString(stored[len(tokenCipherV2Prefix):])
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errTokenUnreadable
	}
	nonce, sealed := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, sealed, tokenAAD(characterID, field))
	if err != nil {
		return "", errTokenUnreadable
	}
	return string(plain), nil
}

// aeadFor selects the opener for a stored value by its envelope,
// or nil when there is no key to open with (or no envelope).
func (b *tokenBox) aeadFor(stored string) cipher.AEAD {
	if b == nil {
		return nil
	}
	switch {
	case strings.HasPrefix(stored, tokenCipherV2Prefix):
		return b.aead
	case strings.HasPrefix(stored, tokenCipherPrefix):
		return b.legacy
	default:
		return nil
	}
}

// sealTokens seals a character's token pair for storing.
func (b *tokenBox) sealTokens(characterID int64, access, refresh string) (sealedAccess, sealedRefresh string, err error) {
	if sealedAccess, err = b.seal(access, characterID, tokenFieldAccess); err != nil {
		return "", "", err
	}
	if sealedRefresh, err = b.seal(refresh, characterID, tokenFieldRefresh); err != nil {
		return "", "", err
	}
	return sealedAccess, sealedRefresh, nil
}

// openTokens opens a character row's stored token pair.
func (b *tokenBox) openTokens(ch db.Character) (access, refresh string, err error) {
	if access, err = b.open(ch.AccessToken, ch.CharacterID, tokenFieldAccess); err != nil {
		return "", "", err
	}
	if refresh, err = b.open(ch.RefreshToken, ch.CharacterID, tokenFieldRefresh); err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

// prepareStoredTokens runs once at boot, before the worker starts,
// and brings the stored tokens in line with the configured key.
//
// Every encrypted token must open with it. If one does not — the
// key was changed, or removed — the app refuses to start: running
// on would fail every sync, and every fresh sign-in would add
// tokens under a second key. The error says how to recover.
//
// With a key set, tokens stored before it was (plain text) are
// encrypted in place, and tokens under a retired envelope (v1)
// are re-sealed as v2, so switching encryption on — or the KDF
// under it — takes effect at the next start rather than a refresh
// at a time.
func (app *Application) prepareStoredTokens(ctx context.Context) error {
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		return fmt.Errorf("check stored tokens: %w", err)
	}

	var reseal []db.Character
	for _, ch := range characters {
		if _, _, err := app.tokens.openTokens(ch); err != nil {
			return fmt.Errorf("character %d: %w.\n"+
				"Put back the TOKEN_ENCRYPTION_KEY this database was last run with. If that key is gone for good, the stored tokens cannot be recovered: clear them with\n"+
				"  UPDATE characters SET access_token = '', refresh_token = '', link_state = 'token_dead';\n"+
				"and each character signs in once more to link again", ch.CharacterID, err)
		}
		if (ch.AccessToken != "" && !tokenSealedCurrent(ch.AccessToken)) ||
			(ch.RefreshToken != "" && !tokenSealedCurrent(ch.RefreshToken)) {
			reseal = append(reseal, ch)
		}
	}

	if !app.tokens.enabled() {
		unsealed := 0
		for _, ch := range reseal {
			if (ch.AccessToken != "" && !tokenIsEncrypted(ch.AccessToken)) ||
				(ch.RefreshToken != "" && !tokenIsEncrypted(ch.RefreshToken)) {
				unsealed++
			}
		}
		if unsealed > 0 {
			logging.Warnf("evesynapse: the EVE tokens of %d linked character(s) are stored unencrypted; set TOKEN_ENCRYPTION_KEY to encrypt them at rest (see the README)", unsealed)
		}
		return nil
	}

	for _, ch := range reseal {
		access, refresh, err := app.tokens.openTokens(ch)
		if err != nil {
			return err
		}
		sealedAccess, sealedRefresh, err := app.tokens.sealTokens(ch.CharacterID, access, refresh)
		if err != nil {
			return fmt.Errorf("character %d: encrypt stored tokens: %w", ch.CharacterID, err)
		}
		if err := app.queries.UpdateCharacterTokens(ctx, db.UpdateCharacterTokensParams{
			AccessToken:  sealedAccess,
			RefreshToken: sealedRefresh,
			TokenExpiry:  ch.TokenExpiry,
			CharacterID:  ch.CharacterID,
		}); err != nil {
			return fmt.Errorf("character %d: store encrypted tokens: %w", ch.CharacterID, err)
		}
	}
	if len(reseal) > 0 {
		logging.Infof("evesynapse: encrypted the stored EVE tokens of %d character(s) to the current format", len(reseal))
	}
	return nil
}
