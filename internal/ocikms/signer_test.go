package ocikms

// The signing golden test: a fixed key, request and instant must
// produce the exact Authorization header Oracle's signing
// specification defines. The expected value is rebuilt in the
// test from a literal signing string, so a drift in how the
// production code assembles that string fails here byte for byte.

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testSigningKey is a throwaway RSA key, generated once for this
// test and used nowhere else.
const testSigningKey = `-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQC50B1Oc0vuFjKr
b+//x+kN0DK17oI2srhcLvUarNR78ibmEcwjVMi5fOJQkZyeoF7CNUWPiPGXZDTw
TMgk4LxN+P8w51Vh1GSLnFvtXUogt7NK4mCTS8HsGdCpjBdjtkLBUN/hY38SpJCs
27Z3nFaEliPWPeLHeaxnnkIPv9ZHvxjH61SfrLjdLnLAVzOrTggdaEP4P3XQitWy
dLG8EH+7UKA1dXvm66dheW9rLhyLR6jREFhIwo3w3fmlTyvJ0cJPFMtqvN2HwaEU
79jW8dXUvjdXP+0c2+sMl29U4qbbs19tJC1aE6kBGH1q8W4k3rLZn3G8xv8cpvf9
tX2qrlvFAgMBAAECggEAFcipiuc29WZqUdF5ekvyTWGFdHFOGEILHZmOuC4fahmJ
SGs5OsInlyFmOepcSXeFLHEI4us74ZMIpsxloXcsdFq29kGEr8ex8vElkNYhSOUo
cqTIDDSzOy4Kmbn4CwNU36uO17JttR6QbIeEpJCyTvB4KpeORPMFjI9P8N89bKMt
0AyyZ+tL1Pm7NWzY4PekzKrqcFiG4/mD8jeuqTLfN6TrNbbXL9ajOZ3dUteEYhn1
6qOm6PIRAKUtnVcB1emi0Ej2u2GPDdIYes2JkhbU+o++wMPxRvQFJv/IR5ZMiaVR
e5jdoM2lFIcs5LIkxGpdomUa54RWBrwDDvkDcxbNcQKBgQD6fDoCSHQN9QuSIMeH
SIhOS8cDr/+Cdf0pLXMnBzPVgOFXruhELIn6jGEx8pwaTRei2yJErS68o028Gtlw
v4LepJHYdqNdaCpfr2PLeLFq+PK2sX37d9zsf8fBr1IGc+1LC4pJsfc7wKr4S9U+
+oFtfQycXUsoHRev0LIEnTNncQKBgQC952KBqJJlcZv6dyjoIAQ7b900nJ2AzVmB
x6YGuYJH75ygl5wBovsKm/dDr7xTNGe/xTcJJotdVA3+xE5svAJH5roO+Mm2Hwk9
Jt9/Ae1iNRumWQzaO59W8gEBqD6ZSyYknExKJBl3GZ56FrzGnbgsJ78L5VX0VWFk
P2/L2PgXlQKBgE17+jHUmdnrcI3eh/poTInz1PdmDSMdPpfrUFDFYJpuCMcn+v+D
y5bur+5Uf0d/i408PkiCTg/acGR85cCD7YbT363XXUe6vkjlJ3i8uB1AL0ymcTNF
69hY2sEma7hFSiVJx5hube1U905hnvCha0IkSa9hCKu6/CXN6BOXUVxxAoGBAKQ/
e4CfNzeMQpcn9vaFsoNh5lX523Qqp+7KqfuI+XiUcsinOGRl/XEYoo0lcuIYIl/I
CCje8IzhBVXDnf0Hw4VCtsv6PCRLxabhd06XcSSOwIJo5Offx5ngqCQxyyq0x6HE
mWtKu8e6HcqE3fN/OGrgWanFSkmQizK0d1I9L57dAoGBAMpaSyOaQfPSZ2E+ZnA9
+9aPoaRj/T/DSxxJWg1BtZVfWQKiDPSX4gLRMW7I/jZwVyHYsWZVfB9lPCy8kpJt
NY1KY9qnk+XcRkBvlRKeocQeeMdB36Hd+T34MTjokpis4F4iPTw/56iBJF+7rZkr
OstEK6KoJLzpNso2dS82m09U
-----END PRIVATE KEY-----
`

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := parseRSAPrivateKey([]byte(testSigningKey))
	if err != nil {
		t.Fatalf("parse the test key: %v", err)
	}
	return key
}

