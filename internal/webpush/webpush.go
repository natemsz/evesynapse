// Package webpush sends a browser push message: the Web Push
// protocol with message encryption (RFC 8291, the aes128gcm content
// coding of RFC 8188) and the sender's VAPID signature (RFC 8292).
// Standard library only.
//
// A browser that subscribes hands over three things: the address of
// its push service, a P-256 public key, and a 16-byte secret. A
// message is encrypted so that only that browser can read it (the
// push service in between cannot), signed with the application's own
// key so the push service knows who is sending, and posted to the
// address.
//
// The address comes from the browser, which means from the user, and
// the server then posts to it. AllowedEndpoint limits it to the push
// services browsers actually use, over https, so a subscription
// cannot be used to make the server call an address of someone's
// choosing.
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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxPayload is the most plaintext one message carries. Push services
// accept about 4096 bytes of body; this leaves room for the header
// and the authentication tag.
const MaxPayload = 3000

// recordSize is the aes128gcm record size written in the header. A
// message is always a single record.
const recordSize = 4096

// Subscription is one browser's push subscription, decoded.
type Subscription struct {
	Endpoint string
	P256dh   []byte // the browser's public key, 65 bytes uncompressed
	Auth     []byte // the browser's shared secret, 16 bytes
}

// ParseSubscription decodes a subscription as a browser's
// PushSubscription.toJSON() gives it (base64url keys) and checks it
// is one a message could be sent to.
func ParseSubscription(endpoint, p256dh, auth string) (Subscription, error) {
	sub := Subscription{Endpoint: endpoint}
	if !AllowedEndpoint(endpoint) {
		return sub, errors.New("the address is not a known browser push service")
	}
	var err error
	if sub.P256dh, err = decodeB64(p256dh); err != nil {
		return sub, errors.New("the browser's key is not base64url")
	}
	if sub.Auth, err = decodeB64(auth); err != nil {
		return sub, errors.New("the browser's secret is not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(sub.P256dh); err != nil || len(sub.P256dh) != 65 {
		return sub, errors.New("the browser's key is not a P-256 public key")
	}
	if len(sub.Auth) != 16 {
		return sub, errors.New("the browser's secret is not 16 bytes")
	}
	return sub, nil
}

// pushHosts are the push services browsers use: Chrome and Edge
// (FCM), Firefox (Mozilla), Edge on Windows (WNS) and Safari (Apple).
// An entry beginning with a dot matches any host ending in it.
var pushHosts = []string{
	"fcm.googleapis.com",
	"updates.push.services.mozilla.com",
	".notify.windows.com",
	".push.apple.com",
}

// AllowedEndpoint reports whether raw is an https address at one of
// the known push services, with nothing unusual about it (no user
// info, no port other than 443).
func AllowedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range pushHosts {
		if host == allowed || (strings.HasPrefix(allowed, ".") && strings.HasSuffix(host, allowed)) {
			return true
		}
	}
	return false
}

// Encrypt returns the request body for plaintext: the aes128gcm
// header and the one encrypted record.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	sender, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(sub, plaintext, sender, salt)
}

// encrypt is Encrypt with the two random values supplied, so the
// RFC's worked example can be reproduced exactly.
func encrypt(sub Subscription, plaintext []byte, sender *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("the message is %d bytes; the most one push carries is %d", len(plaintext), MaxPayload)
	}
	receiver, err := ecdh.P256().NewPublicKey(sub.P256dh)
	if err != nil {
		return nil, errors.New("the subscription's key is not a P-256 public key")
	}
	shared, err := sender.ECDH(receiver)
	if err != nil {
		return nil, err
	}
	senderPublic := sender.PublicKey().Bytes()

	// RFC 8291 section 3.4: the shared secret is mixed with the
	// browser's auth secret and both public keys, then RFC 8188
	// derives the content key and nonce from that and the salt.
	prkKey, err := hkdf.Extract(sha256.New, shared, sub.Auth)
	if err != nil {
		return nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), sub.P256dh...), senderPublic...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The record: the message, then 0x02, the mark of a last record.
	record := append(bytes.Clone(plaintext), 0x02)

	var body bytes.Buffer
	body.Write(salt)
	_ = binary.Write(&body, binary.BigEndian, uint32(recordSize))
	body.WriteByte(byte(len(senderPublic)))
	body.Write(senderPublic)
	body.Write(gcm.Seal(nil, nonce, record, nil))
	return body.Bytes(), nil
}

