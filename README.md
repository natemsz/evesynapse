# EveSynapse (Go rewrite)

Modernization of the 2013 Django EveSynapse: a personal EVE Online
companion (character sheets, market, fitting, intel) rebuilt in Go on
CCP's **ESI** API with **EVE SSO** login, replacing the retired XML API
and key/vCode auth.

## Stack

- **Go** (module `evesynapse`; see `go.mod`)
- **chi** (`github.com/go-chi/chi/v5`) — HTTP router
- **alexedwards/scs v2** — sessions, stored in PostgreSQL via `pgxstore`
- **PostgreSQL 16** — the app's database, reached through
  **pgx/v5** (sqlc generates `database/sql` code over the pgx
  stdlib driver; a pgxpool backs the session store)
- **modernc.org/sqlite** — pure-Go SQLite driver (no cgo), kept
  only so the one-time `-migrate-pg` move can read a SQLite-era
  database file
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
  `killmails.go`, `intel.go`), the background worker (`worker.go`,
  plus `intel_worker.go` for the public-data pass), the
  SDE static-data importer (`sde.go`), and the self-maintenance
  modes (`maintenance.go`: `-version`, `-update`, `-refresh`;
  `migratepg.go`: the one-time `-migrate-pg` cutover)
- `internal/app/templates/` — embedded html/templates (`base.html`
  layout)
- `internal/app/static/` — embedded assets: the 2013 wallpaper
  (`bg.jpg`) and the dependency-free stylesheet (`style.css`), served
  at `/static/`
- `internal/app/schema_pg/` — the Postgres schema (sqlc input;
  `001_baseline.sql`), embedded for DB bootstrap
- `internal/app/schema/` — the original SQLite migrations
  (001–035), kept for the rollback binary and the `-migrate-pg`
  source reader
- `internal/pgtest/` — test-only embedded-Postgres provisioning
  (a fresh database per test; `go test ./...` needs no external
  database)
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

The release build needs a PostgreSQL 16 database to point
`DATABASE_URL` at; for a fresh local database the setup script
below provisions one, or create role+database yourself and put
the URL in `.env`. A fresh database gets the full schema from
the embedded baseline on first boot.

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
- `/intel/wars/` — current wars from the public war list and
  worker-warmed war details: aggressor/defender names, state,
  kill records, open-for-allies/mutual badges, and a flag on wars
  involving one of your characters' corporations (requires login;
  all data is public ESI)
- `/intel/incursions/` — active Sansha incursions: constellation,
  staging system, state, influence bar, boss presence and the
  affected systems (requires login; public ESI)
- `/intel/fw/` — faction warfare: per-faction summary cards
  (systems held, pilots, kills and victory points) and the most
  contested front-line systems with occupier/owner factions
  (requires login; public ESI)
- `/healthz` — plain `ok`
- `/dev-login` — dev-only fake sign-in, registered **only** when
  `DEV_LOGIN=1` (see below)

## Configuration

Everything comes from the environment (after `./.env` fills any
gaps; real environment variables win over the file):

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `EVE_CLIENT_ID` | yes | — | EVE SSO application client ID |
| `EVE_CLIENT_SECRET` | yes | — | EVE SSO application client secret |
| `EVE_CALLBACK_URL` | yes | `http://localhost:8080/auth/callback` | OAuth2 redirect URI; must match the callback registered at developers.eveonline.com character-for-character |
| `DATABASE_URL` | yes | `postgres://evesynapse@localhost:5432/evesynapse?sslmode=disable` | PostgreSQL connection URL |
| `ADDR` | no | `:8080` | HTTP listen address |
| `SESSION_KEY` | no | — | Reserved for cookie signing hardening |
| `EVESYNAPSE_UPDATE_REPO` | no | `natemsz/evesynapse` | GitHub repo (owner/repo) the updater checks |
| `EVE_SDE_BASE_URL` | no | Fuzzwork's dump | Base URL of the SDE CSV dump the importer downloads |
| `DEV_LOGIN` | no | — | Dev build only: `1` registers the `/dev-login` route |
| `DB_PATH` | no | `evesynapse.db` | Legacy SQLite file; read only by the one-time `-migrate-pg` move (see "Upgrading from a SQLite-era install") |

## Install

You need a Linux machine (ARM64 or x86-64) and an EVE app
registration for sign-in ("EVE SSO flow" below). Then:

