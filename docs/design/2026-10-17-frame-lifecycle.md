# The frame lifecycle

**Scope.** This document follows one managed rig through one cycle, end to end: the
reachability probe, the SMB session, the listing, the decision to pull, the pull itself, the
cache-bypassing verify at rest, classification, routing and atomic filing, the delete pass and
the disconnect. Sections 1–10 are those steps, numbered as the steps are numbered everywhere
else (a code comment citing "design frame-lifecycle §4" means the decide-to-pull step). The
later sections cover what a cycle meets on top of the straight path: a frame the ASIAIR revises
on the device after Commander has filed it (§11, hardware finding H13), the slicing that keeps a
long delete backlog from freezing the listing (§12), dismissing a frame that can never be read
(§13), and the two verdicts the card shows for a rig, "Safe to power off" and "drained" (§14).
Classification and template routing are specified in
[routing-and-filing](2026-10-17-routing-and-filing.md); device identity, discovery and removable
media in [device-identity-and-media](2026-10-17-device-identity-and-media.md); culled frames in
[library-reconciliation](2026-10-17-library-reconciliation.md); the cross-cutting delete rules in
[delete-safety](2026-10-17-delete-safety.md). The hardware facts cited here by H-number are
recorded in [asiair-hardware-notes §2](2026-10-17-asiair-hardware-notes.md).

All SMB I/O within one rig is serial: the rig's worker goroutine runs the cycle, and every frame
mutation happens on it ([architecture §3](2026-10-17-architecture.md)). Before step 1 the cycle
runs the NAS-only library scan, so a rig whose ASIAIR is powered off still reconciles culls
([library-reconciliation §4.3](2026-10-17-library-reconciliation.md)), and then resolves the
device's endpoint from the rig's guid binding, its last-known endpoint, or its configured host
([device-identity-and-media §3.2](2026-10-17-device-identity-and-media.md)). A rig on a remote
site whose tunnel is down is offline for the cycle ([remote-sites §8](2026-10-17-remote-sites.md)).
After the delete pass, the clock phase may set the device clock
([features §6](2026-10-17-features.md)).

## 1. Reachability probe

The worker dials TCP/445 at the resolved endpoint through the endpoint's site, bounded by the
site's SMB budget (its stall window plus round-trip margins, 30 s on the local site), and closes
the connection at once. A failed dial
means the rig is offline: the cycle ends silently, the card reads Offline with "Safe to power
off" Unknown/"offline", and the next attempt comes after `retry_interval` (5 min by default)
rather than `poll_interval`, unless a discovery sighting wakes it sooner
([device-identity-and-media §2](2026-10-17-device-identity-and-media.md)). A rig that is not there is a normal state, not an error; the
`unreachable` alert is raised only once a managed rig has been unreachable for longer than
`unreachable_alert_after` (15 min by default). The probe exists so a powered-off rig never walks
the long failure paths inside the SMB stack.

## 2. Connect

The pull phase opens one SMB1 session with `github.com/macourteau/smb1client`, NTLMv2 with the
ASIAIR's documented guest login. The client accepts a guest session, because the ASIAIR answers
that login by granting guest access rather than by authenticating the name. Guest can delete
(H7), so every guard on a destructive act is Commander's own.

At negotiate the session yields the device's clock (H1). The offset `ServerTime − ReceivedAt` is
measured once per session and applies **only** where a device timestamp meets host time — the
`min_file_age` test of §4.4. Device-versus-device comparisons (a listed mtime against a recorded
mtime) use raw values with a ±2 s tolerance and never apply the offset. The same measurement is
the input to device clock sync ([features §6](2026-10-17-features.md)).

While the session is open the worker confirms by a live `scan_air` probe that the device
answering is this rig's device. A foreign device refuses every pull and delete for the cycle and
alerts; an unanswered probe refuses the cycle without alerting until three consecutive misses
([device-identity-and-media §4](2026-10-17-device-identity-and-media.md)). Every later phase that
opens its own session (restore, each delete slice) re-confirms identity on that session.

## 3. List

### 3.1 Shares and roots

