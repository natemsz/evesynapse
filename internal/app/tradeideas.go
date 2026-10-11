package app

// ---------------------------------------------------------------------------
// Trade ideas: Gemma's read on the Forge spread scanner for the
// home page. The worker asks Cloudflare Workers AI about the top
// spread-scanner rows every so often and stores the answer; the
// home widget renders the stored answer only. The model is never
// on the render path: a slow or failed call costs a refresh,
// not a page load, and the previous answer keeps serving
// (stale snapshot, like every other fetch path).
//
// Cost bounds: the prompt carries at most tradeIdeasCSVRows
// scanner rows, refreshes run at most every tradeIdeasRefreshAge
// (and only when the Forge sweep has data), the answer is
// truncated to tradeIdeasBodyCap, and the token travels in the
// Authorization header alone — never a URL, never a log line.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// The model, the refresh age, and the size bounds. Forty rows is
// the common ground between useful context and a small prompt;
// twice a day keeps the ideas within a sweep or two of fresh.
const (
	tradeIdeasModel      = "@cf/google/gemma-4-26b-a4b-it"
	tradeIdeasAPIBase    = "https://api.cloudflare.com/client/v4"
	tradeIdeasRefreshAge = 12 * time.Hour
	tradeIdeasCSVRows    = 40
	tradeIdeasBodyCap    = 8 * 1024
	tradeIdeasTimeout    = 60 * time.Second
	tradeIdeasRespCap    = 1 << 20
)

// tradeIdeasPrompt is the ask, exactly as specified: list
// format, limited narration, across trade-strategy categories.
// The scanner CSV follows it.
const tradeIdeasPrompt = "Find the best trade opportunities from this CSV format list of fictional video game items for the video game EVE Online.\n\nSimply make it list format with limited narration, across different categories/types of trade strategies"

// refreshTradeIdeas asks Gemma about the Forge scanner rows and
// stores the answer. It reports whether a fresh answer landed.
// Anything failing — unconfigured, no sweep yet, the model
// down — keeps the previous answer and says so in the log.
func (app *Application) refreshTradeIdeas(ctx context.Context) (stored int, limited bool) {
	if !app.cfg.TradeIdeasConfigured() {
		return 0, false
	}
	if row, err := app.queries.GetTradeIdeas(ctx); err == nil {
		if time.Since(row.AsOf) < tradeIdeasRefreshAge {
			return 0, false
		}
	}
	csvText, ok := app.buildTradeIdeasCSV(ctx)
	if !ok {
		return 0, false
	}
	body, err := callCloudflareAI(ctx, app.tradeIdeasHTTPClient(), tradeIdeasAPIBase,
		app.cfg.cloudflareAccountID, app.cfg.cloudflareAuthToken, tradeIdeasModel,
		tradeIdeasPrompt+"\n\n"+csvText)
	if err != nil {
		logging.Errorf("trade ideas: refresh: %v", err)
		return 0, false
	}
	now := time.Now().UTC()
	if err := app.queries.PutTradeIdeas(ctx, db.PutTradeIdeasParams{Body: body, AsOf: now, Model: tradeIdeasModel}); err != nil {
		logging.Errorf("trade ideas: store: %v", err)
		return 0, false
	}
	return 1, false
}

// buildTradeIdeasCSV renders the Forge scanner's top rows as the
// CSV the model reads: the same opportunities, filters, and
// formatting the scanner page shows, capped to tradeIdeasCSVRows.
// False when the sweep has not covered the Forge yet.
func (app *Application) buildTradeIdeasCSV(ctx context.Context) (string, bool) {
	view := app.buildScannerView(ctx, url.Values{"region": {forgeRegionID}})
	if !view.HasData || len(view.Rows) == 0 {
		return "", false
	}
	rows := view.Rows
	if len(rows) > tradeIdeasCSVRows {
		rows = rows[:tradeIdeasCSVRows]
	}
	var buf bytes.Buffer
	out := csv.NewWriter(&buf)
	_ = out.Write([]string{"Item", "Station", "Highest buy price", "Lowest sell price", "Spread %", "Sold per day", "Estimated daily profit"})
	for _, row := range rows {
		_ = out.Write([]string{
			row.ItemName, row.Station.Name, row.BestBuy, row.BestSell,
			row.SpreadPct, row.DailyVolume, row.DailyProfit,
		})
	}
	out.Flush()
	if err := out.Error(); err != nil {
		logging.Errorf("trade ideas: csv: %v", err)
		return "", false
	}
	return buf.String(), true
}

// forgeRegionID is The Forge (Jita): the region the trade-ideas
// prompt asks about. It matches the scanner's default region.
const forgeRegionID = "10000002"

