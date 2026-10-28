# Library reconciliation: culled frames

**Scope.** What Commander does when the operator deletes a filed frame from the NAS by hand. It
covers the governing principle and the three rules that make detection safe; the single
`missing_seen_at` mark, which records an observation and never a conclusion; the library marker that
gates every conclusion, and how a root is linked automatically; the library scan, Restore, and the
deletion of a culled frame's device original, including the re-stamp of the settle clock when a
deletion class is armed; Acknowledge, which waives the settle window and nothing else, behind a
server-issued token; the cull drain that extends the deletion to proven-archived `unclassified`
frames; why reorganising library folders by hand is unsafe; and the missing panel's split into
actionable rows and leftovers, with Forget (§13). It hosts the reconciliation invariants I1–I40 (§5)
and the cull-drain acceptance criteria AC1, AC1b and AC2–AC9 (§11.6), and the missing-panel
acceptance criteria AC-T1–AC-T6 (§13.5). Terms such as *proven archived*, *missing mark*, *settle
window*, *cull* and *library marker* are defined in [overview §9](2026-10-17-overview.md); the frame
lifecycle the delete pass belongs to is [frame-lifecycle §9](2026-10-17-frame-lifecycle.md), and the
cross-cutting delete rules are [delete-safety](2026-10-17-delete-safety.md).

## 1. The problem

Operators cull their data by hand. A sub is trailed or a night is clouded out, so they open the NAS
share and delete the file, or the whole `NIGHT_n` folder. Commander never learns this happened,
and without reconciliation its three reactions are all wrong: the delete pass silently
**re-pulls** the file the operator just deleted if the device original still exists; if the
operator deletes it on both sides it raises a **critical `frame_lost` alarm** for routine
cleanup; and once the device original is gone the row points at a file that is not there and is
listed as though it were safely archived.

The index must agree with the disk. A frame the operator removed stays removed, is never
re-pulled, is shown as missing rather than present, and eventually leaves the list, while the
device original is still deleted on its normal schedule so the ASIAIR's card does not fill with
frames the operator has rejected.

The hard part is not detection. It is making detection safe: a destination that is unmounted,
asleep or renamed makes every frame look deleted, and acting on that would either destroy the
index or, far worse, authorize deleting device originals whose only archive copy was merely
unreachable.

### 1.1 Governing principle

**The NAS copy is the safe place for the data.** Once a frame is verified at rest there, the
device original is working storage awaiting cleanup, and the operator's own deletion rules
(`grace`, `keep_originals`, `reconcile_settle`) are the dial they set to match their comfort
with that. Commander honours those rules rather than second-guessing them, and does not
withhold a scheduled deletion because the operator removed the archive copy on purpose.
Everything below follows from this: Commander never re-pulls on its own, never deletes an
original sooner than the operator's rules allow unless the operator asks for it explicitly
(Acknowledge, §10), and never quietly overrides what the operator asked for. The principle is
stated for the whole product in [delete-safety §2.2](2026-10-17-delete-safety.md).

## 2. The design

### 2.1 Three rules

1. **Nothing is concluded without a matching marker.** A destination root is reconciled only
   while it carries a library marker whose id matches the one recorded for it (§2.4). A root is
   linked automatically, and only on proof from a file Commander itself filed and verified there
   (§8).
2. **Nothing is concluded from an error.** A frame is absent only when its directory listed
   successfully, or is itself confirmed gone, *and* an authoritative `Stat` confirms the file is
   not there. Every error class (unreadable directory, permission failure, timeout) concludes
   nothing (§2.5). The rule is product-wide:
   [delete-safety §2.3](2026-10-17-delete-safety.md).
3. **The irreversible act requires fresh confirmation at least one settle window after the first
   recorded and alerted observation**, and is undone at any point in between by the file
   reappearing, a restore request, a relink, or a pause (§2.3). The window is the per-rig
   `reconcile_settle`, 24 h by default (§4.5).

### 2.2 One timestamp, and no stored conclusion

The frame row carries **`missing_seen_at`**: the scan sets it when a pass confirms the file absent
and clears it when a pass finds the file present. That is the only state Commander stores about
absence. (`missing_acked_at`, §10, and `restore_requested_at`, §2.3, are operator intents that
qualify the mark; neither records a conclusion.)

There is deliberately **no column recording that an absence was "concluded"**. A stored
conclusion needs race predicates inside the marking statement, a narrowing clause on the
deletion query, a settle carve-out in the list filter, a clear-versus-recheck split in the
reversal actions and a special case for a manual rescan, and every predicate then has to be
correct in a second behavioural mode. The defects of that shape live in the seams between the
mechanisms rather than in any one of them.

The conclusion is instead **computed at the only moment it matters**, immediately before the
device original is destroyed. The delete pass authorizes a deletion when, on state freshly read
at that instant (§2.7):

- `missing_seen_at != 0` and `now - missing_seen_at >= settle`, or the operator acknowledged the
  cull (§10);
- `restore_requested_at == 0`;
- no re-file or supersede intent is open for the row's `dest_path`;
- `dest_path` falls under a configured root, and that root is confirmed for this pass;
- a fresh local `Stat` confirms the file is still not there;
- the device copy still hashes to the row's recorded `sha256`.

A mark that survives to `seen + settle` has survived every intervening scan pass **that reached
its directory**, because missing rows stay in the scan's input and any pass that finds the file
present clears the mark. The qualifier is real: a pass abandoned on pause or cancellation
restarts from the top next time, and because directory order is stable, a library whose passes
are consistently truncated never re-examines its tail. That weakens the claim without weakening
the guarantee, because the delete pass's own fresh `Stat` immediately before the delete is the
backstop and is unconditional. The settle clock also starts at the same instant the operator is
alerted (§4.8), which is the honest meaning of "a settle window to reverse it".

The scan's only write is a monotone, idempotent, reversible set of one column, so a scan pass
racing an operator action cannot produce a wrong authorization; at worst it restarts a clock.

```
                 rig worker cycle (one goroutine per rig)
   ┌───────────────────────────────────────────────────────────────┐
   │  scan, if due or an operator rescan is queued; never if paused │
   │      │   NAS-only: runs even when the device is offline        │
   │      │   FOR EACH configured Light/Cal root:                   │
   │      │     no recorded id → try auto-link from verified data   │
   │      │     marker id == recorded id?  no → skip this root      │
   │      │     stream directories holding live rows, in order      │
   │      │       name present → clear missing_seen_at              │
   │      │       name absent  → Stat to confirm → mark, alert      │
   │      ▼                                                         │
   │  device reachable? ──no──► return                              │
   │      ▼                                                         │
   │  pull ──► restore ──► delete pass                              │
   │                                                                │
   │  per eligible frame, on a freshly read row:                    │
   │      NAS copy present      → three-way verify (frame-lifecycle)│
   │      present but truncated → restoreNAS (replace)              │
   │      absent → authorize only if every §2.2 condition holds:    │
   │                 hash device == sha256                          │
   │                 → drainCommands, re-read, re-check             │
   │                 → deviceDeleteFrame, resolve the row           │
   │               else defer                                       │
   └───────────────────────────────────────────────────────────────┘
```

### 2.3 Reversal

A mark is undone by any of four ways, none of them a dedicated mechanism:

- **The file reappearing.** The next scan pass clears the mark automatically; **Rescan now**
  (Settings › Library) makes that immediate. The delete pass and the restore phase also clear it
  when they find the copy present and verified (I14).
- **Restore.** `restore_requested_at` on the row; the restore phase re-pulls from a
  still-resident device original. A standing request is a standing hold on the **absent**
  branch, the only branch that authorizes a deletion from a missing copy. If the NAS copy turns
  out to be present and verifies, the request is moot: the delete pass clears both it and the
  mark and proceeds with the ordinary three-way check, because the frame was never missing after
  all.
- **Relink.** Linking a root clears the marks of every row under it, on every rig, whenever the
  id it records differs from the one previously recorded. A mark formed against the wrong share
  was factually true about *that* share; only its clock was tainted, and relinking resets exactly
  that (I10, I11a).
- **Pause.** Per-rig and fleet-wide pause both gate the scan, the restore phase and the delete
  pass at entry, and the restore phase also stops between frames once the rig is paused (§4.4).

There is no "clear all missing marks" action and no "re-check missing frames" action. Re-check
would duplicate the next scan pass. Clear exists only to un-conclude without fixing anything,
which matters only when a conclusion can outlive its cause; with no stored conclusion, "stop
everything at once" is Pause and "never delete originals on this rig" is `keep_originals`, which
skips the delete pass entirely.

### 2.4 The library marker gate

A root is reconciled only when its `.asiair-commander-library-id` marker carries the id recorded
for it in the state database (meta key `libraryid:<root key>`). Any other state (no recorded id,
an absent, unreadable or corrupt marker, or a different id) skips that root: nothing under it is
marked, and the delete pass treats every frame under it as `root_unconfirmed`.

The gate earns its place even though rule 3 already gives a settle window, because the window is
a chance to *notice*, and an operator asleep or away for a weekend gets nothing from it. The
marker is the only mechanism that distinguishes "share absent", which rule 2 catches, from "share
present but wrong or empty" (a stale mount point, a renamed root, an empty restore target), and
that second case is exactly the mass-deletion hazard. There is no cheaper signal of equal
strength: volume serials change across legitimate SMB remounts, and heuristics over tree content
are defeated by Commander's own pull path, which files verified frames into destination roots and
would make any stale mount point non-empty. It mirrors the removable-media marker on the device
side ([device-identity-and-media §6](2026-10-17-device-identity-and-media.md)) and shares its
code: `mediaid.VerifyNamed` and `(*Media).AdoptNamed` over a local-filesystem origin
(`localfs.MarkerOrigin`), which joins paths with `filepath.Join` and returns not-exist errors
unwrapped so an absent marker is distinguishable from an unreadable one. It deliberately does not
reuse `mediaid.PrefixedOrigin`, which joins with `path.Join`: forward slashes are correct for the
SMB device share that origin serves and wrong for a local Windows path.

The marker identifies the destination root, which is global and shared across rigs, so it is
generated with the literal instance `"library"`, never a rig id. `RootLinkState` is the single
evaluation, shared by the worker's gate and the Settings library view; its reasons are:

| Reason | Meaning |
|---|---|
| (empty root) | not configured; skipped silently |
| `not linked` | no recorded id yet; the next verified file links it (§8) |
| `marker missing` | an id is recorded but the marker is gone: a deleted sentinel or an unmounted NAS |
| `different library` | a parseable marker whose id differs from the record |
| `marker corrupt` | a read that succeeded and returned bytes that do not parse; a manual re-link overwrites it |
| `marker unreadable` | the marker, or the recorded id, could not be read; a read that fails partway is this, never `marker corrupt`, so a re-link cannot overwrite another library's valid marker; a store error fails closed |

Reasons drive reporting, not branching: a skipped root defers everything under it either way.

### 2.5 ReadDir filters, Stat decides

A name missing from a directory listing is a *candidate*, not a conclusion. Three steps, because
no single one is sound:

1. **Byte-exact match** of the row's base name against the listing means present. This is exact
   by construction: Commander wrote that path itself and the filesystem stores the bytes it was
   given, so the stored path and the directory entry agree unless something renamed the file.
2. **Case-folded match** against the listing means **not** absent, and logs a warning.
   Destinations are treated as case-insensitive (NTFS, or an SMB share backed by one), so a case
   divergence means a third party renamed the file rather than that it is gone (I8).
3. Otherwise `Stat` of the stored `dest_path`, verbatim, decides; only not-exist counts. A
   directory that itself returns not-exist skips steps 1 and 2 and confirms every frame by
   `Stat` (this is the `dirGone` signal §10.2 reuses).

`Stat` is **not** an independent witness: it traverses the same mount and SMB session, so a share
mid-reconnect fails both, and that class is covered by rules 1 and 3. Filenames are byte-exact
with respect to Unicode normalisation, so an NFC/NFD divergence fails both identically; step 1's
by-construction argument covers that, and §6.6 records the residual case of a server that
normalises on write.

### 2.6 Arming a deletion class

A deletion class is a kind of device delete authorized from a missing archive copy: the verified
cull (§9) and the proven-archived unclassified cull (§11). Marks can exist before a class is armed:
the scan records them whether or not any deletion may act on them.

**Principle: arming a deletion class gives every standing mark of that class a fresh settle
window.** Otherwise the first delete pass with the class armed finds every standing mark older
than the window and authorizes the whole backlog at once, with no notice. That is worst precisely
for the case the window exists to catch: an operator who renamed a target folder while deletion
was disarmed, saw the warn alert and shrugged, would have that folder's entire device-resident
backlog destroyed the moment the class is armed.

The arming is a **schema migration**, not a runtime toggle. `migrate5` (store schema 4 → 5) arms
the verified cull: a store crossing it has every non-null `missing_seen_at` re-stamped to the
migration's own time, and the migration runs once per store (§9.2, I4c, I11b). A
`reconcile_enforce`-style toggle was rejected:
it forces a per-rig setting, an enforcement epoch, an enforcement-change clear with a single-owner
problem, a Clear that has to disable enforcement to be durable, and a second behavioural mode in
which marks are never written, so every predicate must be correct in a configuration that behaves
differently from the one being reasoned about. A migration gives the same "observe it before
trusting it" property with no runtime surface: the safety property comes from which schema the
store is at.

The proven-archived unclassified class (§11) has **no** arming migration and no re-stamp. That is a
recorded deviation from this principle, with its residual, in §11.4.

### 2.7 deleteOne reads fresh state at the moment of action

The delete pass loads its eligible set once and iterates, while `drainCommands` (which services
restore, cancel, acknowledge and other worker requests) runs inside that loop, and the cycle loads
its config pointer once. So `deleteOne` takes a frame **id**, not a frame, and re-reads the row
with `GetFrameByID` and the config with `w.cfg.Load()` at entry. It receives no config parameter,
so there is no stale copy to reach for. A row pruned meanwhile is gone on re-read and is skipped
silently. A row dismissed by a concurrent request is still there, but dismissal nulls its mark and
its `dest_path`, so it can never be authorized: the absent branch defers it as `not_marked`.

Two values are evaluated **once at the top of each delete pass** and passed down: `rootOK`, the
per-root marker verdict, and `settle`, the rig's effective `reconcile_settle` (floored to
`defaultReconcileSettle`, 24 h, for a hand-edited non-positive value). `rootConfirmed` costs a
store read plus a NAS marker read, and every culled frame stays eligible on every pass, so a
per-frame call would be hundreds of marker reads per pass. A share that drops between the
pass-level check and the delete is caught by the `Stat` and `VerifyRead` error paths, which
conclude nothing. Open re-file and supersede intents are likewise read once per pass; that set can
only shrink mid-pass, which is the safe direction.

**The second evaluation is not optional.** Hashing the device copy streams a full frame (a
full-frame FITS is around 116 MB) and takes tens of seconds, during which `drainCommands` has not
run for this frame. Without a second evaluation, a Restore clicked mid-hash would sit unserviced
on the worker channel while the delete proceeded on a stale snapshot, destroying the last copy of
a frame the operator is actively recovering. So after a matching hash, `handleAbsentNAS` calls
`drainCommands`, re-reads the row, reloads config, re-`Stat`s the destination and re-runs
`absentDeferReasons` (I5). If the copy **reappeared** during the hash, it clears the stale mark
and restore request and leaves the frame to the ordinary present-copy path on the next pass. The
removable-media check and the source `Stat` are not repeated, because `deviceDeleteFrame` performs
its own final source re-`Stat` within ±2 s immediately before the delete, and a media marker cannot
change mid-frame without the share dropping, which that re-`Stat` catches. `rootOK` is not
re-evaluated per frame either: it is a few seconds old, and the reasoning above applies.

## 3. Alternatives considered and rejected

#### The reconciliation core

