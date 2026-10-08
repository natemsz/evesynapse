package app

// Browser push from the application's side: subscribing a browser,
// what the worker sends and when, and what happens to a subscription
// a push service refuses. The encryption itself is checked in
// internal/webpush against the RFC's worked example.

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/webpush"
)

// fakePushService stands in for the browsers' push services: it
// records what it is sent and answers with a chosen status.
type fakePushService struct {
	mu     sync.Mutex
	status int
	sent   []*http.Request
}

func (p *fakePushService) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.Copy(io.Discard, req.Body)
	p.sent = append(p.sent, req)
	return &http.Response{StatusCode: p.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (p *fakePushService) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// enablePush gives the fixture's app a VAPID identity and a push
// service that accepts everything.
func (f *notifyFixture) enablePush() *fakePushService {
	f.t.Helper()
	public, private, err := webpush.GenerateVAPID()
	if err != nil {
		f.t.Fatal(err)
	}
	v, err := webpush.ParseVAPID(public, private, "mailto:ops@example.org")
	if err != nil {
		f.t.Fatal(err)
	}
	service := &fakePushService{status: http.StatusCreated}
	f.app.push = v
	f.app.pushClient = &http.Client{Transport: service}
	return service
}

// browserSubscription is the JSON a browser posts when it subscribes.
func browserSubscription(t *testing.T, endpoint string) string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	body, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	return string(body)
}

// postJSON posts a JSON body as the given session.
func (f *notifyFixture) postJSON(cookie *http.Cookie, path, body string) (int, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (f *notifyFixture) devices(userID int64) int64 {
	f.t.Helper()
	n, err := f.q.CountPushSubscriptionsByUser(f.ctx, userID)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

const fcm = "https://fcm.googleapis.com/fcm/send/"

func TestPushSubscribeAndUnsubscribe(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	sub := browserSubscription(t, fcm+"one")

	// With no key pair on the server, push is simply off.
	if code, _ := f.postJSON(cookie, "/notifications/push/subscribe", sub); code != http.StatusServiceUnavailable {
		t.Fatalf("subscribe with push off = %d, want 503", code)
	}
	_, body := getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings (push off)", body, "Browser notifications are not set up on this server.")
	if strings.Contains(body, "push.js") && strings.Contains(body, "push-panel") {
		t.Fatal("the subscribe controls are offered with push off")
	}

	f.enablePush()
	_, body = getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings (push on)", body,
		`<div id="push-panel" data-vapid-key="`+f.app.push.PublicKey+`">`,
		`id="push-enable" hidden>`, `/static/push.js?v=`,
		"No browser on this account receives notifications yet.")

	if code, msg := f.postJSON(cookie, "/notifications/push/subscribe", sub); code != http.StatusNoContent {
		t.Fatalf("subscribe = %d %q", code, msg)
	}
	// Subscribing the same browser again is the same subscription.
	if code, _ := f.postJSON(cookie, "/notifications/push/subscribe", sub); code != http.StatusNoContent || f.devices(f.userID) != 1 {
		t.Fatalf("subscribing twice: %d, %d device(s)", code, f.devices(f.userID))
	}
	_, body = getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings (one device)", body, "1 browser on this account receives notifications.")

	// What is not a real subscription is refused and not stored. The
	// address matters most: the server will post to it.
	for name, bad := range map[string]string{
		"an address that is not a push service": browserSubscription(t, "https://evil.example/collect"),
		"an internal address":                   browserSubscription(t, "https://169.254.169.254/latest/meta-data/"),
		"plain http":                            browserSubscription(t, "http://fcm.googleapis.com/fcm/send/x"),
		"no keys":                               `{"endpoint":"` + fcm + `two"}`,
		"not json":                              `endpoint=` + fcm + `two`,
		"empty":                                 ``,
	} {
		if code, _ := f.postJSON(cookie, "/notifications/push/subscribe", bad); code != http.StatusBadRequest {
			t.Errorf("%s: subscribe = %d, want 400", name, code)
		}
	}
	if n := f.devices(f.userID); n != 1 {
		t.Fatalf("%d subscriptions stored after the refusals, want still 1", n)
	}

	// Signed out, nothing is stored.
	if code, _ := f.postJSON(nil, "/notifications/push/subscribe", browserSubscription(t, fcm+"anon")); code == http.StatusNoContent {
		t.Fatal("a signed-out visitor subscribed a browser")
	}

	// An account can only remove its own.
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherCh := seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Alt")
	otherCookie := sessionCookie(t, f.app, other.ID, otherCh.CharacterID, otherCh.Name)
	if code, _ := f.postJSON(otherCookie, "/notifications/push/unsubscribe", `{"endpoint":"`+fcm+`one"}`); code != http.StatusNoContent || f.devices(f.userID) != 1 {
		t.Fatalf("another account's unsubscribe: %d, the owner now has %d device(s)", code, f.devices(f.userID))
	}
	if code, _ := f.postJSON(cookie, "/notifications/push/unsubscribe", `{"endpoint":"`+fcm+`one"}`); code != http.StatusNoContent || f.devices(f.userID) != 0 {
		t.Fatalf("unsubscribe: %d, %d device(s) left", code, f.devices(f.userID))
	}

	// No more than pushMaxDevices browsers per account.
	for i := 0; i < pushMaxDevices; i++ {
		if code, msg := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fmt.Sprintf("%sdev%d", fcm, i))); code != http.StatusNoContent {
			t.Fatalf("device %d: %d %q", i, code, msg)
		}
	}
	if code, _ := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fcm+"one-too-many")); code != http.StatusConflict {
		t.Fatalf("device %d = %d, want 409", pushMaxDevices+1, code)
	}
}

