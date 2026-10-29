package app

// The market API's front door (/api/market/v1, schema 108). Every
// request passes, in order:
//
//  1. a concurrency cap: at most marketAPIMaxInFlight requests run at
//     once across every key, so the API can never crowd out the site
//     (503 with Retry-After past it);
//  2. a per-address allowance spent by every request that does not
//     carry a live key, so guessing keys is slow (429);
//  3. the key itself: "Authorization: Bearer esk_...", looked up by
//     its SHA-256 only, refused once revoked (401);
//  4. a per-key rate (a steady per-minute pace with a small burst)
//     and a per-key daily quota (429 with Retry-After);
//  5. a body cap and a deadline on the work.
//
// Keys are read-only and market-only: a key acts for no session, and
// no handler behind this door writes anything but the key's own
// usage and an abyssal want. Usage counts in memory and is flushed
// by the worker, so a request costs no write.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/marketwire"
)

const (
	apiKeyPrefix         = "esk_"
	apiKeyBodyLen        = 40
	apiMaxKeysPerUser    = 5
	apiMaxKeysPerDay     = 10 // keys a player may make in a day, revoked or not
	marketAPIPerMinute   = 60
	marketAPIBurst       = 30
	marketAPIBadPerMin   = 20 // requests without a live key, per address
	marketAPIBadBurst    = 10
	marketAPIMaxInFlight = 32
	marketAPIMaxBody     = 64 << 10
	marketAPIDeadline    = 5 * time.Second
	marketAPIMaxBatch    = 200
)

type apiKeyCtx struct{}

// apiKeyRef is the authenticated key a request runs as.
type apiKeyRef struct {
	ID, UserID, Quota int64
}

func apiKeyFrom(ctx context.Context) apiKeyRef {
	k, _ := ctx.Value(apiKeyCtx{}).(apiKeyRef)
	return k
}

// newAPIKey makes a key: the prefix and 40 characters from crypto/rand.
func newAPIKey() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	b.WriteString(apiKeyPrefix)
	for i := 0; i < apiKeyBodyLen; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), nil
}

func hashAPIKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

// apiUsage counts each key's requests today in memory, seeded from
// the stored count the first time a key is seen that day, and hands
// the worker what to flush.
type apiUsage struct {
	mu      sync.Mutex
	day     string
	counts  map[int64]int64    // today's total per key
	pending map[int64][2]int64 // unflushed requests, refused
	touched map[int64]time.Time
}

// spend counts one request for a key against its quota. seed reads
// the stored count the first time; ok is false past the quota.
func (u *apiUsage) spend(key apiKeyRef, now time.Time, seed func() int64) bool {
	day := now.UTC().Format("2006-01-02")
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.day != day || u.counts == nil {
		u.day, u.counts = day, map[int64]int64{}
	}
	if u.pending == nil {
		u.pending, u.touched = map[int64][2]int64{}, map[int64]time.Time{}
	}
	used, seen := u.counts[key.ID]
	if !seen {
		u.mu.Unlock()
		used = seed()
		u.mu.Lock()
		if again, ok := u.counts[key.ID]; ok && again > used {
			used = again
		}
	}
	p := u.pending[key.ID]
	u.touched[key.ID] = now
	if used >= key.Quota {
		p[1]++
		u.pending[key.ID] = p
		u.counts[key.ID] = used
		return false
	}
	u.counts[key.ID] = used + 1
	p[0]++
	u.pending[key.ID] = p
	return true
}

// used is today's count for a key, as far as memory knows.
func (u *apiUsage) used(keyID int64) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.counts[keyID]
}

// take hands over and clears what is waiting to be stored.
func (u *apiUsage) take() (day string, pending map[int64][2]int64, touched map[int64]time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	day, pending, touched = u.day, u.pending, u.touched
	u.pending, u.touched = map[int64][2]int64{}, map[int64]time.Time{}
	return day, pending, touched
}

var (
	marketAPIUsage    = &apiUsage{}
	marketAPIInFlight = make(chan struct{}, marketAPIMaxInFlight)
	marketAPIKeyRate  = newAnonThrottle(marketAPIPerMinute, marketAPIBurst)
	marketAPIBadRate  = newAnonThrottle(marketAPIBadPerMin, marketAPIBadBurst)
)

