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
// idles. The server process is bound to the worker's context
// (handed over when the engine is made), never to a pass's
// own deadline context: a pass ending never stops it, and
// the worker stopping always does.
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
	"strings"
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
	serveCtx         context.Context // the worker's life: the server process is bound to it, never to a pass

	mu      sync.Mutex
	started bool // the server answered its health check
	absent  bool // the runtime or the model file is not on this box
	failed  bool // the last start attempt failed
	lastTry time.Time
	cmd     *exec.Cmd      // the server believed up, nil when none is
	exited  chan struct{}  // closed by the server's reaper once it has been waited
	stderr  *boundedStderr // the current child's captured stderr tail
}

// boundedStderr keeps the last bytes a child wrote to stderr,
// so a death is diagnosable from the log without an unbounded
// buffer growing beside the worker.
type boundedStderr struct {
	mu  sync.Mutex
	buf []byte
}

const residueStderrCap = 8192

func (b *boundedStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > residueStderrCap {
		b.buf = append([]byte{}, b.buf[len(b.buf)-residueStderrCap:]...)
	}
	return len(p), nil
}

func (b *boundedStderr) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func newLlamaEngine(cfg Config, serveCtx context.Context) *llamaEngine {
	return &llamaEngine{
		bin: cfg.residueModelBin, model: cfg.residueModelPath, addr: cfg.residueModelAddr,
		client:   &http.Client{},
		serveCtx: serveCtx,
	}
}

// The pass's two budgets, kept separate: model load is slow
// exactly once per server life, generation is the per-pass
// cost. One shared budget let load time starve generation
// under contention (and a warm server wasted none of it).
const (
	residueLoadTimeout       = 120 * time.Second
	residueGenerationTimeout = 120 * time.Second
)

// Complete runs one pass: the server is started if it is not
// already up (under the load budget), then one chat completion
// is asked for in the strict shape (under the generation
// budget) and its text returned raw. A server already warm
// spends no generation budget on load.
func (e *llamaEngine) Complete(ctx context.Context, prompt string) (string, error) {
	loadCtx, stopLoad := context.WithTimeout(ctx, residueLoadTimeout)
	err := e.ensureStarted(loadCtx)
	stopLoad()
	if err != nil {
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
	genCtx, stopGen := context.WithTimeout(ctx, residueGenerationTimeout)
	defer stopGen()
	req, err := http.NewRequestWithContext(genCtx, http.MethodPost, "http://"+e.addr+"/v1/chat/completions", bytes.NewReader(body))
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

// ensureStarted starts the server if it is not up and waits
// for its health check. The process runs under the worker's
// context, so it outlives any one pass and dies with the
// worker. A server found dead is started fresh for the pass
// that finds it. A missing runtime or model file marks the
// engine absent for the process's life (the owner installs,
// then restarts the service); a failed start is retried at
// most every five minutes.
func (e *llamaEngine) ensureStarted(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		if e.aliveLocked() {
			return nil
		}
		// The server died on its own: the reaper has waited
		// it, so the port is free. Fall through and start a
		// fresh one.
		logging.Warnf("residue: local reader exited; starting it again")
		e.started, e.cmd, e.exited = false, nil, nil
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
	cmd := exec.CommandContext(e.serveCtx, e.bin,
		"-m", e.model,
		"--host", "127.0.0.1", "--port", port,
		"-c", "4096", "-t", "2", "--parallel", "1")
	e.stderr = &boundedStderr{}
	cmd.Stderr = e.stderr
	if err := cmd.Start(); err != nil {
		e.failed = true
		logging.Warnf("residue: start local reader: %v", err)
		return errResidueAbsent
	}
	exited := make(chan struct{})
	e.cmd, e.exited = cmd, exited
	go reapResidueServer(cmd, exited, e.stderr)
	deadline := time.Now().Add(120 * time.Second)
	for {
		if e.healthy(ctx) {
			e.started, e.failed = true, false
			logging.Infof("residue: local reader up on %s", e.addr)
			return nil
		}
		select {
		case <-exited:
			e.cmd, e.exited = nil, nil
			e.failed = true
			logging.Warnf("residue: local reader exited while starting")
			return errResidueAbsent
		default:
		}
		if time.Now().After(deadline) {
			e.abandonStartLocked()
			e.failed = true
			logging.Warnf("residue: local reader did not become healthy on %s", e.addr)
			return errResidueAbsent
		}
		select {
		case <-ctx.Done():
			// The caller gave up waiting: the start is
			// abandoned whole, so a later pass retries from
			// a clean slate rather than finding a server
			// this engine never finished bringing up.
			e.abandonStartLocked()
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// aliveLocked reports whether the server believed up is still
// running: its reaper closes the exit channel once the process
// has been waited, so a closed channel is a certain death
// with the port already free.
func (e *llamaEngine) aliveLocked() bool {
	if e.cmd == nil || e.exited == nil {
		return false
	}
	select {
	case <-e.exited:
		return false
	default:
		return true
	}
}

// abandonStartLocked kills a server whose start is being given
// up on and clears the handles to it; the reaper still waits
// it, so nothing is left running or unreaped.
func (e *llamaEngine) abandonStartLocked() {
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
	e.cmd, e.exited = nil, nil
}

// reapResidueServer waits one server process out and closes
// its exit channel: the process is always reaped (never a
// zombie), and the closed channel is how the engine learns
// the server died. The wait's result and the child's stderr
// tail are logged here, where the death is observed: why the
// server exits per pass is a production question this log
// line exists to answer, and fixing the exit itself is
// deliberately not attempted here.
func reapResidueServer(cmd *exec.Cmd, exited chan struct{}, stderr *boundedStderr) {
	err := cmd.Wait()
	if err != nil {
		logging.Warnf("residue: local reader exited: %v; stderr tail: %s", err, stderrTail(stderr))
	} else {
		logging.Infof("residue: local reader exited cleanly; stderr tail: %s", stderrTail(stderr))
	}
	close(exited)
}

// stderrTail renders a captured stderr tail for one log line.
func stderrTail(b *boundedStderr) string {
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "(none)"
	}
	if len(s) > 500 {
		s = s[len(s)-500:]
	}
	return strings.Join(strings.Fields(s), " ")
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
