# seedremote — every remote-bridge card state, live

The remote bridge's cards show states that only exist at runtime: a remote whose heartbeat has
been failing for less (Reconnecting) or more (Unreachable) than the threshold, a rig that keeps a
live session while its remote drops, a rig's last-known line, which is the verdict of a listing
made by this very process. None of that can be written into config or the store, and a real
remote needs a second PC and a relay. This seeder stages it with no hardware and no network.

```
go run ./tools/seedremote <data-dir>
make spa && go build -tags demo \
  -ldflags "-X main.version=$(git describe --tags --always) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o <bin> ./cmd/asiair-commander
<bin> -data-dir <data-dir>                                   # :8790, password 12345678
OUT=<out-dir> AC_DATA_DIR=<data-dir> NODE_PATH=$(npm root -g) node tools/seedremote/uipass.js
```

The `-ldflags` matter: the stale-build notice compares the remote's build date with Commander's,
and an unstamped build has none, so the notice never shows.

## How it works

The seeder pairs nine remotes in `config.yaml` and writes their scenario to
`<data-dir>/seedremote.json`. A `-tags demo` build that finds that file replaces only the
supervisor's tunnel client constructor (`cmd/asiair-commander/demo_remote.go`): each remote is an
in-memory remote process (a `tunnel.Fake` serving a scripted remote protocol), and the rigs
behind them are the demo build's in-memory devices. The supervisor, its heartbeat and state
machine, the remote sites, discovery's broadcast rounds through them, the rig workers and the hub
all run unchanged, so every state is derived exactly as in production. A normal build has no
such path: `demo_off.go` makes the seam a no-op, and a demo build without the scenario file dials
real tunnels.

The timeline runs through markers in `<data-dir>/seedremote-ctl/`, which the demo build polls:

- `stop-<id>` stops remote `<id>`'s process for good, as `docker compose down` on the remote PC
  would;
- `link-down` drops Commander's own link while it exists: no client reaches its relay, and every
  tunnel flow fails.

`touch` them to drive the timeline by hand, or let `uipass.js` do it. The seeder clears them, and
the previous run's store, on every run; it refuses a non-empty directory that holds no scenario,
so a mistyped path cannot wipe a real data dir.

## What it stages

| Remote | Staged as |
| --- | --- |
| `r1` Spring Camp | Waiting for first connection: its process never runs. |
| `r2` Hilltop Observatory | Connected (direct), with a refused forward at its rig's address and a rejected client key, and an unadopted ASIAIR (`MyRig-Hilltop`) on its LAN. |
| `r3` Dark Sky Lodge | Connected (relayed). |
| `r4` Pier Shed | Needs update (remote): an older data-unit digest; its build is 40 days older than Commander's, so the stale-build notice shows. |
| `r5` Farm Barn | Needs update (Commander): a newer whole-file digest. |
| `r6` Valley Field | Stopped late in the pass: Reconnecting, with `Valley Askar`'s last-known line. |
| `r7` Ridge Pier | Stopped early: Unreachable by capture time. `Ridge FSQ` shows its last-known line; `Ridge Newtonian` is paused before the drop. |
| `r8` Creek Shed | Stopped with Valley Field. `Creek Seestar` reads slowly from a full device, so it is mid-session through the drop and shows the "via Creek Shed (reconnecting)" line. It has a remote of its own because a remote site admits one transfer at a time: a sibling rig would queue behind its slow copy and never end its own cycle. |
| `r9` Desert Camp | Re-paired during the pass and never given the new `.env`: Waiting, with `Desert Askar`'s last-known line. |

`Home FRA300` is a local rig, so the remote pill has something to contrast with.

`uipass.js` asserts each state on the live snapshot and captures Devices and Dashboard on the
mobile and desktop profiles twice: `relay-down-*` while Commander's link is down, and `final-*`
with every remote state above and the relay back up. It writes `overflow-mobile.json` (the
mobile horizontal-overflow report) and `final-snapshot.json` beside the PNGs. The unreachable
threshold is seeded at two minutes, so allow about four.

## Known noise

- The demo devices report no free space, so every rig raises a device-disk-low alert, and the
  demo frames of different rigs share one synthetic header, so filing them raises
  canonical-name clash alerts; both are the demo build's, not the remote bridge's.
- The found device serves no SMB, so the Devices page's share probe for it fails (a 502 in the
  browser console, "Shares couldn't check" on its card).
- A real ASIAIR on the same LAN answers the local broadcast and shows up as found, as with every
  other seeder.
