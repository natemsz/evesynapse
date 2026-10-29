package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// The market API's front door and its pricing: no key or a revoked
// key is refused, a live key is served until its daily quota is
// spent, the API's own host serves nothing but /api/, the health
// answer needs no key and allows only the status page's origin, and
// a researched blueprint prices from its exact research median.
func TestMarketAPIAccessAndPricing(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.cfg.marketAPIHost = "api.example.test"
	app.cfg.statusPageOrigin = "https://status.example.test"
	h := app.Handler()
	do := func(method, host, path, key, origin, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = host
		req.RemoteAddr = "203.0.113.9:4000"
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := newAPIKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	id, err := q.CreateAPIKey(ctx, db.CreateAPIKeyParams{UserID: user.ID, Name: "test", Prefix: key[:10], KeyHash: hashAPIKey(key)})
	if err != nil {
		t.Fatalf("store key: %v", err)
	}
	if _, err := app.db.ExecContext(ctx, `UPDATE api_keys SET daily_quota = 3 WHERE id = $1`, id); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	if rec := do("GET", "example.test", "/api/market/v1/prices?types=34", "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", rec.Code)
	}
	bogus := apiKeyPrefix + strings.Repeat("A", apiKeyBodyLen)
	if rec := do("GET", "example.test", "/api/market/v1/prices?types=34", bogus, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: status %d, want 401", rec.Code)
	}
	if rec := do("GET", "api.example.test", "/market/", "", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("site page on the API host: status %d, want 404", rec.Code)
	}

	// Researched original: a stored ME 10 / TE 20 median prices it.
	if _, err := app.db.ExecContext(ctx, `INSERT INTO blueprint_research_medians (type_id, me, te, median_price, sample_count, computed_at) VALUES (23919, 10, 20, 2500000000, 6, $1)`, time.Now().UTC()); err != nil {
		t.Fatalf("seed research median: %v", err)
	}
	rec := do("POST", "api.example.test", "/api/market/v1/appraise", key, "", `{"lines":[{"type_id":23919,"quantity":1,"me":10,"te":20}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("appraise: status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			Total float64 `json:"total"`
			Lines []struct {
				Tier string `json:"tier"`
			} `json:"lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Data.Lines) != 1 ||
		got.Data.Lines[0].Tier != "research_median" || got.Data.Total != 2.5e9 {
		t.Errorf("appraise = %s, want one research_median line at 2.5B", rec.Body.String())
	}

	// Quota of 3: one spent above, two more pass, the fourth is refused.
	for i := 0; i < 2; i++ {
		if rec := do("GET", "api.example.test", "/api/market/v1/key", key, "", ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i+2, rec.Code)
		}
	}
	if rec := do("GET", "api.example.test", "/api/market/v1/key", key, "", ""); rec.Code != http.StatusTooManyRequests {
		t.Errorf("past quota: status %d, want 429", rec.Code)
	}

	if _, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{ID: id, UserID: user.ID}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rec := do("GET", "api.example.test", "/api/market/v1/key", key, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status %d, want 401", rec.Code)
	}

	rec = do("GET", "api.example.test", "/api/market/v1/health", "", "https://status.example.test", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://status.example.test" {
		t.Errorf("health from the status page: status %d, allow-origin %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec := do("GET", "api.example.test", "/api/market/v1/health", "", "https://evil.example", ""); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("health allows a foreign origin")
	}
}
