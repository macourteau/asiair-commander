#!/bin/bash
# Stands up the Samba/NT1 sidecars, runs the tagged integration tests against them
# over the real smb1client device path, directly and through a real tunnel, and
# tears the containers down. The test exit code is preserved. Usage: test/integration/run.sh [extra go test flags]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cd "$here"

# Seed the Samba build context with the committed FITS fixture (kept out of the
# image's git-tracked tree — see .gitignore) so the Dockerfile can COPY it.
mkdir -p samba/seed
cp "$root/internal/header/testdata/asiair_light_2600mm_ha.fit" samba/seed/asiair_light_2600mm_ha.fit

teardown() {
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap teardown EXIT

docker compose down -v --remove-orphans >/dev/null 2>&1 || true
docker compose up -d --build

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
      docker compose logs --tail 200 || true
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

if [ "$code" -ne 0 ]; then
  echo "=== samba logs (test failed) ==="
  docker compose logs --tail 200 || true
else
  # A passing run still fails on any smbd crash: the client recovers from a
  # killed connection by retrying, so a server-side fault it provokes would
  # otherwise hide behind a green job.
  if docker compose logs 2>/dev/null | grep -E "PANIC|INTERNAL ERROR"; then
    echo "=== samba crashed during an otherwise passing run (see above) ===" >&2
    code=1
  fi
fi

exit "$code"
