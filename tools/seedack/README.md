# seedack — headless UI-pass harness for acknowledge-culled-frames

A local verification aid (sibling to `tools/seeddemo`) for the Acknowledge feature
([`docs/design/2026-10-17-library-reconciliation.md`](../../docs/design/2026-10-17-library-reconciliation.md)
§10). It stands up a real,
fully-wired Commander — web server, manager, worker goroutines, sqlite store — with
no ASIAIR hardware, seeded into exactly the frame states the UI pass needs, so the
whole flow can be exercised headlessly. The pattern generalises to any UI feature.

## What it seeds

`SEED_DIR=<dir> go run ./tools/seedack` writes a throwaway data dir (`config.yaml` +
`state.db`, web_port 8686, login password `12345678`) with two managed rigs:

- **rig-fra** (`keep_originals: false`) — one directory (`NGC 7000/NIGHT_3`) holding
  two verified-missing frames (acknowledgeable) and one **held** missing frame (not).
  This exercises the deliberate mismatch between the group button's raw count (**3**)
  and the dialog's acknowledgeable count (**2** + a `skipped: {not_verified: 1}`
  breakdown), and proves the held row offers no per-row control.
- **rig-keep** (`keep_originals: true`) — one verified-missing frame. The group count
  still renders, but every Acknowledge control is suppressed.

## Reproduce the browser pass

```sh
SCRATCH=/tmp/ackpass
# 1. seed (run from the repository root)
rm -rf "$SCRATCH/data" && SEED_DIR="$SCRATCH/data" go run ./tools/seedack
# 2. stage the SPA into the Go embed dir, THEN build — see gotcha 1
make spa
go build -o "$SCRATCH/server" ./cmd/asiair-commander
# 3. run (offline rigs are expected; acknowledge does not need the device)
"$SCRATCH/server" -data-dir "$SCRATCH/data" &
# 4. headless assertions + screenshots (Playwright is a global dependency, hence NODE_PATH)
NODE_PATH=$(npm root -g) OUT="$SCRATCH" node tools/seedack/uipass.js
```

`uipass.js` logs in, opens the missing panel on `/devices`, and asserts (13 checks):
group button `Acknowledge all 3`; exactly two per-row Acknowledge buttons; the group
dialog's `Delete 2 frames` title, `Not included: 1 not verified`, the `dir_gone`
notice, and the deleted-not-moved copy; the per-frame dialog; the `Acknowledged` pill
after confirm (and that row's button disappearing); and zero Acknowledge controls on
the keep_originals rig. Screenshots land in `$OUT/0*.png`.

The server-side handshake (challenge → confirm → pill, token replay → 403
`ACK_CONFIRMATION_INVALID`, held → 409 `ACK_NOT_VERIFIED`, keep_originals → 409
`ACK_KEEP_ORIGINALS`, group count/`skipped`/`dir_gone`, `ACK_NOTHING_TO_DO`, and a
CSRF-less POST → bare 403) is also drivable with `curl` against the same server:
`POST /api/session` (`{"password":"12345678"}`) for the cookie, then send the
`XSRF-TOKEN` cookie back as the `X-CSRF-Token` header plus an `Origin` header on every
mutation. Empty/`{}`/`{"dir":…}` bodies are challenges; a body with `token` confirms.

## Gotchas

1. **`make spa` before `go build`, every time.** The binary embeds
   `internal/web/dist` via `//go:embed all:dist` — NOT `web/dist`. `npm run build`
   writes `web/dist`; only `make spa` copies it into the embed dir. Skip it and the
   server serves a stale build, or the placeholder page when nothing was ever staged,
   and the UI you screenshot is not the one you changed. Everything `make spa` writes
   under `internal/web/dist` is gitignored, so it leaves the tree clean.
2. **Navigate the SPA with in-app nav, not `page.goto('/devices')`.** A hard reload
   drops the client session and bounces to `/login`. After login, click the `Devices`
   nav link (client-side routing) instead.
3. **The missing panel is on `/devices` (the `Devices` surface / `RigItem`), not the
   Dashboard.** The home page renders `.rigcard`s; the panel lives under `.rig-item`,
   revealed by that rig's `Missing frames` button.
4. **Offline rigs are fine.** The manager starts a worker goroutine for every
   `Managed` rig regardless of device reachability, and Acknowledge is a pure store
   mutation on that goroutine (the device is only touched during a delete pass). So
   the full challenge → confirm → `missing_acked_at` set path works with no hardware;
   the seeded rigs just log SMB session failures, which are expected and harmless.
