package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/webpush"
)

// ---------------------------------------------------------------------------
// Browser push: the second way a notification reaches the user. It
// is the same record and the same per-kind settings as the top-bar
// icon; push is only a delivery channel on top, and only for a
// browser the user has subscribed.
//
// The scope is deliberately small (the decision is option 1 of the
// push and PWA note): a service worker at /sw.js that handles a push
// and a click on it. It has no fetch handler and caches nothing, so
// pages are served exactly as before.
//
// Push is off unless the server has a VAPID key pair in its
// environment (make one with `evesynapse -push-keys`).
// ---------------------------------------------------------------------------

const (
	// pushMaxDevices bounds how many browsers one account subscribes.
	pushMaxDevices = 10
	// pushMaxFailures: a subscription refused this many times in a
	// row is dropped. A push service that says the subscription is
	// gone gets it dropped at once.
	pushMaxFailures = 10
	// pushIndividual: up to this many notifications from one worker
	// pass are pushed one by one; more than that arrive as a single
	// message with a count, not a burst.
	pushIndividual = 3
	// pushTTL is how long a push service holds a message for a
	// browser that is offline.
	pushTTL = 12 * time.Hour
	// pushUserBudget bounds the time one account's sends may take, so
	// a slow push service cannot hold up the worker.
	pushUserBudget = 20 * time.Second
)

// pushConfigFromEnv builds the server's push identity. Unset means
// push is off, which is not an error; a pair that is set but wrong is
// reported and push stays off.
func pushConfigFromEnv(cfg Config) *webpush.VAPID {
	if cfg.pushPublicKey == "" && cfg.pushPrivateKey == "" {
		return nil
	}
	subject := cfg.pushSubject
	if subject == "" {
		subject = cfg.publicOrigin()
	}
	v, err := webpush.ParseVAPID(cfg.pushPublicKey, cfg.pushPrivateKey, subject)
	if err != nil {
		logging.Errorf("evesynapse: browser push is off: VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY / VAPID_SUBJECT are not usable: %v", err)
		return nil
	}
	return v
}

// RunPushKeys implements `evesynapse -push-keys`: print a new VAPID
// key pair as the lines to add to .env. Nothing is stored and the
// server is not touched.
func RunPushKeys(stdout, stderr io.Writer) int {
	public, private, err := webpush.GenerateVAPID()
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't make a key pair: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "# Browser push (notifications while no EveSynapse tab is open).")
	fmt.Fprintln(stdout, "# Add these lines to the server's .env and restart. Keep the private")
	fmt.Fprintln(stdout, "# key secret, and do not change the pair later: every browser that")
	fmt.Fprintln(stdout, "# has subscribed would have to subscribe again.")
	fmt.Fprintf(stdout, "VAPID_PUBLIC_KEY=%s\n", public)
	fmt.Fprintf(stdout, "VAPID_PRIVATE_KEY=%s\n", private)
	fmt.Fprintln(stdout, "# A contact address for the push services (mailto: or https:).")
	fmt.Fprintln(stdout, "VAPID_SUBJECT=mailto:you@example.org")
	return 0
}

// handleServiceWorker serves the service worker from the site root,
// which is what lets it act for the whole site (a script under
// /static/ could only act for /static/). It is never cached for long:
// the browser must be able to pick up a new one.
func handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	script, err := fs.ReadFile(staticFS, "static/sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(script)
}

// pushSubscriptionJSON is a browser's PushSubscription.toJSON().
type pushSubscriptionJSON struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// readPushSubscription reads the small JSON body the settings page's
// script posts.
func readPushSubscription(r *http.Request) (pushSubscriptionJSON, error) {
	var sub pushSubscriptionJSON
	err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&sub)
	return sub, err
}

