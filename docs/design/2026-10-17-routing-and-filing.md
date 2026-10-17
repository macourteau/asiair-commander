# Classification, routing and filing

**Scope.** How a frame's FITS header decides where its archived copy is filed. It covers
classification by header and the quarantine that catches anything that cannot be routed with
confidence; the folder templates, their tokens and the path-safety contract that every
substituted value passes through; night numbering — the observing date, the `(target, OTA)`
scope, the never-renumber rule, the destination folder deciding the ordinal for a date the scope
has not seen, and case as target identity; mosaic panel grouping; re-filing archived frames to
the current templates, which is a move and not a filing; and retargeting, which edits the
archive's `OBJECT` card, keeps the device anchor intact and re-applies the edit on every re-pull.
It hosts the night invariants INV-N1–INV-N7 (§6.9), the retarget invariants INV-1–INV-10 (§9.9),
the mosaic and re-file invariants INV-M1–INV-M11 (§10) and the mosaic and re-file acceptance
criteria (§11). The pull, the at-rest verify, the atomic claim of a filename and the delete pass
are owned by [frame-lifecycle](2026-10-17-frame-lifecycle.md); the meaning of each frame-row
column, including the device and archive anchors, is owned by
[frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md).

## 1. Classification by header

Every FITS frame starts with a header that says what it is: frame type, target, exposure, sensor
temperature, filter, camera and date. Commander reads that header — the front of the file, never
the data unit — and files the frame by what it is, never by the folder or filename it sat under
on the device. A rig's profile lists the source roots to watch (`Autorun/Light`, `Plan`, the
eMMC or USB share), and header classification sorts whatever it finds there, which is what lets
mosaics, plans and calibration runs file without special cases.

Classification is the pure function `classify.Classify` (`internal/classify`). It performs no
I/O and never panics on an odd header value: a value that cannot be read as the expected kind
leaves that field's presence flag false, and the router (§2.1) decides whether that absence is
routing-critical for the template in play.

### 1.1 Frame type: the IMAGETYP alias table

`IMAGETYP` is not standardised. The ASIAIR writes title-case single words (`Light`, `Dark`,
`Flat`, `Bias`), FITS-padded to eight characters; NINA writes upper case; other software writes
`Dark Frame`. The value is trimmed and then matched **case-insensitively on both sides** against
the editable `alias_table`, which maps it to one of the canonical types `LIGHT`, `DARK`, `FLAT`
or `BIAS`. One entry therefore covers `Light`, `LIGHT` and `light`. The shipped table is:

| Alias | Canonical type |
|---|---|
| `light` | `LIGHT` |
| `dark` | `DARK` |
| `flat` | `FLAT` |
| `bias` | `BIAS` |
| `dark frame` | `DARK` |
| `bias frame` | `BIAS` |

A value the table does not know is **not guessed**, and neither is a missing `IMAGETYP`. The
frame is quarantined (§2) and the alert names the trimmed value, so the operator can add the
alias and re-classify (§2.3).

### 1.2 Header facts routing relies on

The observed ASIAIR header behaviour these rules rest on is recorded in
[hardware notes §8](2026-10-17-asiair-hardware-notes.md); only the routing consequence is stated
here.

| Card | What routing takes from it, and why |
|---|---|
| `OBJECT` | The target. Stored raw (trimmed) in `frames.target`; transformed only where it is rendered (§1.4). |
| `DATE-OBS` | UTC with no zone suffix, with or without fractional seconds (a trailing `Z` is tolerated). Converted to the site's local zone before the observing date is taken (§6.1). |
| `SET-TEMP` | The cooler **set-point**, never `CCD-TEMP`. A bias shot while the cooler was still ramping reads its set-point while its sensor reading is degrees away; filing by the reading would scatter matching calibration frames across near-equal folders, while the set-point is the stable key that makes a dark and a light belong together. The ASIAIR writes `SET-TEMP` only while the cooler is on, so an absent card means an uncooled frame: `<SET-TEMP>` renders `uncooled`, giving uncooled frames their own bucket rather than quarantining them. |
| `EXPTIME` | Carries float noise (`0.001` arrives as `0.00100000004749745`), so `<EXPOSURE>` rounds to the millisecond first. Flats are auto-exposed and differ slightly frame to frame, which is why the shipped flat template omits `<EXPOSURE>`. |
| `FILTER` | Absent on one-shot-colour cameras, so `<FILTER>` is the one optional token (§3.4). |
| `INSTRUME` | Mapped to a short sensor name through the editable `sensor_map`, matched exactly on the trimmed value. An unmapped instrument quarantines a frame whose template names `<SENSOR>`. |
| `TELESCOP` | Not used. The ASIAIR writes the **mount** there, not the optical tube, so `<OTA>` comes from the rig profile. |
| `XBINNING`/`YBINNING` | `<BINNING>`; a missing `YBINNING` is taken to equal `XBINNING`. |
| `GAIN`, `OFFSET` | Integers. |

All frame types from one camera at one binning are the same byte length, so size identifies
nothing; that is why startup reconciliation never matches by size
([architecture §7.11](2026-10-17-architecture.md)).

### 1.3 Numeric values are coerced, never asserted

The FITS reader hands back a card value as an `int64` or a `float64` depending on how the
capture software wrote it (`EXPTIME = 60` versus `60.0`). `model.Header.Int` and
`model.Header.Float` (through `model.AsInt` and `model.AsFloat`) accept either type and a
numeric string. `Int` accepts a float only when it is integral. A value that cannot be coerced
reports absent; nothing type-asserts. A bare `.(float64)` would panic, and a recovered panic
would turn every frame from that capture software into a `failed` row, which fills the device.
An absent value quarantines a frame only where the frame's template needs it (§2.1).

### 1.4 Target normalisation

`<TARGET>` renders the target through `route.transformTarget`, the only place the two target
transforms live: when `strip_target_spaces` is on (the shipped default) every internal space is
removed, and then the value is trimmed — **strip, then trim**. The result is sanitised as part of
its segment (§4.1). `frames.target` keeps the raw `OBJECT`, so the transform is applied wherever
a target is rendered and never baked into the stored column. Mosaic panel detection parses the
transformed value (§7.1). Case is never changed by these transforms; folding a case-variant
spelling onto the one a target was first filed under is a separate rule (§6.6).

## 2. Quarantine and re-classify

### 2.1 Any routing failure quarantines

`unclassified` is the catch-all for every frame that cannot be routed with confidence:

- the header cannot be read (a truncated or non-FITS file that passed quiescence);
- `IMAGETYP` is missing or unknown to the alias table (§1.1);
- a light lacks `OBJECT` or `DATE-OBS` — the night-derivation inputs, routing-critical for every
  light whatever its template names;
- a token the **applicable** template references cannot be resolved;
- a template segment renders empty, except when it emptied solely because `<FILTER>` was absent
  (§3.4);
- a mosaic-only token appears in a template that is not the active mosaic template (§7.2);
- defensively, a non-canonical type or an unconfigured template.

"Routing-critical" is precisely the tokens the applicable template references, plus `OBJECT` and
`DATE-OBS` for a light. It is not a fixed list: a light is never quarantined for lacking `GAIN`
when the light template does not name `<GAIN>`, and a flat is judged only against the flat
template. The router signals the outcome with a `*route.QuarantineError` naming the missing
input.

| Token | Unresolvable when |
|---|---|
| `<TARGET>` | `OBJECT` absent, or the transformed value sanitises to empty |
| `<TARGET_BASE>` | the base before the panel suffix sanitises to empty (`OBJECT = "_1-1"`) |
| `<SENSOR>` | `INSTRUME` absent or has no `sensor_map` entry |
| `<DATE>`, `<YEAR>`, `<MONTH>`, `<DAY>` | `DATE-OBS` absent or unparseable |
| `<EXPOSURE>` | `EXPTIME` absent or non-numeric |
| `<GAIN>`, `<OFFSET>` | the card absent or not an integer |
| `<BINNING>` | `XBINNING` absent or not an integer |
| `<SET-TEMP>` | never: an absent set-point renders `uncooled` |
| `<FILTER>` | never: an absent filter collapses (§3.4) |
| `<OTA>`, `<SESSION>`, `<RIG>` | not individually; a blank profile value quarantines only if its whole segment renders empty, which validation reports at save (§3.5) |

A quarantined frame is filed, never deleted while its NAS copy is present, and raises a warning
alert in the `unclassified` category. It keeps its device original as the authoritative copy; only
once the operator culls its NAS copy does it become deletion-eligible, through the cull drain
([library-reconciliation §11](2026-10-17-library-reconciliation.md)). Because such frames stay on
the device, the never-deletable backlog alert escalates to a "capture may stop" critical on a
capacity-constrained rig; the alert catalogue is [architecture §10](2026-10-17-architecture.md).

### 2.2 The unclassified area

The quarantine area is `<light root>/unclassified/<rig id>/`, the rig id passed through the same
sanitiser as every path segment. It is per rig, so re-classify has a clear scope, and under the
light root, so it shares the single managed volume
([architecture §7.5](2026-10-17-architecture.md)) and files through the same atomic,
disambiguating, same-volume rename as every other filing
([frame-lifecycle §8.1](2026-10-17-frame-lifecycle.md)).

### 2.3 Re-classify

Re-classify is a per-rig command that re-runs classification and routing over the rig's
`unclassified` frames and moves each one that resolves. It is dispatched to the rig's worker
and runs on the worker goroutine ([architecture §3.6](2026-10-17-architecture.md)), never in the
HTTP handler, so its moves and `dest_path` rewrites never race live filing into the same tree.

For each frame it re-reads the header from the archived copy, classifies it, resolves the night
through the same `nightFor` the live path uses (§6.4), routes it, and files the existing NAS file
through the shared filing path as a local rename: no network bytes are charged, the device and
archive anchors are carried through unchanged (a rename changes no bytes), the state becomes
`verified`, and `pulled_at` — the grace anchor — is **preserved**, because the copy was already
proven on the NAS when it first landed in quarantine. The row's `dest_path` is written in one store
write once the rename has succeeded, never before it, so the row never names a path the file has not
reached. A frame that still cannot be classified or routed stays where it is. Each frame runs under
its own `recover`, so a malformed header leaves that frame unclassified instead of killing the
worker. Afterwards every `unclassified` alert whose frame is no longer unclassified is acknowledged,
and a `rig.reclassified` event reports how many frames were examined and moved, since a re-classify
produces no transfer for the live feed to show.

## 3. Templates and tokens

### 3.1 One template per frame class

A frame is filed under a directory rendered from a path template:

| Template | Applies to | Root | Shipped default |
|---|---|---|---|
| `templates.light` | `LIGHT` | light | `<TARGET>/<OTA>/<SESSION>/NIGHT_<n>` |
| `templates.cal.DARK` | `DARK` | cal | `<SENSOR>/DARK/<GAIN>/<BINNING>/<SET-TEMP>/<EXPOSURE>/<DATE>` |
| `templates.cal.FLAT` | `FLAT` | cal | `<SENSOR>/FLAT/<OTA>/<GAIN>/<BINNING>/<DATE>` |
| `templates.cal.BIAS` | `BIAS` | cal | `<SENSOR>/BIAS/<GAIN>/<BINNING>/<SET-TEMP>/<DATE>` |
| `templates.mosaic` | a `LIGHT` whose target parses as a mosaic panel, only while `mosaic_detection` is on | light | `<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>` |

Calibration has one template per canonical type — a map, not one shared string — because the
useful grouping differs by type. The template selection is `route.selectTemplate`, shared by
`route.Route` and `route.NightFolder` so the folder a night ordinal is read from is rendered from
the same template the frame is filed through. Lights are organised by what was shot; calibration
by the gear, because a dark is reusable across every target shot with that sensor. `<SESSION>` is
a fixed folder name from the rig profile, the container for the `NIGHT_n` folders.

Commander owns the calibration root, which holds the **raw** calibration frames it pulls. It does
not create or manage stacked masters: an operator's own per-target `CAL` folder of masters, and
any stacking output, are outside its remit, and re-file's prune never removes a directory that
holds them (§8.9).

### 3.2 Calibration correctness drives the depth

The nesting is not arbitrary. The rule is: **a calibration template must include every parameter
that varies between the calibration frames and the lights they calibrate.**

- A dark or bias is a property of the **sensor** (thermal and read signal), so it is
  independent of the optical train. Darks carry `<SET-TEMP>` and `<EXPOSURE>`; bias carries
  `<SET-TEMP>`.
- A flat is a property of the **optical train** (dust, vignetting), so `FLAT`, and only `FLAT`,
  carries `<OTA>`. Flats of different filters share a date folder; filter and exposure stay in
  the flat's filename.
- Every calibration template carries `<GAIN>` and `<BINNING>`: a master must match the light's
  gain and binning, and omitting them would file a gain-100 dark beside a gain-0 one.

To append to an existing calibration library, the operator sets `sensor_map` and `date_format`
to that library's convention (for example a shorter sensor name, or a different date layout); both are
configuration, not code.

### 3.3 The token table

The rendering is specified exactly, so the running service and its golden tests render
byte-identical paths. These spellings are canonical; the Settings token chips use them, and
validation rejects any other `<…>` (§3.5).

| Token | Source | Renders as | Example |
|---|---|---|---|
| `<TARGET>` | `OBJECT`, transformed (§1.4) | verbatim | `NGC7000` |
| `<OTA>` | rig profile | verbatim | `FRA300` |
| `<SESSION>` | rig profile | verbatim | `Summer2026` |
| `<RIG>` | rig profile id | verbatim | `rig-01` |
| `<n>` | night ordinal (§6) | integer, no padding; the folder is written `NIGHT_<n>` | `NIGHT_2` |
| `<SENSOR>` | `INSTRUME` via `sensor_map` | the mapped short name | `6200MM` |
| `<DATE>` | observing date (§6.1) | `date_format`, a Go layout (shipped `2006-01-02`) | `2026-06-18` |
| `<YEAR>`, `<MONTH>`, `<DAY>` | the same observing date | `2006`, `01`, `02` | `2026`, `06`, `18` |
| `<SET-TEMP>` | `SET-TEMP` set-point | integer °C rounded, sign kept, `C` suffix; `uncooled` when absent | `-30C` |
| `<EXPOSURE>` | `EXPTIME` | rounded to the millisecond, shortest decimal, `s` suffix | `300s`, `0.001s`, `1.675s` |
| `<FILTER>` | `FILTER`, optional | verbatim | `Ha` |
| `<GAIN>` | `GAIN` | `gain` + integer | `gain100` |
| `<OFFSET>` | `OFFSET` | `offset` + integer | `offset50` |
| `<BINNING>` | `XBINNING`/`YBINNING` | `Bin<X>`, or `Bin<X>x<Y>` when the axes differ | `Bin1`, `Bin1x2` |
| `<TARGET_BASE>` | the transformed target with the panel suffix removed | verbatim | `Sadr` |
| `<PANEL>` | the panel suffix | verbatim, zero-padding preserved | `1-1` |
| `<PANEL_ROW>`, `<PANEL_COL>` | the parsed row and column | integer, no padding | `1` |

