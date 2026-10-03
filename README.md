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

- `cmd/evesynapse/` — release entrypoint: thin wiring (config → app →
  HTTP server). It does not import the devtools package, so the
  release binary contains no dev-login code; `DEV_LOGIN=1` has no
  effect on it.
- `cmd/evesynapse-dev/` — dev entrypoint: identical wiring plus the
  dev-only routes (`/dev-login`). Local testing only — never deploy
  this binary.
- `internal/app/` — the application: config + `.env` loader
  (`config.go`), EVE SSO auth/sessions/JWT verification (`auth.go`),
  token refresh (`refresh.go`), the application struct, router and DB
  bootstrap (`app.go`, `db.go`), page handlers and view models
  (`pages.go`, `assets.go`, `skills.go`, `corporation.go`,
  `market.go`, `sync.go`, `character.go`, `fittings.go`,
  `killmails.go`), the background worker (`worker.go`), the
  SDE static-data importer (`sde.go`), and the Termux DNS/CA shim
  (`netdns.go`)
- `internal/app/templates/` — embedded html/templates (`base.html`
  layout)
- `internal/app/static/` — embedded assets: the 2013 wallpaper
  (`bg.jpg`) and the dependency-free stylesheet (`style.css`), served
  at `/static/`
- `internal/app/schema/` — SQL schema (sqlc input; `001_init.sql`,
  `002_snapshots.sql`, `003_sde.sql`, `004_module_sweep.sql`),
  embedded for DB bootstrap
- `internal/esi/` — the ESI client: HTTP layer, per-character
  snapshot cache, and the two-tier type/group/place name resolution
  (network tier + cache-only render tier). Never imports
  `internal/app`; access tokens are injected via a callback.
- `internal/devtools/` — dev-only routes (`/dev-login`), imported
  solely by `cmd/evesynapse-dev`
- `internal/db/query/` — hand-written queries (sqlc input)
- `internal/db/sqlc/` — sqlc-generated code (do not edit)

## Run

```sh
cp .env.example .env   # fill in EVE_CLIENT_ID / EVE_CLIENT_SECRET
make run               # or: go build -o bin/evesynapse ./cmd/evesynapse && ./bin/evesynapse
```

`make run` runs the **release** build (`cmd/evesynapse`). If you fork
this project, that's the build you get by default — it has no
dev-login route at all. `make build` produces `bin/evesynapse`,
`make build-arm64` cross-compiles `bin/evesynapse-arm64`. For local
development there is also a dev build (`make build-dev`, binary
`bin/evesynapse-dev`, entrypoint `cmd/evesynapse-dev`) that
additionally registers `/dev-login` when started with `DEV_LOGIN=1`;
see "Dev login" below. Never deploy the dev build.

The app auto-loads `./.env` at startup (keys already set in the real
environment win). Then open <http://localhost:8080>:

