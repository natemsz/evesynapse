package app

// ---------------------------------------------------------------------------
// The residue step's local reader: a llama.cpp server held as a
// child process on a loopback port, started on first use and
// kept for the process's life. Inference never leaves the box:
// the only address this file dials is the configured loopback
// one, and the only payload is the residue prompt. The server
// is asked for replies in one strict JSON shape (the schema
// below, handed over as a grammar); the reply is still parsed
// strictly by the caller, because the step trusts its own
// checks, not the server's.
//
// A box without the runtime or the model file is a graceful
// absence: Complete answers errResidueAbsent and the step
// idles. The server is tied to the first caller's context (the
// worker's), so it stops when the worker stops.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"evesynapse/internal/logging"
)

// errResidueAbsent is a pass skipped because the local reader
// is not installed on this box: not a failure, and the caller
// drops the report quietly.
var errResidueAbsent = errors.New("residue: local reader not installed")

// residueReplySchema is the strict reply shape, given to the
// server as a JSON schema (its grammar form) and enforced
// again on parse (parseResidueReply).
const residueReplySchema = `{
  "type": "object",
  "properties": {
    "claims": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "span": {"type": "string"},
          "kind": {"type": "string", "enum": ["pilot", "ship", "system", "count", "direction", "action", "structure"]},
          "id": {"type": "integer"},
          "name": {"type": "string"},
          "n": {"type": "integer"}
        },
        "required": ["span", "kind"],
        "additionalProperties": false
      }
    }
  },
  "required": ["claims"],
  "additionalProperties": false
}`

// llamaEngine is the residueEngine that speaks to the local
// llama.cpp server.
type llamaEngine struct {
	bin, model, addr string
	client           *http.Client

	mu      sync.Mutex
	started bool // the server answered its health check
	absent  bool // the runtime or the model file is not on this box
	failed  bool // the last start attempt failed
	lastTry time.Time
}

func newLlamaEngine(cfg Config) *llamaEngine {
	return &llamaEngine{
		bin: cfg.residueModelBin, model: cfg.residueModelPath, addr: cfg.residueModelAddr,
		client: &http.Client{},
	}
}

// Complete runs one pass: the server is started if it is not
// already up, then one chat completion is asked for in the
// strict shape and its text returned raw.
func (e *llamaEngine) Complete(ctx context.Context, prompt string) (string, error) {
	if err := e.ensureStarted(ctx); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"model":       "local",
		"temperature": 0,
		"max_tokens":  512,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "residue_reading",
				"strict": true,
				"schema": json.RawMessage(residueReplySchema),
			},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+e.addr+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("residue: local reader call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("residue: local reader answered %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("residue: local reader reply does not parse")
	}
	return out.Choices[0].Message.Content, nil
}

// ensureStarted starts the server once and waits for its
// health check. A missing runtime or model file marks the
// engine absent for the process's life (the owner installs,
// then restarts the service); a failed start is retried at
// most every five minutes.
func (e *llamaEngine) ensureStarted(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}
	if e.absent {
		return errResidueAbsent
	}
	if e.failed && time.Since(e.lastTry) < 5*time.Minute {
		return errResidueAbsent
	}
	e.lastTry = time.Now()
	if _, err := exec.LookPath(e.bin); err != nil {
		e.absent = true
		logging.Infof("residue: local reader runtime %q not found; the step idles", e.bin)
		return errResidueAbsent
	}
	if _, err := os.Stat(e.model); err != nil {
		e.absent = true
		logging.Infof("residue: model file %q not present; the step idles", e.model)
		return errResidueAbsent
	}
	_, port, err := net.SplitHostPort(e.addr)
	if err != nil || port == "" {
		port = "18099"
	}
	// Two threads and a small context: the reader shares the
	// box with the web tier and must never crowd it.
	cmd := exec.CommandContext(ctx, e.bin,
		"-m", e.model,
		"--host", "127.0.0.1", "--port", port,
		"-c", "4096", "-t", "2", "--parallel", "1")
	if err := cmd.Start(); err != nil {
		e.failed = true
		logging.Warnf("residue: start local reader: %v", err)
		return errResidueAbsent
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		if e.healthy(ctx) {
			e.started, e.failed = true, false
			logging.Infof("residue: local reader up on %s", e.addr)
			return nil
		}
		if time.Now().After(deadline) {
			e.failed = true
			logging.Warnf("residue: local reader did not become healthy on %s", e.addr)
			return errResidueAbsent
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// healthy asks the server's health endpoint: llama.cpp answers
// 503 while the model loads and 200 when it can serve.
func (e *llamaEngine) healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+e.addr+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
