// Command evesynapse is the EveSynapse web app: a personal EVE Online
// companion tool (character sheets, market, fitting, intel) backed by
// CCP's ESI API and EVE SSO.
//
// This is the release entrypoint. It intentionally does not import
// internal/devtools, so the release binary contains no dev-login
// code at all; DEV_LOGIN=1 has no effect here. For the local-dev
// build with /dev-login, see cmd/evesynapse-dev.
package main

import (
	"log"
	"net/http"
	"time"

	"evesynapse/internal/app"
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
		Handler:           application.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("evesynapse: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.Addr(), cfg.DBPath(), cfg.SSOConfigured())
	log.Fatal(srv.ListenAndServe())
}
