package app

// The anonymous-market switch's two layers (market_anon.go):
// MARKET_ANON=off is the boot default for a fresh install, and a
// stored site_settings row -- written by the Admin page's live
// flip -- wins over it once it exists.

import (
	"context"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestMarketAnonEnvDefault(t *testing.T) {
	t.Setenv("MARKET_ANON", "off")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.anonMarketOff {
		t.Fatal("MARKET_ANON=off did not set the boot default")
	}

	t.Setenv("MARKET_ANON", "")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.anonMarketOff {
		t.Fatal("MARKET_ANON unset left the boot default off, want on (open)")
	}
}

func TestMarketAnonStartupPrecedence(t *testing.T) {
	_, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	app := &Application{queries: q}

	// No stored row: the environment default decides.
	app.cfg = Config{}
	app.loadAnonMarketSetting(ctx)
	if app.anonMarketBlocked() {
		t.Fatal("no stored row and no env off: switch blocked, want open")
	}
	app.cfg = Config{anonMarketOff: true}
	app.loadAnonMarketSetting(ctx)
	if !app.anonMarketBlocked() {
		t.Fatal("no stored row and env off: switch open, want blocked")
	}

	// A stored row wins over the environment, both ways.
	if err := q.UpsertSiteSetting(ctx, db.UpsertSiteSettingParams{
		Key: siteSettingMarketAnon, Value: "on", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store on: %v", err)
	}
	app.loadAnonMarketSetting(ctx)
	if app.anonMarketBlocked() {
		t.Fatal("stored on under env off: switch blocked, want open (stored wins)")
	}
	if err := q.UpsertSiteSetting(ctx, db.UpsertSiteSettingParams{
		Key: siteSettingMarketAnon, Value: "off", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store off: %v", err)
	}
	app.cfg = Config{}
	app.loadAnonMarketSetting(ctx)
	if !app.anonMarketBlocked() {
		t.Fatal("stored off under env on: switch open, want blocked (stored wins)")
	}

	// The live flip persists and applies without a reload.
	if err := app.setAnonMarketOff(ctx, false); err != nil {
		t.Fatalf("flip on: %v", err)
	}
	if app.anonMarketBlocked() {
		t.Fatal("after the live flip on: switch blocked, want open")
	}
	row, err := q.GetSiteSetting(ctx, siteSettingMarketAnon)
	if err != nil || row.Value != "on" {
		t.Fatalf("stored setting after the flip: %+v err=%v, want value on", row, err)
	}
}
