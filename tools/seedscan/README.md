# seedscan — the network-scan device count, and telling found devices apart

A scan returns the discovery **registry**, not the round's replies. The registry keeps an
unmanaged device for ten minutes after it stops answering and keeps a **managed** one
indefinitely, so a rig that is powered off mid-session stays in the list, and a count of that
list would include devices that are switched off. The scan count therefore counts only the
devices that answered. Exercising it needs a device that answers, then stops, then ages past the
135s staleness window, and none of that is reachable from `scripts/screenshots.mjs` (one URL per
surface) or from a Go test (the registry's retention is correct; the count is a matter of what
the UI makes of it).

This pair answers `scan_air` broadcasts as two fake ASIAIRs and drives the browser across that
timeline.

```
go run ./tools/seedscan <data-dir>                            # foreground; answers on UDP :4720
make spa && go build -o /tmp/ac ./cmd/asiair-commander && /tmp/ac -data-dir <data-dir>
OUT=<out-dir> AC_DATA_DIR=<data-dir> AC_PASSWORD=12345678 TARGET=http://localhost:8789 \
  NODE_PATH=$(npm root -g) node tools/seedscan/uipass.js
```

What it stages:

- `f4a00001` / ssid `FRA300-Pier` — bound to the seeded rig `rig-fra`, so it is **managed** and
  never evicted. This is the one that goes quiet.
- `5pa00002` / ssid `MyRig` — never adopted, answers throughout, so the "Found on your network"
  list stays populated and the second round still has something to count.
- `0no00003` — never adopted, and answers with **no ssid**, the case where naming has to fall back
  to the model-plus-guid label.
- `rig-uc` — a configured rig bound to a guid nothing ever answers, so it is absent from the
  registry entirely. That is why a three-rig install can report two devices.

All three report one identical model string, as same-generation hardware does. That is the second
thing this pair covers: with nothing but the model, three kits are indistinguishable in the found
list and prefill three identical rig names, so the ssid is what an operator actually recognises.
`uipass.js` asserts on the naming, on what the Add-a-rig and Edit dialogs say about which kit they
are configuring, and on the share selector. The fakes serve **no SMB**, so the real share probe
fails against them — which is itself worth asserting, since a device that cannot be reached must
stay configurable by hand. For the populated selector the pass stubs the shares endpoint at the
network boundary rather than standing up an SMB server, which is not what this seeder is for.

The managed device stops answering when `<data-dir>/quiet` appears — `touch` it to drive this by
hand, or let `uipass.js` create it between its two scans. A marker left by a previous run is
removed at startup, so the reproduction never begins already finished.

`uipass.js` asserts on the **change** across the two rounds, not on an absolute count: a real
ASIAIR on the same LAN answers these broadcasts too and legitimately adds to every round. The
check that matters is that the count falls by exactly one while `/api/discovered` still lists the
silenced device with `online: false` — retained, but not counted as having answered. A UI that
counted the registry instead would read `3 → 3` and fail it.

Allow about three minutes: the pass waits out the staleness window between rounds.

The unit-level half of this lives in `web/src/util/scanSummary.test.ts`, `deviceLabel.test.ts` and
`shareOptions.test.ts` (`npm test` in `web/`), which pin the wording, the counting rules and the
naming fallbacks without needing a LAN.
