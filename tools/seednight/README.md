# seednight — staging the retarget preview's night arms

`scripts/screenshots.mjs` drives one URL per surface, so it cannot reach the retarget preview:
that card renders only after selecting frames on Tools and clicking Preview. This pair stages a
library that exercises every arm of the preview's night resolution and drives the browser to it,
the same shape `tools/seedack` uses for the acknowledge dialog.

```
go run ./tools/seednight <data-dir>
make spa && go build -o /tmp/ac ./cmd/asiair-commander && /tmp/ac -data-dir <data-dir>
OUT=<out-dir> AC_PASSWORD=12345678 TARGET=http://localhost:8788 \
  NODE_PATH=$(npm root -g) node tools/seednight/uipass.js
```

What it stages, and why each piece is there:

- `NGC7380` filed across `NIGHT_1` (2026-07-10), `NIGHT_2` (2026-07-13) and `NIGHT_3` (2026-07-16).
- `NIGHT_3`'s frames carry the OBJECT card `NGC 7380` — with `strip_target_spaces` on, that files
  into the same folder while the spelling stays distinct, which is what makes the
  `existing_spellings` line render.
- Mis-labelled `NGC 7830` frames on three observing dates: one the ledger already assigned (exact
  night, no caveat), one only the folder knows (adopted night, caveat), one never shot (projected
  night, caveat).
- One frame already on `NGC7380`, so an ineligible row sits beside the eligible ones.

Use a short data-dir path. The archive path renders verbatim into the preview, so a long one turns
the mobile overflow report into a reading about the path rather than the layout.
