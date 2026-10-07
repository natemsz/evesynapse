#!/usr/bin/env bash
# EveSynapse installer / first-time setup.
#
# Downloads the latest release build for this machine (a signed
# release, and the build verified against the checksum its signed
# manifest names), installs it into
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
