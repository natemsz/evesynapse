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
package main

import (
	"log"
	"net/http"
	"time"

	"evesynapse/internal/app"
	"evesynapse/internal/devtools"
)

func main() {
	cfg := app.LoadConfig()

	application, err := app.New(cfg)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer application.Close()

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           application.Handler(devtools.Register),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("evesynapse: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.Addr(), cfg.DBPath(), cfg.SSOConfigured())
	log.Fatal(srv.ListenAndServe())
}
