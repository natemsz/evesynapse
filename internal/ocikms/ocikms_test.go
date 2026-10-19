package ocikms

// The client end to end against fakes of all three services it
// talks to: the metadata service (identity material), the
// federation endpoint (material in, token out), and KMS itself
// (signed calls). Nothing here touches the real metadata address
// or Oracle: every base URL points at an httptest server, and the
// KMS fake verifies the request signature the way the spec
// defines it.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testTenancy = "ocid1.tenancy.oc1..aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testKeyID   = "ocid1.key.oc1.phx.testkey"
	testSecret  = "the-root-secret-the-root-secret-0123456789"
)

// identityFixture is a fabricated instance: a key and the
// certificate + intermediates the metadata fake serves for it.
type identityFixture struct {
	key       *rsa.PrivateKey
	leafPEM   []byte
	keyPEM    []byte
	interPEM  []byte
	interCert *x509.Certificate
}

func makeIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(1653),
		Subject: pkix.Name{
			CommonName: "ocid1.instance.oc1.phx.test",
			OrganizationalUnit: []string{
				"opc-tenant:" + testTenancy,
				"opc-instance:ocid1.instance.oc1.phx.test",
				"opc-compartment:" + testTenancy,
				"opc-certtype:instance",
			},
		},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	inter := &x509.Certificate{
		SerialNumber: big.NewInt(1654),
		Subject:      pkix.Name{CommonName: "intermediate"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	interDER, err := x509.CreateCertificate(rand.Reader, inter, inter, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	interCert, err := x509.ParseCertificate(interDER)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &identityFixture{
		key:     key,
		leafPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		interPEM: pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: interDER,
		}),
		interCert: interCert,
	}
}

// fakeMetadata serves the fixture's identity material, and
// records the authorization header it was asked with.
type fakeMetadata struct {
	fx       *identityFixture
	lastAuth atomic.Value // string
}

func (f *fakeMetadata) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	serve := func(name string, doc []byte) {
		mux.HandleFunc("/opc/v2/identity/"+name, func(w http.ResponseWriter, r *http.Request) {
			f.lastAuth.Store(r.Header.Get("Authorization"))
			_, _ = w.Write(doc)
		})
	}
	serve("cert.pem", f.fx.leafPEM)
	serve("key.pem", f.fx.keyPEM)
	serve("intermediate.pem", f.fx.interPEM)
	return mux
}

// fakeFederation validates the exchange request against the real
// endpoint's protocol — path, DER-base64 material, the ephemeral
// session key, and the instance key's signature under the
// tenancy/fed-x509-sha256 keyId — and answers a token whose expiry
// the test controls per call. The session public key it captures
// is shared with the fake KMS, which verifies call signatures
// against it.
type fakeFederation struct {
	fx         *identityFixture
	sessionPub *atomic.Value // *rsa.PublicKey, captured from the exchange
	calls      atomic.Int64
	shortOnce  atomic.Bool // first token expires inside the refresh margin
}

func fakeToken(exp time.Time) string {
	b64 := base64.RawURLEncoding.EncodeToString
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := b64([]byte(fmt.Sprintf(`{"exp":%d,"sub":"instance"}`, exp.Unix())))
	return header + "." + payload + "." + b64([]byte("signature"))
}

