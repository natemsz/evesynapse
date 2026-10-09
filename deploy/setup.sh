#!/usr/bin/env bash
# EveSynapse installer / first-time setup.
#
# Downloads the latest release build for this machine (a signed
# release, and the build verified against the checksum its signed
# manifest names), installs it into
# /opt/evesynapse, installs the systemd service, provisions the
# local PostgreSQL the app runs on (role + database + a
# generated password written into .env), sizes that PostgreSQL's
# memory settings to this machine's RAM (step 3b), and links
# `evesynapse` into /usr/bin so the commands stay short. Re-running
# it is safe: it refreshes the binary and the service unit, provisions
# database pieces that are still missing, leaves alone any PostgreSQL
# setting somebody has chosen, and never overwrites an existing .env
# (it only appends a DATABASE_URL that isn't there yet).
#
# Usage:
#   sudo bash deploy/setup.sh
#   sudo EVESYNAPSE_UPDATE_REPO=you/evesynapse bash deploy/setup.sh   # install from your fork
#   sudo EVESYNAPSE_BINARY=./bin/evesynapse bash deploy/setup.sh      # install a build you made
#   sudo EVESYNAPSE_TUNE_POSTGRES=0 bash deploy/setup.sh             # leave PostgreSQL's memory settings alone
#
# A release is checked against the release key in the checkout this
# script is run from (internal/releasesig/trusted_keys.pem), so to
# install from a fork, run the fork's copy of this script.
set -euo pipefail

REPO="${EVESYNAPSE_UPDATE_REPO:-natemsz/evesynapse}"
INSTALL_DIR=/opt/evesynapse
SERVICE_USER=evesynapse
# The link lives in /usr/bin, not /usr/local/bin: updates always
# run under sudo, and sudo's locked-down PATH does not search
# /usr/local/bin on RHEL-family systems.
LINK=/usr/bin/evesynapse

die() { echo "setup: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this with sudo: sudo bash $0"
command -v curl >/dev/null || die "curl is required (it downloads the build and checks it)"

case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) die "EveSynapse publishes builds for ARM64 and x86-64 machines; this machine is $(uname -m)." ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- Release signatures ---
# A release's manifest (its version and the checksum of the build) is
# signed with the project's release key, and `evesynapse -update`
# installs nothing that is not. A first install is held to the same
# rule here wherever this machine can check: that takes an OpenSSL
# that verifies Ed25519 signatures (3.0 or newer). On a machine
# without one the script says so and goes by the checksum alone.
# Whether the check is made depends only on this machine, never on
# what the release serves, so a tampered release cannot switch it off.

# can_check_signatures: does this machine's openssl verify a known
# good Ed25519 signature? (The one in RFC 8032, section 7.1, test 2:
# that key's signature over the single byte "r".)
can_check_signatures() {
  command -v openssl >/dev/null && command -v base64 >/dev/null || return 1
  local d="$TMP/sigprobe"
  mkdir -p "$d"
  printf '%s\n' '-----BEGIN PUBLIC KEY-----' \
    'MCowBQYDK2VwAyEAPUAXw+hDiVqStwqnTRt+vJyYLM8uxJaMwM1V8Sr0Zgw=' \
    '-----END PUBLIC KEY-----' > "$d/key.pem"
  printf 'r' > "$d/msg"
  printf '%s' 'kqAJqfDUyrhyDoILX2QlQKKye1QWUD+Ps3YiI+vbadoIWsHkPhWZbkWPNhPQ8R2MOHsurrQwKu6wDSkWErsMAA==' \
    | base64 -d > "$d/sig" 2>/dev/null || return 1
  openssl pkeyutl -verify -pubin -inkey "$d/key.pem" -rawin -in "$d/msg" -sigfile "$d/sig" >/dev/null 2>&1
}