| Alternative | Why rejected |
|---|---|
| **A stored "concluded" timestamp beside the observation** | The source of most defects a design of this shape produces; see §2.2. |
| **A `reconcile_enforce` runtime toggle** | Forces a per-rig setting, an enforcement epoch, an enforcement-change clear, a Clear that has to disable enforcement, a give-up that has to disarm, and a second mode every predicate must be correct in. A schema migration arms deletion instead; see §2.6. |
| **Clear all missing marks / Re-check missing frames** | Redundant without a stored conclusion. Re-check duplicates the next scan pass; Clear's intents are covered by relink, Pause and `keep_originals`. See §2.3. |
| **A restore give-up after a fixed number of days** | A standing restore request is already a standing hold, since the absent branch defers unconditionally on it, which is the outcome a give-up tries to reach, with no mechanism. Failures alert once per episode; the request stays until the operator cancels it (§4.4). |
| **Per-pass directory caps and persisted cursors** | Make the settle window depend on library size, break the "survived every intervening pass" property §2.2 relies on, and add cursor-persistence defects. A streaming pass over a few thousand directories takes seconds to low minutes, once every six hours. |
| **A durable `missing_hidden_at` column with an opt-in list filter** | Drags a hide predicate into `ListFrames`, whose never-deletable backlog count feeds the capacity alert; hidden rows dropping out of that count would auto-ack the backlog alert, including the "capture may stop" critical, while those frames still occupy the card. Grouping by directory makes a large cull one heading. |
| **Alert superseding per directory** | Unneeded: the message carries the count and the date, so a larger later event produces a different string and is not swallowed by alert dedup (§4.8). |
| **A filesystem change-notification watcher** | Change-notify over SMB does not survive a reconnect and drops events on buffer overflow. Decisively, the operator usually culls while the service is not running. |
| **A full recursive walk of a root** | Enumerates directories Commander does not own and re-derives paths the index already holds. |
| **A cursor-paged `Stat` per frame row** | O(files) round trips instead of O(directories). |
| **`missing` as a `FrameState`** | A `verified` frame (original still on the card, restorable) and a `deleted` frame (original gone, the NAS copy was the archive) can both go missing, and that difference decides whether recovery is possible. State and presence are orthogonal. |
| **Concluding absence in the delete pass with no marker gate** | Would delete originals for an unreachable archive; see §2.4. |
| **Adopting a marker when a root lists non-empty** | Defeated by Commander's own pull path writing into a stale mount point. Linking on a *verified file write* is a different trigger and is adopted (§8.1). |
| **Trusting the directory listing alone** | Case-insensitive lookup and NFC/NFD divergence make a name diff unsound as the sole basis for deleting a device original; see §2.5. |
| **Automatic restore on absence** | Re-pulls the file the operator just deleted, the complaint this design exists to fix. |
| **An in-memory restore queue** | Overflow, drop-on-full, loss on restart, and a race against the delete pass through `drainCommands`. A row flag has none of these. |
| **Reclaiming the `NIGHT_n` ordinal after a folder is deleted** | The surviving night-counter row is what files a re-pulled night into its original folder, and reclaiming it reopens the interleaving hazard the night assignment depends on; see [routing-and-filing §6](2026-10-17-routing-and-filing.md). |
| **Per-frame durable tombstones on the NAS** | Help only when the state database is lost *and* the original is still resident; see §6.5. |
| **A monotonic clock for the settle comparison alone** | Would leave settle and `grace` on inconsistent clocks; see §6.6b. |

#### Acknowledge

| Alternative | Why rejected |
|---|---|
| **A single `POST` with `?confirm=true`** | A query parameter is bookmarkable and replayable, so it fails the one property the handshake delivers (§10.7). |
| **Typed confirmation (retype the path or the count)** | The threat is a request with no preceding challenge, not an operator who misunderstands their own deletion. |
| **A signed stateless token (HMAC of target and expiry)** | Cannot be single-use without server state anyway, so it trades the map for a key plus a replay window, and cannot carry a bound id set without becoming large. |
| **Persisting tokens in SQLite** | A confirmation that survives a restart is a liability. |
| **A mint that performs no state check** | A dialog rendered from possibly stale client state defeats a server-issued confirmation. The SQL `WHERE` is the only *authoritative* guard; the mint's checks exist to produce a truthful dialog and a useful refusal. |
| **A group token bound to the directory alone** | The authorized set could grow between dialog and confirm without bound, and a group's whole risk is that one click covers many files (§10.7). |
| **Waiving more than the settle window** | `root_unconfirmed` is what stops an unmounted NAS from looking like a library the operator culled. No UI control may waive it. |
| **Backdating `missing_seen_at` instead of a column** | Destroys the observation time the panel shows and makes the record lie about when the file was first seen absent. |
| **An `acknowledged` frame state** | Acknowledgement is a property of the missing mark, not of the transfer lifecycle, and changing `state` would take the frame out of the deletion query it is asking to enter. |
| **Per-frame acknowledge only** | A real cull is dozens of frames; thirty identical danger dialogs train click-through, defeating the confirmation. |
| **Writing from the HTTP handler, skipping the worker hop** | Removes the timeout hazard but breaks the rule that every frame mutation happens on the worker goroutine ([architecture §3.6](2026-10-17-architecture.md)). A second write path into these columns is a worse precedent than a 202 (§10.7). |

#### The cull drain

| Alternative | Why rejected |
|---|---|
| **Adding `unclassified` to the deletion query without guarding normal cleanup** | Would delete the device original of a *non-culled* unclassified frame after grace, destroying the keep-until-classified safety the quarantine state exists to provide. |
| **A separate query for culled frames, removing verified culls from the deletion query** | Moves the working verified cull path onto different code for no gain. Broadening the query and guarding normal cleanup leaves verified behaviour unchanged. |
| **Widening group acknowledge to `unclassified`** | Every unclassified frame lives in one flat quarantine directory per rig, so one group would be the entire backlog, and the `dir_gone` signal is inert there (§11.3). |
| **A re-stamp migration for the unclassified backlog** | Not taken; recorded as a deliberate deviation from §2.6 with its residual in §11.4. |
| **A feature flag or an offline validation report** | The settle window is the boundary for a narrowly scoped extension of an existing class. |
| **Fixing classification so these frames become `verified`** | Cannot rescue an already-culled frame: reclassify re-files by moving the NAS copy, which the operator deleted. Worth doing as prevention; it does not recover a stranded frame. |
| **Draining `held` culled originals too** | `held` means the device bytes did **not** match the recorded hash; deleting could destroy the only copy of different data. Never. |

#### The missing panel and Forget

| Alternative | Why rejected |
|---|---|
| **Resolving `deleted` rows automatically when `deleted_at < missing_seen_at`** | A grace delete whose source had already vanished from a pulled card records the same ordering, and resolving it drops the mark that stops a re-pull when the card returns (I1). No column distinguishes the two (§13.1). |
| **Hiding `deleted` rows from the panel** | Leaves the reinserted-card Restore unreachable, offers no way to clear them, and breaks the panel's superset of the power-off verdict's missing check. |
| **Leftovers shown but never clearable** | They grow without bound (I19); a collapsed section nobody can empty only moves the clutter. |
| **Clearing only the mark on Forget** | The next scan re-derives it from the absent file (§10.6). Forget nulls `dest_path` too. |
| **Splitting the rows client-side within the current page, or searching on the client** | Paging still runs over the mixed set, and a client search reaches only the loaded page. |
| **Collapsing old folders by age (`missing_display_period`)** | Age is not actionability: a leftover from last night is as inert as one from August. |
| **Publishing which rows the device is listing, to promote them** | Changes the worker and the status payload to answer a rare case precisely; the empty-state pointer (§13.3) never misleads and costs nothing. |
| **A leftovers caution ("the device is listing one of these") driven by `missing_warning`** | The warning usually comes from actionable rows, and it is a latch that outlives the deletes that make it stale (right after an acknowledge, or on an offline or paused rig), so the caution would claim a leftover is on the device when it is not. |
| **A filtered Forget all (by `q`)** | Its label would change as the operator types, for a bulk write. Folder and section scopes match the counts beside them. |


## 4. Mechanism

### 4.1 Model and store columns

`model.Frame` carries three reconciliation fields on the 0-means-unset convention, each stored as
SQL `NULL` (never 0) through the store's `nullInt`/`intFromNull` helpers, because every predicate
in this design is `IS NULL` / `IS NOT NULL`. A stored 0 would silently break `SetFrameMissing`'s
idempotency (I13), put an unmarked row into the partial index and every `IS NOT NULL` query
(the missing panel among them), and make `PruneDeleted`'s guard (I19) skip a row that carries no
mark.

| Column | Meaning | Set by | Cleared by |
|---|---|---|---|
| `missing_seen_at` | unix second the scan first confirmed the NAS copy absent | `SetFrameMissing`, only on a fresh mark | `ClearFrameMissing`, `ClearMissingUnderRoot`, `ResolveCulledFrame`, `UpsertFrame`, `DismissFrame` |
| `restore_requested_at` | the operator's request to re-pull, a standing hold on the absent branch | `RequestRestore` (missing rows only), `MarkRestoreInProgress` (unconditional, for the truncated-copy replace) | `ClearRestoreRequest`, `ResolveCulledFrame`, `UpsertFrame`, `DismissFrame` |
| `missing_acked_at` | unix second the operator acknowledged the cull, waiving the settle window (§10) | `AcknowledgeMissing`, `AcknowledgeMissingByIDs` | `SetFrameMissing` (fresh mark), `ClearFrameMissing`, `ClearMissingUnderRoot`, `RequestRestore`, `ResolveCulledFrame`, `UpsertFrame`, `DismissFrame` |

A partial index `idx_frames_rig_missing ON frames(rig_id) WHERE missing_seen_at IS NOT NULL`
serves the missing queries. The frame-row column semantics as a whole are
[frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md); the tables and their migrations are
[architecture §7.7 and §7.8](2026-10-17-architecture.md).

Rules the columns depend on:

- **`UpdateFrame` never writes these columns.** It overwrites a full row from a possibly stale
  in-memory snapshot (`markDeleted` and `holdFrame` push whole `model.Frame` values through it),
  so enumerating them would write them back from stale values on every hold and delete. They
  change only through the targeted statements above.
- **`UpsertFrame` clears all three, unconditionally.** A re-file describes a file once again
  present and verified at rest. A conditional clear is wrong: a failed pull upserts a row with no
  `dest_path`, which leaves the scan's input carrying a mark nothing could ever clear.
- **`DismissFrame` clears all three** for the same reason: it nulls `dest_path`, so a surviving
  restore request would send `restoreNAS` at an empty destination and a surviving mark could never
  be cleared by the scan.
- **`PruneDeleted` skips any row with `missing_seen_at` set** (I19). Whether a `deleted` row
  still carries a mark therefore decides whether it is ever prunable, and the two paths that
  reach `deleted` differ on purpose. A culled delete Commander performed resolves the row
  (§10.6), so it ages out on the normal retention schedule. The vanished-source path
  (`handleVanishedSource`) **keeps** the mark, because there the source vanished on its own,
  the operator may have pulled or reformatted the card, and the original can reappear; pruning
  that row would reopen the re-pull this design closes.
- **The frame column list and the scan arguments are matched by position**, with nothing
  enforcing it, so a reconciliation column is appended last rather than inserted.

`model.LibraryMarkerName` is `.asiair-commander-library-id`. It is written to a NAS destination
root and must not be added to the device-side listing filter, although it sits beside the device
marker names that filter does use.

### 4.2 The per-root identity gate

`rootConfirmed(ctx, root)` is `RootLinkState` (§2.4) over the worker's store and local filesystem.
`RootKey` normalises a root to an absolute, cleaned, lowercased path, so a trailing slash or a case
change does not mint a fresh key and silently discard the recorded id. The meta key is per
**root**, not per rig: multiple rigs may share a destination root, and linking it for one links it
for all (§8.4).

Only `different library` warrants its own alert (`library_mismatch`, critical): someone else's
library mounted at the destination is a distinct condition. `marker missing`, `marker corrupt` and
`marker unreadable` surface through `library_unavailable` after six hours, with the reason in the
message. `not linked` raises nothing: such a root is awaiting its first verified file, which links
it, and it is not a fault (§4.8).

There is deliberately **no write probe**. The scan only reads; linking does its own write with its
own error path. Requiring write access on every pass to conclude absence would make a destination
that goes read-only stop reconciling and silently accumulate device originals, for no safety gain.
A root must nevertheless be writable **once**, to be linked (§6.8).

### 4.3 The library scan

`scanLibrary` is a **pure observer**: its only frame writes are `SetFrameMissing` and
`ClearFrameMissing`, both idempotent and reversible, and it authorizes nothing. It is wrapped in a
`recover` so a scan fault never takes down the worker cycle.

**What it walks.** `scanRoots` returns `Roots.Light` and `Roots.Cal` only, the two roots that hold
filed frames; `Staging` and `Logs` never do. The same two-root set is what `ScanTargets` assigns
rows to and what the delete pass's `outside_roots` disqualifier tests, so a row can never be
assigned to a root the scan does not walk.

**Rows.** Each row carries its `dest_path` in full, because §2.5 step 3's confirming `Stat` is the
call that authorizes an irreversible deletion and must use the stored path verbatim, never a path
rebuilt from the directory group key. `store.ScanTargets` streams a rig's rows with a non-empty
`dest_path` in the scannable states (`verified`, `deleted`, `unclassified`, `held`), `ORDER BY
dest_path`, through the read handle so the writer is not blocked, and invokes a callback once per
directory group, so memory is O(largest directory) and the caller can yield, drain commands and
abandon between groups. Each row is assigned to the **longest** configured root that contains it
(`longestRoot`, built on `store.UnderRoot`), so a root re-typed as a nested path does not silently
strand every row under "no configured root" (I12). `UnderRoot` is the single containment predicate:
cleaned, lowercased, and separator-anchored, so `/astro` does not contain `/astrophotography`. It is
Go, not SQL, because SQLite's `LIKE` folds only ASCII and cannot express separator anchoring, and
this predicate gates a deletion authorizer. Grouping by `filepath.Dir` change while streaming can
split one directory into two runs; that costs a second `ReadDir` and nothing else, because the
callback is idempotent.

**`ScanTargets` deliberately does not filter on `missing_seen_at`.** Marked rows must stay in the
input: they are what reappearance-clearing acts on and what makes §2.2's "survived every
intervening pass" argument true, at the cost §6.9 accepts.

**Per pass:**

1. If the rig is paused, return. Pause is tested directly at entry, not through the run loop's
   ticker guard or `workSuspended()`: run-now and an identity reset both enter the cycle on a
   paused rig, and pausing is the operator's response to a suspicious pass (I22).
2. Count rows whose `dest_path` falls under no configured root (`CountOutsideRoots`) and, when
   non-zero, raise a standing `AlertConfig` warn (I21); the count is logged, not put in the
   message, which would make every change of count a new alert, and a scan that finds none
   resolves it ([architecture §10](2026-10-17-architecture.md)). This belongs to the scan, not the
   delete pass, because the delete pass does not run for a `keep_originals`, offline or paused rig
   and returns early when nothing is eligible.
3. Per root: evaluate `rootConfirmed`. A `not linked` root first gets an auto-link attempt from
   data already on disk (§8.1). A root that still fails records its reason, feeds the
   unavailable/mismatch alerting, and is skipped; a failing root never affects another (I11). A
   confirmed root clears its first-failure record.
4. Per directory group: `ReadDir`. A successful listing applies §2.5's three steps; a directory
   that returns not-exist confirms each frame by `Stat`; any other error is transient,
   increments `UnscannedDirs`, and concludes nothing (I7). A confirmed absence calls
   `SetFrameMissing`; a frame found present calls `ClearFrameMissing`. Between groups the scan
   sleeps a few milliseconds, calls `drainCommands`, and abandons the pass on cancellation or
   pause. `drainCommands` only sets `w.paused`; it stops nothing by itself. An abandoned pass
   restarts from the top next time: passes are idempotent, so there is no cursor to persist.