func (f *fakeFederation) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call := f.calls.Add(1)
		if r.URL.Path != "/v1/x509" {
			t.Errorf("federation path = %q; want /v1/x509", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body federationRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("federation body: %v", err)
		}
		leafBlock, _ := pem.Decode(f.fx.leafPEM)
		if got, err := base64.StdEncoding.DecodeString(body.Certificate); err != nil || !bytes.Equal(got, leafBlock.Bytes) {
			t.Errorf("federation certificate field does not carry the leaf DER")
		}
		interBlock, _ := pem.Decode(f.fx.interPEM)
		if len(body.IntermediateCertificates) != 1 {
			t.Errorf("intermediates = %d; want 1", len(body.IntermediateCertificates))
		} else if got, err := base64.StdEncoding.DecodeString(body.IntermediateCertificates[0]); err != nil || !bytes.Equal(got, interBlock.Bytes) {
			t.Errorf("intermediate field does not carry the intermediate DER")
		}
		if body.FingerprintAlgorithm != "SHA256" {
			t.Errorf("fingerprintAlgorithm = %q; want SHA256", body.FingerprintAlgorithm)
		}
		pubDER, err := base64.StdEncoding.DecodeString(body.PublicKey)
		if err != nil {
			t.Errorf("public key field: %v", err)
		} else if pub, err := x509.ParsePKIXPublicKey(pubDER); err != nil {
			t.Errorf("public key field: %v", err)
		} else if sessionPub, ok := pub.(*rsa.PublicKey); !ok {
			t.Errorf("public key field is not RSA")
		} else {
			if sessionPub.N.Cmp(f.fx.key.PublicKey.N) == 0 {
				t.Errorf("public key field is the instance key; want an ephemeral session key")
			}
			f.sessionPub.Store(sessionPub)
		}
		// The exchange must be signed with the instance key under
		// the "<tenancy>/fed-x509-sha256/<fingerprint>" keyId, over
		// the federation header list (no host).
		leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
		if err != nil {
			t.Errorf("parse the fixture leaf: %v", err)
		}
		auth := r.Header.Get("Authorization")
		wantKeyID := testTenancy + "/fed-x509-sha256/" + certFingerprint(leafCert)
		if !strings.Contains(auth, `keyId="`+wantKeyID+`"`) {
			t.Errorf("federation Authorization = %q; want keyId %q", auth, wantKeyID)
		}
		signing := strings.Join([]string{
			"date: " + r.Header.Get("Date"),
			"(request-target): post /v1/x509",
			"content-length: " + strconv.Itoa(len(raw)),
			"content-type: " + r.Header.Get("Content-Type"),
			"x-content-sha256: " + r.Header.Get("x-content-sha256"),
		}, "\n")
		sigText := auth[strings.Index(auth, `signature="`)+len(`signature="`):]
		sigText = strings.TrimSuffix(sigText, `"`)
		if sig, err := base64.StdEncoding.DecodeString(sigText); err != nil {
			t.Errorf("federation signature is not base64: %v", err)
		} else {
			digest := sha256.Sum256([]byte(signing))
			if err := rsa.VerifyPKCS1v15(&f.fx.key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
				t.Errorf("federation signature does not verify under the instance key: %v", err)
			}
		}
		exp := time.Now().Add(time.Hour)
		if call == 1 && f.shortOnce.Load() {
			exp = time.Now().Add(30 * time.Second)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(federationResponse{Token: fakeToken(exp)})
	}
}

// fakeKMS answers encrypt/decrypt and verifies each request's
// signature against the session key the federation exchange
// captured, the way OCI would: API calls are spent with the
// ephemeral key, never the instance key.
type fakeKMS struct {
	sessionPub *atomic.Value // *rsa.PublicKey, set by the fake federation
	failWith   int
	lastAuth   atomic.Value // string
	lastDec    decryptRequest
	lastEnc    encryptRequest
	decryptN   atomic.Int64
	encryptTok atomic.Value // the token the last signature used
}

