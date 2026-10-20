package ocikms

// Request signing per Oracle's HTTP signing specification (the
// "Signature" authorization scheme every OCI service API expects).
// Only what the KMS calls need is implemented: POSTs with a JSON
// body, signed over (request-target), host, date, content-length,
// content-type and x-content-sha256, under a keyId of
// "ST$<security token>" when the caller is an instance principal.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// signingHeaders is the signed-header list for a POST with a body,
// in the order the signing string is built.
var postSigningHeaders = []string{
	"(request-target)", "host", "date",
	"content-length", "content-type", "x-content-sha256",
}

// federationSigningHeaders is the signed-header list for the
// federation exchange. It differs from a normal service call: the
// federation endpoint does not sign the host header (Oracle's own
// client notes the same quirk), and date leads.
var federationSigningHeaders = []string{
	"date", "(request-target)",
	"content-length", "content-type", "x-content-sha256",
}

// getSigningHeaders is the signed-header list for a bodyless GET
// (the Secrets read): with no body there is nothing to length,
// type or hash, so the signature covers the target, the host and
// the date.
var getSigningHeaders = []string{
	"(request-target)", "host", "date",
}

// signRequest stamps req with the Date and x-content-sha256
// headers and an Authorization signature made with key under
// keyID. now is the signing instant (a parameter so tests can pin
// it). body is the exact request body; it must match what req
// sends.
func signRequest(req *http.Request, body []byte, keyID string, key *rsa.PrivateKey, now time.Time) error {
	return signRequestWithHeaders(req, body, keyID, key, now, postSigningHeaders)
}

// signRequestWithHeaders is signRequest over a caller-chosen
// signed-header list.
func signRequestWithHeaders(req *http.Request, body []byte, keyID string, key *rsa.PrivateKey, now time.Time, headers []string) error {
	date := now.UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)

	sum := sha256.Sum256(body)
	contentSHA := base64.StdEncoding.EncodeToString(sum[:])
	// The body hash header travels only with a request that has a
	// body. A bodyless GET carrying x-content-sha256 (the hash of
	// nothing) is rejected by the Vault Secrets service's
	// signature verification (HTTP 401 NotAuthenticated, proven
	// live 2026-10-19), while the same header on the KMS POSTs —
	// where it is also in the signed set — is required. Oracle's
	// SDK likewise hashes, and sends, only for body methods.
	if len(body) > 0 {
		req.Header.Set("x-content-sha256", contentSHA)
	}

	values := map[string]string{
		"(request-target)": strings.ToLower(req.Method) + " " + req.URL.EscapedPath(),
		"host":             req.URL.Host,
		"date":             date,
		"content-length":   fmt.Sprint(req.ContentLength),
		"content-type":     req.Header.Get("Content-Type"),
		"x-content-sha256": contentSHA,
	}
	var signing strings.Builder
	for i, name := range headers {
		if i > 0 {
			signing.WriteByte('\n')
		}
		signing.WriteString(name)
		signing.WriteString(": ")
		signing.WriteString(values[name])
	}

	digest := sha256.Sum256([]byte(signing.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return fmt.Errorf("sign the request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`Signature version="1",keyId=%q,algorithm="rsa-sha256",headers=%q,signature=%q`,
		keyID, strings.Join(headers, " "), base64.StdEncoding.EncodeToString(sig)))
	return nil
}