5. Alert on frames **newly** marked in this directory this pass (§4.8), not on every
   already-missing frame, so a standing cull does not re-alert.
6. Publish the pass's `LibraryScanResult` (per root: linked, reason, newly missing, cleared,
   directories scanned, unscanned directories, and up to 50 newly marked directory names) on the
   rig status as `last_library_scan`. The result is replaced wholesale, never mutated, because
   the status is an immutable value read by SSE subscribers.

**Scheduling.** The scan runs at the top of the cycle, after the config load and before the device
address is resolved, because it touches only the NAS and a rig whose ASIAIR is powered off must
still scan; daytime with the mount parked is when operators cull. It is not gated on
`keep_originals`: such a rig still has a library that can be culled. An automatic pass runs when
`reconcileInterval` (6 h) has elapsed since the last one; a pass in which every root failed, or one
abandoned on pause or cancellation, schedules the next attempt `reconcileRetryInterval` (15 min)
away. A manual rescan arrives on a capacity-1 channel with a non-blocking send (a pending request
already means a scan is queued) and does not move the schedule. Because `SetFrameMissing` is
idempotent, an extra observation can only start a clock, never accelerate one, so a manual rescan
is the same code path as an automatic one with nothing to special-case (I13). The worker has no
mutex; every cross-goroutine path is a channel, which is why the rescan request is one rather than
a flag.

### 4.4 Delete pass, restore phase and pull path

**Delete pass.** The eligible set and grace are [frame-lifecycle
§9.1](2026-10-17-frame-lifecycle.md); the reconciliation-specific behaviour of `deleteOne`, after
its fresh re-read (§2.7), admission, the removable-media presence check and the source `Stat`, is a
cheap local `Stat` of `dest_path` **before** the device hash:

| Condition | Action |
|---|---|
| `Stat` succeeds | Proceed to the device hash and `VerifyRead`. On a readable present copy, clear `missing_seen_at` and `restore_requested_at` if set: the file is demonstrably back. Clearing on the `Stat` alone would be wrong, since a truncated stub exists but is not the file returning. Normal cleanup then continues only for a `verified` row (§11.2). |
| `Stat` errors with anything but not-exist | Transient; defer. |
| absent | `handleAbsentNAS`: defer, logged at INFO with every matching reason, unless `absentDeferReasons` is empty; otherwise authorize as §9.1 describes. |
| present, then `VerifyRead` reports a short file | A truncated copy is corruption, not a cull: `restoreNAS` with replace (I3). |
| present, then `VerifyRead` reports not-exist | The copy vanished between the `Stat` and the read: treated as absent, never restored, reusing the device hash already streamed. Keeping this apart from the short-file row is the point: merging them would re-pull on absence for every race where the file vanishes between the `Stat` and the read, the one behaviour reconciliation exists to remove. |

`absentDeferReasons` is a pure function of stored state and the per-pass inputs:

| Reason | Meaning |
|---|---|
| `not_marked` | no missing mark: the NAS copy was never observed absent by the scan |
| `settling` | the mark is younger than the settle window and not acknowledged |
| `restore_requested` | the operator asked for this frame back |
| `refile_in_flight` | an open re-file intent names this `dest_path` (INV-M2, [routing-and-filing §10](2026-10-17-routing-and-filing.md)) |
| `supersede_in_flight` | an open supersede intent names this `dest_path` ([frame-lifecycle §11.9](2026-10-17-frame-lifecycle.md)) |
| `outside_roots` | `dest_path` is under no configured Light/Cal root |
| `root_unconfirmed` | the longest containing root failed its marker check this pass |

**The `Stat` is a correctness requirement, not an optimisation.** Without it every culled frame
would re-hash its full device copy on every pass while it settles (I6). It sits **after** the media
check and the source `Stat`: hoisting it above the source `Stat` would skip the vanished-source
path and leave a both-sides-culled frame `verified` forever; above the media check it would
defeat the swapped-medium guard. What is saved is the hash, not the two cheap checks. The present
branch also defers, before the hash, when a supersede intent is open for the path, since a
supersede replaces a path in place.

**The root-confirmed condition** closes an asymmetry with the device side. The delete pass already
refuses to act on an unconfirmed removable medium, so an absent stick's empty listing is never read
as mass deletion; without the NAS equivalent a marker mismatch would alert and stop *further* marks
while the delete pass kept destroying originals for old ones.

**A both-sides cull.** When the source is gone too, the vanished-source path ([frame-lifecycle
§9.4](2026-10-17-frame-lifecycle.md)) verifies the NAS copy first. If the copy is absent or
truncated, both copies are gone by the operator's hand. The §2.2 authorization list cannot apply,
since there is nothing left to delete; the precondition is that the row was `verified` with a
recorded `sha256` and that `VerifyRead` confirmed the NAS copy absent or truncated: it raises
`frame_lost` at **warn**, not critical, records `source_vanished`, and marks the row `deleted`
(I23). That transition destroys nothing (the source is already gone and the NAS copy is confirmed
absent) and is necessary: without it the row stays `verified` and is re-selected every poll, one
device `Stat`, one `VerifyRead`, one log line and one alert per frame per cycle, forever. This
branch records no `missing_seen_at` of its own, because a delete-pass mark would start a settle
clock the scan never confirmed. Critical is reserved for a NAS copy that is *present but corrupt*.

**Restore phase.** `restorePhase` runs before the delete pass in the same cycle, so a queued
restore's original is never destroyed in that cycle, and is **not** gated on `keep_originals`: a
keep-originals rig still has a library to restore into. It returns before opening anything when no
row carries a request; otherwise it opens a device session, confirms device identity, and per row
drains commands, skips disabled shares, skips a frame still inside its retry back-off (below),
then takes an admission grant (which enforces `max_concurrent_transfers` and the free-space
floor), mounts the share and requires the removable-media check (an absent or swapped medium
mounts with an empty root, and without the check a restore would read the wrong content and hold
the frame with a critical mismatch). The phase runs only on a cycle whose pull was not refused and
whose rig is not paused (a run-until-idle request overrides a pause), and it stops between frames
as soon as the rig is paused or the destination reads as unavailable, so Pause also stops a replace
that would remove a NAS file. It then decides by
**verification, not existence**:

- the copy verifies against the archive anchor: it is back and good; clear the mark and the
  request, no restore;
- a short file: `restoreNAS` with replace;
- not-exist: `restoreNAS` without replace;
- anything else concludes nothing and defers, bounded (below).

Driving the replace from a bare `Stat` would remove a **good** copy that reappeared while the
request sat queued (I18). The replace mode exists because `restoreNAS` ends with `RenameNoReplace`
onto `dest_path`, which by contract fails while a file is there, so without it a truncated copy
could never be replaced. Remove-then-rename is not atomic, so `restoreNAS` with replace first sets
`restore_requested_at` (`MarkRestoreInProgress`), then removes the truncated file and renames the
staged copy, then clears the request. A crash in that window leaves a standing request, which the
absent branch treats as an unconditional defer and the restore phase retries, rather than a
self-inflicted cull (I17). What a restore streams, and whose bytes it may put back where a
`dest_path` is shared or the device has revised the frame, is [frame-lifecycle
§11.6](2026-10-17-frame-lifecycle.md).

**Retries back off and never give up.** `restoreNAS` streams the whole device copy before it can
fail on the destination, so a read-only or full volume would otherwise re-stream every requested
frame every poll. A frame is attempted at most once per `restoreRetryInterval` (1 h), tracked in a
per-worker in-memory map rather than the pull-retry columns, which would collide; a restart costs
one extra attempt. Each failed attempt raises a `transfer_failed` warn naming the frame, which
alert dedup keeps to one standing alert while the message is unchanged. A verify that stays
inconclusive for `maxRestoreInconclusive` (3) consecutive attempts is reported the same way,
with a message that carries the bound rather than the count so alert dedup keeps it to one
standing alert. There is **no give-up**: the request stays set, which holds the original, until the
operator cancels it (I16). A successful restore clears the mark, the request and both per-frame
counters.

**Pull path.** `decide` never re-pulls a row with `missing_seen_at` set, in any state except
`failed`, while the device holds same-size content (I1). The gate is on **size**, before the mtime
comparison, because a different card reader, a FAT timestamp shift or any touch of the file moves
the mtime without changing content, and re-pulling would re-file the frame and clear the mark,
putting the operator's culled frame back on the NAS (I2a). It subsumes the `deleted`-row re-adopt
branch that would otherwise resurrect a culled night from a reinserted card. `failed` is excluded so
the retry path keeps working (I2). A `failed` row does not carry a mark in practice: `markFailed`
upserts with no `dest_path`, and `UpsertFrame` clears the mark. The carve-out costs nothing and
stops a later change to either predicate from silently stranding failed retries. A size change is
genuinely different content at that path (a counter-reset run writing a different frame):
`resolveDiffer` returns "proceed" for a marked row without alerting, the old row's copy being gone
because the operator deleted it, and `UpsertFrame` clears the mark because the row then describes
the file at that path. Returning "do not proceed" there would strand the new frame unpulled
forever. Hashing the device copy would
be a stronger test, but `decide` holds no share handle, and the pull path's own post-stream
verification catches a same-size overwrite downstream. A device-side revision of a culled frame is
recognised and advances the row's device anchors without filing anything: [frame-lifecycle
§11.12](2026-10-17-frame-lifecycle.md).

