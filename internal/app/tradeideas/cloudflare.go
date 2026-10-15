package tradeideas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

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
	return callCloudflareAIWith(ctx, client, apiBase, accountID, token, model, prompt, tradeIdeasBodyCap, 0)
}

// callCloudflareAIWith is callCloudflareAI with the answer capped to
// bodyCap and, when maxTokens is set, the model allowed that many
// output tokens (the page's long answer needs more than the default).
func callCloudflareAIWith(ctx context.Context, client *http.Client, apiBase, accountID, token, model, prompt string, bodyCap, maxTokens int) (string, error) {
	if !validCloudflareAccountID(accountID) {
		return "", fmt.Errorf("cloudflare: account ID is malformed")
	}
	if token == "" {
		return "", fmt.Errorf("cloudflare: no auth token configured")
	}
	var buf bytes.Buffer
	payload := map[string]any{
		"messages": []cloudflareAIMessage{
			{Role: "system", Content: "You are a trade analyst for the video game EVE Online."},
			{Role: "user", Content: prompt},
		},
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
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
	return clip(answer, bodyCap), nil
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

// clip shortens s to at most max bytes without splitting a character.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}