# verify_release_signature MANIFEST SIGFILE KEYSFILE: succeeds when a
# line of SIGFILE is a valid signature over MANIFEST's exact bytes by
# one of the public keys in KEYSFILE. Either file may hold more than
# one (while a key is being replaced); one good pair is enough.
verify_release_signature() {
  local manifest="$1" sigfile="$2" keys="$3" d line key n=0
  d="$(mktemp -d "$TMP/sigcheck.XXXXXX")" || return 1
  # One file per public key: the lines of each PEM block, and nothing
  # of the commentary around them.
  awk -v d="$d" '
    { sub(/\r$/, "") }
    /^-----BEGIN PUBLIC KEY-----$/ { n++; inside = 1 }
    inside { print > (d "/key-" n ".pem") }
    /^-----END PUBLIC KEY-----$/ { inside = 0 }
  ' "$keys"
  ls "$d"/key-*.pem >/dev/null 2>&1 || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    line="$(printf '%s' "$line" | tr -d '[:space:]')"
    [ -n "$line" ] || continue
    n=$((n + 1))
    printf '%s' "$line" | base64 -d > "$d/sig-$n" 2>/dev/null || continue
    for key in "$d"/key-*.pem; do
      # (</dev/null: nothing in this loop may read the signature file
      # the loop itself is reading.)
      if openssl pkeyutl -verify -pubin -inkey "$key" -rawin -in "$manifest" -sigfile "$d/sig-$n" </dev/null >/dev/null 2>&1; then
        return 0
      fi
    done
  done < "$sigfile"
  return 1
}

# --- 1. Get the binary: a signed release, verified against its checksum ---
if [ -n "${EVESYNAPSE_BINARY:-}" ]; then
  [ -f "$EVESYNAPSE_BINARY" ] || die "EVESYNAPSE_BINARY=$EVESYNAPSE_BINARY does not exist"
  cp "$EVESYNAPSE_BINARY" "$TMP/evesynapse"
  echo "Installing your local build from $EVESYNAPSE_BINARY (you built it, so no checksum check)."
else
  BASE="https://github.com/$REPO/releases/latest/download"
  echo "Downloading the latest $ARCH build from $REPO..."
  curl -fsSL "$BASE/latest-$ARCH.json" -o "$TMP/manifest.json" || die "could not reach the latest release of $REPO"
  # The signature is checked before anything in the manifest is read
  # and before the build is downloaded.
  if can_check_signatures; then
    KEYS="$REPO_ROOT/internal/releasesig/trusted_keys.pem"
    if [ ! -f "$KEYS" ]; then
      # Run on its own, outside a checkout: take the key from the repo.
      KEYS="$TMP/trusted_keys.pem"
      curl -fsSL "https://raw.githubusercontent.com/$REPO/main/internal/releasesig/trusted_keys.pem" -o "$KEYS" \
        || die "could not fetch the release key of $REPO, so its release cannot be checked; not installing"
    fi
    grep -q '^-----BEGIN PUBLIC KEY-----' "$KEYS" \
      || die "$KEYS holds no release key, so the release cannot be checked; not installing"
    curl -fsSL "$BASE/latest-$ARCH.json.sig" -o "$TMP/manifest.json.sig" \
      || die "the latest release of $REPO is not signed (or its signature could not be fetched); not installing"
    verify_release_signature "$TMP/manifest.json" "$TMP/manifest.json.sig" "$KEYS" \
      || die "the latest release of $REPO is not signed with the release key in $KEYS; not installing. (To install from a fork, run the fork's own copy of this script.)"
    echo "The release's signature checks out."
  else
    echo "NOTE: this machine's OpenSSL cannot check Ed25519 signatures (that takes OpenSSL 3.0 or newer),"
    echo "      so the release's signature is NOT checked here, only the build's checksum."
  fi
  VERSION="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$TMP/manifest.json")"
  SHA="$(sed -n 's/.*"sha256":"\([0-9a-f]*\)".*/\1/p' "$TMP/manifest.json")"
  [ -n "$SHA" ] || die "the latest release of $REPO has no readable checksum; not installing"
  curl -fsSL "$BASE/evesynapse-$ARCH" -o "$TMP/evesynapse"
  echo "$SHA  $TMP/evesynapse" | sha256sum -c - >/dev/null || die "the download did not match its published checksum; not installing"
  echo "Verified EveSynapse v$VERSION ($ARCH) against its published checksum."
fi