**Safe to power off.** `safelyOnNAS` keeps its meaning (`verified`, `unclassified`, `dismissed`),
because it drives the "Safe to power off" verdict and widening it would read safe for a frame whose
only copy is the card. Instead the pinning call site skips a missing frame, so a cull does not pin
the rig at Pulling, and the pull phase sets `missingWithSource` whenever a listed source belongs to
a marked row. `publishListingStatus` then downgrades `SafeYes` to `SafeUnknown` with power-off
reason `missing_on_device` and sets `RigStatus.MissingWarning` ("frames are missing from the
library; the device holds the only copy"). It moves confidence down, never up (I20). The downgrade
is required, not optional: skipping a missing frame at the pinning call site is exactly what would
otherwise let such a frame reach `SafeYes`. The reason rides its own `MissingWarning` field rather
than `LastError`, because `publishListingStatus` clears `LastError` on every listing and `LastError`
means something else. The verdict itself is defined in [frame-lifecycle
§14](2026-10-17-frame-lifecycle.md).

### 4.5 Commands, API and settings

Every frame mutation runs on the rig's worker goroutine
([architecture §3.6](2026-10-17-architecture.md)). The manager reaches a worker through a channel
request with a buffered (capacity 1) reply and a 10 s caller context; the web layer reaches the
manager only through the `Supervisor` interface.

| Manager method | Transport | Notes |
|---|---|---|
| `LinkLibrary(rigID, root)` | link request channel, sync | `root` must be the configured Light or Cal root. Adopts an existing parseable id without rewriting the file, so linking can never destroy another installation's identity; generates and writes only when the marker is absent or corrupt (a corrupt marker yields no id to record, and refusing would leave the root permanently unlinkable); refuses an unreadable root. §8.1 says why automatic linking differs. When the recorded id changes, clears the marks under the root on every rig (I10, I11a). Serviced only from the run loop, not `drainCommands`, because it touches the NAS and `drainCommands` runs between device deletes. |
| `RescanLibrary(rigID)` | rescan channel, cap 1, non-blocking | Returns nil on a full channel: a scan is already queued. |
| `RestoreFrame(rigID, frameID)` | restore request channel, sync | `RequestRestore`: applies only to a marked row, `deleted` rows included (a reinserted card may still hold the original), and clears any acknowledgement in the same statement. |
| `CancelRestore(rigID, frameID)` | restore request channel | `ClearRestoreRequest`, scoped to the rig so a cancel routed through one rig cannot clear another's frame. |
| `AcknowledgeMissing(rigID, frameID)` | restore request channel | §10.8. |
| `AcknowledgeMissingIDs(rigID, dir, ids)` | restore request channel | §10.8; `dir` is carried only for the audit log line. |
| `ForgetFrame(rigID, frameID)` | restore request channel | §13.4; the worker re-reads the row and refuses anything but a marked `deleted` row with no restore pending. |
| `ForgetLeftovers(rigID, dir, olderThanDays, ids)` | restore request channel | §13.4; `dir` (empty for the whole section) and `olderThanDays` (0 for any age) are carried only for the audit log line. |

Restore, cancel, acknowledge and forget are serviced both in the run loop and in `drainCommands`, so
they land promptly during a long pass and an idle rig still answers. **Synchronous timeouts are a UI
contract, not a safety one.** While a frame transfer is in flight a 10 s call can time out and the
work still be applied moments later; the UI treats a timeout as *unknown* and refreshes. Nothing in
§2.1 depends on request latency: §2.7's second evaluation is what makes a late restore still stop a
deletion.

| Route | Response |
|---|---|
| `POST /api/rigs/{id}/library/link`, body `{"root": "…"}` | 204; 404 unknown rig; 400 otherwise (unconfigured root, unreadable root) |
| `POST /api/rigs/{id}/library/scan` | 202; 404 unknown rig; 500 when the scan cannot be queued |
| `GET /api/library/status` | 200 `{"roots": [{root, linked, reason}]}`, computed fresh per configured root, deduplicated when Light and Cal resolve to one root |
| `POST /api/rigs/{id}/frames/{frameId}/restore` | 202; 400 non-integer frame id; 404 unknown rig; 409 not missing; 500 store error |
| `DELETE /api/rigs/{id}/frames/{frameId}/restore` | 204; 400 non-integer frame id; 404 unknown rig; 500 store error |
| `POST /api/rigs/{id}/frames/{frameId}/acknowledge` | §10.7 |
| `POST /api/rigs/{id}/missing-groups/acknowledge` | §10.7 |
| `POST /api/rigs/{id}/frames/{frameId}/forget` | §13.4 |
| `POST /api/rigs/{id}/missing-leftovers/forget` | §13.4 |
| `GET /api/rigs/{id}/frames?missing=true` | the missing panel's page for one section, with `groups: [{dir, count, ackable}]` (§13.2) |

Library routes use path segments rather than the colon-action convention for argument-free rig
commands, because `link` carries a body and `scan` is a distinct resource. Mutating routes are
wrapped in `guardWrite` and read routes in `guardRead`
([architecture §8.3](2026-10-17-architecture.md)); every route string appears in the registered
route list a test diffs against the mux.

`?missing=true` routes to `ListMissingFrames` and **overrides** `state` and `filter` rather than
intersecting with them (a missing frame keeps its real state, so intersecting would surprise);
passing both is a 400. It returns every marked row with **no age cutoff**, so the panel is always a
superset of the Safe-to-power-off downgrade, which counts every marked row. `groups` comes from
`MissingGroupCounts`: the true per-directory totals for the whole missing set, not the page,
aggregated by directory in a map (rows of one directory need not be adjacent in `dest_path`
order) and returned as a non-null array. The frame DTO carries `missing_seen_at`,
`restore_requested_at` and `missing_acked_at`.

**Settings.**

| Key | Scope | Default | Meaning |
|---|---|---|---|
| `reconcile_settle` | `defaults`, with a per-rig override | 24 h | The settle window: how long after the first recorded, alerted observation before a culled frame's device original may be deleted. Per device, exactly like `grace`, resolved by `EffectiveReconcileSettle`. Validation rejects an effective value ≤ 0 for every rig, because a zero window would authorize deletion the instant a mark is set; unlike `grace` it is validated for `keep_originals` rigs too. The delete pass floors a hand-edited non-positive value to 24 h. Config schema migration v3 → v4 backfills it. |
| `missing_display_period` | top level | unset | Accepted, validated as a duration, and editable in Settings; reserved for collapsing old directory groups in the missing panel by age, which the panel does not do (§4.7). Zero or negative means never collapse. |

The effective settle window is also published per rig on the status as `reconcile_settle_sec`.

### 4.6 Where each fact is shown

| Fact | Where it is visible |
|---|---|
| Which roots are linked, and why one is not | Settings › Library, computed fresh per root, with Re-link (§8.3) |
| Which frames are missing | The per-rig missing-frames panel on the Devices surface, with Restore and Acknowledge (§4.7) |
| That the card holds the only copy | `missing_warning` beside the power-off verdict on the rig card and the Devices surface; power-off reason `missing_on_device` |
| A cull was observed | One `library_reconciled` warn alert per directory per pass (§4.8) |
| Per-pass scan outcome (linked, newly missing, cleared, unscanned directories) | `last_library_scan` on the rig status |
| Why a culled frame is still waiting | An INFO log line per deferred frame naming every matching reason. `delete_blocked_total` covers the *blocked* paths (media unconfirmed, source changed before the delete, holds), not this deferral, which is a normal waiting state |
| What was deleted, and why | `frames_deleted_total{manner="culled"}` or `{manner="culled_acked"}`, distinct from the ordinary post-grace `device_deleted` so the riskiest delete path can be alerted on by itself, plus a WARN log line carrying the source and destination paths and whether the cull was acknowledged ([docs/metrics.md](../metrics.md)) |
| An acknowledgement | A WARN audit line per frame, or one per directory with the count (§10.8) |

There is no offline report of reconciliation state. Everything such a report could show is
available live, and a second, staler view of the same state invites the defect it would exist to
catch: a counterfactual computed over every missing row counts `deleted` and non-deletable rows
as "would delete".

### 4.7 Web UI

- **Missing-frames panel** (Devices surface, per rig). A notice explains that missing frames had
  their filed copy removed outside Commander and are treated as deliberately culled, that their
  original leaves the device once the settle window passes, that **Restore** pulls the original
  back and holds it meanwhile, and that **Acknowledge** skips the wait for frames culled on
  purpose. Below it the rows are split into an actionable section and a collapsed section of
  leftovers already gone from the device, each with a search box, an unpaged stack of directory
  rows (`dir · count`, a click filters the table) and a paged table (§13.3). Actionable folders
  carry **Acknowledge all N** where something is acknowledgeable; leftover folders carry
  **Forget all N**. Columns: preview,
  filed copy, type, "missing since", a status pill (Missing, Restore requested, or Acknowledged), and
  frame actions (Restore or Cancel, and a per-row Acknowledge or Forget). On a `keep_originals`
  rig no Acknowledge control renders, but the group counts do.
- **Status pill.** `missingFrameView` renders a missing frame; a pending restore takes precedence
  over acknowledged. The labels are Missing, Restore requested and Acknowledged. The Acknowledged
  pill uses the warn tone (rendered as `working`, the tone warn severity maps to), the `missing`
  icon, and the help text "You confirmed this was culled on purpose — the device original is
  removed on the next pass." It requires a proven-archived state *and* `missing_seen_at` *and*
  `missing_acked_at`: never `missing_acked_at` alone (§10.5), and never on a terminal `deleted`
  row, where it would read as a deletion still pending.
- **Previews.** Image eligibility (the SPA's `frameHasImage`, the server's `eligibleForImage`)
  tests state, `dest_path` and `sha256`, not the mark, so the missing panel's preview column
  renders for every row. A mark evicts nothing from the preview cache, which is keyed by content
  hash: a size already cached still serves, and one never cached fails to render from the absent
  copy and answers 404.
- **Unclassified view.** Culled frames are filtered out client-side (§11.2), so a culled frame
  appears only in the missing panel.
- **Rig card and Devices status.** `missing_warning` renders beside the power-off verdict, because
  that verdict otherwise reads Unknown without saying why.
- **Settings › Library.** Per configured root: the path, a link-state pill and note
  (`rootLinkView`), and **Re-link** for every state except linked and `not linked`; **Rescan
  now** below. Both actions route through any managed rig, since the marker keys the root, and are
  disabled with an explanation when no managed rig exists.
- **Alert labels.** `library_reconciled`, `library_unavailable` and `library_mismatch` have entries
  in the alert-category labels and the `AlertCategory` union.

### 4.8 Alerts

Three categories, one meaning each, because acknowledging alerts by category for a rig would let a
category with two meanings silently ack the other. The catalogue is
[architecture §10](2026-10-17-architecture.md).

| Category | Severity | Raised when |
|---|---|---|
| `library_reconciled` | warn | frames newly marked missing, one alert per directory per pass |
| `library_unavailable` | warn | a root `marker missing`, `marker corrupt` or `marker unreadable` for `rootUnavailableAlertAfter` (6 h), message carrying the reason and pointing at Settings › Library or a remount |
| `library_mismatch` | critical | a root's marker parses and its id differs from the recorded id, immediately, pointing at Settings › Library |

The `library_reconciled` alert is labelled "Frames removed from the library outside the app", and
its message reads, for example, `14 frames removed outside the app from M31/RC8/NIGHT_2 in the
light library (observed 2026-03-14)`: the folder relative to its root and which root that is,
because the absolute mount path says nothing about where in the library the frames were. The
Alerts page links the alert to the rig's missing-frames panel in Devices
(`/devices?rig=<id>&show=missing`), which lists the frames the scan marked missing, grouped by
folder, with Restore and Acknowledge. The count and date are in the message on purpose:
alerts are deduplicated on `(rig_id, category, message)` while unacked, so a larger later event
produces a different string and is not suppressed. An equal-sized cull in the same directory on the
same day produces an identical message and is swallowed; that is accepted, because an unacked
alert already names that directory. The count is a per-pass delta, not a running total.

The first-failure time for `library_unavailable` lives in meta under `rootunavail:<root key>`,
keyed per root to match `libraryid:`: the first rig to cross the threshold raises, and any rig's
successful confirmation clears the record. `library_unavailable` and `library_mismatch` are standing
alerts ([architecture §10](2026-10-17-architecture.md)): a root's successful confirmation resolves
the confirming rig's alerts **for that root**, matched by the root in the message prefix, never by
category, which would let one root recovering retire another root's live alert. Each scan also
resolves the rig's root alerts for roots no longer configured, which will never confirm.
`library_reconciled` is an event and is never auto-acknowledged. `frame_lost` keeps its
meaning (no copy provably exists anywhere) and is raised at warn for a both-sides cull (§4.4). A
repointed root's orphaned rows raise `config` at warn (§4.3).

## 5. Invariants

Each invariant keeps its number; numbers are never reused. The proving tests are mapped in
`test/ADVERSARIAL_COVERAGE.md`. "No dedicated test" means the property is exercised by the wiring
and by neighbouring tests but has no test that pins it alone. The data-loss invariants these
contribute to are [delete-safety §6](2026-10-17-delete-safety.md).

| # | Invariant | Proven by |
|---|---|---|
| I1 | A frame confirmed missing is never re-pulled while the device holds same-size content, in any state except `failed`, including a `deleted` row whose source reappears. | `TestDecideNeverRepullsMissingFrame` |
| I2 | A `failed` row that is marked missing still retries on its normal backoff. | `TestFailedRetryUnaffectedByMissing` |
| I2a | A marked frame whose device mtime shifts without a size change is not re-pulled by the differ branch. | `TestDifferDoesNotResurrectOnWeakSignal` |
| I3 | A truncated NAS copy is still restored automatically, including replacing the short file; an absent one never is: it routes to the deletion authorization. | No dedicated test |
| I4 | A culled frame's device original is deleted only after its mark settles, never a pass earlier. | `TestCulledFrameDeletedAfterSettle` |
| I4a | Every disqualifier (not marked, settling, restore requested, re-file in flight, supersede in flight, root unconfirmed, outside roots) defers; only a frame clear of all of them is authorized. | `TestAbsentDeferReasons`, `TestRefileInFlightDefersDeletion` |
| I4b | A device copy whose content changed since filing is held, never deleted: it is not the frame that was culled. | `TestCulledFrameWithChangedDeviceContentIsHeld` |
| I4c | Migrating a store across the migration that arms the verified cull (`migrate5`, schema 4 → 5) re-stamps every standing missing mark, so no backlog is authorized at once. | `TestUpgradeV4ToV5RestampsMissingMarks` |
| I5 | The authorization is re-established from fresh state after the device hash: a Restore, or the NAS copy reappearing, during the hash stops the delete. | `TestCulledFrameDeleteRechecksFreshStateAfterHash` |
| I6 | The delete pass performs no device hash for an absent NAS copy it is not authorized to act on, while still performing the media check and source `Stat` that precede it. | No dedicated test |
| I7 | An absence is concluded only from a successful listing (or a directory confirmed gone) **and** a successful confirming `Stat`; every error class concludes nothing, in the scan and in the delete pass. | No dedicated test |
| I8 | A name differing only by case is never marked missing. | `TestCaseFoldedNearMatchIsNotAbsence` |
| I9 | A root whose marker is mismatched, corrupt, unreadable, or missing while an id is recorded is skipped and is never adopted or rewritten automatically; only a root with no recorded id is linked automatically, and only on proof from a verified file. | `TestLinkLibrarySemantics`, `TestAutoLinkOnFiledFrame`, `TestAutoLinkFromExistingDataOnScan` |
| I10 | Linking adopts an existing parseable id without rewriting, overwrites only an absent or corrupt marker, and clears marks under the root when the recorded id changes. | `TestLinkLibrarySemantics` |
| I11 | A failing root does not prevent reconciliation of another, nor ack another root's alert. | No dedicated test |
| I11a | A relink clears marks, and acknowledgements, for **every** rig under that root, not just the relinking one. | `TestClearMissingUnderRootClearsAcknowledgement` (single rig); cross-rig scope by construction |
| I11b | Arming a deletion class re-stamps the standing marks of that class, so no backlog accumulated before the class could act is authorized without a fresh settle window. Holds for the verified cull (I4c); the proven-archived unclassified class is the recorded exception (§11.4). | `TestUpgradeV4ToV5RestampsMissingMarks` |
| I12 | Containment is separator-anchored and case-folded, and a row is assigned to the longest configured root that contains it. | `TestUnderRootContainment`, `TestLongestRootAssignment` |
| I13 | `SetFrameMissing` is idempotent: a re-observation never restarts a running settle clock, so a manual rescan cannot accelerate a deletion. | `TestMarkIsIdempotent` |
| I14 | A file that reappears has its mark cleared by the scan, by the delete pass's present branch, by the post-hash re-check, and by the restore phase before it would replace anything. | `TestReappearanceClearsMark`, `TestCulledFrameDeleteRechecksFreshStateAfterHash`, `TestReappearedUnclassifiedCopyNotDeletedAndMarkCleared` |
| I15 | `UpdateFrame` cannot clobber the reconciliation columns; `UpsertFrame` and `DismissFrame` clear all three (`missing_seen_at`, `restore_requested_at`, `missing_acked_at`). | `TestMissingColumnsLifecycle`, `TestDismissFrameClearsAcknowledgement`, `TestUpsertFrameConflictClearsAcknowledgement` |
| I16 | A restore requested at any point before a missing frame's original is deleted prevents that deletion, including one arriving mid-pass, and holds it indefinitely once retries fail. A request on a frame whose copy turns out to be present is cleared, not honoured. | `TestCulledFrameDeleteRechecksFreshStateAfterHash`, `TestAbsentDeferReasons`, `TestRestoreHeldOnDeviceMismatch`, `TestHalfCommittedRestoreEscalatesAfterItsBound` (bounded retry, no give-up) |
| I17 | An interrupted truncated-copy replace leaves the frame deferred and retried, never marked and deleted. | No dedicated test |
| I18 | The restore phase never removes a NAS copy that verifies. | No dedicated test |
| I19 | A row carrying a missing mark is never pruned. | `TestPruneDeletedSkipsMissingRows` |
| I20 | A missing frame whose source is still listed does not pin pending work, **and** downgrades Safe to power off to Unknown with reason `missing_on_device`, asserted after `publishListingStatus` has run, and sets `MissingWarning`. | `TestCullDrainYieldsAndStaysSafe`, `TestPowerOffReasonPriority` (verdict and reason); `MissingWarning` has no dedicated test |
| I21 | A missing row whose `dest_path` falls under no configured root is never authorized, and the scan reports the count. | `TestAbsentDeferReasons` (authorization half); reporting has no dedicated test |
| I22 | The scan runs for an offline rig and for a `keep_originals` rig, and never runs for a paused one, including under run-now and an identity reset. | `TestScanSkippedWhenPaused` (pause half) |
| I23 | A both-sides cull raises `frame_lost` at warn, not critical, and reaches a terminal `deleted` state, so it is not re-selected every poll cycle forever. | `TestDeleteDoublyVanishedConverges` |
| I24 | Reserved; not used. | — |
| I25 | Acknowledge waives only the settle window; every other disqualifier (`root_unconfirmed`, `outside_roots`, `restore_requested`, `refile_in_flight`) still defers on an acknowledged frame. | `TestAcknowledgeDoesNotWaiveOtherReasons`, `TestAcknowledgedFrameSkipsSettleWindow` |
| I26 | An acknowledgement never outlives its missing mark: a fresh mark nulls a stale acknowledgement, while a re-observation of a standing mark preserves a live one. | `TestSetFrameMissingClearsStaleAcknowledgement` |
| I27 | A confirm that never saw a challenge (a token never minted, replayed after success, expired, or scoped to another frame or rig) cannot delete. | `TestAcknowledgeFrameRejectsBadTokens`, `TestAcknowledgeFrameReplayRejected`, `TestAcknowledgeFrameExpiredToken` |
| I28 | A group token binds its candidate ids, so a directory that grows between mint and confirm cannot widen the acknowledgement. | `TestAcknowledgeGroupChallengeAndConfirm` |
| I29 | Acknowledge races Restore safely: Restore clears the acknowledgement, and an acknowledgement is refused while a restore is pending. | `TestRequestRestoreClearsAcknowledgement`, `TestAcknowledgeRefusesNonVerifiedAndRestorePending` |
| I30 | A row outside the acknowledgeable set cannot be acknowledged, enforced by the SQL guard, the mint refusal, and the absence of a UI control. The per-frame set is proven-archived; the group set is `verified` only (I35). | `TestAcknowledgeMissingRefusesUnmarkedRestorePendingAndNonVerified` |
| I31 | A culled delete resolves the row out of the scan, so the following scan does not re-mark it or raise a false `library_reconciled` alert. | `TestAcknowledgedCulledFrameDeletedAndDoesNotReappear`, `TestResolveCulledFrameRetiresRow` |
| I32 | A device original is cull-deleted only for a proven-archived frame (`sha256` recorded, state `verified` or `unclassified`). The gate compares a fresh device hash to the row's current device anchor `sha256`, never the archive anchor; a changed device copy is held, never deleted; `held`, `deleted`, `failed` and `dismissed` are never cull-deleted. | `TestCullDrainDeviceRehashByteExact` (integration, real SMB), `TestCulledUnclassifiedFrameDeletedAfterSettle`, `TestCulledUnclassifiedWithChangedDeviceContentIsHeld` |
| I33 | Only a **missing** `unclassified` frame is deletion-eligible; a non-culled one is never in the query, and a reappeared copy is skipped after its mark is cleared. | `TestCullDrainReappearedCopyRealFS` (integration, real filesystem), `TestReappearedUnclassifiedCopyNotDeletedAndMarkCleared`, `TestDeletionEligibleScopesUnclassifiedByRigAndGrace` |
| I34 | Every `absentDeferReasons` gate, and the device-hash check, apply to an `unclassified` cull identically to a `verified` one. | `TestCulledUnclassifiedRespectsDeferReasons`, `TestAbsentDeferReasons` |
| I35 | Per-frame acknowledge is restricted to the proven-archived set; group acknowledge stays `verified` only; `held` and `deleted` are not acknowledgeable at the worker, the mint, the SQL guard or the UI. Acknowledge waives only the settle window, so the acknowledgeable set is a superset of what is cull-deletable, which additionally requires `sha256`, grace, a confirmed root and a matching device hash. | `TestServiceAcknowledgeAcceptsUnclassifiedRefusesHeld`, `TestAcknowledgeFrameChallengeAcceptsUnclassified`, `TestAcknowledgeMissingUnclassifiedPerFrameVsGroup`; the UI layer by a manual pass |
| I36 | A culled `unclassified` frame does not count toward the never-deletable backlog alert; a non-culled one, and every `held` or `failed` frame, does. A culled frame under an unconfirmed root never drains yet is excluded too, as verified culls are. | `TestCountNeverDeletableExcludesCulledUnclassified` |
| I37 | The deletion query always applies `rig_id`, grace and a recorded `sha256` across **both** state branches: no cross-rig or pre-grace frame is ever returned. | `TestDeletionEligibleScopesUnclassifiedByRigAndGrace` |
| I38 | Forget changes only a rig's own marked `deleted` rows with no restore pending, at the worker and in the SQL guard; every other row is refused. | `TestForgetRefusesNonLeftovers`, `TestServiceForgetGuards` |
| I39 | A forgotten row is never re-marked by a later scan and is removed by `PruneDeleted`; Forget deletes no file. | `TestForgottenRowNotRemarkedAndPruned`, `TestForgottenLeftoverReadoptedWhenSourceReappears` |
| I40 | A bulk Forget changes only the ids bound to its token, and reports the count actually changed. | `TestForgetLeftoversTokenBindsIDs` |

I7–I12 concern real filesystem and encoding behaviour a fake cannot fully prove. The ones with
dedicated tests, I8 (a case-differing filename) and I9 and I10 (linking, and auto-linking after the
marker is removed), run as ordinary `go test` cases against a real temporary directory, so they are
covered by the required `build-test` check rather than only by the Docker/Samba `integration`
package. I12's containment tests are pure path logic. No test pins a corrupt marker, an
NFD-normalised filename or an unreadable directory, and I7 and I11 carry no dedicated test. I32
and I33 have integration tests against real SMB and
a real local filesystem.

Several properties are structurally unfalsifiable rather than asserted: there is no stored
conclusion to race, no second behavioural mode to diverge, no cursor to desynchronise, and no
clear-versus-scan ordering to get wrong.

## 6. Consequences and accepted limitations

### 6.1 A root reconciles only once linked

A root reconciles only after its first verified file links it, or after the scan links it from
existing verified data (§8.1). Until then the scan skips it, every culled frame under it defers as
`root_unconfirmed`, and originals accumulate on the card. Settings shows such a root as "Links
automatically", and no alert is raised, because for a root that has received nothing yet this is
not a fault. A root that holds filed frames none of which verifies at rest is never linked
automatically, and its only signal is the "Links automatically" state in Settings: that path logs
nothing (only a failed link attempt logs). A manual link through the API remains possible.

### 6.2 Settling culls accumulate device originals

A culled frame's original stays on the card for the settle window, and indefinitely while its root
is unconfirmed or a restore is pending. The missing panel, the per-directory alert and the
power-off downgrade make that visible; `reconcile_settle` is the operator's dial, and Acknowledge
(§10) shortens it per frame.

### 6.3 A NAS-side loss is indistinguishable from a cull

A frame lost to disk error or a third-party program is marked like a deliberate deletion, and its
original is deleted one settle window later. Mitigations: Restore while the original is resident,
the warn alert when both copies are gone, and the operator's own `grace`.

### 6.4 The safety observations are not independent

A persistently unmounted dataset produces the same answer from every pass. What covers that class
is the settle window and the marker gate, not repetition.

### 6.5 Marks do not survive loss of the state database

Missing frames are re-pulled if the database is lost while originals are resident: the same
exposure Dismiss accepts, bounded by grace. The recorded library ids are lost too; the next
verified file into each root, or the next scan pass that verifies a filed frame, adopts the
existing marker's id without rewriting it (§8.1), so no manual step is needed.

### 6.6 A normalising server, or a moved directory, reads as mass deletion

§2.5's by-construction argument holds for every destination Commander writes directly; a server
that normalises names on write breaks it, and moving or renaming a target folder makes every frame
under it absent. The settle window and the alert make noticing possible; restoring the directory
to its old path is the repair. §12 records why this is unsafe and what bounds it.

#### 6.6a A nested mount beneath a linked root defeats the marker gate

The marker is read at the root. If a junction, mapped mount or child dataset *below* it fails
while the root itself stays healthy, `rootConfirmed` passes, `ReadDir` returns not-exist, the
per-frame `Stat` confirms absence, and the whole subtree is marked. §2.4's claim that the marker
distinguishes "share absent" from "share present but wrong" holds at the root only. The settle
window and the per-directory alert are what cover this; a marker per night directory is not worth
its cost. Acknowledge narrows this cover (§10.2).

#### 6.6b The settle comparison is wall-clock

`missing_seen_at` and the settle test both use the worker's wall clock, so a forward NTP
correction or a bad-RTC boot can satisfy the window early. `grace` has the same exposure, but this
is a comparison that directly authorizes an irreversible act, so it is named rather than assumed.
`internal/clock` provides `Instant` and `Clock.Monotonic()` for exactly this, but `grace` is
wall-clock too, and adopting it here alone would leave the two timing paths inconsistent; the
exposure is accepted and recorded rather than half-fixed.

### 6.7 A case-renamed file is never reconciled

§2.5 step 2 refuses to conclude on a case-folded near-match, so such a file warns on every pass and
is never marked. Safe, but non-converging. On a case-sensitive destination the same rule, and
`UnderRoot`'s case-folding, are conservative: they can only withhold a conclusion or merge two roots
that differ by case, never conclude an absence.

### 6.8 A destination that cannot be written once cannot be reconciled

Linking writes the marker, so a root that is never writable is never linked, its device originals
accumulate, and the only signals are Settings and a log line (§6.1). A root that becomes read-only
after linking keeps reconciling, because the scan only reads.

### 6.9 Missing rows stay in the scan's input

They must, since that is what makes "the mark survived every intervening pass" true (§2.2). The cost
is one `Stat` per missing frame per pass. `ScanTargets` streams only the scanning rig's own rows and
a row belongs to exactly one rig, so each missing frame is checked once per pass however many rigs
share the root; rigs repeat a `ReadDir` only for directories holding rows of several rigs. A
900-frame cull is about 900 local stats every six hours, against a delete pass that would otherwise
pay three SMB round trips per frame per poll. Resolving a culled row after its original is deleted
(§10.6) removes it from the input, so the cost is bounded by culls still waiting.

## 7. Non-goals

- Move and rename detection (§6.6, §12).
- Any change to `NIGHT_n` naming or the template token set.
- Night-ordinal durability across state-database loss
  ([routing-and-filing §6](2026-10-17-routing-and-filing.md)).
- Deleting a device original *earlier* than the operator's rules allow without an explicit
  operator action. Acknowledge (§10) is that action; nothing else shortens the window.
- Automatic restoration. Restore exists only as an operator action.
- Backing up the state database.
- Surfacing the live defer reason on each acknowledged row. A frame later blocked by
  `root_unconfirmed` shows an Acknowledged pill and no explanation; showing the reason needs a NAS
  marker read per root on a UI read path.
- Acknowledging the `library_reconciled` alert as part of Acknowledge, a configurable "delete
  immediately versus next pass", and a "keep" action for a missing frame (Restore already keeps
  one).

