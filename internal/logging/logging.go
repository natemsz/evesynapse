// Package logging gives every log line a level.
//
// Call sites say what kind of line they are writing — Errorf, Warnf,
// Infof, Debugf — and LOG_LEVEL decides which kinds are written.
//
// What the levels mean here:
//
//	Error  something failed that should not have: a query, a
//	       decode, a store, a template, a recovered panic.
//	Warn   something went wrong in a way the app expects and
//	       handles: ESI asking it to back off, stale data served
//	       because a refresh failed, a sign-in that did not
//	       complete, a setting worth changing.
//	Info   what the app is doing: starting and stopping, sign-ins,
//	       cycle summaries, imports, requests.
//	Debug  nothing yet; there for diagnosing.
//
// At the default level (info) the same lines are written as
// before, each now carrying its level.
//
// The messages stay as they were: formatted strings with their
// subsystem in front ("worker: ...", "sso: ..."). The functions sit
// on top of log/slog, so call sites that want structured fields
// can use slog directly and share the same level and output.
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
)

// Setup applies the operator's logging settings to the whole
// process. level is LOG_LEVEL ("debug", "info", "warn" or "error";
// empty means info) and format is LOG_FORMAT ("text" or "json";
// empty means text). Output goes to w.
//
// Text keeps the familiar line with the level after the time:
//
//	2026/10/07 12:00:00 WARN worker: ESI error limit hit ...
//
// JSON writes one object per line (time, level, msg), for log
// collectors that want to filter on fields.
func Setup(level, format string, w io.Writer) error {
	lvl, err := ParseLevel(level)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		// slog's default handler writes through the standard log
		// package, which is exactly the classic line; only the
		// threshold and the destination need setting.
		log.SetOutput(w)
		slog.SetLogLoggerLevel(lvl)
	case "json":
		// SetDefault also routes anything still written through the
		// standard log package (the HTTP server's own error log,
		// for one) to this handler, at info.
		slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})))
	default:
		return fmt.Errorf("LOG_FORMAT %q is not a log format (use text or json)", format)
	}
	return nil
}

// ParseLevel reads a LOG_LEVEL value. Empty means info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("LOG_LEVEL %q is not a log level (use debug, info, warn or error)", s)
}

func logf(level slog.Level, format string, args ...any) {
	logger := slog.Default()
	ctx := context.Background()
	if !logger.Enabled(ctx, level) {
		return
	}
	logger.Log(ctx, level, fmt.Sprintf(format, args...))
}

// Errorf logs a failure that should not have happened.
func Errorf(format string, args ...any) { logf(slog.LevelError, format, args...) }

// Warnf logs a problem the application expects and handles.
func Warnf(format string, args ...any) { logf(slog.LevelWarn, format, args...) }

// Infof logs what the application is doing.
func Infof(format string, args ...any) { logf(slog.LevelInfo, format, args...) }

// Debugf logs detail that is only wanted while diagnosing.
func Debugf(format string, args ...any) { logf(slog.LevelDebug, format, args...) }