// handlePushSubscribe stores the browser's subscription for the
// signed-in account.
func (app *Application) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if app.push == nil {
		http.Error(w, "browser push is not set up on this server", http.StatusServiceUnavailable)
		return
	}
	posted, err := readPushSubscription(r)
	if err != nil || userID == 0 {
		http.Error(w, "not a push subscription", http.StatusBadRequest)
		return
	}
	// Checked here, before it is stored: the address has to be one of
	// the browsers' push services, and the keys real ones.
	sub, err := webpush.ParseSubscription(posted.Endpoint, posted.Keys.P256dh, posted.Keys.Auth)
	if err != nil {
		// Said in the log too: a browser using a push service this
		// server does not know is something its operator can only
		// learn of here.
		logging.Warnf("push: refused a subscription from user %d at %s: %v", userID, webpush.EndpointHost(posted.Endpoint), err)
		http.Error(w, "not a push subscription: "+err.Error(), http.StatusBadRequest)
		return
	}
	if n, err := app.queries.CountPushSubscriptionsByUser(ctx, userID); err == nil && n >= pushMaxDevices {
		known := false
		if subs, lerr := app.queries.ListPushSubscriptionsByUser(ctx, userID); lerr == nil {
			for _, s := range subs {
				known = known || s.Endpoint == sub.Endpoint
			}
		}
		if !known {
			http.Error(w, fmt.Sprintf("this account already has %d browsers subscribed; turn one off first", pushMaxDevices), http.StatusConflict)
			return
		}
	}
	if err := app.queries.UpsertPushSubscription(ctx, db.UpsertPushSubscriptionParams{
		UserID:    userID,
		Endpoint:  sub.Endpoint,
		P256dh:    base64.RawURLEncoding.EncodeToString(sub.P256dh),
		Auth:      base64.RawURLEncoding.EncodeToString(sub.Auth),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("push: store subscription for user %d: %v", userID, err)
		http.Error(w, "could not save the subscription", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePushUnsubscribe forgets one of the account's subscriptions.
func (app *Application) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	posted, err := readPushSubscription(r)
	if err != nil || userID == 0 || posted.Endpoint == "" {
		http.Error(w, "not a push subscription", http.StatusBadRequest)
		return
	}
	if err := app.queries.DeletePushSubscription(ctx, db.DeletePushSubscriptionParams{UserID: userID, Endpoint: posted.Endpoint}); err != nil {
		logging.Errorf("push: delete subscription for user %d: %v", userID, err)
		http.Error(w, "could not remove the subscription", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePushTest sends a test message, so the user can see that it
// arrives. The settings page names the browser it was clicked in, and
// then only that browser is sent to and a refusal by its push service
// is reported: with several browsers subscribed, one taking the
// message must not hide another refusing it. With no browser named,
// every browser of the account is sent to.
func (app *Application) handlePushTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if app.push == nil || userID == 0 {
		http.Error(w, "browser push is not set up on this server", http.StatusServiceUnavailable)
		return
	}
	test := pushMessage{Title: "EveSynapse", Body: "Browser notifications are working.", URL: "/notifications/settings", Tag: "test"}
	if posted, err := readPushSubscription(r); err == nil && posted.Endpoint != "" {
		app.pushTestOne(ctx, w, userID, posted.Endpoint, test)
		return
	}
	if app.pushToUser(ctx, userID, []pushMessage{test}) == 0 {
		http.Error(w, "no browser took the test message", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushTestOne sends the test to the one subscription of the account
// with this endpoint and answers with what happened.
func (app *Application) pushTestOne(ctx context.Context, w http.ResponseWriter, userID int64, endpoint string, test pushMessage) {
	subs, err := app.queries.ListPushSubscriptionsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("push: list subscriptions for user %d: %v", userID, err)
		http.Error(w, "could not read this account's browsers", http.StatusInternalServerError)
		return
	}
	for _, row := range subs {
		if row.Endpoint != endpoint {
			continue
		}
		sub, err := webpush.ParseSubscription(row.Endpoint, row.P256dh, row.Auth)
		if err != nil {
			break
		}
		payload, _ := json.Marshal(test)
		client := app.pushClient
		if client == nil {
			client = &http.Client{Timeout: 10 * time.Second}
		}
		host := endpoint
		if u, perr := url.Parse(endpoint); perr == nil {
			host = u.Hostname()
		}
		res, err := webpush.Send(ctx, client, app.push, sub, payload, pushTTL)
		switch {
		case err != nil:
			logging.Warnf("push: test to subscription %d: %v", row.ID, err)
			http.Error(w, "this browser's push service ("+host+") could not be reached", http.StatusBadGateway)
		case res.Gone:
			_ = app.queries.DeletePushSubscriptionByID(ctx, row.ID)
			http.Error(w, "this browser's push service ("+host+") no longer knows this browser. Turn notifications off and on again here.", http.StatusBadGateway)
		case !res.OK():
			logging.Warnf("push: test to subscription %d: push service answered %d", row.ID, res.Status)
			http.Error(w, fmt.Sprintf("this browser's push service (%s) refused the message with HTTP %d%s", host, res.Status, pushDetail(res)), http.StatusBadGateway)
		case strings.Contains(strings.ToLower(res.Detail), "dropped"):
			// Microsoft's service answers 201 to a message it then
			// drops, and says so only in its headers.
			logging.Warnf("push: test to subscription %d: accepted with HTTP %d, then dropped (%s)", row.ID, res.Status, res.Detail)
			http.Error(w, "this browser's push service ("+host+") took the message and then dropped it"+pushDetail(res), http.StatusBadGateway)
		default:
			_ = app.queries.MarkPushSubscriptionOK(ctx, db.MarkPushSubscriptionOKParams{At: timeSet(time.Now().UTC()), ID: row.ID})
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	http.Error(w, "the server has no record of this browser. Turn notifications off and on again here.", http.StatusNotFound)
}

// pushDetail is a push service's own account of a delivery, for the
// test's answer.
func pushDetail(res webpush.Result) string {
	if res.Detail == "" {
		return ""
	}
	return " [" + res.Detail + "]"
}

// pushMessage is what the service worker is handed (sw.js reads these
// names).
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag,omitempty"`
}

// pushMessagesFor turns one worker pass's new notifications into the
// messages to push: each on its own when there are a few, one with a
// count when there are many.
func pushMessagesFor(events []notifyEvent) []pushMessage {
	if len(events) == 0 {
		return nil
	}
	if len(events) > pushIndividual {
		return []pushMessage{{
			Title: "EveSynapse",
			Body:  fmt.Sprintf("%d new notifications", len(events)),
			URL:   "/notifications/",
			Tag:   "summary",
		}}
	}
	out := make([]pushMessage, 0, len(events))
	for _, ev := range events {
		out = append(out, pushMessage{Title: "EveSynapse", Body: ev.Title, URL: notifyTarget(ev.URL), Tag: ev.Key})
	}
	return out
}

// pushToUser sends messages to every browser the account has
// subscribed and returns how many sends were accepted. A subscription
// its push service no longer knows is deleted; one that keeps being
// refused is deleted after pushMaxFailures.
func (app *Application) pushToUser(ctx context.Context, userID int64, messages []pushMessage) (accepted int) {
	if app.push == nil || len(messages) == 0 {
		return 0
	}
	subs, err := app.queries.ListPushSubscriptionsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("push: list subscriptions for user %d: %v", userID, err)
		return 0
	}
	if len(subs) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, pushUserBudget)
	defer cancel()
	client := app.pushClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	for _, row := range subs {
		sub, err := webpush.ParseSubscription(row.Endpoint, row.P256dh, row.Auth)
		if err != nil {
			// Stored before a rule tightened, or damaged: it can never
			// be sent to.
			_ = app.queries.DeletePushSubscriptionByID(ctx, row.ID)
			continue
		}
		for _, msg := range messages {
			if ctx.Err() != nil {
				return accepted
			}
			payload, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			res, err := webpush.Send(ctx, client, app.push, sub, payload, pushTTL)
			switch {
			case err == nil && res.OK():
				accepted++
				_ = app.queries.MarkPushSubscriptionOK(ctx, db.MarkPushSubscriptionOKParams{At: timeSet(time.Now().UTC()), ID: row.ID})
				continue
			case err == nil && res.Gone:
				_ = app.queries.DeletePushSubscriptionByID(ctx, row.ID)
			default:
				if err != nil {
					logging.Warnf("push: send to subscription %d: %v", row.ID, err)
				} else {
					logging.Warnf("push: subscription %d: push service answered %d", row.ID, res.Status)
				}
				if n, ferr := app.queries.MarkPushSubscriptionFailed(ctx, row.ID); ferr == nil && n >= pushMaxFailures {
					_ = app.queries.DeletePushSubscriptionByID(ctx, row.ID)
				}
			}
			break // this browser refused: do not send it the rest
		}
	}
	return accepted
}