func TestWorkerPushesNewNotifications(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	service := f.enablePush()
	now := notifyT0
	f.seed(esi.SnapKillmails, []esi.KillmailRef{})
	f.seed(esi.SnapMail, esi.MailHeaders{})
	f.pass(now) // baselines

	// No browser subscribed: a notification is recorded, nothing is sent.
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 1}})
	if n := f.pass(now.Add(time.Minute)); n != 1 || service.count() != 0 {
		t.Fatalf("%d notification(s), %d push(es); want 1 and 0 with no browser subscribed", n, service.count())
	}

	for _, name := range []string{"laptop", "phone"} {
		if code, msg := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fcm+name)); code != http.StatusNoContent {
			t.Fatalf("subscribe %s: %d %q", name, code, msg)
		}
	}

	// One new event: one message to each browser, encrypted and signed.
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 2}, {KillmailID: 1}})
	if n := f.pass(now.Add(2 * time.Minute)); n != 1 || service.count() != 2 {
		t.Fatalf("%d notification(s), %d push(es); want 1 and 2 (one per browser)", n, service.count())
	}
	for _, req := range service.sent {
		if req.Method != http.MethodPost || !strings.HasPrefix(req.URL.String(), fcm) ||
			req.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(req.Header.Get("Authorization"), "vapid t=") {
			t.Fatalf("sent %s %s with headers %v", req.Method, req.URL, req.Header)
		}
	}

	// Nothing new: nothing sent.
	if n := f.pass(now.Add(3 * time.Minute)); n != 0 || service.count() != 2 {
		t.Fatalf("a quiet pass: %d notification(s), %d push(es) in total", n, service.count())
	}

	// A burst: more than pushIndividual at once is one message per
	// browser, not one per event.
	f.seed(esi.SnapMail, esi.MailHeaders{
		{MailID: 1, From: 5}, {MailID: 2, From: 5}, {MailID: 3, From: 5}, {MailID: 4, From: 5}, {MailID: 5, From: 5},
	})
	if n := f.pass(now.Add(4 * time.Minute)); n != 5 || service.count() != 4 {
		t.Fatalf("a burst of %d: %d push(es) in total, want 4 (one summary per browser)", n, service.count())
	}

	// A kind that is switched off is neither shown nor pushed.
	f.post(cookie, "/notifications/settings", map[string][]string{"kind": {notifySkill}})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 3}, {KillmailID: 2}, {KillmailID: 1}})
	if n := f.pass(now.Add(5 * time.Minute)); n != 0 || service.count() != 4 {
		t.Fatalf("a switched-off kind: %d notification(s), %d push(es) in total", n, service.count())
	}

	// The test button reaches both browsers.
	if code, msg := f.postJSON(cookie, "/notifications/push/test", `{}`); code != http.StatusNoContent || service.count() != 6 {
		t.Fatalf("test: %d %q, %d push(es) in total, want 6", code, msg, service.count())
	}
}