// tradeIdeasHTTPClient returns the Cloudflare client: the
// injected one in tests, else a 60-second client (the refresh
// runs at most twice a day, so connections are not reused).
func (app *Application) tradeIdeasHTTPClient() *http.Client {
	if app.tradeIdeasHTTP != nil {
		return app.tradeIdeasHTTP
	}
	return &http.Client{Timeout: tradeIdeasTimeout}
}

// tradeIdeasWidget is the home widget: the stored answer as
// lines, with its age. Template-escaped on render, like every
// other widget: the model's text is data, never markup.
type tradeIdeasWidget struct {
	Lines      []string
	AsOf       string // "Jan 2, 3:04 PM", empty until an answer lands
	HasIdeas   bool
	Configured bool // Cloudflare credentials are set
}

// maxTradeIdeasLines caps one render: the stored answer is
// already capped, this is the belt beside it.
const maxTradeIdeasLines = 60

// buildTradeIdeas reads the stored answer for the home widget.
// Rendered reads only, one bounded row, no fetches.
func (app *Application) buildTradeIdeas(ctx context.Context) *tradeIdeasWidget {
	w := &tradeIdeasWidget{Configured: app.cfg.TradeIdeasConfigured()}
	row, err := app.queries.GetTradeIdeas(ctx)
	if err != nil || strings.TrimSpace(row.Body) == "" {
		return w
	}
	w.HasIdeas = true
	w.AsOf = row.AsOf.UTC().Format("Jan 2, 3:04 PM")
	for _, line := range strings.Split(row.Body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.Lines = append(w.Lines, line)
		}
		if len(w.Lines) >= maxTradeIdeasLines {
			break
		}
	}
	return w
}

// cloudflareAIMessage is one Workers AI chat message.
type cloudflareAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// cloudflareAIResponse is the Workers AI run envelope. Decoded
// leniently (no DisallowUnknownFields): Cloudflare may add
// fields, and only success plus the answer matter here. The
// answer arrives as result.response on older text models and as
// result.choices[0].message.content on chat models (the Gemma 4
// sync-output schema); both shapes are read.
type cloudflareAIResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result struct {
		Response string `json:"response"`
		Choices  []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	} `json:"result"`
}

// callCloudflareAI runs one Workers AI chat call and returns the
// answer text, truncated to tradeIdeasBodyCap. The account ID is
// charset-checked (it becomes URL path), the token travels in
// the Authorization header only, and every error keeps the token
// out of it. apiBase is a parameter for tests; production passes
// tradeIdeasAPIBase.
func callCloudflareAI(ctx context.Context, client *http.Client, apiBase, accountID, token, model, prompt string) (string, error) {
	if !validCloudflareAccountID(accountID) {
		return "", fmt.Errorf("cloudflare: account ID is malformed")
	}
	if token == "" {
		return "", fmt.Errorf("cloudflare: no auth token configured")
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]any{
		"messages": []cloudflareAIMessage{
			{Role: "system", Content: "You are a trade analyst for the video game EVE Online."},
			{Role: "user", Content: prompt},
		},
	}); err != nil {
		return "", fmt.Errorf("cloudflare: encode request: %w", err)
	}
	url := strings.TrimSuffix(apiBase, "/") + "/accounts/" + accountID + "/ai/run/" + model
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return "", fmt.Errorf("cloudflare: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cloudflare: call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tradeIdeasRespCap))
	if err != nil {
		return "", fmt.Errorf("cloudflare: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cloudflare: status %d", resp.StatusCode)
	}
	var decoded cloudflareAIResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("cloudflare: decode response: %w", err)
	}
	if !decoded.Success {
		detail := ""
		if len(decoded.Errors) > 0 {
			detail = ": " + decoded.Errors[0].Message
		}
		return "", fmt.Errorf("cloudflare: model call failed%s", detail)
	}
	answer := strings.TrimSpace(decoded.Result.Response)
	if answer == "" && len(decoded.Result.Choices) > 0 {
		answer = strings.TrimSpace(decoded.Result.Choices[0].Message.Content)
	}
	if answer == "" {
		return "", fmt.Errorf("cloudflare: model call returned no answer")
	}
	return truncateRunes(answer, tradeIdeasBodyCap), nil
}

// validCloudflareAccountID checks the account ID before it
// becomes URL path: word characters and dashes only, bounded
// length. Anything else refuses rather than shaping a URL.
func validCloudflareAccountID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

// truncateRunes caps text at n bytes without splitting a rune.
func truncateRunes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	keep := 0
	for i := range text {
		if i > n {
			break
		}
		keep = i
	}
	return text[:keep]
}
