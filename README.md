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
- **golang.org/x/oauth2** — EVE SSO authorization-code flow
- **github.com/golang-jwt/jwt/v5** — SSO access-token verification
  against CCP's JWKS (issuer `login.eveonline.com`)
- **html/template** + `embed.FS` — server-rendered pages

## Layout

- `main.go` — entrypoint: DB open (schema bootstrap), router, server
- `config.go` — environment config + a small hand-rolled `.env` loader
- `auth.go` — EVE SSO: login redirect, callback, JWT/JWKS verification,
  sign-out, and the dev-only `/dev-login`
- `refresh.go` — access-token freshness: refresh + rotation persistence
- `esi.go` — ESI client with snapshot caching and type-name resolution
- `pages.go` — home/admin handlers + template rendering
- `assets.go` — assets browser (per-location stacks, switchable per character)
- `skills.go` — full skill sheet (every skill grouped by category,
  complete queue, switchable per character)
- `market.go` — market browser (public ESI: name search, price guide,
  regional order books)
- `worker.go` — background ESI refresh scheduler (60s cycle)
- `netdns.go` — Android/Termux DNS + embedded CA roots
- `templates/` — embedded html/templates (`base.html` layout)
- `static/` — embedded assets: the 2013 wallpaper (`bg.jpg`) and the
  dependency-free stylesheet (`style.css`), served at `/static/`
- `schema/` — SQL schema (sqlc input; `001_init.sql`, `002_snapshots.sql`)
- `internal/db/query/` — hand-written queries (sqlc input)
- `internal/db/sqlc/` — sqlc-generated code (do not edit)

## Run

```sh
cp .env.example .env   # fill in EVE_CLIENT_ID / EVE_CLIENT_SECRET
make run               # or: go build -o bin/evesynapse . && ./bin/evesynapse
```

The app auto-loads `./.env` at startup (keys already set in the real
environment win). Then open <http://localhost:8080>:

- `/` — home; signed out it shows the EVE SSO login button, signed in
  it shows the character sheet (identity, wallet, skills, skill queue)
- `/auth/eve` — starts EVE SSO login (also "Link another character")
- `/auth/callback` — OAuth2 callback (see SSO flow below)
- `/auth/logout` — destroys the session
- `/admin/` — users, linked characters, worker status (requires login)
- `/assets/` — asset browser: every stack grouped by location for the
  signed-in user's characters (requires login; `esi-assets.read_assets.v1`)
- `/market/` — market browser: item search (local name cache + exact
  ESI resolution), guide prices, and best/top orders for The Forge,
  Domain, Sinq Laison, Heimatar and Metropolis (requires login; all
  data is public ESI, order books capped at 20 pages)
- `/skills/` — full skill sheet: total/unallocated SP, level-V count,
  the complete training queue, and every known skill grouped by
  category with per-group SP subtotals (requires login;
  `esi-skills.read_skills.v1` + `esi-skills.read_skillqueue.v1`)
- `/healthz` — plain `ok`
- `/dev-login` — dev-only fake sign-in, registered **only** when
  `DEV_LOGIN=1` (see below)

Environment: `EVE_CLIENT_ID`, `EVE_CLIENT_SECRET`, `EVE_CALLBACK_URL`
(must match the callback registered at developers.eveonline.com
character-for-character), `SESSION_KEY`, plus optional `ADDR`
(default `:8080`) and `DB_PATH` (default `evesynapse.db`).

## EVE SSO flow

1. **Register the app** at <https://developers.eveonline.com> with the
   callback URL from `EVE_CALLBACK_URL`, and put the issued client ID
   and secret in `.env`.
2. **Login**: `GET /auth/eve` stores a random `state` in the session
   and redirects to `login.eveonline.com` requesting the full
   read-only ESI scope set (63 scopes from the OAuth2 catalog in
   CCP's ESI OpenAPI document; every mutating scope — names
   containing `write_`, `send_`, `respond_`, `organize_`, `manage_`
   or `open_window` — is excluded). The one-time subset the app
   originally requested (`esi-skills.read_skills.v1`,
   `esi-skills.read_skillqueue.v1`,
   `esi-wallet.read_character_wallet.v1`,
   `esi-assets.read_assets.v1`) is contained in that set; characters
   linked before the expansion keep their granted scopes until
   re-linked.