## 8. Linking a library

Linking is automatic. Commander links a destination root **the first time it successfully files
and verifies a frame into that root**, and the scan links a root that already holds filed frames
by re-verifying one of them. There is no manual link step in normal operation.

### 8.1 Why automatic linking is safe

Filing a frame and re-verifying it at rest proves everything an operator's "link" click could
confirm: the configured path resolves to a real, writable, mounted filesystem that holds this
installation's own data. At that instant Commander has strictly more evidence than an operator
looking at a path typed into Settings. The click would add friction, not safety.

This is not the rejected adoption-on-non-empty-listing (§3). That trigger is passive and is
defeated by any stale content, including Commander's own writes to a stale mount point. The
triggers here are active and current:

- **On filing.** `recordFiled` calls `ensureLibraryLinked` with the frame's destination. It finds
  the frame's longest containing Light/Cal root and, only if that root has **no recorded id**,
  links it. A per-worker set of already-ensured roots makes the common case one map lookup with no
  store read.
- **On scan, from existing data.** A root reported `not linked` gets `tryLinkFromExistingData`:
  up to eight of the rig's filed rows under that root (`FiledSamplesUnderRoot`) are re-verified at
  rest against their **archive** anchors (`archive_sha256` at `archive_size`), and the first that
  verifies links the root. A row's archive anchor is what its copy must hash to after a retarget
  or a supersede, so verifying against it rather than re-deriving an expectation is what keeps a
  superseded sample from silently never linking
  ([frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md)). This covers an existing or drained
  library that will never see a fresh filed frame. An unmounted or wrong mount presents no copy
  that verifies, so nothing is linked.

`autoLinkRoot` adopts a parseable marker already on disk, generates an id for an absent one, and
**refuses** a corrupt or unreadable marker, leaving that to a manual re-link. The manual link
(`LinkLibrary`, §4.5) overwrites a corrupt marker because an operator asked for it; an automatic
link has no operator behind it and does not guess. Neither path ever rewrites a parseable marker,
so linking can never destroy another installation's identity. `autoLinkRoot` arbitrates the id
through the store's single-writer `EnsureMeta`, so concurrent auto-links converge on one id (§8.4),
then re-reads the marker immediately before writing: if the real NAS mounted into the path in the
meantime and carries a different library's marker, overwriting it would plant this id on a
filesystem that does not hold the frames the evidence came from, so the write is abandoned. The id
is already recorded by then, so nothing re-evaluates the link: neither `ensureLibraryLinked` nor
`tryLinkFromExistingData` acts on a root with a recorded id. The root then reads `different
library`, raises the critical `library_mismatch`, and stays skipped until a manual Re-link. The
outcome fails closed; the other library's marker is never touched.

The marker gate itself is unchanged, so the anti-mass-deletion property holds through every mount
transition:

- Root linked while the NAS is mounted: the marker lives on the NAS filesystem. The NAS later
  unmounts, the mount point is an empty local directory with no marker, `rootConfirmed` fails, the
  root is skipped, **nothing is marked and nothing is deleted.**
- A frame filed to a *stale* mount point (the NAS unmounted, writes landing on local disk, a
  filing-layer misconfiguration independent of linking) links the root to that local copy. When
  the real NAS mounts over it, the local marker is hidden, `rootConfirmed` finds the recorded id
  but no matching marker, and skips the root. Still no mass deletion; the residue is orphaned
  local frames and a paused reconcile, which §8.3's alert surfaces. A manual link has exactly this
  exposure: an operator who links a stale mount point links the wrong disk too.

### 8.2 Never linked versus marker deleted

Two facts are kept apart:

- the **recorded library id** in the store (`libraryid:<root key>`): "this installation has
  linked this root before";
- the **marker file** on disk: "the real library is mounted here at this moment".

Automatic linking fires **only when a root has no recorded id**. A root with a recorded id whose
marker is absent is **never** silently re-created: that is the suspicious case, and healing it
silently would rob the operator of the signal. So:

| Recorded id | Marker | Behaviour |
|---|---|---|
| none | any parseable or absent | link on the next verified file or scan sample, silently |
| present | present and matching | reconcile |
| present | absent | skip the root; `marker missing`; alert after 6 h (§8.3). An unmount heals itself when the NAS returns, because the marker rides the NAS filesystem; a genuine deletion needs a re-link |
| present | different id | skip the root; `different library`; critical alert immediately |

### 8.3 Recovery when the marker is deleted

Because the marker is invisible in normal operation, an operator who finds the hidden
`.asiair-commander-library-id` file and deletes it must be guided back, not left with a library
that silently stops reconciling.

- **Detection and alert.** A root with a recorded id and no matching marker fails `rootConfirmed`;
  the scan raises `library_unavailable` (warn) after 6 h, or `library_mismatch` (critical)
  immediately for a different id. Both messages point at Settings › Library.
- **Re-link control.** Library management is global, on the Settings page beside the root
  configuration, because the marker keys the root, not the rig. Each root shows its link state; a
  root in any failed state except `not linked` offers **Re-link**, which calls the link endpoint.
- **Explanatory dialog.** Re-link confirms in plain language what it will do (recreate a hidden
  file named `.asiair-commander-library-id` inside the root), why the file exists (it is how
  Commander confirms the NAS is really mounted before it ever reconciles or deletes), and that
  deleting it pauses reconciliation for that root until it is re-linked. The marker name and the
  root are shown verbatim so the operator recognises the file they removed.
- A re-link that records a different id clears every mark under the root, on every rig (§2.3), so
  marks formed against the wrong filesystem do not keep their clocks.

### 8.4 Several rigs, one root

`model.Config.Roots` is global and `RigProfile` has no roots override, so every managed rig files
into the same tree and the marker baseline is per root.

- **Converging on one id.** Two rigs filing their first frames into one root at once each reach
  `autoLinkRoot`. `EnsureMeta` records the first candidate and returns it to both, and each writes
  or confirms the marker with that arbitrated id, so the on-disk marker always matches the record
  and never trips a self-inflicted `different library` alert (`TestAutoLinkConvergesAcrossTwoRigs`,
  run under `-race`).
- **Clearing across rigs.** `ClearMissingUnderRoot` is deliberately not scoped by rig. A
  rig-scoped clear would relink the root, clear one rig's marks, and leave every other rig's marks
  armed with their original clocks against the newly recorded id (I11a). It selects marked rows
  and filters them in Go with `UnderRoot`, sharing the scan's containment predicate, and clears
  `missing_seen_at` and `missing_acked_at` in batches of 500.
- **Alerts.** The unavailable-root timer is per root; the first rig past the threshold raises, and
  any rig's success clears it (§4.8).
- **Settings.** The link and rescan endpoints are per rig but act on the root, so the Settings
  page routes them through any managed rig.

## 9. Deleting a culled original

### 9.1 The absent branch

`handleAbsentNAS` is the only code that deletes a device original because its archive copy is
missing:

1. **Every disqualifier must be clear** (`absentDeferReasons`, §4.4): the mark has settled or is
   acknowledged, no restore is pending, no re-file or supersede intent is open, the path is under
   a configured root, and that root is confirmed. Otherwise defer, logged at INFO with every
   matching reason, before hashing anything.
2. **Hash the device copy and compare it to the row's recorded `sha256`.** With the NAS copy gone
   this is the only evidence that the original still on the device is the frame that was archived
   and then culled; it replaces the three-way check, which needs a NAS copy that does not exist.
   `sha256` is the row's **current device anchor**: it advances when the device revises the frame
   ([frame-lifecycle §11.2](2026-10-17-frame-lifecycle.md)), so the comparison is always against
   what the untouched device file is recorded to hash to. A hash streamed earlier in the same visit
   is reused rather than reading the device twice. The device-proven data digest is recorded for
   the row from the same stream ([frame-lifecycle §11.8](2026-10-17-frame-lifecycle.md)).
3. **Re-establish the authorization from fresh state** (§2.7): `drainCommands`, re-read the row,
   reload config, re-`Stat` the destination. A copy that reappeared clears the stale mark and
   request and yields to the present-copy path; a disqualifier that arrived defers.
4. **`deviceDeleteFrame`** removes the original and its sidecars, with its own final ±2 s source
   re-`Stat` immediately before the delete; a source that changed in the meantime is a benign
   refusal, counted as `source_changed_pre_delete` and reconciled by the pull path.