func (f *fakeKMS) verifySignature(t *testing.T, r *http.Request, body []byte) {
	auth := r.Header.Get("Authorization")
	f.lastAuth.Store(auth)
	if !strings.HasPrefix(auth, `Signature version="1",keyId="ST$`) {
		t.Errorf("Authorization = %q; want a Signature header with an ST$ keyId", auth)
		return
	}
	keyID := auth[len(`Signature version="1",keyId="`):]
	keyID = keyID[:strings.Index(keyID, `"`)]
	f.encryptTok.Store(strings.TrimPrefix(keyID, "ST$"))
	signing := strings.Join([]string{
		"(request-target): post " + r.URL.Path,
		"host: " + r.Host,
		"date: " + r.Header.Get("Date"),
		"content-length: " + strconv.FormatInt(r.ContentLength, 10),
		"content-type: " + r.Header.Get("Content-Type"),
		"x-content-sha256: " + r.Header.Get("x-content-sha256"),
	}, "\n")
	sigText := auth[strings.Index(auth, `signature="`)+len(`signature="`) : len(auth)-1]
	sig, err := base64.StdEncoding.DecodeString(sigText)
	if err != nil {
		t.Errorf("signature is not base64: %v", err)
		return
	}
	sum := sha256.Sum256(body)
	if got := base64.StdEncoding.EncodeToString(sum[:]); got != r.Header.Get("x-content-sha256") {
		t.Errorf("x-content-sha256 does not match the body")
	}
	pub, _ := f.sessionPub.Load().(*rsa.PublicKey)
	if pub == nil {
		t.Errorf("no session key was captured from a federation exchange")
		return
	}
	digest := sha256.Sum256([]byte(signing))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("request signature does not verify under the session key: %v", err)
	}
}

func (f *fakeKMS) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/20180608/decrypt", func(w http.ResponseWriter, r *http.Request) {
		f.decryptN.Add(1)
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.lastDec)
		f.verifySignature(t, r, body)
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(decryptResponse{
			Plaintext: base64.StdEncoding.EncodeToString([]byte(testSecret)),
		})
	})
	mux.HandleFunc("/20180608/encrypt", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.lastEnc)
		f.verifySignature(t, r, body)
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(encryptResponse{Ciphertext: "d3JhcHBlZC1ibG9i"})
	})
	return mux
}

type fakeStack struct {
	client *Client
	md     *fakeMetadata
	fed    *fakeFederation
	kms    *fakeKMS
}