`<YEAR>`, `<MONTH>` and `<DAY>` read the same instant as `<DATE>`, so a date-split tree and a
`<DATE>` stamp always agree. The last four tokens are valid only in the mosaic template (§7.2).
`<n>` applies to lights only; calibration is filed by date and never night-bucketed.

### 3.4 Optional tokens collapse

`<FILTER>` is the only optional token. When the card is absent it renders nothing, and
`route.renderSegment` absorbs one adjacent separator (`_`, `-`, `.`, `,` or a space; the
following one first, otherwise the preceding one) so the segment carries no dangling, leading or
doubled punctuation (`NGC7000_` or `2600MM__1.7s`). If the segment then renders empty it is
dropped from the path rather than quarantined, so no `//` appears. Every other token that renders
empty in a segment that renders empty is a quarantine (§2.1), never a degenerate path such as
`TARGET//SESSION`. Letters and digits are never absorbed, because they can be literal template
content.

### 3.5 Template validation

Templates are validated by every path that saves the config: `PUT /api/config`, first-run setup,
the guid-binding applier, and the remote supervisor's commit of a remotes edit. `config.Load` does
not validate — a hand-edited `config.yaml` reaches routing unchecked, which is why the router
itself quarantines rather than trusting validation — but it does normalise.
`config.NormalizeTemplates` runs on load, on save, in setup and in the Settings preview: it
rewrites `\` to `/` in every template, and fills a blank mosaic template with the shipped default.
Normalising at load is load-bearing, not tidiness: the guid-binding applier validates and saves
the config it loaded without passing through the save handler, so a blank mosaic template that
only the save handler filled would fail the emptiness rule there and block the binding. Load
normalisation is also what carries the backslash rewrite to a config that is never re-saved.

| Rule | Severity | Applies to |
|---|---|---|
| No `..` segment | error | every template |
| Relative: no leading `/` or `\` | error | every template |
| No `:` (no drive letter) | error | every template |
| Contains `<n>` | error | light; mosaic while detection is on |
| Every `<…>` is a canonical token | error | every template |
| A mosaic-only token is used only in the mosaic template | error | light, cal |
| Not empty | error | mosaic while detection is on |
| Distinguishes panels above the first `<n>` segment (§7.3) | error | mosaic while detection is on |
| Worst-case realized path within 260 characters | warning | every template, scored against its root |
| A rig whose `ota` (or `session`) is blank while a template names `<OTA>` (or `<SESSION>`) | error | per rig and per field, naming every template that needs it; the mosaic template counts only while detection is on |

The light template must contain `<n>` because dropping it would flatten every night into one
folder and leave the startup rebuild (§6.3) with no `NIGHT_n` folders to read, silently
renumbering. Calibration templates use `<DATE>` and are exempt. Emptiness is required only of
the mosaic template: making it global would reject a present-but-empty calibration key on
installs that are valid. The path-length estimate counts literal characters plus 20 per token
and a 48-character filename, deliberately moderate so genuinely deep trees warn and ordinary ones
do not. The `<OTA>`/`<SESSION>` cross-check exists so a blank profile value is caught at save
rather than quarantining every frame mid-night; it emits one problem per rig per field, because
the Settings page routes field errors by field.

Acceptance of every token other than the four mosaic-only ones is global on the server; the
Settings page scopes tokens per template kind on the client, and its validator checks token
membership only, so the `<n>` and emptiness rejections surface when saving.

## 4. Path safety

### 4.1 The sanitisation contract

`OBJECT`, `INSTRUME`, `FILTER` and the free-text templates are device- or user-controlled and
flow into filesystem paths, so the contract is stated rather than delegated. `route.renderSegment`
substitutes every token in a segment and then passes the **whole** substituted segment through
`sanitize.Component`, the one implementation of the rule set:

- path separators (`/`, `\`), the other characters NTFS refuses (`<>:"|?*`) and control
  characters are each replaced with `_`; an embedded separator is neutralised, never treated as a
  boundary, so a value can never add a path level;
- trailing dots and spaces are trimmed (Windows strips them silently, so names differing only by
  them would collide anyway); a value of `.`, `..` or only dots and spaces therefore sanitises to
  empty, which is a quarantine for a routing-critical segment — there is no separate traversal
  check because none is needed;
- a Windows reserved device name (`CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`),
  matched case-insensitively on the part before the first dot, is prefixed with `_` so a
  legitimate value such as a filter named `Aux` still files;