5. **Close the row** with `markDeleted`, manner `culled`, or `culled_acked` when the operator
   acknowledged it; if the final `Stat` found the source already gone, nothing was removed and the
   manner is `source_vanished` (that override is applied last, so neither culled label claims a
   deletion that did not happen). When a delete actually happened and `markDeleted` persisted,
   resolve the row (§10.6). A WARN log line records the deletion with both paths and whether it
   was acknowledged; the no-removal case logs its own WARN saying nothing was removed. Each
   manner is listed in `metrics.AllDeleteManners`, and any future manner must be added there
   too. The metrics tests seed the `manner` label and bound its values from that slice, so a
   manner missing from it is never seeded, never bounded, and ships unchecked; no test catches
   the omission.
6. On a **hash mismatch** the device holds different content at that path (a counter-reset run
   overwrote it), so it is *not* the frame the operator culled, and deleting it would destroy a
   never-archived frame. The frame is **held**: that refuses the delete, alerts the operator, and
   takes the row out of the deletion query, so the pass stops re-hashing a full device copy every
   cycle (I4b). The pull path's differ branch reconciles the different content as its own frame.

### 9.2 Re-stamping the settle clock at arming

The verified cull is armed by store migration `migrate5` (schema 4 → 5):

```sql
UPDATE frames SET missing_seen_at = CAST(strftime('%s','now') AS INTEGER)
        WHERE missing_seen_at IS NOT NULL;
```

A store below schema 5 has never had a verified-cull deletion act on its marks. Without the
re-stamp, the first delete pass after a store crosses `migrate5` would find every one of them
older than the settle window and authorize the entire backlog at once, with no notice. The re-stamp
gives every
such mark a fresh full window from the moment the class can act on it (§2.6, I4c, I11b). A store
created at the current schema runs the migration over an empty table.

`migrate5` is also the only statement besides `SetFrameMissing` that writes `missing_seen_at` to a
non-null value, and it touches only rows already marked. It runs before `migrate6` adds
`missing_acked_at`, so it cannot re-stamp an acknowledged row. `migrate6` adds the column
nullable, with no backfill and no index change: `idx_frames_rig_missing` still serves the
candidate queries. **Any later migration of the same
shape must also null `missing_acked_at`**, or it would re-stamp acknowledged rows and leave them
acknowledged, silently skipping the very window it exists to grant, on the highest-risk rows. The
note sits beside `migrate6` in the store.

## 10. Acknowledge

The settle window is right for an *accidental* absence (an unmounted share, a half-finished move,
a deletion the operator wants back) and pure friction for the common case: an operator who culled
a night of bad subs on purpose and wants the ASIAIR's storage back. **Acknowledge**, per frame and
per directory, lets them say so. It waives the settle window and nothing else, and because it is
the only control that makes a deletion happen *sooner*, it sits behind a two-step, server-issued
confirmation.

### 10.1 What Acknowledge waives

**Exactly one thing: the settle timer.** It sets `missing_acked_at`; `absentDeferReasons` then
omits `settling` for that frame. Its effect on each reason:

| Reason | Effect of Acknowledge |
|---|---|
| `not_marked` | unchanged, still blocks |
| `settling` | **waived** |
| `restore_requested` | unchanged, still blocks |
| `refile_in_flight`, `supersede_in_flight` | unchanged, still block |
| `outside_roots` | unchanged, still blocks |
| `root_unconfirmed` | unchanged, still blocks; never waivable |

Every gate outside `absentDeferReasons` is likewise untouched: the per-pass device-identity
confirmation, the removable-media presence check, the source `Stat` and its size and mtime match,
the device-hash match against the recorded `sha256`, the post-hash second evaluation, and the
final source re-`Stat` (I25).

**Acknowledge deletes nothing itself.** It sets a column. The ordinary delete pass acts on it, on
the worker goroutine, with all its usual gates. There is no separate delete path.

### 10.2 What Acknowledge honestly costs

An acknowledged frame is **not** guaranteed to be deleted "if and only if it would have been
deleted after the settle window anyway". The settle window's value is not only elapsed time: a mark
that survives to `seen + settle` has survived every intervening scan pass that reached its
directory (§2.2). Acknowledge reduces that from many observations to **one**, leaving the delete
pass's single fresh `Stat` as the confirming read. On a nested-dataset NAS a child dataset can be
unmounted while the parent, and therefore the library marker `rootConfirmed` reads, stays healthy;
`root_unconfirmed` does not cover that case (§6.6a). That residual is the price of the feature, and
it is acceptable because the operator is asserting first-hand knowledge that these files were culled
on purpose. The confirmation copy therefore claims no more proof than exists.

**The moved-directory case is the dangerous one, and it is a different residual.** Moving or
renaming a target directory under a healthy root produces mass absence (§6.6, §12), and Acknowledge
can destroy the repair:

1. The operator reorganises the NAS, moving `M31/` under `2026/M31/`.
2. The scan marks 300 frames missing. `rootConfirmed` passes; the root was never touched.
3. The panel offers **Acknowledge all 300**. Asked "were these deleted on purpose?", an operator
   who just deliberately acted on exactly those files says yes.
4. Every gate passes: marked, acknowledged, no restore, under a confirmed root, and the device
   hash matches because the original is genuine and untouched. 300 originals are deleted.
5. The NAS copies still exist under the destination path, but nothing follows a move, so the
   frames are single-copy at a path Commander does not track.

The operator answered truthfully; the question was the wrong one. Two mitigations follow:

- The confirmation copy discriminates **deleted** from **moved or renamed** rather than asking the
  general "on purpose?" question (§10.7).
- The group challenge reports whether the **directory itself still exists** (`dir_gone`), with the
  scan's not-exist-only discipline: true only on not-exist, false on any other error or with no
  local filesystem wired. A vanished directory is the move signature, though deleting a whole
  target folder is an ordinary cull that sets it too, so the dialog leads with it neutrally rather
  than asserting a move, which would train the operator to dismiss the one warning on this path.

Three reasons "the next pass" may never come, none of which Acknowledge changes:
`keep_originals` skips the delete pass entirely (so the UI offers no control and the mint refuses,
§10.3); the deletion query still requires `pulled_at` past grace, so a just-filed-then-culled frame
is not deletable on the next pass; and a paused or offline rig runs no pass at all.

### 10.3 Which frames can be acknowledged

The missing panel is not limited to deletable rows: the scan marks `verified`, `deleted`,
`unclassified` and `held` rows, and the panel lists every marked row. Offering "Delete from device"
on a `held` row would target the one class the system *refused* to delete because the device
content mismatched; on a `deleted` row the original is already gone. Acknowledgeability is
therefore guarded in deliberately redundant layers (I30):

