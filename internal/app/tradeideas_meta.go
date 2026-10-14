package app

// ---------------------------------------------------------------------------
// Trade ideas through Meta's Muse Wonder model (the Responses API at
// api.meta.ai), the alternative to Gemma on Cloudflare Workers AI.
// With MODEL_API_KEY set, the refresh asks Muse Wonder; without it,
// Gemma, as before. Same contract as the Cloudflare call: the key
// travels in the Authorization header only — never a URL, never a
// log line — and the answer is capped before it is stored.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	metaAIModel   = "muse-wonder-1.0"
	metaAIAPIBase = "https://api.meta.ai/v1"
)

// metaAIRequest is one Responses API call: a single user turn, no
// streaming.
type metaAIRequest struct {
	Model  string          `json:"model"`
	Input  []metaAIMessage `json:"input"`
	Stream bool            `json:"stream"`
}

type metaAIMessage struct {
	Role    string          `json:"role"`
	Content []metaAIContent `json:"content"`
}

type metaAIContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// metaAIResponse is the Responses API envelope, decoded leniently:
// the answer is every output_text part of every output message, and
// a top-level output_text (the convenience field some responses
// carry) is read when no parts are.
type metaAIResponse struct {
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	OutputText string `json:"output_text"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// callMetaAI runs one Muse Wonder call and returns the answer text,
// truncated to bodyCap. The analyst framing the Cloudflare call
// sends as a system message leads the user turn here. apiBase is a
// parameter for tests; production passes metaAIAPIBase.
func callMetaAI(ctx context.Context, client *http.Client, apiBase, apiKey, model, prompt string, bodyCap int) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("meta ai: no API key configured")
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(metaAIRequest{
		Model: model,
		Input: []metaAIMessage{{
			Role: "user",
			Content: []metaAIContent{{
				Type: "input_text",
				Text: "You are a trade analyst for the video game EVE Online.\n\n" + prompt,
			}},
		}},
	}); err != nil {
		return "", fmt.Errorf("meta ai: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(apiBase, "/")+"/responses", &buf)
	if err != nil {
		return "", fmt.Errorf("meta ai: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("meta ai: call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tradeIdeasRespCap))
	if err != nil {
		return "", fmt.Errorf("meta ai: read response: %w", err)
	}
	var decoded metaAIResponse
	decodeErr := json.Unmarshal(raw, &decoded)
	if resp.StatusCode != http.StatusOK {
		if decodeErr == nil && decoded.Error != nil && decoded.Error.Message != "" {
			return "", fmt.Errorf("meta ai: status %d: %s", resp.StatusCode, clip(decoded.Error.Message, 200))
		}
		return "", fmt.Errorf("meta ai: status %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("meta ai: decode response: %w", decodeErr)
	}
	var answer strings.Builder
	for _, out := range decoded.Output {
		for _, part := range out.Content {
			if part.Type == "output_text" {
				answer.WriteString(part.Text)
			}
		}
	}
	text := strings.TrimSpace(answer.String())
	if text == "" {
		text = strings.TrimSpace(decoded.OutputText)
	}
	if text == "" {
		return "", fmt.Errorf("meta ai: model call returned no answer")
	}
	return clip(text, bodyCap), nil
}

// tradeIdeasModelLabel names the model behind a stored answer for the
// page and module footers.
func tradeIdeasModelLabel(model string) string {
	if strings.HasPrefix(model, "muse-") {
		return "Muse Wonder"
	}
	return "Gemma"
}
