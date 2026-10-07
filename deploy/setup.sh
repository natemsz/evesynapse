#!/usr/bin/env bash
# EveSynapse installer / first-time setup.
#
# Downloads the latest release build for this machine (verified
# against the checksum published with it), installs it into
# /opt/evesynapse, installs the systemd service, provisions the
# local PostgreSQL the app runs on (role + database + a
# generated password written into .env), and links `evesynapse`
# into /usr/bin so the commands stay short. Re-running it is
# safe: it refreshes the binary and the service unit, provisions
# database pieces that are still missing, and never overwrites
# an existing .env (it only appends a DATABASE_URL that isn't
# there yet).
#
# Usage:
#   sudo bash deploy/setup.sh
#   sudo EVESYNAPSE_UPDATE_REPO=you/evesynapse bash deploy/setup.sh   # install from your fork
#   sudo EVESYNAPSE_BINARY=./bin/evesynapse bash deploy/setup.sh      # install a build you made
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

# --- 1. Get the binary, verified against its published checksum ---
if [ -n "${EVESYNAPSE_BINARY:-}" ]; then
  [ -f "$EVESYNAPSE_BINARY" ] || die "EVESYNAPSE_BINARY=$EVESYNAPSE_BINARY does not exist"
  cp "$EVESYNAPSE_BINARY" "$TMP/evesynapse"
  echo "Installing your local build from $EVESYNAPSE_BINARY (you built it, so no checksum check)."
else
  BASE="https://github.com/$REPO/releases/latest/download"
  echo "Downloading the latest $ARCH build from $REPO..."
  curl -fsSL "$BASE/latest-$ARCH.json" -o "$TMP/manifest.json" || die "could not reach the latest release of $REPO"
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

# --- 2b. PostgreSQL (the app's database since v0.3.26) ---
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
