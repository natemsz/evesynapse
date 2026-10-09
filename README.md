# EveSynapse (Go rewrite)

EveSynapse is a lightweight and fast, yet comprehensive and powerful all-in-one EVE Online companion (character sheets, market, fitting, intel, industry, etc) rebuilt in Go on CCP/Fenris Creation's ESI API with EVE SSO login.

The modernization is developed by natemsz (Nate / IGN: Burzrujat), rewriting the original Python-based closed-source app module by module in Go. Not all modules have been finished but development is very active. Built in the hopes it might be useful to the players of this game that holds a special place in my heart and other third party developers for their own projects.

EveSynapse is based on the 2013 project originally developed by natemsz, element, and j0ker (Rest in peace Matt. See you on the other side of the Eve Gate). Without them, the original project would not have been possible and this rewrite would not exist. While they have not contributed to the modernization, their work was critically important to both it's development and to my skills as a developer today. Contributions from Sahir (UI) and ado (helping me fix my own mistakes and answering questions! thank you)

This project is dedicated to EVE Online, the game and community that I have loved for over two decades. To its pilots and its developers, past, present, and future; and to all those who we have lost over the years o7.

While there is a hosted version (https://evesynapse.app) you are welcome to install this on your local machine or as a corporation/alliance service (see below on how to restrict sign-ups). If you have suggestions, concerns, questions or complaints, feel free to shoot me an email.

Contact: nate@synap6.io or in-game 'Burzrujat'

## AI Use Disclosure and Policy
- AI is used in this project purely for security auditing, hardening, and performance improvements, writing tests, certain UI elements such as icons and glyphs (I'm no artist), and to assist with simplifying repetitive tasks. This is a human-built project built for other humans, and a labor of love. 

## Stack

- **Go** (module `evesynapse`; see `go.mod`)
- **chi** (`github.com/go-chi/chi/v5`) — HTTP router
- **alexedwards/scs v2** — sessions, stored in PostgreSQL via `pgxstore`
- **PostgreSQL 16** — the app's database, reached through
  **pgx/v5** (sqlc generates `database/sql` code over the pgx
  stdlib driver; a pgxpool backs the session store)
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
- `cmd/releasesign/` — the maintainer's tool for release signing:
  makes the release key, signs the update manifests in the release
  job, checks a signature by hand. Never deployed either
- `cmd/mkicons/` — draws the raster icons from the logo's SVG files in
  `internal/app/static/`: `logo.svg` (the mark, shown in the page
  header), `icon.svg` (the app icon) and `favicon.svg` (the simplified
  small one). To change the logo, replace those and run
  `go run ./cmd/mkicons`: it rewrites `favicon.ico`, the four PNG app
  icons and the notification badge beside them
- `internal/app/` — the application: config + `.env` loader
  (`config.go`), EVE SSO auth/sessions/JWT verification (`auth.go`),
  account linking and the sign-up policy (`links.go`, `signup.go`),
  token refresh and encryption at rest (`refresh.go`,
  `tokencrypt.go`), cookie, header and cross-site protections
  (`httpsec.go`), the application struct and router (`app.go`),
  page handlers and view models
  (`pages.go`, `assets.go`, `skills.go`, `corporation.go`,
  `market.go`, `sync.go`, `character.go`, `fittings.go`,
  `killmails.go`, `intel.go`), the background worker (`worker.go`
  runs the cycle; each `*_worker.go` file is the fetching behind one
  group of pages, and `name_harvest.go` collects the names a cycle
  has to resolve), what pages and the worker both read
  (`snapshots.go`, `market_book.go`), the
  SDE static-data importer (`sde.go`), static assets and their
  caching (`static.go`), the health check (`health.go`), and the
  two maintenance modes that need the application (`maintenance.go`:
  `-version` and `-refresh`)
- `internal/app/templates/` — embedded html/templates (`base.html`
  layout)
- `internal/app/static/` — embedded assets: the 2013 wallpaper
  (`bg.jpg`) and the dependency-free stylesheet (`style.css`), served
  at `/static/`
- `internal/store/` — the database: opens Postgres and brings the
  schema up to date at startup (`store.go`, `schema.go`). The schema
  itself is the numbered steps in `internal/store/schema_pg/`, which
  are also sqlc's input
- `internal/fit/` — the fitting simulator's stat engine: the dogma
  arithmetic that turns a ship, its modules, skills and charges into
  the fit's statistics. Pure over the static data it loads; the
  fitting pages in `internal/app` are its only caller
- `internal/skillplan/` — skill-plan arithmetic: SP per level,
  training speed, ordering targets with their prerequisites, the
  remap advisor. Pure; the skill pages supply the skill graph
- `internal/buildplan/` — the industry build planner's engine: the
  tree of everything a product needs, the manufacturing formulae,
  what is already held, and the price of the rest. Pure
- `internal/markethistory/` — figures and the SVG chart computed
  from a type's stored daily price history. Pure
- `internal/selfupdate/` — `evesynapse -update`: the release channel,
  checking a release's signature, downloading, verifying and swapping
  in a new build, and handing over to the running server. It is told
  the running version and knows nothing else about the application
- `internal/releasesig/` — the signature on a release: how an update
  manifest is signed and how the updater checks one, plus
  `trusted_keys.pem`, the public keys compiled into every build
  ("Release signing" below)
- `internal/pidfile/` — the server's pidfile: written at start-up,
  read by `-update` to restart the server and by `-refresh` to refuse
  while it is running (`pidfile/pidfiletest` is test support)
- `internal/dotenv/` — the `.env` loader the server and the updater
  both use
- `internal/pgtest/` — test-only embedded-Postgres provisioning
  (a fresh database per test; `go test ./...` needs no external
  database). A package whose tests use it needs a `TestMain` that
  calls `pgtest.TestMain`, which stops the server when the tests
  finish; without one pgtest refuses to start a server
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
- `/auth/logout` — destroys the session (POST; the sidebar's Sign out button)
- `/admin/` — users, linked characters, worker status (requires an admin account; see `EVE_ADMIN_CHARACTER_IDS`)
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
  (requires an admin account)
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
- `/healthz` — `ok` (200) when the database answers and the background
  worker is running; 503 with one line per problem otherwise
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
| `TOKEN_ENCRYPTION_KEY` | no | — | Encrypts the EVE tokens stored in the database (see "Token encryption") |
| `EVE_ADMIN_CHARACTER_IDS` | no | — | Comma-separated EVE character IDs whose accounts may open the Admin and Sync pages. Any character linked to an account makes that whole account an admin's. Empty = nobody |
| `EVE_ALLOWED_CHARACTER_IDS`, `EVE_ALLOWED_CORPORATION_IDS`, `EVE_ALLOWED_ALLIANCE_IDS` | no | — | Limit who may create an account (see "Who can sign up"). All empty = anyone who can sign in with EVE |
| `EVESYNAPSE_UPDATE_REPO` | no | `natemsz/evesynapse` | GitHub repo (owner/repo) the updater checks |
| `EVE_SDE_BASE_URL` | no | Fuzzwork's dump | Base URL of the SDE CSV dump the importer downloads |
| `ESI_CONTACT` | no | — | How CCP can reach whoever runs this instance (an email address, a Discord handle, a character name). Sent in the User-Agent of every ESI request, as CCP asks of third-party apps |
| `LOG_LEVEL` | no | `info` | Least severe kind of log line written: `debug`, `info`, `warn` or `error` (see "Logging") |
| `LOG_FORMAT` | no | `text` | `text` for the classic line, `json` for one object per line |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` | no | — | The key pair that turns on browser push notifications (see "Notifications and browser push"). Make one with `evesynapse -push-keys`. Unset, notifications show in the top bar only |
| `VAPID_SUBJECT` | no | the site's address | A contact address (`mailto:` or `https:`) the browsers' push services may use to reach the operator |
| `OPS_MANAGER_ROLES` | no | `Director` | The in-game corporation roles whose holders may create, change and cancel ops on the calendar, comma separated and spelled as ESI spells them (`Director,Personnel_Manager`) |
| `NOTIFY_POLL_SECONDS` | no | `30` | How often, in seconds, an open page checks whether its notifications icon has changed, so new notifications show without a reload. `0` turns the checks off; other values are kept between 5 and 3600. Takes effect on restart |
| `DISCORD_CLIENT_ID`, `DISCORD_CLIENT_SECRET` | no | — | A Discord application's id and secret: turns on "Connect Discord" (see "Discord") |
| `DISCORD_BOT_TOKEN` | no | — | That application's bot: lets it be added to Discord servers, where it gives roles and sends messages. What it does in each server is set on the site by that server's directors, not here |
| `DEV_LOGIN` | no | — | Dev build only: `1` registers the `/dev-login` route |

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

   It downloads the latest build for your machine, checks the
   release's signature and verifies the build against the checksum
   in its signed manifest ("Release signing" below; on a machine
   whose OpenSSL is older than 3.0 it can check only the checksum,
   and says so), creates the `evesynapse` user and
   `/opt/evesynapse`, installs and starts
   PostgreSQL if it's missing and creates the app's database
   (role `evesynapse`, database `evesynapse`, with a generated
   password written into `/opt/evesynapse/.env` as `DATABASE_URL`),
   installs the systemd service (ordered after
   `postgresql.service`), and links `evesynapse` into `/usr/bin` so you
   can run it without typing the full path. It never overwrites
   an existing `.env` (it only appends a `DATABASE_URL` that
   isn't there yet).

   The program, `/opt/evesynapse` and `.env` belong to root; the
   service account can read them but not change them, and the
   unit runs it sandboxed with `/run/evesynapse` as its only
   writable directory. Updates run as root, so the account the
   server runs as must not be able to replace what root runs.
   If you installed before this layout, run the script once more
   to move to it. Installing from a fork or from a build you
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

   Run the fork form from a checkout of the fork: the script checks
   the release against the release key in the checkout it is run
   from, and a fork's releases are signed with the fork's own key.

3. **Fill in `/opt/evesynapse/.env`** with your EVE app's client
   ID, secret, and callback URL, then start it:

   ```sh
   sudo systemctl start evesynapse
   ```

   Open the address you configured and sign in with EVE.

Updating afterwards is one command — see "Updating" below.


## Who can sign up

Out of the box, anyone who can reach the site and sign in with
EVE gets an account, and the worker then keeps every character
they link in sync. To limit an instance to the people it is meant
for, list them in `.env` (comma-separated EVE IDs; any mix):

```sh
EVE_ALLOWED_CHARACTER_IDS=90000001,90000002
EVE_ALLOWED_CORPORATION_IDS=98000001
EVE_ALLOWED_ALLIANCE_IDS=99000001
```

A new account then needs a character that is listed itself or
whose corporation or alliance is (checked against EVE's public
API at sign-in). Characters in `EVE_ADMIN_CHARACTER_IDS` are
always allowed. Everyone else is turned away at sign-in with a
message, and no account is created.

The lists decide who may *join*:

- someone who already has an account can still link more
  characters to it, including alts outside the lists;
- accounts created before the lists were set keep working.

A list that is set but cannot be read (a typo, a stray
separator) stops the app at startup rather than silently
allowing everyone.

## Notifications and browser push

EveSynapse tells you when something happens to your characters:
a skill finishes, new mail, a new in-game calendar event, a new op
planned for one of your corporations, a reminder before an op you
signed up to starts (10 minutes to 2 hours ahead, your choice), a new
killmail, planetary
extractors stopping, an industry job finishing, and watch list
alerts. They show under the bell in the top bar with no setup at
all. Under **bell → Settings** (`/notifications/settings`) each kind
can be switched off altogether, or for some of your characters: the
list beside a kind has a tick box per character and can be searched
by name. A character linked later starts with everything on.

Browser push delivers the same notifications while no EveSynapse
tab is open, including to a phone with the site installed to its
home screen. It is off until the server has a key pair, and each
browser has to be switched on by hand. Nothing asks for
permission on its own: not opening the site, and not installing
it as an app.

### Turn push on for the server (once)

1. On the server, make a key pair:

   ```sh
   evesynapse -push-keys
   ```

   It prints three lines starting `VAPID_PUBLIC_KEY=`,
   `VAPID_PRIVATE_KEY=` and `VAPID_SUBJECT=`. Nothing is saved
   anywhere; the lines are only printed.

2. Paste those three lines into the server's `.env`
   (`/opt/evesynapse/.env` on an install made by the setup
   script), and change `VAPID_SUBJECT` to an address you can be
   reached at, such as `mailto:you@example.org`.

3. Restart:

   ```sh
   sudo systemctl restart evesynapse
   ```

Keep the private key secret, and do not replace the pair later:
every browser that switched push on would have to do it again.
The site has to be served over https (see "HTTPS").

### Turn push on in a browser (each browser, each device)

1. Sign in, click the bell, then **Settings**.
2. Under **Browser notifications**, click **Turn on in this
   browser**, and choose **Allow** when the browser asks.
3. Click **Send a test**. A notification reading "Browser
   notifications are working." should appear within a few
   seconds.

On an iPhone or iPad, first add EveSynapse to the Home Screen
(Share → Add to Home Screen) and do these steps in the app that
opens from there; Safari does not offer push to an ordinary tab.

### If it does not work

| What you see | What it means |
|--------------|---------------|
| "Browser notifications are not set up on this server." | The server did not find a usable key pair. Check the three lines are in the `.env` the service reads and that it was restarted. A pair that is set but wrong is reported in the log as `browser push is off` |
| "This browser cannot receive push notifications." | The browser has no push support. On iOS, open the installed app, not a Safari tab |
| "Notifications from this site are blocked in this browser." | Permission was refused earlier. Allow notifications for the site in the browser's site settings, then reload the page |
| "The test could not be sent: no browser took the test message" | The push service refused the message. The reason is in the server log (`journalctl -u evesynapse`), on a line starting `push:` |
| The test says sent, but nothing appears | The operating system is hiding it: check its notification settings for the browser, and Do Not Disturb / Focus |

### What does and does not notify

- Notifications are worked out after each background sync, so
  one arrives some minutes after the thing happened in-game,
  not at that instant.
- The first sync of a character only records what is already
  there. Mail that was in the inbox before then is not announced.
- Mail a character sends to itself is not announced, and neither
  is mail already read.
- A kind switched off in Settings is switched off for push too.
- When more than three things happen at once, push sends a single
  message with a count instead of one each.

## Discord

Optional. EveSynapse can connect a user's Discord account to their
EveSynapse account, send their notifications to them on Discord, and,
through one bot that can be added to any number of servers, give roles
there and post new ops in a channel. It only ever sends to Discord; it
reads nothing from any channel.

What the bot does in a server is not part of this install's settings.
It is set on the site, per server, by the directors of the corporation
or alliance the server belongs to.

### Set the bot up (once, by whoever runs the site)

1. At <https://discord.com/developers/applications>, create an
   application. If Discord asks what it is for, "Build a Bot" is the
   one; the Social SDK is not used.
2. On **General Information**, copy the **Application ID**. That is the
   client id. (The Public Key on that page is not used.)
3. Under **OAuth2**, press **Reset Secret** and copy the **Client
   Secret**, and add this redirect, with your site's address:
   `https://your.site/discord/callback`
4. Under **Bot**, press **Reset Token** and copy the token. No
   privileged intents are needed; leave them off. Leave **Public Bot**
   on if other corporations and alliances are to add it to their
   servers.
5. Add to the server's `.env` (`/opt/evesynapse/.env`) and restart
   (`sudo systemctl restart evesynapse`):

   ```sh
   DISCORD_CLIENT_ID=<the Application ID>
   DISCORD_CLIENT_SECRET=...
   DISCORD_BOT_TOKEN=...
   ```

Treat the secret and the token like passwords. With only the first two
set, "Connect Discord" works and there is no bot.

### Add the bot to a server (each corporation or alliance)

Who may: an account with a character that holds the **Director** role
in the corporation (a CEO does), for that corporation's servers; and a
Director of the alliance's **executor corporation**, for the
alliance's. That character's link to EVE has to be working.

1. On the site: **bell → Settings → Discord → Set up your
   corporation's or alliance's Discord server**, then **Add the bot to
   a server**.
2. Discord asks which server and shows what the bot may do there: give
   roles, send messages, and add members. Discord only lets someone who
   can manage that server add a bot to it.
3. In that Discord server's **Roles** list, drag the bot's own role
   **above** the roles it is to give. Discord lets a bot manage only
   the roles below its own.
4. Back on the site, write that server's **role rules**: "give *these
   people* *this role*", as many as there are roles to give. A member
   gets every role they qualify for. "These people" can be:
   - everyone there who has connected EveSynapse,
   - members of the corporation or alliance,
   - (alliance servers) members of one of its corporations,
   - the CEO,
   - holders of an in-game corporation role (Director, Accountant,
     Diplomat, ...),
   - a **group** the directors keep themselves (below).

   Then, if wanted, pick the channel new ops are posted in. Roles and
   channels are picked from that server's own.

**Adding members automatically.** Someone with a character in the
corporation (or alliance) who has connected Discord is put in its
server by the bot, with their roles, without needing an invite: on the
worker's next pass after they qualify, whether that came from
connecting Discord, linking the character, the character joining the
corporation, or the server being set up. Only people who belong are
added; a rule that gives a role to "everyone connected" adds nobody.
Each person agrees to it on Discord when they connect ("join servers
for you"). Untick the box on the server's settings to switch it off. A
bot that was added before this existed lacks the permission: add it to
the server again from the site (its settings are kept).

**Groups** (`/groups/` on the site) are lists of characters that a
corporation's or alliance's directors keep by hand: a special interest
group, the fleet commanders, a logistics wing. A director makes a
group, ticks the characters in it, and adds a rule giving the group a
role; the bot does the rest. Only characters linked to EveSynapse that
are in the corporation (or the alliance) can be put in one. A member
who leaves the corporation stays on the list but loses the role until
they are back. Deleting a group removes the rules that used it.

**Which channels a role can open** is set in Discord, the usual way: on
a channel or category, under Permissions, allow the role and deny
@everyone. The bot gives the roles; it has no permission to change
channels, and does not need it.

**Ops in an alliance's server.** A corporation's ops are posted in its
own servers. They are posted in its alliance's server only after a
Director of that corporation ticks the box for it on the same page.

**Removing a server** first takes back the roles the bot gave there;
press Remove again a few minutes later to finish, and the bot leaves.

### What users do

On **bell → Settings**, under **Discord**: **Connect Discord** and
approve on Discord's page, which asks for two things: to see their
Discord name, and to add them to servers. Within a few minutes they
are in their corporation's server and have their roles in every server
that uses EveSynapse; **Check my roles now** does it
at once, for someone who has just joined a server. To get notifications
there, tick "Send my notifications to me on Discord" and press **Send a
test message**; direct messages only arrive for someone who shares a
server with the bot and allows messages from its members. **Disconnect
Discord** removes the link and takes back the roles EveSynapse gave.

### What the roles mean, and what they do not

A role is given to a Discord account when the EveSynapse account
connected to it has a character, with a link to EVE that still works,
for which the last sync saw what the rule asks: in the corporation,
its CEO, holding the in-game role, in the group. It is taken away when
that stops being true, and each role on its own: losing Director takes
the directors' role and leaves the members' one.

- **Roles are taken back when the account goes.** Deleting the
  EveSynapse account, disconnecting Discord, connecting a different
  Discord account, a character leaving, or its EVE access being
  revoked or expiring all remove them. What the bot gave is recorded
  against the Discord account itself, so this does not depend on the
  EveSynapse account still existing, and it is retried until Discord
  confirms it.
- It follows a change within a sync or two (minutes), not at the
  instant it happens in-game.
- In a server the bot gives and takes only the roles named in that
  server's rules. Every other role is left as it is.
- A role removed by hand from someone who still qualifies comes back
  within six hours.
- It shows that someone controls a character in the corporation. It
  does not stop a member sharing their Discord account, and it is not
  a check of in-game roles.
- If the bot is kicked from a server by hand, the roles it gave there
  stay until someone removes them: it can no longer act there.
- To add someone to a server later (when their character joins a
  corporation, say) EveSynapse keeps the Discord token they granted,
  sealed with `TOKEN_ENCRYPTION_KEY` like EVE tokens. That token can
  read their Discord name and add them to servers the bot is in, and
  nothing else. Someone who connected before this, or who removes
  EveSynapse under Discord's Authorized Apps, is asked on the settings
  page to connect again.


## HTTPS

EveSynapse itself speaks plain HTTP. On anything but your own
machine, put a reverse proxy that terminates TLS in front of it,
so sign-in cookies and character data never cross the network
unencrypted. With [Caddy](https://caddyserver.com), which obtains
and renews the certificate itself, the whole configuration is:

```
eve.example.org {
    reverse_proxy 127.0.0.1:8080
}
```

Then, in `/opt/evesynapse/.env`:

```sh
ADDR=127.0.0.1:8080                                    # only the proxy can reach the app
EVE_CALLBACK_URL=https://eve.example.org/auth/callback # and the same URL at developers.eveonline.com
```

The app reads its public address from `EVE_CALLBACK_URL`. When
that is an `https://` address it marks the session cookie
`Secure` and sends `Strict-Transport-Security`; when it is plain
`http://` on anything other than localhost it logs a warning at
startup.

Every response also carries `X-Content-Type-Options`,
`X-Frame-Options`, `Referrer-Policy` and a
`Content-Security-Policy`, and state-changing requests coming
from another site are refused.

## Build from source

Needs Go (the version in `go.mod`). The fonts and wallpaper
travel through the repo in encoded form (under `ci-assets/`), so
decode them into place first, then build:

```sh
make assets        # decode the fonts and wallpaper
make build         # bin/evesynapse for this machine
make build-arm64   # bin/evesynapse-arm64
make build-amd64   # bin/evesynapse-amd64
make test          # the test suite (starts its own embedded PostgreSQL)
make check         # what CI checks: gofmt, go vet, tests under the race detector
```

Install your build with:

```sh
sudo EVESYNAPSE_BINARY=$PWD/bin/evesynapse bash deploy/setup.sh
```

## Updating

Builds are published automatically: every push to `main` runs the
test suite in CI and, when `internal/app/version.txt` names a
version that has no release yet, publishes a GitHub release with
builds for ARM64 and AMD64 plus, for each build, a small manifest
(the version and the build's SHA-256 checksum) and that manifest's
signature.

A release is one edit: `internal/app/version.txt`. Nothing else in
the repository repeats the version, and the tests read it from that
file, so there are no version numbers in tests to keep in step.

Versions are four plain numbers with no leading zeros,
`stable.major.feature.fix`: `0.4.1.4`, then `0.4.1.5` for a fix or a
small addition, `0.4.2.0` for a feature, `0.5.0.0` for a major
version. The first number stays 0 until there is a feature-complete
stable release. A development build adds a suffix (`0.4.2.0-dev`).
Releases up to `0.4.01.003` padded the last two parts with zeros. The
updater reads every part as a number and compares part by part, so
the padding makes no difference to it: `0.4.01.003` is 0.4.1.3, and
installs on it update to `0.4.1.4`. A test fails if `version.txt` is
ever set to something those installs would take for an older version.

To update a running install, run the updater. With no flag it
automatically picks the build that matches the machine it's
running on:

```sh
sudo /opt/evesynapse/evesynapse -update          # right build for this machine
sudo /opt/evesynapse/evesynapse -update -arm64   # ARM build explicitly
sudo /opt/evesynapse/evesynapse -update -amd64   # Intel/AMD build (-x86 and -x64 also work)
```

The updater first checks the latest release's signature, and refuses
a release that is not signed with the project's release key ("Release
signing" below). Then it asks the release what version it carries
and compares it with its own. If they're the same, it just says so
and stops. If the release is newer, it downloads that build,
verifies it against the checksum in the signed manifest and checks
it's built for the right kind of computer, swaps it into place,
restarts the running server onto it, and reports the new version
number.

There's also a manual form that installs a specific build: from an
address, with the checksum published for it, or from a local file:

```sh
sudo /opt/evesynapse/evesynapse -update <url> <sha256>
sudo /opt/evesynapse/evesynapse -update <file> [sha256]
```

The manual form checks what you give it and no signature: it is
your own word for that build.

### Development builds

Builds from the `evesynapse-dev` branch carry a version ending in
`-dev` and are published as prereleases, which a plain `-update`
never installs. To follow them on a test box:

```sh
sudo /opt/evesynapse/evesynapse -update -dev
```

A plain `-update` from a development build goes back to the
latest release once that release's number catches up.

### Release signing

Every release is signed, and the updater installs nothing that is
not.

- **What is signed.** Each build's manifest, `latest-<arch>.json`:
  the version, the kind of computer, and the build's SHA-256. Its
  signature is published beside it as `latest-<arch>.json.sig`.
  Because the checksum is inside what is signed, the signature
  covers every byte of the build.
- **With what.** An Ed25519 key. Its private half exists only as the
  repository's `RELEASE_SIGNING_KEY` Actions secret, which the
  release job signs with. Its public half is
  `internal/releasesig/trusted_keys.pem`, compiled into every build.
- **What the updater does with it.** It fetches the manifest and the
  signature, checks the signature against the keys it was built
  with, and only then reads the manifest. A release that is
  unsigned, signed with another key, or changed after signing is
  refused and nothing is downloaded. An older release offered again
  is still genuinely signed, and is turned down by the version
  comparison instead.
- **What it proves, and what it does not.** That the release was
  published by this repository's release job. Someone who can
  replace the files of a release, but cannot run that job with the
  secret, can no longer get a build installed. It is no protection
  against someone who controls the repository itself, since they
  control what the job signs.

To check a release by hand, with nothing but OpenSSL 3:

```sh
base64 -d latest-arm64.json.sig > sig.bin
openssl pkeyutl -verify -pubin -inkey internal/releasesig/trusted_keys.pem \
    -rawin -in latest-arm64.json -sigfile sig.bin
sha256sum evesynapse-arm64   # must be the "sha256" in latest-arm64.json
```

(`go run ./cmd/releasesign verify latest-arm64.json` makes the first
check exactly as the updater does. OpenSSL reads only the first key
in `trusted_keys.pem`.)

**Making the key.** Once, from the repository root, signed in to
`gh`:

```sh
go run ./cmd/releasesign keygen -- gh secret set RELEASE_SIGNING_KEY
```

It makes the key pair, hands the private half to `gh secret set` on
its standard input without printing or saving it, and only when that
has worked adds the public half to `trusted_keys.pem`. Commit that
file. Add `-out <file>` to keep a copy of the private half as well.
That is a trade: a copy is one more place the key can leak from, and
without one, losing the secret means the manual update described
under "If the key is lost".

The release job fails, and publishes nothing, when the secret is
missing or is not the other half of a key in `trusted_keys.pem`.

**Replacing the key.** An install only trusts the keys of the build
it is running, so the new key has to reach installs in a release
signed with the old one:

1. `go run ./cmd/releasesign keygen -add -out ~/new-release-key.pem`
   adds the new public key beside the old, and writes the new private
   half outside the repository. Commit `trusted_keys.pem` and publish
   a release. It is still signed with the old key, and its updater
   trusts both.
2. Once installs have had time to take that release, switch the
   secret, `gh secret set RELEASE_SIGNING_KEY < ~/new-release-key.pem`,
   then delete that file or move it somewhere offline.
3. Later, delete the old key's block from `trusted_keys.pem`.

An install that skipped the release from step 1 needs the manual
update below. If you kept a copy of the old private half you can
avoid even that: put both keys in the secret for a while (one PEM
block after the other) and every release carries both signatures,
which an install knowing either key accepts. If the private half may
have leaked, replace it the same way without waiting.

**If the key is lost** (the secret deleted, and no copy kept), make
a new one (delete the old block from `trusted_keys.pem`, then
`keygen` as above) and publish a release. Existing installs cannot
verify it, because the only key they trust is the lost one. Each
needs one manual update, with the address and checksum from the
release page, and updates normally from then on:

```sh
sudo evesynapse -update https://github.com/natemsz/evesynapse/releases/download/v<version>/evesynapse-arm64 <sha256>
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

A fork signs its releases with its own key. In the fork, delete the
mainline key's block from `internal/releasesig/trusted_keys.pem`,
run the `keygen` command under "Release signing", and commit the
file; until the fork has a key its release job fails rather than
publish unsigned builds. It follows that the setting above only
moves an install that is already running a build of the fork: a
mainline build refuses the fork's releases, since they are not
signed with the key it trusts. Install the fork's build first (its
setup script does that), and it updates from the fork from then on.

## Command-line modes

```sh
evesynapse                          run the web app
evesynapse -version                 print the version and exit
evesynapse -update                  update to the latest release (right build for this machine)
evesynapse -update -arm64           update, fetching the ARM build
evesynapse -update -amd64           update, fetching the Intel/AMD build (-x86, -x64 also work)
evesynapse -update -dev             update to the newest development build
evesynapse -update <url> <sha256>   install a specific build from an address (checksum required)
evesynapse -update <file> [sha256]  install a build from a local file
evesynapse -refresh                 mark all cached data stale (run while the app is stopped)
evesynapse -push-keys               print a new key pair for browser push, as lines for .env
evesynapse -h                       show this list
```

## EVE SSO flow

1. **Register the app** at <https://developers.eveonline.com> with the
   callback URL from `EVE_CALLBACK_URL`, and put the issued client ID
   and secret in `.env`.
2. **Login**: `GET /auth/eve` stores a random `state` in the session
   and redirects to `login.eveonline.com` requesting 60 scopes (the
   list is `eveScopes` in `internal/app/auth.go`): the read scopes
   from the OAuth2 catalog in CCP's ESI OpenAPI document, plus four
   that are not read-only, each for one feature:

   | Scope | What the app does with it |
   |---|---|
   | `esi-fittings.write_fittings.v1` | "Save to EVE" in the fitting editor |
   | `esi-mail.send_mail.v1` | sending mail from the compose page |
   | `esi-mail.organize_mail.v1` | marking a mail as read |
   | `esi-planets.manage_planets.v1` | reading colonies; CCP publishes no read scope for planetary industry, and the app only ever reads |

   Everything else that can change something in the game (contacts,
   fleets, calendar responses, waypoints, opening in-game windows) is
   not requested. Characters linked before a scope was added keep the
   scopes they granted until they sign in again; the feature that
   needs the missing scope says so.
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
JSON in `character_snapshots`, keyed by
(character, kind) with the response's `Expires` header stored as
`cached_until` (5-minute fallback when ESI sends none). The module
sweep caches the character live-state endpoints the same way
(location, ship, online, clones, implants, fittings, fatigue and the
recent-killmails list). Killmail *details* are different: they are
immutable, so the worker warms them once per killmail into the
`killmail_details` store (at most 10
per character per cycle) and pages read the store only. Corporation
datasets (module sweep cluster 2) ride the
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

Refreshes are conditional where they can be. A dataset that comes
in one response is stored with the `ETag` ESI sent, and the next
refresh offers it back (`If-None-Match`). When nothing has changed
ESI answers `304 Not Modified` with no body: the stored payload
stays as it is and only its cache window is renewed. Datasets
spread over several pages (assets, contracts, blueprints, …) have
an ETag per page and none for the whole, so they are downloaded in
full each time. `evesynapse -refresh` clears the stored ETags, so
everything really is downloaded again.

Name resolution is split in two tiers. Page renders resolve type,
group and place names from local data only — the in-process maps,
the SDE static-data tables (below), and the `type_names` fallback
table — a render never waits on ESI; anything still missing shows
as `Type #<id>` (or an "Ungrouped" skill section) until the
worker's warm-up pass fills it in. The interactive Market lookup
is the one exception: it may make a single name fetch for an item
nobody has cached yet.

The Intel cluster (module sweep cluster 4) is public ESI data —
wars, incursions, faction
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
`sde_*` tables: types,
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


## Logging

Every log line carries a level:

- **ERROR**: something failed that should not have (a query, a
  decode, a store, a template, a recovered panic, a request the
  server answered with a 5xx).
- **WARN**: something went wrong in a way the app expects and
  handles (ESI asking it to back off, stale data served because a
  refresh failed, a sign-in that did not complete, a setting worth
  changing).
- **INFO**: what the app is doing (starting and stopping, sign-ins,
  worker cycle summaries, imports, requests).

`LOG_LEVEL` sets the least severe kind that is written. The
default, `info`, writes everything, as before; `warn` leaves only
the lines that may need attention. `LOG_FORMAT=json` writes one
JSON object per line (`time`, `level`, `msg`) for a log collector;
the default is the plain line:

```
2026/10/07 12:00:00 WARN worker: ESI error limit hit refreshing intel; backing off until next cycle
```

Under systemd, `journalctl -u evesynapse -g ' (WARN|ERROR) '` shows
only those lines without changing what is logged.
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
wallpaper, teal-blue accents have been changed to molten-ember reminiscent of the Dominion expansion from 2009, one which holds special meaning to us as it is the year our characters were born.
(`#326b8c`), translucent dark nav and header panels over the art — rebuilt with a
single dependency-free stylesheet (`internal/app/static/style.css`): no Bootstrap,
no jQuery, it's not 2013 anymore :), no external fonts, responsive down to phone widths.

## Dev login

The dev build (`cmd/evesynapse-dev`, `make build-dev`) registers
`/dev-login` when started with `DEV_LOGIN=1`: it flips the session to
signed-in without EVE SSO so the signed-in pages can be exercised
locally (it is never an admin: the dev session has no account). The
handler lives in `internal/devtools` and is only wired into the dev
entrypoint — the release binary (`cmd/evesynapse`) doesn't contain
the code, and `DEV_LOGIN=1` has no effect on it. `/dev-login` hands
a session to anyone who asks: **never enable it on a deployment
anyone else can reach, and never deploy the dev build.** The dev
server logs a loud warning at boot when it is on.

## Database & sqlc

The app runs on PostgreSQL 16 (the live deployment is 16.15;
tests self-provision an embedded Postgres 16, so CI needs no
database service). Every query and the hand-rolled importer ride
a `database/sql` handle over the pgx/v5 stdlib driver; a pgxpool
exists only to back the scs `pgxstore` session store (sessions
live in the `sessions` table the baseline creates).

The schema is a series of numbered steps in
`internal/store/schema_pg/`: `001_baseline.sql` is the whole schema
as of the move to Postgres (BIGINT/DOUBLE PRECISION keep the
generated Go models' int64/float64 types), and each later file is
one change. At startup `store.Open` applies whichever steps a database
is missing, in order. Each step runs in a single transaction
together with its row in the `schema_migrations` table, so a step
lands completely or not at all, and that table is the record of
what has been applied. A database from before the table existed is
adopted on first start: its existing steps are recorded, not run
again.

Times are `timestamptz` columns and `time.Time` (or `sql.NullTime`
where "never" is a possible answer) in the code; a connection
always hands them back in UTC, whatever zone the machine is in.
The baseline kept them as RFC 3339 text, and steps 009–011 convert
those columns, carrying every stored value over as the same
instant. Those three steps rewrite the tables they touch, so the
first start after upgrading past them takes as long as copying
those tables once; the snapshot table is by far the largest.

To change the schema, add the next numbered file, then embed it and
add one line to `schemaSteps`, both in `internal/store/schema.go`.
Regenerate query code after editing `internal/db/query/` with:

```sh
make gen   # sqlc generate
```

## Token encryption

Each linked character's EVE access and refresh tokens are stored
in the `characters` table. A refresh token is a standing
credential, so anyone holding a copy of the database could read
that character's data and use the write scopes the app requests
(sending mail, saving fittings).

Set `TOKEN_ENCRYPTION_KEY` to any random value of at least 32
characters and both tokens are stored encrypted (AES-256-GCM),
each bound to its character and column:

```sh
openssl rand -hex 32    # put the output in .env as TOKEN_ENCRYPTION_KEY=...
```

The setup script generates a key for new installs. add the line and restart: tokens already
stored are encrypted in place at that start.

Two things to know before switching it on:

- **Keep the key.** It lives in `.env`, not in the database, so
  back the two up together. If the key is changed or removed the
  app refuses to start rather than run with tokens it cannot
  read; the error explains how to clear the stored tokens if the
  key is truly lost, after which every character signs in once
  more.
- **Don't go back to a build from before token encryption.** It
  would present the encrypted values to CCP as tokens, be
  refused, and mark every character as needing a fresh sign-in.
- **Upgrading the format is one way.** Tokens sealed by earlier
  builds (`enc:v1:`, a single SHA-256 of the key) still open, and
  the first start of this build re-seals them as `enc:v2:` (a
  proper key derivation, HKDF). A build from before that change
  cannot read `enc:v2:` values, so take a database backup
  before upgrading if you might roll back.

Without a key, tokens are stored unencrypted as before and the
app logs a warning at startup.

## Backups

Back the database up with `pg_dump` (and restore with `psql`):

```sh
pg_dump evesynapse > evesynapse-$(date +%F).sql
psql evesynapse < evesynapse-2026-10-05.sql   # into a fresh database
```

The generated SQL file plus `/opt/evesynapse/.env` is a complete
backup of an install.
