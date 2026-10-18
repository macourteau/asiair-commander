# starfield — synthetic sky for the demo and the screenshots

Renders ASIAIR-shaped 16-bit FITS lights of real star fields, so the `-tags demo` build and
the README screenshots show sky that previews, thumbnails and plate-solves like a real capture,
with no real capture (which carries PII) anywhere in the repository. Decision and rejected
alternatives: [architecture §14.9](../../docs/design/2026-10-17-architecture.md).

```
go run ./tools/starfield/cmd/starfield list                       # the preset targets
go run ./tools/starfield/cmd/starfield render -png -out <dir>     # every preset, plus PNG previews
go run ./tools/starfield/cmd/starfield render -out <dir> M45 M13  # just these
go run ./tools/starfield/cmd/starfield cachedir                   # where the cache lives
```

## Targets

Each is recognisable from its stars alone; the renderer draws no nebulosity.

| Target | Rig | Field | Exposure |
| ------ | --- | ----- | -------- |
| M45 (the Pleiades) | FRA300 (300 mm f/5) + ASI533MM Pro | 2.16° x 2.16° | 60 s L |
| Double Cluster | FRA300 | 2.16° x 2.16° | 60 s L |
| Coathanger (Collinder 399), turned hook-up | FRA300 | 2.16° x 2.16° | 60 s L |
| M44 (the Beehive) | FRA300 | 2.16° x 2.16° | 60 s L |
| M13 | 200 mm f/4 Newtonian + ASI533MM Pro | 0.81° x 0.81° | 30 s L |

Frames are unbinned 3008x3008 (about 18 MB). `XPIXSZ` is the binned pixel pitch, as an ASIAIR
writes it ([asiair-hardware-notes §8.1](../../docs/design/2026-10-17-asiair-hardware-notes.md)).
Every field solves with ASTAP's default d05 catalog; to check one by hand:

```
ASTAP_BIN=<astap_cli> ASTAP_CATALOG_DIR=<d05 dir> ASTAP_FRAME=<rendered .fit> \
  go test ./internal/platesolve -run TestRealSolveE2E -v
```

## The catalog: Gaia DR3, fetched on demand, never committed

Stars come from the ESA Gaia archive's synchronous TAP endpoint
(`https://gea.esac.esa.int/tap-server/tap/sync`), one cone per field against
`gaiadr3.gaia_source_lite`, limited to the field's half-diagonal and to the magnitude a frame
shows (G 14.5 for the refractor, 18 for the Newtonian): a few thousand to about twelve thousand
rows, seconds per query. A result that reaches the row cap is refused rather than rendered short.

Responses are cached verbatim under `<user cache dir>/asiair-commander/starfield/gaia/`, keyed by
the cone and the magnitude limit, and renders under `.../starfield/frames/`, keyed by a digest of
the target and the renderer version; the log says whether each came from the cache or the
network. Delete the directory to start over.

Gaia data are licensed [CC BY-NC 3.0 IGO](https://www.cosmos.esa.int/web/gaia-users/license);
credit **ESA/Gaia/DPAC**. So nothing Gaia-derived is committed or shipped: the cache is outside
the checkout, and only `tools/` and the `-tags demo` build import this package (never the
production binary, the image or the installer). Each rendered header carries a `COMMENT` with the
credit.

## The renderer

Deliberately small, cut to what a static frame needs:
the gnomonic (TAN) projection, a magnitude-to-flux model with one zero-point constant, a Gaussian
seeing core with a Moffat halo (so bright stars look bigger once they saturate), a Newtonian's
four diffraction spikes, a sky background with a gentle gradient and vignetting, Poisson shot
noise and Gaussian read noise. The noise is seeded from the target name, so a re-render gives
identical pixels. The header follows the ASIAIR fixtures in `internal/header/testdata`, with the
mount's `RA`/`DEC` a few arcminutes off the true centre and no WCS, so Commander's own solve
places the frame; it carries no site, observer or GPS keyword.

## In the demo

`cmd/asiair-commander/demo.go` renders every target once at startup (from the cache after the
first run) and, for each generated frame, puts that render behind a fresh header and an ASIAIR
Autorun name such as `Autorun/Light/M45/Light_M45_60.0s_Bin1_533MM_gain100_20261018-031502_-10.0C_0004.fit`.
The FRA300 rig cycles the four clusters; the Newtonian shoots M13. Each rig holds at most three
rendered frames at a time, since the throttled pipeline pulls an 18 MB frame more slowly than the
generator ticks. A target that cannot be rendered is logged, and its rig falls back to
random-byte frames under a synthetic header; a remote scenario's rigs (`tools/seedremote`) always
use those.
