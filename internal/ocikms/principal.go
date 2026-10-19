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
//     endpoint (POST /v1/x509/<tenancy OCID>); the tenancy is read
//     out of the certificate itself. The endpoint answers with a
//     short-lived security token (a JWT, good for about an hour).
//  3. API calls are signed with the instance key under the keyId
//     "ST$<token>" (see signer.go).
//
// No credential is ever stored: the certificate and key exist only
// in this process's memory, and the token is cached only until it
// nears expiry.

import (
	"bytes"
	"context"
	"crypto/rsa"
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
	for _, seq := range cert.Subject.ToRDNSequence() {
		for _, atv := range seq {
			if s, ok := atv.Value.(string); ok && strings.HasPrefix(s, "ocid1.tenancy.") {
				return s
			}
		}
	}
	return ""
}

// The federation endpoint's wire shapes: the certificate material
// travels as base64 of its PEM text; the answer is the token.
type federationRequest struct {
	Certificate              string   `json:"certificate"`
	IntermediateCertificates []string `json:"intermediateCertificates"`
	PublicKey                string   `json:"publicKey"`
}

type federationResponse struct {
	Token string `json:"token"`
}

// exchange presents id to the federation endpoint and returns the
// security token it issues.
func (f *federationClient) exchange(ctx context.Context, id *instanceIdentity) (string, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(&id.privateKey.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal the instance public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	b64 := base64.StdEncoding.EncodeToString
	payload := federationRequest{
		Certificate: b64(id.leafPEM),
		PublicKey:   b64(pubPEM),
	}
	for _, inter := range id.intermediates {
		payload.IntermediateCertificates = append(payload.IntermediateCertificates, b64(inter))
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(f.federationBase, "/") + "/v1/x509/" + id.tenancyOCID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call the federation endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("the federation endpoint answered HTTP %d", resp.StatusCode)
	}
	var answer federationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return "", fmt.Errorf("read the federation answer: %w", err)
	}
	if answer.Token == "" {
		return "", errors.New("the federation endpoint returned no token")
	}
	return answer.Token, nil
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
