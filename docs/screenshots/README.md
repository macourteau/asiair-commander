# Screenshots

The images in this directory are the curated screenshots the top-level README shows. They are
captured from the `-tags demo` build, whose in-memory rigs produce synthetic frames: no real
device, network or capture appears in them.

## Credit for the rendered sky

The star fields in the frame previews and thumbnails are rendered by `tools/starfield` from the
Gaia DR3 catalogue: **ESA/Gaia/DPAC**, licensed
[CC BY-NC 3.0 IGO](https://www.cosmos.esa.int/web/gaia-users/license). The renders are synthetic
(a projection of catalogue positions and magnitudes through a simple optical and noise model),
not photographs. No catalogue data or rendered frame is stored in this repository; see
[tools/starfield/README.md](../../tools/starfield/README.md).

## Regenerating

```
make screenshots
```

This builds the SPA and a demo binary, seeds a live demo data dir, runs the demo until enough
frames have been filed and plate-solved, captures every surface with `scripts/screenshots.mjs`,
and rewrites the WebP files here. The build is stamped with the latest `v*` tag, so the images
show a release version, and the update check is turned off in the seeded config, so the run never
calls GitHub. It needs Go, Node with a global Playwright install
(`npm install -g playwright`), `sqlite3` and `cwebp`. A first run downloads the Gaia cones, the
pinned `astap_cli` and ASTAP's d05 star catalog (about 100 MB) into the user cache directory;
later runs reuse them. `PORT=<port>` runs the demo on another port when 8686 is taken, and
`KEEP=1` keeps the scratch directory (data dir, server log, raw captures) for inspection.

Look at every image before committing: the capture is of whatever the demo shows at that moment.
