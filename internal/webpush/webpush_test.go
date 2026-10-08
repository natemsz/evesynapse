package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64url %q: %v", s, err)
	}
	return b
}

// The worked example in RFC 8291, appendix A.
const (
	rfcPlaintext     = "When I grow up, I want to be a watermelon"
	rfcSenderPrivate = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcSenderPublic  = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	rfcBrowserPriv   = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcBrowserPublic = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcAuthSecret    = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcSalt          = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcBody          = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

// decrypt is the browser's side: what a push service's client does
// with a message. It exists only to check Encrypt against.
func decrypt(t *testing.T, browser *ecdh.PrivateKey, auth, body []byte) ([]byte, error) {
	t.Helper()
	if len(body) < 21 || int(body[20]) != 65 || len(body) < 21+65+16 {
		t.Fatalf("body is %d bytes with a key length of %d; not an aes128gcm push message", len(body), body[20])
	}
	salt, senderPublic, ciphertext := body[:16], body[21:86], body[86:]
	sender, err := ecdh.P256().NewPublicKey(senderPublic)
	if err != nil {
		return nil, err
	}
	shared, err := browser.ECDH(sender)
	if err != nil {
		return nil, err
	}
	prkKey, _ := hkdf.Extract(sha256.New, shared, auth)
	keyInfo := append(append([]byte("WebPush: info\x00"), browser.PublicKey().Bytes()...), senderPublic...)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	record, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	if len(record) == 0 || record[len(record)-1] != 0x02 {
		t.Fatalf("record does not end with the last-record mark: % x", record)
	}
	return record[:len(record)-1], nil
}

// TestEncryptMatchesRFC8291 reproduces the RFC's example byte for
// byte: same keys, same salt, same output. Nothing weaker would show
// that a real browser can read what this sends.
func TestEncryptMatchesRFC8291(t *testing.T) {
	sender, err := ecdh.P256().NewPrivateKey(b64(t, rfcSenderPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(sender.PublicKey().Bytes()); got != rfcSenderPublic {
		t.Fatalf("the example's sender key pair does not match itself: public %s", got)
	}
	sub, err := ParseSubscription("https://fcm.googleapis.com/fcm/send/example", rfcBrowserPublic, rfcAuthSecret)
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	body, err := encrypt(sub, []byte(rfcPlaintext), sender, b64(t, rfcSalt))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(body); got != rfcBody {
		t.Fatalf("encrypted body differs from RFC 8291 appendix A:\n got %s\nwant %s", got, rfcBody)
	}
	// And the example's browser key reads it back.
	browser, err := ecdh.P256().NewPrivateKey(b64(t, rfcBrowserPriv))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decrypt(t, browser, b64(t, rfcAuthSecret), body)
	if err != nil || string(plain) != rfcPlaintext {
		t.Fatalf("the example's browser key read %q, %v", plain, err)
	}
}

// newBrowser makes a subscription the way a browser would, at the
// given push service address.
func newBrowser(t *testing.T, endpoint string) (Subscription, *ecdh.PrivateKey) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	sub, err := ParseSubscription(endpoint,
		base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(auth))
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	return sub, key
}

func TestEncryptIsReadableOnlyByItsBrowser(t *testing.T) {
	sub, browser := newBrowser(t, "https://updates.push.services.mozilla.com/wpush/v2/abc")
	_, stranger := newBrowser(t, "https://updates.push.services.mozilla.com/wpush/v2/def")
	message := []byte(`{"title":"Fixture Ceo finished training Mechanics V"}`)

	first, err := Encrypt(sub, message)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Encrypt(sub, message)
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of one message are identical: the key or salt is not fresh")
	}
	if plain, err := decrypt(t, browser, sub.Auth, first); err != nil || !bytes.Equal(plain, message) {
		t.Fatalf("the browser read %q, %v", plain, err)
	}
	if _, err := decrypt(t, stranger, sub.Auth, first); err == nil {
		t.Fatal("another browser's key decrypted the message")
	}
	wrongAuth := bytes.Repeat([]byte{7}, 16)
	if _, err := decrypt(t, browser, wrongAuth, first); err == nil {
		t.Fatal("the message decrypted without the subscription's secret")
	}
	tampered := bytes.Clone(first)
	tampered[len(tampered)-1] ^= 1
	if _, err := decrypt(t, browser, sub.Auth, tampered); err == nil {
		t.Fatal("a changed message still decrypted")
	}
	if _, err := Encrypt(sub, make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("a message over the size a push carries was encrypted")
	}
}

func TestAllowedEndpoint(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":                true,
		"https://updates.push.services.mozilla.com/wpush/v2/abc": true,
		"https://wns2-by3p.notify.windows.com/w/?token=abc":      true,
		"https://web.push.apple.com/abc":                         true,
		"https://FCM.googleapis.com/fcm/send/abc":                true,
		"https://fcm.googleapis.com:443/fcm/send/abc":            true,

		"http://fcm.googleapis.com/fcm/send/abc":       false, // not https
		"https://fcm.googleapis.com:8443/x":            false,
		"https://user@fcm.googleapis.com/x":            false,
		"https://fcm.googleapis.com.evil.example/x":    false,
		"https://evilfcm.googleapis.com/x":             false,
		"https://notify.windows.com.evil.example/x":    false,
		"https://evilnotify.windows.com/x":             false,
		"https://127.0.0.1/x":                          false,
		"https://localhost/x":                          false,
		"https://169.254.169.254/latest/meta-data/":    false,
		"https://internal.service.local/x":             false,
		"file:///etc/passwd":                           false,
		"//fcm.googleapis.com/x":                       false,
		"":                                             false,
		"https://fcm.googleapis.com@evil.example/x":    false,
		"https://evil.example/?h=fcm.googleapis.com":   false,
		"https://evil.example/#.push.apple.com":        false,
		"https://evil.example\\.push.apple.com/x":      false,
		"https://push.apple.com.evil.example/x":        false,
		"https://xpush.apple.com/x":                    false,
		"https://x.push.apple.com/x":                   true,
		"https://x.notify.windows.com/x":               true,
		"https://updates.push.services.mozilla.com:80": false,
	} {
		if got := AllowedEndpoint(raw); got != want {
			t.Errorf("AllowedEndpoint(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseSubscriptionRefusals(t *testing.T) {
	good, _ := newBrowser(t, "https://fcm.googleapis.com/fcm/send/abc")
	key := base64.RawURLEncoding.EncodeToString(good.P256dh)
	auth := base64.RawURLEncoding.EncodeToString(good.Auth)
	// Padded base64 is accepted: some clients send it.
	if _, err := ParseSubscription(good.Endpoint, key+"=", auth+"=="); err != nil {
		t.Errorf("padded keys refused: %v", err)
	}
	for name, args := range map[string][3]string{
		"an address that is not a push service": {"https://evil.example/x", key, auth},
		"a key that is not base64":              {good.Endpoint, "!!!", auth},
		"a key that is not a curve point":       {good.Endpoint, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 65)), auth},
		"a key of the wrong length":             {good.Endpoint, base64.RawURLEncoding.EncodeToString(good.P256dh[:33]), auth},
		"a secret of the wrong length":          {good.Endpoint, key, base64.RawURLEncoding.EncodeToString(good.Auth[:8])},
		"no secret":                             {good.Endpoint, key, ""},
	} {
		if _, err := ParseSubscription(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVAPID(t *testing.T) {
	pub, priv, err := GenerateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	v, err := ParseVAPID(pub, priv, "mailto:ops@example.org")
	if err != nil {
		t.Fatalf("ParseVAPID of a generated pair: %v", err)
	}
	otherPub, otherPriv, _ := GenerateVAPID()
	for name, args := range map[string][3]string{
		"halves of two different pairs": {otherPub, priv, "mailto:ops@example.org"},
		"a private key that is not one": {pub, "AAAA", "mailto:ops@example.org"},
		"no subject":                    {pub, priv, ""},
		"a subject that is no address":  {pub, priv, "ops@example.org"},
		"swapped halves":                {priv, pub, "mailto:ops@example.org"},
	} {
		if _, err := ParseVAPID(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = otherPriv

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	header, err := v.Authorization("https://fcm.googleapis.com/fcm/send/abc?x=1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(header, "vapid t=") || !strings.HasSuffix(header, ", k="+pub) {
		t.Fatalf("header %q, want vapid t=<token>, k=<public key>", header)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(header, "vapid t="), ", k="+pub)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	var head struct{ Typ, Alg string }
	var claims struct {
		Aud, Sub string
		Exp      int64
	}
	if json.Unmarshal(b64(t, parts[0]), &head) != nil || head.Typ != "JWT" || head.Alg != "ES256" {
		t.Fatalf("token header %s", b64(t, parts[0]))
	}
	if json.Unmarshal(b64(t, parts[1]), &claims) != nil {
		t.Fatalf("token claims %s", b64(t, parts[1]))
	}
	// The token is for that push service's origin alone, names who
	// sends, and expires within the day a push service allows.
	if claims.Aud != "https://fcm.googleapis.com" || claims.Sub != "mailto:ops@example.org" {
		t.Fatalf("claims %+v", claims)
	}
	if life := time.Unix(claims.Exp, 0).Sub(now); life <= 0 || life > 24*time.Hour {
		t.Fatalf("token lives %s, want more than nothing and at most 24h", life)
	}
	// And it is signed by the key whose public half it carries.
	sig := b64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (r and s)", len(sig))
	}
	pubKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pubKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the token's signature does not verify against the public key")
	}
	otherKey, _ := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, otherPub))
	if ecdsa.Verify(otherKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the token verifies against a different key")
	}
}

// pushService stands in for a browser's push service: it records the
// one request it is sent and answers with a chosen status.
type pushService struct {
	status int
	req    *http.Request
	body   []byte
}

func (p *pushService) RoundTrip(req *http.Request) (*http.Response, error) {
	p.req = req
	p.body, _ = io.ReadAll(req.Body)
	return &http.Response{StatusCode: p.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestSend(t *testing.T) {
	pub, priv, _ := GenerateVAPID()
	v, err := ParseVAPID(pub, priv, "mailto:ops@example.org")
	if err != nil {
		t.Fatal(err)
	}
	sub, browser := newBrowser(t, "https://fcm.googleapis.com/fcm/send/abc")
	message := []byte(`{"title":"hello"}`)

	service := &pushService{status: http.StatusCreated}
	res, err := Send(context.Background(), &http.Client{Transport: service}, v, sub, message, 12*time.Hour)
	if err != nil || !res.OK() || res.Gone {
		t.Fatalf("Send = %+v, %v; want accepted", res, err)
	}
	if service.req.Method != http.MethodPost || service.req.URL.String() != sub.Endpoint {
		t.Fatalf("sent %s %s", service.req.Method, service.req.URL)
	}
	for name, want := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "43200"} {
		if got := service.req.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.HasPrefix(service.req.Header.Get("Authorization"), "vapid t=") {
		t.Errorf("Authorization = %q", service.req.Header.Get("Authorization"))
	}
	if bytes.Contains(service.body, []byte("hello")) {
		t.Fatal("the message went out in the clear")
	}
	if plain, err := decrypt(t, browser, sub.Auth, service.body); err != nil || !bytes.Equal(plain, message) {
		t.Fatalf("the browser read %q, %v", plain, err)
	}

	// A subscription the push service no longer knows is reported as
	// gone; any other refusal is not.
	for status, gone := range map[int]bool{http.StatusGone: true, http.StatusNotFound: true, http.StatusTooManyRequests: false, http.StatusForbidden: false, http.StatusInternalServerError: false} {
		res, err := Send(context.Background(), &http.Client{Transport: &pushService{status: status}}, v, sub, message, time.Hour)
		if err != nil || res.OK() || res.Gone != gone {
			t.Errorf("status %d: %+v, %v; want refused, gone=%v", status, res, err, gone)
		}
	}

	// An address that is not a push service is never called, even if
	// a stored subscription somehow carries one.
	service = &pushService{status: http.StatusCreated}
	bad := sub
	bad.Endpoint = "https://169.254.169.254/latest/meta-data/"
	if _, err := Send(context.Background(), &http.Client{Transport: service}, v, bad, message, time.Hour); err == nil || service.req != nil {
		t.Fatalf("Send to a non-push address: err %v, request made: %v", err, service.req != nil)
	}
}