A rig's `sources` name one or more shares, each with its watched roots; sources naming the same
share merge, so each share is mounted and listed once. The ASIAIR's share layout — `EMMC
Images`, `TF Images`, `Udisk Images`, the `Autorun/<TYPE>/<TARGET>/` capture tree and the
`ASIAIR/` nesting on a raw removable stick — is recorded in
[asiair-hardware-notes §3](2026-10-17-asiair-hardware-notes.md). The worker resolves each share's
data prefix once (the share root on a fixed share, `ASIAIR/` on a raw Udisk), so the walk and
every pull, delete and orphan path inherit it. The share root lists as `""`, not `"."` (H2).
Session logs are pulled from the share's `log/` folder in the same session under relaxed rules
([architecture §11](2026-10-17-architecture.md)); a log-pull failure never fails the frame cycle.

Every verdict derived from a listing is only as trustworthy as the listing's completeness
([delete-safety §5](2026-10-17-delete-safety.md)). A share that fails to mount or list makes the cycle's listing incomplete (`listingOK = false`),
which keeps "Safe to power off" from reading Yes (§14.1). Each successful share listing records
its entry count with a timestamp, so a frozen count with an old timestamp reads as stale rather
than fresh.

An empty removable slot is not a listing failure: when the configured root resolves nowhere and
the structural presence probe proves the slot empty, the share is skipped at Debug level and
marked absent ([device-identity-and-media §7](2026-10-17-device-identity-and-media.md)).

### 3.2 The junk filter

The walk drops, case-insensitively and by name only:

- **directories**, pruned whole: the ASIAIR's live-stack scratch `batch_stack_tmp`, `System
  Volume Information`, `$RECYCLE.BIN`, `lost+found`, NAS recycle bins and thumbnail folders,
  Syncthing and macOS metadata folders;
- **files**: `FSCK*.REC` filesystem-recovery artifacts (H4), AppleDouble `._*` twins, `~$*` lock
  files, `.DS_Store`, `Thumbs.db`, `desktop.ini` and similar, any `*.tmp` or `*~` file, and
  Commander's own marker files — the removable-media marker and `.asiair-commander-rig-id`, a
  rig-identity marker Commander does not use.

Identity is proven by the per-session probe, not by a marker ([device-identity-and-media
§4.5](2026-10-17-device-identity-and-media.md)), so when the pull phase finds a rig-identity marker
at a share root it removes it, silently and best-effort; a read-only root may keep it, which is
harmless because the filter already excludes it. Junk is never transferred, classified or counted.
Tests: `TestIsJunkFile`, `TestIsJunkDir`.

All extension matching — transfer, exclude and sidecar — is case-insensitive. FAT commonly
stores `.FIT` and `.JPG` uppercase, and a case-sensitive compare would leak previews onto the NAS
or skip a science frame.

### 3.3 Sidecars and the orphan-preview sweep

Files whose extension is in `exclude_extensions` (by default `jpg`, `jpeg`: the ASIAIR's
previews) are not transferred, but they are not forgotten. Each is a **sidecar** of the frame
whose stem it matches once its `_thn` suffix is stripped: the ASIAIR writes the preview of
`Light_NGC 7000_300.0s_Bin1_20260101-000000_0001.fit` as
`Light_NGC 7000_300.0s_Bin1_20260101-000000_0001_thn.jpg`. A preview whose base does not end in
`_thn` pairs with nothing. The sidecars observed at listing are recorded on the frame row and
deleted from the device with their parent (§9.6). Tests: `TestPreviewStem`, `TestIsSidecarOf`.

A preview whose parent never becomes deletion-eligible would otherwise accumulate on a capacity
rig. The **orphan-preview sweep** runs after the pull walk, only when the listing was complete,
the rig is not suspended and `keep_originals` is off (a rig set never to delete from the device
deletes nothing there, previews included; `TestKeepOriginalsSkipsOrphanSweep`). It removes a
preview only when no frame in the same share's listing is its parent — the parent is gone from
the device, so there is nothing left to protect. A preview whose parent is still resident is
kept: it is deleted with its parent once that parent is proven, or kept for as long as the parent
stays unresolved, because deleting it could discard the only artifact of a frame that is not yet
safe. On a Udisk share the sweep also requires the media marker's cleanup verdict
([device-identity-and-media §6](2026-10-17-device-identity-and-media.md)).

### 3.4 Two shares on a USB-primary rig

When the USB stick drops out mid-session, the ASIAIR silently fails over to the internal eMMC, so
one capture run's frames split across `Udisk Images` and `EMMC Images` with no clear log error
(H12). A USB-primary rig should therefore watch both shares, and config validation warns when a
rig watches `Udisk Images` but not `EMMC Images` (`TestValidateUdiskWithoutEMMCWarning`). If the
operator dismisses the warning, the failed-over frames are invisible: not lost (they sit on the
eMMC), but neither backed up nor surfaced until the share is added.

Split-session frames need no special handling. They file by header, so both halves land in the
same `TARGET/OTA/NIGHT_n` folder whichever medium held them; their sequence numbers do not clash;
and a byte-identical frame present on both shares is de-duplicated by the content-hash idempotent
keep at filing (§8.2). That keep is what makes two rows share one `dest_path`, the case §11.1's
commit rule and §13's purge rule exist for.

## 4. Decide to pull

Decide asks two questions of each listed frame: have we already got this file, and is it done
being written. The first is answered against the frame row keyed on `(rig_id, source_share,
source_path)`; the second is quiescence (§4.4), applied only to frames the first question sends
to pull.

### 4.1 The skip predicate

The listed size and mtime are compared with the row's **raw** stored `size` and `mtime`, the
mtime within ±2 s. The tolerance covers FAT's 2 s granularity and per-connect clock-offset
jitter; an exact or offset-corrected compare would flip "match" to "differ" on an unchanged file,
re-pull it every cycle, reset grace and stall the drain.

| Row | Listed file | Outcome |
| --- | --- | --- |
| none | any | Pull. A fresh frame, or the whole state database was lost (§8.4). |
| marked missing (culled), not `failed` | same size | Skip, before the mtime compare ([library-reconciliation §4.4](2026-10-17-library-reconciliation.md)). |
| `verified`, `unclassified`, `held`, `dismissed` | matches | Skip: a settled state. |
| `failed` | matches | Retry once its backoff has elapsed (§4.3). |
| `deleted` | matches | Pull: the source reappeared; the idempotent keep re-adopts a still-present NAS copy. |
| any | differs | The differ branch (§4.2). |

The cull gate is keyed on size deliberately: a different card reader or a FAT mtime shift must
not resurrect a frame the operator deleted on the NAS, so only content of a different size falls
through to the differ branch.

This one predicate is the whole reconciler. After a database loss every resident frame looks
fresh and is re-pulled, adopting its NAS copy through the idempotent keep; there is no separate
reconciliation pass.

A frame that is not pulled still counts against "Safe to power off" unless its state is already
safe on the NAS: `verified`, `unclassified` (verified at rest before it was quarantined) or
`dismissed` (deliberately abandoned). `held`, `failed` and un-pulled frames count as pending
work. A culled frame whose source is still listed does not pin pending work; it downgrades the
verdict instead (§14.1).

### 4.2 A reused path: the differ branch

A size or mtime beyond tolerance means the device has different content at that path: a
counter-reset capture run overwrote the old file on FAT, or the ASIAIR revised the frame in
place (H13, §11). The differ branch resolves the **old** row by proof before the pull overwrites
it, mirroring the vanished-source rule of §9.4.

- A row marked missing is pulled with no alert: the operator deleted its NAS copy, and filing
  clears the stale mark.
- A `verified`, `unclassified` or `held` row with a recorded hash and destination has its NAS
  copy read back with the cache-bypassing `VerifyRead` at `archive_size` and compared with
  `archive_sha256` (§11.1). A match is a benign overwrite: the old copy is safe, and the pull
  proceeds. A confirmed missing, truncated or mismatching copy is a genuine loss and raises a
  critical `frame_lost` alert. A transient read error is not a conclusion: the pull is deferred
  to the next cycle, so the old row is never overwritten while its fate is unproven.
- A `held` row belongs to the arm that looks. A held row is exactly what a device-side revision
  leaves behind after the delete pass met it first (§11.13), and its archive copy is the suspect
  one, which is a reason to verify it, not a proof about it. Concluding loss from the state flag
  would raise a critical `frame_lost` over an intact copy ([delete-safety
  §2.3](2026-10-17-delete-safety.md)). No backfill could fix that from the flag side: a held row
  with no missing mark is exactly what `holdFrame` leaves behind, so only reading the copy settles
  it.
- A `failed` row never had a good copy, and no read can settle that: it raises a critical
  `frame_lost`.
- A `dismissed` row's copy was purged on purpose: the changed source is pulled with no alert
  (§13.2). A `deleted` row is pulled.

Tests: `TestDecideDifferBenignThenPull`, `TestDecideDifferMissingNASAlertsLost`,
`TestHeldRowWithAnIntactArchiveCopyIsNotALostFrame`, `TestFailedRowStillConcludesFromItsState`.

### 4.3 Failed frames retry with backoff

A `failed` row does not skip forever. A classify blip or a header quirk later fixed by an alias
or a coercion must not strand a frame, so a failed frame is retried once its backoff has elapsed:
2 min doubling per attempt, capped at 1 h, up to 6 attempts (`failed_retry`). Each failure raises
a warn `transfer_failed`; the attempt that exhausts the budget raises a critical "permanently
failed" alert, and the frame then stays `failed` until an explicit Run now or reclassify
(`TestDecideFailedRetryBackoff`).

### 4.4 Quiescence

A pull candidate must pass both gates: its offset-corrected mtime must be at least `min_file_age`
old, and its size and mtime must have held unchanged for `settle_cycles` consecutive
observations. A still-writing frame fails at least one. Settle matters because the ASIAIR's
storage is FAT (H4), where a timezone or DST shift can let a still-writing file pass the age test
alone.

Settle is cycle-grained by nature: it is observed once per listing, so it is expressed in whole
cycles rather than as a sub-`poll_interval` duration that would read as finer than it can be. A
first sighting counts 0 and each unchanged re-sighting adds one, so the shipped `settle_cycles`
of 1 means "unchanged across two consecutive listings". A file that disappears from a listing
loses its settle history.

The settle observation is taken **before** the age test, so the counter accrues through the age
wait. At the shipped 45 s poll and 300 s age a frame has been sighted several times by the time
the age gate opens, so `settle_cycles` of 1 and 2 behave identically; a difference appears only
when two cycle periods together outlast the age wait, a cycle period above about 150 s, which
happens only while a rig is transferring a long batch. A device-side revision (H13) moves the
size and mtime, which resets the counter and restarts the age wait together, so it does not
separate the two values either. Raising
`settle_cycles` alongside `min_file_age` therefore changes nothing; it is listed because it looks
like an obvious improvement and is not one (§11.14).

The shipped `min_file_age` is **300 s**. A frame is complete and readable well before that, but
the ASIAIR can revise it in place afterwards (H13): the revision becomes visible some 180–250 s
after the frame's timestamp, so a pull at two minutes lands before it and a pull at five minutes
usually lands after it. Pulls under a two-minute age land some 140–180 s after the frame's
timestamp, inside that window; 300 s clears the latest revision observed by about 50 s. A
frame pulled after the device has finished carries the device's WCS,
which is better data and costs no Tier-1 solve ([features §3](2026-10-17-features.md)). Three
limits bound what the number is worth:

- It is a default: `EffectiveMinFileAge` resolves a per-rig override first, so nothing may reason
  as though every rig waits 300 s.
- Lowering it increases supersedes (§11.7), not duplicates. That is acceptable because the
  supersede path is the correctness guarantee and the gate is only the optimisation. The limit is
  stated so that a support question about `min_file_age` has an answer for why it matters.
- It stays probabilistic: the device's solve queue has no bound Commander can observe, so 300 s
  makes the race rarer and does not end it.

Tests: `TestSettleTracker`, `TestQuiescence`, `TestShippedMinFileAgeHoldsAFrameForFiveMinutes`,
`TestShippedMinFileAgeDefersARealPull`.

## 5. Pull

### 5.1 Admission and the transfer semaphore

Every transfer passes the global admission gate ([architecture §3.4](2026-10-17-architecture.md)),
acquired with the worker's context so a shutdown or a pause aborts a queued acquire rather than
only a between-files check. The gate's policy picks a concurrency slot: rigs on the local site
share `max_concurrent_transfers` (3 by default) in FIFO order, and every remote site has one slot
of its own outside that cap ([remote-sites §9](2026-10-17-remote-sites.md)). Within one rig,
transfers are serial.

Once the slot is held, the gate admits the frame only if the destination volume keeps
`disk_reserve_bytes` (5 GiB by default) free beyond this frame's projected size **and** the bytes
already reserved by other in-flight transfers ([architecture §7.6](2026-10-17-architecture.md)).
Below the reserve the frame is skipped for this cycle, not failed. A free-space query that fails
means the destination is unreadable: the worker latches that for the cycle, abandons the rest of
the walk and logs once, rather than failing every queued frame in turn
(`TestDestinationUnavailableDeletePassDoesNotFlood` covers the delete side).

The slot and the in-flight reserve are both held until the frame's **terminal outcome** — filed
or aborted — not released at the end of the stream, because the bytes occupy the volume through
verify and filing; releasing early would let a concurrent admission over-commit the reserve. The
release is idempotent and fires on every exit path (mismatch, cancel, I/O error, recovered
panic, successful filing), so it can never leak phantom bytes and wedge every pull it protects.

### 5.2 Streaming hash, byte count and source re-stat

The frame is streamed with the share's reader and `io.CopyBuffer` through a 512 KiB buffer, which
keeps smb1client on its pipelined read path; smaller reads fall back to a sequential path and
throughput drops sharply ([asiair-hardware-notes §9](2026-10-17-asiair-hardware-notes.md)). The
bytes go to a staging temp file and through SHA-256 in the same pass (`streamHash`). Whole-file
read helpers are never used: the stream is what is hashed, and no read path is trusted to return a
whole file (a silent truncation reports no error), so the streamed byte count is always checked
against the listing ([asiair-hardware-notes §9.8](2026-10-17-asiair-hardware-notes.md)).

After the stream:

- the byte count must equal the listed size, or the temp is discarded and the frame retried next
  cycle (`TestPullSizeMismatchDiscardsTemp`);
- a re-`Stat` of the **source** must still match the listing (size, mtime within ±2 s), or the
  file changed under the pull: the temp is discarded and the frame re-pulled next cycle.

An open failure that is a network error is transient; any other open failure marks the frame
`failed` (§4.3). A stream error discards the temp and retries.

The live transfer feed's pending set — every pull candidate of this cycle without a terminal
outcome — is ephemeral: it lives on the published rig status and is never persisted
([features §4](2026-10-17-features.md)).

### 5.3 The staging temp and the in-flight reserve

The staging temp lives under the staging root, which validation requires to be on the same volume
as the light and calibration roots ([architecture §7.5](2026-10-17-architecture.md)), so the
filing rename of §8.1 is always same-volume and atomic and there is no cross-volume copy path to
get wrong. The temp has a unique name and is removed by a `defer` on every exit path; after a
successful filing the rename has consumed it and the removal is a no-op. Nothing sweeps the
staging root at startup, so a temp orphaned by a hard crash or a power loss stays there, costing
space but never read as a frame. The temp is `fsync`ed and closed before the verify of §6.

A staging write, flush or close failure means the destination is refusing writes. The worker
records it, pauses pulls and alerts (rig-scoped), and settles the condition with a small write
probe before streaming anything again, so a broken volume is not re-proven by downloading a whole
frame to throw away. Frames are still counted while it stands, so an empty tally can never read
"drained" over frames that exist only on the device.

## 6. Verify the copy at rest

### 6.1 A cache-bypassing read-back

This step carries the product's core promise. After `fsync` and close, the temp is
re-opened with the page cache bypassed and hashed again; the result `diskHash` must equal
`streamHash`. The bypass is load-bearing: `fsync` flushes dirty pages to the controller but leaves
the just-written pages clean and resident, so an ordinary buffered read-back would be served from
memory and prove nothing about what the disk holds. Only a true device read catches a write
fault, a controller lie or a bad sector.

| Platform | Mechanism |
| --- | --- |
| Windows | `FILE_FLAG_NO_BUFFERING` |
| Linux (the container image, the integration tests) | `O_DIRECT`; where a filesystem rejects it (tmpfs), a normal read followed by `posix_fadvise(DONTNEED)` |
| macOS | `F_NOCACHE` |

A mismatch raises a critical `verify_mismatch` alert, discards the temp and re-pulls; the device
original is untouched (`TestPullCorruptAtRestNeverFiles`). Only a frame that passes is filed. The
same cache-bypassing read is used for every later NAS read that a decision rests on: the differ
branch, the idempotent keep, the delete-time re-verify, the vanished-source check. That is also
why grace is not the sole durability guarantee (§9.3).

The same pass tees the FITS data unit's digest (`data_sha256`, §11.3) off the bytes it already
reads, so the file is read whole exactly once.

### 6.2 Sector-aligned reads, exact-size hashing

`NO_BUFFERING` and `O_DIRECT` require the offset, buffer and length to be sector-aligned, and a
FITS file's length is not a sector multiple. `VerifyRead` therefore reads in aligned blocks and,
on the final block, reads a whole sector but feeds **exactly** the recorded size into the hasher,
discarding the padding past EOF; otherwise every file would either mismatch or hash padding and
false-pass. A file shorter than the expected size is `ErrShortFile`, a definitive corruption
distinct from a transient I/O error, so callers can alert on a genuinely lost frame instead of
retrying forever. Tests: `TestVerifyReadDiscardsTail`, `TestVerifyReadTailSizes`.

## 7. Classify

The FITS header is read from the **local** staging temp, never over SMB: nothing is skipped on
header content before the download (previews are dropped by extension), so a header read over
SMB would buy nothing. The reader is `internal/header`, which parses only the 2880-byte header
blocks and never decodes the data unit; it exists because `fitsio.Open` decodes the whole data
unit eagerly ([asiair-hardware-notes §11.1](2026-10-17-asiair-hardware-notes.md)).

The header produces the `Classification`
([routing-and-filing §1](2026-10-17-routing-and-filing.md)). A header that cannot be read, an
unknown `IMAGETYP`, or a missing routing-critical value sends the frame to `unclassified`: it is
filed to the rig's quarantine area and its device original is kept as the authoritative copy,
never made deletion-eligible by ordinary cleanup
([routing-and-filing §2](2026-10-17-routing-and-filing.md); `TestQuarantineUnclassifiedNeverDeletable`).
For a light frame, a missing `OBJECT` or `DATE-OBS` quarantines before a night is assigned, so a
missing value never pollutes `night_counters`. The embedded WCS is extracted here once and
carried into the row ([features §2](2026-10-17-features.md)).

A retargeted frame re-pulled from the device carries its original `OBJECT` card, so before
classification the filing step re-applies the operator's edit when the incoming bytes are the
same exposure the overlay describes
([routing-and-filing §9.4](2026-10-17-routing-and-filing.md); §11.11 for a revised frame).

## 8. Route and file

The router builds the destination directory and the candidate filename, the sanitized source
basename ([routing-and-filing §3–§5](2026-10-17-routing-and-filing.md)). ASIAIR filenames are not
unique: sequence counters reset every capture run, and two rigs — or two rigs sharing a camera
model, on the calibration path — emit identically named frames. A name clash is therefore a
routine event, and two rigs can reach the same calibration folder at the same instant from
separate goroutines.

Before claiming a name, filing checks whether the incoming file is a device-side revision of the
frame its row already describes (§11.4). If it is, the archive copy is superseded in place
(§11.7) and the rest of this section does not apply.

### 8.1 Claiming the name atomically

Filing never checks and then acts. It claims the name with an **atomic exclusive rename** of the
verified staging temp, a rename that fails when the target exists rather than replacing it:
`renameat2(RENAME_NOREPLACE)` on Linux, `renameatx_np(RENAME_EXCL)` on macOS, and `MoveFileEx`
without `MOVEFILE_REPLACE_EXISTING` on Windows. All three are atomic across goroutines and
processes and need no hard-link support, so the destination filesystem is unconstrained. Go's
`os.Rename` replaces, which is why it is not used. This is what makes filing race-free across rigs
without a lock (`TestCrossRigConcurrentFilingNoLoss`).

- **The rename succeeds**: the destination holds the full verified content and the temp is
  consumed.
- **The target exists**: if its size equals the incoming file's archive size, it is read back
  with the cache-bypassing `VerifyRead`. Identical content is an **idempotent keep** — a re-pull
  of a frame already proven on the NAS — and nothing is written; the streamed bytes are counted
  as wasted transfer. Different content goes to §8.2.

### 8.2 Deterministic disambiguation

Different content at a candidate name is either a genuinely distinct frame or a rotted copy; in
both cases the frame is filed under the next deterministic candidate:

1. the canonical name;
2. the canonical stem plus the `DATE-OBS` capture instant (`_YYYYMMDD-HHMMSS`, UTC), when the
   header carries one;
3. that stem plus an 8-hex-digit prefix of the file's SHA-256, lengthening by two digits until
   the name is free or an identical file is found.

The sequence always terminates: two files with the same full hash are byte-identical and resolve
as an idempotent keep, and any shorter clash only lengthens the prefix. The token derives only
from the frame, so a re-pull recomputes the same name and resolves idempotently, with no
duplicate proliferation. There is no `collision` state or tree: disambiguation always resolves a
clash, and every routing failure goes to `unclassified`. Test:
`TestFileDifferentContentSameNameBothKept`.

### 8.3 A canonical-name clash alerts

A different-content clash at the frame's **own canonical** name raises a `rot_clash` alert (warn)
while the frame files under a token. That is the signature of a rotted copy wearing the clean
name, which must be surfaced rather than filed away silently. A device-side revision of an
already-filed frame looks exactly like this from the outside, which is why §11 exists: a proven
revision supersedes and raises nothing, and only a genuine clash keeps the alert.

### 8.4 Recording the row, and the grace anchor

Filing upserts the frame row: the device anchors (`sha256` = the streamed bytes' hash, `size`,
`mtime`), the archive anchors (`archive_sha256`, `archive_size`) and the exposure identity
(`data_sha256`) of §11.1, the source, the destination, every classification field, the night,
the sidecars observed at listing, the embedded astrometry, and `state = verified`. Counts that
report frames dedupe by `dest_path` ([architecture §7.7](2026-10-17-architecture.md)).

`pulled_at` is the **grace anchor** (§9.1).

- A fresh rename sets it to the filing instant.
- An idempotent keep **preserves** a surviving row's `pulled_at`: a re-pull after a crash must
  not restart the grace clock, nor leave a filed frame unrecorded and so never deletable
  (`TestFileIdempotentRePullPreservesPulledAt`).
- Reclassify and retarget preserve it: they move the archive copy, and the device copy was not
  read again.
- A supersede **resets** it (§11.7). The rule above governs a re-pull of identical bytes, where
  nothing was read afresh; a revision is different bytes, freshly written by the device and
  freshly verified, and the clock exists so that a later device read is independent of the first.
- After a full database loss there is no row to preserve from, so grace restarts. That is
  benign: the frame is proven on both the NAS and the device, and only its drain is delayed by
  one grace window after the rare wipe.

A row that fails to persist after a verified filing is the worst outcome in the pipeline; it is
logged at Error and counted by its own metric. Filing then emits `frame.done` with the full row,
nudges the Tier-1 solver for a light without an embedded WCS, warms the preview cache and
auto-links the destination root ([library-reconciliation §8](2026-10-17-library-reconciliation.md)).

## 9. Delete pass

The delete pass runs after the pull and restore phases, in its own short session, as a bounded
**slice** (§12). Restore runs first in every cycle, so a queued restore's device original is never
destroyed in the same cycle. A rig with `keep_originals` set runs no delete pass. Every path that
removes a device or NAS file, and the gate on each, is enumerated in
[delete-safety §3](2026-10-17-delete-safety.md).

### 9.1 Eligibility and grace

A frame is deletion-eligible when it has a recorded `sha256`, its `pulled_at` is at least `grace`
old, and it is `verified` — or `unclassified` with a cull mark, which only the culled-original
drain may delete ([library-reconciliation §9](2026-10-17-library-reconciliation.md)). Ordinary
cleanup deletes `verified` frames only; an unclassified frame whose NAS copy is present keeps its
device original as the authoritative copy until the operator resolves it. The eligible set is
ordered by `(pulled_at, id)`.

`grace` is per rig, 1 h by default. A grace of zero is forbidden while deletion is enabled:
validation rejects it (`TestValidateGraceKeepOriginals` covers the keep-originals exemption), and
the delete pass refuses to run on a non-positive effective grace anyway, because a hand-edited
config file bypasses validation. Why zero is unsafe is §9.3.

Before any delete the slice confirms device identity on its own session (§2); a foreign or
unconfirmed device deletes nothing. Each frame then takes one admission grant (with no reserve),
so one frame and its recorded sidecars are deleted per grant. The row is re-read fresh before
acting, since a restore request or a dismissal may have arrived since the eligible set was read.

Per frame, in order, each check exiting before the costly device read where it can:

1. A removable Udisk share must present a readable media marker, so an absent or unmarked stick's
   empty listing is never read as mass deletions (H5, H10). This is a presence check, not an
   identity check; the swap backstop is the three-way check and re-stat below
   ([device-identity-and-media §6.4](2026-10-17-device-identity-and-media.md)).
2. `Stat` the source. Not found is vanished (§9.4); any other error defers the frame.
3. A source whose size or mtime no longer matches the row is deferred, within a bound, and held
   once the bound is spent (§11.13). Nothing is hashed at this gate.
4. `Stat` the NAS copy. Absent is a cull ([library-reconciliation §9](2026-10-17-library-reconciliation.md));
   any other error defers.
5. A NAS copy mid-swap under an open supersede intent defers (§11.9).
6. The three-way integrity check (§9.2).

### 9.2 The three-way integrity check

The device copy is streamed and hashed again in full (`deviceHash`), and the NAS copy is read back
through the same cache-bypassing `VerifyRead` as §6 (`nasHash`) — not the recorded digest, and not
a buffered read, which on a quiet NAS could be a page-cache hit that never touches the platter.
Deletion requires

- `deviceHash == sha256`, the device leg against the **device anchor**, and
- `nasHash == archive_sha256`, read at `archive_size`, the archive leg against the **archive
  anchor** (§11.1),

plus the source re-stat of §9.6 immediately before the delete. On a frame never revised or
retargeted the two anchors are one value and the check is one transitive proof that both copies
are byte-identical to what was filed. A genuine mismatch holds the frame with a critical
`delete_mismatch` alert and never deletes (`TestDeleteCorruptNASHoldsNoDelete`); a match deletes
(`TestDeleteThreeWayMatchDeletesFrameAndSidecar`).

An error is never a conclusion. A transient device or NAS read error defers the frame to a later
pass; it never holds it and never marks it deleted
([delete-safety §2.3](2026-10-17-delete-safety.md)). A Pause interrupts the device re-hash between
buffers and also defers (§12.8). On a remote site the device-side digest is measured by the
remote and compared here ([remote-sites §7](2026-10-17-remote-sites.md)).

### 9.3 Why grace matters to the second device read

The check proves two different things, and they relate to grace differently.

- **A durably good copy exists on the NAS.** The cache-bypassing NAS re-hash proves it, as the
  verify at rest did before it. This is grace-independent: the bypass forces a true disk read
  however recently the file was written. It alone is what makes deletion safe in the "never lose
  a frame" sense: nothing is deleted unless a byte-verified copy is on the NAS.
- **The device's stored bytes match that copy**, which catches a transient read fault at pull
  time. That is what the second device read is for, and it depends on grace, because SMB1 offers
  no way to bypass the ASIAIR's own Linux page cache. If the delete ran seconds after the pull,
  the device would serve the re-read from RAM and faithfully reproduce a transient fault rather
  than re-read the eMMC: the tie-breaker would degenerate into re-reading the first read. Grace,
  or enough intervening capture I/O to evict those pages, is what makes the second device read
  independent.

So zero grace is not claimed safe for the second property, and a capacity rig's grace is a floor,
not zero. The residual, a byte-stable read fault that reproduces identically on both device reads,
is undetectable by design: the device is the sole source of truth
([architecture §12](2026-10-17-architecture.md), [§16](2026-10-17-architecture.md)).

### 9.4 A vanished source

A source that has vanished since filing (deleted on the device, stick pulled) is usually benign,
but only after a fresh proof that the NAS copy is still good: the NAS copy is read back with
`VerifyRead` and must match `archive_sha256`.

- It matches: the frame is marked `deleted`, recorded as a vanished source rather than a device
  delete (`TestDeleteSourceVanishedGoodNASDeletes`).
- It is present but mismatching: a genuine integrity problem; a critical `frame_lost` alert is
  raised and the row is left as it is.
- It is missing or truncated as well — a doubly-vanished frame, removed from both sides — which
  is the operator's cull carried through to the device: `frame_lost` at warn, and the row reaches
  terminal `deleted` so it is not re-probed every pass
  ([library-reconciliation I23](2026-10-17-library-reconciliation.md);
  `TestDeleteDoublyVanishedConverges`).
- A transient read error defers.

"Vanished" means a successful `Stat` reports the file absent. A read or list error is never
treated as vanished: that would strand a still-present file as `deleted` on a flaky or returning
medium.

### 9.5 A vanished NAS copy

An **absent** NAS copy with the device copy still present is the operator's cull. It is never
restored by the delete pass; it is reconciled by the rules of
[library-reconciliation](2026-10-17-library-reconciliation.md): the culled-original drain may
delete the device original once its settle window, root confirmation and every other
disqualifier clear and the device copy still hashes to `sha256`, and a restore happens only on the
operator's explicit request, serviced by the restore phase before any delete.

A **truncated** NAS copy (present, shorter than `archive_size`) is corruption, not a cull, and is
the one case the delete pass repairs: it re-pulls and re-verifies the frame from its
still-resident device original, replacing the short file, before any delete is considered
(`restoreNAS`). Where the copy is shared by several rows, the restore streams whichever sharing
row's device copy answers the archive expectation (§11.6). A restore whose device content no
longer matches what was filed holds the frame instead.

A NAS copy that is present but fails the hash is a genuine mismatch and is held (§9.2).

### 9.6 Removing the frame and its sidecars

The device delete re-stats the source one final time immediately before removing it. The hashes
above take seconds, during which a counter-reset capture could overwrite this exact path with a
never-verified frame; a source that no longer matches is not deleted, raises nothing, and is left
to the pull path's differ branch (`TestDeleteSourceOverwrittenBetweenVerifyAndDeleteIsNotLost`).
A source that vanished in that window is recorded deleted with nothing removed.

After the frame, the delete removes **only the sidecars recorded for it** whose extension is in
`delete_sidecar_extensions`, each re-`Stat`ed against its recorded size and mtime (±2 s) first, so a
sidecar replaced by a different file at the same name is left alone. The AppleDouble twin
(`._<name>`) of each file removed is swept with it; an orphan twin whose parent Commander never
owned is left alone (`TestDeleteSweepsAppleDoubleTwin`, `TestDeleteLeavesOrphanAppleDoubleAlone`).
Sidecar and twin failures never fail the frame's deletion.

A device delete that fails — a file still held open by a capture fails at once with a sharing
violation — does not escalate or force anything: the frame stays `verified` (already safe on the
NAS), a warn alert is raised, and it is retried on a later pass. A successful delete marks the row
`deleted`, emits `frame.deleted`, and records the removed path for the listing-idle discount of
§12.4.

### 9.7 Delete configuration guards

- Validation refuses a `delete_sidecar_extensions` entry that is a science-frame extension
  (`fit`, `fits`, `xisf`) or that is not also in `exclude_extensions`, so a mistyped `.fit` there
  can never delete science frames.
- Validation refuses a non-positive effective grace on a deleting rig, and the delete pass refuses
  one at run time (§9.1).
- A rig with `keep_originals` set skips the delete pass and the orphan-preview sweep entirely.
- A disabled share is never mounted, cleaned or restored by the delete and restore passes
  ([device-identity-and-media §8](2026-10-17-device-identity-and-media.md), D1).

## 10. Disconnect, and per-frame recovery

Each phase's session — pull, restore, each delete slice — is closed when the phase ends: unmount,
logoff, close. No session idles between cycles, and the pull and delete sessions never overlap.
The clock phase runs after the last session has closed and after every device-time comparison of
the cycle, so stepping the device clock cannot invalidate one mid-cycle
([features §6](2026-10-17-features.md)).

Each cycle runs under a `recover()`, and so does each frame's transfer. A malformed header or an
unexpected header value turns **that frame** into a `failed` row and an alert, never a panic
that takes down the multi-rig service (`TestPerFileRecoverYieldsFailedRow`). The transfer's own
defers (admission release, temp removal) run as the panic unwinds. Every other worker entry point
that mutates frames outside the cycle — reclassify, dismiss — carries its own recover for the same
reason (`TestReclassifyPanicDoesNotKillWorker`). A recovered cycle panic sets the rig to Error with
the panic text.

Throughout the cycle the worker publishes its status and raises alerts through the hub and the
notifier ([architecture §10](2026-10-17-architecture.md)).

## 11. Device-side frame revision

The ASIAIR rewrites a light frame **on the device after the file is already complete and
readable**, injecting the plate-solve WCS it has since computed into the FITS header (H13). On an
ASI6200MM that overflows the primary header from two 2880-byte blocks to three, so the file grows
by exactly 2880 bytes and every data byte shifts. On another camera the WCS may fit in the
existing blocks and the size stays the same. Commander may have pulled and filed the frame before
the revision; its next pass then sees different content at a path it has already filed.

Without a model for "the same exposure, revised in place", three things go wrong, all from that
one gap:

1. **A duplicate frame and a false rot alarm.** The revision is filed as a token-suffixed second
   copy with a `rot_clash` alert, the plain copy is orphaned in the ledger, and stacking the
   folder double-counts the exposure.
2. **A retargeted frame silently loses its re-label.** The overlay's re-apply test sees content
   that does not match and resets the overlay, so the `OBJECT` edit reverts.
3. **A culled frame silently returns.** A revision that changes the size falls through the cull
   gate of §4.1, so a frame the operator deleted on the NAS is re-pulled and re-filed.

The design rests on these premises. The ASIAIR is the actor; the evidence is that the size delta is
exactly one FITS header block and that the revised frames are exactly the ones carrying no WCS when
first pulled. That is an inference from byte counts and headers, not an instrumented measurement
(asiair-hardware-notes H13). There is no way to ask the device whether a frame is final: the control
channel exposes no such call. The revised copy is the one worth keeping, because it carries the
astrometry. Two files for one exposure is wrong, not merely untidy. Fixing it in Commander is
correct; waiting for a firmware change is not. The margin arithmetic of §4.4 and §11.13 assumes the
shipped cadence defaults (45 s poll, 300 s `min_file_age`). Commander handles it automatically,
because every user meets the behaviour and none should have to know a setting exists.

The response has three pieces, and none is an alternative to another:

- a **discriminator** separating a revision of an exposure already pulled from a genuinely
  different exposure at the same path (§11.4);
- on a proven revision, **one file survives**, carrying the final bytes, with the ledger
  following and no alert (§11.5–§11.9); a genuine clash keeps the clash path of §8.2–§8.3;
- the **300 s quiescence default** of §4.4, so the pull usually lands after the device has
  finished. The gate makes revisions rarer; the discriminator makes the outcome correct when one
  is seen anyway, which it will be.

### 11.1 The columns, keyed on the copy they describe

Every column describing a copy is keyed on which copy it describes. The device copy and the
archive copy are one value only while the archive copy is a byte-for-byte image of the device
copy, which a revision falsifies, and which a row sharing its archive file with another row breaks
at once. This is the home of the frame-row column semantics; [architecture
§7.7](2026-10-17-architecture.md) lists the tables.

| Group | Columns | What they hold |
| --- | --- | --- |
| Device | `sha256`, `size`, `mtime` | The device file's whole-file hash, length and modification time as last proven. A supersede advances them; only device-side checks read them. `sha256` is the **current device anchor**, not an immutable one. |
| Archive | `archive_sha256`, `archive_size` | What the archive copy must hash to, and the length every archive-side comparison uses: `VerifyRead`'s length, the idempotent-keep size test, the retarget faithfulness checks. |
| Neither | `data_sha256` | The exposure's identity, side-independent and fixed for the exposure's life (§11.3). |
| Overlay | `retarget_sha256`, `retarget_from` | The audit record of a retarget: that a re-label happened and what it was. Not an archive-side expectation ([routing-and-filing §9.2](2026-10-17-routing-and-filing.md)). |
| Lifecycle | `state`, `pulled_at`, `delete_defer_count` | Where the row stands, when its grace clock last started, and how many delete passes have deferred on a changed source in the current episode (§11.13). They describe the row's progress, not either copy. |

`archiveHash(f)` is a field read of `archive_sha256`. There is no fallback from an empty archive
column to `sha256`, and no `COALESCE` bridge: a missed write site leaves the column empty and
turns a good archive copy into a critical `delete_mismatch`, which a store-level test catches
loudly (AC18) instead of letting it degrade silently. A row whose archive hash is empty is never
superseded (§11.4). The store migration seeds a row whose archive columns are empty:
`archive_sha256` to `retarget_sha256` where an overlay exists, else `sha256`, and `archive_size`
to `size` (AC19).

**Any path that writes a shared archive file commits every row sharing that `dest_path`.** The
archive side moves with the file, so a path that changes the file and commits one row leaves every
other row anchored to bytes the file no longer holds. Rig-local sharers are reached through
`FramesByDestPath`; a sharer on **another rig's** ledger is out of that accessor's reach, so the
write is refused instead (`CountUndeletedFramesByDestAnyRig`), the frame takes the clash path, and
both ledgers stay consistent (AC23). The refusal is the design, not a gap in the accessor: another
rig's rows are mutated only by that rig's worker goroutine (architecture §3.6), so widening
`FramesByDestPath` to every rig would break the single-writer rule on a delete-adjacent path. This
is the same policy re-file uses for the same hazard ([routing-and-filing
§8.7](2026-10-17-routing-and-filing.md)). It is a guard rather than a mechanism because reachability
is low: a `dest_path` shared across rigs needs the idempotent keep to adopt a byte-identical file,
and two sensors do not produce identical bytes.

A `deleted` row is not a sharer for any of this — neither for the cross-rig refusal nor for the
restore's candidate list (§11.6): its device original is gone and its row is terminal, so it has
no live claim on the file's content. Counting one would refuse every supersede at a `dest_path`
whose two-share pair has had one half drained. It would also close the restore's diverged-adopt
arm and let the tombstone's unreadable device copy stand in for a genuine refusal, turning a
satisfiable restore into an hourly re-stream of the whole device copy with an alert, forever: a
device that never drains, by another route. The residual is a cross-rig tombstone anchored to pre-revision bytes,
which cannot become a `delete_mismatch` because a tombstone never re-enters the delete pass; the
two readers that touch a deleted row's archive anchors, the auto-link sampler and a retarget of a
deleted frame, both refuse non-destructively (`TestSupersedeIgnoresACrossRigTombstone`).

Two rows sharing one `dest_path` is the H12 two-share case (§3.4). What the delete-time check
proves for such a sharer after a supersede is less than for any other row: its device copy and the
shared archive file differ by a header block, so the check becomes two independent comparisons,
each against the value recorded for the copy it read, rather than one transitive proof of byte
identity. The guarantee for that row is that each copy is what the ledger recorded for it and that
both carry the same exposure. That narrower guarantee is accepted for one artifact — a second row
sharing a revised file — whose own archive expectation does match the file in front of it.

One path outside this section does not honour the commit rule: the retarget operation removes
the old archive copy without a sharer check. That gap is recorded with retargeting
([routing-and-filing §9.8](2026-10-17-routing-and-filing.md)).

### 11.2 What each write path advances

The matrix decides the columns of §11.1 for every path that writes them; the sections below carry
each path's reasoning. Other columns of the row belong to the doc that owns them. **Advances**
writes the value the path has just proven; **preserved** is a value the path deliberately carries
through a write that would otherwise clear it; **reset** returns the counter to zero; **untouched**
is not written.

| Write path | `sha256` | `size` | `mtime` | `data_sha256` | `archive_sha256` | `archive_size` | `retarget_sha256` | `retarget_from` | `state` | `pulled_at` | `delete_defer_count` |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Supersede, the revised row | advances | advances | advances | preserved | advances | advances | untouched | untouched | advances | advances | reset |
| Supersede, a `dest_path` sharer | untouched | untouched | untouched | untouched | advances | advances | advances | untouched | untouched | untouched | untouched |
| Retarget re-apply, the revised row | advances | advances | advances | preserved | advances | advances | advances | untouched | advances | advances | reset |
| Culled targeted write | advances | advances | advances | advances | untouched | untouched | untouched | untouched | untouched | untouched | reset |
| Restore, the row whose device copy is read | advances | advances | advances | preserved | advances | advances | advances | untouched | advances | advances | reset |
| Restore, a `dest_path` sharer | untouched | untouched | untouched | untouched | advances | advances | advances | untouched | untouched | untouched | untouched |
| Pre-delete gate deferral | untouched | untouched | untouched | untouched | untouched | untouched | untouched | untouched | untouched | untouched | advances |
| `data_sha256` backfill | untouched | untouched | untouched | advances | untouched | untouched | untouched | untouched | untouched | untouched | untouched |
| Crash recovery (supersede intent replay) | advances | advances | advances | preserved | advances | advances | as recovered | untouched | advances | advances | reset |

Crash recovery replays a commit and decides nothing of its own; **as recovered** is whatever the
commit being recovered advances in that column. `retarget_sha256` advances only on a row that
already carries an overlay, so a frame never retargeted never gains one.

A sharer's `state` and `pulled_at` stay untouched because a supersede changes only what that row
records about the archive copy: its own device copy was not read, its grace clock guards that
untouched original, and its state describes its own situation. A sharer left `held` does not
drain, and the supersede does not clear that: it did not cause the hold and cannot discharge it,
and clearing it would assert a proof nobody made. The restore's sharer stays untouched on the same
terms; whether a repair discharges another row's hold depends on why that row was held, and the
restore of the standing request already clears it once the archive verifies.

A restore's `pulled_at` advances because a restore writes an archive copy from device bytes read
once, so grace restarts before that original becomes deletable. `retarget_sha256` advances
wherever the edited file is rewritten, because an audit record that silently stops matching the
file it describes invites reading it as the archive expectation, conflating the device and archive
anchors. Advancing it
is safe because `archiveHash` reads `archive_sha256`, so the live expectation and the audit
record are independent values.

### 11.3 data_sha256: an exposure identity that survives header edits

A FITS file is a header of whole 2880-byte blocks terminated by an `END` card, followed by the data
unit. The ASIAIR's revision edits only the header, so the data unit is the stable identity of an
exposure. Read noise makes two exposures with identical pixels vanishingly improbable, not
impossible; such a collision is the one case in which this design could overwrite one exposure
with another.

`data_sha256` is the SHA-256 of the file from its data offset to EOF. Filing records it beside
`sha256`, and `UpsertFrame` preserves a recorded value rather than overwriting it with an empty
one, so a re-file arriving without a digest never nulls a recorded identity (AC12).

The rule binds the **consumers** of the digest, not every producer of a `sha256`. There are five:
the incoming staging temp at filing (leg 1 of §11.7), the archive copy for leg 3, the retarget
re-apply's comparison, the culled-original drain's device re-hash (which carries the backfill's
device-side proof, §11.8), and the restore's device stream. None makes a second whole-file pass
(AC10):

- The two device-side consumers hold the bytes as they flow and scan the stream for the `END`
  card as it passes.
- The three archive-side consumers read the header first through `internal/header` and hand the
  offset to `VerifyReadTee`, which tees the second digest off the same cache-bypassing pass
  `VerifyRead` makes. That is two opens of the file — a header pre-read and one whole-file pass —
  and the rule forbids only a second **whole-file** pass.
- Nothing is cached or carried between the sides: mid-supersede the two copies' header block
  counts differ, so each consumer finds the data offset in the copy it reads.
- `localfs` stays format-blind: its tee is handed an offset and parses nothing, and the FITS
  offset scan lives in `internal/header` alone.
- A header that does not parse yields the whole-file digest and an **empty** data-unit digest,
  never an error. An empty digest fails every identity test by construction, the safe direction,
  and keeps a rotted archive copy's `frame_lost` critical rather than turning it into an I/O
  deferral.

Leg 1 is teed off the staging temp's **at-rest** verify (§6.1), not the SMB stream that wrote it:
the two are distinct reads reconciled only by `diskHash == streamHash`, and leg 1 is one leg of
the only authorization for a destructive overwrite, so it comes from the read that proves what the
disk holds.

### 11.4 Recognising a revision

A listed frame is a **device-side revision** of a frame already filed when both hold:

- its bytes differ from the row's `sha256`, **and**
- its data unit hashes to the row's `data_sha256`.

Anything else is a different exposure at a reused path, and every ordinary behaviour applies.

One precondition stands in front of **every identity test** — these two legs, the three-way
agreement of §11.7 and the re-apply disjunction of §11.11: the data unit must be **non-empty and
parseable** (`IsUsableDataDigest`). A legal FITS file can carry no data unit, and every such file's
data unit hashes to the digest of the empty input, so between two genuinely different header-only
frames every identity test would agree deterministically. The probabilistic argument of §11.3 does
not reach a deterministic case, so a zero-length data unit is refused as an identity wherever
identity is tested (AC6).

Nothing in the discriminator turns on the file's size. The +2880-byte growth is one camera's
behaviour, so the pull path's revision trigger is a size **or** mtime change, and a same-size
revision is recognised (AC2).

A row with no recorded `data_sha256` cannot answer leg 2 from its record. For such a row the
filing step establishes leg 2 from the archive copy's own leg-3 read, once that copy has proven
itself the archived bytes (§11.8), and records the value whether or not this filing turns out to be
a revision. Leg 1 and the precondition are tested first, so an ordinary re-pull of such a row
never pays that read.

### 11.5 The outcome, by what the row owes

A **proven revision** on the pull path is decided by what the row owes: whether there is an
archive copy and whether it verifies against `archive_sha256`; whether the row carries an overlay
to re-apply; and whether it carries reconciliation marks an ordinary filing would clear. Lifecycle
state is not a further axis: `held`, `dismissed` and `deleted` are projections of the first and
third and land in the cells below. A standing restore request is resolved by the restore path
(§11.6). Anything that is not a proven revision takes the clash path, with an overlay resetting
alongside it because the content genuinely differs.

| Archive copy | Overlay | Reconciliation marks | Outcome |
| --- | --- | --- | --- |
| Verifies | no | none | **Supersede** (§11.7): the archive copy comes to hold the revised bytes at its name, through the four-step claim (§11.9); the anchors advance per §11.2, sharers included; the astrometry is re-extracted (§11.10); no alert. A `held` row whose copy verifies lands here: it is not a lost frame. |
| Verifies | yes | none | **Re-apply, then supersede**: the overlay is re-applied to the incoming file, and the archive anchors are taken from the edited bytes (§11.11). |
| Absent | either | culled | **Targeted write** (§11.12): nothing is filed. Culled and retargeted is this cell. A culled row whose copy has reappeared is not: it verifies, and its marks clear as on any filing. |
| Present but drifted | either | either | Not a proven revision: leg 3 cannot be established against drifted bytes, so the frame takes the clash path, the right outcome for a copy that has genuinely rotted. |
| Absent | either | none | Nothing to supersede: a `dismissed` row is re-pulled, its copy having been purged on purpose, and a copy that merely vanished reaches `frame_lost` through the differ branch. |

### 11.6 Where a stale row is first observed

Six sites independently notice "the source no longer matches the row". They are decided
together so the six answers stay consistent.

| Site | What it sees | Behaviour | How the frame reaches a resolver |
| --- | --- | --- | --- |
| Cull skip gate (§4.1) | size only | Skips a same-size source, so a card-reader swap or a FAT mtime shift cannot resurrect a cull. A size-changing revision falls through; the same-size case is §11.12's limitation. | The filing step, the only place holding the content. |
| Differ branch (§4.2) | size and mtime, plus the archive copy on the `verified`/`unclassified`/`held` arm | Proves the old row's fate. The `held` arm verifies its copy rather than concluding loss from the flag (§4.2 says why); `failed` still concludes from its state. | It decides whether the frame is pulled; filing then resolves it. |
| Pre-delete gate (§9.1 step 3) | size and mtime | Defers with a WARN, hashing nothing, within a row-persisted bound; holds once it is spent (§11.13). | A later cycle's pull. |
| Culled-original drain | device content | Holds rather than delete a culled frame's original whose content changed. Unreachable for a revised source, because the pre-delete gate fires first; reached by a culled row whose source is not revised, which is the read the backfill's device-side proof rides (§11.8). | Not applicable: the pre-delete gate has already deferred. |
| Restore (`restoreNAS`) | device content | Applies the discriminator to the streamed device bytes against the row's `data_sha256`, so a proven revision restores from the revised bytes instead of holding with `HoldRestoreMismatch`. Both copies then hold those bytes, so the row commits per §11.2: a half-commit would leave the row two header blocks short of both files, every later `VerifyRead` hashing a truncated prefix. The restore still clears `missing_seen_at`, `missing_acked_at` and `restore_requested_at` on success. | The restore is the resolver, writing the archive copy under §11.1's commit rule. |
| Quiescence (§4.4) | listing metadata only | Gates the pull on `min_file_age` and `settle_cycles`. | It delays the pull until the device has usually finished. |

Where a `dest_path` is shared, a restore puts back the bytes the archive expectation names, so the
device copy it streams is **whichever sharing row's** hashes to `archive_sha256` — after a
supersede, the revised row's, not necessarily the row whose restore was requested. Where no
sharing row's device copy satisfies it, the restore is refused and the frame held (AC24). This adds
no mechanism: two rows sharing one archive file have one correct content for it, and
`archive_sha256` names it.

A restore whose file landed but whose commit failed leaves the archive copy at the revised length
while the row records the pre-revision one; each later pass reads that as inconclusive and defers.
That arm has its own bound, `maxRestoreInconclusive` (3 passes, an hour apart), held in memory. The
difference from the pre-delete gate's persisted counter is deliberate: there the bound is the only
signal the deferral leaves, so losing it to a restart would lose the escalation entirely; here it
is a second signal, the commit failure having already alerted when it happened, so a restart costs
a follow-up rather than the notification. That is the whole of the distinction: if the restore's arm
ever stops alerting on the way in, its counter must be persisted on the row as well.

### 11.7 Superseding the archived copy

The archive tracks the device. The device's copy is the authoritative one, and an archive that
deliberately held a known-stale version would be a thing every future reader had to be told about.

Replacing a proven archive copy is the one destructive act in this section, so it is authorized
only by a **three-way agreement on the data unit, established in the same cycle**:

1. the incoming file's data unit, teed off the staging temp's at-rest verify;
2. the row's recorded `data_sha256`;
3. a cache-bypassing re-read of the existing archive copy, which must first hash to
   `archive_sha256` at `archive_size`, and whose data unit must then match.

Plus one structural gate: **a valid FITS file is never replaced by one that does not parse**
(AC5). It needs no separate test, because the digest of a header that does not parse is empty and
the precondition of §11.4 refuses it. There is no converse rule: Commander does not repair a
corrupt archive copy from a device copy (§11.14).

Any leg that fails to establish, for any reason including a transient read error, leaves the frame
on the clash path, which is the drifted row of §11.5. The fallback is always the existing
behaviour, never a weaker check, and never a weaker substitute for leg 2.

The supersede is also refused, and the frame takes the clash path, when the row names no archive
file or records no archive hash, when the proven archive anchors are empty, when a cross-rig sharer
exists (§11.1), or when the row carries an overlay that was not re-applied to the incoming file
(§11.11).

The commit advances the columns of §11.2 and the rows of §11.1's commit rule; what lives here is
why they fall that way. The two sides no longer move together. The device side moves with the
device copy, while a sharer's device copy was never touched and still hashes to its own `sha256`,
so advancing it there would falsify that row's anchor. `data_sha256` is side-independent and
identical across sharers by construction, since they share a file only because their content
matched. The **revised** row, if `held`, lands `verified`: a completed supersede has proven exactly
what the hold was raised to doubt, and the delete pass admits only `verified` (or culled
`unclassified`) rows, so a row left `held` would never drain. Every other state is carried through:
a proof discharges a suspicion without un-deleting a frame or promoting a quarantined one.

A supersede's `pulled_at` is the supersede instant, so the grace clock restarts (AC21). Grace is the
window after a frame is verified on the archive before its device original may be deleted, a
different clock from the quiescence gate, and it exists so a second device read is independent of
the first; a superseded archive copy has just been read once. The idempotent-keep rule of §8.4
governs a crash re-pull of identical bytes, so the two do not contradict.

Committing the revised row alone would leave a sharer anchored to pre-revision bytes in a file that
holds revised ones: its next delete pass would fail the archive leg and hold it with a permanent
critical `delete_mismatch` no pull can clear. Filing a revision at a fresh path, as the clash path
does, never reaches that state, which is why the sharer commit is part of the supersede itself
(AC1, AC9).

### 11.8 Backfilling data_sha256 for rows without one

Rows with no recorded `data_sha256` carry no exposure identity, so leg 2 cannot be established
for them from the record, and a row already held by a revision would raise a critical `frame_lost`
on its next listing. A low-priority cursor records the value for them, bound by three rules:

- **A row is backfilled only from a copy proven to be the archived bytes.** Either side can supply
  the proof: an archive copy that still hashes to `archive_sha256`, or a device copy that still
  hashes to `sha256` and is therefore byte-identical to what was archived. The cursor reads only
  archive copies and opens no device session. The device source is what lets a culled row, which
  has no archive copy, be backfilled at all, and it rides the culled-original drain's own device
  re-hash at no extra read — so it reaches only the rows that drain passes through. Without the
  condition, a rotted file's data unit would be recorded as authoritative and the rot blessed
  permanently (AC11).
- **The job runs on a budget, not a population argument.** A `keep_originals` rig never deletes,
  so every row it has filed still has a live original, and only `deleted` rows are pruned, so no
  population bound exists. Unbudgeted, the job is the whole library read as uncached `O_DIRECT`
  passes against the volume live pulls are writing to. The cursor paces itself by duty cycle: after each cache-bypassing
  whole-file read it sleeps long enough that reading takes at most a tenth of the wall-clock,
  whatever the volume's speed. The Tier-0 astrometry backfill's pacing does not transfer: it is
  calibrated for 2880-byte header reads.
- **Rows held by a revision, with their device copy present, come first**, then every other row
  with an archive copy, then nothing else proactively. It walks by an advancing id cursor, so a row
  whose archive copy is unreadable is left without a value rather than re-fetched forever.

A row needed before the cursor reaches it has the value computed on demand under the same proof
(§11.4), so nothing depends on the cursor having finished. That also covers a culled row whose
device copy is later re-adopted.

### 11.9 Claiming the name without ever holding only one copy

No atomic replace is available: `RenameNoReplace` cannot replace by construction, and SMB replace
is not atomic. The supersede is therefore four steps, each a no-replace rename or an unlink, so the
bytes exist under some name at every instant:

1. rename the existing archive copy aside, to a reserved sibling name derived from its own content
   hash (`<name>.superseding-<hash prefix>`, which carries no FITS extension, so no frame scan
   counts it). The file renamed is the one the row describes, `row.dest_path`, never the
   candidate name filing would claim for a fresh frame;
2. `RenameNoReplace` the verified staging temp into the name so freed;
3. commit the rows, per §11.1 and §11.7, under an uncancellable context, so a cancel arriving here
   cannot drive a routine cancellation through the rollback and undo a swap whose bytes are in
   place;
4. unlink the copy set aside.

A **persisted supersede intent** is written before step 1 and cleared after step 4, carrying every
column the commit advances, the overlay included. Recovery runs in `Manager.Start` before any worker
starts, beside re-file intent recovery ([architecture §7.11](2026-10-17-architecture.md)), and it
needs the intent's payload because no device session exists then and `mtime` is the device file's
mtime, which the archive copy cannot supply. An open intent is a deferral reason in the delete
pass, in **both** windows the swap opens:

- between steps 1 and 2 the canonical path is **absent**, the shape a re-file also leaves, and the
  absent-copy branch's defer reasons cover it;
- between steps 2 and 3 the canonical path is **present**, holding bytes no row is anchored to yet,
  a shape a re-file never produces. A row sharing that `dest_path`, whose own device copy the swap
  did not touch, would pass the pre-delete gate, reach the three-way check and earn a critical
  `delete_mismatch` on a good copy. The present-copy branch therefore defers on an open intent too,
  before the device hash is spent. Reaching that window takes a crash **and** a recovery that
  leaves the intent open, since the swap and the delete pass run on one goroutine within a process;
  it is guarded regardless, the cost being one map lookup against a critical alert on a good copy.

That deferral is what closes the crash window by construction rather than after the fact.

A swap that fails before its commit rolls back: the installed revised copy, if any, is removed (it
is a copy of a device original that is still on the device), and the copy set aside is renamed back.
A rollback that itself fails leaves the intent open, which keeps the frame out of the delete pass
until startup recovery resolves it. A commit that finds no row (pruned mid-swap) does not roll back:
the file in place is what any committed sharer describes. A failed step-4 unlink leaves the intent
open, and recovery's both-present arm removes the leftover.

Recovery reads the swap's progress off the reserved sibling, whose name is unambiguous because it
derives from the content hash of the copy set aside:

| On disk | Meaning | Recovery |
| --- | --- | --- |
| Sibling absent | Step 1 never ran, or step 4 ran (so step 3 had committed). Both are consistent. | Drop the intent; write nothing. |
| Sibling present, canonical absent | Crashed between steps 1 and 2. | Rename the sibling back into place. |
| Both present | Step 2 completed. | If the canonical file's length equals the intent's `archive_size` (a `Stat`, no read), unlink the sibling and re-commit; otherwise leave the intent open and log per start, with no alert. |

Recovery commits exactly as step 3 does, matching the revised row on its `dest_path` as well as its
id, so a row re-filed elsewhere since the intent was written commits nothing rather than acquiring
anchors for a file it no longer names. The length check closes the one residual of the both-present
arm, a file at the canonical name that step 2 did not install, which needs something outside
Commander writing that path inside the crash window. The rename-back arm needs no such guard: it can
only put a pre-revision copy back at a name the row has stopped using, a stale orphan rather than a
bad anchor. Tests: `TestSupersedeCrashAtEveryStepRecovers`,
`TestSupersedeInFlightDefersASharerWhoseCopyIsPresent`, `TestSupersedeRollsBackAFailedSwap`,
`TestSupersedeRecoveryRefusesAnUnexpectedCanonicalFile` (AC13).

### 11.10 Astrometry on a superseded frame

The revised file carries the WCS the device embedded, so a supersede re-runs the Tier-0 header
extraction and records it ([features §2](2026-10-17-features.md)). `UpsertFrame` cannot deliver
that: its `wcs_status` write is sticky, so a Tier-1 `solving`, `solved`, `failed` or `unsolvable`
verdict survives a re-file and would shadow the header the device has since written. The supersede
calls `ClearFrameAstrometry` — the store's sanctioned reset for a genuine content change — and then
writes the extracted astrometry (AC17). The write is unconditional, so on this path the clear is not
what makes the header verdict win; it guards any commit that goes through `UpsertFrame`, and a
re-extraction whose own write fails, where a cleared row is re-solved and an uncleared one keeps a
verdict about bytes that are gone. Every other re-file path keeps its own stickiness, and a
superseded light still without a WCS is a Tier-1 candidate like any other.

A **restore** that puts back revised bytes writes no astrometry column, so a Tier-1 `failed` or
`unsolvable` verdict recorded against the pre-revision file survives a restore of the revised one.
The verdict misdescribes the file's header and suppresses a solve that would succeed, though the
exposure and its pointing are unchanged. That is a recorded residual: clearing it would make the
restore a second astrometry writer, and the path that re-extracts is the column's one owner.

### 11.11 The other two consequences

**The retarget re-apply test** decides whether freshly streamed device bytes are the frame whose
`OBJECT` card was edited. It is a disjunction: `dataHash == prior.data_sha256` **or**
`streamHash == prior.sha256` — the same exposure, whatever its header says. The first arm is what
the rule means; the second is kept because a byte-identical re-pull is the same exposure by
definition, and because a row the backfill has not reached has an empty `data_sha256` that no
incoming file matches. The precondition of §11.4 governs the first arm: an unset value is not a
recorded digest of an empty data unit, and two header-only frames never match on the shared empty
digest (AC6), which would otherwise re-apply the overlay to a file it does not describe and leave
the frame deferred forever.

The re-apply's faithfulness proof is three checks, and all three are **re-anchored to the file in
hand** without being relaxed: the claim they prove is that the edit changed exactly the `OBJECT`
card and left the science data untouched, the authorization chain the retarget invariants protect
([routing-and-filing §9.9](2026-10-17-routing-and-filing.md), INV-4).

| Check | Anchored to |
| --- | --- |
| The edit's own assertion (`assertRetargetEdit`): sizes and the complement digests of every byte outside the 80-byte `OBJECT` window | the **incoming** file's size |
| The rewritten temp's at-rest verify length | the incoming file's size |
| The rewritten temp's at-rest verify hash | the digest the rewriter computed over every byte it wrote (`OutputSHA256`) |

The third keeps its read-back, the only check on that path that the durable file holds what the
rewriter produced; replacing it with a data-unit comparison would prove nothing the complement
check does not (§11.14). The row then commits with the archive side taken from the **edited** file
— those are the bytes that land on the archive — and the device side from the streamed bytes:
`sha256` is always the hash of the bytes just pulled off the device. Committing the pre-revision
device hash would leave `deviceHash != sha256` on every delete pass, a permanent critical
`delete_mismatch` (AC16).

One row falls outside the disjunction. A retargeted frame with no recorded `data_sha256` has
no identity, so the first arm cannot speak, and a revision changes its bytes, so the second cannot
either. The re-apply declines, and the supersede is then **refused**: installing the device's
unedited bytes under an overlay claiming an edit they do not carry would create a false audit record
beyond later detection, every anchor agreeing while the row's target and the file's `OBJECT`
disagreed for good. The frame is instead filed as genuinely new content: the overlay resets, the
re-label reverts, and the frame files under its original target, routed from the unedited header.
The copy carrying the edit is left on the NAS with no row naming it. Nothing alarms: the
destination is a different folder, so the claim never meets a name collision and no `rot_clash`
fires; the only signal is a WARN ("not superseding the archive copy; filing as a clash instead").
The orphan is invisible to the missing-frames scan, which walks rows to files and not the reverse
(`TestRetargetedFrameWithNoRecordedIdentityIsNotSuperseded`). The backfill bounds how long this
window stays open — once it records the row's `data_sha256`, the first arm holds and a revision
supersedes with the re-label intact (`TestBackfilledRetargetedFrameThenSupersedes`) — but it does
not repair a frame already caught in it. Reaching the case needs a retargeted row with no recorded
`data_sha256` and a revision arriving before the backfill reaches that row; that narrowness is why
it is accepted as a residual and recorded rather than left to be discovered.

### 11.12 A revision of a culled frame

`UpsertFrame` clears `missing_seen_at`, `restore_requested_at` and `missing_acked_at` on every
re-file, so an ordinary filing would bring a culled frame back. Leaving the row alone is no better:
the cull gate is keyed on size, so an untouched row never matches the revised device file again —
the frame is pulled in full every poll interval forever, pins pending work so the rig never reads
Safe, and the pre-delete gate holds it with a critical alert instead of draining it.

`decide` sees only the listing, so the revision is recognised at filing, where the content is in
hand. There is no archive copy to protect, so legs 1 and 2 are the whole proof. A **targeted write**
then advances the device anchors and `data_sha256` (§11.2), preserves the three reconciliation
marks, and leaves the astrometry and archive columns untouched: no archive copy changed, so the
archive expectation must not move, which keeps a file the operator later restores by hand from
raising a `delete_mismatch` against a good copy, and keeps a culled and retargeted frame from becoming
a permanent pin. Nothing is filed, because the operator deleted
that exposure and the device rewriting its header does not un-delete it. With the anchors advanced
the size gate matches again on the next cycle, and the culled-original drain can delete the device
copy because `deviceHash == sha256` once more (AC7). If the copy's presence cannot be determined,
the frame is deferred rather than filed back onto a NAS that may still hold it. The outcome is
terminal and files nothing, so the frame leaves the live feed's pending set and is counted as
`culled_revision` rather than as a failed transfer.

Two residuals are recorded rather than designed away:

- **A same-size revision of a culled frame** never passes the cull gate, so the targeted write never
  happens; the pre-delete gate sees the mtime move, defers for its bound and then holds with the
  critical `delete_mismatch`. The symptom is one held frame with that alert and a device original
  that does not drain: loud, and recoverable by re-adopting the frame. Widening the gate to mtime is
  rejected (§11.14). A culled row with a **standing restore request** does resolve: the restore
  reads content whatever the gate does and completes from the revised bytes (AC20), and so does any
  revision arriving inside `min_file_age`, the restore carrying no age gate — both only once the row
  carries `data_sha256`.
- **A culled row with no recorded `data_sha256` whose device original the drain does not delete** — one
  inside `reconcile_settle`, one held by a restore request, one on an unconfirmed root, or any row on
  a `keep_originals` rig — is never reached by the device-side proof, which rides only the drain's
  own read. If its device copy is revised, leg 2 can never be established: the frame is pulled,
  fails to prove itself a revision, and is filed as the different exposure it has come to look like,
  the marks clearing. The operator sees a frame they deleted on the NAS come back once: it is filed
  as a different exposure, the same outcome as any different exposure at a reused path.

### 11.13 The delete pass and a late revision

The delete pass runs every cycle regardless of quiescence, while the pull that supersedes waits out
`min_file_age`, and grace (1 h by default) does not rule out a revision arriving after it. If the
pre-delete gate held on the first sighting, a late revision would earn a hold, a critical
`delete_mismatch`, and then a critical `frame_lost` for a frame whose archive copy is intact. So the
gate **defers**, with a WARN and the `source_changed_deferred` metric, and hashes nothing: it sits
before the ~100 MB device re-hash precisely so a frame still being revised never re-streams its copy
every pass. The proving belongs to the pull path, the only place holding both copies (AC14).

The deferral is bounded by `delete_defer_count`, **persisted on the row** and advanced in SQL so a
reset the pull committed in the meantime is never written back. A frame whose source is still
changed after `maxDeleteDefers` (20) passes has stopped looking like one mid-revision, and deferring
it silently forever would leave its original undrainable, so the gate then falls back to the hold
and the critical `delete_mismatch` — at the bound and on no earlier pass. The counter survives a
restart, and every path that brings the source back into agreement with the row resets it, so the
bound is per episode of disagreement rather than per row lifetime (AC15): without the reset, a row
that spent its budget on one revision would hold on the first sighting of the next one, the outcome
the deferral exists to prevent. There is no second alert class and no configuration key.

The gate evaluates no discriminator, so the deferral delays a genuine clash too. A counter-reset
overwrite that the delete pass reaches after grace, before the pull has filed it, defers with a WARN
like a revision. Unless the pull resolves the frame first (§4.2), the gate's own critical
`delete_mismatch` then arrives at the bound rather than on the first sighting, up to `maxDeleteDefers`
passes later, with only WARNs before it. That is the price of keeping the alert off a settling revision,
and it is why `source_changed_deferred` counts what the gate observed, a changed source, and never why
it changed.

The bound is a count of passes and the wait it outlasts is a duration. At the shipped 45 s poll and
300 s age the pull can attempt the supersede after about nine passes, and 20 doubles that. A per-rig
`min_file_age` override raised past `20 × poll_interval` exhausts the budget before the pull can
resolve the revision and earns the hold the deferral exists to prevent; `docs/metrics.md` tells an
operator to weigh a sustained deferral rate against `min_file_age` for the same reason. A bound
derived from the two knobs would be better than a constant if the constant ever stops being
generous.

`source_changed_deferred` is its own `delete_blocked_total` reason rather than folded into the
source-changed one, because of cardinality before diagnosis: that reason fires at most once per
frame, where the deferral fires once per pass up to its bound, so one deferring frame summed into it
would read as a score of benign races.

The three-way check itself is the ordinary one: by the time it runs on a superseded frame, both
anchors describe the revised bytes.

### 11.14 Rejected alternatives

| Alternative | Why rejected |
| --- | --- |
| `DATE-OBS` as the discriminator | The capture instant is metadata, and the premise is that the device rewrites metadata. Content is the only honest identity. |
| Hash a prefix of the data unit | Cheaper only if the read were not already happening. A false "same" overwrites one exposure with another, and prefix collisions stop being negligible exactly where the leading rows are not sky: a masked or overscan border, a clipped flat. |
| Suppress the duplicate and keep the filed copy | Leaves the archive deliberately stale and forces the delete check to prove data-unit rather than byte equality on every revised frame, weakening "verification has no off switch". |
| Suppress with no other change | Every revised frame then fails the pre-delete gate, is held and never deleted; the device stops draining and the verdict goes Unknown. |
| Strengthen the quiescence gate alone | No gate value removes the race; a rarer silent data problem is worse than a common one, because nobody watches for it. |
| Raise `settle_cycles` with `min_file_age` | Inert at the shipped cadence (§4.4), and listed because it looks like an obvious improvement and is not one. |
| Replace the file with an atomic replace | No such primitive exists here, and an in-place rename window lets the library scan see the path missing and, after the settle window, delete the device original on a false cull premise. |
| File the revision under a token and remove the original | The safest ordering, but the survivor is token-suffixed, the outcome this design exists to remove. |
| Repair an invalid archive copy from a valid device copy | Commander does not repair archive files: it would paper over genuine rot, which `rot_clash` exists to surface. |
| Widen the cull gate from size to size-or-mtime | Catches the same-size revision, but a card-reader swap or FAT mtime shift would then resurrect culls, a commoner case. The narrower harm is accepted (§11.12). |
| Hold the deferral count in memory | It resets on restart, so a rig restarting more often than the bound never escalates and the frame defers forever. |
| Make the pull path the sole resolver, every gate only deferring | Re-opens the cull gate's size keying and costs a full device pull on every spurious FAT mtime shift. The six sites keep their own gates and share §11.6. |
| Wait for a firmware change | The behaviour is undocumented, long-standing, and costs frames and disk meanwhile. |
| Leave `localfs` untouched, taking the data digest from a second pass or a buffered read | A second whole-file pass is what AC10 forbids, and a buffered read discards the cache bypass that makes leg 3 a proof. |
| Bind every producer of a `sha256` rather than the consumers | Pushes FITS structure into the byte-level hash every `VerifyRead` shares, and makes an unparseable header an I/O failure everywhere, turning a rotted copy's critical `frame_lost` into an indefinite deferral. |
| Replace the third faithfulness check with a data-unit comparison | The complement check already covers every byte outside the `OBJECT` window, data unit included; it would remove the only at-rest read-back on that path, which is what stops a write fault minting the safety anchor from corrupted bytes (INV-4). |
| A `COALESCE` bridge inside `archiveHash` | A missed write site would degrade silently, re-welding the two sides. The store-level test fails loudly instead. |
| Evaluate the discriminator at the pre-delete gate | Streams the whole file at a gate that costs nothing, and mints the very alert the deferral exists to keep off a settling frame. |
| Key the four-step claim off the candidate name | A row's `dest_path` can be a token-suffixed name while candidate 0 is a different frame's file; keying off the candidate would orphan the file the row, and its overlay, describe. |
| Close the crash window with a scan that alerts afterwards | Closes it only after the fact; on a delete-adjacent path an alert is no substitute for the bytes being where the row says. |
| Gate only lights arriving with no WCS more strictly | The gate sees only the listing; frame type and WCS presence come from the header after the pull, and a per-candidate header read over SMB is machinery the pull path does not have. |
| Bulk de-duplication of token-suffixed revision pairs | A cleanup that wants per-file operator confirmation, not a pull-path rule; a bulk file-removal tool on a delete-adjacent path widens what can delete the wrong file. It is out of scope. |

### 11.15 Acceptance criteria

The criteria keep their numbers; tests cite them as "design frame-lifecycle AC*n*".

1. Replaying a frame filed and then re-listed 2880 bytes larger with its data unit unchanged
   produces **one** file at the canonical name holding the revised bytes, no `rot_clash`, and a row
   whose `sha256`, `data_sha256`, `size` and `mtime` match the revised device file and whose
   `archive_sha256` and `archive_size` match the bytes on the NAS. A second row sharing that
   `dest_path` (the H12 case) ends with its archive anchors advanced and its device anchors
   untouched, and drains without a `delete_mismatch`. Tests: `TestDeviceRevisionSupersedesTheArchiveCopy`,
   `TestDeviceRevisionCommitsEveryDestPathSharer`, `TestDeviceRevisionOverRealSMB` (integration).
2. A revision that changes the bytes and the mtime but **not** the size is recognised, superseded
   and raises no `rot_clash`. This alone carries the claim that the discriminator does not turn on
   size. The same-size revision of a culled frame is §11.12's limitation, not this criterion's.
   Test: `TestSameSizeRevisionIsRecognised`.
3. A genuinely different exposure at the same path still produces the token-suffixed copy, keeps
   both frames and raises `rot_clash`. Test:
   `TestFileDifferentContentSameNameBothKept`.
4. A revision whose data unit does **not** match falls through to the clash path; nothing is
   overwritten. Test: `TestDifferentExposureAtTheSamePathStillClashes`.
5. A file that does not parse as FITS never replaces one that does, in any ordering. Test:
   `TestUnparseableFileNeverReplacesAValidOne`.
6. Two frames whose data units are both zero-length are never the same exposure: no supersede, and
   the incoming frame takes the clash path. Both files are built through `internal/header/fitstest`
   and carried through the real pull, classify and filing path. The same precondition holds on the
   re-apply disjunction: a retargeted row whose `data_sha256` is the empty-input digest does not
   match a second header-only frame, so no overlay is re-applied and the frame is filed. Test:
   `TestZeroLengthDataUnitIsNeverAnIdentity`.
7. A revision of a culled frame leaves `missing_seen_at`, `missing_acked_at` and
   `restore_requested_at` standing, files nothing, advances the device anchors and leaves
   `archive_sha256` and `archive_size` untouched. Two further cycles make zero further pulls, the
   culled-original drain then deletes the device copy, and the frame leaves the pending set with no
   `file_failed` outcome. Tests: `TestCulledFrameRevisionAdvancesAnchorsAndFilesNothing`,
   `TestCulledFrameRevisionStopsRepullingAndDrains`.
8. A retargeted frame the device then revises keeps its `OBJECT` overlay and destination folder, is
   filed rather than deferred, and ends with `archive_sha256` and `retarget_sha256` matching the
   bytes on the NAS; the faithfulness check still rejects an edit touching any byte outside the
   `OBJECT` card. Tests: `TestRetargetedFrameSurvivesARevision`,
   `TestRevisionReapplyRefusesACorruptingWriter`.
9. A retargeted frame sharing a `dest_path` with a frame the device revises ends with **both** rows'
   `retarget_sha256` describing the bytes the shared file holds, and neither row is held on its next
   delete pass. The two-row state is built through the real filing path's idempotent keep. Test:
   `TestSupersedeAdvancesASharersOverlay`.
10. No consumer of a data unit makes a second whole-file pass; the leg-3 header pre-read does not
    count. The archive-side tee keeps the cache bypass, `localfs` has no format knowledge, and an
    unparseable header through the tee returns the whole-file digest with an empty data digest rather
    than an error. Tests: `TestDataUnitDigestsCostNoSecondWholeFilePass`,
    `TestOnDemandArmCostsOneWholeFilePass`, `TestTeeCostsOneWholeFilePass`,
    `TestTeeUnparseableHeaderNeverFails`, `TestPackageHasNoFormatKnowledge`,
    `TestFakeVerifyReadTeeRidesTheSameRead`.
11. The backfill records `data_sha256` only from a copy that still hashes to its own anchor: a rotted
    archive copy leaves the row without a value; a culled row the drain is about to delete gets its
    value from the drain's own device read; a culled row the same pass defers (inside
    `reconcile_settle`, or on a `keep_originals` rig) gets none from any session opened on its behalf.
    A held row with its device original present is backfilled first, then supersedes on the next
    cycle instead of raising `frame_lost`, landing `verified`. A held row with `missing_seen_at == 0`
    and an intact archive copy raises no alert through the differ branch. Tests:
    `TestBackfillRefusesARottedArchiveCopy`, `TestBackfillTakesHeldRowsFirst`,
    `TestCulledDrainSuppliesTheExposureIdentity`,
    `TestCulledRowTheSamePassDefersGetsNoExposureIdentity`,
    `TestBackfilledHeldRowSupersedesInsteadOfAlerting`,
    `TestHeldRowWithAnIntactArchiveCopyIsNotALostFrame`.
12. A re-file of a row that carries `data_sha256`, through a path whose incoming frame has none,
    leaves the recorded value intact. Regressing this nulls the identity silently. Tests:
    `TestUpsertFramePreservesDataSHA256`, `TestRetargetPreservesRecordedDataSHA256`.
13. Crash injection at each step of the four-step claim leaves recoverable state, and recovery
    restores the canonical file before any worker starts. While an intent is open the delete pass
    defers the frame in **both** the absent-copy and the present-copy branch. No injection point
    yields a critical `frame_lost` or `delete_mismatch`, and a crash during a supersede of a
    retargeted frame recovers with `retarget_sha256` matching the committed file. Tests:
    `TestSupersedeCrashAtEveryStepRecovers`, `TestSupersedeInFlightDefersASharerWhoseCopyIsPresent`,
    `TestSupersedeRecoveryReAnchorsARetargetedFrame`.
14. A revision arriving after grace — the delete pass reaching the frame before the pull supersedes
    it — defers with no alert and no device hash at that gate, on every pass within the bound; the
    frame supersedes on a later cycle and then passes the three-way check, the device leg against
    `sha256` and the archive leg against `archive_sha256`. Test: `TestLateRevisionDefersTheDeletePass`.
15. A frame whose source stays changed past the bound is held with a critical `delete_mismatch` at
    the bound and on no earlier pass. The count survives a manager restart, and a second revision
    after a supersede gets the whole bound again. Tests: `TestChangedSourcePastTheBoundHolds`,
    `TestDeferralsSurviveARestart`, `TestTheBudgetIsPerEpisodeOfDisagreement`.
16. A retargeted frame the device then revises ends with `sha256` equal to the **revised** device
    bytes, so its next delete pass passes the three-way check. Test:
    `TestRevisedRetargetedFrameAnchorsToTheRevisedDeviceBytes`.
17. A supersede leaves `wcs_status` reflecting the revised file's own header: a seeded Tier-1
    `failed` verdict is replaced by the header verdict. The criterion asserts that outcome, not the
    `ClearFrameAstrometry` call. Test: `TestSupersedeReExtractsTheAstrometry`.
18. Every filing path — filing, retarget and restore — leaves `archive_sha256` non-empty and
    `archive_size` non-zero. Test: `TestEveryFilingWriterSetsTheArchiveAnchors`.
19. The store migration seeds a row whose archive columns are empty: `archive_sha256` =
    `retarget_sha256` where an overlay exists, else `sha256`; `archive_size` = `size`; and a delete
    pass over seeded rows of both shapes holds none of them. The unseeded rows are built through the
    store. Test: `TestDayZeroMigrationSeedsTheArchiveAnchors`.
20. A restore request on a culled frame the device has since revised, at an unshared `dest_path`,
    completes: the restore writes the revised bytes, the row's anchors advance on both sides (`size`,
    `mtime`, `pulled_at`, and `retarget_sha256` where an overlay exists), and no
    `HoldRestoreMismatch` is raised. Tests: `TestRestoreOfARevisedCulledFrameWritesTheRevisedBytes`,
    `TestRestoreOfARevisedRetargetedFrameAdvancesTheOverlay`.
21. A superseded frame's `pulled_at` is the supersede instant: its device original is not deletable
    until grace has elapsed from the supersede. Test: `TestSupersedeRestartsTheGraceClock`.
22. The shipped `min_file_age` is 300 s: a frame whose offset-corrected mtime is younger is not a
    pull candidate, and becomes one once older. Tests:
    `TestShippedMinFileAgeHoldsAFrameForFiveMinutes`, `TestShippedMinFileAgeDefersARealPull`.
23. A supersede whose `dest_path` is named by a row on another rig's ledger is refused: the frame
    takes the clash path, both ledgers stay consistent, and neither raises a `delete_mismatch`. The
    two-rig state is built through the real filing path. Tests:
    `TestFilingRefusesToSupersedeACrossRigSharer`, `TestSupersedeRefusesACrossRigSharer`.
24. A restore requested on a sharer whose `dest_path` was superseded restores the revised bytes,
    streamed from the sharing row whose device copy hashes to `archive_sha256`, leaves both rows'
    archive anchors matching the file with the un-read row's device anchors untouched, and raises no
    `HoldRestoreMismatch`. A restore no sharing row can satisfy is refused. Tests:
    `TestRestoreOfASharerStreamsTheRowThatAnswersTheArchiveExpectation`,
    `TestRestoreIsRefusedWhenNoSharingRowAnswers`.
25. A destination root holding this installation's frames auto-links even when a sampled frame has
    been superseded: the sampler verifies against `archive_sha256` at `archive_size`. A wrong
    expectation here fails silently — a correctly mounted root is simply never linked. Test:
    `TestAutoLinkFromExistingDataVerifiesTheArchiveAnchors`.
26. The race-detector, integration-tag vet and integration suites cover these criteria.

Four rules have no numbered criterion: the backfill's budget value (the rule is that a budget
exists); the `min_file_age` help text on the Settings and Devices surfaces, which says a frame can be
complete and still be revised by the device; the four-step claim keying off `row.dest_path` rather
than the candidate name, which diverge only in a state no criterion builds; and the restore clearing
its marks on the revision arm, which is the restore's ordinary behaviour (§11.6).

## 12. Delete-pass slicing

Every delete streams the whole device original for the three-way check and re-reads the NAS copy,
so one delete costs as much device-link time as one copy. Run to completion, a large backlog on a
slow link would hold the cycle for its whole length: nothing listed, nothing pulled, and every
status the listing writes — "Last listed", "Waiting to settle", the cadence prediction, per-share
free space, the device-low and capacity alerts, the Safe verdict — frozen. Simply deferring deletes
behind pulls is no answer either, because a full device stops capture. Slicing never removes,
samples or caches the device re-hash to save link time: every delete keeps its full-stream device
hash and its full NAS `VerifyRead`.

So each cycle's delete pass is a **slice** bounded by a deadline anchored on the cycle's listing,
and the cycle continues after 1 s while a backlog remains. The slice decides only **when it stops**
and **which eligible row it starts from**. `deleteOne`'s decisions, the three-way check, the identity
and intent guards and every store query are those of §9; stopping early is always safe, because a
frame not visited is not deleted and stays eligible. There is no configuration knob. Tests cite this
section's slice properties and invariants doc-qualified, as "design frame-lifecycle I1" or "design
frame-lifecycle §12.9 invariant 4".

The goal is bounded staleness. While a backlog drains, consecutive listing starts are at most
`max(poll, listing + pull + restore time)` plus one frame operation plus 1 s apart when the device
has room, and at most the listing, pull and restore time plus `2 × poll` plus one frame operation
plus 1 s when it is tight (§12.2). The skip budget (§12.3) can add one more `poll` to either bound.
One frame operation is one `deleteOne`, including a re-pull of a truncated NAS copy. The slice
prologue — session open, the `scan_air` identity probe (at most 3 s on the local site), one marker read per root, two intent
queries — runs after the deadline and is not counted.

Slicing bounds the delete pass, not the pull phase: a burst of many frames still blocks the listing
for as long as copying them takes, with the pill reading Transferring and the current file set, which
is why both bounds include pull time. Bounding the pull phase is a separate problem. When the device is
tight, deletes get at least as much link time as pulls up to the `2 × poll` cap; once every cycle's pull
phase exceeds `2 × poll`, deletes get `2 × poll` per cycle and the debt (§12.1) grows without a ceiling
until the device has room or a slice exhausts its list.

### 12.1 The deadline

`runCycle` computes the deadline after the restore phase and before the slice:

- **Device has room**: `pullStart + poll` — the next listing is due one poll after this one started.
  It can already be in the past when the pull phase alone overran `poll`; the progress floor
  (§12.3) still lets the slice do one frame's work. The anchor is the `pullStart` `runCycle` stamps
  immediately before the pull phase, on every online cycle.
- **Device is tight**: `sliceStart + min(max(poll, deleteDebt), 2 × poll)`, where this cycle's
  listing, pull and restore time accrues to `deleteDebt` before the slice and the slice's length is
  repaid after it (floor 0).

Every frame's space is freed only after two full device reads, the pull and the delete re-hash.
When the link cannot carry twice the capture rate the device fills under any schedule, and the
freeing rate is maximised by splitting link time about 1:1 between pulls and deletes. The debt
carries that split across cycles, so one long pull phase is repaid by several slices of at most
`2 × poll`, each followed by a listing, rather than by one slice as long as the pull. The debt is a
worker field, owned by the worker goroutine and not persisted. It is cleared only when the device
stops being tight or a slice exhausts its list; a prologue return, a suspend, a context end or an
unavailable destination leaves it owed, so a missed identity probe or a Pause does not forgive the
fair share. Completing a sweep (§12.5) is not exhaustion and does not clear it, since a suspend after
the wrap index also completes one.

### 12.2 Device delete pressure

The device is **tight** when its tightest share has less than `2 × disk_reserve_bytes` free, or when
no share's free space could be measured — a device that cannot be measured must not be scheduled as
if it had room. The reserve is where `device_disk_low` fires, so deletes get their fair share before
that point rather than from it. The value is refreshed at every listing and logged at Info when it
changes ("device delete pressure changed", with the tightest free space and whether the reason is
`tight` or `unknown`), so a permanently tight rig is one log line rather than silence. It is a
scheduling bias only; no safety decision reads it.

Two cases are permanently tight by construction: a small eMMC or TF share that never has `2 ×
reserve` free, and a rig whose every enabled share fails `Statfs`. Both get the fair-share slice
every cycle; the `2 × poll` cap bounds the cost. On a multi-share rig the pressure is measured on
the tightest share, but the slice deletes in `(pulled_at, id)` order from every share, not first
from the tight one. That is a known limit rather than a decision against tight-share-first ordering;
such an ordering reorders delete-path work and would need its own delete-path review.

### 12.3 One slice

A slice is one `deletePhase` call. Its prologue is §9.1's: the grace floor and a fresh
`DeletionEligible` cutoff, the session, identity confirmation, the per-root marker verdicts, the
settle window, and one read each of the open re-file and supersede intent sets; a read error on
either set skips the slice (INV-M2). Every prologue return reports no progress and no yield, so a
broken session or an unconfirmed identity falls back to poll cadence and never spins at 1 s. An empty
eligible list is a whole sweep by itself (§12.6) and leaves no debt.

The loop, per frame of the rotated eligible list, in this order:

1. serve queued commands (`drainCommands`);
2. stop on a context end, a suspend, or an unavailable destination;
3. skip a row on a disabled share — never mounted, never counted toward the floor;
4. if the deadline has passed **and** either one streamed visit has happened or the skip budget is
   spent, record a yield and return `more = true`;
5. move the cursor to this frame, publish the cleanup progress, and run `deleteOne`.

The order is load-bearing. Suspend and context come first, so Pause keeps its meaning. The
disabled-share skip comes before the deadline, so a slice left holding only disabled-share rows never
reports `more`. The deadline comes before `deleteOne`, so a slice stops only between frames, never
between a frame's hash and its delete.

**The progress floor.** A **streamed visit** is a `deleteOne` call that reached the device re-hash,
directly or through the culled-original drain, whether the hash then matched, mismatched or failed.
Cheap exits — a changed source, an open intent, an unconfirmed medium, the absent-copy defer reasons —
cost almost no link time, so past the deadline the slice keeps visiting past them until one streamed
visit has happened, the list is exhausted, or its **skip budget** is spent: one poll interval past
the later of the deadline and the loop's start. Without the budget, a run of cheap-deferring rows
after the cursor would let each slice yield having done no delete work; without the floor, a pull
phase that overran `poll` would leave every slice with zero deletes.

**Slice property I1.** *A slice that starts with rows ahead of the cursor advances the cursor.* It
either streams one frame or spends its poll of skipping moving past cheap exits, so a later slice
reaches the streamable rows. A budget yield has `streamed == 0`, `more = true`, and logs
`streamed=0`. Test:
`TestDeleteFloorWhenPullOverruns`.

`deleteOne` and the culled-original drain report what they did — whether they streamed the device
original, and the listing key of the source they removed — and nothing else about them changes. A
source that vanished after verification is not recorded as removed, because Commander removed
nothing.

### 12.4 Own-delete-aware idle signature

"Safe to power off" requires the rig to have been idle a full cycle: the listing signature unchanged
since the previous listing (§14.1). With slicing, every listing after a slice differs from the one
before by the files the slice deleted, so without a correction the card would read "Working — don't
power off" for the whole drain while every frame is already on the NAS.

The worker keeps the previous listing's frames beside its signature, and the set of `(share, path)`
keys it has deleted since that listing. When the listing is complete and the set is non-empty, the
idle test compares against the previous listing **minus those keys**:

- **Exact keys.** Removal matches the exact `(share, path)`, never a prefix, so `a.fit` never removes
  `a.fits`.
- **Any other change reads Working.** Size and mtime still come from the previous listing, so a fresh
  capture, a rewrite of a deleted path, an external deletion or a changed size or mtime still differ.
- **Other Commander deletions are not discounted.** Dismissal, the orphan sweep, sidecars and
  AppleDouble twins are not recorded; if one shows in the signature the verdict stays Working, the
  conservative direction.
- **Partial listings** compare against the raw previous signature.

The set means "since the previous listing", so it is cleared at every listing, complete or not. A
listing that differs only by files Commander removed after proving them on the NAS carries the same
evidence of a quiet device as an unchanged listing, so a drain on an otherwise quiet device keeps
reading Yes. It does not raise confidence beyond that. Test: `TestSafeVerdictHoldsThroughDrain`.

### 12.5 The round-robin cursor

The cursor is the `(pulled_at, id)` of the last frame a slice attempted, in `DeletionEligible`'s own
order. Each slice starts at the first row strictly after it, wrapping to the head; a zero cursor
starts at the head, a cursor past the end wraps, and a cursor naming a row that has left the list
resolves to its successor. An exhausted slice resets the cursor to zero. The cursor is a worker field,
not persisted: a restart starts again at the head.

The rotation is what makes the deadline safe. Without it, a head frame that hashes in full but
never concludes — a transient `VerifyRead` error, a source changed just before the delete, a sharing
violation, an unclassified row whose NAS copy is present — would use up every slice's single
guaranteed visit, and the tail would never be reached. With it, every eligible frame is visited within
N consecutive slices. Order has no safety role, because each `deleteOne` is self-contained. Tests:
`TestRotateAfter`, `TestDeleteCursorNoHeadStarvation`.

The rotation also reports the **wrap index**, where the original head row lands in the rotated list.
A slice completes a **sweep** when it visits a row at or past the wrap index — counted on the visit,
just before the cursor moves, not on merely reaching the index, which would count one boundary twice
when a slice yields at the wrap without visiting — or when it iterates its whole list. The remaining
imprecision only undercounts, which can delay a drain's or a run's end but never make one premature.

### 12.6 Scheduling and end-of-cycle status

When a slice yields with eligible frames left, `runCycle` returns `minPollInterval` (1 s) instead of
`poll`. A yield happens only once the deadline has passed, and every deadline is at least
`pullStart + poll`, so the next listing is already due: consecutive listing starts are never closer
than `poll_interval`, which keeps the settle cadence and `maxDeleteDefers` meaningful (§12.9,
invariants 5 and 7). The 1 s gap is when `Run`'s select serves queued retarget, re-file, link and
wake requests, usually within one slice: `Run`'s select draws them against the ready 1 s ticker,
and Go picks among ready cases at random, so each lost draw delays them by one more cycle (dismiss
and restore requests are also served between frames). A command that runs no cycle (a config
reload, reclassify, Stop run, a Run now during a run) leaves a pending continuation in place rather
than pushing it out to a full poll.

At the end of every online cycle, a refused or suspended one included, `NextPollAt` is stamped from
the delay actually returned, after the slice rather than before it; `PollIntervalSec` keeps the
configured poll, which the share-disable guard reads as the cadence for its freshness check.

**Slice property I2, nothing more to do.** *Since the last copy or delete, a whole sweep has run and
a cycle has listed in full and attempted a pull.* "A whole sweep since an event" means the sweep
count has advanced by two, the first boundary only closing the sweep that held the event. A copy, or
the start of a run, with the cursor at the head counts as held by the sweep that already closed, so
a run with nothing to do ends at its first sweep boundary rather than one poll later; this can only
bring an end forward to where I2 already holds, never before every frame was tried. A run until idle
ends when I2 holds. Test: `TestRunNowWhilePausedRunsUntilIdle`.

#### The listing's pill

The pill reads "Transferring" (`Pulling`) only while a transfer is in progress or at least one frame
is ready to copy — settled, not held, not failed, admitted, and not filed by the end of the walk. The
walk keeps the pill it inherited, and `Pulling` is set when a transfer actually starts. Frames that
are only settling, held or failed leave the listing at "Monitoring" (`Idle`), and the
waiting-to-settle line and the alerts carry them. Pending work is unchanged for every other
reader, so settling, held and failed frames still keep Safe at Working. A frame the walk never
reached, because the destination became unreadable or a share's mount dropped, is not counted, so
the pill reads "Monitoring" while the destination alert says why nothing moves. The listing records the pill
state it set, and a slice's end restores that state, not the one it saw at slice start: a Pause and a
Resume served before the slice must not reinstate "Paused" on an unpaused rig. The restore undoes the
slice's own "Cleaning up" and any stale "Paused" on an unpaused rig; a Pause that still holds, or an
Error written mid-slice, survives. A Resume landing mid-slice republishes "Cleaning up" at once, but
only when the pill reads "Paused", so a Resume never overwrites any other state written mid-slice. A
Resume landing outside a slice with no cycle following it at once (nothing eligible,
`keep_originals`, a missed identity probe) restores the listing's pill straight away rather than
reading "Paused" until the next listing; this restore applies only while the rig is online, so a
Resume on a rig last seen offline does not show the stale listing's "Monitoring" before its own cycle
has probed the device. Test: `TestListingPillReadsReadyWork`.

#### Draining indication

The UI cannot tell a drain from an idle poll, so the published status carries a draining indication,
set at the end of a cycle whose slice yielded and cleared by every cycle that runs no yielding slice
(drained, paused, offline, identity error, `keep_originals`) and at once by Pause and Stop run. While
it is set the Dashboard and Devices surfaces hide the "next check in Xs" ring; the pill still shows the
listing's state. The cleanup readout ("Checking, then removing from the device · N of M") counts
across the whole drain: it shows while a slice runs and is hidden between slices, its numbers carry
from one slice to the next, a frame becoming eligible mid-drain raises the total, and rows on disabled
shares are not counted. A drain ends when a whole sweep deletes nothing, so frames stuck deferring do
not hold the count open; the next drain counts from 1. The count is not persisted. Tests:
`TestDrainingIndication`, `TestDrainCountResets`.

#### Run now while paused runs until idle

On an unpaused rig, Run now means "check now": the next cycle starts at once. On a paused rig it sets
a **run-until-idle** flag rather than running one long cycle, so each cycle runs the same pull, restore
and sliced delete path as an unpaused rig, with the same 1 s continuation. The rig is *suspended* when
it is paused with no run in progress; the gates that decide whether a cycle works read "suspended".
During the run the pill follows the cycle and the Safe verdict is computed from each listing, while the
published `paused` stays true. The library scan and the discovery-wake gate keep reading "paused", so a
paused run lists, pulls and deletes but does not scan the library.

The run ends when I2 holds, or after 3 consecutive cycles without progress (`maxNoProgressCycles`, a
code constant). Run now re-bases the run's progress record and resets its no-progress count when it
starts a run, so neither I2 nor the count can carry over from before the run began. A copy, a cursor
advance (behind a failed listing, only a copy or a delete) or the start of a run resets that count; a skip-budget yield advances the cursor and so counts as progress, which
I1 guarantees. On a `keep_originals` rig the delete side counts as a sweep every cycle, so idle means
nothing is ready to copy. Settling, held and failed frames do not keep a run going.

#### Run state

Commander tracks two things per rig. The **current** state is what the worker publishes (Transferring,
Cleaning up, Monitoring, Paused, Running until idle — the run label taking precedence while the flag is
set). The **requested** state is what the operator last asked for (running, paused, or run until idle),
recorded by the hub the instant a command is queued, by a conditional set under the hub's lock so it
follows the command queue rather than a mirror of the worker. The pill shows the current state and adds
a pending note while the two differ: "Cleaning up · pausing", "Paused · starting run", "Paused ·
resuming", "Running until idle · resuming", "Running until idle · stopping".

| Run state | paused | run flag | requested | Pill |
| --- | --- | --- | --- | --- |
| suspended | true | clear | paused | "Paused" |
| running | true | set | run until idle | "Running until idle" |
| stopping | true | set | paused | "Running until idle · stopping" |

| Command | unpaused | suspended | running or stopping |
| --- | --- | --- | --- |
| Run now | an immediate cycle; no note | sets the run flag: running | no change and no immediate cycle: the pending ticker deadline stays in place, so repeated presses never start listings closer than the run's own cadence (§12.9, invariants 5 and 7); the request returns to run until idle, so a queued Stop run followed by Run now leaves the run going |
| Stop run | ignored | no effect | stopping until applied at the next frame boundary; then the run ends |
| Pause | pauses | no effect beyond pausing | stopping until applied; then pausing ends the run |
| Resume | as usual | unpauses, then an immediate cycle | clears the flag and unpauses, then an immediate cycle |

| Run end | When | State after | Safe verdict after |
| --- | --- | --- | --- |
| exhausted | I2 holds, or the third cycle without progress | `Paused` | the run's last listing's verdict, labelled "as of" that listing (§14.4) |
| Stop run | applied at the next frame boundary; the in-flight frame finishes, Stop run not setting §12.8's `pauseRequested` | `Paused` | as exhausted; a Stop run applied during the pull phase suspends the rest of that listing, so the verdict is Unknown/"paused" |
| Pause | applied | `Paused` | Unknown/"paused" |
| Resume | applied | the immediate cycle's listing state | computed as on any unpaused rig |
| offline cycle | the probe fails | `Paused` | Unknown/"offline, during the last run" |
| identity error | a foreign device answers | `Error` | Unknown/"identity" |

Every end clears the run flag. The exhausted, Stop run and offline ends apply the same transition as
a pause: the cadence prediction, the waiting-to-settle count and the draining indication are
cleared, while the Safe verdict is the one the table gives. A slice returning `more == false` is not
by itself idle: a prologue return, a suspend or context end, an unavailable destination and a
skipped slice return it too, so idle is read from the sweep count and cursor advance. When a run
ends by itself (exhausted, offline, identity), the worker settles the request to paused if run until
idle is still the request, so the pill reads "Paused" with no note; the settle is the worker's act
at those ends, so a Run now queued behind a Pause or a Stop run is not overwritten. A Stop run
served by the library scan's `drainCommands`, before the cycle's listing, ends the run like one
applied mid-pull: that cycle's listing runs suspended and publishes Unknown/"paused". Queued
commands take effect in arrival order in `drainCommands` as in `apply`, so the later of a Run now
and a Stop run wins. The run flag is published for exactly as long as it is set and is not
persisted: a restart leaves the rig simply paused. Stop run is a rig command beside pause, resume
and run-now. Test: `TestRunUntilIdlePublished`.

### 12.7 Metrics and logging

- `delete_pass_yield_total` counts slices that yield, making a rig that keeps slicing visible.
- The delete phase's duration histogram observes one slice.
- `delete_blocked_total{reason="identity_unconfirmed"}` counts slices dropped for an unanswered
  identity probe. Every slice runs the UDP probe, many times an hour during a drain, so on lossy Wi-Fi a
  larger share of slices is dropped, each costing a poll wait; the metric makes that loss measurable.
- `delete_hash_interrupted_total` counts device re-hashes interrupted by a Pause (§12.8); a steady
  climb with no Pause behind it is a stuck flag.
- Each yield logs at Info with `visited`, `streamed`, `remaining` and `device_tight`; a pressure change
  logs at Info (§12.2); the slice start, cursor, deadline and debt log at Debug.

Metric definitions are in `docs/metrics.md`. Per-cycle overhead (listing walk, session opens, identity
probes, marker reads) is expected to stay at a few percent of link time; if it proves significant on a
share with thousands of files, the mitigation is a minimum slice of about `2 × poll`, deliberately not
built until measured.

### 12.8 A pausable device re-hash

The device re-hash wraps its reader so it stops between buffers once the context ends or a
`pauseRequested` flag is set; a Pause during a drain therefore takes effect within one read buffer
rather than one whole frame. The flag must never outlive the Pause it announces, or every later re-hash
would abort and deletes would stall while the device fills. Three rules scope it:

- `Send(Pause)` sets the flag **before** enqueuing, and records the requested state only after the
  enqueue succeeds. A Pause dropped on a full channel clears the flag again and leaves the requested
  state untouched, since nothing else would ever clear it.
- Applying the pause clears the flag. Every delivery path goes through that one point — `Run`'s select,
  the startup drain, `drainCommands` — and because the flag is set before the enqueue, no stale set can
  survive the application, so a later Run now while paused is never interrupted.
- `Send(Resume)` also clears it, covering a Pause and a Resume both still queued.

An interrupted stream returns an error before any comparison or delete, so it can only become a
deferral: the frame stays verified on the device for a later slice. It never reaches the culled-original
drain's digest reuse, and the wrapper touches reads only, never the device delete. The interrupt logs at
Info, distinctly from a transient read error ("interrupted by pause" or "by shutdown"). The flag is one
flag: a second Pause dropped on a full channel, or a Resume racing a later Pause, clears a flag a
still-queued Pause set, and that Pause then waits for the frame boundary; nothing can leave the flag set
with no Pause behind it. Tests: `TestPauseInterruptsDeviceRehash`,
`TestContextCancelInterruptsDeviceRehash`, `TestDroppedPauseClearsRehashFlag`,
`TestAppliedPauseClearsRehashFlag`, `TestResumeClearsRehashFlag`.

### 12.9 Safety argument per invariant

Slicing decides only when a slice stops and which row it starts from. `test/COVERAGE.md` maps the
numbered invariants to tests.

1. **Three-way hash check.** The device re-hash, the NAS `VerifyRead`, the `deviceHash == sha256 &&
   nasHash == archive_sha256` test and the culled-original drain's re-read all run in full inside one
   `deleteOne` call for every frame deleted. The deadline is checked only before a frame starts. No hash,
   digest or verdict is carried across slices: each attempt re-reads the row and re-hashes both copies,
   and an interrupted re-hash maps onto the transient-defer return before any comparison. Tests:
   `TestDeleteThreeWayMatchDeletesFrameAndSidecar`, `TestSlicedDrainExactCount` (integration).
2. **Device identity per delete session.** Each slice opens its own session and confirms identity before
   any delete; a yielding slice closes its session, and the next re-confirms on a fresh one, so the window
   between a confirmation and a delete is about one slice. The foreign and unconfirmed returns report no
   yield and register no pill restore, so `Error` survives; an unconfirmed skip is counted. Test:
   `TestIdentityForeignOnLaterSlice`.
3. **Grace.** The non-positive-grace refusal and `DeletionEligible(now − grace)` are evaluated at every
   slice start with a fresh cutoff; the cursor only reorders a set already grace-filtered. Test:
   `TestDeleteGraceNotElapsedNotEligible`.
4. **Open re-file and supersede intents.** They are read once per slice, and a read error skips the
   slice (INV-M2). Re-file intents commit inside a request served only in `Run`'s select, never in
   `drainCommands`; supersede intents open and close within one supersede on the pull phase's filing
   path. Neither runs inside a slice, so neither set can grow while a slice holds its snapshot, and a
   request queued mid-drain is served in the gap before the next slice re-reads them. The same holds for
   the per-root marker verdicts and the settle window. Test: `TestIntentOpenedBetweenSlices`.
5. **Source changed before delete.** The deferral, its row-stored count and the final re-stat before
   the delete are unchanged. `maxDeleteDefers` is calibrated for one pass per cycle at poll cadence;
   consecutive listing starts stay at least `poll` apart and a frame is visited at most once per slice, so
   deferrals accrue no faster per unit of wall time than on an idle rig, and a revising frame is not held
   sooner. Test: `TestRevisingFrameNotHeldSooner`.
6. **Disabled shares.** Skipped per frame before any mount and before the deadline check; they neither
   count toward the floor nor trigger a continuation. Tests: `TestOnlyDisabledSharesRemain`,
   `TestOnlyDisabledSharesRemainCycle`.
7. **Listing completeness.** No delete decision reads a listing: deletes are driven by rows plus a
   per-frame `Stat` and hash. The verdict is still computed only after a listing, with its completeness
   criteria. Settle is still one observation per listing and listings stay at least `poll` apart, so
   `settle_cycles` keeps its wall-time meaning; `min_file_age` is untouched. Tests:
   `TestSettleSpacingPreserved`, `TestSlicedDrainExactCount` (integration).
8. **Restore before delete.** The restore phase runs before the slice in every cycle, so "a queued
   restore's device original is never destroyed in the same cycle" holds per slice, and more cycles serve
   restores sooner. Tests: `TestAbsentDeferReasons`, `TestRestoreRepullsAndClears`.
9. **Pause.** The suspend check comes before every frame; the deadline can only end a slice earlier;
   the pill restore never overwrites the `Paused` of a paused rig; a Pause or Stop run ends a run at the
   next frame boundary like any other slice. Tests: `TestPauseMidDrain`,
   `TestRunNowWhilePausedRunsUntilIdle`, `TestRunUntilIdlePublished`,
   `TestPillRestoreAfterRestorePhasePause`, and §12.8's tests.
10. **Safe verdict.** Slicing adds no writer of the verdict. Deferring deletes cannot make Yes wrong, because frames
    awaiting deletion never count against it. §12.4 changes one input, the idle test, and a paused rig's
    kept verdict is published with its listing's time, so it claims nothing past that listing. Tests:
    `TestSafeVerdictHoldsThroughDrain`, `TestCullDrainYieldsAndStaysSafe`.
11. **Device fill.** Every slice that starts with rows ahead of the cursor advances it (I1); a tight or
    unmeasurable device gets the carried-debt fair share (§12.1); and because every slice is capped, free
    space and alerts stay fresh throughout. When the device is full, captures stop and each cycle is a
    listing, an empty pull, a slice of `poll` to `2 × poll` and a 1 s continuation, within a few percent of
    an uninterrupted drain. Tests: `TestDeleteFloorWhenPullOverruns`, `TestDeviceTightFairShare`,
    `TestDeviceTightDebtCapped`, `TestCullDrainYieldsAndStaysSafe`.

The exact-count integration test `TestSlicedDrainExactCount` stages frames through the real pull path
against real Samba, drops a fresh file mid-drain, and asserts the end state under whatever slicing
happens: every eligible original removed exactly once, every NAS copy re-hashing to its record, the
fresh file filed and verified, nothing else touched. Slicing on a fast Docker link is not deterministic,
so yield counts are asserted only in unit tests.

### 12.10 Rejected designs

- **A fixed wall-clock budget** (for example three minutes per pass). Unrelated to anything the
  operator configures: on a slow link it leaves "Last listed" several times staler than a poll-derived
  deadline, and on a LAN it is effectively unbounded.
- **A delete share independent of pull time.** Its strength is that no pull phase can starve deletes.
  The fair share replaces it, and applies only when space is tight: when the device has room, pull
  latency and freshness should win.
- **An uncapped fair share** sized to the whole pull phase (`max(poll, elapsed)`). One slice then grows
  with an unbounded pull phase; the carried debt with a `2 × poll` cap keeps the split while bounding
  each slice. `min(poll + debt, 2 × poll)` is rejected too, because adding `poll` on top of the debt gives
  deletes more than a 1:1 share.
- **Pill continuity machinery** — a "still draining" state that shows "Cleaning up" while the rig is only
  listing. It adds a status-machine input and more ways to strand the pill; the truthful alternation was
  preferred. The listed-state field the slice restores is one recorded value, not that machinery.
- **Starting the fair share at the reserve.** `device_disk_low` already fires there, so the threshold is
  `2 × reserve`.
- **A mid-pass probe for pullable work.** Pullable work appears only at a listing boundary: settle is one
  observation per listing and quiescence also needs `min_file_age`, so a probe between deletes would
  either break the settle tracker's per-cycle meaning or need a real listing anyway.
- **Draining the whole backlog when the device is low.** It brings back exactly the blindness slicing
  removes — no listing, frozen free space and alerts, no pulls — in the situation where fresh readings
  matter most.
- **Anchoring the deadline on a timestamp stamped inside the pull phase.** It couples two phases through
  a field and can leave a stale anchor that produces sub-poll cycles; `runCycle`'s own `pullStart` has
  neither problem.
- **A unified per-rig work queue** of list, pull, restore, delete and clock items. It would need one
  long-lived session with an "identity confirmed within T" rule in place of per-session probes, a
  periodic list item to keep settle meaningful, restore-before-delete moved from phase order to per-item
  order, and the verdict, alerts and free space split per item — a rewrite of the delete path's control
  flow for no better interleaving than per-frame yields. It would pay off only if re-file and retarget
  had to be served between individual items rather than between cycles.
- **A need-scheduled planner** with capture, idle and pressure modes. Its capture mode runs zero deletes
  while any quiescent frame is unpulled, so a persistently deferred frame pins deletes at zero with no
  alert; its gap-fill relists run listings far closer than `poll`, eroding `settle_cycles` and spending
  `maxDeleteDefers` in minutes, which would hold mid-revision frames before the pull can supersede them;
  and its scheduling depends on the device clock offset and a read-rate estimate an operator cannot
  predict.
- **Run now while paused as one long cycle.** A second code path for the same action; run until idle is a
  flag on the ordinary cycle instead.
- **Leaving the Safe idle test unchanged.** The verdict would read "Working" for the whole drain while
  every frame is on the NAS (§12.4).
- **A fraction-of-total criterion for tightness**, so a small share is not permanently tight. With the
  cap, an always-tight rig gets the tight-device bound, not an unbounded one, and a small share close to
  the reserve is genuinely close to `device_disk_low`; a second threshold adds reasoning for little.
- **A separate cleanup-status block on the card.** The per-frame cleanup readout already carries delete
  progress.
- **Reusing the transfer's "current file" for delete progress.** It is the live feed's join key for a
  transferring row, so a delete would render as a copy.

## 13. Dismissing a stuck unclassified frame

A frame Commander pulls but cannot make sense of — a zero-byte or truncated FITS file whose header cannot
be read, an unknown `IMAGETYP` — is filed to the quarantine area as `unclassified`, never deleted, and
counted in the never-deletable backlog alert. That is right for a frame the operator can still fix with
an alias or a re-shoot. Some frames are permanently unreadable, and without an exit the operator would be
stuck: deleting the file on the device does nothing (the row is never reconciled against device-side
deletion for a non-`verified` frame), reclassify re-reads the same corrupt copy and fails again, and
acknowledging the alert brings it back on the next cycle. **Dismiss** is that exit: one click and a
confirmation remove the corrupt NAS copy and stop the frame counting toward the alert.

### 13.1 What dismiss does

`DELETE /api/rigs/{id}/frames/{frameId}` routes through the supervisor to the rig's worker, which:

1. **Guards** the state: only an `unclassified` frame may be dismissed. Re-dismissing a `dismissed` frame
   is a no-op success, so a retried request never errors. A frame on another rig is not found.
2. **Purges** the quarantined NAS copy at its `dest_path`, **but only if no other row on the rig names
   that `dest_path`** (the H12 two-share case files identical content to one copy shared by two rows).
   The count has no state filter: a `deleted` row's `dest_path` is its permanent NAS archive, and excluding
   it would let a dismiss purge an archived science frame. A copy already gone is success. A real failure
   (permission, NAS unreachable) aborts the dismiss, leaving the frame `unclassified`, so nothing is left in
   a half-state. The purge also evicts the frame's cached previews.
3. **Tombstones** the row: `state = dismissed`, and `pulled_at`, `dest_path` and the reconciliation marks
   (`missing_seen_at`, `restore_requested_at`, `missing_acked_at`) cleared in one update. `size` and
   `mtime` stay, because they are all decide needs to recognise the file as unchanged. Clearing
   `pulled_at` drops the frame from the transfer-history and "transferred tonight" surfaces, which key on
   it: a dismissed frame must not read as a successful transfer whose file was just deleted. Clearing the
   marks keeps a dismissed row out of the restore queue and the missing-frames scan, which skip an empty
   `dest_path`.
4. **Auto-acknowledges** the per-frame unclassified alert, and the aggregate backlog alert once the rig's
   never-deletable count reaches zero, then recomputes the "kept" and "tonight" counters and publishes.

The API answers 204 on success, 409 for a frame that is not unclassified, 404 for an unknown frame or
rig, and 500 for anything else (a purge failure, a store error, a worker timeout), whose operator action
is simply "retry". The web UI shows a trash button on each unclassified row; its confirmation says that
dismissing removes the corrupt copy from the NAS, cannot be undone, and does not delete the file from the
device. A dismissed frame renders as a muted "Dismissed" pill in the all-states views. Tests:
`TestDismissTombstonesAndPurges`, `TestDismissPurgeFailureLeavesUnclassified`,
`TestDismissSucceedsWhenNASCopyAlreadyGone`, `TestDismissGuardsStateAndIsIdempotent`,
`TestDismissSharedDestPathKeepsSiblingCopy`, `TestDismissClearsTransferHistorySurfaces`,
`TestDismissAutoAcksBacklogAlert`, `TestDiscardFrame` (web).

### 13.2 The tombstone, and how a frame un-dismisses

The row survives so the file is never silently re-discovered and re-held. A `dismissed` row is a settled
state in the skip predicate (§4.1), so an unchanged corrupt file on the device is never re-pulled. If the
device file at that path **changes** — a reused FAT short name such as `LIGHT_~1.FIT`, or a counter-reset
run — the size or mtime no longer match, the differ branch pulls the different content with no
`frame_lost` (there is nothing to prove, the copy having been purged deliberately), and if it is still
unreadable it becomes `unclassified` again, which is correct for genuinely different content. The
tombstone is sticky only while the file is unchanged, so a genuinely different frame at a reused path is
never shadowed (`TestDismissedFrameDecideSkipsUnchangedRepullsChanged`).

A dismissed frame is safe on the NAS for the Safe verdict's purposes: it was deliberately abandoned, so
powering off loses nothing worth keeping. Without that, a dismissed frame still on the device — skipped by
decide every cycle — would pin pending work forever and freeze the rig at Working, the opposite of the
point (`TestDismissedFrameDoesNotBlockPowerOff`).

Tombstones are never pruned (pruning removes only `deleted` rows), deliberately, so an unchanged corrupt
source stays recognised. Growth is bounded by the number of distinct corrupt source paths per rig, since
a reused path overwrites its own row through the `(rig_id, source_share, source_path)` key.

| Alternative | Why rejected |
| --- | --- |
| Hard-delete the row instead of a tombstone | The row's size and mtime are what let decide recognise the file as unchanged; without it the corrupt source is re-pulled and re-held on the next cycle and the alert never clears. |
| A permanent tombstone that ignores later changes | A reused filename, common with FAT short names, would be shadowed and never pulled. Tying stickiness to size and mtime reuses the change detection Commander trusts everywhere else. |
| Dismiss off the worker goroutine | Leaves a race: a concurrent differ re-pull could raise a spurious `frame_lost` after the purge, or the purge could delete a copy the worker just re-filed at the same path. |
| Allow dismissing `held` and `failed` | `held` is a delete-time integrity signal that a copy may be corrupt, which a one-click trash button would paper over; `failed` frames retry themselves (§4.3). Dismiss has no handling for `held` or `failed` frames; surfacing them is a separate, known gap. |
| Best-effort purge, tombstoning anyway on failure | Leaves orphaned corrupt files the operator cannot see or clean; the purge must succeed, or the file must already be gone, before the state flips. |
| Reuse the fire-and-forget command channel | It cannot carry a frame id or return a per-frame result to the UI. |

### 13.3 Dismiss runs on the worker goroutine

Frame mutations for a rig happen on exactly one goroutine, the rig's worker, and that single goroutine is
the serialization ([architecture §3.6](2026-10-17-architecture.md)). Dismiss therefore travels on a
dedicated synchronous request channel carrying the frame id and a reply channel; the HTTP handler blocks
on the reply, bounded by a 10 s supervisor timeout. The worker services the channel from `Run`'s select
and from `drainCommands` between files, so a dismiss issued during a long pull is handled within one
file's time rather than at the cycle boundary. `Run`'s select serves it whatever the pause state (only the
ticker branch checks it), so a frame can be dismissed while the rig is paused or offline: dismiss touches
only the NAS and the database, never the device. It runs under its own `recover()`, because it is a worker
entry point outside the cycle's recover (`TestDiscardFrameThroughWorkerLoop`).

The aggregate backlog alert is acknowledged on the resolving transition without scanning alerts every
cycle: a latch records that the cycle raised it, and the scan runs only on the cycle where the count
reaches zero. The dismiss path checks the count unconditionally, because a rig dismissed while paused or
offline may never have run a cycle to set the latch. The latch is not persisted, so a worker that starts
with a never-deletable count of zero while a stale backlog alert is still open leaves that alert standing
until the operator acknowledges it, if nothing is ever dismissed; that is accepted.

### 13.4 Edge cases

- **The corrupt source stays on the device, unchanged**: the tombstone matches and decide skips it for
  good; the alert stays clear.
- **The source changes after dismiss**: re-pulled and re-classified (§13.2).
- **A shared NAS copy**: dismissing one row tombstones it and keeps the file; the file is purged only when
  a dismiss finds no other row naming it. If the surviving sibling later re-files elsewhere after a
  content change, the original copy is left as harmless dead space — wasted space, never data loss. The
  per-frame unclassified alert is keyed on the source path, so while a sibling at the same path is still
  unclassified the alert correctly stays.
- **Dismiss during an active pull**: serviced at the next between-file drain or the cycle boundary.
- **A timeout after the worker already completed**: the caller sees 500, but dismiss is idempotent, so the
  retry returns 204 and a reload shows the row gone.
- **The purge succeeds but the tombstone write fails**: the NAS copy is gone while the row is still
  `unclassified`. The operator's retry self-heals it (the purge finds nothing, the tombstone write re-runs);
  in the window before the retry a source change could raise a spurious `frame_lost` through the differ
  branch. It is logged distinctly.
- **A hung NAS mount**: the 10 s timeout bounds the HTTP caller, not the `Remove` syscall, which takes no
  context; a dead network mount can block the worker goroutine past it. This is not particular to
  dismiss: the worker issues uncancellable NAS `Remove`, `VerifyRead` and `RenameNoReplace` calls on every
  pull and delete, and dismiss shares that exposure: `localfs` calls take no context.
- **An empty `dest_path`**: the purge is skipped and the row tombstoned anyway.
- **Device space is not freed.** Dismiss never deletes the source on the device — a one-click device delete
  is exactly the dangerous path it avoids — so on a capacity-constrained rig the corrupt source still
  occupies space after the backlog alert's escalated "capture will stop" wording is acknowledged. Nothing
  is hidden: the separate `device_disk_low` alert keeps firing while space is genuinely low, and reclaiming
  the space is a manual device-side delete, which the tombstone tolerates without re-holding.
- **Acknowledging a still-non-zero backlog alert** re-raises it on the next cycle, unchanged by dismiss;
  dismiss removes the driver, which is what gives the operator an exit.

## 14. Safe to power off, and drained

The card shows two statuses that are easy to conflate, as different facts. **"Safe to power off"** means
every frame on the device has been pulled and proven on the NAS, so powering off loses nothing. **Drained**
means the device originals have been deleted, so the device's space is free. A pulled-but-in-grace
original is safe and not drained. For a capacity-constrained rig that difference is the whole point, so
the card shows "device drained" or "N originals still resident" beside the Safe verdict.

### 14.1 The verdict

A non-programmer will pull the plug on this word, so the verdict is computed conservatively, from a fresh
listing, never from database state alone. It is computed once per listing and reads:

- **Yes** only when the rig is online, the listing was complete (every enabled share freshly and
  successfully listed, no absent removable medium, §14.2), the rig is not suspended, there is **zero
  pending work** — nothing un-pulled, nothing inside `min_file_age` or not yet settled, nothing mid-transfer,
  nothing `held` or `failed`, every listed frame already safe on the NAS (§4.1) — **and** the rig was idle a
  full cycle: the listing signature is unchanged since the previous listing, discounting only Commander's
  own proven deletes (§12.4). The idle requirement keeps a frame written moments after one listing from
  being papered over by a stale Yes.
- **Working** when the listing was complete but work remains or the device changed since the last
  listing.
- **Unknown** in every other case — offline, a failed or partial listing, an absent removable medium, a
  paused rig, an unconfirmed or foreign device — rendered "unknown / don't rely on this", never safe.
  Otherwise a rig that captured three subs and dropped offline before the next listing would read safe
  while those subs sit only on the device.

A frame marked missing from the NAS library whose device source is still listed means the card holds the
only copy: it downgrades Yes to Unknown and sets the "frames are missing from the library; the device
holds the only copy" warning, moving confidence down, never up, without pinning pending work
([library-reconciliation I20](2026-10-17-library-reconciliation.md)). Frames awaiting deletion never count
against Yes. The verdict surfaces the **listing timestamp** it came from.

The verdict and its reason are written by one helper, so they never diverge and neither goes stale on the
long-lived status struct; every path that sets the verdict outside a listing (session failure, identity
refusal, offline, pause) goes through it. Tests: `TestSafeToPowerOffFreshVerifiedIdleSafe`,
`TestSafeToPowerOffOfflineUnknown`, `TestSafeToPowerOffStaleListingUnknown`.

### 14.2 Multiple shares and removable slots

For a rig watching several shares (the H12 USB-plus-eMMC case), Yes requires **every** enabled share to
have been freshly listed and fully drained; one share whose listing failed or is stale makes the verdict
Unknown. A missing USB stick mounts with an empty root, indistinguishable from "everything already
transferred" (H5), so a watched removable slot with no medium reads Unknown, by design: an empty slot the
rig watches might be hiding un-pulled frames
([device-identity-and-media §7.5](2026-10-17-device-identity-and-media.md)). A medium that is inserted but
drained is distinguished structurally by the presence oracle and reads safe
([device-identity-and-media §7.2](2026-10-17-device-identity-and-media.md); A2;
`TestSafeToPowerOffDrainedRemovableIsSafe`). The operator's remedy for a slot they do not use is to disable
that share, which the server allows only for a slot the last listing proved empty
([device-identity-and-media §8](2026-10-17-device-identity-and-media.md), D4), not a weaker verdict. An
absent removable share is also excluded from the per-share free space and the tightest-share capacity
math, because with no medium its `statfs` reports the ASIAIR's own root filesystem
([device-identity-and-media §7.4](2026-10-17-device-identity-and-media.md)).

### 14.3 Power-off reasons

An Unknown verdict carries a `power_off_reason`, so the badge can say truthfully why it cannot confirm
safety and what to do; the reason is empty whenever the verdict is Yes or Working.

| Reason | Set when | What the card says |
| --- | --- | --- |
| `paused` | the rig is suspended; a pause can itself truncate the listing | resume to refresh; treat as not safe until then |
| `offline` | the probe or session open failed | the rig is offline |
| `offline_during_run` | a run until idle ended on an offline cycle | the rig went offline during the last run |
| `remote` | the tunnel to the rig's remote site is down | the remote the rig is reached through is not connected ([remote-sites §10](2026-10-17-remote-sites.md)) |
| `identity` | a foreign device answered, or the identity probe went unanswered | a different device answered, or this rig's identity is unconfirmed |
| `missing_on_device` | a frame missing from the library still has its device source | some frames are saved only on the device; points at the warning line rather than repeating it |
| `removable_absent` | a watched removable slot has no medium | insert it, or turn that share off |
| `listing_failed` | none of the above: the listing did not complete | usually clears on the next check |

**B1.** *For a verdict computed from a listing, the reason is set for its Unknown condition in the priority
paused > offline > missing_on_device > removable_absent > listing_failed, and is empty whenever the verdict
is not Unknown.* Missing-on-device ranks above an absent slot because a frame known to exist only on the
device is a concrete data risk, while an absent slot is merely uncheckable; paused ranks first because a
pause can truncate the listing, so "clears on the next check" would be false. Test:
`TestPowerOffReasonPriority`.

A distinct fourth badge state for "frames only on the device" is rejected: the verdict genuinely is
Unknown, and only its explanation varies, so the explanation is a reason carried beside the state. Inferring the
reason in the frontend is rejected too: the absent-slot and stale-listing cases cannot be derived
client-side without fragile heuristics, while the backend knows the reason exactly.

### 14.4 After a run until idle

During a run until idle the verdict is computed from each listing as on an unpaused rig. When the run ends
at idle or on Stop run, the card keeps the verdict of the run's final listing, labelled with that
listing's time ("as of"), until a Resume or a new Run now replaces it. "The final listing" is the last
listing completed while the run was active, unless a later refused cycle (an unconfirmed identity, a
session-open failure) published Unknown, which then stands, since the refusal invalidated the listing's
verdict. The "as of" time is published only for a verdict computed from a listing of an unsuspended rig —
Yes, Working, or a listing-derived Unknown such as `missing_on_device` or `removable_absent` — and is zero on
Unknown/"paused", Unknown/"offline…" and Unknown/"identity", so a kept verdict never claims anything past
its listing. A plain Pause shows Unknown/"paused"; an offline end shows its own Unknown; an identity-error
end shows Error with Unknown/"identity" (§12.6, run ends).
