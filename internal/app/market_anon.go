package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The anonymous-market switch: anonymous visitors get the Market
// page's order book from stored sweep data and may queue market
// history wants (market wants only -- every other anonymous want
// stays blocked, as it always was). The owner can turn that off
// under high load, live, from the Admin page: no restart, because
// high load is exactly when a restart is least welcome.
//
// Two layers decide the state:
//   - MARKET_ANON=off in the environment is the boot default for
//     a fresh install (core_config.go).
//   - A site_settings row (key below), once an admin has flipped
//     the switch, wins over the environment and survives restarts.
//
// The Application keeps the effective state in an atomic; every
// market check reads it through anonMarketBlocked, never the raw
// config field, so a flip lands on the very next request.
// ---------------------------------------------------------------------------

// siteSettingMarketAnon is the site_settings key for the switch.
// Its value is "on" (anonymous market open) or "off" (blocked).
const siteSettingMarketAnon = "market_anon"

// anonMarketBlocked reports whether anonymous market access is
// currently switched off: the stored order book is not served to
// anonymous visitors and their market renders queue no wants.
func (app *Application) anonMarketBlocked() bool {
	return app.anonMarketOffState.Load()
}

// loadAnonMarketSetting settles the switch at startup: the stored
// setting when one exists, the environment default otherwise. A
// database that cannot be read falls back to the environment
// default rather than failing the boot.
func (app *Application) loadAnonMarketSetting(ctx context.Context) {
	off := app.cfg.anonMarketOff
	if row, err := app.queries.GetSiteSetting(ctx, siteSettingMarketAnon); err == nil {
		switch row.Value {
		case "off":
			off = true
		case "on":
			off = false
		default:
			logging.Warnf("market-anon: stored setting %q is not on/off; using the MARKET_ANON default", row.Value)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("market-anon: read stored setting: %v (using the MARKET_ANON default)", err)
	}
	app.anonMarketOffState.Store(off)
	if off {
		logging.Infof("market-anon: anonymous market access is OFF (stored book and market wants blocked for anonymous visitors)")
	}
}

// setAnonMarketOff flips the switch: the new state is persisted
// first (so a restart keeps it), then held live, so the flip
// takes effect on the next request without a restart.
func (app *Application) setAnonMarketOff(ctx context.Context, off bool) error {
	value := "on"
	if off {
		value = "off"
	}
	if err := app.queries.UpsertSiteSetting(ctx, db.UpsertSiteSettingParams{
		Key: siteSettingMarketAnon, Value: value, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	app.anonMarketOffState.Store(off)
	return nil
}
