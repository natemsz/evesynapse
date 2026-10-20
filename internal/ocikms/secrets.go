package ocikms

// Reading one secret's value out of OCI Secrets (the Vault
// service's secrets side): GET /20190301/secretbundles/{secretId}
// against https://secrets.vaults.<region>.oci.oraclecloud.com,
// authenticated exactly like the KMS calls — the instance
// principal's security token, spent with the ephemeral session
// key under the keyId "ST$<token>". The endpoint template, the
// path and the answer's shape are Oracle's Secrets API as its own
// SDK models them (secrets/secrets_client.go, models/secret_bundle.go).
//
// A secret's value is only ever returned to the caller: never
// logged, never written anywhere.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Secrets wire shape: the bundle carries its content typed
// and encoded; a secret created from text is BASE64 content whose
// decoding is the value.
type secretBundleResponse struct {
	SecretID            string `json:"secretId"`
	VersionNumber       int    `json:"versionNumber"`
	SecretBundleContent struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"secretBundleContent"`
}

// GetSecret reads the current version of one secret and returns
// its value. A bundle that does not carry BASE64 content, or
// whose content decodes to nothing, is refused rather than handed
// back as a credential that cannot work.
func (c *Client) GetSecret(ctx context.Context, secretID string) (string, error) {
	secretID = strings.TrimSpace(secretID)
	if secretID == "" {
		return "", errors.New("ocikms: the secret ID is not set")
	}
	if c.secretsBase == "" {
		return "", errors.New("ocikms: no secrets endpoint is configured (the region could not be read from the cryptographic endpoint and no secrets endpoint was set)")
	}
	var answer secretBundleResponse
	path := "/20190301/secretbundles/" + url.PathEscape(secretID)
	if err := c.callGet(ctx, path, &answer); err != nil {
		return "", err
	}
	content := answer.SecretBundleContent
	if content.ContentType != "BASE64" {
		return "", fmt.Errorf("ocikms: the secret bundle for %s carries content type %q; want BASE64", secretID, content.ContentType)
	}
	raw, err := base64.StdEncoding.DecodeString(content.Content)
	if err != nil {
		return "", fmt.Errorf("ocikms: the secret bundle for %s does not decode as base64: %w", secretID, err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("ocikms: the secret bundle for %s is empty", secretID)
	}
	return string(raw), nil
}

// callGet makes one signed GET against the Secrets service and
// decodes its JSON answer: call()'s sibling for a bodyless read.
func (c *Client) callGet(ctx context.Context, path string, answer any) error {
	token, sessionKey, err := c.securityToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.secretsBase+path, nil)
	if err != nil {
		return err
	}
	if err := signRequestWithHeaders(req, nil, "ST$"+token, sessionKey, time.Now(), getSigningHeaders); err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ocikms: call the secrets service %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ocikms: the secrets service %s answered HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(answer); err != nil {
		return fmt.Errorf("ocikms: read the secrets service %s answer: %w", path, err)
	}
	return nil
}