func TestSignRequestGolden(t *testing.T) {
	key := testKey(t)
	body := []byte(`{"keyId":"ocid1.key.oc1.phx.test","ciphertext":"aGVsbG8="}`)
	req, err := http.NewRequest(http.MethodPost,
		"https://efvnnezsaabpw-crypto.kms.us-phoenix-1.oraclecloud.com/20180608/decrypt",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	now := time.Date(2026, 10, 19, 22, 10, 44, 0, time.UTC)
	if err := signRequest(req, body, "ST$the-token", key, now); err != nil {
		t.Fatalf("sign: %v", err)
	}

	if got := req.Header.Get("Date"); got != "Mon, 19 Oct 2026 22:10:44 GMT" {
		t.Fatalf("Date header = %q", got)
	}
	bodySum := sha256.Sum256(body)
	wantContentSHA := base64.StdEncoding.EncodeToString(bodySum[:])
	if got := req.Header.Get("x-content-sha256"); got != wantContentSHA {
		t.Fatalf("x-content-sha256 = %q; want %q", got, wantContentSHA)
	}

	// The signing string, written out exactly as the spec builds
	// it; the signature must be the deterministic PKCS#1 v1.5
	// signature of its SHA-256.
	signingString := strings.Join([]string{
		"(request-target): post /20180608/decrypt",
		"host: efvnnezsaabpw-crypto.kms.us-phoenix-1.oraclecloud.com",
		"date: Mon, 19 Oct 2026 22:10:44 GMT",
		"content-length: " + strconv.Itoa(len(body)),
		"content-type: application/json",
		"x-content-sha256: " + wantContentSHA,
	}, "\n")
	digest := sha256.Sum256([]byte(signingString))

	auth := req.Header.Get("Authorization")
	sigText := strings.TrimPrefix(auth[strings.Index(auth, `signature="`)+len(`signature="`):], "")
	sigText = strings.TrimSuffix(sigText, `"`)
	sig, err := base64.StdEncoding.DecodeString(sigText)
	if err != nil {
		t.Fatalf("decode the signature out of %q: %v", auth, err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("the signature does not verify against the spec signing string: %v", err)
	}

	wantPrefix := `Signature version="1",keyId="ST$the-token",algorithm="rsa-sha256",headers="(request-target) host date content-length content-type x-content-sha256",signature="`
	if !strings.HasPrefix(auth, wantPrefix) {
		t.Fatalf("Authorization = %q; want prefix %q", auth, wantPrefix)
	}

	// PKCS#1 v1.5 is deterministic: signing the same request at
	// the same instant again yields the identical header.
	req2, _ := http.NewRequest(http.MethodPost, req.URL.String(), bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	if err := signRequest(req2, body, "ST$the-token", key, now); err != nil {
		t.Fatal(err)
	}
	if req2.Header.Get("Authorization") != auth {
		t.Fatal("signing is not deterministic for identical inputs")
	}
}

// Ensure the embedded test key really is the one whose PEM the
// parser accepts in both encodings it may meet.
func TestParseRSAPrivateKeyForms(t *testing.T) {
	key := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	again, err := parseRSAPrivateKey(pkcs1)
	if err != nil {
		t.Fatalf("PKCS#1 form: %v", err)
	}
	if again.N.Cmp(key.N) != 0 {
		t.Fatal("PKCS#1 form parsed to a different key")
	}
	if _, err := parseRSAPrivateKey([]byte("not pem at all")); err == nil {
		t.Fatal("garbage was accepted as a private key")
	}
}
