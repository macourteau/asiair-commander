# seeddemo — a demo data dir, static or live

Writes a throwaway data dir (`config.yaml` and `state.db`, password `12345678`, port 8686)
for screenshotting the SPA with realistic content.

```
SEED_DIR=<data-dir> go run ./tools/seeddemo               # static: offline rigs, seeded history
SEED_DIR=<data-dir> SEED_LIVE=1 go run ./tools/seeddemo   # live: for a -tags demo build
```

Without `SEED_LIVE` the two rigs have no GUID and sit offline; the store carries a spread of
frames in every state, so History, the frame detail and the rig cards' counts have content
with a normal build.

With `SEED_LIVE=1` the rigs carry the GUIDs the `-tags demo` build keys its in-memory devices
by (`cmd/asiair-commander/demo.go`), with delete-after-grace on and fast timings, so the feed
shows frames go pending → transferring → filed → deleted within seconds. The store is left
empty except for one held frame per rig (below), because the generator produces every other
frame end to end. Those frames carry synthetic sky from `tools/starfield`: the FRA300 cycles the
Pleiades, the Double Cluster, the Coathanger and the Beehive, the Newtonian shoots M13, and every
frame previews and plate-solves (see [tools/starfield/README.md](../starfield/README.md)).
`make screenshots` drives this mode end to end for the README images.

## The device file browser

The File browser page (`/files`) needs files on the devices and a delete dialog whose
unarchived count does not drift while the live demo drains frames. `internal/demofiles` is
the contract between this seeder and the demo build:

- Both rigs carry a `Preview/` tree that the harvester never touches, with
  `Preview/M42/<name>.fit` and its `_thn.jpg` on both, so copying `Preview/M42` from the
  FRA300 to the Newtonian shows two conflicts and one new file.
- Both rigs carry `Autorun/Light/M42/Light_M42_300.0s_Bin1_2600MM_gain100_20261007-231502_-10.0C_0001.fit`,
  a full-length ASIAIR frame name under the watched `Autorun`, the longest unbroken string the
  panel renders on a phone.
- This seeder records that frame as **held** on both rigs, with the device file's size and
  mtime. `decide()` skips a held row whose size and mtime match the listing
  (`internal/rig/decide.go`) and the delete pass selects only verified rows and culled
  unclassified ones (`DeletionEligible`, `internal/store/store.go`), so the file stays on the
  device and the delete dialog counts it as 1 unarchived file on every run. The held rows
  also raise the rig cards' never-deletable backlog alert, which is expected here.

A demo build with a remote scenario (`tools/seedremote`) stages none of these files.

`scripts/screenshots.mjs` captures the panes (its `files` surface addresses both
by query string). The dialogs are not URL-addressable, so `uipass.js` drives them on the
mobile and retina desktop profiles: both panes, the copy plan with a conflict, the copy
report, the copy plan with the watched warning, and the delete dialog with its unarchived
count and checkbox. For rename: the toolbar with Upload and Rename, the Rename dialog on
`Preview/Stack_M42_20261007.jpg` with its stem selected, a taken name refused inline, a real
rename and its toast, Rename refused in the watched `Autorun/Light/M42` (the folder notice's
"Items here can't be renamed." and the button's reason) and on the `Autorun` folder itself
(the visible refusal line). For upload, each file picked through `setInputFiles` on the
pane's hidden input: the Sending state (the staging request held back by a route), the plan
with a conflict on the Newtonian's `Preview/IC 1805` frame, the Overwrite choice and the
result toast; the plan with the watched warning into `Autorun`; the reserved-path refusal of
a file named `Autorun` at the share root; a row skipped as `appeared` (the file put there
through the API between the plan and the run); and a failed row from a run whose request
fails at the network, whose Retry writes the staged file without a second staging request.
Each mobile capture is checked for horizontal overflow from the panel or the dialog, and the
probes are written to `devfiles-overflow-mobile.json`.

```
SEED_DIR=<data-dir> SEED_LIVE=1 go run ./tools/seeddemo
make spa && go run -tags demo ./cmd/asiair-commander -data-dir <data-dir>   # :8686
OUT=<out-dir> NODE_PATH=$(npm root -g) node tools/seeddemo/uipass.js
```

The pass writes to the demo devices and undoes each write through the API: the copied
frame and the uploaded `..._0003.fit` are deleted from the Newtonian, the rename is reversed,
and the overwritten `Preview/IC 1805` frame is written back with its seeded size and time,
so a re-run (and the second profile) plans the same operations. It never runs the delete
dialog. Keep
the data-dir path short: the Settings roots render it verbatim and inflate the mobile
overflow report of the other surfaces.
