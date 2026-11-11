package tradeideas

import (
	"context"
	"net/http"
)

// callCloudflareAI runs one Workers AI chat call and returns the
// answer text, truncated to tradeIdeasBodyCap. The account ID is
// charset-checked (it becomes URL path), the token travels in
// the Authorization header only, and every error keeps the token
// out of it. apiBase is a parameter for tests; production passes
// tradeIdeasAPIBase.
func callCloudflareAI(ctx context.Context, client *http.Client, apiBase, accountID, token, model, prompt string) (string, error) {
	return callCloudflareAIWith(ctx, client, apiBase, accountID, token, model, prompt, tradeIdeasBodyCap, 0)
}

// tradeIdeasPromptFor wraps the scanner CSV for the model. Station
// names in it can be player structures, named by players: the rows go
// in only as data, fenced between markers the model is told to read
// as data and never as instructions, and no cell can contain a
// marker (tradeIdeasCell).
func tradeIdeasPromptFor(csvText string) string {
	return tradeIdeasWrap(tradeIdeasPrompt, csvText)
}
