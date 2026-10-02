package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Minimal ESI client for the first character pull. ESI is at
// https://esi.evetech.net; every request carries a descriptive
// User-Agent (CCP asks third parties to identify themselves).
const (
	esiBaseURL   = "https://esi.evetech.net"
	esiUserAgent = "EveSynapse/0.1 (dev)"
)

// esiCharacter is the public character sheet: GET /characters/{id}/
// (returned fields EveSynapse currently consumes).
type esiCharacter struct {
	Name           string  `json:"name"`
	CorporationID  int64   `json:"corporation_id"`
	Birthday       string  `json:"birthday"` // RFC3339
	SecurityStatus float64 `json:"security_status"`
}

// esiCorporation is GET /corporations/{id}/ (name only, for now).
type esiCorporation struct {
	Name string `json:"name"`
}

// esiGet performs a GET against ESI and JSON-decodes the response into
// out. The access token is sent as a Bearer header when non-empty and
// is never logged. Errors carry the path and status, nothing sensitive.
func esiGet(ctx context.Context, accessToken, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, esiBaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", esiUserAgent)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := loginHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ESI GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ESI GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("ESI GET %s: decode: %w", path, err)
	}
	return nil
}