1. **Get the program.** Easiest is the prebuilt binary from the
   [releases page](https://github.com/natemsz/evesynapse/releases):
   `evesynapse-arm64` for ARM machines (most ARM cloud instances),
   `evesynapse-amd64` for Intel/AMD ones. Or build it yourself
   ("Build from source" below).
2. **Run the setup script** from a checkout of this repo:

   ```sh
   sudo bash deploy/setup.sh
   ```

   It downloads the latest build for your machine and verifies it
   against the checksum published with the release, creates the
   `evesynapse` user and `/opt/evesynapse`, installs and starts
   PostgreSQL if it's missing and creates the app's database
   (role `evesynapse`, database `evesynapse`, with a generated
   password written into `/opt/evesynapse/.env` as `DATABASE_URL`;
   the file is chmod 600), installs the systemd service (ordered
   after `postgresql.service`), and links `evesynapse` into `/usr/bin` so you
   can run it without typing the full path. It never overwrites
   an existing `.env` (it only appends a `DATABASE_URL` that
   isn't there yet). Installing from a fork or from a build you
   made yourself works too:

   > **Why `/usr/bin`?** Updating always runs under `sudo`, and
   > `sudo` searches its own locked-down PATH — which does not
   > include `/usr/local/bin` on RHEL-family systems. A link in
   > `/usr/local/bin` works in your shell but is invisible to
   > `sudo evesynapse …`, so the setup script puts the link in
   > `/usr/bin` (and retires an older `/usr/local/bin` link if it
   > finds one).

   ```sh
   sudo EVESYNAPSE_UPDATE_REPO=you/evesynapse bash deploy/setup.sh
   sudo EVESYNAPSE_BINARY=/path/to/evesynapse bash deploy/setup.sh
   ```

3. **Fill in `/opt/evesynapse/.env`** with your EVE app's client
   ID, secret, and callback URL, then start it:

   ```sh
   sudo systemctl start evesynapse
   ```

   Open the address you configured and sign in with EVE.

Updating afterwards is one command — see "Updating" below.

## Build from source

Needs Go (the version in `go.mod`). The fonts and wallpaper
travel through the repo in encoded form (under `ci-assets/`), so
decode them into place first, then build:

```sh
make assets        # decode the fonts and wallpaper
make build         # bin/evesynapse for this machine
make build-arm64   # bin/evesynapse-arm64
make build-amd64   # bin/evesynapse-amd64
```

Install your build with:

```sh
sudo EVESYNAPSE_BINARY=$PWD/bin/evesynapse bash deploy/setup.sh
```

## Updating

Builds are published automatically: every push to `main` runs the
test suite in CI and, when `internal/app/version.txt` names a
version that has no release yet, publishes a GitHub release with
builds for ARM64 and AMD64 plus a small manifest per build (the
version and its SHA-256 checksum).

To update a running install, run the updater. With no flag it
automatically picks the build that matches the machine it's
running on:

```sh
sudo /opt/evesynapse/evesynapse -update          # right build for this machine
sudo /opt/evesynapse/evesynapse -update -arm64   # ARM build explicitly
sudo /opt/evesynapse/evesynapse -update -amd64   # Intel/AMD build (-x86 and -x64 also work)
```

The updater asks the latest release what version it carries and
compares it with its own. If they're the same, it just says so
and stops. If the release is newer, it downloads that build,
verifies it against the published checksum and checks it's built
for the right kind of computer, swaps it into place, restarts the
running server onto it, and reports the new version number.

There's also a manual form that installs from a specific address
(or local file), with an optional checksum:

```sh
sudo /opt/evesynapse/evesynapse -update <url|file> [sha256]
```

### Updating from your own fork

By default the updater checks the mainline repo's releases. To
have it check your fork instead, set this in
`/opt/evesynapse/.env` (the setup script writes it for you when
you install from a fork):

```sh
EVESYNAPSE_UPDATE_REPO=yourname/evesynapse
```

For your fork to publish releases the same way, enable Actions
in the fork (GitHub turns them off on new forks): the workflow
is already in the repo under `.github/workflows/`, and once it's
allowed to run, your pushes get tested, built, and released
exactly like the mainline ones.

## Command-line modes

```sh
evesynapse                          run the web app
evesynapse -version                 print the version and exit
evesynapse -update                  update to the latest release (right build for this machine)
evesynapse -update -arm64           update, fetching the ARM build
evesynapse -update -amd64           update, fetching the Intel/AMD build (-x86, -x64 also work)
evesynapse -update <url|file> [sha256]   install a specific build manually
evesynapse -refresh                 mark all cached data stale (run while the app is stopped)
evesynapse -h                       show this list
```

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

The Intel cluster (module sweep cluster 4, schema
`007_intel.sql`) is public ESI data — wars, incursions, faction
warfare and Tranquility status — so it doesn't ride the
per-character snapshot table. The worker keeps it in a small
global store instead: `global_snapshots` (one raw payload per
dataset, same `Expires`/`cached_until` contract) plus a
`war_details` store for `GET /wars/{war_id}/` payloads, warmed
newest-first at most 50 per cycle and refreshed while a war is
active (finished wars are immutable). War-party corporation and
alliance names and incursion constellation names warm into
in-process caches through the worker cycle's shared lookup
budget; faction names come from the stored factions list itself.
The Intel pages and the Home page's Tranquility line render from
the store only — and the worker pass runs even with no characters
linked, since none of it needs a token.

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
bounded per cycle) so those pages never wait on ESI. The same
cycle refreshes the public Intel store (server status, war list
and details, incursions, faction-warfare systems/stats, factions)
and the public names behind the Intel pages, spending from the
same per-cycle lookup budget.
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

## Performance principles

EveSynapse is built to stay lightweight, lean, fast, and efficient at
scale — without compromising features, power, or security. Two rules
govern how data moves:

1. **Precompute at ingest, never at render.** Expensive work (medians,
   bands, aggregates, rankings) happens once in the background worker
   when data arrives, and its results are stored. Page handlers never
   recompute what a scheduled job already computed.
2. **Bounded reads on every render.** A page handler may only read a
   bounded number of rows per render (on the order of ~100). No
   full-table scans, no unbounded sorts or filters in Go, no per-row
   queries in loops. Filtering, sorting, and ranking belong in SQL
   with a LIMIT; anything the page needs beyond that belongs in a
   stored table written at ingest time.

Data changes on a known schedule (market sweeps complete hourly;
character snapshots refresh on their own cadence), so a page that
re-derives everything per load is doing repeat work for an identical
answer. Under one user that is a CPU spike; under a hundred it is an
outage. When in doubt, push the work into the worker and store the
result.

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

The app runs on PostgreSQL 16 (the live deployment is 16.15;
tests self-provision an embedded Postgres 16, so CI needs no
database service). `openDB` connects with `DATABASE_URL` and, on
a database where the EveSynapse tables are absent, applies the
collapsed baseline in `internal/app/schema_pg/001_baseline.sql`
— the one-time fold of the 35 SQLite migrations into a single
Postgres schema (BIGINT/DOUBLE PRECISION keep the generated Go
models' int64/float64 types; timestamps stay app-written RFC3339
TEXT). Every query and the hand-rolled importer ride a
`database/sql` handle over the pgx/v5 stdlib driver; a pgxpool
exists only to back the scs `pgxstore` session store (sessions
live in the `sessions` table the baseline creates). Future
schema changes land as new numbered files in `schema_pg/`.
Regenerate query code after editing `internal/db/query/` with:

```sh
make gen   # sqlc generate
```

### Upgrading from a SQLite-era install

The binary carries a one-time migration mode for installs that
still run on SQLite (pre-v0.3.26):

```sh
sudo systemctl stop evesynapse
sudo /opt/evesynapse/evesynapse -update   # install the new build
sudo bash deploy/setup.sh                  # provisions Postgres + appends DATABASE_URL to .env
sudo /opt/evesynapse/evesynapse -migrate-pg
sudo systemctl start evesynapse
```

`-migrate-pg` reads the SQLite file named by `DB_PATH`
(read-only; it is never written), creates/verifies the Postgres
schema, copies every table in foreign-key-safe order, rewinds
the identity sequences, and verifies per-table row counts plus
a refresh-token spot check before it declares success. Sessions
are not migrated: everyone signs in once on the new build.
Re-running into a non-empty target is refused unless `-force`
is given (which truncates the target tables and re-copies).
Rollback is the previous binary plus the untouched SQLite file
and the old `.env`.

## Backups

Back the database up with `pg_dump` (and restore with `psql`):

```sh
pg_dump evesynapse > evesynapse-$(date +%F).sql
psql evesynapse < evesynapse-2026-10-05.sql   # into a fresh database
```

The generated SQL file plus `/opt/evesynapse/.env` is a complete
backup of an install.

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
- [x] Module sweep cluster 4 (intel): Wars, Incursions and
      Faction Warfare pages over a worker-warmed global public-data
      store (no token needed; war details bounded like killmail
      details), plus a Tranquility players-online line on Home —
      renders stay cache-only throughout
