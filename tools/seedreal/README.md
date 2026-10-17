# seedreal — a library of real FITS files

Writes a throwaway data dir (`config.yaml` and `state.db`, password `test1234`, port 8787)
whose frames point at real FITS files on disk, so a running server backfills real astrometry
and renders real previews. Where `tools/seeddemo` stages synthetic frames, this one runs the
real header-parse and classify path against captured subs.

```
go run ./tools/seedreal <data-dir> <fits-root>
SEED_FITS_ROOT=<fits-root> go run ./tools/seedreal <data-dir>   # same, root from the environment
go run ./cmd/asiair-commander -data-dir <data-dir>                # :8787, password test1234
OUT=<out-dir> AC_PASSWORD=test1234 TARGET=http://localhost:8787 \
  NODE_PATH=$(npm root -g) node scripts/screenshots.mjs
```

Run `make spa` before starting the server, or it serves the placeholder page.

What it stages:

- One managed rig, `rig-01`, so the Devices page has a card to render the seeded frames on.
- The first `.fit`/`.fits` file (by name) in each directory under `<fits-root>`, at most 24, so a
  per-filter capture tree yields one sub per filter rather than thousands of near-identical rows.
  Each is classified through `internal/classify` and recorded as a verified frame whose
  destination is the file itself. Nothing is copied or moved.

`internal/header/testdata` works as a `<fits-root>` when no capture tree is at hand; it yields a
single frame, since all its files share one directory. Do not commit screenshots of a real
capture tree without checking what its headers, target names and paths reveal.
