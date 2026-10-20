#!/usr/bin/env bash
# Starts, stops and reports a local Commander for UI passes (`make demo-up`,
# `make demo-down`, `make demo-status`):
#
#   scripts/demo-server.sh up|down|status
#
# The server records its own PID with -pid-file: a supervisor that took `$!`
# would hold whatever it backgrounded, which for a `(cd dir && cmd) &` chain is
# the subshell, not the server, and a stop aimed at that PID silently misses.
# Neither verb reports success on assumption: `up` returns once /healthz answers
# on the seeded port, `down` once the PID is confirmed gone. Any other outcome
# exits non-zero with the reason (and, for `up`, the log tail).
#
# Environment (the Makefile passes its variables through):
#   DEMO_DATA      data dir (default tmp/demo, gitignored, relative to the repo root)
#   SEED           a tools/<seeder> to run into a fresh DEMO_DATA before starting
#   SEED_ARGS      extra seeder arguments after the data dir (seedmosaic's fits root)
#   TAGS           build tags for the server ("demo" or empty)
#   PORT           rewrite the config's web_port before starting
#   MAX_LIFETIME   -demo-max-lifetime for a demo build (the binary defaults to 16h)
#   BIN            a prebuilt server binary to run instead of building one
#   LDFLAGS        go build -ldflags for the built binary
#   UP_TIMEOUT     seconds `up` waits for /healthz (default 60)
#   STOP_TIMEOUT   seconds `down` waits after SIGTERM before SIGKILL (default 30)
set -euo pipefail

cd "$(dirname "$0")/.."
DATA=${DEMO_DATA:-tmp/demo}
TAGS=${TAGS-demo}
PIDFILE="$DATA/server.pid"
LOG="$DATA/server.log"
# Marks a data dir this script created, the only kind `up SEED=` will wipe.
MARKER="$DATA/.demo-server"
UP_TIMEOUT=${UP_TIMEOUT:-60}
STOP_TIMEOUT=${STOP_TIMEOUT:-30}

say() { echo "demo-server: $*"; }
die() {
  echo "demo-server: $*" >&2
  exit 1
}

# pid_from_file prints the PID the pid file records, or nothing without one.
pid_from_file() {
  [ -f "$PIDFILE" ] || return 0
  local pid
  pid=$(tr -d '[:space:]' <"$PIDFILE")
  [[ $pid =~ ^[0-9]+$ ]] || die "$PIDFILE holds '$pid', not a PID; remove it if no server runs"
  echo "$pid"
}

alive() { kill -0 "$1" 2>/dev/null; }

# ours reports whether PID $1 is a server started on this pid file, so a PID a
# dead server's file names but the OS has since reused is never signalled.
# SERVER_RE matches a server's command line on this pid file: up always starts
# the run verb with -data-dir first, then -pid-file.
SERVER_RE="asiair-commander run -data-dir .* -pid-file $PIDFILE"
ours() { ps -o command= -p "$1" 2>/dev/null | grep -q -- "$SERVER_RE"; }

# strays lists live servers started on this pid file, whatever the file says.
strays() { pgrep -f -- "$SERVER_RE" || true; }

# served_url prints the URL the server logged once its listener bound.
served_url() {
  [ -f "$LOG" ] || return 0
  sed -n 's/.*msg="web server listening" url=\(http:[^ ]*\).*/\1/p' "$LOG" | tail -1
}

