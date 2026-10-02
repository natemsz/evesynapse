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
- `esi.go` — minimal ESI client (character/corporation lookups)
- `pages.go` — home/admin handlers + template rendering
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

The app auto-loads `./.env` at startup (keys already set in the real
environment win). Then open <http://localhost:8080>:

- `/` — home; signed out it shows the EVE SSO login button, signed in
  it shows the character sheet (name, portrait, corporation, birthday,
  security status)
- `/auth/eve` — starts EVE SSO login (also "Link another character")
- `/auth/callback` — OAuth2 callback (see SSO flow below)
- `/auth/logout` — destroys the session
- `/admin/` — users, linked characters, worker status (requires login)
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
   and redirects to `login.eveonline.com` requesting these scopes:
   `esi-skills.read_skills.v1`, `esi-skills.read_skillqueue.v1`,
   `esi-wallet.read_character_wallet.v1`, `esi-assets.read_assets.v1`.
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
5. **Character pull**: the signed-in home page fetches
   `GET https://esi.evetech.net/characters/{id}/` live with the stored
   access token (User-Agent `EveSynapse/0.1 (dev)`), resolves the
   corporation name via `GET /corporations/{id}/`, and shows the
   portrait from `images.evetech.net`.

Tokens, authorization codes and the client secret are never logged;
request logs contain paths only, no query strings.

Known limitations (by design, for now): ESI is fetched live per page
load (no caching yet — ESI caching is per-endpoint and belongs with
the worker's scheduler), and stored access tokens expire after ~20
minutes with no refresh yet, so the sheet gracefully degrades to
identity-only until the worker grows refresh-token handling.

## Dev login

`DEV_LOGIN=1` registers `/dev-login`, which flips the session to
signed-in without EVE SSO so the admin can be exercised locally. It
hands a session to anyone who asks: **never enable it on a deployment
anyone else can reach.** The server logs a loud warning at boot when
it is on.

## Database & sqlc

The app opens/creates the SQLite file from `DB_PATH`, creates the scs
`sessions` table, and — on first boot of a fresh database — applies
`schema/001_init.sql` (the `users`/`characters` tables). Regenerate
query code after editing `internal/db/query/queries.sql` with:

```sh
make gen   # sqlc generate
```

## Status / next steps

- [x] Wire EVE SSO: redirect, `/auth/callback`, JWT verification,
      token persistence
- [x] Character sheet basics on the home page (live ESI pull)
- [ ] Token refresh + worker-driven ESI caching honoring `cached_until`
- [ ] Fuller character sheet (skills, wallet, assets — scopes already
      requested at login)
- [ ] Import CCP SDE into side tables for the market/fitting modules
- [x] Multiple characters per account (link more while signed in)
