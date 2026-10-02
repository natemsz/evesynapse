# EveSynapse (Go rewrite)

Modernization of the 2013 Django EveSynapse: a personal EVE Online
companion (character sheets, market, fitting, intel) rebuilt in Go on
CCP's **ESI** API with **EVE SSO** login, replacing the retired XML API
and key/vCode auth.

## Stack

- **Go** (module `evesynapse`; see `go.mod`)
- **chi** (`github.com/go-chi/chi/v5`) — HTTP router
- **alexedwards/scs v2** — sessions, stored in SQLite via `sqlite3store`
- **modernc.org/sqlite** — pure-Go SQLite driver (no cgo)
- **sqlc** — type-safe Go generated from hand-written SQL
- **golang.org/x/oauth2** — EVE SSO (wiring in progress)
- **html/template** + `embed.FS` — server-rendered pages

## Layout

- `main.go` — entrypoint: config, DB open, router, server
- `pages.go` — home/admin handlers + template rendering
- `auth.go` — EVE SSO OAuth2 config + login stubs (incl. a dev-only
  `/dev-login` that must be removed before public deployment)
- `worker.go` — background goroutine (future ESI refresh scheduler)
- `templates/` — embedded html/templates (`base.html` layout)
- `schema/` — SQL schema (sqlc input; `001_init.sql`)
- `internal/db/query/` — hand-written queries (sqlc input)
- `internal/db/sqlc/` — sqlc-generated code (do not edit)

## Run

```sh
cp .env.example .env   # fill in EVE_CLIENT_ID / EVE_CLIENT_SECRET
make run               # or: go build -o bin/evesynapse . && ./bin/evesynapse
```

Then open <http://localhost:8080>:

- `/` — home, shows login state and the EVE SSO button (stub)
- `/admin/` — placeholder admin (behind an auth stub; in dev, hit
  `/dev-login` to flip the session flag and see it)
- `/healthz` — plain `ok`

Environment: `EVE_CLIENT_ID`, `EVE_CLIENT_SECRET`, `SESSION_KEY`,
plus optional `ADDR` (default `:8080`) and `DB_PATH` (default
`evesynapse.db`).

## Database & sqlc

The app opens/creates the SQLite file from `DB_PATH` and creates the
scs `sessions` table on boot. The `users`/`characters` tables are
defined in `schema/001_init.sql`; regenerate query code with:

```sh
make gen   # sqlc generate
```

## Status / next steps

- [ ] Wire EVE SSO: redirect to EVE, `/auth/callback`, verify the
      character, persist tokens (the `characters` table already has
      the fields)
- [ ] Character sheet page fed by ESI (replaces the 2013 XML
      CharacterSheet)
- [ ] Worker: scheduled ESI refresh honoring `cached_until`
- [ ] Import CCP SDE into side tables for the market/fitting modules
- [ ] Multiple characters per account (schema already models it)