config_port() {
  local p
  p=$(sed -n 's/^[[:space:]]*web_port:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$DATA/config.yaml" 2>/dev/null | head -1)
  echo "${p:-8686}"
}

port_taken() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

stop() {
  local pid=$1
  if ! ours "$pid"; then
    die "PID $pid from $PIDFILE is not a server on this pid file ($(ps -o command= -p "$pid" 2>/dev/null | cut -c1-80)); remove the file by hand"
  fi
  say "sending SIGTERM to PID $pid"
  kill -TERM "$pid" 2>/dev/null || true
  local waited=0
  while alive "$pid" && [ "$waited" -lt "$STOP_TIMEOUT" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  if alive "$pid"; then
    say "PID $pid still running ${STOP_TIMEOUT}s after SIGTERM; sending SIGKILL"
    kill -KILL "$pid" 2>/dev/null || true
    waited=0
    while alive "$pid" && [ "$waited" -lt 5 ]; do
      sleep 1
      waited=$((waited + 1))
    done
  fi
  alive "$pid" && die "PID $pid is STILL RUNNING after SIGKILL; it was not stopped"
  # A clean shutdown removes the file itself; a killed server cannot.
  if [ -f "$PIDFILE" ]; then
    say "the server left its pid file (no clean shutdown); removing it"
    rm -f "$PIDFILE"
  fi
  say "PID $pid is gone"
}

# check_no_strays fails when a server on this pid file survives.
check_no_strays() {
  local left
  left=$(strays)
  [ -z "$left" ] || die "server process(es) on $PIDFILE still running: $left"
}

up() {
  local pid
  pid=$(pid_from_file)
  if [ -n "$pid" ] && alive "$pid"; then
    die "already running as PID $pid ($(served_url)); run make demo-down first"
  fi
  [ -z "$(strays)" ] || die "a server on $PIDFILE runs without its pid file (PID $(strays)); stop it first"

  if [ -n "${SEED:-}" ]; then
    [ -d "tools/$SEED" ] || die "no seeder tools/$SEED"
    # seedscan is itself a long-running process (the fake devices answering
    # broadcasts); it runs in its own terminal, see tools/seedscan/README.md.
    [ "$SEED" != seedscan ] || die "seedscan runs in the foreground beside the server; start it by hand, then make demo-up without SEED"
    if [ -e "$DATA" ] && [ ! -f "$MARKER" ] && [ -n "$(ls -A "$DATA")" ]; then
      die "refusing to wipe $DATA for SEED=$SEED: it was not created by demo-server"
    fi
    rm -rf "$DATA"
    local live=
    [[ " $TAGS " == *" demo "* ]] && live=1
    say "seeding $DATA with $SEED"
    # shellcheck disable=SC2086 # SEED_ARGS is a word list
    SEED_DIR="$DATA" SEED_LIVE="${SEED_LIVE:-$live}" go run "./tools/$SEED" "$DATA" ${SEED_ARGS:-}
  fi
  if [ ! -d "$DATA" ]; then
    mkdir -p "$DATA"
  fi
  touch "$MARKER"

  if [ -n "${PORT:-}" ]; then
    [ -f "$DATA/config.yaml" ] || die "PORT=$PORT needs a config.yaml in $DATA to rewrite"
    sed "s/^\([[:space:]]*web_port:\).*/\1 $PORT/" "$DATA/config.yaml" >"$DATA/config.yaml.tmp"
    mv "$DATA/config.yaml.tmp" "$DATA/config.yaml"
  fi
  local port
  port=$(config_port)
  # The server would fall back to another port, and a pass would then capture
  # whatever holds this one.
  port_taken "$port" && die "something already listens on :$port; stop it first"

  local bin=${BIN:-}
  if [ -z "$bin" ]; then
    [ -f internal/web/dist/index.html ] || die "internal/web/dist has no SPA; run make spa (make demo-up does)"
    bin=bin/demo-server/asiair-commander
    say "building $bin (tags: ${TAGS:-none})"
    go build -tags "$TAGS" -ldflags "${LDFLAGS:-}" -o "$bin" ./cmd/asiair-commander
  fi
  local extra=()
  if [ -n "${MAX_LIFETIME:-}" ]; then
    [[ " $TAGS " == *" demo "* ]] || [ -n "${BIN:-}" ] || die "MAX_LIFETIME needs a demo build (TAGS=demo)"
    extra=(-demo-max-lifetime "$MAX_LIFETIME")
  fi

  rm -f "$LOG"
  # The server is this shell's own child, so `$!` is right here; it is used only
  # to notice an early exit. Every later step reads the PID the server wrote.
  nohup "$bin" run -data-dir "$DATA" -pid-file "$PIDFILE" "${extra[@]+"${extra[@]}"}" >"$LOG" 2>&1 </dev/null &
  local child=$!
  say "started; waiting up to ${UP_TIMEOUT}s for /healthz on :$port (log: $LOG)"

  local deadline=$((SECONDS + UP_TIMEOUT)) url=""
  while :; do
    if ! alive "$child"; then
      echo "demo-server: the server exited during startup; tail of $LOG:" >&2
      tail -30 "$LOG" >&2
      exit 1
    fi
    url=$(served_url)
    if [ -n "$url" ] && curl -fsS -o /dev/null --max-time 2 "${url}healthz"; then
      break
    fi
    if [ $SECONDS -ge $deadline ]; then
      echo "demo-server: /healthz did not answer within ${UP_TIMEOUT}s; tail of $LOG:" >&2
      tail -30 "$LOG" >&2
      stop "$child" >&2 || true
      exit 1
    fi
    sleep 1
  done

  pid=$(pid_from_file)
  [ "$pid" = "$child" ] || {
    stop "$child" >&2 || true
    die "the pid file names '$pid', not the started server $child"
  }
  if [ "$url" != "http://localhost:$port/" ]; then
    stop "$pid" >&2 || true
    die "the server bound $url instead of :$port; stopped it"
  fi
  local cap
  cap=$(sed -n 's/.*DEMO: lifetime capped.*" \(max_lifetime=[^ ]* deadline=[^ ]*\).*/\1/p' "$LOG" | tail -1)
  [ -z "$cap" ] || say "demo lifetime cap: $cap (MAX_LIFETIME= to change it)"
  say "up: PID $pid, $url, data $DATA, log $LOG; stop it with make demo-down"
}

down() {
  local pid
  pid=$(pid_from_file)
  if [ -z "$pid" ]; then
    check_no_strays
    say "no pid file at $PIDFILE and no server on it; nothing to stop"
    return 0
  fi
  if ! alive "$pid"; then
    say "PID $pid from $PIDFILE is not running (a crash or kill left the file); removing it"
    rm -f "$PIDFILE"
  else
    stop "$pid"
  fi
  check_no_strays
  say "down: no server on $PIDFILE remains"
}

status() {
  local pid
  pid=$(pid_from_file)
  if [ -n "$pid" ] && alive "$pid"; then
    say "running: PID $pid, $(served_url), data $DATA"
    return 0
  fi
  [ -z "$pid" ] || say "stale pid file: PID $pid is not running"
  local left
  left=$(strays)
  [ -z "$left" ] || die "no live pid file, but server process(es) on $PIDFILE run: $left"
  say "not running"
  return 1
}

case "${1:-}" in
up) up ;;
down) down ;;
status) status ;;
*) die "usage: $0 up|down|status" ;;
esac
