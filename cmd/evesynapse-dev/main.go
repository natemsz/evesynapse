// Command evesynapse-dev is the local-development entrypoint for
// EveSynapse. It wires the application exactly like the release
// entrypoint (cmd/evesynapse) and additionally registers the
// dev-only routes from internal/devtools (today: /dev-login, only
// active when DEV_LOGIN=1).
//
// !!! Local testing only. /dev-login hands out a signed-in session
// to anyone who asks; never deploy this binary anywhere anyone else
// can reach. Forkers and production deployments should use
// cmd/evesynapse, which contains no devtools code at all. !!!
//
// The maintenance modes (-version, -update, -refresh) work here
// too; -update installs over this dev binary and the restart
// hand-off targets whichever server wrote the pidfile.
package main

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
	"evesynapse/internal/devtools"
	"evesynapse/internal/logging"
	"evesynapse/internal/pidfile"
	"evesynapse/internal/selfupdate"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "-version", "--version":
			fmt.Fprintln(stdout, app.Version())
			return 0
		case "-update":
			return selfupdate.Run(app.Version(), args[1:], stdout, stderr)
		case "-refresh":
			cfg, err := app.LoadConfig()
			if err != nil {
				fmt.Fprintln(stderr, "evesynapse-dev:", err)
				return 2
			}
			return app.RunRefresh(args[1:], cfg, stdout, stderr)
		case "-h", "--help", "-help":
			printUsage(stdout)
			return 0
		default:
			fmt.Fprintf(stderr, "evesynapse-dev: unknown option %q\n\n", args[0])
			printUsage(stderr)
			return 2
		}
	}
	return serve()
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  evesynapse-dev                          run the web app (with dev routes)")
	fmt.Fprintln(w, "  evesynapse-dev -version                 print the version and exit")
	fmt.Fprintln(w, "  evesynapse-dev -update <url> <checksum> install a downloaded update and restart (a local file needs no checksum)")
	fmt.Fprintln(w, "  evesynapse-dev -refresh                 refresh all cached data on next start (app must be stopped)")
}

// serve mirrors the release entrypoint's serve, plus the dev
// route hook: graceful shutdown on SIGTERM/SIGINT, pidfile for
// the self-restart hand-off.
func serve() int {
	cfg, err := app.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "evesynapse-dev:", err)
		return 2
	}
	if err := cfg.SetupLogging(); err != nil {
		fmt.Fprintln(os.Stderr, "evesynapse:", err)
		return 2
	}

	application, err := app.New(cfg)
	if err != nil {
		logging.Errorf("evesynapse: cannot start: %v", err)
		return 1
	}
	defer application.Close()

	pidPath := pidfile.Start()
	defer pidfile.Remove(pidPath)

	srv := app.NewHTTPServer(cfg.Addr(), application.Handler(devtools.Register))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	logging.Infof("evesynapse-dev: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.Addr(), cfg.DatabaseLabel(), cfg.SSOConfigured())

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Errorf("evesynapse-dev: server: %v", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		logging.Infof("evesynapse-dev: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logging.Errorf("evesynapse-dev: shutdown: %v", err)
		}
		return 0
	}
}
