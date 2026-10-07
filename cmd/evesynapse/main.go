// Command evesynapse is the EveSynapse web app: a personal EVE Online
// companion tool (character sheets, market, fitting, intel) backed by
// CCP's ESI API and EVE SSO.
//
// This is the release entrypoint. It intentionally does not import
// internal/devtools, so the release binary contains no dev-login
// code at all; DEV_LOGIN=1 has no effect here. For the local-dev
// build with /dev-login, see cmd/evesynapse-dev.
//
// With no arguments it runs the web app. Maintenance modes:
//
//	evesynapse -version                print the version and exit
//	evesynapse -update -arm64 / -x86   update to the latest release
//	                                   for this kind of computer
//	evesynapse -update <url> <sha256>  download, verify, and install
//	                                   a new build, restarting the
//	                                   running server onto it
//	evesynapse -refresh                mark all cached data out of
//	                                   date so the next start fetches
//	                                   fresh copies (server stopped)
//	                                   into Postgres (DATABASE_URL);
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
	"evesynapse/internal/logging"
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
			return app.RunUpdate(args[1:], stdout, stderr)
		case "-refresh":
			return app.RunRefresh(args[1:], app.LoadConfig(), stdout, stderr)
		case "-h", "--help", "-help":
			printUsage(stdout)
			return 0
		default:
			fmt.Fprintf(stderr, "evesynapse: unknown option %q\n\n", args[0])
			printUsage(stderr)
			return 2
		}
	}
	return serve()
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  evesynapse                          run the web app")
	fmt.Fprintln(w, "  evesynapse -version                 print the version and exit")
	fmt.Fprintln(w, "  evesynapse -update -arm64           update to the latest release for this computer (-x86 on Intel/AMD)")
	fmt.Fprintln(w, "  evesynapse -update -dev             update to the latest dev-branch release")
	fmt.Fprintln(w, "  evesynapse -update <url> <checksum> install a downloaded update and restart (a local file needs no checksum)")
	fmt.Fprintln(w, "  evesynapse -refresh                 refresh all cached data on next start (app must be stopped)")
}

// serve runs the web app until a termination signal (SIGTERM from
// the service manager or an update, SIGINT from a terminal) or a
// server error, then shuts down gracefully: HTTP drains first,
// then Close stops the worker and releases the database.
func serve() int {
	cfg := app.LoadConfig()
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

	pidfile := app.StartServerPidfile()
	defer app.RemoveServerPidfile(pidfile)

	srv := app.NewHTTPServer(cfg.Addr(), application.Handler())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	logging.Infof("evesynapse: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.Addr(), cfg.DatabaseLabel(), cfg.SSOConfigured())

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Errorf("evesynapse: server: %v", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		logging.Infof("evesynapse: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logging.Errorf("evesynapse: shutdown: %v", err)
		}
		return 0
	}
}