- `/` — home; signed out it shows the EVE SSO login button, signed in
  it shows the character sheet (identity, wallet, skills, skill queue,
  plus a "Currently" block — system/station, active ship, online
  state — and a live countdown to the training skill's finish)
- `/character/` — character live state for one character: online
  status and login history, location, active ship, jump fatigue,
  active implants, home/jump clones with their implants (requires
  login; `esi-location.*`, `esi-clones.*`, `esi-characters.read_fatigue.v1`)
- `/fittings/` — saved ship fittings grouped by slot category
  (requires login; `esi-fittings.read_fittings.v1`)
- `/killmails/` — the 50 most recent kills/losses with KILL/LOSS
  badges, final-blow attacker, involved count and an estimated
  destroyed+dropped value (requires login;
  `esi-killmails.read_killmails.v1`; details warmed by the worker)
- `/corporations/` — public corporation overviews for the user's
  characters' corporations (name, CEO, alliance, tax, home station;
  public ESI, cached per CCP's `Expires` header)
- `/corporations/members/` — member roster with, when member
  tracking is available, join date, last login, current ship and
  location (roster: no role; tracking requires the **Director**
  role in-game)
- `/corporations/wallets/` — the seven wallet divisions with
  balances, plus the selected division's recent journal and
  transactions (requires the **Accountant** or **Junior
  Accountant** role in-game)
- `/corporations/orders/` — open corporation orders with item,
  location and region names, price, volume remaining and expiry
  (requires the **Accountant** or **Trader** role in-game)
- `/corporations/assets/` — corporation assets grouped by
  location, with player-given singleton names (named ships,
  renamed containers) resolved by the worker via the assets/names
  endpoint (requires the **Director** role in-game)
- `/corporations/structures/` — Upwell structures: type, system,
  state, fuel expiry, reinforce timers and service states
  (requires the **Station Manager** role in-game)
- `/corporations/killmails/` — the corporation's recent kills and
  losses, sharing the killmail store and page rendering with
  `/killmails/` (a victim in the corp is a LOSS; requires the
  **Director** role in-game)

  All corporation subpages follow the selected character's
  corporation (switch characters with `?character=`, like Assets),
  render from worker-warmed snapshots only, and show a plain
  "needs the role" state when ESI refused the dataset for want of
  an in-game role — see the caching section below.
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
- `/sync/` — sync status: worker state, per-character snapshot
  freshness and type-name coverage, with re-warm buttons, plus the
  SDE static-data block (row counts, import state, update check);
  the page auto-refreshes so an import can be watched as it lands
  (requires login)
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
(internal/app/refresh.go) returns the stored token while it has more
seconds left; otherwise it refreshes against CCP and persists the new
access token, the **rotated** refresh token, and the new expiry.
Refreshes are serialized process-wide and the character row is
re-read first, so a rotated refresh token is never replayed.

ESI responses for skills, skill queue, wallet and assets are cached as raw
JSON in `character_snapshots` (schema
`internal/app/schema/002_snapshots.sql`), keyed by
(character, kind) with the response's `Expires` header stored as
`cached_until` (5-minute fallback when ESI sends none). The module
sweep caches the character live-state endpoints the same way
(location, ship, online, clones, implants, fittings, fatigue and the
recent-killmails list). Killmail *details* are different: they are
immutable, so the worker warms them once per killmail into the
`killmail_details` store (schema `004_module_sweep.sql`, at most 10
per character per cycle) and pages read the store only. Corporation
datasets (module sweep cluster 2, schema `005_corp.sql`) ride the
same snapshot table as `corp_*` kinds, fetched with each viewing
character's token against that character's corporation — so pages
follow the selected character's corp, and character/corporation ID
collisions can never cross-contaminate. Role-gated corporation
endpoints answer 403 when the character lacks the in-game role;
the worker records that in `snapshot_fetch_state` (state
`role_missing` plus the role label), backs the kind off for six
hours instead of retrying every minute, and never writes a
snapshot for a refused kind — the subpages and the Sync page
render the recorded state ("Needs the Director role in-game")
until a fetch succeeds. Corp asset singleton names come from
`POST /corporations/{id}/assets/names/` (bounded to 1,000 items
per cycle, stored in `item_names`; CCP's `"None"` placeholder for
unnamed items is skipped). Assets are
paginated: every page is fetched and stored as one merged JSON array. Pages serve
fresh snapshots without calling ESI; on fetch failure a stale
snapshot is served instead of an error. ESI's error-limit statuses
(420/429) are treated as a hard back-off signal.

Name resolution is split in two tiers. Page renders resolve type,
group and place names from local data only — the in-process maps,
the SDE static-data tables (below), and the `type_names` fallback
table — a render never waits on ESI; anything still missing shows
as `Type #<id>` (or an "Ungrouped" skill section) until the
worker's warm-up pass fills it in. The interactive Market lookup
is the one exception: it may make a single name fetch for an item
nobody has cached yet.

## Static data (SDE)

Item, skill, group, station and system names come primarily from a
local copy of CCP's static data export (the SDE), stored in the
`sde_*` tables (schema `internal/app/schema/003_sde.sql`): types,
groups, categories, NPC stations, solar systems and regions. The
ESI drip-feed caches remain only as fallback for anything the SDE
lacks — notably player-structure names, which aren't in the dump.

- **Source**: [Fuzzwork's community CSV conversion](https://www.fuzzwork.co.uk/dump/)
  of the SDE. The six tables are fetched from
  `https://www.fuzzwork.co.uk/dump/latest/csv/` (plain `.csv`;
  `.csv.bz2` names and bzip2 content are also handled). Override
  with `EVE_SDE_BASE_URL` to use a mirror.
- **Import**: on first boot with empty SDE tables the worker
  imports automatically. The import downloads and parses all six
  files first, then replaces the tables in a single transaction —
  a failed import leaves the previous data untouched.
- **Cadence**: static data changes on patch days, not on ESI's
  cache clock. The worker checks the remote files' ETag /
  Last-Modified weekly and re-imports only when they changed.
- **Manual update**: the Sync page's "Check for SDE update" button
  runs the same check on demand (and imports when the dump moved);
  it also shows import state, per-table row counts, and each file's
  remote last-modified marker.

## Background worker

Every 60 seconds the worker walks all linked characters: it ensures
each access token is usable (refreshing when needed) and re-fetches
any snapshot whose `cached_until` has passed — a first pass runs at
boot. It also warms killmail details behind each character's recent
list and contract item lists behind the contracts list (both
bounded per cycle) so those pages never wait on ESI.
It then warms the name caches from the fresh snapshots (type
names and type→group links, group names, station/system names, and
character names for killmail victims and final-blow attackers) with
a small concurrent pool, capped per cycle; a huge account simply
converges over a few cycles. Characters are warmed first when they
were just linked via SSO or flagged on the Sync page. On a 420/429
the cycle stops and waits for the next tick. Logs stay quiet: one
summary line per cycle only when something was refreshed or failed,
plus a heartbeat every 10 minutes. The current status (last run,
summary, cumulative names resolved) shows on the Admin and Sync
pages.

## Look & feel

The UI echoes the 2013 EveSynapse theme — the original planet/nebula
wallpaper (served from `/static/bg.jpg`), teal-blue accents
(`#326b8c`), translucent dark panels over the art — rebuilt with a
single dependency-free stylesheet (`internal/app/static/style.css`): no Bootstrap,
no jQuery, no external fonts, responsive down to phone widths.

## Dev login

The dev build (`cmd/evesynapse-dev`, `make build-dev`) registers
`/dev-login` when started with `DEV_LOGIN=1`: it flips the session to
signed-in without EVE SSO so the admin can be exercised locally. The
handler lives in `internal/devtools` and is only wired into the dev
entrypoint — the release binary (`cmd/evesynapse`) doesn't contain
the code, and `DEV_LOGIN=1` has no effect on it. `/dev-login` hands
a session to anyone who asks: **never enable it on a deployment
anyone else can reach, and never deploy the dev build.** The dev
server logs a loud warning at boot when it is on.

## Database & sqlc

The app opens/creates the SQLite file from `DB_PATH`, creates the scs
`sessions` table, applies `internal/app/schema/001_init.sql` on first
boot of a fresh database (the `users`/`characters` tables), applies
`internal/app/schema/002_snapshots.sql` whenever the snapshot
tables are absent, `internal/app/schema/003_sde.sql` whenever
the SDE tables are absent, and `internal/app/schema/004_module_sweep.sql`
whenever the killmail-detail table is absent, and
`internal/app/schema/005_corp.sql` whenever the corporation
support tables (fetch-state log, character→corporation map, item
names) are absent (existing databases gain
the new tables
in place), and `internal/app/schema/006_economy.sql` whenever the
contract-detail table is absent. Regenerate query code after editing `internal/db/query/queries.sql`
with:

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
- [x] Cache-only renders: pages resolve names locally while the
      worker pre-warms snapshots + names; Sync page shows progress
- [x] Login requests the full read-only ESI scope set (63 scopes;
      mutating scopes excluded), matching the developer-portal app
- [x] Import CCP SDE into local tables (types/groups/categories/
      stations/systems/regions) as the primary name source for the
      market and character pages
- [x] Multiple characters per account (link more while signed in)
- [x] Module sweep cluster 1 (character): location/ship/online on
      the home sheet with a live training countdown, Character page
      (status, fatigue, implants, clones), Fittings page, Killmails
      page with worker-warmed immutable details
- [x] Module sweep cluster 2 (corporation): members + member
      tracking, wallets with per-division journal/transactions,
      orders, assets (with singleton names), structures, and corp
      killmails sharing cluster 1's store and rendering — all
      snapshot-cached with role-missing (403) states recorded
      instead of retried
- [x] Module sweep cluster 3 (economy): Wallet page (balance,
      bounded journal + transaction windows), Orders page (open +
      recent history), Contracts page (couriers routed, item lists
      warmed into a detail store like killmail details), Industry
      page (jobs incl. completed, blueprint library with BPO/BPC
      semantics, mining ledger)