# --- 2. Service user, install dir, binary, short link ---
id "$SERVICE_USER" >/dev/null 2>&1 || useradd --system --home-dir "$INSTALL_DIR" --shell /bin/false "$SERVICE_USER"
SERVICE_GROUP="$(id -gn "$SERVICE_USER")"
mkdir -p "$INSTALL_DIR"
# The program and the directory it lives in belong to root, never
# to the service account. Root runs this program for
# `evesynapse -update`, so an account that could replace it — or
# anything else in this directory — could have its own code run as
# root. The service only reads from here; its one writable place
# is the runtime directory the unit gives it (/run/evesynapse).
# Re-running this script moves an older install to this layout.
chown root:root "$INSTALL_DIR"
chmod 0755 "$INSTALL_DIR"
# Install under a temp name and rename into place, so a running
# server is never overwritten mid-write (it picks the new build
# up on its next restart).
install -m 0755 -o root -g root "$TMP/evesynapse" "$INSTALL_DIR/.evesynapse.new"
mv -f "$INSTALL_DIR/.evesynapse.new" "$INSTALL_DIR/evesynapse"
ln -sf "$INSTALL_DIR/evesynapse" "$LINK"
# An older setup put the link in /usr/local/bin, where sudo can't
# see it; retire that one so there's a single canonical link.
if [ -L /usr/local/bin/evesynapse ] && [ "$(readlink /usr/local/bin/evesynapse)" = "$INSTALL_DIR/evesynapse" ]; then
  rm -f /usr/local/bin/evesynapse
fi
command -v restorecon >/dev/null && restorecon "$INSTALL_DIR/evesynapse" || true
echo "Installed the program to $INSTALL_DIR/evesynapse (run it as plain \`evesynapse\`)."

# --- 2b. PostgreSQL (the app's database) ---
# Install + initialize + enable PostgreSQL when it's missing,
# then make sure the evesynapse role and database exist. The
# role's password is generated once and written to .env below
# (readable only by root and the service); an install that
# already has a DATABASE_URL in its .env is left completely
# alone — this step never rotates a password or drops a database.
PG_PASSWORD=""
if ! grep -qs '^DATABASE_URL=' "$INSTALL_DIR/.env" 2>/dev/null; then
  if command -v dnf >/dev/null && ! command -v psql >/dev/null; then
    echo "Installing PostgreSQL..."
    dnf install -y postgresql-server >/dev/null || die "could not install postgresql-server"
  fi
  if command -v psql >/dev/null; then
    if command -v postgresql-setup >/dev/null && [ ! -f /var/lib/pgsql/data/PG_VERSION ]; then
      postgresql-setup --initdb >/dev/null || die "postgresql-setup --initdb failed"
    fi
    if command -v systemctl >/dev/null; then
      systemctl enable --now postgresql >/dev/null 2>&1 || die "could not enable+start postgresql"
    fi
    PG_PASSWORD="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    # Create the role when missing; when it already exists (a
    # re-run of setup whose .env lost its DATABASE_URL), reset
    # the password to the fresh one so the .env about to be
    # written always matches the role.
    su postgres -c "psql -v ON_ERROR_STOP=1 -q" >/dev/null <<SQL || die "could not create the evesynapse database role"
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'evesynapse') THEN
    CREATE ROLE evesynapse LOGIN PASSWORD '$PG_PASSWORD';
  ELSE
    ALTER ROLE evesynapse PASSWORD '$PG_PASSWORD';
  END IF;
END
\$\$;
SQL
    su postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname = 'evesynapse'\"" | grep -q 1 \
      || su postgres -c "createdb -O evesynapse evesynapse" || die "could not create the evesynapse database"
    echo "PostgreSQL ready: database 'evesynapse' owned by role 'evesynapse'."
  else
    echo "NOTE: no PostgreSQL tools found on this machine. Install PostgreSQL 16,"
    echo "      create a role+database, and put its DATABASE_URL in $INSTALL_DIR/.env."
  fi
fi

