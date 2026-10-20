// Package ocikms is the smallest honest client for OCI Key
// Management's Encrypt/Decrypt calls, authenticated by instance
// principal. It exists so EveSynapse can hold its token encryption
// key wrapped by a key that lives in an OCI Vault (HSM-backed,
// non-exportable) while no usable key material — and no cloud
// credential at all — sits on the machine: the app's identity is
// the instance's own, minted by the metadata service and good for
// about an hour at a time (see principal.go).
//
// The implementation is standard library only. It covers exactly
// the two KMS operations the token key needs, plus one Secrets
// read (secrets.go) the working credentials are fetched with.
package ocikms

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config names a vault key and the endpoints the client talks to.
// CryptoEndpoint and KeyID are the deployment's values (the
// vault's cryptographic endpoint and the key's OCID). The two
// base URLs default to the real services; they are configurable
// so tests — and only tests, in practice — can point the client
// at fakes.
type Config struct {
	CryptoEndpoint    string
	KeyID             string
	MetadataBaseURL   string // default http://169.254.169.254
	FederationBaseURL string // default https://auth.<region>.oraclecloud.com, the region read from CryptoEndpoint
	SecretsBaseURL    string // default https://secrets.vaults.<region>.oci.oraclecloud.com, the region read from CryptoEndpoint
	HTTPClient        *http.Client
}

const defaultMetadataBase = "http://169.254.169.254"

// Client is a KMS Encrypt/Decrypt caller. It caches the instance
// identity and the current security token, refreshing the token
// when it nears expiry; the identity itself is re-read only if a
// token exchange fails with it.
type Client struct {
	cfg         Config
	httpClient  *http.Client
	federation  *federationClient
	secretsBase string // the Secrets service base URL; "" when no region could be read and none was configured

	mu         sync.Mutex
	identity   *instanceIdentity
	token      string
	tokenExp   time.Time
	sessionKey *rsa.PrivateKey // the ephemeral key the live token is bound to
}

// NewClient settles the configuration's defaults and checks the
// pieces every call needs.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.KeyID) == "" {
		return nil, errors.New("ocikms: the key ID is not set")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.CryptoEndpoint), "/")
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("ocikms: the cryptographic endpoint %q is not a usable URL", cfg.CryptoEndpoint)
	}
	cfg.CryptoEndpoint = endpoint
	if strings.TrimSpace(cfg.MetadataBaseURL) == "" {
		cfg.MetadataBaseURL = defaultMetadataBase
	}
	region := regionFromCryptoEndpoint(u.Host)
	if strings.TrimSpace(cfg.FederationBaseURL) == "" {
		if region == "" {
			return nil, fmt.Errorf("ocikms: cannot read the region from the cryptographic endpoint %q; set the federation endpoint explicitly", cfg.CryptoEndpoint)
		}
		cfg.FederationBaseURL = "https://auth." + region + ".oraclecloud.com"
	}
	secretsBase := strings.TrimRight(strings.TrimSpace(cfg.SecretsBaseURL), "/")
	if secretsBase == "" && region != "" {
		secretsBase = "https://secrets.vaults." + region + ".oci.oraclecloud.com"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		cfg:         cfg,
		httpClient:  httpClient,
		secretsBase: secretsBase,
		federation: &federationClient{
			httpClient:     httpClient,
			metadataBase:   cfg.MetadataBaseURL,
			federationBase: cfg.FederationBaseURL,
		},
	}, nil
}

// regionFromCryptoEndpoint reads the region label out of a KMS
// cryptographic endpoint host, which is shaped
// <vault>-crypto.kms.<region>.oraclecloud.com.
func regionFromCryptoEndpoint(host string) string {
	labels := strings.Split(host, ".")
	if len(labels) >= 4 && labels[1] == "kms" {
		return labels[len(labels)-3]
	}
	return ""
}

// tokenRefreshMargin is how long before its expiry a security
// token stops being used, so a call never starts on a token that
// dies mid-flight.
const tokenRefreshMargin = 2 * time.Minute

// securityToken returns a live security token and the session
// key it is bound to, fetching the instance identity and
// exchanging it when there is no cached token still comfortably
// valid.
func (c *Client) securityToken(ctx context.Context) (string, *rsa.PrivateKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExp) > tokenRefreshMargin {
		return c.token, c.sessionKey, nil
	}
	if c.identity == nil {
		id, err := c.federation.fetchIdentity(ctx)
		if err != nil {
			return "", nil, err
		}
		c.identity = id
	}
	token, sessionKey, err := c.federation.exchange(ctx, c.identity)
	if err != nil {
		// The identity material may have rotated under us; drop
		// it so the next attempt re-reads it.
		c.identity = nil
		return "", nil, err
	}
	c.token = token
	c.sessionKey = sessionKey
	c.tokenExp = tokenExpiry(token, time.Now())
	return token, sessionKey, nil
}

// The KMS wire shapes: plaintext and ciphertext both travel as
// base64, as the KMS API defines them.
type encryptRequest struct {
	KeyID     string `json:"keyId"`
	Plaintext string `json:"plaintext"`
}

type encryptResponse struct {
	Ciphertext string `json:"ciphertext"`
}

type decryptRequest struct {
	KeyID      string `json:"keyId"`
	Ciphertext string `json:"ciphertext"`
}

type decryptResponse struct {
	Plaintext string `json:"plaintext"`
}

// Encrypt asks the vault key to wrap plaintext and returns the
// base64 ciphertext.
func (c *Client) Encrypt(ctx context.Context, plaintext string) (string, error) {
	var answer encryptResponse
	body := encryptRequest{
		KeyID:     c.cfg.KeyID,
		Plaintext: base64.StdEncoding.EncodeToString([]byte(plaintext)),
	}
	if err := c.call(ctx, "/20180608/encrypt", body, &answer); err != nil {
		return "", err
	}
	if answer.Ciphertext == "" {
		return "", errors.New("ocikms: the encrypt call returned no ciphertext")
	}
	return answer.Ciphertext, nil
}

// Decrypt asks the vault key to unwrap a base64 ciphertext and
// returns the plaintext it held.
func (c *Client) Decrypt(ctx context.Context, ciphertext string) (string, error) {
	var answer decryptResponse
	body := decryptRequest{KeyID: c.cfg.KeyID, Ciphertext: ciphertext}
	if err := c.call(ctx, "/20180608/decrypt", body, &answer); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(answer.Plaintext)
	if err != nil {
		return "", fmt.Errorf("ocikms: the decrypt answer's plaintext is not base64: %w", err)
	}
	return string(raw), nil
}

// call makes one signed KMS request and decodes its JSON answer.
func (c *Client) call(ctx context.Context, path string, payload, answer any) error {
	token, sessionKey, err := c.securityToken(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.CryptoEndpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := signRequest(req, body, "ST$"+token, sessionKey, time.Now()); err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ocikms: call KMS %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ocikms: KMS %s answered HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(answer); err != nil {
		return fmt.Errorf("ocikms: read the KMS %s answer: %w", path, err)
	}
	return nil
}