// TestPushDropsDeadSubscriptions: a browser its push service no
// longer knows is forgotten at once; one that keeps being refused is
// forgotten after pushMaxFailures; one that works stays.
func TestPushDropsDeadSubscriptions(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	service := f.enablePush()
	if code, _ := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fcm+"gone")); code != http.StatusNoContent {
		t.Fatal("subscribe failed")
	}
	msg := []pushMessage{{Title: "EveSynapse", Body: "hello", URL: "/notifications/"}}

	service.status = http.StatusGone
	if sent := f.app.pushToUser(f.ctx, f.userID, msg); sent != 0 || f.devices(f.userID) != 0 {
		t.Fatalf("a gone subscription: %d accepted, %d left; want it deleted", sent, f.devices(f.userID))
	}

	if code, _ := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fcm+"flaky")); code != http.StatusNoContent {
		t.Fatal("subscribe failed")
	}
	service.status = http.StatusInternalServerError
	for i := 1; i < pushMaxFailures; i++ {
		f.app.pushToUser(f.ctx, f.userID, msg)
		if f.devices(f.userID) != 1 {
			t.Fatalf("dropped after %d failure(s), want it kept until %d", i, pushMaxFailures)
		}
	}
	// One success wipes the slate.
	service.status = http.StatusCreated
	if sent := f.app.pushToUser(f.ctx, f.userID, msg); sent != 1 {
		t.Fatalf("%d accepted, want 1", sent)
	}
	service.status = http.StatusInternalServerError
	for i := 1; i < pushMaxFailures; i++ {
		f.app.pushToUser(f.ctx, f.userID, msg)
	}
	if f.devices(f.userID) != 1 {
		t.Fatal("a success did not reset the failure count")
	}
	f.app.pushToUser(f.ctx, f.userID, msg)
	if f.devices(f.userID) != 0 {
		t.Fatalf("still stored after %d refusals in a row", pushMaxFailures)
	}
}

// TestServiceWorkerIsPushOnly: the worker is served from the root,
// is never cached for long, and has no fetch handler: it must not
// come between the browser and the server's pages.
func TestServiceWorkerIsPushOnly(t *testing.T) {
	f := newNotifyFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sw.js = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	script := rec.Body.String()
	for _, want := range []string{"addEventListener('push'", "addEventListener('notificationclick'", "showNotification("} {
		if !strings.Contains(script, want) {
			t.Errorf("sw.js is missing %q", want)
		}
	}
	for _, banned := range []string{"addEventListener('fetch'", "caches.", "importScripts"} {
		if strings.Contains(script, banned) {
			t.Errorf("sw.js contains %q: it handles push and nothing else", banned)
		}
	}
}

func TestPushMessagesFor(t *testing.T) {
	if got := pushMessagesFor(nil); got != nil {
		t.Fatalf("no events gave %v", got)
	}
	events := []notifyEvent{
		{Kind: notifyMail, Key: "mail|1|1", Title: "A has new mail", URL: "/mail/?character=1"},
		{Kind: notifySkill, Key: "skill|1|2|5", Title: "A finished training X V", URL: "https://evil.example/"},
	}
	got := pushMessagesFor(events)
	if len(got) != 2 || got[0].Body != "A has new mail" || got[0].URL != "/mail/?character=1" || got[0].Tag != "mail|1|1" {
		t.Fatalf("messages %+v", got)
	}
	// A message never points off the site.
	if got[1].URL != "/notifications/" {
		t.Fatalf("a message points at %q", got[1].URL)
	}
	many := make([]notifyEvent, pushIndividual+1)
	if got := pushMessagesFor(many); len(got) != 1 || got[0].Body != fmt.Sprintf("%d new notifications", pushIndividual+1) || got[0].URL != "/notifications/" {
		t.Fatalf("a burst gave %+v, want one summary", got)
	}
}