# --- 3. Configuration (an existing .env is never overwritten) ---
if [ -f "$INSTALL_DIR/.env" ]; then
  echo "Keeping your existing $INSTALL_DIR/.env."
  # Encrypting the stored EVE tokens is the operator's switch to
  # flip on an existing install: once on, the key has to be kept,
  # and an older build can no longer read the database's tokens.
  if ! grep -q '^TOKEN_ENCRYPTION_KEY=' "$INSTALL_DIR/.env"; then
    echo "NOTE: this install stores its characters' EVE login tokens unencrypted."
    echo "      To encrypt them, add this line to $INSTALL_DIR/.env and restart:"
    echo "        TOKEN_ENCRYPTION_KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    echo "      Keep that value with your backups (see \"Token encryption\" in the README)."
  fi
else
  if [ -f "$REPO_ROOT/.env.example" ]; then
    cp "$REPO_ROOT/.env.example" "$INSTALL_DIR/.env"
  else
    curl -fsSL "https://raw.githubusercontent.com/$REPO/main/.env.example" -o "$INSTALL_DIR/.env" \
      || die "could not fetch .env.example; create $INSTALL_DIR/.env by hand (see the README)"
  fi
  if ! grep -q '^EVESYNAPSE_UPDATE_REPO=' "$INSTALL_DIR/.env"; then
    printf '\n# Which GitHub repo (owner/repo) the updater checks for new versions.\nEVESYNAPSE_UPDATE_REPO=%s\n' "$REPO" >> "$INSTALL_DIR/.env"
  fi
  # A fresh install encrypts the EVE tokens it stores from the
  # first sign-in on.
  if ! grep -q '^TOKEN_ENCRYPTION_KEY=' "$INSTALL_DIR/.env"; then
    printf '\n# Encrypts the EVE login tokens stored in the database (generated by setup).\n# Keep this value with your backups: without it the stored tokens cannot be\n# read and every character has to sign in again.\nTOKEN_ENCRYPTION_KEY=%s\n' "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" >> "$INSTALL_DIR/.env"
  fi
  echo "Wrote $INSTALL_DIR/.env — it needs your EVE app credentials before the first start (below)."
fi
# Fresh installs get their database connection written in; an
# install whose .env predates Postgres (a SQLite-era box) gets
# DATABASE_URL appended, pointing at the role/database above.
if [ -n "$PG_PASSWORD" ] && ! grep -q '^DATABASE_URL=' "$INSTALL_DIR/.env"; then
  printf '\n# Postgres connection (generated by setup; the password is only stored here).\nDATABASE_URL=postgres://evesynapse:%s@localhost:5432/evesynapse?sslmode=disable\n' "$PG_PASSWORD" >> "$INSTALL_DIR/.env"
  echo "Wrote DATABASE_URL to $INSTALL_DIR/.env."
fi
# The service reads .env (through its group) but cannot change it:
# the updater takes its download address from this file, so it is
# root's to edit, like the program beside it.
chown "root:$SERVICE_GROUP" "$INSTALL_DIR/.env"
chmod 640 "$INSTALL_DIR/.env"

# --- 3b. PostgreSQL memory settings ---
# PostgreSQL ships set up for a machine with almost no memory: 128 MB
# of its own cache however much RAM there is. On a machine with room,
# three settings are raised here so the whole EveSynapse database
# (a few hundred MB) is served from memory:
#
#   shared_buffers            an eighth of the RAM, at most 2 GB
#   effective_cache_size      two thirds of the RAM (a planning hint;
#                             it reserves nothing)
#   shared_preload_libraries  pg_prewarm, which reloads what was in the
#                             cache after a restart
#
# Each is changed only while it is still as PostgreSQL shipped it and
# nobody has set it with ALTER SYSTEM, so a value somebody chose is
# never replaced, and running this again changes nothing. Only a PostgreSQL on this machine is touched: one
# reached over the network is its operator's to tune. A machine with
# under 2 GB of RAM is left alone. EVESYNAPSE_TUNE_POSTGRES=0 skips the
# whole step.
#
# The first and third only take effect when PostgreSQL restarts. With
# EveSynapse not running (a first install) it is restarted here; with
# it running, the restart is left to you and the command is printed.

# pg_sql runs the SQL on its standard input as PostgreSQL's superuser
# and prints bare values.
pg_sql() { su postgres -c "psql -tAq -v ON_ERROR_STOP=1" 2>/dev/null; }

# pg_memory_plan MEM_MB prints "SHARED_BUFFERS_MB EFFECTIVE_CACHE_MB" for
# a machine with that much RAM, and fails for one with under 2 GB,
# which is left as PostgreSQL shipped it. An eighth of the RAM is well
# under the quarter PostgreSQL's own documentation suggests, because
# this machine runs EveSynapse too; 2 GB is several times the size of
# the database, so more would hold nothing.
pg_memory_plan() {
  local mem="$1" buffers
  [ "$mem" -ge 2000 ] || return 1
  buffers=$((mem / 8))
  [ "$buffers" -le 2048 ] || buffers=2048
  echo "$buffers $((mem * 2 / 3))"
}

# pg_chosen NAME: has somebody (or an earlier run of this script) set
# NAME with ALTER SYSTEM? That is so from the moment it is written,
# restarted or not.
pg_chosen() {
  echo "SELECT 1 FROM pg_file_settings WHERE name = '$1' AND sourcefile LIKE '%postgresql.auto.conf'" | pg_sql | grep -q 1
}

tune_postgres() {
  if [ "${EVESYNAPSE_TUNE_POSTGRES:-1}" = "0" ]; then
    echo "Leaving PostgreSQL's memory settings alone (EVESYNAPSE_TUNE_POSTGRES=0)."
    return 0
  fi
  command -v psql >/dev/null || return 0
  if grep -qs '^DATABASE_URL=' "$INSTALL_DIR/.env" \
    && ! grep -qsE '^DATABASE_URL=[^#]*@(localhost|127\.0\.0\.1|\[::1\])[:/]' "$INSTALL_DIR/.env"; then
    return 0 # the database is on another machine
  fi
  echo 'SELECT 1' | pg_sql >/dev/null || return 0 # no local server this script can administer

  local mem_mb plan buffers_mb cache_mb changed="" data_dir pending
  echo 'SELECT pg_reload_conf()' | pg_sql >/dev/null || true
  mem_mb="$(awk '/^MemTotal:/ { print int($2 / 1024) }' /proc/meminfo 2>/dev/null || true)"
  if [ -z "$mem_mb" ] || ! plan="$(pg_memory_plan "$mem_mb")"; then
    echo "PostgreSQL's memory settings are left at their defaults (this machine has under 2 GB of RAM)."
    return 0
  fi
  buffers_mb="${plan% *}"
  cache_mb="${plan#* }"

  # shared_buffers: 16384 pages of 8 kB is the 128 MB every new
  # PostgreSQL is created with.
  if ! pg_chosen shared_buffers \
    && [ "$(echo "SELECT setting FROM pg_settings WHERE name = 'shared_buffers'" | pg_sql)" = "16384" ]; then
    echo "ALTER SYSTEM SET shared_buffers = '${buffers_mb}MB'" | pg_sql \
      && changed="$changed shared_buffers=${buffers_mb}MB"
  fi
  if ! pg_chosen effective_cache_size \
    && [ "$(echo "SELECT source FROM pg_settings WHERE name = 'effective_cache_size'" | pg_sql)" = "default" ]; then
    echo "ALTER SYSTEM SET effective_cache_size = '${cache_mb}MB'" | pg_sql \
      && changed="$changed effective_cache_size=${cache_mb}MB"
  fi
  if ! pg_chosen shared_preload_libraries && [ -z "$(echo 'SHOW shared_preload_libraries' | pg_sql)" ]; then
    # pg_prewarm comes in PostgreSQL's contrib package. Naming a
    # library that is not installed would stop PostgreSQL starting, so
    # it is only named once it is known to be there.
    if ! echo "SELECT 1 FROM pg_available_extensions WHERE name = 'pg_prewarm'" | pg_sql | grep -q 1; then
      if command -v dnf >/dev/null && rpm -q postgresql-server >/dev/null 2>&1; then
        dnf install -y postgresql-contrib >/dev/null 2>&1 || true
      fi
    fi
    if echo "SELECT 1 FROM pg_available_extensions WHERE name = 'pg_prewarm'" | pg_sql | grep -q 1; then
      echo "ALTER SYSTEM SET shared_preload_libraries = 'pg_prewarm'" | pg_sql \
        && changed="$changed shared_preload_libraries=pg_prewarm"
    else
      echo "NOTE: pg_prewarm is not installed (it is in PostgreSQL's contrib package), so the"
      echo "      database's cache will not be reloaded after a restart. Everything else works."
    fi
  fi

  echo 'SELECT pg_reload_conf()' | pg_sql >/dev/null || true
  # Is PostgreSQL running with something other than what is now
  # written down for it? (From this run, or from an earlier one that
  # left the restart for later.)
  pending="$(pg_sql <<'SQL' || true
SELECT count(*) FROM pg_file_settings f
WHERE f.sourcefile LIKE '%postgresql.auto.conf'
  AND ((f.name = 'shared_preload_libraries' AND f.setting IS DISTINCT FROM current_setting('shared_preload_libraries'))
    OR (f.name = 'shared_buffers' AND pg_size_bytes(f.setting) <> (SELECT s.setting::bigint * 8192 FROM pg_settings s WHERE s.name = 'shared_buffers')))
SQL
)"
  if [ -z "$changed" ] && [ "${pending:-0}" = "0" ]; then
    echo "PostgreSQL's memory settings are already set; nothing changed."
    return 0
  fi
  [ -z "$changed" ] || echo "PostgreSQL memory settings for this machine's ${mem_mb} MB of RAM:$changed"
  [ "${pending:-0}" != "0" ] || return 0

  if ! command -v systemctl >/dev/null; then
    echo "NOTE: restart PostgreSQL for those settings to take effect."
    return 0
  fi
  if systemctl is-active --quiet evesynapse; then
    echo "NOTE: they take effect when PostgreSQL restarts, which drops the site's connections for a"
    echo "      few seconds. When it suits you:"
    echo "        sudo systemctl restart postgresql && sudo systemctl restart evesynapse"
    return 0
  fi
  # A PostgreSQL this script did not install may run under another
  # unit name (postgresql-16, from the PostgreSQL project's own
  # packages). The settings are written; the restart is its owner's.
  if ! systemctl cat postgresql >/dev/null 2>&1; then
    echo "NOTE: restart PostgreSQL for those settings to take effect (its service is not called"
    echo "      'postgresql' here, so this script leaves that to you)."
    return 0
  fi
  data_dir="$(echo 'SHOW data_directory' | pg_sql || true)"
  if systemctl restart postgresql >/dev/null 2>&1 && echo 'SELECT 1' | pg_sql >/dev/null; then
    echo "PostgreSQL restarted with the new settings."
    return 0
  fi
  # It did not come back. Take out what this run put in and start it
  # as it was: a working database matters more than a tuned one.
  if [ -n "$data_dir" ] && [ -f "$data_dir/postgresql.auto.conf" ]; then
    sed -i -E '/^(shared_buffers|effective_cache_size|shared_preload_libraries) *=/d' "$data_dir/postgresql.auto.conf"
  fi
  systemctl restart postgresql >/dev/null 2>&1 || die "PostgreSQL did not start with the new memory settings, and did not start again without them; see: journalctl -u postgresql"
  echo "NOTE: PostgreSQL did not start with the new memory settings, so they were taken out again"
  echo "      and it is running as before. Run this script with EVESYNAPSE_TUNE_POSTGRES=0 to skip them."
}
tune_postgres