func buildFakeStack(t *testing.T) *fakeStack {
	t.Helper()
	fx := makeIdentityFixture(t)
	sessionPub := &atomic.Value{}
	md := &fakeMetadata{fx: fx}
	fed := &fakeFederation{fx: fx, sessionPub: sessionPub}
	kms := &fakeKMS{sessionPub: sessionPub}
	mdSrv := httptest.NewServer(md.handler(t))
	fedSrv := httptest.NewServer(fed.handler(t))
	kmsSrv := httptest.NewServer(kms.handler(t))
	t.Cleanup(func() { mdSrv.Close(); fedSrv.Close(); kmsSrv.Close() })
	client, err := NewClient(Config{
		CryptoEndpoint:    kmsSrv.URL,
		KeyID:             testKeyID,
		MetadataBaseURL:   mdSrv.URL,
		FederationBaseURL: fedSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &fakeStack{client: client, md: md, fed: fed, kms: kms}
}

func TestDecryptRoundTripAgainstFakes(t *testing.T) {
	s := buildFakeStack(t)
	ctx := context.Background()

	secret, err := s.client.Decrypt(ctx, "dGhlLXdycHBlZC1ibG9i")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if secret != testSecret {
		t.Fatalf("Decrypt = %q; want the fake's secret", secret)
	}
	if got := s.md.lastAuth.Load(); got != "Bearer Oracle" {
		t.Fatalf("metadata authorization = %v; want Bearer Oracle", got)
	}
	if s.kms.lastDec.KeyID != testKeyID || s.kms.lastDec.Ciphertext != "dGhlLXdycHBlZC1ibG9i" {
		t.Fatalf("decrypt request = %+v", s.kms.lastDec)
	}
	if tok, _ := s.kms.encryptTok.Load().(string); tok == "" || strings.Count(tok, ".") != 2 {
		t.Fatalf("signature keyId token = %q; want the federation JWT", tok)
	}

	// The token is cached: a second call does not re-exchange.
	if _, err := s.client.Decrypt(ctx, "dGhlLXdycHBlZC1ibG9i"); err != nil {
		t.Fatal(err)
	}
	if got := s.fed.calls.Load(); got != 1 {
		t.Fatalf("federation calls = %d; want 1 (the token is cached)", got)
	}
	if got := s.kms.decryptN.Load(); got != 2 {
		t.Fatalf("decrypt calls = %d; want 2", got)
	}
}

func TestTokenRefreshWhenNearExpiry(t *testing.T) {
	s := buildFakeStack(t)
	s.fed.shortOnce.Store(true)
	ctx := context.Background()
	if _, err := s.client.Decrypt(ctx, "eA=="); err != nil {
		t.Fatal(err)
	}
	// The first token expired inside the refresh margin, so the
	// second call exchanges again — and its long-lived answer is
	// then cached.
	if _, err := s.client.Decrypt(ctx, "eA=="); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Decrypt(ctx, "eA=="); err != nil {
		t.Fatal(err)
	}
	if got := s.fed.calls.Load(); got != 2 {
		t.Fatalf("federation calls = %d; want 2", got)
	}
}

func TestEncryptCallShape(t *testing.T) {
	s := buildFakeStack(t)
	wrapped, err := s.client.Encrypt(context.Background(), testSecret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if wrapped != "d3JhcHBlZC1ibG9i" {
		t.Fatalf("Encrypt = %q; want the fake's ciphertext", wrapped)
	}
	if s.kms.lastEnc.KeyID != testKeyID {
		t.Fatalf("encrypt keyId = %q", s.kms.lastEnc.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(s.kms.lastEnc.Plaintext)
	if err != nil || string(raw) != testSecret {
		t.Fatalf("encrypt plaintext field = %q (decoded %q, %v); want base64 of the secret", s.kms.lastEnc.Plaintext, raw, err)
	}
}

func TestKMSErrorSurfaces(t *testing.T) {
	s := buildFakeStack(t)
	s.kms.failWith = http.StatusUnauthorized
	_, err := s.client.Decrypt(context.Background(), "eA==")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Decrypt error = %v; want the HTTP status named", err)
	}
}

func TestNewClientValidationAndDefaults(t *testing.T) {
	if _, err := NewClient(Config{CryptoEndpoint: "https://x-crypto.kms.us-phoenix-1.oraclecloud.com"}); err == nil {
		t.Fatal("a missing key ID was accepted")
	}
	if _, err := NewClient(Config{KeyID: testKeyID, CryptoEndpoint: "not a url"}); err == nil {
		t.Fatal("an unusable endpoint was accepted")
	}
	// The federation default derives from the endpoint's region.
	c, err := NewClient(Config{
		KeyID:          testKeyID,
		CryptoEndpoint: "https://efvnnezsaabpw-crypto.kms.us-phoenix-1.oraclecloud.com/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.federation.federationBase; got != "https://auth.us-phoenix-1.oraclecloud.com" {
		t.Fatalf("federation default = %q", got)
	}
	if got := c.federation.metadataBase; got != defaultMetadataBase {
		t.Fatalf("metadata default = %q", got)
	}
	// A host whose region cannot be read needs the explicit
	// federation endpoint.
	if _, err := NewClient(Config{KeyID: testKeyID, CryptoEndpoint: "https://kms.example.internal"}); err == nil {
		t.Fatal("an endpoint with no readable region was accepted without a federation override")
	}
}

// The tenancy extraction reads the certificate shape the metadata
// service actually serves: the tenancy rides in an "opc-tenant:"
// OU value, not as a bare OCID.
func TestTenancyOCIDFromCertShapes(t *testing.T) {
	mk := func(ous ...string) *x509.Certificate {
		return &x509.Certificate{Subject: pkix.Name{OrganizationalUnit: ous}}
	}
	if got := tenancyOCIDFromCert(mk("opc-tenant:"+testTenancy, "opc-instance:ocid1.instance.oc1.phx.test")); got != testTenancy {
		t.Errorf("opc-tenant shape: got %q, want %q", got, testTenancy)
	}
	if got := tenancyOCIDFromCert(mk(testTenancy)); got != testTenancy {
		t.Errorf("bare shape: got %q, want %q", got, testTenancy)
	}
	if got := tenancyOCIDFromCert(mk("opc-instance:ocid1.instance.oc1.phx.test")); got != "" {
		t.Errorf("no tenancy: got %q, want empty", got)
	}
}
