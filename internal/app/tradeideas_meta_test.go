package app

// Muse Wonder (Meta's Responses API) as the trade-ideas model: the
// request shape, the answer parsing, the key staying in its header,
// and the refresh choosing it over Gemma when MODEL_API_KEY is set.

import (
	"context"
	"evesynapse/internal/app/tradeideas"
	"evesynapse/internal/ideas"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

const metaReply = `{"id":"resp_1","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"### Station Flips\n* **Tritanium** · Jita IV-4 — deep volume"}]}]}`

// TestTradeIdeasRefreshUsesMeta: with MODEL_API_KEY set the refresh
// asks Muse Wonder for the page's answer (never Cloudflare), stores it
// under its model name, and the page credits it. One ask only: the
// home module displays a smaller amount of the page's saved answer
// and never asks itself.
func TestTradeIdeasRefreshUsesMeta(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.cfg.metaAIKey = "meta-test-key"
	app.cfg.cloudflareAccountID = "abcdef0123456789abcdef0123456789"
	app.cfg.cloudflareAuthToken = "test-token-for-trade-ideas-only"
	seedTradeIdeasScanner(t, conn, q)
	stub := &cloudflareStub{reply: metaReply}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}

	if stored := tradeideas.Refresh(ctx, app); stored != 1 || stub.calls != 1 {
		t.Fatalf("refresh stored %d with %d calls, want 1 and 1", stored, stub.calls)
	}
	if host := stub.last.URL.Host; host != "api.meta.ai" {
		t.Fatalf("asked %q, want api.meta.ai", host)
	}
	page, err := q.GetTradeIdeasPage(ctx)
	if err != nil || page.Model != ideas.MuseModel || !strings.Contains(page.Body, "Tritanium") {
		t.Fatalf("page row: %+v err %v", page, err)
	}

	code, body, _ := doReq(t, app, http.MethodGet, "/market/ideas/", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /market/ideas/: status %d", code)
	}
	mustContain(t, "/market/ideas/", body, "Muse Wonder on the Forge spread scanner", "Station Flips")
}

func TestTradeIdeasConfiguredWithMetaKey(t *testing.T) {
	c := Config{metaAIKey: "meta-test-key"}
	if !c.TradeIdeasConfigured() || !(c.MetaAIKey() != "") {
		t.Fatal("a Meta key alone should configure trade ideas through Muse Wonder")
	}
	c = Config{cloudflareAccountID: "abcdef0123456789abcdef0123456789", cloudflareAuthToken: "tok"}
	if !c.TradeIdeasConfigured() || (c.MetaAIKey() != "") {
		t.Fatal("Cloudflare credentials alone should configure trade ideas through Gemma")
	}
}

// TestTradeIdeasModelSwitchRefreshesNow: a fresh Gemma page answer
// does not hold off Muse Wonder once MODEL_API_KEY is set, the answer
// another model gave is asked again on the next cycle, not 12 hours
// later, and the new model's fresh answer then holds as usual.
func TestTradeIdeasModelSwitchRefreshesNow(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	seedTradeIdeasScanner(t, conn, q)
	fresh := time.Now().UTC().Add(-time.Hour)
	if err := q.PutTradeIdeasPage(ctx, db.PutTradeIdeasPageParams{Body: "### Gemma\n* old", AsOf: fresh, Model: ideas.GemmaModel}); err != nil {
		t.Fatalf("seed page answer: %v", err)
	}
	app.cfg.metaAIKey = "meta-test-key"
	stub := &cloudflareStub{reply: metaReply}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}

	if stored := tradeideas.Refresh(ctx, app); stored != 1 || stub.calls != 1 {
		t.Fatalf("after the switch: stored %d with %d calls, want 1 and 1", stored, stub.calls)
	}
	if row, err := q.GetTradeIdeasPage(ctx); err != nil || row.Model != ideas.MuseModel {
		t.Fatalf("page row after the switch: %+v err %v", row, err)
	}
	if stored := tradeideas.Refresh(ctx, app); stored != 0 || stub.calls != 1 {
		t.Fatalf("second refresh: stored %d with %d calls, want 0 and 1", stored, stub.calls)
	}
}