- the result is capped at 200 runes (headroom under NTFS's 255-unit component limit) and
  re-trimmed;
- case is never changed.

`sanitize.Component` returning `""` is the single signal that a value is unusable; the router
quarantines on it. The rule set is unit-tested against adversarial values (`..\..\Windows`,
`CON`, embedded `\`) in `TestComponent`, `TestComponent_EmptySignalsQuarantine` and
`TestAdversarialTargets`, not only against the happy-path fixtures. The same sanitiser renders
the device basename into the destination filename, falling back to `frame_<short hash><ext>`
when the basename sanitises away. Template-level path safety (`..`, absolute roots, drive letters)
is enforced at save (§3.5).

### 4.2 Long paths

A deep calibration template under a nested destination root can exceed the legacy 260-character
Windows `MAX_PATH` and fail the atomic rename after a good pull. All destination filesystem access
goes through the `localfs` seam, which maps each absolute path to an extended-length path
(`\\?\`, or `\\?\UNC\` for a share) on Windows, so the limit does not apply; on other platforms
there is no such limit to lift. The save-time worst-case estimate (§3.5) still warns when a
plausible frame would produce an unreasonable path.

## 5. Filename uniqueness

The routing function is not injective and never needs to be. ASIAIR sequence counters reset every
capture run, and two rigs — including two sharing a camera model on the calibration path, which
routes by sensor — routinely emit identically-named frames of distinct content. Treating every
clash as an error would push a whole night into quarantine. Instead, uniqueness is guaranteed when
the file is written: the canonical name is claimed with an exclusive rename, a clash with
**identical** content is an idempotent re-pull, and a clash with different content takes the next
candidate on a deterministic ladder — the `DATE-OBS` token, then lengthening `sha256` prefixes —
so both frames are kept under distinct names. A canonical-name content clash raises a `rot_clash`
alert. The mechanism is owned by [frame-lifecycle §8.1–§8.3](2026-10-17-frame-lifecycle.md).

This one rule covers multi-rig, shared-sensor and counter-reset clashes uniformly, with no
per-scenario template coupling, no obligation to hand-craft clash-proof templates and no separate
collision tree. `<RIG>` is available for an operator who wants rig-segregated trees; it is a
preference, not a safety requirement.

## 6. Night numbering

`NIGHT_1`, `NIGHT_2`, … are invented by Commander, not by the ASIAIR. The operator writes them
into notes and stacking scripts, so an ordinal, once given, is an external identifier.

### 6.1 Observing date and scope

A light's **observing date** is `route.ObservingDate`: convert `DATE-OBS` into the site's local
zone, subtract an absolute 12 hours, and take that local calendar date. A night is a
noon-to-noon window and is labelled by its opening noon, so a frame captured at 02:00 carries the
previous evening's date. The worker computes the value once per frame and feeds it to both the
night lookup and the `<DATE>` token, so the light and calibration trees agree on dates.

`night_counters(target, ota, observing_date, night)`, unique on
`(target, ota, observing_date)`, records which ordinal each observing date was given. Its key
uses the fixed layout `2006-01-02` (`route.ObservingDateKey`), never the editable `date_format`,
so changing the display format can never re-key an existing night.

The ordinal is the count of distinct observing dates **per `(target, OTA)`** — the scope of the
directory it labels. The OTA half comes from the rig profile. Imaging one target through several
telescopes is ordinary, and each telescope's folder keeps its own `NIGHT_n` sequence. Only lights
are numbered; the router ignores the ordinal for calibration frames.

### 6.2 An assigned night is never renumbered

The first time an observing date is seen in a scope it is assigned an ordinal and persisted; it
is never renumbered, so a frame filed under `NIGHT_2` never moves because an earlier date appears
later. An earlier date that arrives late simply takes the next free ordinal. Ordinals are never
decremented either: retargeting or re-filing frames out of a scope leaves its counter as it was.

`store.AssignNight` runs in one write transaction and applies three rules, because the hints it
is given (§6.4) were read on the read pool and may be stale:

1. A date that already has a row keeps its ordinal; the hints are ignored.
2. A `Preferred` ordinal that a **different** observing date in the scope already holds is
   discarded. One ordinal serves at most one date per scope, and a stale hint must not be what
   breaks that.
3. With no usable hint the ordinal is `max(MAX(night), FolderHighWater) + 1`.

Two mechanisms are jointly load-bearing and neither suffices alone. The unique constraint, with
`INSERT … ON CONFLICT DO NOTHING` and a read-back of the stored row, guarantees each date maps to
exactly one ordinal even when two rigs sharing a scope insert it at once. The single-writer store
([architecture §7.9](2026-10-17-architecture.md)) serialises the `MAX + 1` computation, so two
different new dates cannot both read the same maximum; the constraint cannot catch that, because
the dates differ.

`store.LookupNight` lets the filing path skip both the destination-folder read and the write
transaction when the date is already assigned, so only the first frame of a night opens a write
transaction.

### 6.3 Rebuilding the counters at startup

If the state database is lost, `rig.RebuildNightCounters` reconstructs `night_counters` from the
library. It runs locally, needs no device, and completes **before any rig worker starts**, so a
live pull can never race the rebuild and mis-number a night. Startup runs the re-file intent
recovery (§8.6) and the supersede intent recovery
([frame-lifecycle §11.9](2026-10-17-frame-lifecycle.md)) first — a file a crashed move already
placed under the new tree would otherwise be read as a second tree — and a failure of any of the
three fails startup, because starting workers over an unresolved state is the failure, not a
symptom of it. [architecture §7.11](2026-10-17-architecture.md) places this step in startup
reconciliation.

The walk descends the light root looking for directories named exactly `NIGHT_<n>` with `n ≥ 1`;
it stops descending at each one. The folder name supplies the ordinal. The `target` and
`observing_date` come from **one representative frame's header** per folder — the first `.fit`,
`.fits` or `.fts` file directly inside it whose header carries `OBJECT` and `DATE-OBS` — and the
OTA from the first path segment that matches one of the configured rigs' OTA values, rather than
from reverse-parsing the editable template, so an edited or reordered template cannot break the
mapping. One header suffices because a single night's frames all fall in one noon-to-noon window.
Each entry is inserted with `ON CONFLICT DO NOTHING`, so an existing ordinal is never changed,
and its target is folded onto the canonical spelling (§6.6) inside the transaction, because two
folders of one tree can hand back two case-variant spellings.

An empty `NIGHT_n` folder, or one with no readable frame directly inside it, anchors no date and
is skipped: its ordinal is not reserved, so a later frame for that real date takes the next free
ordinal instead of being wedged behind an unmappable gap. With no light root configured the
rebuild does nothing, rather than walking a drive root.

**A library split across two layouts collapses silently.** The walk takes the target from the
header, not the path, so `Sadr_1-1/<OTA>/<SESSION>/NIGHT_1` (filed flat) and
`Sadr/<OTA>/<SESSION>/Sadr_1-1/NIGHT_1` (filed through the mosaic template) yield one
`(target, ota, observing_date)` key, and `ON CONFLICT DO NOTHING` discards the second entry with
no log. The discarded entry is identical to the kept one — both trees were numbered from the same
ledger row, so they carry the same `NIGHT_n` — which makes a split library untidy rather than
destructive, and it is tolerated rather than alerted on (§12). Re-file (§8) heals a split only
among frames that still have rows (§8.4); older panes on a delete-enabled rig stay split. The same
collapse is why re-file intent recovery must run before the rebuild, as above.

### 6.4 The destination folder decides a new date

The ledger is keyed by the raw `OBJECT` (folded for case, §6.6), but the folder a frame lands in
is whatever the template renders — the target with spaces stripped, trimmed and sanitised. Two
spellings of one target (`NGC 6888` and `NGC6888` with space-stripping on) therefore render one
folder while drawing from two ledger scopes, and `MAX(night) + 1` over a fresh scope is 1. A
renamed ASIAIR target, or a retarget typed the way the folder reads, would land a night in
`NIGHT_1` beside that folder's real first night. So for a date its scope has not yet assigned,
the ordinal is decided by looking at the destination folder:

```
  look the date up in night_counters
     ├── found ─────────────────────────────────────────► use it
     └── not found: read the destination night folders
            ├── one already holds this observing date ──► adopt its ordinal
            └── none does ─────────────────────────────► max(scopeMAX, folderMAX) + 1
```

Both arms matter. Adoption joins a frame to the night its folder already holds for that date.
Minting above the folder's high-water mark keeps a genuinely new date from landing on an occupied
`NIGHT_1` when the destination target was shot under another spelling.

**The folder is consulted, never stored.** `rig.nightFor` renders the prefix from current
configuration at every use, queries with it, and discards it, so nothing template-derived is
persisted to go stale (§6.10 records why every stored key is rejected).

**Rendering the destination.** `route.NightFolder` returns the rendered segments above the first
`<n>`-naming segment, plus a closure that renders the night folder's own name for a candidate
ordinal through the same `renderSegment` and sanitiser `Route` uses — the night segment may name
other tokens (`<DATE>_NIGHT_<n>`). The prefix is rendered through `renderSegment` wholesale too,
including its drop-and-absorb behaviour for an absent `<FILTER>`: concatenating separately rendered
pieces would diverge from `Route` for an OSC frame. A candidate whose night name fails to render
(its segment quarantines) is dropped from the consultation; it does not fail the frame. It shares
`Route`'s template selection, including the mosaic check, and `route.SplitAtNight` splits with
`Route`'s semantics (separators normalised, empty segments kept) so the prefix aligns index for
index with `Route`'s segment list; the mosaic validator uses the same function (§7.3). The
consultation is disabled — filing proceeds on the scope's own counter — when the template names no
`<n>`, when the prefix renders empty (a template whose first segment names `<n>` would otherwise
range over the whole library), or when the frame has no applicable template.

**A quarantine is not a store error.** `NightFolder` returns the same `*QuarantineError` that
`Route` would. Every caller (the live pull, re-classify and retarget apply) discriminates it with
`errors.As`: on a quarantine the frame proceeds with night 0 and `Route` re-derives the identical
quarantine from the same segments, filing it unclassified, loudly and recoverably. Treating every
error alike would be wrong: sending a quarantine down the store-error path would replace a loud,
recoverable quarantine with a hot loop that defers the frame every cycle, and in re-classify, which
skips a frame by returning without a word, a silent one. A store error, by contrast, **aborts the
filing** and defers the frame to the next cycle: proceeding with empty hints would mint `scopeMAX +
1`, the land-on-an-occupied-folder failure this exists to prevent. Every caller builds its routing
options once (`rig.routeOptionsFor`) and passes the same configuration snapshot to both the folder
read and `Route`, because if the two diverged the folder consulted would not be the folder filed
into.

**Reading the folder** takes two index-ranged steps over `frames.dest_path`, so the date and the
path provably come from the same folder:

1. `store.NightsUnder` returns the distinct ordinals of every row under the prefix.
2. For each candidate `k`, `store.NightFolderCounts` counts the rows in that **exact** night
   folder, and how many of them fall in this observing date's window. `COALESCE` on the `SUM` is
   required: zero rows is the normal case for a pruned folder, a fresh folder and every candidate
   sourced from a sibling subtree, and `SUM` over zero rows is `NULL`.

The exact-folder range is the shape check, so no depth rule is needed: rows nested below the
night folder (a savable `<TARGET>/NIGHT_<n>/<FILTER>` template) still count, and rows in a sibling
subtree — the panel trees that the shipped mosaic template places under a plain target's prefix —
fall outside it. `folderMAX` is the highest `k` with any rows. The **adopted** ordinal is the `k`
holding the most frames of this observing date, ties broken by the higher `k`: one folder can hold
one date at two ordinals, and bare highest-wins cements the error whenever the contamination runs
upward.

The ranges are byte-exact half-open intervals built by `rig.folderRange`: the base (joined to the
light root with the same join filing uses) plus a trailing separator — guarded against doubling
when a root is itself a drive or UNC root — up to the successor byte of that separator, which
excludes true siblings such as `M31_old` beside `M31`. A range, not `LIKE` or `GLOB`: `LIKE` is
case-insensitive for ASCII, so a Linux install would match `M31/` against `m31/`, and `GLOB` has no
`ESCAPE` clause while `[` and `]` survive sanitisation.

The observing-date window is the exact inverse of `route.ObservingDate`
(`rig.observingWindow`): both bounds are the observing date plus 12 hours, and the next calendar
date plus 12 hours. An absolute 24-hour span is wrong at one end of a DST day, which is 23 or 25
hours long, and would skew the counts the adoption is decided on. The bound strings are rendered to
the second with no zone suffix: `date_obs` is stored as RFC 3339 with nanoseconds, which drops the
fraction at zero nanoseconds, and since `.` sorts below `Z` a suffixed bound would push every
fractional timestamp in a window's first second into the previous date.

The folder cannot see frames whose rows were pruned, nor acknowledged culls whose `dest_path` was
nulled. An unseen folder yields no adoption and a lower `folderMAX`: a missed adoption, never a
wrong one. Rows marked missing by reconciliation keep their `dest_path` and stay visible.

Every assignment is logged at Info (`rig: night assigned`) with the decision (adopt or mint), the
ordinal, the target and any spelling it was folded from, the OTA, the observing date, the resolved
folder and both hints — the only field-diagnosable trace of why a frame took a given night.

### 6.5 Previews state the night

The retarget preview (§9.7) resolves each frame's ordinal through the same `nightHints` read the
filing path uses and states it in the projected path; it never renders `NIGHT_0`, and it never
calls `AssignNight`, because a previewed and abandoned retarget must not spend an ordinal. The
projection (`rig.nightProjector`) models apply exactly:

| Case | Night shown | Caveat |
|---|---|---|
| The date already has a ledger row | that ordinal | none |
| The destination folder holds the date, **and no other date in the scope holds that ordinal** (rule 2 of §6.2, asked as `store.NightHeldByOtherDate` and against the batch's own projections) | the ordinal it would adopt | yes |
| Otherwise | `max(scopeMAX, folderMAX, runningMAX) + 1` | yes |

It carries three distinct pieces of state, and conflating any two breaks it:

| State | Keyed on | Why |
|---|---|---|
| The folder-read cache | (prefix, observing date) | Step 2's matching count depends on the date window; a prefix-only key would give every frame of a multi-date batch the first frame's answer. |
| The resolved-ordinal memo | observing date alone | Apply's ledger hit is scope-wide, so two frames of one date under different prefixes get one ordinal; a (prefix, date) key would show two. It also makes a repeated id cost nothing. |
| The running maximum | the batch | Advanced by every arm that takes an ordinal — mints, adoptions and failed reads — in apply's id order, which the apply handler follows without deduplicating. |

Frames of one observing date always show one ordinal. A read the preview cannot make leaves the
row eligible, carrying an informational reason, at the next ordinal the batch would take: a
preview that cannot reach the folder or the ledger must not declare frames ineligible for a reason
that has nothing to do with them, and it never falls back to `NIGHT_0`. Only
the already-assigned arm is exact; an adoption or a mint can be shifted by a live pull, a template
edit, a re-file or an earlier frame of the same apply. So `NightApprox` is set when the template
names `<n>` and the ordinal is not from the ledger (a calibration frame, or a light template with
no night segment, carries no caveat), and also whenever the template contains the non-canonical
`<NIGHT>`. A `<NIGHT>`-only template therefore shows the caveat although no folder consultation
runs (§6.4 disables it for a template naming no `<n>`). The page warns that the night number may
change, because a frame filed before the apply can shift it.

The projection equals apply except where the batch's own moves change the destination's
occupancy: if a batch's frames of one date are alone in the destination's highest `NIGHT_k` and
adopt a lower ordinal, applying them empties that folder, and a later date in the same apply mints
lower than projected. The outcome is still safe — the mint lands in an unoccupied folder — and the
row already carries the caveat, so modelling how a batch reshapes the folder it reads is not worth
its complexity.

### 6.6 Case is target identity

On Windows and on an SMB share `M27` and `m27` name one directory, so two case-variant
`OBJECT` spellings would be two ledger scopes writing into one tree: the second mints from an empty
scope, and the byte-exact folder range built from its spelling cannot see the rows filed under the
other, so the folder consultation misses too. Comparing paths case-insensitively is the wrong
lever — right on Windows and SMB, wrong on a case-sensitive volume, and it would leave the archive
genuinely forked into two trees wherever the filesystem is case-sensitive. **The lever is the
target:** a case-variant `OBJECT` is folded onto the spelling that target was first filed under,
so one target renders one path and the byte-exact range works on every filesystem.

The canonical spelling is the first row for that target in `night_counters`, by insertion order
(`store.CanonicalTarget`). That table is the right home: it is the scope the ordinal is minted in,
it is not the rolling window `frames` is, and its first row is the spelling the destination folder
was created under, so the canonical spelling and the directory on disk agree by construction. The
fold happens:

- in `nightFor`, before anything else, on every light filing path (live pull, re-classify,
  retarget apply), which also writes the folded target to the frame row — the ordinal and the
  spelling cannot be settled apart, since `<TARGET>` renders the folder the ordinal is scoped to;
- in the retarget operation, before `RewriteObject` writes the card, so the card agrees with the
  row (a disagreement would fail the re-apply check on every later re-pull, §9.4); the preview
  folds the same way, keeping the typed spelling if the read fails;
- at every night-counter read in the store (`target = ? COLLATE NOCASE` in `AssignNight`,
  `LookupNight`, `MaxNight` and `NightHeldByOtherDate`), so the ledger is one scope even for a
  caller that did not fold, and a projection still models `AssignNight` exactly;
- in the startup rebuild (§6.3).

Consequences, accepted:

- **A case-only rename is not a rename.** Retargeting `M 27` to `m 27` is reported as a
  no-op, because the folder it asks for is the folder the frames are already in.
- **Nothing is renumbered and nothing moves.** Frames already filed into the wrong `NIGHT_n` stay
  there (§6.8).
- **Rows written under a forked spelling keep it.** No migration rewrites `night_counters`; the
  case-insensitive reads make a mixed-spelling ledger behave as one scope. Such a scope can hold
  one ordinal against two dates, and rule 2 then refuses to adopt it, so the effect is a mint,
  never a wrong adoption.
- **Only lights fold.** The shipped calibration templates name no `<TARGET>`.

### 6.7 Spelling divergence stays visible

Unifying the night does not unify the archive. Frames retargeted to `NGC6888` beside siblings
written as `NGC 6888` carry two `OBJECT` spellings, and anything grouping by `OBJECT` — re-file
included, which routes off the raw stored target — sees two targets. So the retarget preview
reports `existing_spellings`: the raw spellings (`store.DistinctTargetsUnder`) found in the
**exact night folder** the projection resolved, that differ from the target as it will be written.
It compares against the sanitised, folded target, because `RewriteObject` sanitises before writing
and comparing against the typed string would report a spurious divergence for `NGC:6888`. It reads
the resolved night folder rather than the prefix, or a plain-target retarget under the shipped
mosaic template would report every panel's spelling. On the mint arm the folder does not exist
yet, so the list is empty by construction. The page renders it as "Already filed here as …
— applying will leave two spellings of this target in one folder", listing up to three.

Nothing is rewritten on the operator's behalf and the row stays eligible. There is no eligibility
check on the typed target: `RewriteObject` sanitises before writing the card, so `CON`, `M31.` and
`NGC:6888` retarget successfully, landing as `_CON`, `M31` and `NGC_6888`. The no-op guard stays a
string comparison after folding, because a spelling-only retarget (`NGC6888` → `NGC 6888`) is the
supported way to re-point frames at a target's established spelling.

### 6.8 What night numbering does not do

- **It does not move frames.** Frames already filed into the wrong `NIGHT_n` stay there. Re-file
  cannot repair it: it passes each frame's stored `night` and never calls `AssignNight` (INV-M9).
- **It does not recover a folder whose frame rows are gone** (§6.4), nor, at rebuild, a folder
  whose `<n>` segment is not the last one — the rebuild reads only files directly inside a
  `NIGHT_<n>` folder — nor a night segment named other than exactly `NIGHT_<n>`. Both degrade to a
  missed adoption or an unreserved ordinal.
- **It does not survive losing the state database after the highest night folder was deleted.**
  The rebuild finds only the folders that exist, so the next night can reissue the deleted
  folder's ordinal. A durable ledger was considered and not built (§6.10).
- **Cross-scope racing is accepted.** Rule 2 is scope-local, so two workers in different scopes
  sharing one folder (two rigs on one OTA name, or a template omitting `<OTA>`) could mint the same
  ordinal into it at once. Closing it would put folder-shape queries inside the write transaction,
  to guard a window that opens only when two scopes file the first frame of one night at the same
  instant on a single-writer store.
- **One scope can span several night folders.** A savable template with a per-frame-varying token
  above the night segment (`<TARGET>/<FILTER>/NIGHT_<n>`) puts one scope's frames in several
  folders. In a fresh scope the first frame decides the ordinal from its own folder, and the ledger
  row then forces every sibling folder to it, so a later frame of that date can land in a sibling
  `NIGHT_k` holding a different date. The shipped templates render one folder per scope per date,
  because `<OTA>` and `<SESSION>` come from the rig profile; consulting every folder a scope could
  reach is not enumerable from the template. Accepted, and pinned by a test so it is a known
  quantity.
- **A prefix quarantine spends no ordinal; a quarantine below `<n>` does.** A light that
  quarantines on a token above the night segment is caught before `AssignNight`; one that
  quarantines on a token below it (an absent `<GAIN>` in `<TARGET>/NIGHT_<n>/<GAIN>`) has already
  taken its ordinal, because the order is `nightFor`, then `Route`.
- **A light template lacking `<TARGET>` can produce multi-target night folders**, which the
  rebuild then reads through one representative header. Validation does not require `<TARGET>` in
  the light template. The mosaic route to the same shape is closed (§7.3) except for the
  component-truncation residual of §7.1. Both routes are accepted.

### 6.9 Invariants INV-N1–INV-N7

INV-N1 is the property the folder consultation exists to establish. It is asserted over
`dest_path`s rather than over `night_counters`, because the harm is two nights sharing a
directory.

| ID | Invariant | Test |
|---|---|---|
| INV-N1 | Under the shipped templates, and within one `(target, ota)` scope, no two observing dates are filed into one night folder. The qualification is not decoration: §6.8 records two ways the absolute form fails — the cross-scope race and one scope spanning several folders — and a test written to the absolute statement cannot pass. | `TestOneObservingDatePerNightFolder` |
| INV-N2 | A frame whose destination night folder already holds its observing date joins that night, whatever its `OBJECT` spelling. | `TestAdoptsNightHeldByDestinationFolder`, `TestRetarget_JoinsExistingNightLedger`, `TestLivePullAdoptsFolderNightAfterSpellingChange`, `TestAdoptedNightIsDecidedByFrameCount` |
| INV-N3 | A frame whose observing date no folder holds is issued an ordinal above every ordinal that folder holds. | `TestMintsAboveFolderHighWater`, `TestNestedRowsCountTowardTheNightFolder` |
| INV-N4 | An observing date that already has a ledger row is never renumbered, whatever the folder holds. | `TestAssignedNightIgnoresFolder`, `TestAssignNightAppliesHintRules` |
| INV-N5 | Only the frame's own night folder is consulted — never a sibling subtree, never a deeper descendant of another night folder. A case-variant path cannot arise: the target is folded before the folder is rendered (§6.6), so one target renders one path. | `TestNoAdoptionAcrossFolders`, `TestSanitizedSiblingFoldersMintSeparately`, `TestNightFolderRangeQueries` |
| INV-N6 | A preview never mutates `night_counters`, and its projection equals what apply produces for every frame apply actually files, except where the batch's own moves change the destination's occupancy (§6.5). A frame that fails before filing (hash drift, at-rest verify) consumes no ordinal while the preview already advanced its running maximum, so the equality is asserted over successful frames. | `TestPreviewMatchesApply` |
| INV-N7 | Two `OBJECT` spellings differing only in case are one target: one ledger scope, one destination folder, one `frames.target` value — the spelling that target was first filed under. | `TestLivePullFoldsObjectCase`, `TestLivePullFoldsObjectCaseMidNight`, `TestRetarget_FoldsTargetCase`, `TestNightLedgerFoldsTargetCase` |

### 6.10 Rejected designs

Every rejected way of keying the ledger computes a durable identity for "this folder" and stores it.
A target-derived key cannot match the folder, because the folder comes from the template; a
folder-derived key matches at the instant it is written and rots afterwards, because templates are
editable at runtime, re-file relocates the library without touching the ledger, and retention prunes
frame rows so the key cannot be rebuilt.

| Alternative | Rejected because |
|---|---|
| Key the ledger on the transformed target | Fixes the shipped templates but not a template that normalises the panel index, concatenates the target with another token in one segment, or omits the target. |
| Key on the mosaic `(base, panel)` split | Breaks the shipped mosaic default: its leaf `<TARGET_BASE>_<PANEL>` sanitises as one piece, so `Sadr._1-1` and `Sadr_1-1` are two genuine folders the split key merges. |
| Key on the rendered directory above `<n>` | The stored key rots under template edits, re-file and row pruning, and it redefines `NIGHT_n` from "the nth night of this target" to "the nth night of this folder". |
| Refuse templates whose folders cannot tell targets apart | Accepts a bare-`<PANEL>` leaf that still collides, ignores the scope's OTA half, refuses the row/column layout INV-M11 accepts, and a refused light template blocks saving any other setting. |
| Re-key existing rows by copy-up | Manufactures the collision it exists to prevent, and the repair is order-dependent under `ON CONFLICT DO NOTHING`. |
| Adopt only, never mint above the folder | Leaves the failure unrepaired for any date the destination target never shot. |
| One `GROUP BY night` over the prefix subtree | `MIN(date_obs)` and `MIN(dest_path)` are independent aggregates, so the sampled date and path can come from different folders; under the shipped mosaic default it adopts a wrong night for targets sorting after `NIGHT_` and silently disables adoption for those sorting before it. |
| A depth rule ("exactly one component below the night folder") | Breaks `<TARGET>/NIGHT_<n>/<FILTER>`, where every row sits two components down, collapsing `folderMAX` to 0. |
| Bare highest-wins on an ambiguous folder | Cements the error whenever the misfiled frames landed in the higher folder. The count decides. |
| Use the hints without re-validating them in the transaction | They are stale cross-pool reads; two dates could land on one ordinal. |
| Call `AssignNight` from the preview | It mutates: an abandoned preview would permanently spend an ordinal. |
| A preview eligibility check mirroring the writer's refusals | The writer sanitises before writing, so the check would refuse retargets that work. |
| Compare `dest_path` case-insensitively | Filesystem-dependent, and leaves a case-sensitive archive forked; folding the target fixes it everywhere (§6.6). |
| Store the resolved night folder on each `night_counters` row as a hint | Survives row pruning, but reintroduces a stored template-derived value for a case that already degrades to a missed adoption. |
| Relocate already-misfiled frames automatically | Larger and riskier than the defect; a spelling-only retarget re-points future frames. |

**A durable ordinal ledger** is rejected: an append-only JSONL file beside the
state database, with a second copy in the staging root, unioned into the startup rebuild so that
deleting the highest `NIGHT_n` folder and then losing the database could not reissue its ordinal
(§6.8). It would reduce that exposure rather than eliminate it (a record lost
in a crash, or both copies lost together, still reissues), the failure already needs both a
deleted highest folder and a lost database, the folder scan recovers every ordinal whose folder
survives, and the ledger would add a second persistent store with its own merge, torn-line,
seeding and two-copy failure modes.

**Repairing a night folder that already holds two observing dates** is deferred: a `plan`/`apply`
operation that would renumber the later-assigned date to a fresh ordinal (rule 3 of §6.2) and then
let the existing re-file move its frames, keeping re-file's "writes only `dest_path`" contract
(INV-M1) intact. It is not built because the folding of §6.6 prevents the cause at source and the
remaining causes are the two accepted residuals of §6.8; because it would weaken INV-N4 from "never
renumbered" to "never renumbered automatically" for every later reader; and because re-file is
row-driven and rows of deleted frames are pruned after seven days (§8.4), so on a delete-enabled rig
a mixed night would be repairable for about a week, while a mixed night is usually found at stacking
time, long after that window has passed. A `keep_originals` rig never prunes, so a collision on such
a rig, or one from a cause other than case, is what would justify building it. Editing
`frames.night` and `night_counters` by hand and then running re-file is the manual escape hatch;
moving files by hand is not (§8, and [library-reconciliation
§12](2026-10-17-library-reconciliation.md)). Any future version of this repair must be a
`plan`/`apply` operation, never a continuous reconciler: folders rearranging without a prompt is the
harm INV-N4 exists to prevent.

## 7. Mosaic panels

The ASIAIR's mosaic planner produces one distinct `OBJECT` per pane (`Sadr_1-1` … `Sadr_2-5` for a
2×5 grid). Because `<TARGET>` renders `OBJECT`, each pane would land as a sibling top-level folder,
burying the year's real targets under look-alike directories. Mosaic detection files a mosaic's
panes under one parent folder instead — `Sadr/FRA300/Summer2026/Sadr_1-1/NIGHT_1` — through a
separate template, exactly as calibration frames get their own template per type:

```
mosaic detection OFF (default) ──> nothing below happens; routing is unchanged

mosaic detection ON:
  OBJECT = "Sadr_1-1"
            └──┬─┘ └┬┘
            base   panel (row-col)

    LIGHT + panel suffix parsed ──> route with templates.mosaic
    anything else               ──> route with templates.light / templates.cal
```

Because an in-progress mosaic already has panes filed flat, and dragging folders by hand is a
data-loss path (§8), re-file (§8) is the supported way to move archived frames.

### 7.1 The panel suffix is a producer contract

No FITS card identifies a frame as part of a mosaic: there is no `MOSAIC`, `PANEL` or `TILE` card,
and no plate-solve result in a light frame. The name is the signal. The planner takes pane counts
along X and Y and an overlap percentage (10% by default) and names each pane **`<base>_<row>-<col>`**
— the first index is the row (Y), the second the column (X), both 1-indexed. The separator before
the panel token is a literal underscore, and the base may contain spaces (`NGC 7000_1-1`). The
pointing centres agree: adjacent panes are separated by the field of view along that axis times
(1 − overlap), computable from `FOCALLEN`, `XPIXSZ` and `NAXIS1/2`, and a row shot after a meridian
flip carries a `ROTATOR` 180° from the first.

`panel.ParsePanel` (`internal/panel`, a leaf package so the store migration can use the same
parser) matches the end-anchored `_([0-9]+)-([0-9]+)$` and requires row and column in **1–20**.
It is a producer-format parse backed by a plausibility bound, not a pure contract:

- the underscore requirement keeps catalogue designations as normally written (`Sh2-101`,
  `M 1-92`, `K 1-16`, `Abell 2-1`, `PK 164+31.1`) from matching, and a mosaic *of* such an object
  still parses (`Sh2-101_1-1` → base `Sh2-101`);
- the bound rejects date suffixes (`M42_2026-07`, `NGC7000_2024-11`, `M42_12-25`), `Sadr_0-0` and
  any integer overflow. Without it such a frame would start routing through the mosaic template,
  and re-file's default scope would then offer to move archived data on a false positive;
- only the final suffix counts: `Sadr_1-2_3-4` yields base `Sadr_1-2`;
- `base` is a pure prefix slice of the input and `panel` the verbatim suffix, so
  `base + "_" + panel` reconstructs the input exactly.

**A residual false-positive class is accepted, not solved.** A designation spelled with an
underscore for its space and carrying small numbers — `Abell_2-1`, `K_1-16` — parses as a pane, and
underscore-for-space is a real convention in exported target lists. Such a frame files to
`Abell/<OTA>/<SESSION>/Abell_2-1/NIGHT_1` instead of `Abell_2-1/…`: a wrong folder shape, not lost
or corrupted data, and re-file's preview shows every affected frame before anything moves. Pointing
geometry cannot resolve it, since a single non-mosaic frame has no siblings to compare with. **This
class is the main reason detection is opt-in**: an operator who never shoots mosaics never runs the
parse and never carries the risk, and one who opts in has accepted a documented heuristic, stated in
the Settings copy, and can rename the target or turn the setting off. That is a better trade than a
maintained exception list for a case with no known instance.

The planner's list also carries the un-suffixed centre entry beside the panes (`NGC 7000` beside
`NGC 7000_1-1`), so a base name can be both a mosaic and a real target with its own frames. Those
frames route through the light template as usual, landing as siblings of the panel directories
under the same session folder. Nothing collides, and no sentinel folder is invented to keep the
tree uniform.

**Detection rules**, all in `route.mosaicPanel`, the single check both `Route` and the exported
predicate ask:

| Rule | Why |
|---|---|
| Requires `mosaic_detection`, checked first | Off is the default, and then no frame reaches the mosaic template. |
| Requires `LIGHT` | A calibration frame can carry a stale `OBJECT`, and the mosaic template routes under the light root, so a match would file it in the wrong tree. |
| Parsed on the **transformed** target (§1.4) | Template selection and the token equivalence (§7.2) both depend on the strip and trim having been applied: with space-stripping on, `"Sadr_1 -1"` is a panel only after the strip. |
| A degenerate base quarantines | `OBJECT = "_1-1"` leaves `<TARGET_BASE>` empty; a frame whose base is unknowable has no correct home, and quarantine is loud and recoverable where filing it flat is not. |
| No zero-padding | `<PANEL>` reproduces the suffix verbatim, so the folder name matches the frame's own `OBJECT`; a grid wider than nine sorts `_1-10` before `_1-2` in a file browser, the lesser cost. |
| Long base names can truncate | `sanitize.Component` caps a component at 200 runes and the shipped leaf packs base and panel into one component, so a base within a few characters of the cap could lose its panel token, merging two panes into one directory — a silent multi-target night folder, which breaks night recovery as §7.3 describes. Reachable only near 200-character targets; recorded and accepted, not guarded. |

The parse lives in `route`, and `classify` is not touched. Moving it into classification would
make it ambiguous whether `Classification.Target` is rewritten, and both readings break something:
rewriting it would make retarget's faithfulness check compare a stripped classification against
an unstripped label and would re-key night lookups against rows written from the raw target; not
rewriting it would leave the persisted panel columns disagreeing with routing. It would also
force the panel fields onto every hand-built classification, including the two preview paths,
where missing them would make the feature silently do nothing.

`route.IsMosaicPanel(target, type, opts)` exports the predicate. It takes the raw target and
applies the transforms itself, so it is callable where only a stored row exists. Its three
consumers are re-file's default scope (§8.3), the Settings preview's LIGHT and MOSAIC rows (§7.5)
and the retarget preview's `NightApprox` template choice (§6.5). No routing or predicate decision
calls `panel.ParsePanel` directly.

### 7.2 Tokens and the mosaic template

`<TARGET>` keeps its meaning everywhere — the full transformed `OBJECT` (`Sadr_1-1`). It is never
redefined per template. Four tokens are valid only in the mosaic template (§3.3): `<TARGET_BASE>`
(`Sadr`), `<PANEL>` (`1-1`), `<PANEL_ROW>` and `<PANEL_COL>` (`1`). The operator picks a layout by
writing the template:

```
<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>   (default)
                                        ->  Sadr/FRA300/Summer2026/Sadr_1-1/NIGHT_1
<TARGET_BASE>/<OTA>/<SESSION>/<TARGET>/NIGHT_<n>
                                        ->  Sadr/FRA300/Summer2026/Sadr_1-1/NIGHT_1
<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>/NIGHT_<n>
                                        ->  Sadr/FRA300/Summer2026/1-1/NIGHT_1
```

The first two render identically **by construction**: the panel suffix is split off the
already-transformed target, so `<TARGET_BASE>` + `_` + `<PANEL>` reassembles exactly the string
`<TARGET>` produces, and the segment is sanitised as a whole either way. Splitting the raw value and
transforming the pieces separately would diverge on `"NGC 7000 _1-1"`, because sanitising trims only
trailing spaces. The explicit form is the default because it makes every segment of the mosaic
template visibly mosaic-aware. `<PANEL_ROW>` and `<PANEL_COL>` validate but are not offered as
Settings chips: they re-render parsed integers, so a zero-padded producer name `Sadr_01-02` would
reassemble as `1-2`, while `<PANEL>` reproduces the producer's spelling.

`<TARGET_BASE>` is restricted to the mosaic template because in the light template it would be
indistinguishable from `<TARGET>` (panes never route through the light template), and because
allowing it there would let the flat multi-target shape of §7.3 be built through a different door.
**A mosaic-only token anywhere but the active mosaic template quarantines the frame.** Validation
refuses it at save, but a hand-edited config reaches routing unchecked, and an unrecognised token
would otherwise pass through as literal text into a folder named `_PANEL_`. The check is "the
mosaic template is the one being rendered", which is false whenever detection is off, so it holds
regardless of the toggle. This does not contradict INV-M4's "unchanged with detection off": that
guarantee covers every config naming no mosaic-only token, and a config naming one outside the
mosaic template has no correct rendering to preserve — quarantining it is the safe reading.

Because the mosaic template is selected only when a panel parsed, its panel tokens are always
resolvable; no optional-token collapse is needed, and the `<FILTER>` drop rule is unchanged.

### 7.3 The mosaic template must distinguish panels

A mosaic template that renders the same directory for every pane would put ten distinct `OBJECT`
values in one `NIGHT_n` folder. That breaks night recovery: the rebuild reads one header per folder
(§6.3), so nine panes' counters are lost at the next startup, `AssignNight` re-mints ordinal 1, and
frames land beside a different pane's. So it is refused at save: the **union of tokens across every
segment before the first segment containing `<n>`** must include `<TARGET>`, or `<PANEL>`, or both
`<PANEL_ROW>` and `<PANEL_COL>`.

The union, so `<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>` is accepted; the
first `<n>` segment, so a template naming `<n>` twice is unambiguous. The positional rule is not
pedantry: a containment test would accept `<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>/<PANEL>`, which is
worse than the flat layout — the rebuild stops descending at `NIGHT_<n>` and reads only files
directly inside it, so such a folder yields no entry at all and every ordinal in the tree is lost.
The rule is evaluated through `route.SplitAtNight`, the same split the router uses (§6.4).

Making this a validation rule rather than a runtime accommodation keeps the rest of the design
small. A rebuild that detected templates able to produce multi-target folders and read every header
in that case cannot work: it would compute its predicate over the current templates while the
folders it walks were produced by historical ones, so changing a template or turning detection off
would strand the folders on disk. Forbidding the input removes the problem instead of tracking it.
The rule applies only while detection is on (§7.5).

### 7.4 Data model

Each frame row carries `panel_base`, `panel_row` and `panel_col`, parsed by `panel.ParsePanel` from
the **raw** stored `target` and populated unconditionally, whatever `mosaic_detection` says. The
filing path writes them, so a retarget, which re-files through it, keeps them consistent with the
target it writes. The store migration that adds the columns backfills them with a Go row loop over
the same parser, not an approximate SQL parser that could diverge from it.

- **Raw, not transformed.** Routing parses the transformed target, but a migration has no access to
  the configuration. If the live path used the transformed value and the backfill the raw one, a base
  containing a space — the producer format, `NGC 7000_1-1` under the shipped space-stripping — would
  be `NGC7000` on live rows and `NGC 7000` on backfilled ones, splitting one mosaic into two groups.
  Parsing the raw target everywhere keeps the columns consistent with the column they derive from.
  The single residual is a hand-typed `"Sadr_1 -1"`, a panel to routing but not to the columns; the
  ASIAIR never produces such a name.
- **Unconditional.** The columns are derived data that nothing reads for routing. Gating them on the
  toggle would make their meaning depend on when a frame was filed.

No read path consumes the columns, and the frame DTO on the wire does not carry them; they exist so
a mosaic completeness view (per-pane frame counts) is a read-side feature rather than a re-parse of
the library. The upsert the filing path ends in writes the pane columns from its inputs on every
write, so they follow `target` wherever it goes — including a retarget that renames a frame into or
out of a pane name, which must not leave the old coordinates behind. A target that does not parse
binds `""`/0, stored as `NULL`, the "no pane suffix" encoding. The migration that adds the columns
also creates the re-file intent table (§8.6) and the `dest_path` index that re-file and the night
folder read range over.

The general-purpose row update (`UpdateFrame`) deliberately does not write the pane columns, as it
does not write the reconciliation or overlay columns: its callers pass a copy of a row read earlier,
so a column listed there is one a stale copy could clobber. It **does** write `dest_path`. Its two
callers, holding a frame and marking it deleted, are therefore the one place a stale in-memory row
could revert a re-filed `dest_path`, leaving the row naming a vacated path: the frame is marked
missing and, one settle window later, its device original is released against an archive nothing
references — the INV-M2 failure, reached from outside re-file. Both callers run on the rig's worker
goroutine, the same one that runs re-file (§8.8), and must work from a row read after any re-file
that could have moved it, never from a copy taken before one.

### 7.5 Configuration and the Settings toggle

Two keys: **`mosaic_detection`** (boolean, shipped `false`) and **`templates.mosaic`** (shipped
`<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>`). With detection off every routing
outcome is identical to routing without the feature for any config that names no mosaic-only token
(INV-M4), so turning the feature on is the operator's choice of layout.

A blank or missing `templates.mosaic` is filled with the default during normalisation (§3.5), so
validation never sees `""` and no configuration schema migration exists for it. Writing the default
through a migration would be wrong in the other direction: it names `<OTA>` and `<SESSION>`, so an
install whose rig leaves those blank would fail validation from the moment it migrated. The way to
opt out is the toggle, not a blank template, and not a copy of the light template, which the
panel-distinguishing rule refuses for any light template lacking `<TARGET>`.

**While detection is off, the mosaic template's own validation is suppressed** — tokens, `<n>`,
emptiness, panel distinction and the `<OTA>`/`<SESSION>` cross-check together. Otherwise a field the
operator cannot see (a hand-edited value, or one typed before the toggle was turned off) could
reject every config save. All of it applies in full the moment detection is turned on, since that
is itself a save. While on, a blank OTA with an `<OTA>`-bearing mosaic template is an error naming
the mosaic template, because every pane would otherwise quarantine at night.

The Settings page shows the template field only behind a toggle labelled "Detect and sort mosaic
panels", whose copy says panels are recognised by the ASIAIR's `<target>_<row>-<col>` naming and that
a target sharing that shape will be treated as a panel. Turning it off moves nothing — panes already
filed under the mosaic layout stay where they are and future panes file through the light template —
and fixed copy says so, without computing anything. The live Settings preview shows a `MOSAIC`
sample row while detection is on, using a real panel frame when the library has one and a synthetic
`Sadr_1-1` header otherwise; the LIGHT row then samples only non-panel frames, or after a mosaic night
it would preview the mosaic template under the LIGHT label. The preview is not a frame type: it reuses
the light sample machinery with the panel predicate inverted, because a pseudo-type in
`model.FrameType` would have no sensible answer in validation, routing or type canonicalisation.

**A client that omits the key must not undo the opt-in.** A config `PUT` that omits `mosaic_detection`
would decode it as `false` and silently start filing panes flat. So the handler decodes into a
shadow of the config whose `mosaic_detection` and whole `templates` object are pointers, and takes
the stored value for every field the payload omits — `templates.light`, `templates.cal` and
`templates.mosaic` included, since normalisation would otherwise fill the default over a customised
mosaic template. First-run setup takes a caller-supplied config the same way and decodes it through
the same shadow, so a setup payload omitting `mosaic_detection` cannot turn detection off either;
normalising its templates alone would not cover the toggle. A payload carrying unknown keys is
rejected outright, so the reverse skew is loud. A binary that does not know the two keys still loads
such a config, since YAML decoding ignores unknown keys; what refuses a downgrade is the store
schema, not the config.

## 8. Re-filing archived frames

Re-file is a general operation, not a mosaic one: for every eligible frame whose re-routed directory
differs from its stored `dest_path`, it moves the archived file to where the **current** templates
say it belongs and updates the row. It repairs any template change, and it is the supported
alternative to moving folders by hand, which reconciliation reads as a cull and which can authorise
deleting a device original against an archive nothing references
([library-reconciliation §12](2026-10-17-library-reconciliation.md)). Editing a template is harmless
on its own: saving moves nothing, new frames land in the new layout, and old rows keep valid paths.

### 8.1 It is a move, not a filing

**Re-file never calls the filing path** (`fileFrom`, `recordFiled`, `UpsertFrame`). That path files a
frame arriving from the device, and routing an archived file through it drags in behaviour a move
must not have:

| Filing behaviour | What it would do to a re-filed frame |
|---|---|
| The upsert writes the overlay and archive anchors from its inputs | Resets the retarget overlay and archive anchor; the archive-side check then fails against the edited file, the frame is held with a critical `delete_mismatch`, and its device original is never released. |
| Re-classify sets `state = verified`, clears `deleted_at` | Resurrects a `deleted` frame: it re-enters deletion eligibility against a device original that no longer exists, inflates the resident and power-off counts, and escapes retention. |
| The upsert clears `missing_seen_at`, `missing_acked_at`, `restore_requested_at` | Silently erases reconciliation state. |
| A non-reclassify filing charges transfer bytes | Charges a local rename to the transfer budget. |
| The idempotent-keep branch removes the source | Deletes the operator's archived file when a byte-identical file already sits at the destination. |
| A canonical-name content clash raises `rot_clash` | Reports possible corruption for what, during a migration, is ordinary disambiguation. |

So re-file is its own narrow primitive, `rig.refileOne`, applied per **file**:

1. Resolve the destination directory from the current templates (§8.2).
2. Choose the destination filename itself with the filing ladder's `candidateName` — the archived
   basename, sanitised (falling back to `frame_<short hash><ext>`), then the `DATE-OBS` token (the
   stored date reformatted as `20060102-150405`, empty when unparseable), then lengthening prefixes
   of the full 64-hex archive hash — so the exact destination path is known before the rename. A
   frame whose archived name already carries a disambiguation token keeps it, as on every
   archive-side re-filing path.
3. Write the intent record (§8.6). **If that write fails, abort before renaming**: the crash-safety
   guarantee is void without it.
4. `MkdirAll`, then `RenameNoReplace`. On `EEXIST`, compare before advancing: a case-only rename on
   a case-insensitive volume names the same file (detected by comparing the paths
   case-insensitively, since the seam's `Stat` cannot answer "same file") and proceeds; otherwise
   the occupant's size is compared to `archive_size` and, only on a match, a cache-bypassing read
   is compared to the archive hash. **Equal means the file is already where it belongs**: commit
   the path and leave the copy at the old path untouched, reported. Different content advances to
   the next candidate, rewriting the intent first; an exhausted ladder fails the frame and leaves
   the file where it was.
5. `store.MoveDestPath`: in one transaction, set `dest_path` on every row of this rig sharing the
   old path. **No other column is written.**
6. Clear the intent (a failed clear is benign; recovery drops it) and, after the group, prune
   emptied directories (§8.9).

The comparison in step 4 is not optional. Advancing blindly would duplicate a frame an interrupted
run had already moved, and on a case-insensitive volume a template edit changing only case would
permanently append a date token to a frame that was already correct.

### 8.2 Selection is difference-driven

Nobody hand-picks five hundred frames to migrate. Re-file computes, for every eligible frame,
whether the directory the current templates produce differs from the stored one, and presents the
result **grouped `old dir → new dir` with counts**; the operator selects groups. A group is
identified by the pair `(FromDir, ToDir)`, both absolute like `dest_path`, because two source
directories can collapse onto one destination. `Count` counts distinct `dest_path`s, not rows, so a
frame held on two shares (two rows, one file) counts once.

**Routing from stored columns.** The preview routes each frame from its stored row
(`rig.refileRouteInputs`), so previewing a library is a database scan. Apply rebuilds its routing
inputs through the same helper, which is what makes preview and apply agree by construction. Two
details are load-bearing:

- **The stored `night` is passed through.** A zero would render `NIGHT_0` for every light and INV-M10
  would refuse them all; `AssignNight` is never called, since minting would strand the frame in a
  `NIGHT_n` its siblings do not share.
- **Every stored scalar except `SET-TEMP` (and the parsed binning and date) is treated as present.**
  Binning and `DATE-OBS` count as present only when the stored value parses. Exposure, gain and
  offset are stored raw, so a legitimate zero is indistinguishable from an absent card. A `!= 0`
  test would report a cooled camera's legitimate gain 0 as unroutable across a whole library.
  `SET-TEMP` alone records its absence on the row, because an absent set-point routes to `uncooled`
  rather than quarantining, and assuming presence would move every uncooled frame into a `0C`
  folder. The residual: a frame that genuinely lacks `GAIN` re-files into a `gain0` folder where
  live filing would have quarantined it, reachable only when a template acquired `<GAIN>` after the
  frame was filed. Nothing is lost; the row still points at the file.
- The target is the **raw** stored `target`, matching the filing path, because template selection
  depends on it: a preview that transformed it differently from apply would disagree about whether
  a frame is in scope at all. The retarget preview makes the opposite choice for the same reason
  (§9.6).

**Eligibility.** Eligible states are `verified` and `deleted`: both mean an archived copy exists.
`store.RefileCandidates` returns only those, with a non-empty `dest_path` and no missing mark or
standing restore request. Every other row is filtered out rather than reported — a refusal line per
`unclassified`, `dismissed` or `failed` frame would run to tens of thousands and say nothing
surprising:

| Filtered | Why |
|---|---|
| empty or NULL `dest_path` | No archived file. Not implied by the state filter: an authorised culled-original drain leaves `deleted` with a NULL path, and a failed re-pull stores a literal `''`; without the predicate a single move keyed on an empty path would rewrite every such row of the rig. |
| `missing_seen_at` set | The file is not where the row says. |
| `restore_requested_at` set | Usually implied by a missing mark, but a truncated-copy replace sets it on a present frame; a crash there leaves a known-short archive with no missing mark. |
| `unclassified` | Re-classify's job (§2.3). |
| `dismissed`, `failed` | No live `dest_path`. |
| `held` | Held because an integrity check failed; moving a file whose correctness is in question is not this tool's business, so it stays until the hold is resolved. |

**Eligibility is decided per `dest_path`, not per row.** A hold applies to a single row, and a frame
present on both a USB and an eMMC share (H12) is two rows on one path, so one share's row can be
ineligible while its sibling is `verified`; moving on behalf of the eligible row would drag the
other along. If any row sharing the path is ineligible, the path is refused as a whole and
reported. A `Refusal` is reserved for a path that would have moved and did not: an ineligible
sharer, a cross-rig sharer (§8.7), or a frame the current templates would quarantine, which is
reported and never moved to `unclassified/`. Apply re-reads every row and applies the same refusals
to any id that reaches it.

**A library already moved by hand is not repaired.** Those frames carry missing marks and are
filtered out of the preview; apply refuses one that reaches it by id (AC 28). Relinking a row to a
file found by content is a different safety model (§12); re-file is the alternative to moving by
hand, not a cure for having done it.

### 8.3 Scope

Routing resolves `<OTA>`, `<SESSION>` and `<RIG>` from the **live** rig profile, so after an OTA or
session rename, or a `date_format` change, every frame's routed directory differs, and a raw diff
becomes a one-click reorganisation of the whole library with the largest group on top. The engine
stays general; the **default scope is `mosaic`**: frames the current templates route through the
mosaic template (`route.IsMosaicPanel`). Nothing records which template filed a frame, so "changed"
is not computable and "routes through the mosaic template under the current configuration" is the
closest honest predicate. The unrestricted `all` scope sits behind an explicit opt-in.

The mosaic scope requires `mosaic_detection` on: with detection off no frame's template selection
can have changed, so the scope is empty by construction, and the Tools page shows it disabled with a
link to the Settings toggle rather than an empty diff. The unrestricted scope is available either
way. Undoing a mosaic migration needs no reverse mode: turn detection on, edit the mosaic template
to match the light layout, and re-file.

### 8.4 Reach is bounded by row retention

Re-file is driven by rows, and rows do not live forever. Retention prunes `deleted` rows seven days
after `deleted_at`, except rows still carrying a missing mark
([architecture §7.10](2026-10-17-architecture.md)). On a delete-enabled rig an archived frame whose
device original was deleted loses its row after a week and its file becomes unreferenced, so re-file
reaches roughly the last week plus everything still resident. A `keep_originals` rig never prunes,
so its whole history stays reachable; two operators running the same migration can legitimately see
very different counts. This is accepted: a filesystem-driven mode adopting rowless files would have
no recorded hash to verify against and no row to commit — a different safety model. An in-progress
mosaic is recent by definition.

### 8.5 Verification before moving

**No content check, and no size or mtime precondition.** Re-file confirms the source exists and
renames it. `frames.mtime` is the **device** file's mtime; nothing stamps the NAS copy, so a
size-plus-mtime precondition is unimplementable, and size alone discriminates nothing among
same-camera lights. The precondition is also unnecessary: re-file writes only `dest_path`, re-derives
and re-compares no hash, and alters no byte, so it neither creates nor conceals drift — the
archive-side check that would catch drift runs at the same point, against the same recorded hash,
before any device original is released. Verifying here would be threat-modelling inherited from
retargeting, which does rewrite bytes. Hashing would read every migrated frame over SMB — tens of
gigabytes for one mosaic — to detect a condition the move cannot cause.

**`EXDEV` refuses rather than falling back to a copy.** Configuration validation already requires the
staging, light and calibration roots to resolve to one volume, so a cross-device rename means a
light root that internally spans mountpoints; re-file reports that frame with its own reason instead
of carrying an untested copy-verify-delete branch. Consequently **no path in re-file ever rewrites
bytes**, which is what makes moving without a content check sufficient (INV-M3).

### 8.6 Crash safety

Retargeting survives a crash between rename and commit by construction (§9.3): it renames an edited
temp to a new path, commits, and only then removes the old file. Re-file moves the original itself,
which inverts that property: a crash after the rename and before the commit leaves the row naming a
path that no longer exists — the state that marks a frame missing and, one settle window later,
authorises deleting its device original against an archive nothing references.

**An intent record** closes that window:

```sql
CREATE TABLE refile_intents (
  rig_id     TEXT NOT NULL,
  from_path  TEXT NOT NULL,
  to_path    TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (rig_id, from_path)
);
```

It is keyed on the `dest_path` being vacated rather than a frame id, because one move covers every
row sharing the path. Writing it upserts on the key, since an `EEXIST` retry rewrites `to_path`, so
the intent always names the exact path the rename is about to attempt. `created_at` is diagnostic:
intents never expire, because an intent that outlives its move is exactly what recovery exists to
resolve.

**Startup recovery** (`rig.RecoverRefileIntents`) runs before the night-counter rebuild and before
any worker starts, and checks `from` **first**:

| Observed | Meaning | Action |
|---|---|---|
| File still at `from` | The rename never happened | Drop the intent |
| Nothing at `from`; no row names `from`; a row names `to` | The commit landed; only the intent clear was lost | Drop the intent |
| Nothing at `from`; no row names either path | The row was pruned or dismissed while the intent was open | Drop the intent and log it |
| Nothing at `from`; rows name `from`; file at `to` | The rename landed and the commit was lost | Finish the commit for **every** row sharing `from`, then drop the intent |
| Nothing at `from` or `to`; rows name `from` | The file is genuinely gone | Drop the intent; normal missing-frame handling applies |

The order is not cosmetic. Checking `to` first would let recovery bind the row to a sibling's file:
in a flat panel layout the ASIAIR's repeating filenames collide by design and same-camera lights are
the same size, so neither name nor size distinguishes them. Establishing that `from` is empty is what
makes the `to` match meaningful, and why no size or content heuristic is needed. A per-intent
problem (an unreadable path, a commit that fails again) leaves that intent open and recovery
continues; only a failure to read the intent table fails startup.

The row and intent states in the table are the only ones recovery has to handle. A `frame_lost`
alert from the device-side differ is not a second symptom of a crashed move: the scan phase runs
before the device probe in the same cycle, so the missing mark is set first, and the differ returns
early for a frame whose missing mark is set.

`MoveDestPath` tolerates a zero-row update **only during recovery**, which may re-run a commit that
already happened. On the live path zero rows means the row was pruned between the re-read and the
commit (retention deletes one of re-file's two eligible states hourly): the frame is reported as
failed, the intent cleared, and the file is **not** renamed back, since restoring it would re-create
something nothing references.

**A commit that fails in a living process is handled there**, not left for a startup that may be
days away: re-file renames the file back and drops the intent. If the rename back also fails, the
intent **survives** and a critical alert in the `refile` category is raised. It is a category of
its own because the category set is closed and doubles as a bounded metric label, so reusing an
unrelated category would mislabel both the UI and the metric. An alert alone is
visibility, not prevention, so the guarantee is structural: **an open intent is a deferral reason
(`refile_in_flight`) in the delete pass's absent-frame check**, so a frame whose `dest_path` matches
an open intent is never eligible for device-original deletion, however the move failed — crash,
commit failure or failed rename-back. That is what makes INV-M2 true by construction. The frame can
still be marked missing in that state; what the deferral guarantees is that it is never
deletion-eligible.

| Rejected | Why |
|---|---|
| Copy, commit, delete | Inherits retargeting's crash property, but turns a minutes-long migration into hours (a full copy of every frame) and needs admission for the transient space. |
| A routed-path probe before marking a frame missing — compute where the templates would put it and relink on a match | Suppressing the missing mark could disable the culled-original drain, which keys on unclassified frames carrying one; it could relink a row to a disambiguated duplicate by name, after which the row's hash would authorise deleting a device original against the wrong archive; and it adds a directory read per absent frame on exactly the scan that runs when the library is unhealthy. The intent record is the guarantee. |
| Record the intent's destination as a directory and regenerate the ladder at recovery | Recovery would then have to pick the right file among same-named, same-sized siblings — the failure that disqualified the probe. |

### 8.7 Rows that share a dest_path move together

A frame present on both the USB and eMMC shares (H12) is **two rows with one `dest_path`**, and
several consumers depend on that identity: the "is another row using this NAS file?" guard dismiss
consults before deleting a file, tonight's frame and byte counts (which count distinct `dest_path`),
the resident count behind the drain label, and the cadence predictor. So re-file **moves the file
once and updates every row sharing that `dest_path` in one transaction** — a second reason it cannot
reuse the filing path, which commits one row at a time.

The update is **rig-scoped**: one goroutine owns a rig's frame mutations
([architecture §3.6](2026-10-17-architecture.md)), so rig A's worker must not rewrite rig B's rows. A
`dest_path` also referenced by another rig is detected with an unscoped count
(`store.CountFramesByDestAnyRig`), refused and reported. `MoveDestPath` refuses an empty old path
outright, a second line of defence behind the eligibility filter (INV-M6).

### 8.8 Execution

**One worker request carries one selected group.** Re-file is serviced in the worker's `Run` select
(through its own request channel), never in the command drain the delete pass runs between frames:
the drain does not pull, run the delete pass or publish status, so resuming pulls mid-group would need
resume state this operation does not have. A per-frame request would wait up to a poll cycle each, so
a large migration would stretch across a night of imaging. A group holds the worker while it runs, but
each frame costs a `Stat`, a rename and a small transaction, so even hundreds of frames take seconds.
Re-file takes no admission slot, because a rename costs no transient space and a slot would contend
with live pulls on a single-slot rig.

**Preview runs on the HTTP goroutine**, not the worker: it is read-only, and a whole-library diff in a
select with no default would block pulls and the poll ticker for its duration.

**Each row is re-read immediately before its move**, so a frame that stopped being eligible between
preview and apply is skipped and reported rather than moved from a stale snapshot; every frame is also
re-routed, and one whose directory is not the group's `ToDir` is refused (INV-M10).

**Cancellation stops a group between frames, never inside one.** The supervisor derives the group's
context from the HTTP request, so a cancelled request reaches the worker, with a deadline of 10 s per
frame, floored at 5 minutes and capped at 30 (generous: a move is milliseconds, so a timeout means
something is badly wrong). The preview gets 2 minutes, since it scans the library and lists each
vacated directory over a network mount. The commit runs under `context.WithoutCancel`: a cancel
arriving mid-frame would otherwise fail that frame's commit and drive a routine button press through
the commit-failure path. There is **no resume state**: re-running the diff is idempotent, so an
interrupted migration is finished by running it again.

**Night ordinals are untouched**: the stored `night` renders `<n>` and `AssignNight` is never called
(INV-M9). Re-file changes only `dest_path`, leaving `sha256` and the
`(rig_id, source_share, source_path)` key intact, so a later device re-pull fires no differ or
re-adopt branch; that is tested rather than assumed (AC 33).

Each file's outcome is counted in `asiair_commander_refile_frames_total{result}`, `result` one of
`moved`, `skipped`, `failed`; a move never touches the filed-frames counter
([docs/metrics.md](../metrics.md)). Each move and each adoption of an identical copy is logged at Info;
a commit failure, a vanished row and an unremovable intent are logged as warnings or errors with both
paths.

### 8.9 Emptied directories are pruned

Without pruning, a mosaic migration would leave every vacated panel folder behind, empty, in exactly
the listing the operator wanted cleaned up. After a group, `rig.pruneEmptyDirs` removes each vacated
directory and walks upward removing empty parents, stopping at the **longest-matching** configured
root, ranked on the cleaned root path. A first-match rule would let the walk pass a calibration root
nested inside the light root and remove the configured root itself, and an unrestricted re-file moves
calibration frames too. A directory outside every configured root is left alone and reported.

- `os.Remove` refuses a non-empty directory, so the failure mode is inert; `MkdirAll` runs before
  every rename, so a concurrent filing race costs a failed rename, not a lost frame.
- A directory holding the library-link marker is never removed
  ([library-reconciliation §2.4](2026-10-17-library-reconciliation.md)).
- **Unmanaged files are never touched.** An operator's masters folder or stacking output keeps a
  directory from emptying. The **preview** names each directory it expects to strand and why — one
  listing per vacated directory, subtracting the files the migration takes out — and the final
  summary reports every vacated directory that could not be removed, with the reason. An ancestor
  that merely still holds the migration's other folders is where the walk was always going to stop
  and is not reported.
- **On a macOS-mounted NAS the Finder's `.DS_Store` strands most vacated folders.** The preview warns
  about it up front rather than letting it surface as a pile of skip reports.

### 8.10 What a move does not disturb

- **Sidecars are device-side.** A sidecar path is share-relative on the device and is consumed only by
  the device delete pass, so a NAS move needs no sidecar handling.
- **Previews survive.** The preview cache is content-addressed by hash; `dest_path` is only the decode
  source on a miss.
- **Astrometry survives.** `wcs_status` and `astrometry` are columns on the row; the solver writes
  nothing beside the frame. A solve or backfill racing a move stats `dest_path`, treats a miss as
  transient, and degrades without burning an attempt.

Because re-file bypasses the filing path, it deliberately does not emit the `frame.done` event (the
Tools page reloads after apply), does not count a filing, does not enqueue a solve (solve status lives
in the row), does not warm the preview cache (still valid), and does not attempt a library auto-link
(the frame was filed under a linked root already).

### 8.11 API and the Tools page

| Endpoint | Behaviour |
|---|---|
| `POST /api/rigs/{id}/refile/preview` | Body names the scope (`mosaic` or `all`). Returns a `RefilePreview`: every group, every refusal, and the scope — no truncation, since the preview is a database scan. |
| `POST /api/rigs/{id}/refile/apply` | Body names one group's `ToDir` and frame ids. Streams NDJSON: one `RefileResult` per **file** (an H12 frame's two rows produce one line), then a final `RefileSummary`. Honours the request context. |

Both are write-guarded (Origin and CSRF checks, [architecture §8.3](2026-10-17-architecture.md)),
including the read-only preview, since it is a `POST`. The wire shapes:

- `StrandRisk{Dir, Reason}` — a directory the prune expects to be unable to remove, and why.
- `RefileGroup{FromDir, ToDir, Count, FrameIDs, StrandRisk}` — `FrameIDs` carries every row, because
  every row's `dest_path` is rewritten.
- `Refusal{DestPath, Reason}` — a path excluded from every group. Refusals are not group members, so
  they can never surface as results.
- `RefilePreview{Groups, Refusals, Scope}`.
- `RefileResult{FrameID, Status, From, To, Reason}` — `Status` is `moved`, `skipped` or `failed`.
- `RefileSummary{PrunedDirs, SkippedDirs}` — prune outcomes are directories, not frames, so they
  arrive as the final line.

The supervisor's `PreviewRefile` and `RefileGroup` take a context, unlike the per-frame retarget
methods, so cancellation reaches the worker. The endpoints are per rig; the Tools page fans the
preview out across rigs (which is also what makes the cross-rig refusal meaningful), applies selected
groups sequentially with per-group progress and a cancel button, shows the scope toggle and the
strand warnings, says which retention regime applies, and reloads after apply.

## 9. Retargeting

An ASIAIR Autorun left on the wrong target name stamps every following frame with the wrong
`OBJECT`, so Commander classifies, files and records them under that name. The frames are good data;
only the label is wrong. The retarget tool on the Tools page lets the operator select archived frames,
type the correct target, preview where each will move, and apply. Applying rewrites the archive's
`OBJECT` card, re-files the archive to the correct folder and updates the row — **without disturbing
the anchors that let the delete pass release the device original.**

### 9.1 The OBJECT card rewriter

`header.RewriteObject(src, dst, target)` streams `src` to `dst`, replacing exactly the effective
`OBJECT` value card and copying every other byte — every other card and the whole data unit —
verbatim, so byte offsets and the 2880-byte block padding are preserved. It never decodes the data
unit; the cost is one streamed copy.

- It locates the single card whose normalised key is `OBJECT` and which is a **value** card (columns
  9–10 are `= `), scanning the header blocks before `END`.
- The replacement is the canonical form `OBJECT  = '<target>'`, the target sanitised (§4.1), quoted
  with doubled-quote escaping, blank-padded to the FITS fixed-format minimum of eight characters (the
  closing quote at column 20 or later) and the card space-padded to 80 columns. Any comment on the
  original card is dropped, keeping the rewrite deterministic.
- In the same pass it computes the **complement digests**: a SHA-256 over every byte outside the
  80-byte card window of the input, and the same over the output. Equal digests byte-prove that the
  data unit and every other card survived, at no extra I/O. It also computes `OutputSHA256` over
  every byte written, which the caller compares to the at-rest read-back.
- `dst` is fsynced before returning, so the durable file precedes any rename.
- It refuses when there is no `OBJECT` value card, when there is more than one (pathological for
  ASIAIR frames; refuse rather than guess), when the target sanitises empty, or when the sanitised
  value exceeds 60 characters — a conservative ceiling under the 70-column value field.

The worker calls the writer through an injectable `rewriter` field, defaulting to
`header.RewriteObject`, so a test can substitute a deliberately corrupting writer and prove the
complement check refuses it; the real writer is deterministic and cannot be made to fail otherwise.

Adding an `OBJECT` card to a frame that has none is out of scope: it can overflow the 2880-byte header
block and shift every byte.

### 9.2 The archive anchor and the overlay

A frame row carries two anchors, keyed on the copy each describes
([frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md)):

- **`sha256`** is the **device anchor**: what the device copy hashes to. Every comparison against
  bytes read from the device uses it, and deleting a device original requires a fresh device read to
  equal it. A retarget never moves it, because a retarget never touches the device. It advances only
  when the device itself revises a frame (H13, [frame-lifecycle §11](2026-10-17-frame-lifecycle.md)).
- **`archive_sha256`** and **`archive_size`** are the **archive anchor**: what the NAS copy at
  `dest_path` should hash to, and its length. Every comparison against bytes read from the NAS uses
  them. `rig.archiveHash(f)` is a plain read of `archive_sha256`, not a derivation: a fallback from
  an overlay to `sha256` would rest on the device copy hashing to `sha256` for the frame's whole life,
  which a device-side revision falsifies.

The **retarget overlay** is the audit record of a re-label:

- **`retarget_sha256`** — the hash the re-label produced. Non-empty means "this frame was
  retargeted", and it is what the re-apply rule (§9.4) keys on.
- **`retarget_from`** — the original target, kept across repeated retargets for display and audit.
  `target` holds the corrected label.

A retarget does not change the frame's state: it stays `verified`, or `deleted` for an archive whose
device original is already gone. A re-label is not a lifecycle change, in the same way that missing
and acknowledged are orthogonal columns rather than states.

The overlay columns are written **only from explicit inputs** to the filing path, never carried by a
sticky expression: a retarget or a re-apply passes the real values, and a genuinely new device file
passes empty and so clears them — a stale overlay would otherwise point the archive-side checks at old
content and hold the frame forever. The general row update (used to mark a frame deleted or held) does
not list the overlay or archive columns and must not: a `deleted` retargeted archive keeps its overlay
so later archive-side checks use the right hash.

Faithfulness — the edit changed only the label — is proven **once, at edit time** (§9.3 step 5).
After that the archive anchor catches later NAS-side drift exactly as it does for any frame.

### 9.3 The retarget operation

`rig.retargetFrame` runs on the rig's worker goroutine (§9.6), one frame per request:

1. **Precondition guards.** Skip unless the state is `verified` or `deleted` and `dest_path` is set.
   Sanitise the requested target (skip if empty), fold it onto the canonical spelling (§6.6), and
   skip if the result equals the frame's current target — a no-op, including a case-only change.
2. **Admission.** Acquire a destination admission slot on the local site, sized at `archive_size`:
   the edited temp transiently adds one copy of the archive before the old copy is removed. The work
   touches only the NAS, whichever site the device is reached through.
3. **Verify the archive before editing.** A cache-bypassing read of `dest_path` at `archive_size`
   must equal `archiveHash(f)`; on a mismatch refuse and log, never edit a drifted file (INV-3).
4. **Rewrite to a staging temp** (`asiair-retarget-*.tmp` in the staging root, on the same volume as
   the destination so the rename works), through the injectable rewriter, with the folded target.
5. **Faithfulness assertion.** The two complement digests must be equal, the temp's size must equal
   `archive_size`, and the temp's header must re-classify to the sanitised target. The complement
   equality is the load-bearing check (INV-4); the re-classification closes the one gap the complement
   cannot see — garbage written inside the card window — and guarantees the folder the frame routes to
   matches the label it carries. Any mismatch refuses before anything is committed.
6. **Hash and file the temp to a destination path.** A cache-bypassing read of the temp must equal
   the rewriter's `OutputSHA256`; otherwise a write fault between the streamed write and the durable
   file would mint the archive anchor from corrupted bytes, and later authorise deleting the device
   original against a corrupted archive. Then the temp is filed through the shared filing path,
   updating the existing row: it re-reads the edited header, classifies it, resolves the night under
   the corrected target (§6.4), routes it, and claims a disambiguated name with an exclusive rename.
   It passes `sha256` unchanged, the edited hash as the archive anchor and as `retarget_sha256`,
   `retarget_from` as the first original target, the frame's state and `deleted_at` unchanged, and
   `pulled_at` unchanged (the grace anchor is preserved). It is a local rename, so no transfer bytes
   are charged. The filed name derives from the archived basename. Every filing caller passes the
   device anchor explicitly, not only this one: omitting it at any site writes `sha256=''` and
   silently drops that frame out of deletion eligibility, which INV-1 and the delete tests guard.
7. **Remove the old file — only after the commit is proven.** The row is re-read and must carry the
   edited hash in `retarget_sha256`; an unproven commit leaves the old archive and the device original
   intact and reports failure. Only then is the old `dest_path` removed, and only if the row names a
   different path. A failed removal leaves a cosmetic orphan and is logged.

The device original is never read-modified-written, and the edited file goes to a path of its own
through the same exclusive-rename claim every pull uses; there is no rename-in-place and no replace,
which `localfs` does not offer and SMB does not do atomically. Because the operation runs on the
worker goroutine, the only interruption is a process restart:

| Crash point | On disk | Row | Delete pass and scan | Verdict |
|---|---|---|---|---|
| After the temp write, before the rename | Old archive intact; stray temp | Old path, no overlay | The scan sees the old path present; the three-way check on it passes | Healthy and un-retargeted. The stray temp's space is counted by admission's disk reserve. No loss. |
| After the rename, before the commit | Old and new both exist | Old path, no overlay | The scan sees the old path present; the three-way check on it passes | Healthy and un-retargeted; the new file is an orphan. No loss, no false delete. |
| After the commit, before the old file is removed | Old orphan and new | New path, overlay set | The scan sees the new path present; the delete pass checks the device against `sha256` and the new file against the archive anchor | Retargeted; the old file is an orphan. No loss. |

In no window is the device original deleted on a false premise, and in none is data lost. The worst
cases are an orphan file and a retarget that did not take effect, which the operator re-runs; a
`deleted` frame is retargetable, so a re-run is always possible.

### 9.4 Re-applying the edit on any re-pull

The device copy keeps the **original** `OBJECT`. So every path that re-pulls it and files against the
retargeted row — the differ branch on device-mtime jitter of identical bytes, the re-adopt of a
`deleted` row, and both restore paths — would otherwise classify under the original header, route
back to the old folder, orphan the edited archive and then release the device original under the
wrong label. Rather than patch each path, one rule applies at the two places freshly pulled device
bytes are filed against a prior row:

> **Re-apply rule.** When the prior row carries a retarget overlay and the incoming file is the same
> exposure — its data-unit digest equals the row's `data_sha256` (when that digest is usable), **or**
> its whole-file hash equals the row's `sha256` — rewrite the incoming file's `OBJECT` card to the
> row's target before classification, prove the edit (§9.3 step 5 and the at-rest check of step 6,
> anchored to the incoming file's own length), and file the edited file with the overlay preserved.
> Only when **neither** arm holds is the content genuinely new: no re-apply, and the overlay resets.

The data-digest arm is what makes the rule survive a device-side revision (H13): the ASIAIR rewriting
a frame's header in place changes its whole-file hash, and without that arm the revision would be
indistinguishable from new content and silently revert the label. The whole-file arm stays, because a
byte-identical re-pull is the same exposure by definition and a row whose `data_sha256` has not been
backfilled matches no incoming digest ([frame-lifecycle §11.3](2026-10-17-frame-lifecycle.md)). The
usability precondition stops two header-only files matching on the digest every empty data unit
shares.

`rig.reapplyRetargetIfNeeded` implements it, at the pull path's `classifyAndFile` (covering differ,
re-adopt, and a database-loss pull with no prior row, which is simply a new frame) and in the restore
path's read of the device source, which re-applies through the same helper before its own at-rest
verify so the restored file is verified against the edited hash. On a re-apply the row's `sha256` is
the hash of the bytes just pulled — the device anchor always describes the device copy in hand, so a
revised device file never leaves a pre-revision anchor behind — while the archive anchor and
`retarget_sha256` take the edited file's hash and `retarget_from` is kept.

A retargeted frame is **not** excluded from the differ re-pull the way culled frames are: ASIAIR
restarts its file counter every session, so a genuinely new frame can reuse a path at the same size,
and a size-only skip would strand it. The rule handles both: the same exposure is re-applied, and
different content files as the new frame it is.

### 9.5 Every hash comparison, by side

The correctness argument is an exhaustive classification of every hash comparison in `internal/rig`
and `internal/store`: **device side compares to `sha256`; archive side compares to the archive anchor
(`archiveHash(f)` and `archive_size`).**

| Comparison | Side | Compares against |
|---|---|---|
| Delete pass three-way check, device term | device | `sha256` |
| Delete pass three-way check, NAS term | archive | `archiveHash(f)` |
| Culled-original drain: fresh device read | device | `sha256` |
| Vanished source: present-but-corrupt NAS copy | archive | `archiveHash(f)` |
| Restore: streamed device bytes | device | `sha256` (then the re-apply, §9.4) |
| Restore: at-rest verify of the file about to land | archive | the edited hash after a re-apply, else the streamed hash (comparing the re-applied temp to the streamed hash would fail every time and turn the recovery into a permanent no-op that pins the device original) |
| Restore: a copy found present at the destination | archive | `archiveHash(f)` |
| Operator restore pre-check ("back and good already") | archive | `archiveHash(f)` (else a healthy retargeted copy reads as damaged and a standing restore request pins the device original) |
| Differ: is the old NAS copy still safe? | archive | `archiveHash(row)` (else a false critical `frame_lost`) |
| Library auto-link sample verify | archive | the sample's stored archive anchor (else a retarget sample fails to verify the root, a false library-unavailable) |
| Re-file: is the file at the destination this frame? | archive | `archive_size`, then `archiveHash(f)` |
| Retarget: archive intact before editing | archive | `archiveHash(f)` |

Deletion eligibility is unchanged by a retarget: the frame keeps its state, a non-empty `sha256` and
its `pulled_at`, so it flows through the normal grace check. The full list of delete paths and their
checks is [delete-safety §3](2026-10-17-delete-safety.md).

### 9.6 Supervisor, manager and API

Everything is per rig. The supervisor exposes `RetargetFrame(rig, frameID, target)` and
`PreviewRetarget(rig, ids, target)`; the manager wraps each in its own timeout — 5 minutes for one
frame, which is a full read and write of the archive, and 10 seconds for a preview, which reads only
the database.

`RetargetFrame` is dispatched to the worker over a dedicated request/reply channel serviced **only in
the `Run` select**, the between-cycles point where no admission slot is held and no phase is running.
It is not serviced from the command drain the delete pass runs while it holds an admission slot:
retarget acquires a slot of its own, so servicing it there would deadlock a single-slot
configuration. Running at the select point serialises a retarget against the pull, restore and
delete phases and the scan by construction, so a retarget never runs concurrently with a delete pass
and two retargets of one frame are impossible. `PreviewRetarget` is read-only and runs on the caller's
goroutine.

| Endpoint | Behaviour |
|---|---|
| `POST /api/rigs/{id}/retarget/preview` | Body `{ids, target}`. Returns one `RetargetPreview` per id: `frame_id`, `current_target`, `old_path`, `new_path` (the index-0 canonical path, night included; apply may disambiguate the filename), `night_approx`, `existing_spellings`, `eligible`, and `reason` — which also explains a read the preview could not make on an eligible row. |
| `POST /api/rigs/{id}/retarget/apply` | Body `{ids, target}`. Calls `RetargetFrame` per id in the given order, without deduplicating, and streams one `RetargetResult` (`frame_id`, `status` of `retargeted`/`skipped`/`failed`, `reason`, `old_path`, `new_path`) per line as NDJSON, flushed per frame. A per-frame error becomes a failed line; an unknown rig before the first line is a 404. An empty id set answers an empty 200. |

Both are write-guarded. The handler trims the target and rejects an empty one or one longer than 60
bytes; the writer enforces its own 60-rune ceiling and the sanitise-empty refusal. The preview
rebuilds a classification from the row's stored columns with the folded, sanitised target, resolves
the night as §6.5 describes, routes it, and derives the canonical path the filing path would; it
reads neither the NAS nor the device. The sanitised target is deliberate and is the opposite of
re-file's raw target (§8.2), for the same reason: each preview must route what its own apply routes.
Retarget apply writes the sanitised label into `OBJECT` and routes the classification read back from
the edited file, so routing the raw typed string would create a preview/apply divergence on inputs
like `"Sadr/1-1"`, which sanitises to `Sadr_1-1` and is a panel on apply but would not be on
preview. Its numeric presence flags are approximated from the stored values (a stored zero reads as
absent, except `SET-TEMP`, whose absence is recorded); apply re-reads the real header.

### 9.7 The Tools page

The Tools page hosts the retarget tool and re-file (§8.11).

- **Filter bar:** a date range over `date_obs` and a case-insensitive substring match on the target.
- **Result list:** matching frames with checkboxes and select-all / deselect-all, showing target,
  filter, exposure, observing timestamp and current folder.
- **Target input** with live validation and a **Preview** action showing `old path → new path` per
  selected frame, with the night caveat (§6.5) and the spelling report (§6.7).
- **Apply** with a confirmation stating the count of eligible frames and the target, then a progress
  view driven by the streamed results. A selection spanning rigs is grouped by rig and issued as one
  preview and one apply per rig.

The page is deliberately thin; correctness lives on the server.

### 9.8 Scope and open edges

**In scope:** frames that carry an `OBJECT` card and have an archive to edit — `verified` (the device
original may or may not still exist; retargeting keeps it deletable under the right label) and
`deleted` (a pure archive relabel; the state stays `deleted`). **Out of scope:** `unclassified` (no
usable `OBJECT`), `held`, `failed`, `dismissed`, any frame without a `dest_path`, frames mid-transfer
(no row yet), and frames with duplicate `OBJECT` cards (the writer refuses them).

Known edges, stated as they stand:

- **The old path is removed without a sharer check.** Step 7 removes the pre-retarget `dest_path`
  once the retargeted row's commit is proven, without asking whether another row shares that path.
  The filing path updates the one row being retargeted, so for a frame held on two shares (H12) the
  sibling row keeps naming the removed path; the scan then reads it as missing and it enters
  reconciliation ([library-reconciliation](2026-10-17-library-reconciliation.md)) while the frame's
  bytes survive at the retargeted path under the other row. Re-file, by contrast, moves every sharing
  row together (§8.7).
- **Stray temps are not swept.** A crash in the first window leaves an `asiair-retarget-*.tmp` in the
  staging root, and the third window leaves an orphaned old archive. Neither is reclaimed
  automatically. No invariant depends on cleanup, and admission's disk reserve still accounts for the
  space.
- **The target filters run in the browser.** The frame list reuses the cross-rig transfer-history
  read, which has no target-substring or observing-date filter, so the Tools filters narrow the
  loaded page only; a match on a later page is not shown until that page is loaded.
- **Byte and rune limits differ.** The API caps the target at 60 bytes and the writer at 60 runes, so
  a multibyte target is refused at the API before it could reach the writer's limit; the two never
  disagree in the other direction.
- **Night bookkeeping.** Re-filing under the corrected target assigns the night in that target's scope
  (§6.4); the vacated scope's counter is not decremented (§6.2).
- **Sidecars and previews are unaffected.** Device-side sidecars stay on the row and are removed by the
  delete pass; the preview cache is keyed on content and derived from pixels an `OBJECT` edit does not
  touch.

### 9.9 Invariants INV-1–INV-10

Byte-level and delete-path invariants use real, header-carrying FITS fixtures, because fakes cannot
prove FITS byte layout or the real delete state machine.

| ID | Invariant | Test |
|---|---|---|
| INV-1 | A retarget never mutates `sha256`. | `TestRetarget_PreservesOriginalSHA256` |
| INV-2 | A retarget never reads-modifies-writes or deletes the device original; only the NAS copy is edited. | `TestRetarget_DeviceOriginalUntouched` |
| INV-3 | A retarget refuses unless the current archive hashes to `archiveHash(f)`. | `TestRetarget_RefusesDriftedArchive` |
| INV-4 | The edit changes only the `OBJECT` card's 80 bytes: the complement digests of source and edit are equal (every other card and the entire data unit byte-identical) and the size is unchanged; a data-unit-corrupting writer is refused at edit time and never committed to the overlay. | `TestRewriteObject_OnlyObjectCardChanges`, `TestRewriteObject_RefusesDuplicateObject`, `TestRewriteObject_ComplementCatchesDataCorruption`, `TestRetarget_RefusesCorruptingWriter` |
| INV-5 | Delete, restore, differ, vanished-source and library auto-link verification compare the device side to `sha256` and every archive side to `archiveHash(f)`; device-original deletion still requires `deviceHash == sha256`. | `TestDelete_RetargetedThreeWay`, `TestDelete_RetargetedVanishedSource`, `TestDiffer_RetargetedNoFalseLost`, `TestRestoreOne_RetargetedPresentRecognized` |
| INV-6 | Interruption is possible only at process restart (worker-goroutine execution), and every crash window leaves the frame recoverable — never a false device delete, never data loss. | `TestRetarget_CrashWindows` |
| INV-7 | Retarget is re-runnable: a repeat edits from the current archive, updates `retarget_sha256`, and `retarget_from` keeps the first original target. | `TestRetarget_Idempotent_RetargetFromStable` |
| INV-8 | Any re-pull of a retargeted frame's device bytes (differ, deleted re-adopt, restore) re-applies the edit and files with the overlay; it never files an original-header archive into a retargeted folder nor reverts the label. | `TestReapplyRetarget_OnDifferJitter`, `TestReapplyRetarget_OnDeletedReadopt`, `TestRestore_ReappliesRetarget` |
| INV-9 | Retargeting is restricted to `verified` or `deleted` frames with a single `OBJECT` card and a `dest_path`; a `deleted` frame stays `deleted`. | `TestRetarget_ScopeGuards`, `TestRetarget_DeletedStaysDeleted` |
| INV-10 | The overlay resets **only** when re-pulled content is genuinely new — matching neither the row's `sha256` nor its `data_sha256`; an identical re-pull and a device-side revision of the same exposure both preserve it. | `TestDiffer_ResetsOverlayOnNewContent`, `TestDiffer_PreservesOverlayOnIdenticalContent` |

## 10. Invariants INV-M1–INV-M11

INV-M1 comes first because it subsumes much of the rest: a reset retarget overlay, a resurrected
`deleted` frame and erased reconciliation marks are all violations of it, and one test asserting
whole-row equality except `dest_path` catches every one and any future regression of the same shape.
INV-M2 is second because its failure destroys data: every way a frame can end up marked missing leads,
one settle window later, to its device original being deleted against an archive nothing references.

| ID | Invariant | Test |
|---|---|---|
| INV-M1 | **A re-file writes `dest_path` and no other column.** State, hashes, the retarget overlay, `night` and every reconciliation mark are byte-equal across the move. | `TestRefileWritesOnlyDestPath` |
| INV-M2 | **No re-file failure can lead to a device original being deleted.** An open intent is a deferral reason, so a frame mid-move is never deletion-eligible however the move failed — crash, commit failure or failed rename-back — and startup recovery resolves it without ever binding a row to a sibling's file. | `TestRefileNeverStrandsAFrame`, `TestRecoverRefileIntentChecksSourceFirst`, `TestRefileCommitFailureAlertsWhenRenameBackFails`, `TestRefileInFlightDefersDeletion`, `TestRefileCommitFailureNeverReleasesTheDeviceOriginal` |
| INV-M3 | Re-file never alters a single byte of a frame's contents. | `TestRefileContentUnchanged`, `TestRefileRefusesCrossVolumeRename` |
| INV-M4 | The mosaic template routes a frame only when `mosaic_detection` is on **and** the frame is a `LIGHT` whose transformed target parsed as a panel with row and column in 1–20; a mosaic-only token elsewhere quarantines rather than rendering verbatim. With detection off, routing is identical to routing without the feature for every config that names no mosaic-only token. | `TestRouteMosaicOnlyForParsedLightPanels`, `TestMosaicTokenOutsideMosaicTemplateQuarantines` |
| INV-M5 | Every row of the rig sharing a `dest_path` is updated in the same transaction as the move; if any sharing row is ineligible the path is refused entirely, and a cross-rig sharer is refused. | `TestRefileUpdatesAllSharingRows`, `TestRefileRefusesPathWithIneligibleSibling`, `TestRefileRefusesCrossRigSharer`, `TestRecoverRefileIntentCommitsEverySharingRow`, `TestMoveDestPathMovesSharingRowsTogether` |
| INV-M6 | Re-file never touches a row whose `dest_path` is NULL or empty, and never issues a move keyed on an empty path. | `TestRefileIgnoresPathlessRows`, `TestRefileCandidatesNeverOffersAPathlessRow`, `TestMoveDestPathRefusesEmptyOldDest` |
| INV-M7 | Re-file never moves or deletes a file that is not a frame it was asked to move, and never creates a second copy of one it was. | `TestRefileLeavesUnmanagedFiles`, `TestRefileAdoptsIdenticalDestination`, `TestRefileCaseOnlyRenameAdoptsTheSameFile` |
| INV-M8 | The prune never removes a non-empty directory, and reports each one it skipped. | `TestPruneSkipsAndReportsNonEmpty`, `TestPruneNeverRemovesAMarkedDirectory`, `TestRefilePrunesEmptiedDirectories`, `TestRefileMosaicMigrationRealFS` |
| INV-M9 | Re-file never re-assigns a night ordinal; `night_counters` is unchanged. | `TestRefilePreservesNight` |
| INV-M10 | Apply never files a frame into a directory outside the selected groups. | `TestRefileRefusesUnselectedDestination` |
| INV-M11 | A saved mosaic template always distinguishes panels before the `<n>` segment, so a mosaic produces no multi-target `NIGHT_n` folder except through the accepted component-truncation residual of §7.1. | `TestMosaicTemplateMustDistinguishPanels` |

## 11. Acceptance criteria: mosaic panels and re-file

The numbers are stable references; code and tests cite them as
`design routing-and-filing AC<N>`.

**Routing**

1. With `mosaic_detection` off — the default — every routing outcome is byte-identical to routing
   without the feature for every config that names no mosaic-only token, including for
   `OBJECT = "Sadr_1-1"`; the shipped default is off.
2. Toggling detection changes only which template is selected; the toggle itself moves no frame.
3. A `LIGHT` with `OBJECT = "Sadr_1-1"` and the default mosaic template files to
   `<light root>/Sadr/<OTA>/<SESSION>/Sadr_1-1/NIGHT_1`.
4. A `LIGHT` with `OBJECT = "NGC6888"` files through the light template, unchanged.
5. `Sh2-101`, `M 1-92`, `K 1-16`, `Abell 2-1` and `PK 164+31.1` route through the light template;
   `Sh2-101_1-1` routes through the mosaic template with base `Sh2-101`.
6. Date-suffixed targets route through the light template: `M42_2026-07`, `NGC7000_2024-11`,
   `M42_12-25`, and `Sadr_0-0`, all rejected by the 1–20 bound.
7. The accepted residual false positives are pinned, so changing the bound is a deliberate decision:
   `Abell_2-1` and `K_1-16` do parse as panels.
8. A `DARK`, `FLAT` or `BIAS` frame carrying `OBJECT = "Sadr_1-1"` routes through its calibration
   template under the calibration root.
9. `OBJECT = "_1-1"` quarantines rather than filing under an empty base.
10. All three layout forms of §7.2 render the paths shown there.
11. The two default-equivalent forms agree on adversarial targets: `"NGC 7000 _1-1"` with and without
    space-stripping, and `"Sadr_1 -1"` with stripping on (a panel only after the strip).
12. The panel-distinguishing rule is positional, not a containment test:
    `<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>` is rejected, and so is
    `<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>/<PANEL>`;
    `<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>` is accepted.

**Config**

13. With detection on, saving a mosaic template containing an unknown token or no `<n>` is rejected
    with a field-level error (suppressed while off, as AC 15's check is); an empty mosaic template is
    normalised to the default rather than rejected, on both the load and save paths.
14. A mosaic-only token in the light or a calibration template is rejected at save, and a hand-edited
    config carrying one quarantines the frame instead of filing it into a directory named `_PANEL_` —
    exercised with detection on and a non-panel light, since a panel light never reaches the light
    template.
15. A mosaic template using `<OTA>` on a rig with a blank OTA is an error while detection is on, is not
    reported while it is off, and the message names the mosaic template.
16. `<PANEL_ROW>` and `<PANEL_COL>` save and render while being absent from the Settings token chips.
17. The Settings live preview shows a mosaic sample row when detection is on, and neither the row nor
    the template field when it is off, using a real panel frame when one exists and a synthetic
    `Sadr_1-1` header otherwise.
18. A `config.yaml` with no `templates.mosaic` key loads with the default applied and a subsequent
    config save succeeds. A `PUT` omitting both keys succeeds **and leaves a stored
    `mosaic_detection: true` and a customised `templates.mosaic` unchanged**.
19. A config carrying both keys still loads in a binary that does not know them; the config is never
    what blocks a downgrade.

**Store**

20. The panel columns are populated for a frame filed while detection is off, and hold the same values
    the migration backfill produces for an identical pre-existing row — including a base containing a
    space (`NGC 7000_1-1`) with space-stripping on.
21. The migration adds the panel columns and backfills every existing row from its stored `target`,
    including rows with no panel suffix (empty base, zero row and column).

**Re-file**

22. Preview over a library with no template change returns zero groups.
23. After switching to the mosaic template, preview groups the panel directories and apply moves every
    frame, leaving `dest_path` correct and the files readable at their new paths.
24. A re-filed row differs from its pre-move self in `dest_path` and nothing else — `state`,
    `deleted_at`, `sha256`, `retarget_sha256`, `retarget_from`, `night`, `missing_seen_at`,
    `missing_acked_at` and `restore_requested_at` are byte-equal (asserted over the whole row, with the
    solver's own columns excluded or the solver disabled, since it writes them from its own goroutine).
    This is INV-M1.
25. A retargeted frame survives a re-file: its archive anchor still resolves to the edited hash and the
    delete pass raises no `delete_mismatch`.
26. A `deleted` frame is still `deleted` after a re-file, does not reappear as deletion-eligible, and
    does not inflate the resident count.
27. A row with a NULL or empty `dest_path` is never offered and never moved — specifically a resolved
    culled-original drain row (`deleted`, no missing mark, NULL path) and a failed re-pull row (literal
    `''`) — and `MoveDestPath` refuses an empty old path.
28. Frames in `unclassified`, `dismissed`, `failed` or `held`, or carrying `missing_seen_at`, are never
    moved, and apply refuses any that reach it with a stated reason.
29. Preview reports no spurious quarantine for a calibration frame recorded at `SET-TEMP 0` or `GAIN 0`,
    and renders each frame's stored `night`, never `NIGHT_0`. Preview and apply build their routing
    inputs through the same helper, so a frame either previews as movable and moves, or is refused by
    both; the skips apply adds are the row becoming ineligible in between, an INV-M10 mismatch, `EXDEV`
    and a failed rename.
30. Two rows sharing one `dest_path` are both updated by a single move, and tonight's frame count is
    unchanged across the migration. A `dest_path` shared across two rigs is refused and reported.
31. Emptied directories are removed up to whichever root contains them; a directory holding an operator
    file or a library marker is left in place and reported (against a real filesystem).
32. A crash between rename and commit, injected through the re-file crash hook, leaves no frame marked
    missing after restart, and recovery binds the row to its own file rather than a same-named,
    same-sized sibling. A crash after the commit but before the intent is cleared resolves with no
    further change.
33. Re-pulling a re-filed frame's device source fires no differ or re-adopt branch and raises no
    `frame_lost` alert.
34. A frame that stops being eligible between preview and apply is skipped and reported rather than
    moved from a stale snapshot.
35. A destination already holding a byte-identical copy of the frame is treated as already moved, not
    disambiguated into a second copy — including the case-only rename on a case-insensitive volume,
    where the exclusive rename reports the destination exists because it is the file itself.
36. If any row sharing a `dest_path` is ineligible the whole path is refused: a `held` or
    missing-marked sibling of a `verified` H12 row is never relocated.
37. A database commit failure in a living process renames the file back and drops the intent; if the
    rename back also fails, the intent survives and a critical alert is raised. The frame may be marked
    missing then; what the open intent guarantees is that it is never deletion-eligible.
38. Cancelling the HTTP request stops the migration; re-running preview shows the remaining work and a
    second apply completes it.

**Operator verification**

These two are checked by hand on a real install rather than by a test.

39. In a browser, the mosaic template field is hidden until the toggle is turned on, and the copy
    names the heuristic plainly. With the toggle on, the Settings preview shows the `MOSAIC` row; the
    Tools re-file preview and apply replace a mosaic's flat `<base>_<row>-<col>` folders with one
    `<base>/` tree holding per-panel subfolders, as the file browser shows. On a macOS-mounted share,
    `.DS_Store` files strand some folders and the preview warns about them. Turning the toggle back
    off shows the warning about already-filed panels.
40. The rig still reports safe-to-power-off after the migration: no frame was marked missing by the
    move, and the drain and resident counts are unchanged.

## 12. Rejected alternatives

The night-numbering alternatives are in §6.10 and the re-file crash-safety alternatives in §8.6.

**Mosaic panels and re-file**

| Alternative | Rejected because |
|---|---|
| One light template with an optional, collapsing `<PANEL>` | Generalising the `<FILTER>` collapse rule would change routing for every template mixing optional tokens with literals. A separate template makes the panel tokens unconditional. |
| Redefine `<TARGET>` as the base inside the mosaic template | One token would mean two things depending on the field; pasting the light template into the mosaic field would silently merge every pane into one folder. `<TARGET_BASE>` costs one token and removes the ambiguity. |
| A "flat" mosaic layout with no panel token | The only way a mosaic produces a multi-target `NIGHT_n` folder, which silently destroys night recovery; supporting it needs a rebuild predicate that cannot see the templates that produced the folders on disk. Refused at save (§7.3). |
| Pointing geometry (`RA`/`DEC` plus field of view) as the detector | The name is a producer contract, so geometry is redundant, and it would add field-of-view inference, meridian-flip handling and a tolerance someone would have to tune. It also cannot resolve the single-frame false positive. |
| Parse the panel in classification | See §7.1: ambiguous about rewriting the target, both readings break something, and it forces panel fields onto every hand-built classification. |
| A first-class mosaic entity with completeness tracking | A distinct feature with its own schema, API and surface; persisting the parsed columns (§7.4) keeps it a read-side addition. |
| A sentinel panel folder for standalone base-target frames | Speculative structure for a case the operator resolves by naming. |
| Read the ASIAIR plan over SMB for authoritative pane counts | Useful only to a completeness view; the format is undocumented and firmware-dependent, and the name already yields base and panel. |
| An alert when the library is split across layouts | Not built: re-file's mosaic-scoped preview already shows exactly which frames are in the old layout, on request. |
| Base-scoped night ordinals for a mosaic's panes | The rebuild derives the target from `OBJECT`, so it would have to strip panel suffixes, conflicting with rows already written; `NIGHT_2` inside `Sadr_1-1/` meaning the second night on that pane is what someone opening the folder expects. |
| Detection on by default | Detection is a heuristic with an accepted false-positive class, so the operators who carry its risk should be the ones who asked for it; off also keeps routing unchanged for everyone else. |
| An exception list of target names never treated as panels | Machinery for a case with no known instance; an operator with such a target who does not shoot mosaics leaves detection off. |
| Route re-file through the filing path | Resets the overlay, resurrects `deleted` frames, clears reconciliation marks, charges a rename to the transfer budget and can delete the operator's archive in the idempotent-keep branch (§8.1). |
| A size and mtime precondition before moving | `frames.mtime` is the device file's; nothing stamps the NAS copy, and size alone discriminates nothing. Unnecessary once re-file writes only `dest_path` (§8.5). |
| Read every header during the night-counter rebuild | One open and read per frame at startup, proportional to the library rather than its folder count; unnecessary once a mosaic template that cannot distinguish panes is refused at save. Two narrow routes to a multi-target folder remain and are accepted: a light template lacking `<TARGET>` (§6.8) and the component truncation of §7.1. |
| A configuration schema migration that writes the default mosaic template | The default names `<OTA>` and `<SESSION>`, so an install whose rig leaves them blank would fail validation from the moment it migrated. Normalising an empty value achieves the same without rewriting anyone's config (§7.5). |
| Derive the default mosaic template from the install's light template | Migration machinery to avoid a problem normalisation already avoids; a plain default is smaller and sufficient. |
| Repair a hand-moved library by relinking rows to files found by content | A digest-based reconciliation — re-hashing files that are not where the database expects and relinking them — would dissolve the hand-move hazard rather than guard against it, but it is a separate safety model with its own design. Nothing here depends on how it resolves, and the intent record stays correct either way. |

**Retargeting**

| Alternative | Rejected because |
|---|---|
| Overwrite the device anchor with the post-edit hash | The untouched device copy would then hash to a value that no longer equals `sha256`: every re-pull would read as different content, and the culled drain and three-way check would hold the frame permanently, so the device original could never be released. |
| A `renamed` frame state | A retargeted frame is still verified and archived. A state would force every consumer — eligibility, the safe-on-NAS checks, the backlog UI, dismiss, restore — to learn it; orthogonal columns already model the analogous missing and acknowledged facts. |
| Route retargeted frames through the culled path | The archive is not absent; it exists at a path of its own. Forcing it through the absent-frame path means a fake missing mark and fighting the path-nulling drain. The anchor split preserves the right insight — the device anchor authorises the device delete — without hijacking that plumbing. |
| Reverse the edit at delete time to reconstruct the original digest | Needs the exact original card bytes and a re-edit on every delete pass, with no safety gain: a buggy edit would corrupt the archive and the reconstruction identically. The edit-time complement check catches a corrupting writer once, before anything commits. |
| Rename in place, then move | Needs an atomic replace that `localfs` lacks and SMB does not provide, and the intermediate window lets the scan see the old path missing and, after the settle window, release the device original on a false cull. |
| Move the file only, leaving `OBJECT` untouched | Leaves the archived file's `OBJECT` contradicting the folder it sits in, misleading every tool that groups by `OBJECT`. |
| Add an `OBJECT` card to frames that lack one | Can overflow the 2880-byte header block and shift every byte. |
| Exclude retargeted frames from the differ re-pull | ASIAIR reuses paths at the same size across sessions, so a size-only skip would strand genuinely new frames. The re-apply rule handles both cases (§9.4). |
