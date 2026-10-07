package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// restoreLogging puts the process-wide logging state back the way
// a fresh process has it, whatever a test did to it.
func restoreLogging(t *testing.T) {
	t.Helper()
	original := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(original)
		slog.SetLogLoggerLevel(slog.LevelInfo)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		" INFO ":  slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"Warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"verbose", "2", "errors"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q) was accepted", bad)
		} else if !strings.Contains(err.Error(), "LOG_LEVEL") {
			t.Errorf("error %q does not name LOG_LEVEL", err)
		}
	}
}

func TestSetupRejectsWhatItDoesNotKnow(t *testing.T) {
	restoreLogging(t)
	var buf bytes.Buffer
	if err := Setup("loud", "text", &buf); err == nil {
		t.Error("an unknown level was accepted")
	}
	if err := Setup("info", "xml", &buf); err == nil || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Errorf("an unknown format: err = %v, want one naming LOG_FORMAT", err)
	}
}

// TestTextLinesCarryTheirLevel: the default format is the classic
// log line with the level after the time, and lines below the
// threshold are not written.
func TestTextLinesCarryTheirLevel(t *testing.T) {
	restoreLogging(t)
	var buf bytes.Buffer
	if err := Setup("", "", &buf); err != nil {
		t.Fatalf("Setup with defaults: %v", err)
	}
	Debugf("diagnostic %d", 0)
	Infof("worker: cycle done: %s", "3 refreshed")
	Warnf("worker: ESI error limit hit; backing off")
	Errorf("worker: list characters: %v", "connection refused")

	out := buf.String()
	for _, want := range []string{
		" INFO worker: cycle done: 3 refreshed\n",
		" WARN worker: ESI error limit hit; backing off\n",
		" ERROR worker: list characters: connection refused\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output %q is missing %q", out, want)
		}
	}
	if strings.Contains(out, "diagnostic") {
		t.Errorf("a debug line was written at the default level: %q", out)
	}

	// Raising the threshold drops the routine lines and keeps the
	// ones that need attention.
	buf.Reset()
	if err := Setup("warn", "text", &buf); err != nil {
		t.Fatalf("Setup at warn: %v", err)
	}
	Infof("routine")
	Warnf("handled")
	Errorf("broken")
	out = buf.String()
	if strings.Contains(out, "routine") || !strings.Contains(out, "WARN handled") || !strings.Contains(out, "ERROR broken") {
		t.Errorf("at warn the log holds %q; want the warning and the error only", out)
	}

	// And lowering it shows the diagnostics.
	buf.Reset()
	if err := Setup("debug", "text", &buf); err != nil {
		t.Fatalf("Setup at debug: %v", err)
	}
	Debugf("diagnostic %d", 1)
	if !strings.Contains(buf.String(), "DEBUG diagnostic 1") {
		t.Errorf("at debug the log holds %q; want the debug line", buf.String())
	}
}

// TestJSONLines: LOG_FORMAT=json writes one object per line with the
// level and the message as fields, and lines other code writes
// through the standard log package arrive the same way.
func TestJSONLines(t *testing.T) {
	restoreLogging(t)
	var buf bytes.Buffer
	if err := Setup("info", "json", &buf); err != nil {
		t.Fatalf("Setup json: %v", err)
	}
	Debugf("hidden")
	Warnf("sso callback: state mismatch")
	log.Printf("http: a line from the standard library")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines written, want 2: %q", len(lines), buf.String())
	}
	want := []struct{ level, msg string }{
		{"WARN", "sso callback: state mismatch"},
		{"INFO", "http: a line from the standard library"},
	}
	for i, line := range lines {
		var rec struct {
			Time  string `json:"time"`
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d %q is not JSON: %v", i, line, err)
		}
		if rec.Level != want[i].level || rec.Msg != want[i].msg || rec.Time == "" {
			t.Errorf("line %d = %+v, want level %s and message %q with a time", i, rec, want[i].level, want[i].msg)
		}
	}
}
