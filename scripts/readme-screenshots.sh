#!/usr/bin/env bash
# Regenerates the curated README screenshots in docs/screenshots/ (`make screenshots`).
#
# It builds the SPA and a `-tags demo` binary, seeds a live demo data dir, runs the
# demo with the pinned astap_cli beside it, waits until enough rendered frames have
# been filed and plate-solved to look like a real night, captures every surface with
# scripts/screenshots.mjs, and writes the curated set as WebP. Every process it
# starts is stopped on exit, and its scratch dir is removed (KEEP=1 keeps it).
#
# The frames' sky comes from tools/starfield (Gaia DR3, cached under the user cache
# dir); the astap_cli binary and the d05 star catalog the demo solves with are cached
# beside it, so only a first run downloads them. Needs Go, Node with a global
# Playwright install, sqlite3 and cwebp.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO=$(pwd)
PORT=${PORT:-8686}   # the seeddemo config's port unless overridden
PASSWORD=12345678    # the seeddemo login (test/FIXTURES.md)
MIN_FILED=${MIN_FILED:-8}
MIN_SOLVED=${MIN_SOLVED:-5}
SETTLE_TIMEOUT=${SETTLE_TIMEOUT:-900}
DEST=docs/screenshots

for tool in go node npm sqlite3 cwebp; do
  command -v "$tool" >/dev/null || { echo "screenshots: $tool is required" >&2; exit 1; }
done
if curl -s -o /dev/null "http://localhost:$PORT/healthz"; then
  echo "screenshots: something already listens on :$PORT; stop it first" >&2
  exit 1
fi

SKY_CACHE=$(go run ./tools/starfield/cmd/starfield cachedir)
ASTAP_CACHE="$(dirname "$SKY_CACHE")/astap"
# A short scratch path: Settings renders the data roots verbatim.
WORK=$(mktemp -d /tmp/acshots.XXXXXX)
DATA="$WORK/data"
BIN="$WORK/bin"
RAW="$WORK/raw"
PID=""

cleanup() {
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID"
    wait "$PID" 2>/dev/null || true
  fi
  if [ "${KEEP:-}" = 1 ]; then
    echo "screenshots: kept $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

if [ ! -x "$ASTAP_CACHE/astap_cli" ]; then
  echo "screenshots: fetching the pinned astap_cli into $ASTAP_CACHE"
  go run ./tools/fetch-astap fetch "$(go env GOOS)/$(go env GOARCH)" "$ASTAP_CACHE"
fi

# The images show the latest release's version, not a git-describe build string.
VERSION=$(git describe --tags --abbrev=0 --match 'v*')
echo "screenshots: stamping the build as $VERSION"
make spa VERSION="$VERSION"
mkdir -p "$BIN"
go build -tags demo -ldflags "-X main.version=$VERSION" -o "$BIN/asiair-commander" ./cmd/asiair-commander
cp "$ASTAP_CACHE/astap_cli" "$BIN/"

SEED_DIR="$DATA" SEED_LIVE=1 go run ./tools/seeddemo
# A clean release version arms the update check. The capture must not call
# api.github.com or show an update pill, so the check is turned off in the seeded
# config, and the demo's releases URL points at a closed local port in case it runs.
grep -q '^update_check_disabled: false$' "$DATA/config.yaml" || {
  echo "screenshots: the seeded config has no update_check_disabled key to turn on" >&2
  exit 1
}
sed -e "s/^\([[:space:]]*web_port:\).*/\1 $PORT/" \
  -e "s/^update_check_disabled: false$/update_check_disabled: true/" \
  "$DATA/config.yaml" >"$DATA/config.yaml.tmp"
mv "$DATA/config.yaml.tmp" "$DATA/config.yaml"
# The demo downloads the d05 catalog (about 100 MB) itself when none is installed;
# a cached copy is linked in instead, and a fresh download is cached after the run.
mkdir -p "$DATA/astap"
if [ -d "$ASTAP_CACHE/d05" ]; then
  ln -s "$ASTAP_CACHE/d05" "$DATA/astap/d05"
fi

echo "screenshots: starting the demo on :$PORT (log: $WORK/server.log)"
AC_DEMO_RELEASES_URL=http://127.0.0.1:9 \
  "$BIN/asiair-commander" run -data-dir "$DATA" >"$WORK/server.log" 2>&1 &
PID=$!

count() {
  sqlite3 "$DATA/state.db" "$1" 2>/dev/null || echo 0
}
deadline=$((SECONDS + SETTLE_TIMEOUT))
while :; do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "screenshots: the demo exited; tail of its log:" >&2
    tail -20 "$WORK/server.log" >&2
    exit 1
  fi
  filed=$(count "SELECT count(*) FROM frames WHERE frame_type='LIGHT' AND state IN ('verified','deleted')")
  solved=$(count "SELECT count(*) FROM frames WHERE wcs_status='solved'")
  echo "screenshots: $filed filed, $solved solved"
  if [ "$filed" -ge "$MIN_FILED" ] && [ "$solved" -ge "$MIN_SOLVED" ]; then
    break
  fi
  if [ $SECONDS -ge $deadline ]; then
    echo "screenshots: the demo did not settle within ${SETTLE_TIMEOUT}s; recent log:" >&2
    grep -iE "warn|error|starfield|astap|catalog" "$WORK/server.log" | tail -20 >&2
    exit 1
  fi
  sleep 10
done

if [ ! -d "$ASTAP_CACHE/d05" ] && [ -d "$DATA/astap/d05" ] && [ ! -L "$DATA/astap/d05" ]; then
  echo "screenshots: caching the downloaded d05 catalog in $ASTAP_CACHE"
  cp -R "$DATA/astap/d05" "$ASTAP_CACHE/d05.tmp" && mv "$ASTAP_CACHE/d05.tmp" "$ASTAP_CACHE/d05"
fi

OUT="$RAW" AC_PASSWORD="$PASSWORD" TARGET="http://localhost:$PORT" \
  NODE_PATH="$(npm root -g)" node scripts/screenshots.mjs

# The curated set: <capture> <published name> <width> [<crop x y w h>]. Desktop
# captures are retina (2880x1800), halved; phone captures are 3x, shrunk to a
# column; the frame detail is cropped to its dialog (title, preview, identity),
# above the archive path.
mkdir -p "$DEST"
rm -f "$DEST"/*.webp
while read -r src name width crop; do
  [ -n "$src" ] || continue
  if [ ! -f "$RAW/$src.png" ]; then
    echo "screenshots: capture $src.png is missing" >&2
    exit 1
  fi
  # shellcheck disable=SC2086 # crop is four separate numbers
  cwebp -quiet -q 82 -m 6 ${crop:+-crop $crop} -resize "$width" 0 "$RAW/$src.png" -o "$DEST/$name.webp"
done <<'EOF'
desktop-dashboard-viewport dashboard 1440
desktop-feed-viewport feed 1440
desktop-history-modal frame-detail 720 920 70 1040 1370
mobile-dashboard-viewport dashboard-mobile 786
EOF

ls -l "$DEST"/*.webp
if grep -rlF "$REPO" "$DEST" >/dev/null 2>&1; then
  echo "screenshots: a curated image's metadata names the checkout path" >&2
  exit 1
fi
echo "screenshots: done; look at every image in $DEST before committing"