3. **Callback**: `GET /auth/callback` verifies the `state`
   (constant-time, single-use), exchanges the authorization code for
   tokens, then verifies the access-token JWT: RS256 signature against
   CCP's JWKS (found via the OAuth discovery document), `iss` =
   `login.eveonline.com`. Character ID/name come from the `sub`/`name`
   claims.
4. **Persist**: first login creates a row in `users`; the character is
   upserted into `characters` with the access/refresh tokens, expiry,
   and granted scopes. Re-login (or "Link another character" while
   signed in) attaches characters to the same account. The session
   token is rotated at sign-in.
5. **Character pull**: the signed-in home page shows the character
   sheet — identity (name, portrait, corporation, birthday, security
   status), ISK wallet balance, total/unallocated SP, the currently
   training skill, and the 25 heaviest skills with names resolved via
   `GET /universe/types/{id}/` (cached in-process and in the
   `type_names` table).

Tokens, authorization codes and the client secret are never logged;
request logs contain paths only, no query strings.

## Token refresh & caching

EVE SSO access tokens live ~20 minutes. `validAccessToken`
(refresh.go) returns the stored token while it has more than 60
seconds left; otherwise it refreshes against CCP and persists the new
access token, the **rotated** refresh token, and the new expiry.
Refreshes are serialized process-wide and the character row is
re-read first, so a rotated refresh token is never replayed.

ESI responses for skills, skill queue, wallet and assets are cached as raw
JSON in `character_snapshots` (schema `002_snapshots.sql`), keyed by
(character, kind) with the response's `Expires` header stored as
`cached_until` (5-minute fallback when ESI sends none). Assets are
paginated: every page is fetched and stored as one merged JSON array. Pages serve
fresh snapshots without calling ESI; on fetch failure a stale
snapshot is served instead of an error. ESI's error-limit statuses
(420/429) are treated as a hard back-off signal.

## Background worker

Every 60 seconds the worker walks all linked characters: it ensures
each access token is usable (refreshing when needed) and re-fetches
any snapshot whose `cached_until` has passed — a first pass runs at
boot. On a 420/429 the cycle stops and waits for the next tick. Logs
stay quiet: one summary line per cycle only when something was
refreshed or failed, plus a heartbeat every 10 minutes.

## Look & feel

The UI echoes the 2013 EveSynapse theme — the original planet/nebula
wallpaper (served from `/static/bg.jpg`), teal-blue accents
(`#326b8c`), translucent dark panels over the art — rebuilt with a
single dependency-free stylesheet (`static/style.css`): no Bootstrap,
no jQuery, no external fonts, responsive down to phone widths.

## Dev login

`DEV_LOGIN=1` registers `/dev-login`, which flips the session to
signed-in without EVE SSO so the admin can be exercised locally. It
hands a session to anyone who asks: **never enable it on a deployment
anyone else can reach.** The server logs a loud warning at boot when
it is on.

## Database & sqlc

The app opens/creates the SQLite file from `DB_PATH`, creates the scs
`sessions` table, applies `schema/001_init.sql` on first boot of a
fresh database (the `users`/`characters` tables), and applies
`schema/002_snapshots.sql` whenever the snapshot tables are absent
(existing databases gain the new tables in place). Regenerate
query code after editing `internal/db/query/queries.sql` with:

```sh
make gen   # sqlc generate
```

## Status / next steps

- [x] Wire EVE SSO: redirect, `/auth/callback`, JWT verification,
      token persistence
- [x] Character sheet on the home page (identity, wallet, skills,
      skill queue) via cached ESI snapshots
- [x] Token refresh + worker-driven ESI caching honoring `cached_until`
- [x] 2013 look & feel (original wallpaper, dark panels, teal accents)
- [x] Assets page: every stack grouped by location, worker-refreshed
      (scope already requested at login)
- [x] Market page: type search, guide prices and regional order
      books via public ESI (no scope needed)
- [x] Skill sheet page: every skill grouped by category, full
      queue, per-group totals (from the cached snapshots)
- [x] Login requests the full read-only ESI scope set (63 scopes;
      mutating scopes excluded), matching the developer-portal app
- [ ] Import CCP SDE into side tables for the market/fitting modules
- [x] Multiple characters per account (link more while signed in)