# --- 4. systemd service ---
if command -v systemctl >/dev/null; then
  if [ -f "$SCRIPT_DIR/evesynapse.service" ]; then
    cp "$SCRIPT_DIR/evesynapse.service" /etc/systemd/system/evesynapse.service
  else
    curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/evesynapse.service" -o /etc/systemd/system/evesynapse.service
  fi
  systemctl daemon-reload
  systemctl enable evesynapse >/dev/null 2>&1 || true
  if systemctl is-active --quiet evesynapse; then
    echo "Service installed. It's running the previous build right now; apply this one with: sudo systemctl restart evesynapse"
  else
    # An older install kept the server's pidfile in the install
    # directory; it lives in /run/evesynapse now. With nothing
    # running, the old one is only a stale leftover.
    rm -f "$INSTALL_DIR/evesynapse.pid"
    echo "Service installed and set to start at boot."
  fi
else
  echo "No systemd on this machine — start it by hand with: $LINK"
fi

cat <<EOF

Almost there. EveSynapse still needs its EVE sign-in settings:

  1. Register an app at https://developers.eveonline.com and set its
     callback to the address you'll use, e.g. https://your-host/auth/callback
  2. Put that client ID and secret (and the same callback URL) in
     $INSTALL_DIR/.env
  3. Start it:  sudo systemctl start evesynapse
     then open your address and sign in with EVE.

Its data lives in PostgreSQL (the setup created the database and
wrote the connection into $INSTALL_DIR/.env as DATABASE_URL).
Back it up with:  pg_dump evesynapse > backup.sql

Updating from then on is one command:  sudo evesynapse -update
It picks the right build for this machine and only downloads when
a newer version exists.
EOF