// flushMarketAPIUsage stores the counted usage; the worker calls it
// every cycle, and it prunes usage rows older than 90 days.
func (app *Application) flushMarketAPIUsage(ctx context.Context) {
	day, pending, touched := marketAPIUsage.take()
	for id, p := range pending {
		if err := app.queries.AddAPIKeyUsage(ctx, db.AddAPIKeyUsageParams{KeyID: id, Day: day, Requests: p[0], Refused: p[1]}); err != nil {
			logging.Errorf("worker: market api usage for key %d: %v", id, err)
		}
	}
	for id, at := range touched {
		_ = app.queries.TouchAPIKey(ctx, db.TouchAPIKeyParams{ID: id, LastUsedAt: timeSet(at)})
	}
	_ = app.queries.PruneAPIKeyUsage(ctx, time.Now().UTC().AddDate(0, 0, -90).Format("2006-01-02"))
}

// writeMarketAPIJSON writes an answer with the API's fixed headers.
func writeMarketAPIJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "private, max-age=30")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMarketAPIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	writeMarketAPIJSON(w, status, marketwire.Error{Error: marketwire.ErrorBody{Code: code, Message: msg}})
}

func writeMarketAPIData(w http.ResponseWriter, data any, notes ...string) {
	writeMarketAPIJSON(w, http.StatusOK, marketwire.Envelope{Data: data, Meta: marketwire.Meta{
		Version: marketwire.Version, GeneratedAt: time.Now().UTC(), Notes: notes,
	}})
}

// marketAPIGuard is the front door described above.
func (app *Application) marketAPIGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case marketAPIInFlight <- struct{}{}:
			defer func() { <-marketAPIInFlight }()
		default:
			w.Header().Set("Retry-After", "5")
			writeMarketAPIError(w, http.StatusServiceUnavailable, "busy", "The API is busy. Try again in a few seconds.")
			return
		}
		now := time.Now()
		bearer, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		bearer = strings.TrimSpace(bearer)
		badRequest := func(status int, code, msg string) {
			if ok, wait := marketAPIBadRate.allow(clientAddr(r)); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeMarketAPIError(w, http.StatusTooManyRequests, "slow_down", "Too many requests without a valid key from this address.")
				return
			}
			writeMarketAPIError(w, status, code, msg)
		}
		if !hasBearer || !strings.HasPrefix(bearer, apiKeyPrefix) || len(bearer) != len(apiKeyPrefix)+apiKeyBodyLen {
			badRequest(http.StatusUnauthorized, "no_key", "Send your API key as: Authorization: Bearer esk_...")
			return
		}
		row, err := app.queries.GetLiveAPIKeyByHash(r.Context(), hashAPIKey(bearer))
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				logging.Errorf("market api: key lookup: %v", err)
				writeMarketAPIError(w, http.StatusServiceUnavailable, "unavailable", "Try again shortly.")
				return
			}
			badRequest(http.StatusUnauthorized, "bad_key", "That key is unknown or revoked.")
			return
		}
		key := apiKeyRef{ID: row.ID, UserID: row.UserID, Quota: row.DailyQuota}
		if ok, wait := marketAPIKeyRate.allow(strconv.FormatInt(key.ID, 10)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeMarketAPIError(w, http.StatusTooManyRequests, "rate_limited", "Slow down: this key is over its per-minute pace.")
			return
		}
		if !marketAPIUsage.spend(key, now, func() int64 {
			n, err := app.queries.GetAPIKeyUsage(r.Context(), db.GetAPIKeyUsageParams{KeyID: key.ID, Day: now.UTC().Format("2006-01-02")})
			if err != nil {
				return 0
			}
			return n
		}) {
			tomorrow := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
			w.Header().Set("Retry-After", strconv.Itoa(int(tomorrow.Sub(now).Seconds())+1))
			writeMarketAPIError(w, http.StatusTooManyRequests, "quota_spent", "This key has used its requests for today (UTC).")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, marketAPIMaxBody)
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), apiKeyCtx{}, key), marketAPIDeadline)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
