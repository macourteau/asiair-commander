#!/bin/bash
# Stands up the Samba/NT1 sidecars, runs the tagged integration tests against them
# over the real smb1client device path, directly and through a real tunnel, and
# tears the containers down. The test exit code is preserved. Usage: test/integration/run.sh [extra go test flags]
#
# smb1client dials port 445 and nothing else, so the direct mode publishes the
# Samba container on the host's 127.0.0.1:445. When that port is already taken —
# macOS File Sharing, a NAS's own Samba — or AC_INTEGRATION_ISOLATED=1 is set, the
# script runs isolated instead: no host port is published, and the tests run in a
# Go container sharing the "samba" container's network namespace, where
# 127.0.0.1:445 is that container's smbd and a socat relay on 127.0.0.1:1445 reaches
# "samba-remote". The Go image follows go.mod's toolchain line, and the module and
# build caches persist in the ac-integration-gomod and ac-integration-gocache volumes.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cd "$here"

# Seed the Samba build context with the committed FITS fixture (kept out of the
# image's git-tracked tree — see .gitignore) so the Dockerfile can COPY it.
mkdir -p samba/seed
cp "$root/internal/header/testdata/asiair_light_2600mm_ha.fit" samba/seed/asiair_light_2600mm_ha.fit

port_open() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null; }

isolated="${AC_INTEGRATION_ISOLATED:-}"
if [ -z "$isolated" ] && [ -z "${AC_INTEGRATION_SMB_HOST:-}" ] && port_open 127.0.0.1 445; then
  echo "127.0.0.1:445 is already in use on this host; running isolated (no host port published)"
  isolated=1
fi

compose_files=(-f docker-compose.yml)
if [ -n "$isolated" ]; then
  compose_files+=(-f docker-compose.isolated.yml)
fi
dc() { docker compose "${compose_files[@]}" "$@"; }

teardown() {
  dc down -v --remove-orphans >/dev/null 2>&1 || true
}
trap teardown EXIT

dc down -v --remove-orphans >/dev/null 2>&1 || true
dc up -d --build

if [ -n "$isolated" ]; then
  goversion="$(sed -n 's/^toolchain go//p' "$root/go.mod")"
  if [ -z "$goversion" ]; then
    goversion="$(sed -n 's/^go //p' "$root/go.mod")"
  fi
  samba="$(dc ps -q samba)"
  remote_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$(dc ps -q samba-remote)")"
  echo "isolated: golang:${goversion} in the network namespace of samba; 1445 relays to samba-remote at ${remote_ip}"
  set +e
  docker run --rm --network "container:${samba}" \
    -v "$root":/src -w /src \
    -v ac-integration-gomod:/go/pkg/mod -v ac-integration-gocache:/root/.cache/go-build \
    -e REMOTE_IP="$remote_ip" \
    "golang:${goversion}" bash -c '
      set -euo pipefail
      if ! (apt-get update -qq && apt-get install -y -qq socat) >/tmp/apt.log 2>&1; then
        cat /tmp/apt.log >&2
        echo "could not install socat for the samba-remote relay" >&2
        exit 1
      fi
      socat TCP-LISTEN:1445,fork,reuseaddr "TCP:${REMOTE_IP}:445" &
      for port in 445 1445; do
        echo "waiting for SMB on 127.0.0.1:${port} ..."
        for i in $(seq 1 60); do
          if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
            echo "SMB port ${port} open after ${i}s"
            break
          fi
          if [ "$i" -eq 60 ]; then
            echo "SMB never came up on 127.0.0.1:${port}" >&2
            exit 1
          fi
          sleep 1
        done
      done
      AC_INTEGRATION_SMB_HOST=127.0.0.1 go test -tags "integration remotetest" ./test/integration/ -v -count=1 "$@"
    ' bash "$@"
  code=$?
  set -e
else
  # Wait for both Samba containers to accept TCP on their published loopback
  # ports: 445 for the direct tests, and the remote-LAN one (docker-compose.yml).
  host="${AC_INTEGRATION_SMB_HOST:-127.0.0.1}"
  for port in 445 1445; do
    echo "waiting for SMB on ${host}:${port} ..."
    for i in $(seq 1 60); do
      if (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; then
        exec 3>&- 3<&- 2>/dev/null || true
        echo "SMB port ${port} open after ${i}s"
        break
      fi
      if [ "$i" -eq 60 ]; then
        echo "SMB never came up on ${host}:${port}" >&2
        dc logs --tail 200 || true
        exit 1
      fi
      sleep 1
    done
  done

  # remotetest admits the local test relay the real-tunnel tests run through
  # (remote_tunnel_test.go); no release is built with it.
  set +e
  AC_INTEGRATION_SMB_HOST="$host" go test -tags "integration remotetest" "$root/test/integration/" -v -count=1 "$@"
  code=$?
  set -e
fi

if [ "$code" -ne 0 ]; then
  echo "=== samba logs (test failed) ==="
  dc logs --tail 200 || true
else
  # A passing run still fails on any smbd crash: the client recovers from a
  # killed connection by retrying, so a server-side fault it provokes would
  # otherwise hide behind a green job.
  if dc logs 2>/dev/null | grep -E "PANIC|INTERNAL ERROR"; then
    echo "=== samba crashed during an otherwise passing run (see above) ===" >&2
    code=1
  fi
fi

exit "$code"