// VAPID is the application's identity to push services: a P-256 key
// pair and a contact address.
type VAPID struct {
	private   *ecdsa.PrivateKey
	PublicKey string // base64url, as the browser's subscribe call wants it
	Subject   string // "mailto:..." or an https address
}

// ParseVAPID reads a key pair as GenerateVAPID writes it and checks
// that the two halves belong together.
func ParseVAPID(publicKey, privateKey, subject string) (*VAPID, error) {
	d, err := decodeB64(privateKey)
	if err != nil {
		return nil, errors.New("the private key is not base64url")
	}
	private, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, errors.New("the private key is not a P-256 key")
	}
	pub, err := private.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	want, err := decodeB64(publicKey)
	if err != nil || !bytes.Equal(want, pub) {
		return nil, errors.New("the public key is not the other half of the private key")
	}
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return nil, errors.New("the subject has to be a mailto: or https: address a push service could contact")
	}
	return &VAPID{private: private, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Subject: subject}, nil
}

// GenerateVAPID makes a new key pair, both halves base64url.
func GenerateVAPID() (publicKey, privateKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return "", "", err
	}
	d, err := key.Bytes()
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(d), nil
}

// vapidLifetime is how long one signed token is good for. Push
// services refuse more than 24 hours.
const vapidLifetime = 12 * time.Hour

// Authorization returns the Authorization header value for a request
// to endpoint: a token signed for that push service alone.
func (v *VAPID) Authorization(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(vapidLifetime).Unix(),
		"sub": v.Subject,
	})
	if err != nil {
		return "", err
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, v.private, digest[:])
	if err != nil {
		return "", err
	}
	// ES256 in a JWT is r and s as two fixed 32-byte numbers.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signed + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + v.PublicKey, nil
}

// Result is what a push service said.
type Result struct {
	Status int
	// Gone: the subscription no longer exists (the browser dropped
	// it, or the user revoked permission). It should be deleted.
	Gone bool
	// Detail is what the push service said about delivery beyond the
	// status, where it says anything: Microsoft's answers 201 to a
	// message it then drops, and only its X-WNS-* headers tell.
	Detail string
}

// OK reports whether the push service took the message.
func (r Result) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Send encrypts payload for sub and posts it. ttl is how long the
// push service may hold it for a browser that is offline. An error is
// a request that could not be made; a push service's refusal is in
// the Result.
func Send(ctx context.Context, client *http.Client, v *VAPID, sub Subscription, payload []byte, ttl time.Duration) (Result, error) {
	if !AllowedEndpoint(sub.Endpoint) {
		return Result{}, errors.New("the address is not a known browser push service")
	}
	body, err := Encrypt(sub, payload)
	if err != nil {
		return Result{}, err
	}
	auth, err := v.Authorization(sub.Endpoint, time.Now())
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(ttl/time.Second)))
	req.Header.Set("Authorization", auth)
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	var detail []string
	for _, name := range []string{"X-WNS-Status", "X-WNS-NotificationStatus", "X-WNS-DeviceConnectionStatus", "X-WNS-Error-Description"} {
		if v := resp.Header.Get(name); v != "" {
			if len(v) > 200 {
				v = v[:200]
			}
			detail = append(detail, name+": "+v)
		}
	}
	return Result{
		Status: resp.StatusCode,
		Gone:   resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone,
		Detail: strings.Join(detail, "; "),
	}, nil
}

// decodeB64 reads base64url with or without padding (browsers give
// it without).
func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(s), "="))
}
