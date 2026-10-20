# seedmosaic — a mosaic filed flat, for the re-file tool

Writes a throwaway data dir (`config.yaml` and `state.db`, password `test1234`, port 8787)
holding a mosaic that was filed flat: one top-level folder per pane (`Sadr_1-1/` …
`Sadr_2-5/` side by side). That is the library an operator has when mosaic detection was off
while the mosaic was shot, and it is what the Tools page's re-file panel exists to repair.

```
make demo-up SEED=seedmosaic SEED_ARGS=<fits-root>   # :8787, password test1234
make demo-down                                       # when done
```

`demo-up` runs `go run ./tools/seedmosaic <data-dir> <fits-root>` into its data dir first and
builds a plain binary (a `-tags demo` build would bring `rig-01` online against an in-memory
device).

Then log in, turn on mosaic detection in Settings, and run the re-file preview on Tools. The
screenshot harness works against it too (`TARGET=http://localhost:8787 AC_PASSWORD=test1234`).

What it stages:

- Every light frame among the `.fit`/`.fits` files under `<fits-root>` (not one per directory,
  as `tools/seedreal` takes, because a mosaic's panes all live in one capture folder). Each is
  classified through the real header-parse path, routed with the default light template, copied
  into the seeded light root at that path, and recorded as a verified frame whose destination is
  the copy. Other frame types are skipped.
- Mosaic detection seeded **off**, so turning it on is the step under test.
- One managed rig, `rig-01`, with `keep_originals` on and a host under `.invalid`. The host is
  never contacted; it is there because a managed rig with no device fails config validation and
  Settings would refuse every save.

The panes come from the files' own OBJECT cards, so `<fits-root>` needs frames whose targets carry
the ASIAIR mosaic planner's `<base>_<row>-<col>` names. Any other tree still seeds, but gives the
re-file panel nothing to regroup.
