#!/usr/bin/env bash
# EveSynapse installer / first-time setup.
#
# Downloads the latest release build for this machine (verified
# against the checksum published with it), installs it into
# /opt/evesynapse, installs the systemd service, and links
# `evesynapse` into /usr/local/bin so the commands stay short.
# Re-running it is safe: it refreshes the binary and the service
# unit, and never overwrites an existing .env.
#
# Usage:
#   sudo bash deploy/setup.sh
#   sudo EVESYNAPSE_UPDATE_REPO=you/evesynapse bash deploy/setup.sh   # install from your fork
#   sudo EVESYNAPSE_BINARY=./bin/evesynapse bash deploy/setup.sh      # install a build you made
set -euo pipefail

REPO="${EVESYNAPSE_UPDATE_REPO:-natemsz/evesynapse}"
INSTALL_DIR=/opt/evesynapse
SERVICE_USER=evesynapse
LINK=/usr/local/bin/evesynapse

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
mkdir -p "$INSTALL_DIR"
# Install under a temp name and rename into place, so a running
# server is never overwritten mid-write (it picks the new build
# up on its next restart).
install -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$TMP/evesynapse" "$INSTALL_DIR/.evesynapse.new"
mv -f "$INSTALL_DIR/.evesynapse.new" "$INSTALL_DIR/evesynapse"
ln -sf "$INSTALL_DIR/evesynapse" "$LINK"
command -v restorecon >/dev/null && restorecon "$INSTALL_DIR/evesynapse" || true
echo "Installed the program to $INSTALL_DIR/evesynapse (run it as plain \`evesynapse\`)."

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
chown "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR" "$INSTALL_DIR/.env"
chmod 600 "$INSTALL_DIR/.env"

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

Updating from then on is one command:  sudo evesynapse -update
It picks the right build for this machine and only downloads when
a newer version exists.
EOF
