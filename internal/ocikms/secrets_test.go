package ocikms

// The Secrets read end to end against fakes: the same metadata
// and federation fakes as the KMS tests, plus a Secrets service
// fake that verifies the request the way the real service would —
// GET method, the /20190301/secretbundles/<id> path, and an
// Authorization signature that verifies under the session key the
// federation exchange issued, with the keyId "ST$<token>".

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testSecretID    = "ocid1.vaultsecret.oc1.phx.testsecret"
	testSecretValue = "fake-working-credential-value"
)

// fakeSecrets answers GetSecretBundle and verifies each request:
// the session-key signature over the GET header list, the exact
// path, and the ST$ keyId.
type fakeSecrets struct {
	sessionPub  *atomic.Value // *rsa.PublicKey, set by the fake federation
	contentType string
	content     string
	failWith    int
	calls       atomic.Int64
	lastPath    atomic.Value // string
	lastKeyID   atomic.Value // string
}

func (f *fakeSecrets) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.lastPath.Store(r.URL.Path)
		if r.Method != http.MethodGet {
			t.Errorf("secrets method = %q; want GET", r.Method)
		}
		if want := "/20190301/secretbundles/" + testSecretID; r.URL.Path != want {
			t.Errorf("secrets path = %q; want %q", r.URL.Path, want)
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, `Signature version="1",keyId="ST$`) {
			t.Errorf("Authorization = %q; want a Signature header with an ST$ keyId", auth)
			return
		}
		keyID := auth[len(`Signature version="1",keyId="`):]
		keyID = keyID[:strings.Index(keyID, `"`)]
		f.lastKeyID.Store(keyID)
		if headers := auth[strings.Index(auth, `headers="`)+len(`headers="`):]; !strings.HasPrefix(headers, `(request-target) host date"`) {
			t.Errorf("signed headers = %q; want the GET list (request-target) host date", headers)
		}
		signing := strings.Join([]string{
			"(request-target): get " + r.URL.Path,
			"host: " + r.Host,
			"date: " + r.Header.Get("Date"),
		}, "\n")
		sigText := auth[strings.Index(auth, `signature="`)+len(`signature="`):]
		sigText = strings.TrimSuffix(sigText, `"`)
		sig, err := base64.StdEncoding.DecodeString(sigText)
		if err != nil {
			t.Errorf("signature is not base64: %v", err)
			return
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
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(secretBundleResponse{
			SecretID:      testSecretID,
			VersionNumber: 3,
			SecretBundleContent: struct {
				ContentType string `json:"contentType"`
				Content     string `json:"content"`
			}{ContentType: f.contentType, Content: f.content},
		})
	}
}

type fakeSecretsStack struct {
	client  *Client
	secrets *fakeSecrets
	fed     *fakeFederation
}

func buildSecretsStack(t *testing.T) *fakeSecretsStack {
	t.Helper()
	fx := makeIdentityFixture(t)
	sessionPub := &atomic.Value{}
	md := &fakeMetadata{fx: fx}
	fed := &fakeFederation{fx: fx, sessionPub: sessionPub}
	secrets := &fakeSecrets{
		sessionPub:  sessionPub,
		contentType: "BASE64",
		content:     base64.StdEncoding.EncodeToString([]byte(testSecretValue)),
	}
	mdSrv := httptest.NewServer(md.handler(t))
	fedSrv := httptest.NewServer(fed.handler(t))
	secretsSrv := httptest.NewServer(secrets.handler(t))
	kmsSrv := httptest.NewServer((&fakeKMS{sessionPub: sessionPub}).handler(t))
	t.Cleanup(func() { mdSrv.Close(); fedSrv.Close(); secretsSrv.Close(); kmsSrv.Close() })
	client, err := NewClient(Config{
		CryptoEndpoint:    kmsSrv.URL,
		KeyID:             testKeyID,
		MetadataBaseURL:   mdSrv.URL,
		FederationBaseURL: fedSrv.URL,
		SecretsBaseURL:    secretsSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &fakeSecretsStack{client: client, secrets: secrets, fed: fed}
}

func TestGetSecretAgainstFakes(t *testing.T) {
	s := buildSecretsStack(t)
	value, err := s.client.GetSecret(context.Background(), testSecretID)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if value != testSecretValue {
		t.Fatalf("GetSecret = %q; want the fake's value", value)
	}
	if got, _ := s.secrets.lastPath.Load().(string); got != "/20190301/secretbundles/"+testSecretID {
		t.Fatalf("secrets path = %q", got)
	}
	if keyID, _ := s.secrets.lastKeyID.Load().(string); !strings.HasPrefix(keyID, "ST$") || strings.Count(keyID, ".") != 2 {
		t.Fatalf("signature keyId = %q; want ST$ plus the federation JWT", keyID)
	}
	// The security token is shared with the KMS calls: one
	// federation exchange covers the read.
	if got := s.fed.calls.Load(); got != 1 {
		t.Fatalf("federation calls = %d; want 1", got)
	}
}

func TestGetSecretRefusals(t *testing.T) {
	s := buildSecretsStack(t)
	ctx := context.Background()

	s.secrets.contentType = "TEXT"
	if _, err := s.client.GetSecret(ctx, testSecretID); err == nil ||
		!strings.Contains(err.Error(), "BASE64") {
		t.Fatalf("non-BASE64 content: err = %v", err)
	}
	s.secrets.contentType = "BASE64"

	s.secrets.content = ""
	if _, err := s.client.GetSecret(ctx, testSecretID); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty content: err = %v", err)
	}
	s.secrets.content = "!!! not base64 !!!"
	if _, err := s.client.GetSecret(ctx, testSecretID); err == nil ||
		!strings.Contains(err.Error(), "base64") {
		t.Fatalf("undecodable content: err = %v", err)
	}
	s.secrets.content = base64.StdEncoding.EncodeToString([]byte(testSecretValue))

	s.secrets.failWith = http.StatusInternalServerError
	if _, err := s.client.GetSecret(ctx, testSecretID); err == nil ||
		!strings.Contains(err.Error(), "500") {
		t.Fatalf("HTTP 500: err = %v; want the status named", err)
	}
	s.secrets.failWith = 0

	if _, err := s.client.GetSecret(ctx, "  "); err == nil {
		t.Fatal("an empty secret ID was accepted")
	}
}

func TestSecretsEndpointDefault(t *testing.T) {
	c, err := NewClient(Config{
		KeyID:          testKeyID,
		CryptoEndpoint: "https://efvnnezsaabpw-crypto.kms.us-phoenix-1.oraclecloud.com/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.secretsBase; got != "https://secrets.vaults.us-phoenix-1.oci.oraclecloud.com" {
		t.Fatalf("secrets default = %q; want the region's secrets.vaults host", got)
	}
	// An explicit base wins, trailing slash and all.
	c, err = NewClient(Config{
		KeyID:          testKeyID,
		CryptoEndpoint: "https://efvnnezsaabpw-crypto.kms.us-phoenix-1.oraclecloud.com/",
		SecretsBaseURL: "https://secrets.example.internal/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.secretsBase; got != "https://secrets.example.internal" {
		t.Fatalf("secrets override = %q", got)
	}
}
