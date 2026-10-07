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
// The stored form is "enc:v1:" + base64(nonce | ciphertext). Each
// value is bound to the character and the column it belongs to, so
// a ciphertext copied onto another row does not decrypt there.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// tokenCipherPrefix marks a stored token as encrypted. No EVE
// token starts this way (access tokens are JWTs, "eyJ…").
const tokenCipherPrefix = "enc:v1:"

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
	aead cipher.AEAD
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
	// The operator's value is stretched to an AES-256 key; the
	// label keeps this use of it apart from any other.
	key := sha256.Sum256([]byte("evesynapse/token-encryption/v1\x00" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &tokenBox{aead: aead}, nil
}

// enabled reports whether a key is configured.
func (b *tokenBox) enabled() bool { return b != nil && b.aead != nil }

func tokenAAD(characterID int64, field string) []byte {
	return []byte(fmt.Sprintf("character:%d:%s", characterID, field))
}

func tokenIsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, tokenCipherPrefix)
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
	return tokenCipherPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// open returns the usable token from its stored form. A value
// that was never encrypted (stored before a key was set) is
// returned as it is.
func (b *tokenBox) open(stored string, characterID int64, field string) (string, error) {
	if !tokenIsEncrypted(stored) {
		return stored, nil
	}
	if !b.enabled() {
		return "", errTokenKeyMissing
	}
	raw, err := base64.RawStdEncoding.DecodeString(stored[len(tokenCipherPrefix):])
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errTokenUnreadable
	}
	nonce, sealed := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, sealed, tokenAAD(characterID, field))
	if err != nil {
		return "", errTokenUnreadable
	}
	return string(plain), nil
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
// encrypted in place, so switching encryption on takes effect at
// the next start rather than a refresh at a time.
func (app *Application) prepareStoredTokens(ctx context.Context) error {
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		return fmt.Errorf("check stored tokens: %w", err)
	}

	var plain []db.Character
	for _, ch := range characters {
		if _, _, err := app.tokens.openTokens(ch); err != nil {
			return fmt.Errorf("character %d: %w.\n"+
				"Put back the TOKEN_ENCRYPTION_KEY this database was last run with. If that key is gone for good, the stored tokens cannot be recovered: clear them with\n"+
				"  UPDATE characters SET access_token = '', refresh_token = '', link_state = 'token_dead';\n"+
				"and each character signs in once more to link again", ch.CharacterID, err)
		}
		if (ch.AccessToken != "" && !tokenIsEncrypted(ch.AccessToken)) ||
			(ch.RefreshToken != "" && !tokenIsEncrypted(ch.RefreshToken)) {
			plain = append(plain, ch)
		}
	}

	if !app.tokens.enabled() {
		if len(plain) > 0 {
			logging.Warnf("evesynapse: the EVE tokens of %d linked character(s) are stored unencrypted; set TOKEN_ENCRYPTION_KEY to encrypt them at rest (see the README)", len(plain))
		}
		return nil
	}

	for _, ch := range plain {
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
	if len(plain) > 0 {
		logging.Infof("evesynapse: encrypted the stored EVE tokens of %d character(s)", len(plain))
	}
	return nil
}
