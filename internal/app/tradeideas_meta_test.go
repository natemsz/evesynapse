package app

// Muse Wonder (Meta's Responses API) as the trade-ideas model: the
// request shape, the answer parsing, the key staying in its header,
// and the refresh choosing it over Gemma when MODEL_API_KEY is set.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const metaReply = `{"id":"resp_1","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"### Station Flips\n* **Tritanium** · Jita IV-4 — deep volume"}]}]}`

func TestCallMetaAIRequestAndAnswer(t *testing.T) {
	stub := &cloudflareStub{reply: metaReply}
	got, err := callMetaAI(context.Background(), &http.Client{Transport: stub}, metaAIAPIBase,
		"meta-test-key", metaAIModel, "Item,Station\nTritanium,Jita\n", tradeIdeasBodyCap)
	if err != nil {
		t.Fatalf("callMetaAI: %v", err)
	}
	if !strings.Contains(got, "**Tritanium**") || !strings.Contains(got, "### Station Flips") {
		t.Fatalf("answer = %q", got)
	}
	if u := stub.last.URL.String(); u != "https://api.meta.ai/v1/responses" {
		t.Fatalf("URL = %q", u)
	}
	if strings.Contains(stub.last.URL.String(), "meta-test-key") {
		t.Fatal("API key leaked into the URL")
	}
	if h := stub.last.Header.Get("Authorization"); h != "Bearer meta-test-key" {
		t.Fatalf("Authorization = %q", h)
	}
	var sent metaAIRequest
	if err := json.Unmarshal(stub.body, &sent); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if sent.Model != metaAIModel || sent.Stream || len(sent.Input) != 1 || sent.Input[0].Role != "user" ||
		len(sent.Input[0].Content) != 1 || sent.Input[0].Content[0].Type != "input_text" ||
		!strings.Contains(sent.Input[0].Content[0].Text, "Tritanium") {
		t.Fatalf("request = %+v", sent)
	}
}

func TestCallMetaAIFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := callMetaAI(ctx, &http.Client{Transport: &cloudflareStub{reply: metaReply}}, metaAIAPIBase, "", metaAIModel, "hi", 100); err == nil {
		t.Error("no key: want an error")
	}
	for name, stub := range map[string]*cloudflareStub{
		"status":    {code: http.StatusUnauthorized, reply: `{"error":{"message":"bad key"}}`},
		"no answer": {reply: `{"output":[]}`},
		"not json":  {reply: `<html>`},
	} {
		_, err := callMetaAI(ctx, &http.Client{Transport: stub}, metaAIAPIBase, "meta-test-key", metaAIModel, "hi", 100)
		if err == nil {
			t.Errorf("%s: want an error", name)
		} else if strings.Contains(err.Error(), "meta-test-key") {
			t.Errorf("%s: error carries the key: %v", name, err)
		}
	}
	// The convenience field stands in when no parts carry text.
	stub := &cloudflareStub{reply: `{"output_text":"* **Pyerite**"}`}
	if got, err := callMetaAI(ctx, &http.Client{Transport: stub}, metaAIAPIBase, "meta-test-key", metaAIModel, "hi", 100); err != nil || got != "* **Pyerite**" {
		t.Errorf("output_text fallback = %q, %v", got, err)
	}
}

// TestTradeIdeasRefreshUsesMeta: with MODEL_API_KEY set the refresh
// asks Muse Wonder for both answers (never Cloudflare), stores them
// under its model name, and the page credits it.
func TestTradeIdeasRefreshUsesMeta(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.cfg.metaAIKey = "meta-test-key"
	app.cfg.cloudflareAccountID = "abcdef0123456789abcdef0123456789"
	app.cfg.cloudflareAuthToken = "test-token-for-trade-ideas-only"
	seedTradeIdeasScanner(t, conn, q)
	stub := &cloudflareStub{reply: metaReply}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}

	if stored, _ := app.refreshTradeIdeas(ctx); stored != 2 || stub.calls != 2 {
		t.Fatalf("refresh stored %d with %d calls, want 2 and 2", stored, stub.calls)
	}
	if host := stub.last.URL.Host; host != "api.meta.ai" {
		t.Fatalf("asked %q, want api.meta.ai", host)
	}
	row, err := q.GetTradeIdeas(ctx)
	if err != nil || row.Model != metaAIModel || !strings.Contains(row.Body, "Tritanium") {
		t.Fatalf("module row: %+v err %v", row, err)
	}
	page, err := q.GetTradeIdeasPage(ctx)
	if err != nil || page.Model != metaAIModel {
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
	if !c.TradeIdeasConfigured() || !c.tradeIdeasUsesMeta() {
		t.Fatal("a Meta key alone should configure trade ideas through Muse Wonder")
	}
	c = Config{cloudflareAccountID: "abcdef0123456789abcdef0123456789", cloudflareAuthToken: "tok"}
	if !c.TradeIdeasConfigured() || c.tradeIdeasUsesMeta() {
		t.Fatal("Cloudflare credentials alone should configure trade ideas through Gemma")
	}
}
