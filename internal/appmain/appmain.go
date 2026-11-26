// Package appmain is the shared body of the server entrypoints: the
// command-line modes and the serve loop. cmd/evesynapse (release) and
// cmd/evesynapse-dev (local development, with dev-only routes) differ
// only in their name and the Hooks they pass. It never imports
// internal/devtools; the dev entrypoint passes those in.
package appmain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"evesynapse/internal/app"
	"evesynapse/internal/logging"
	"evesynapse/internal/pidfile"
	"evesynapse/internal/selfupdate"
)

// Hooks are an entrypoint's additions to the web app.
type Hooks struct {
	// Prepare runs on the constructed application before its
	// handler is built.
	Prepare func(*app.Application)
	// Routes are extra route hooks for the handler.
	Routes []app.RouteHook
	// RunNote follows "run the web app" in the usage text.
	RunNote string
}

// Run handles one command line: a maintenance mode when the first
// argument names one, otherwise the web app. name is the binary's
// name in messages and usage. It returns the process exit code.
func Run(name string, args []string, stdout, stderr io.Writer, hooks Hooks) int {
	if len(args) == 0 {
		return serve(name, hooks)
	}
	withConfig := func(mode func(cfg app.Config) int) int {
		cfg, err := app.LoadConfig()
		if err != nil {
			fmt.Fprintln(stderr, name+":", err)
			return 2
		}
		return mode(cfg)
	}
	rest := args[1:]
	switch args[0] {
	case "-version", "--version":
		fmt.Fprintln(stdout, app.Version())
		return 0
	case "-update":
		return selfupdate.Run(app.Version(), rest, stdout, stderr)
	case "-refresh":
		return withConfig(func(cfg app.Config) int { return app.RunRefresh(rest, cfg, stdout, stderr) })
	case "-populate-names":
		return withConfig(func(cfg app.Config) int { return app.RunPopulateNames(rest, cfg, stdout, stderr) })
	case "-seed-demo":
		return withConfig(func(cfg app.Config) int { return app.RunSeedDemo(rest, cfg, stdout, stderr) })
	case "-push-keys":
		return app.RunPushKeys(stdout, stderr)
	case "-wrap-token-key":
		return withConfig(func(cfg app.Config) int { return app.RunWrapTokenKey(rest, cfg, stdout, stderr) })
	case "-h", "--help", "-help":
		printUsage(stdout, name, hooks.RunNote)
		return 0
	default:
		fmt.Fprintf(stderr, "%s: unknown option %q\n\n", name, args[0])
		printUsage(stderr, name, hooks.RunNote)
		return 2
	}
}

func printUsage(w io.Writer, name, runNote string) {
	line := func(opts, what string) {
		fmt.Fprintf(w, "  %-*s %s\n", len(name)+25, name+opts, what)
	}
	fmt.Fprintln(w, "Usage:")
	line("", "run the web app"+runNote)
	line(" -version", "print the version and exit")
	line(" -update -arm64", "update to the latest release for this computer (-x86 on Intel/AMD)")
	line(" -update -dev", "update to the latest dev-branch release")
	line(" -update <url> <checksum>", "install a downloaded update and restart (a local file needs no checksum)")
	line(" -refresh", "refresh all cached data on next start (app must be stopped)")
	line(" -populate-names", "store every alliance and reachable corporation name now (safe while running)")
	line(" -seed-demo", "create or restore the shared demo account's seed data (safe while running)")
	line(" -push-keys", "print a new key pair for browser push, as lines for .env")
	line(" -wrap-token-key", "wrap TOKEN_ENCRYPTION_KEY with the OCI KMS key; prints TOKEN_KEY_WRAPPED")
}

// serve runs the web app until a termination signal (SIGTERM from
// the service manager or an update, SIGINT from a terminal) or a
// server error, then shuts down gracefully: HTTP drains first, then
// Close stops the worker and releases the database. The pidfile is
// the self-restart hand-off.
func serve(name string, hooks Hooks) int {
	cfg, err := app.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 2
	}
	if err := cfg.SetupLogging(); err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 2
	}

	application, err := app.New(cfg)
	if err != nil {
		logging.Errorf("%s: cannot start: %v", name, err)
		return 1
	}
	defer application.Close()

	pidPath := pidfile.Start()
	defer pidfile.Remove(pidPath)

	if hooks.Prepare != nil {
		hooks.Prepare(application)
	}
	srv := app.NewHTTPServer(cfg.Addr(), application.Handler(hooks.Routes...))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	logging.Infof("%s: listening on %s (db: %s, EVE SSO configured: %t)",
		name, cfg.Addr(), cfg.DatabaseLabel(), cfg.SSOConfigured())

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Errorf("%s: server: %v", name, err)
			return 1
		}
		return 0
	case <-ctx.Done():
		logging.Infof("%s: shutting down", name)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logging.Errorf("%s: shutdown: %v", name, err)
		}
		return 0
	}
}
