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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"evesynapse/internal/app"
	"evesynapse/internal/devtools"
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
		case "-migrate-pg":
			return app.RunMigratePG(args[1:], app.LoadConfig(), stdout, stderr)
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
	fmt.Fprintln(w, "  evesynapse-dev -update <url> [checksum] install a downloaded update and restart")
	fmt.Fprintln(w, "  evesynapse-dev -refresh                 refresh all cached data on next start (app must be stopped)")
}

// serve mirrors the release entrypoint's serve, plus the dev
// route hook: graceful shutdown on SIGTERM/SIGINT, pidfile for
// the self-restart hand-off.
func serve() int {
	cfg := app.LoadConfig()

	application, err := app.New(cfg)
	if err != nil {
		log.Printf("open db: %v", err)
		return 1
	}
	defer application.Close()

	pidfile := app.StartServerPidfile()
	defer app.RemoveServerPidfile(pidfile)

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           application.Handler(devtools.Register),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	log.Printf("evesynapse-dev: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.Addr(), cfg.DatabaseLabel(), cfg.SSOConfigured())

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("evesynapse-dev: server: %v", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		log.Printf("evesynapse-dev: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("evesynapse-dev: shutdown: %v", err)
		}
		return 0
	}
}