| Layer | Per frame | Group |
|---|---|---|
| SQL `WHERE` (authoritative): `AcknowledgeMissing` / `AcknowledgeMissingByIDs` | `verified` or `unclassified`, marked, no restore pending, not already acknowledged, rig-scoped | `verified` only, same other predicates |
| Worker (`serviceAcknowledge`, `classifyAckRefusal`) | `ProvenArchived()` | delegated to the SQL guard |
| Mint (`ackMintRefusal`, the group challenge's classification) | `ProvenArchived()` | non-`verified` rows counted as skipped `not_verified` |
| UI | control on a proven-archived, marked, unrestored, unacknowledged row | **Acknowledge all N** on each directory, absent on `keep_originals` rigs |

The `missing_acked_at IS NULL` predicate keeps the column meaning "when the operator *first*
vouched": a re-acknowledgement never refreshes the timestamp, and because the worker returns early
on an already-acknowledged row (§10.8), a re-ack is not an error.

`model.FrameState.ProvenArchived()` is the single Go definition of the per-frame set; the SQL
guards carry the literal disjunction, which a Go helper cannot reach. The per-frame set is widened
for the cull drain and the group set is not (§11.3, I35). `keep_originals` is refused at the mint
(`ACK_KEEP_ORIGINALS`) and re-checked at confirm.

### 10.4 Interaction with Restore

Acknowledge and Restore are opposites, and the tie breaks toward safety (I29):

- **Restore beats Acknowledge.** `RequestRestore` clears any acknowledgement in the same
  statement. Changing one's mind toward keeping a frame always works, in one click, with no
  confirmation.
- **Acknowledge does not beat Restore.** Acknowledging a frame with a pending restore is refused
  (409 `ACK_RESTORE_PENDING`). Silently discarding an explicit keep-this-frame instruction from
  inside an Acknowledge dialog would be a destructive override.

Even if the two ever coexisted, `absentDeferReasons` still returns `restore_requested` and the
delete defers.

### 10.5 An acknowledgement never outlives its missing mark

If the file comes back (restored, remounted, copied back) the mark is cleared, and a surviving
`missing_acked_at` would be a **pre-armed waiver** on a healthy frame, ready to fire the instant it
went missing again for any reason, including a share that failed to mount (I26).

The invariant is enforced at the **single writer that sets a mark**:

```sql
UPDATE frames SET missing_seen_at=?, missing_acked_at=NULL
WHERE id=? AND missing_seen_at IS NULL
```

`SetFrameMissing` is the only code that moves `missing_seen_at` from null to non-null in normal
operation (`migrate5` only re-stamps rows already marked, §9.2). Its `WHERE` fires on a **fresh**
mark only, so a re-observation never clears a running acknowledgement, and because the clear and
the set are one statement there is no crash window. Enforcing the invariant by enumerating every
clear site instead is fragile: the relink clear is the easy one to miss, and a missed site ships
with every test green. The clear sites (`ClearFrameMissing`, `ClearMissingUnderRoot`, `UpsertFrame`,
`DismissFrame`, `RequestRestore`, `ResolveCulledFrame`) also null it, as defence in depth (§4.1).

**Read discipline.** Write-side enforcement *repairs* on the next mark rather than preventing the
intermediate state: between a clear that missed the acknowledgement and the next fresh mark, a row
could hold `missing_acked_at != 0` with `missing_seen_at == 0`. That is harmless only because the
sole consumer sits behind the `missing_seen_at` test in `absentDeferReasons`. So:

> **`missing_acked_at` is only ever read after testing `missing_seen_at != 0`.**

The frame DTO exposes both fields on every frame endpoint, so the UI's Acknowledged pill tests the
mark too (§4.7). SSE event payloads are snapshots and fall under the same rule: the `frame.deleted`
event for a culled delete is emitted before the row is resolved and still carries the mark, the
acknowledgement and the old `dest_path`.

### 10.6 A completed cull leaves the panel

After a culled delete the row is `deleted`. Leaving its mark would keep it in the panel with an
Acknowledged pill and a live Restore button, and that Restore would be accepted (`RequestRestore`
admits `deleted` rows on purpose) and then fail every hour against an original Commander just
destroyed.

**Clearing the mark alone does not work.** The row keeps a valid `dest_path` pointing at a file
that is permanently gone, `deleted` is a scannable state, and the very next scan re-derives the
mark from the filesystem. The row returns to the panel, `PruneDeleted` skips it forever, and every
culled delete raises a false `library_reconciled` alert claiming a third party removed files
Commander itself removed. A culled row after its delete is structurally indistinguishable from the
both-sides cull the scan exists to protect, and the scan re-derives the mark from disk.

So when a device delete **actually happened**, `handleAbsentNAS` resolves the row out of the scan
with `ResolveCulledFrame`, one statement that nulls `missing_seen_at`, `missing_acked_at`,
`restore_requested_at` **and `dest_path`** (I31). An empty `dest_path` takes the row out of
`ScanTargets`, exactly as `DismissFrame` keeps a tombstone out, and it is honest: the destination
no longer exists. The row leaves the panel, is not re-marked, raises no false alert, and ages out
through `PruneDeleted`. The only cost is that `dest_path` is no longer shown for that row in the
frame detail view.

The resolve runs only when `markDeleted` persisted. If that write failed, the row is still
`verified`, and nulling its `dest_path` would leave it deletion-eligible with no destination,
tripping a false doubly-vanished alert on the next pass
(`TestCulledResolveSkippedWhenMarkDeletedFails`). The two writes are not atomic: a crash between
them leaves one `deleted`, marked, valid-path row that nothing re-resolves (it no longer enters the
deletion query), seen as one stuck panel row with a Restore that fails. That is accepted; a
transaction spanning a device delete is not available.

The **no-removal** case (the final `Stat` found the source already gone, most plausibly a
removable medium pulled between the hash and the delete) must **not** resolve the row. That is a
both-sides cull, whose mark is deliberately kept so a reinserted card can still be restored from
(`TestBothSidesCullKeepsMissingMark`). The resolve is not inside `markDeleted` either, because the
vanished-source path calls `markDeleted` for exactly that case.

With the row resolved, a source that later reappears on the device is re-adopted and pulled like
any frame after an ordinary grace delete. For a culled frame that requires someone to put the file
back on the device, which is exactly the signal the re-adopt branch honours.

### 10.7 The confirmation handshake

**The property, stated precisely:** *a request that has never seen a challenge response cannot
delete anything, and a token cannot be replayed once it has successfully acknowledged* (I27). The
guard is against a request arriving without a preceding challenge: a bookmarked URL, a replayed
form post, a stray `curl` from shell history. It is not, and cannot be, a guard against a client
that deliberately walks both steps. Every call is already authenticated and passes the Host,
Origin and CSRF checks ([architecture §8.3](2026-10-17-architecture.md)); the token is a second,
short-lived, per-target factor on top. It is deliberately not a typed confirmation.

```
  UI                          Server                        Worker
   │  POST …/acknowledge  ──────►│
   │  (no token)                 │  rig configured, not keep_originals, valid target
   │                             │  refuse what can't work
   │                             │  mint token bound to (rig, target, frame ids)
   │  ◄─ 200 {token, expires_at, …server's description…}
   │                             │      (or 409 + code, with the reason)
   │  dialog built from the SERVER's description
   │  [Delete from device]
   │  POST …/acknowledge  ──────►│
   │  {"token": "…"}             │  re-check rig configured, not keep_originals,
   │                             │  valid target
   │                             │  validate scope + expiry, do NOT consume
   │                             │  ──── acknowledge request ────►│ re-read, re-check,
   │                             │  consume ONLY on success       │ set missing_acked_at
   │  ◄──── 204 / 200 {n}        │◄───────────────────────────────│
```

**Token properties.**

| Property | Value | Why |
|---|---|---|
| Source | `crypto/rand`, 32 bytes, hex | unguessable |
| Scope | one `(rig, frame)`, or `(rig, dir)` carrying its bound frame ids | a token for one target cannot acknowledge another |
| Lifetime | `ackTokenTTL`, 60 s, from the server's injectable clock | long enough to read a dialog; testable without sleeping |
| Uses | consumed on **successful** acknowledgement | see below |
| Storage | in-memory map on the server, mutex-guarded, at most `ackTokenMax` (1024) | a pending confirmation must not survive a restart |
| Comparison | `constantEqual` | constant time |
| Logging | INFO on mint and on a successful consume, WARN on a failed validation | a failed validation is the signal that a stray or replayed request was refused |

**The group token carries its frame ids, not just the directory** (I28). Binding only the
directory and acknowledging whatever qualifies at confirm time would let the authorized set grow
between dialog and confirm: a child dataset unmounting inside the 60 s window would turn a dialog
that said "3 frames" into an acknowledgement of the whole directory. So the challenge computes the
candidate ids (`MissingUnderDir`, matching the directory **byte-exactly** against
`filepath.Dir(dest_path)` and never case-folding, since folding would merge two distinct panel
groups into one delete-sooner target) and stores them with the token, and the confirm applies
`id IN (…)` with every per-row guard re-applied. Shrinkage stays safe because the guards refuse
departed rows; growth is not authorized; the dialog's number is the ceiling.

**Consume on success, not on receipt.** The manager's acknowledge call has a 10 s context while
the worker may be inside a device hash for tens of seconds. The request lands on the channel, the
*reply* times out, and `drainCommands` applies the request anyway. A token spent on receipt would
then make the operator's retry fail with 403 and report that nothing happened while the frame is
armed. Two simultaneous confirms can both validate; that is harmless, because the acknowledgement is
idempotent and both land serialized on the same worker goroutine.

**A timeout is not reported as failure.** `context.DeadlineExceeded` maps to **202** with body
`{"code": "ACK_QUEUED", …}` ("the rig is busy; this may already have applied"), written as a
success body, and the token is left unconsumed. A 202 is a success to `fetch`, so the client
reads the code from the success value, not from an `ApiError`, and reloads rather than asserting
either outcome.

**The map is bounded.** Entries are keyed by target, so re-challenging overwrites rather than
accumulates; each mint sweeps expired entries and, if still full, evicts the earliest-expiring one.
`consumeAckToken` compares before deleting, so one tab's confirm cannot delete the fresh token
another tab just minted. Group keys quote both segments (`strconv.Quote`), because rig ids are
validated only for non-emptiness and a raw `|` join would let `(rig "a|x", dir "/p")` and
`(rig "a", dir "x|/p")` collide.

**Request order**, for both routes; after the body decodes (a malformed body is a 400), the rig
is resolved **first**, because frame rows outlive a rig's removal from config and reading
`keep_originals` off a missing profile would panic. Steps 1–3 run for the challenge and the
confirm alike:

1. Rig not configured: 404.
2. `keep_originals`: 409 `ACK_KEEP_ORIGINALS`.
3. Bad frame id, or an empty `dir`: 400.
4. Frame: absent or owned by another rig, 404; then `ackMintRefusal` in mint order (not marked,
   not proven-archived, restore pending, already acknowledged), so a `held` row is explained
   rather than reported as already done. Already acknowledged answers `200 {"already": true}`
   with no token. Group: every row is classified, and zero acknowledgeable rows answer 409
   `ACK_NOTHING_TO_DO`.
5. Mint and describe: per frame `{token, expires_at, dest_path, source_path, state}`; group
   `{token, expires_at, dir, count, dir_gone, skipped: {gone, restore_pending, not_verified, already}}`,
   where `gone` counts `deleted` rows apart from `not_verified` (§13.3).

Steps 4–5 are the challenge only. On the frame route an empty body and `{}` are both challenges,
and on the group route `{"dir": "…"}` is: the handler branches on an empty token after step 3,
not on end-of-input. The **confirm** runs steps
1–3 and then validates the token (403 `ACK_CONFIRMATION_INVALID` otherwise) before handing the
request to the worker. Re-running step 2 at confirm closes the window in which `keep_originals`
toggled on between mint and confirm would persist a pre-armed waiver that fires when it is toggled
back off.

| Code | Status | Meaning |
|---|---|---|
| `ACK_CONFIRMATION_INVALID` | 403 | token missing, wrong, expired, or for another target |
| `ACK_NOT_MISSING` | 409 | the row carries no missing mark |
| `ACK_NOT_VERIFIED` | 409 | the frame is held or deleted (per-frame route only; the group route counts such rows as skipped) |
| `ACK_RESTORE_PENDING` | 409 | cancel the restore first |
| `ACK_KEEP_ORIGINALS` | 409 | the rig keeps originals; nothing would be deleted |
| `ACK_NOTHING_TO_DO` | 409 | a group whose acknowledgeable count is 0 |
| `ACK_QUEUED` | 202 | the rig was busy; the request is queued and may already have applied |

The confirm maps the worker's result onto the same codes, and consumes the token only on a nil
result: nil is 204 (per frame) or `200 {"acknowledged": n}` (group); `context.DeadlineExceeded` is
the 202 above; rig not found or frame not found is 404; not acknowledgeable is 409
`ACK_NOT_VERIFIED`; not missing is 409 `ACK_NOT_MISSING`; restore pending is 409
`ACK_RESTORE_PENDING`; any other error is logged and answers 500, which is the case the reload
below covers.

"No session" is a 401, which the SPA handles globally before reading any code; host, Origin and
CSRF failures are bare 403s without a code and must never be read as an expired confirmation.

**The dialog** is built from the server's description, with a danger-styled confirm. Title: "Delete
this frame from the device?" or "Delete N frames from the device?". Body: the filed copy and the
device original to be removed (group: the directory, the count, and any skipped breakdown), then:

> Only do this if these files were **deleted**. If you moved or renamed the folder, cancel —
> Commander can't follow a move, and the originals on the device are the only remaining copies.
>
> Commander will remove the original from the device on its next pass instead of waiting out the
> reconcile-settle window. It still re-checks that the device copy matches what it archived, but it
> will not wait for further confirmation that the file is really gone. This cannot be undone.

When the group challenge reports `dir_gone`, a problem notice leads: "The folder itself is gone,
not just its files. If you moved or renamed it rather than deleting it, cancel." Footer: **Keep
waiting** / **Delete from device**. The group button's N (every marked row in the directory) and the
dialog's N (the acknowledgeable rows) are deliberately different numbers; labelling the button with
the acknowledgeable count would need a mint call just to render. The copy says "frames", never
"files": one frame pulled from two shares is two rows sharing a `dest_path`, and acknowledging one
leaves its sibling gated normally.

**After confirm** the UI toasts the outcome (the group toast reports the server's actual count) and
reloads on every outcome except the pre-write refusals (`ACK_CONFIRMATION_INVALID` and the 409s,
though `ACK_NOTHING_TO_DO` reloads too, since the group emptied). It reloads on a 500 as well:
`AcknowledgeMissingByIDs` batches in 500s with no spanning transaction, so an error on a later
batch leaves earlier rows acknowledged, and the 500 copy says some frames may have been
acknowledged.

### 10.8 Servicing an acknowledgement on the worker

Both acknowledge requests travel on the worker's restore request channel, whose reply channel is
buffered (capacity 1) because the timeout path abandons the reply while the worker still sends it;
an unbuffered channel would block the worker goroutine forever. The reply is a value carrying the
count and the error, never a field the worker writes and the caller reads.

`serviceAcknowledge` (per frame) re-reads the row and tests, in **canonical order**: the row exists
and belongs to this rig; `missing_seen_at` is set (read before `missing_acked_at`, per §10.5);
already acknowledged returns success idempotently; the state is proven-archived; no restore is
pending. It then calls `AcknowledgeMissing`. A `false` result means the row changed between the read
and the write, so the acknowledgement did **not** apply and the worker never reports success:
`classifyAckRefusal` re-reads and returns the sentinel for whichever guard fails on the re-read,
falling back to "not missing" (`TestAcknowledgeRaceReturnsRefusal`). The mint checks state before
idempotence so it can explain a `held` row; the two orders differ only for a row that is both
acknowledged and not proven-archived, for which no token can be minted, and where they disagree the
worker wins because it holds the write. Success logs a WARN audit line naming the frame, its source
and destination.

`serviceAcknowledgeIDs` (group) calls `AcknowledgeMissingByIDs` with the bound ids, which applies
the group predicate rig-scoped in batches of 500 and returns the rows actually changed, so the
confirm's count stays truthful if some rows were restored or acknowledged meanwhile. It logs one
WARN with the directory and count, only when something was acknowledged, so a scripted or
click-through burst is visible after the fact.

An acknowledgement arriving during a device hash is honoured in that same pass by the post-hash
re-evaluation; a restore arriving during the hash still wins.

### 10.9 Failure modes

| Scenario | Behaviour |
|---|---|
| Acknowledged, then the file reappears | The scan clears the mark and the acknowledgement; a later fresh mark starts unacknowledged. |
| Acknowledged, then the root is relinked | `ClearMissingUnderRoot` clears both; even if it did not, the next fresh mark would. |
| Acknowledged, then Restore | `RequestRestore` clears the acknowledgement and sets the request; the delete defers. |
| Restore pending, then Acknowledge | 409 `ACK_RESTORE_PENDING`; cancel first. |
| Acknowledgement lands mid-hash | The post-hash re-read picks it up; deleted this pass. |
| Restore lands mid-hash on an acknowledged frame | The post-hash re-read sees `restore_requested` and defers. |
| NAS unmounted, the whole library looks culled, operator acknowledges | `root_unconfirmed` still blocks; the acknowledgement sits inert. Must never regress. |
| A child dataset unmounts under a healthy root | Not covered by `root_unconfirmed` (§10.2). Binding the group token to its ids keeps the blast radius at what the dialog showed. |
| The operator moved or renamed a target directory | Every gate passes. Covered only by the copy and the `dir_gone` signal (§10.2, §12). |
| A scan runs after a completed cull | The row was resolved, so it is not re-marked, not listed, raises no alert, and ages out. |
| A group confirm fails partway | Earlier batches stand; the UI reloads and says so; the token is unconsumed, so a retry is idempotent. |
| The source vanishes between the hash and the delete | Nothing removed; closed as `source_vanished`; the mark is kept for a reinserted-card Restore; no alert and no Acknowledged pill (§10.10). |
| `keep_originals` toggled on between mint and confirm | The confirm re-checks it and refuses. |
| Device content changed since filing | The device-hash check fails; held and alerted. |
| A `held` or `deleted` row | No control; the mint refuses `ACK_NOT_VERIFIED`; the SQL guard refuses. |
| A `keep_originals` rig | No control; the mint refuses `ACK_KEEP_ORIGINALS`. |
| A configured rig with no running worker | The confirm answers 404; the mint cannot detect it. Accepted. |
| The worker is busy and the 10 s reply times out | 202 `ACK_QUEUED`; token not consumed; the request still applies; the UI reloads. |
| The directory grows or shrinks between mint and confirm | Only bound ids are acknowledged; departed rows are refused; the returned count is what happened. |
| A token leaked from a response body | Dead within 60 s, one target, and usable only by a caller already holding the session and passing CSRF. |
| The server restarts between challenge and confirm | 403 `ACK_CONFIRMATION_INVALID`; click again. |
| Two tabs challenge the same target | The second mint overwrites; the first tab's confirm gets 403. |
| Already acknowledged | The challenge answers `{"already": true}`; the worker returns success idempotently. |

### 10.10 Why an acknowledged frame that is never deleted stays quiet

Acknowledge waives the settle timer; it does not promise a deletion. Deletion can still not happen,
and the outcomes are signalled differently on purpose:

- **A genuine failure** (the device delete errors) logs a WARN, and the frame stays eligible for
  the next pass.
- **An integrity mismatch** (the device copy no longer hashes to what was archived) holds the frame
  and raises a **critical** `delete_mismatch`. This is the most important of the three.
- **No removal** (the source vanished between the hash and the delete) is neither: nothing was
  lost and nothing failed, most plausibly a removable medium pulled mid-pass. It gets a WARN and
  the `source_vanished` manner, and no alert, because an alert for a benign-by-construction event
  trains the operator to dismiss its category.

An acknowledged frame still blocked by `root_unconfirmed`, grace or a paused rig is silent beyond
its INFO deferral log line; surfacing that reason in the panel is a non-goal (§7).

## 11. Draining culled originals for proven-archived frames

A frame Commander pulled and byte-verified onto the NAS but could not classify (a flat with no
sensor keyword, for example) is filed `unclassified` into the quarantine directory
(`<light root>/unclassified/<rig id>/`). If the operator culls its NAS copy, the original would
otherwise strand on the card permanently: `unclassified` frames are not eligible for normal
cleanup, and they cannot be reclassified once their NAS copy is gone. The cull drain extends the
culled-original deletion to every frame Commander can **prove was archived**.

### 11.1 The unifying principle

The deletion query answers two questions that must be kept apart:

- **NAS copy present → normal cleanup.** The archive is there; delete the working copy off the
  card ([frame-lifecycle §9](2026-10-17-frame-lifecycle.md)). This is legitimately `verified`
  only: an `unclassified` frame is not filed to its real home, so its device original remains the
  authoritative copy until the operator resolves it.
- **NAS copy absent → cull drain.** Its safety rests entirely on the device-hash check against the
  row's recorded `sha256` (§9.1), which an `unclassified` frame satisfies exactly as a `verified`
  one does. The proof is about the **device** copy's identity, not the NAS copy's, which is why it
  holds even for a frame whose archive copy was retargeted or superseded: the device leg compares
  against the device anchor `sha256`, never the archive anchor. `sha256` advances when the device
  revises the frame, so the comparison is always to what the device file is recorded to be
  ([frame-lifecycle §11.12](2026-10-17-frame-lifecycle.md) covers the revision of a culled frame).

> **Cull-delete eligibility = the frame is proven archived (`sha256` recorded, state `verified` or
> `unclassified`) AND its NAS copy is confirmed absent AND every `absentDeferReasons` gate passes
> AND a fresh device hash still equals `sha256`.** Normal cleanup stays `verified` only.

`held` and `deleted` are never cull-deletable: `held` means the device bytes did not match the
recorded hash (the one class deliberately withheld, and the load-bearing exclusion), and `deleted`
has no device original left. `failed` and `dismissed` are not scannable, so the scan never marks
them, and neither is in the deletion query.

### 11.2 The delete pass with the drain

```
delete pass
  ├─ DeletionEligible(rig, graceCutoff)
  │     verified past grace (missing or not)
  │     + unclassified past grace WHERE missing_seen_at IS NOT NULL
  ├─ device identity confirmed; rootOK, settle, open intents once per pass
  └─ per eligible frame:
        stat the NAS copy
          ├─ present → clear any stale mark/request, then normal cleanup
          │            only if state == verified (else skip)
          └─ absent  → handleAbsentNAS (state-agnostic)
```

1. **`DeletionEligible` returns `verified` rows past grace plus `unclassified` rows past grace that
   carry a mark.** A non-culled unclassified frame is never in the query, so its original is
   untouched (I33). The state disjunction sits inside its own parentheses so `rig_id`, grace and a
   recorded `sha256` apply to both branches; written without them, SQL precedence would drop the
   rig and grace scoping from the unclassified branch and authorize other rigs' and pre-grace
   originals (I37).
2. **Normal cleanup is guarded to `verified`**, and the guard sits **after** the stale-mark clear:
   an unclassified copy that reappears has its mark and request cleared and is then skipped, never
   deleted and never held, rather than lingering in the query until the next scan (AC2).
3. **`handleAbsentNAS` and `absentDeferReasons` are state-agnostic**: they read only `sha256`,
   the reconciliation columns, `dest_path` and root state (I34).

The NAS re-`Stat` inside the pass means the drain never fires on a stale mark alone.

Culled unclassified frames are filtered out of the unclassified view client-side, so a culled
frame appears only in the missing panel. Otherwise it would appear in both, the missing panel
offering Acknowledge (drains the original) and the unclassified view offering Discard, which
dismisses the row, nulls its `dest_path`, takes it out of the deletion query and re-strands the
original permanently.

A culled unclassified frame reaches the same sinks as a verified one (`deleted` after a drain,
`held` on a device mismatch or a changed source, and the vanished-source path). One narrow race
has no guard: if the NAS copy reappears in the same pass the device source vanishes, the
vanished-source path marks the row `deleted` without nulling `dest_path`, orphaning the quarantine
file. No device data is lost (the source is already gone) and the ordinary scan reconciles the
orphan; the lost reclassify affordance is accepted.

Two other consumers that might assume a verified history hold under the drain as they are. A `held` frame carrying a
standing restore request stays outside the deletion query, so its original is never cull-deleted;
the restore phase keeps retrying the request on its back-off, re-holding on a device mismatch
without clearing the mark or the request. The `deleted`-row re-adopt in `decide` fires for a
drained unclassified frame only on a deliberate re-imaging of the card, because a successful drain
removed the device original, and re-pulling in that case is correct.

Culled unclassified deletions reuse the `culled` and `culled_acked` manners; no label is scoped to
`verified`.

### 11.3 Acknowledge: per-frame widens, group does not

Per-frame Acknowledge accepts the proven-archived set, so an operator can reclaim a stranded
unclassified frame without waiting out the window. **Group Acknowledge stays `verified` only.**
Every unclassified frame of a rig lives in one flat quarantine directory, so a single "Acknowledge
all" there would drain the entire backlog across all targets and nights in one click, and the
`dir_gone` move signal is inert there because the quarantine directory never vanishes. The group
path's guards (the worker's id-set path delegating to `AcknowledgeMissingByIDs`, that statement's
`state = 'verified'`, and the group challenge's classification, which counts every non-verified row
as skipped `not_verified`) are what keep it so; on the quarantine directory the group button
answers `ACK_NOTHING_TO_DO`. Unclassified frames are therefore reclaimable only one explicit
confirmation at a time.

The invariant is **cull-deletable ⇒ per-frame-acknowledgeable**, both restricted to the
proven-archived, marked set, with acknowledgeability the superset because Acknowledge waives only
the settle window (I35).

### 11.4 Arming the backlog without a re-stamp

The proven-archived unclassified class has no arming migration, so its standing marks are never
re-stamped: a deliberate deviation from §2.6. The drain needs no schema change, and no migration
or schema version marks the point at which the class can act (AC8). Consequently, culled
unclassified marks recorded by a binary that does not drain, and already older than the settle
window when a draining delete pass first meets them, are authorized on that pass, with no fresh
window. Every mark recorded by a draining binary is governed by the ordinary per-frame settle
window, so a store whose unclassified marks were all recorded under the drain is never in that
state.

**Recorded tension.** I11b holds for the verified class and not, for marks recorded by a binary
that does not drain, for the unclassified class. The uncomfortable part is that `unclassified`
frames are the ones Commander could *not* identify, so the operator is least able to tell a
deliberate cull from an accidental removal, and this class receives the least protection for marks
recorded by a binary that does not drain. It is acceptable because the marker gate and the final
±2 s source re-`Stat` still catch the mass-unmount and accidental-vanish cases, and the ongoing settle window still governs every
later cull. Adding a fresh-window re-stamp for this class would be a store migration that re-stamps
only marked `unclassified` rows (and nulls their `missing_acked_at`, §9.2). That re-stamp is not
taken; the residual above is accepted.

### 11.5 Keeping the never-deletable alert honest

`countNeverDeletable` feeds the backlog alert and its low-disk "capture may stop" critical. It
counts `held` and `failed` frames always, and `unclassified` frames only while **not** culled: a
culled unclassified frame is on its way off the device, so it drops out of the count and of the
frames the alert links to, while a non-culled one is still never auto-removed until reclassified
(I36). The predicate is one SQL clause in the store (`ListNeverDeletable`), shared by the count
and by the History view the alert links to, so the two cannot disagree. A culled frame under an
unconfirmed root never actually drains yet is excluded too, matching how verified culls behave.

### 11.6 Acceptance criteria

1. **AC1.** A culled `unclassified` frame with a recorded `sha256`, past grace and settle, whose
   device hash equals `sha256` and whose root marker is confirmed, has its device original deleted
   (unit and integration test).
2. **AC1b.** `DeletionEligible` never returns a matching `unclassified` frame that belongs to a
   different rig or is still inside grace: the query stays rig- and grace-scoped across both state
   branches (I37).
3. **AC2.** An `unclassified` frame whose NAS copy reappears after being marked missing is **not**
   deleted; its mark and restore request are cleared, and it does not linger in the deletion query.
4. **AC3.** A `held` frame is never cull-deleted, even when marked, past settle and past grace.
5. **AC4.** Every `absentDeferReasons` gate blocks an `unclassified` cull exactly as a verified
   one (one test per reason).
6. **AC5.** A device-hash mismatch aborts the delete for an `unclassified` frame and holds it, as
   for a verified one.
7. **AC6.** Culled `unclassified` frames are per-frame acknowledgeable at all four layers (worker,
   mint, SQL, UI); group Acknowledge on the quarantine directory acknowledges nothing; `held` and
   `deleted` are not acknowledgeable (tests, plus a manual UI check).
8. **AC7.** A culled, armed `unclassified` frame does not count toward `countNeverDeletable` and is
   absent from the frames the backlog alert links to; a non-culled one still counts.
9. **AC8.** The drain needs no schema migration and no `CurrentStoreSchemaVersion` of its own.
10. **AC9.** Manual, on real hardware: an operator with stranded culled frames sees them per-frame
    acknowledgeable, and after Acknowledge or after the settle window the device originals leave
    the card and its device line counts them no more.

## 12. Reorganising folders by hand is unsafe

Commander compares hashes pervasively, but only ever to compare bytes at a path it already knows.
**There is no lookup by content**: no query selects a frame by hash, and every presence check reads
only the directory derived from the stored `dest_path`, matching by base name with a single hedge
for case-only renames (§2.5). An out-of-band **move** is therefore indistinguishable from an
out-of-band **delete**:

1. The frame is marked missing and its settle window starts. Root membership is evaluated against
   the **stored** `dest_path`, which still names a path under the light root, so `outside_roots`
   never fires and nothing flags the situation as unusual.
2. Once the window elapses, the absent branch authorizes **deleting the device original**, because
   the device copy still hashes to `sha256`.
3. The moved NAS file is then the only copy, and permanently unreferenced by the database.
4. **Restore** re-files to the *old* `dest_path`, creating a duplicate rather than repairing the
   link, and **Acknowledge** waives the window, so the intuitive button accelerates the device
   deletion (§10.2).

Two facts bound the risk:

- **Editing a template is harmless on its own.** Saving the config validates, saves and hot-swaps
  it; it enumerates no frames and moves nothing. Frames filed afterwards land in the edited
  layout, old rows keep valid paths, and nothing goes missing.
- **Commander moves archived files only through its own tools**, each of which updates the row as
  part of the move: reclassify (unclassified frames only), retarget
  ([routing-and-filing §9](2026-10-17-routing-and-filing.md)) and re-file
  ([routing-and-filing §8](2026-10-17-routing-and-filing.md)). Re-file is the supported way to move
  a library into the current templates' layout. It refuses rows that carry a missing mark or a
  pending restore, and while a re-file intent is open the absent branch defers on
  `refile_in_flight`, so a move in progress never reads as a cull.

The operator-facing guidance follows: reorganise the library with re-file or retarget, never by
hand. A directory moved by hand is repaired by moving it back before the settle window elapses, and
never by acknowledging its frames.

## 13. Triaging the missing panel, and forgetting leftovers

### 13.1 Two kinds of missing row

The panel (§4.7) lists every row carrying a missing mark, and two kinds of row share it:

| Kind | State | Original on the device | What the operator can do |
|---|---|---|---|
| **Actionable** | anything but `deleted` (`verified`, `unclassified`, `held`) | usually still present | Restore; Acknowledge where §10.3 allows |
| **Leftover** | `deleted` | already gone | nothing that needs doing; Restore only if a card still holding the original is reinserted; Forget (§13.4) |

A leftover is most often a frame whose original Commander removed after the ordinary grace period
and whose filed copy the operator later deleted by hand. `deleted` is a scannable state, so the scan
marks it, and `PruneDeleted` keeps it forever because a marked `deleted` row is the both-sides cull
that guards a reinserted card (I1, I19). On a long-running install leftovers grow without bound and
bury the few actionable rows: hundreds of rows on a dozen pages, sorted by path, with every folder
offering **Acknowledge all N** for rows the group acknowledge refuses.

Commander cannot tell an inert leftover from the one I1 protects on its own. A `deleted` row whose
`deleted_at` precedes its `missing_seen_at` was removed from the device before its copy went
missing, but a grace delete whose source had already vanished from a pulled card records the same
ordering, and no column records how a row reached `deleted`. Clearing leftovers is therefore the
operator's call, made explicitly (§13.4), never inferred.

### 13.2 The panel's read path

`GET /api/rigs/{id}/frames?missing=true` partitions marked rows on state alone, with one predicate
in the store that every panel query takes:

| Parameter | Meaning |
|---|---|
| `section` | `actionable` (default) or `leftover`; anything else is a 400 |
| `dir` | Filter the table to one folder, matched byte-exactly against `filepath.Dir(dest_path)` as `MissingUnderDir` matches, never case-folded (§10.7) |
| `q` | Filter the table to rows whose `dest_path` contains `q`, both sides lowercased with `strings.ToLower` |

Both filters are applied in Go after the marked rows are read, never through SQLite `LIKE`, which
folds only ASCII and treats `%` and `_` as wildcards; paging and the section total are computed
after filtering. The response carries the page, the filtered total (`total`), both sections'
unfiltered totals (`totals: {actionable, leftover}`), the `section` it answers for, and the
section's folder stack (`groups`). Each folder entry carries `count` (its rows in the section)
and `ackable`: the rows the group challenge would bind (`verified`, no restore pending, not yet
acknowledged), so the button and the challenge cannot disagree. The folder stack ignores `dir` and
`q`: its counts describe whole folders, which is what the group actions act on, and a clicked
folder stays visible beside its siblings.

The union of the two sections is the unfiltered marked set, so the panel remains a superset of the
power-off verdict's missing check (§4.7).

### 13.3 The panel

- **Actionable section first**, headed with its count, then a search box, the folder stack and
  the paged table. A folder line is a button that sets `dir`; the active folder shows as a
  removable chip. **Acknowledge all N** renders only where `ackable > 0`, names `ackable`, and
  never renders on a `keep_originals` rig.
- **"Already gone from the device (M)"**, a disclosure that starts collapsed. Its body opens with
  one sentence: these frames were removed from both the device and the NAS, nothing is waiting on
  them, Restore works only if a card still holding the original is reinserted, and Forget stops
  tracking them. Then **Forget all M**, the same search, a folder stack with **Forget all N** per
  folder, and the paged table with Restore and Forget per row.
- Search text, folder filter and page are per section; a section's page resets to 1 whenever its
  filter or search changes.
- **An empty actionable section** says so in a sentence. While the rig's `missing_warning` is set
  it reads instead: no frames here need action, and if the warning is still shown after
  Commander's next check, the frame it refers to is under **Already gone from the device**. The
  warning is raised for any marked row whose source the device lists, whatever its state
  (`internal/rig/pull.go:278`; `decide` returns the prior row for a marked row at
  `internal/rig/decide.go:41`), and it is a latch written only at the end of the pull walk, before
  the delete pass (`internal/rig/status.go:127-129`), so it can outlive the deletes that make it
  stale. The panel therefore makes no claim about which row it refers to.
- **The group-acknowledge refusal explains itself.** The challenge counts `deleted` rows as `gone`,
  separately from `not_verified`, and the 409 `ACK_NOTHING_TO_DO` message is built on the server
  from the non-zero counts ("Nothing to acknowledge in this folder: 69 already removed from the
  device, 1 restore pending"). With the button hidden at `ackable = 0` the refusal is reached only
  through a race.

### 13.4 Forget

**Forget** retires leftover rows the operator vouches no card still holds. It performs the write
§10.6 performs after a culled delete: `missing_seen_at`, `missing_acked_at` and `dest_path` are
nulled in one statement, so the row leaves the panel, the next scan cannot re-mark it (an empty
`dest_path` takes it out of `ScanTargets`), and `PruneDeleted` ages it out like any deleted row.
Forget touches no file, on the NAS or the device.

| Rule | Why |
|---|---|
| Only `state = 'deleted'` rows carrying a mark, with no restore pending, are forgettable; the guard is in the `UPDATE`'s `WHERE`, rig-scoped, applied in batches of 500 | An actionable row still has its original on the device, and clearing its mark would let the delete pass treat it as ordinary; a pending restore is the operator's other standing intent, and forgetting would cancel it silently. The operator cancels the restore first. |
| Per row, per folder (**Forget all N** on a folder line) and for the whole section: **Forget all**, **Forget older than 7 days** and **Forget older than 28 days**. A bulk Forget takes an optional folder and an optional age in whole days, measured on `missing_seen_at` (the panel's "Missing since"); it ignores `dir` and `q` | Folder and section scopes match the counts beside them, and a filtered bulk action would need a label that changes under the operator's typing. The age scopes let an operator clear old leftovers while keeping recent ones, whose cull may still be reversed by a reinserted card. |
| A two-step handshake: the challenge mints a token bound to the forgettable ids, with a skipped breakdown (`restore_pending`); the confirm forgets only the bound ids and returns the count actually changed | The same handshake as Acknowledge (§10.7): a set that grows between the dialog and the confirm cannot widen the write. Tokens are keyed separately from acknowledge tokens. |
| Serviced on the rig's worker through the restore request channel, like Acknowledge (§10.8) | The library scan and the delete pass run on the worker, so Forget cannot interleave with a scan that read the row before the write and marks it after. |
| One WARN audit line per confirmed Forget, naming the rig, the folder or section, and the count | A bulk Forget is visible after the fact. |

The dialog states the cost plainly: if a card still holding one of these originals is reinserted,
or the device is listing one right now, Commander pulls it back to the NAS on its next pass. That is
the only consequence. A forgotten row's original is re-adopted as any frame is after an ordinary
grace delete (§10.6), so Forget can put a culled frame back on the NAS but can never lose data.

| Route | Response |
|---|---|
| `POST /api/rigs/{id}/frames/{frameId}/forget` | 204; 400 non-integer frame id; 404 unknown rig or frame; 409 `FORGET_NOT_LEFTOVER` (not a marked `deleted` row) or `FORGET_RESTORE_PENDING`; 202 `FORGET_QUEUED` when the worker's reply timed out (as `ACK_QUEUED`, §10.7) |
| `POST /api/rigs/{id}/missing-leftovers/forget`, body `{"dir"?: "…", "older_than_days"?: n, "token"?: "…"}` | Without a token: 200 `{token, expires_at, count, skipped}`, or 409 `FORGET_NOTHING_TO_DO`; with a token: 200 `{forgotten: n}`, 202 `FORGET_QUEUED` when the worker's reply timed out (the token stays spendable), or 403 `FORGET_CONFIRMATION_INVALID` on a missing, wrong or expired token, or one minted for a different `dir` or `older_than_days`. No `dir` means the whole leftover section; no `older_than_days` means any age; a non-positive or non-integer age is a 400. |

### 13.5 Acceptance criteria

- **AC-T1.** Store: the two sections together equal the unfiltered marked set; totals and folder
  counts match the paged rows; folder counts do not change under `dir` or `q`; `dir` matches
  byte-exactly (a case-variant folder is not merged); `q` matches case-insensitively, non-ASCII
  letters included, and treats `%` and `_` literally.
- **AC-T2.** Web: over a fixture seeded through the real store mixing `verified`, `unclassified`,
  `held`, `deleted`, restore-pending and acknowledged rows, each folder's `ackable` equals the
  `count` of a real group-acknowledge challenge for that folder, with and without `q` and `dir`;
  an invalid `section` is a 400; the 409 `ACK_NOTHING_TO_DO` message names the non-zero skip
  counts with `gone` separate.
- **AC-T3.** Forget (I38–I40): forgetting a marked `deleted` row nulls its mark and `dest_path`;
  the next scan does not re-mark it; `PruneDeleted` later removes it; a `verified`,
  `unclassified` or `held` row, an unmarked row and a restore-pending row are refused at the
  worker and by the SQL guard; a token bound before a row gained a restore request forgets every
  bound row but that one; `older_than_days` binds only rows whose `missing_seen_at` is older than
  that many days; a forgotten row whose source reappears is re-adopted.
- **AC-T4.** Vitest: the folder-line helpers in `web/src/util/`; `MissingFrames` rendered with
  `missingWarning` set and an empty actionable section shows the pointer to the leftovers.
- **AC-T5.** A browser pass on `tools/seedack`, extended with leftover rows across several folders
  and one folder holding only leftovers: the actionable section leads; **Acknowledge all** appears
  only where something is acknowledgeable and shows that count; a folder click filters the table
  and shows a chip; search filters across pages; the leftovers start collapsed; **Forget older
  than 7 days** leaves recent leftovers in place; **Forget all** empties a folder and then the
  section; no horizontal overflow on mobile. The existing
  `uipass.js` checks that expect the raw folder count ("Acknowledge all 3", button differing from
  dialog) change to the `ackable` count.
- **AC-T6.** `make check` and `make integration` pass (Forget extends the `web.Supervisor`
  interface the integration tests fake).
