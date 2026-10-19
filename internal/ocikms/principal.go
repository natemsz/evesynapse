package ocikms

// Instance-principal authentication, by hand, on the standard
// library alone. The flow (Oracle's federation protocol for
// instance principals):
//
//  1. The instance's identity material — leaf certificate, private
//     key, intermediate certificates — comes from the link-local
//     metadata service (169.254.169.254/opc/v2/identity/), which
//     only processes on the instance itself can read.
//  2. The leaf certificate is presented to the region's federation
//     endpoint (POST /v1/x509), signed with the instance key under
//     a keyId of "<tenancy>/fed-x509-sha256/<leaf fingerprint>";
//     the tenancy is read out of the certificate itself. The
//     request carries a fresh ephemeral session public key, and
//     the endpoint answers with a short-lived security token (a
//     JWT, good for about an hour) bound to that session key.
//  3. API calls are signed with the session key under the keyId
//     "ST$<token>" (see signer.go). The instance key itself never
//     signs a service call.
//
// No credential is ever stored: the certificate and key exist only
// in this process's memory, and the token is cached only until it
// nears expiry.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// instanceIdentity is the metadata service's answer: who this
// instance is, in a form the federation endpoint accepts.
type instanceIdentity struct {
	leafCert      *x509.Certificate
	leafPEM       []byte
	privateKey    *rsa.PrivateKey
	intermediates [][]byte // PEM blocks, as served
	tenancyOCID   string
}

// federationClient exchanges instance identities for security
// tokens against one federation endpoint.
type federationClient struct {
	httpClient     *http.Client
	metadataBase   string
	federationBase string
}

// fetchIdentity reads the instance's certificate, key and
// intermediates from the metadata service.
func (f *federationClient) fetchIdentity(ctx context.Context) (*instanceIdentity, error) {
	certPEM, err := f.metadata(ctx, "cert.pem")
	if err != nil {
		return nil, err
	}
	keyPEM, err := f.metadata(ctx, "key.pem")
	if err != nil {
		return nil, err
	}
	interPEM, err := f.metadata(ctx, "intermediate.pem")
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the metadata service's cert.pem is not a PEM certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the instance certificate: %w", err)
	}
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	tenancy := tenancyOCIDFromCert(leaf)
	if tenancy == "" {
		return nil, errors.New("the instance certificate names no tenancy")
	}
	return &instanceIdentity{
		leafCert:      leaf,
		leafPEM:       certPEM,
		privateKey:    key,
		intermediates: splitPEMBlocks(interPEM),
		tenancyOCID:   tenancy,
	}, nil
}

// metadata GETs one identity document. The /opc/v2/ tree asks for
// the "Bearer Oracle" authorization header.
func (f *federationClient) metadata(ctx context.Context, name string) ([]byte, error) {
	url := strings.TrimRight(f.metadataBase, "/") + "/opc/v2/identity/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer Oracle")
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read the instance %s from the metadata service: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("the metadata service answered HTTP %d for %s", resp.StatusCode, name)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// parseRSAPrivateKey accepts the PKCS#1 and PKCS#8 forms the
// metadata service has used for the instance key.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the metadata service's key.pem is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the instance private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the instance private key is not RSA")
	}
	return key, nil
}

// splitPEMBlocks returns the PEM blocks of a possibly-chained
// document, each in its original encoded form.
func splitPEMBlocks(doc []byte) [][]byte {
	var blocks [][]byte
	for {
		var block *pem.Block
		block, doc = pem.Decode(doc)
		if block == nil {
			return blocks
		}
		blocks = append(blocks, pem.EncodeToMemory(block))
	}
}

// tenancyOCIDFromCert finds the tenancy OCID in the leaf
// certificate's subject, where the metadata service stamps it as
// a distinguished-name value.
func tenancyOCIDFromCert(cert *x509.Certificate) string {
	// The instance certificate carries the tenancy in an OU value
	// of the form "opc-tenant:<tenancy OCID>"; that RDN wins. A
	// bare tenancy OCID in any RDN is the fallback.
	fallback := ""
	for _, seq := range cert.Subject.ToRDNSequence() {
		for _, atv := range seq {
			s, ok := atv.Value.(string)
			if !ok {
				continue
			}
			if rest, found := strings.CutPrefix(s, "opc-tenant:"); found {
				return rest
			}
			if strings.HasPrefix(s, "ocid1.tenancy.") {
				fallback = s
			}
		}
	}
	return fallback
}

// The federation endpoint's wire shapes: the certificate material
// travels as the base64 of its DER bytes — a PEM with its header,
// footer and line breaks removed — and the answer is the token.
type federationRequest struct {
	Certificate              string   `json:"certificate"`
	IntermediateCertificates []string `json:"intermediateCertificates"`
	PublicKey                string   `json:"publicKey"`
	FingerprintAlgorithm     string   `json:"fingerprintAlgorithm"`
}

type federationResponse struct {
	Token string `json:"token"`
}

// pemToBase64DER reduces a PEM document to the base64 of the DER
// bytes of its first block: the encoding the federation endpoint
// expects for certificates and public keys alike.
func pemToBase64DER(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", errors.New("not a PEM document")
	}
	return base64.StdEncoding.EncodeToString(block.Bytes), nil
}

// certFingerprint is the leaf certificate's SHA-256 fingerprint
// in OCI's colon-separated hex form; it names the signing key in
// the federation request's keyId.
func certFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return strings.ReplaceAll(fmt.Sprintf("% x", sum), " ", ":")
}

// exchange presents id to the federation endpoint and returns the
// security token it issues together with the ephemeral session
// key the token is bound to. The request is signed with the
// instance key; the token it buys is spent with the session key.
func (f *federationClient) exchange(ctx context.Context, id *instanceIdentity) (string, *rsa.PrivateKey, error) {
	sessionKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", nil, fmt.Errorf("generate the session key: %w", err)
	}
	sessionPubDER, err := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	if err != nil {
		return "", nil, fmt.Errorf("marshal the session public key: %w", err)
	}
	payload := federationRequest{
		Certificate:          base64.StdEncoding.EncodeToString(id.leafCert.Raw),
		PublicKey:            base64.StdEncoding.EncodeToString(sessionPubDER),
		FingerprintAlgorithm: "SHA256",
	}
	for _, inter := range id.intermediates {
		b64, err := pemToBase64DER(inter)
		if err != nil {
			return "", nil, fmt.Errorf("encode an intermediate certificate: %w", err)
		}
		payload.IntermediateCertificates = append(payload.IntermediateCertificates, b64)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	url := strings.TrimRight(f.federationBase, "/") + "/v1/x509"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	keyID := id.tenancyOCID + "/fed-x509-sha256/" + certFingerprint(id.leafCert)
	if err := signRequestWithHeaders(req, body, keyID, id.privateKey, time.Now(), federationSigningHeaders); err != nil {
		return "", nil, err
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("call the federation endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("the federation endpoint answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var answer federationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return "", nil, fmt.Errorf("read the federation answer: %w", err)
	}
	if answer.Token == "" {
		return "", nil, errors.New("the federation endpoint returned no token")
	}
	return answer.Token, sessionKey, nil
}

// tokenExpiry reads the exp claim out of a security token (a JWT
// this process just received over TLS from the federation
// endpoint; the signature is not re-verified). A token whose
// expiry cannot be read is treated as already expired, so it is
// never cached.
func tokenExpiry(token string, now time.Time) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return now
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return now
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return now
	}
	return time.Unix(claims.Exp, 0)
}
