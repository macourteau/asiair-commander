# Delete safety

**Scope.** This is the page to read before touching any code that can remove a file, whether
from an ASIAIR or from the NAS. It states the principles every delete path obeys, lists each
path that removes a file and the guard on it, and explains the content-hash check, its anchors
and why listing completeness is a safety property. It also holds the numbered data-loss
invariants 1–28, indexes every delete-adjacent invariant series, records the residual risks
accepted on purpose, and gives the checklist for reviewing a delete-path change. It sets
cross-cutting rules and links each mechanism to the doc that specifies it. It does not restate
those docs' invariants.

## 1. Who must read this

Commander deletes originals from the device. Done wrongly, that destroys the only copy of an
exposure. Read this page before changing any of the following:

- the delete pass, culled-original deletion, the cull drain, Acknowledge, restore, or any
  query that selects deletion-eligible frames (`DeletionEligible`, `absentDeferReasons`);
- device identity: the per-session probe, guid binding, Reset identity, or the removable-media
  marker and presence oracle;
- library reconciliation: the library marker, the scan, `missing_seen_at`;
- listing, enumeration or the SMB transport, including the remote tunnel;
- any code that removes, renames over, or prunes a file on the NAS (dismiss, re-file,
  retarget, supersede, restore) or on the device (sidecars, previews, the file browser);
- the frame-row anchors (`sha256`, `size`, `mtime`, `archive_sha256`, `archive_size`) or any
  comparison against them.

The mechanism docs hold the specifications. This page states the rules they share, and §3 maps
each path to its specification.

## 2. Principles

### 2.1 Verification has no off switch

An ASIAIR cannot compute a checksum. Over SMB it can only list, read and delete, so nothing in
the system gives an independent second opinion. A read fault while pulling would be hashed,
written and verified faithfully, and every check would agree with itself. Reading the device
original a second time before deleting it is the only witness that catches this. The product
argument, and why the second read costs almost nothing when spread over a night, is in
[overview §4](2026-10-17-overview.md).

Deletion therefore always runs the full check (§4); no configuration skips it. The only way to
avoid the cost is `keep_originals`, which turns deletion off for the rig. Grace must be
positive on any rig that deletes, because a second device read microseconds after the first
is served from the device's page cache and proves nothing
([frame-lifecycle §9.3](2026-10-17-frame-lifecycle.md)). Config validation rejects a
non-positive effective grace on a deleting rig. The delete pass also refuses to run on one,
because startup does not re-validate a hand-edited `config.yaml`.

### 2.2 The NAS copy is the safe place

Once a frame is verified at rest on the NAS, the device original is working storage awaiting
cleanup. The operator's rules, `grace` and `keep_originals`, decide how long it stays. Commander
honours those rules and does not second-guess them. It never re-pulls a frame the operator
culled from the NAS, never deletes earlier than the rules allow, and never quietly overrides
an operator instruction. Acknowledge is the one control that makes a deletion happen sooner.
It is an explicit operator act and waives only the settle window
([library-reconciliation §1.1, §10](2026-10-17-library-reconciliation.md)).

The converse is just as binding: no device original is deleted unless a byte-verified copy is
on the NAS, or the operator deliberately removed that copy and the culled-original rules have
been satisfied (§3.1).

#### When a missing NAS copy authorizes a delete

A missing NAS copy is read as an operator cull, and a cull authorizes deleting the device
original. Reconciliation therefore draws that conclusion only under two rules besides §2.3
([library-reconciliation §2.1](2026-10-17-library-reconciliation.md)):

- **Nothing is concluded without a matching marker.** A destination root is reconciled only
  while it carries a library marker whose id matches the one recorded for it. An unmounted or
  substituted volume concludes nothing.
- **The irreversible act waits a settle window.** The delete requires fresh confirmation at least
  one settle window (`reconcile_settle`, 24 h by default) after the first recorded and alerted
  observation. The file reappearing, a restore request, a relink or a pause cancels it at
  any point in between. Acknowledge is the only way to waive the window (above).

#### A NAS-side change creates no absence

Because absence reads as a cull, any NAS-side move or removal must never leave a live row's
`dest_path` absent, unless that row is committed to its moved path or held out of the delete pass
by an open intent. Otherwise reconciliation sees the old path missing and, once the mark
settles, `handleAbsentNAS` deletes the device original on a false "culled" premise. This is why
retarget files the edited copy to a different path before removing the old one, why re-file and
supersede run under durable intents that defer the delete pass, and why rename-in-place is
rejected for retarget ([routing-and-filing §9.3, §12](2026-10-17-routing-and-filing.md)). The retarget
step-7 sharer gap in §8 is a known breach of this rule.

### 2.3 Nothing is concluded from an error

A file is absent only when a successful operation reports it absent: an SMB not-found status
from the device, or `fs.ErrNotExist` from the local filesystem. Every other failure concludes
nothing and defers, including a read error, a list error, a permission failure, a timeout or
a store error. Concretely:

- A pre-delete `Stat` error on the device source is a transient condition, retried next pass.
  Only not-found takes the vanished-source branch
  ([frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md)).
- A `VerifyRead` I/O error on the NAS copy defers. Behind a present source, a short file
  (`ErrShortFile`) is corruption and is restored from the device original, never treated as
  absence. Behind a vanished source there is nothing left to restore from, so a truncated copy
  counts as doubly-vanished (§3.1).
- A listing error leaves the listing incomplete (`listingOK` false), and an incomplete listing
  never supports a verdict (§5).
- The presence oracle reads *not present* when the share root cannot be listed, so an
  unreadable medium can never satisfy Safe to power off
  ([device-identity-and-media §7.2](2026-10-17-device-identity-and-media.md)).
- Failing to read the open re-file or supersede intents skips the whole delete pass, because
  without them the pass cannot prove that no move is in flight.
- In reconciliation, every error class leaves the frame unmarked, and a name that differs only
  by case is not absence ([library-reconciliation §2.1, §2.5](2026-10-17-library-reconciliation.md)).
- A restore whose candidate device copies cannot be read leaves the restore request standing.

The rule holds over a remote site too. SMB not-found statuses come from the ASIAIR through
Commander's own smb1client, end to end, so the tunnel cannot turn an outage into "source vanished"
or "slot empty"; a dropped link surfaces as an error ([remote-sites
§7](2026-10-17-remote-sites.md)).

### 2.4 A held frame is never deleted

`held` means a delete-time check found that the device copy, the NAS copy and the recorded hash
disagree. It is the one state where Commander knows the device bytes differ from what was archived.
A held frame is excluded from `DeletionEligible`, from the cull drain, from Acknowledge and from
re-file. It leaves `held` only through a write that proves what the hold doubted: a supersede or a
restore that reads the device copy and verifies the replaced archive copy at rest ([frame-lifecycle
§11.7](2026-10-17-frame-lifecycle.md)). `unclassified` frames are likewise never removed by normal
cleanup. Their device original stays the authoritative copy until the operator resolves them. The
only path that removes one is the cull drain, after the operator deletes its NAS copy
([library-reconciliation §11](2026-10-17-library-reconciliation.md)). `failed` and `dismissed` rows
are never deletion-eligible on either branch: `DeletionEligible` selects only `verified` and culled
`unclassified` rows, and neither state can be cull-deleted (library-reconciliation I32).

### 2.5 Every delete decision is made in Commander

The ASIAIR enforces no delete protection: its guest session may delete any file on any
storage share ([asiair-hardware-notes H7](2026-10-17-asiair-hardware-notes.md)), so every
guard in §3 is Commander's own. Every decision to delete is made in Commander, on the rig's
worker goroutine, from fresh state. Neither the device nor a remote site decides anything:

- **A remote measures and never compares.** A remote site's *hash* call returns digests and the
  identity it probed. It never receives the expected hash, so a bug on the remote side cannot
  produce a false match. A false match is the only failure direction that loses data
  ([remote-sites §7](2026-10-17-remote-sites.md), invariants 1 and 5 in remote-sites §12).
  The decision stays on the worker goroutine for two more reasons: a culled frame's veto (a
  Restore click, a reappearing NAS copy, an opened re-file or supersede) must be able to cancel
  between the hash and the delete, and a decision queued at the remote would act on facts that
  go stale while the link is down ([remote-sites §14](2026-10-17-remote-sites.md)).
- **Every time-based guard runs on Commander's clock.** Grace, settle, defer counts and every
  other timed guard are computed from Commander's clock and store. A remote's wall clock is
  untrusted and never feeds a delete decision (invariant 11 in remote-sites §12).
- **Operator actions never delete a device original.** Acknowledge, Restore and Dismiss reach
  the worker through its request channels. Acknowledge only writes a column, and the ordinary
  delete pass acts on it with every usual check
  ([architecture §3.6](2026-10-17-architecture.md)). Dismiss is the one that removes a file
  itself: it purges an `unclassified` frame's NAS quarantine copy when no other live row shares
  it (§3.2).
- **NAS-side moves never run concurrently with a delete pass.** Retarget, re-file and link are
  serviced at the worker's select point, outside `runCycle`. Heavy, admission-acquiring NAS
  work is never run from `drainCommands` inside `deleteOne`.
- **The file browser is the one operator tool that deletes device files directly.** It deletes
  only what the operator confirmed in a plan, under its own guards (§3.1).

Three further rules apply across every path rather than to one:

- **Nothing is carried across a slice or session boundary.** The delete pass's deadline is
  checked only before a frame starts, never between its hash and its delete. No hash, digest,
  verdict or identity confirmation survives from one slice to the next: each attempt re-reads
  the row, re-hashes both copies and runs in a session whose identity it confirmed itself, and
  the restore phase still runs before the delete phase in every cycle, so a queued restore's
  device original is never destroyed in the same cycle (delete-yield invariants 1, 2 and 8 in
  [frame-lifecycle §12.9](2026-10-17-frame-lifecycle.md)).
- **A path that writes a shared archive file commits every row sharing that `dest_path`.** Two rows
  share one file when a capture is listed on two shares (H12). A path that rewrites or moves the
  file without advancing every sharer leaves rows anchored to bytes the file no longer holds, which
  surface as false holds or as a row that reads as culled; a sharer on another rig's ledger is
  refused rather than silently skipped. The rule is stated over the act, not per path, so it covers
  a path written later ([frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md)). Retarget's old-path
  removal is the recorded exception (§8).
- **Arming a deletion class re-stamps that class's standing marks.** When a change lets a new
  class of frame be cull-deleted, every mark of that class already standing gets a fresh settle
  window, so no backlog accumulated before the class could act is deleted without one
  (library-reconciliation I11b). The proven-archived `unclassified` class is the recorded
  exception (§8).

### 2.6 Identity is proven before any device write or delete

Every SMB session that can pull, delete, write or measure a pre-delete digest re-proves the
device's identity with a live unicast `scan_air` probe while the session is open. This holds
for the pull session, the separate delete session, a remote's own *hash* session and every file
browser session. A probe with no reply refuses the session and fails closed. A reply naming a
different device refuses all pulls and deletes on the rig and raises the critical
`identity_mismatch` alert. A refused pull session also skips that cycle's delete session
entirely, so a cycle that could not prove identity deletes nothing. Identity is per session,
not per cycle, because the delete pass opens its own session after the pull session closes,
and a DHCP handoff in between must not redirect a delete. The truth table, the threat model
and Reset identity are in
[device-identity-and-media §4–§5](2026-10-17-device-identity-and-media.md).

Identity is defence in depth. The real backstop is the content hash (§4): a different device
reached by mistake does not hold a byte-identical file at the same path, so the frame is held
and nothing is removed. That backstop is why the probe's weaker trust boundary, a UDP reply on
a trusted LAN, is acceptable.

## 3. Every path that removes a file

Each row names the guard and the section that specifies it. The guards are not restated here.
Any path that removes a file must appear in one of these tables; a removal path missing from
them is a review finding (§9).

### 3.1 Device files

| Path | What it removes | Guard, in brief | Specified in |
|---|---|---|---|
| Normal cleanup | A `verified` frame's device original | Grace elapsed; identity confirmed in the delete session; the Udisk share presents a readable media marker, which the pass reads and never writes (TF is a removable slot but is not marker-guarded: the marker exists because the Udisk share reports a synthetic volume serial, H10, and no marker path is exercised for the microSD slot; device-identity-and-media A4); source `Stat` matches the row; NAS copy present; no supersede in flight at its `dest_path`; three-way check (§4); final source re-`Stat` within ±2 s (FAT mtime granularity, H4) immediately before `Delete` | [frame-lifecycle §9](2026-10-17-frame-lifecycle.md) |
| Culled original | A `verified` frame's device original whose NAS copy the operator deleted | Every `absentDeferReasons` disqualifier clear (marked; settled or acknowledged; no restore requested; no re-file or supersede in flight; under a confirmed library root); fresh device hash equals `sha256`; mandatory re-evaluation from fresh state after the hash; final re-`Stat` | [library-reconciliation §9.1](2026-10-17-library-reconciliation.md) |
| Cull drain | A proven-archived `unclassified` frame's device original, after its NAS copy was culled | Exactly the culled-original guard. Only a frame carrying a missing mark is eligible; group Acknowledge never reaches the quarantine directory | [library-reconciliation §11](2026-10-17-library-reconciliation.md) |
| Vanished source | Nothing. It records `deleted` for a source already gone | Not-found from a successful operation (§2.3), and the NAS copy re-verified against the archive anchor first. A doubly-vanished frame (NAS copy missing or truncated) is marked `deleted` with a warn alert; a full-length NAS copy whose hash mismatches raises a critical `frame_lost` | [frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md) |
| Sidecars and AppleDouble twins | A deleted frame's recorded sidecars, and the `._<name>` twin of a file this pass removed | Only after the parent is removed; only sidecars recorded for that frame whose extension is in `delete_sidecar_extensions`, each re-`Stat`ed within ±2 s (H4); validation forbids a science extension in that list; an orphan twin is left alone | [frame-lifecycle §9.6, §9.7](2026-10-17-frame-lifecycle.md) |
| Orphan-preview sweep | A preview whose parent frame is no longer on the device | A complete listing (§5); not on a `keep_originals` rig; a removable medium must pass the full cleanup verdict (adopted, not foreign), not just the presence check | [frame-lifecycle §3.3](2026-10-17-frame-lifecycle.md) |
| Commander's own markers | A rig-identity marker file, which Commander never writes, removed if present; a media marker at the share root, relocated into `ASIAIR/`; markers cleared by Reset identity | Rig-id marker file: best-effort on mount, silent on error. Root media marker: only after the copy under `ASIAIR/` is read back and verified. Reset: only on a matching live probe | [device-identity-and-media §4.4, §4.5, §6.3](2026-10-17-device-identity-and-media.md) |
| File browser delete | Files and empty folders the operator selected | Only items in the confirmed plan, each only while its size and mtime match what was shown; folders only while still folders and empty; the unarchived count is recounted and refused if it grew past the acknowledged count; identity per session; never on a host-bound rig; Commander's markers are invisible | [device-file-browser §5.8](2026-10-17-device-file-browser.md), invariant 2 in device-file-browser §8.1 |
| File browser overwrite | A destination file replaced by a copy or upload | Only a destination named in that request's overwrite list, only while it has the size and mtime the operator saw; every write lands on an exclusive temp and reaches its name by a never-replacing rename | [device-file-browser §6.3](2026-10-17-device-file-browser.md), invariant 1 in device-file-browser §8.1 (and invariant 1 in §8.2, which extends it to uploads) |

The file browser's rename moves a device file and removes no content, but the harvester sees
the old path vanish. That interaction, and the layout guard that stops a folder created at a
removable share root from hiding `ASIAIR/<root>`, are in
[device-file-browser §6.5, §7](2026-10-17-device-file-browser.md).

The browser's delete is the only device delete that runs without the three-way check, and it
can remove a file that has no archived copy. That is deliberate. The operator sees the
unarchived count and confirms it. An archived frame deleted this way resolves through the
vanished-source branch with no alert. A culled one raises the doubly-vanished warning.

### 3.2 NAS files

| Path | What it removes | Guard, in brief | Specified in |
|---|---|---|---|
| Dismiss purge | An `unclassified` frame's quarantine copy | Only for `unclassified`; only when no other live row of the rig shares the `dest_path`; the row is tombstoned so the device file is not re-pulled while unchanged | [frame-lifecycle §13](2026-10-17-frame-lifecycle.md) |
| Retarget old path | The pre-edit archive file | Only after the edited file is filed to a different path by a never-replacing rename **and** a re-read of the row proves the overlay committed; the device original is never touched. Known gap: no check for another row sharing the old `dest_path` (§8) | [routing-and-filing §9.3, §9.8](2026-10-17-routing-and-filing.md) |
| Re-file | Moves an archive file; prunes directories it emptied | A move by never-replacing rename under a durable intent, which keeps the frame out of the delete pass while open; every row sharing the `dest_path` moves in one transaction, and a cross-rig sharer refuses the move; pruning removes only empty directories below a configured root, never one holding the library marker | [routing-and-filing §8.6, §8.7, §8.9](2026-10-17-routing-and-filing.md) |
| Supersede claim | The superseded archive copy (set aside), or the installed copy on rollback | Four steps (aside, claim, commit, unlink) under an intent written first, so the bytes always exist under some name; the aside copy is unlinked only after the commit; a cross-rig sharer refuses the swap; an open intent defers the delete pass on that path | [frame-lifecycle §11.9](2026-10-17-frame-lifecycle.md) |
| Truncated-copy restore | A present but short NAS copy, replaced by a fresh pull | Only on `ErrShortFile`, never on absence; the restore-in-progress mark is persisted before the `Remove`, so a crash leaves a standing restore request rather than an armed cull | [frame-lifecycle §9.5](2026-10-17-frame-lifecycle.md), [library-reconciliation §4.4](2026-10-17-library-reconciliation.md) |
| Staging temps | Pull, restore and retarget temps in the staging root | Each temp is created under a unique name in the staging root, never in the light or cal tree, and removed by a `defer` on every exit path | [frame-lifecycle §5.3](2026-10-17-frame-lifecycle.md) |
| Preview cache | Rendered JPEG previews under the data directory | Derived data only, never an archive file; LRU eviction at the size cap; a digest's renders are evicted when dismiss purges its NAS copy; stray `.tmp` files are removed at startup | [features §1](2026-10-17-features.md) |

Commander keeps other scratch outside the archive, such as the file browser's upload staging
under the data directory. Removing it never touches a frame.

## 4. The content-hash check and its anchors

A frame row records two sets of anchors. Each comparison uses the set that belongs to the side
it read. The column semantics are in [frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md):

| Anchor | Columns | Describes | Compared against |
|---|---|---|---|
| Device anchor | `sha256`, `size`, `mtime` | The device copy as last proven. It advances when a supersede adopts the device's own revision of the frame (H13) | Bytes and `Stat` results read from the ASIAIR |
| Archive anchor | `archive_sha256`, `archive_size` | What the file at `dest_path` must hash to, and the length every archive-side read uses | Bytes read from the NAS (`archiveHash` is a field read of `archive_sha256`) |
| Exposure identity | `data_sha256` | The data unit, which survives the device's header-only revision | Used to recognise a revision. It never authorizes a delete |
| Retarget overlay | `retarget_sha256`, `retarget_from` | An audit record of an `OBJECT` edit | Never compared by an archive-side check |

The delete pass's checks, by branch:

- **NAS copy present (normal cleanup).** A fresh full read of the device original must equal
  `sha256`. A cache-bypassing `VerifyRead` of the NAS copy at `archive_size` must equal
  `archive_sha256`. Any mismatch holds the frame with a critical `delete_mismatch` alert.
  Before the hash, a source whose size or mtime disagrees with the row is deferred for a
  bounded number of passes and then held, so a frame the device is still revising is not held
  on first sight ([frame-lifecycle §11.13](2026-10-17-frame-lifecycle.md)).
- **NAS copy absent (culled).** With no NAS copy there is no three-way check. The fresh device
  hash equal to `sha256` is the only evidence that the original on the device is the frame that
  was archived and then culled. A mismatch means a counter-reset run overwrote the path, so the
  frame is held, never deleted
  ([library-reconciliation §9.1](2026-10-17-library-reconciliation.md)).
- **Both branches.** Immediately before `Delete`, the source is re-`Stat`ed and must still
  match within ±2 s. Device storage is FAT-family with 2-second mtime granularity, so exact
  equality would over-fire and the window must not be tightened or widened
  ([asiair-hardware-notes H4](2026-10-17-asiair-hardware-notes.md)). The re-`Stat` catches a
  later capture written at the same path during the tens-of-seconds hash. A change there skips
  the delete without an alert, and the pull path's differ branch reconciles the differing
  content.

The rules for code that compares hashes:

1. A device-side read compares only to `sha256`/`size`/`mtime`. An archive-side read compares
   only to `archive_sha256`/`archive_size`. Never compare across sides, and never fall back from
   one to the other. `sha256` is mutable, so an archive-side fallback to it would move the
   expectation of every frame. The audit of every comparison, labelled by side, is in
   [routing-and-filing §9.5](2026-10-17-routing-and-filing.md).
2. Deletes are authorized only on the whole-file digest. `data_sha256` lets a proven revision
   advance the anchors, and the whole-file check then runs against the advanced anchors.
3. A NAS read that informs a delete bypasses the page cache. A buffered read on a quiet NAS can
   be a cache hit that never touches the disk
   ([frame-lifecycle §6.1](2026-10-17-frame-lifecycle.md)).
4. The device read must be fresh and full. SMB1 offers no device-side cache bypass, so its
   independence from the pull-time read comes from grace and intervening capture I/O
   ([frame-lifecycle §9.3](2026-10-17-frame-lifecycle.md)).
5. Whoever measures the device digest, the local session or a remote, the compare and the
   decision stay in `deleteOne` and `handleAbsentNAS` (§2.5).

The check is what makes every other guard defence in depth. A wrong device, a swapped stick or a
reused path presents different bytes, and different bytes are held, never deleted. The marker and
identity guards make that outcome rare and visible, but deletion safety rests on the hash. That is
not licence to drop them: the media marker is a cheap pre-check that stops Commander scanning or
cleaning a stick it did not adopt, and it answers the one question the identity probe cannot,
whether the stick is the adopted one rather than the box ([device-identity-and-media
§6](2026-10-17-device-identity-and-media.md)).

## 5. Listing completeness

Any verdict derived from a directory listing is only as trustworthy as the listing is complete.
A listing that silently omits entries is indistinguishable from a drained device:

- Safe to power off and drained rest on "every frame listed has been pulled". A truncated
  listing makes un-pulled frames invisible, and the verdict reads Safe while they are still on
  the device ([frame-lifecycle §14](2026-10-17-frame-lifecycle.md)).
- The orphan-preview sweep deletes a preview whose parent is missing from the listing. A
  truncated listing can drop the parent and delete a preview whose frame is still there.
- The file browser builds plans, summaries and zips only from a complete walk (invariant 6 in
  [device-file-browser §8.1](2026-10-17-device-file-browser.md)).

Device-side *vanished* is decided per path by a `Stat` not-found, not from a listing. On the
NAS side, reconciliation lets `ReadDir` only filter candidates and lets an authoritative `Stat`
decide ([library-reconciliation §2.5](2026-10-17-library-reconciliation.md)).

The known failure class is SMB1 TRANS2 FIND fragmentation. A server answers a FIND with only as
many bytes as its send buffer holds and continues under the same MID. A client that reads one
message per MID returns a truncated prefix in name order and reports no error. The junk filter
runs after enumeration, so a truncation window holding only AppleDouble files can leave a short
listing that looks clean, and a false Safe to power off while frames are still on the device.
`github.com/macourteau/smb1client` reads every message of a fragmented response; the device
behaviour is in [asiair-hardware-notes §9.2](2026-10-17-asiair-hardware-notes.md).

The guards:

- `TestLargeDirectoryListsCompletely` (`test/integration`, data-loss item 27) lists a directory
  sized past the fragmentation threshold on the Samba NT1 container and asserts the **exact**
  count. Only a real server fragments, and "at least N" would pass while still losing entries.
- Through a real tunnel, `TestRemoteLargeDirectoryListsCompletely` asserts the same exact count,
  and `TestRemoteListingDropIsError` asserts that a link drop mid-listing surfaces as an error
  with `listingOK` false (invariant 3 in [remote-sites §12](2026-10-17-remote-sites.md)).

The rule: any change to a transport or client path that affects enumeration (an smb1client
bump, a wrapper, a tunnel, a different dialect) needs an exact-count integration test. A smoke
test does not suffice. A transport failure must surface as an error and never as a shorter
listing.

## 6. Data-loss invariants

These are the properties that "never lose a frame" consists of, each with the test that proves
it. The numbers are stable and are cited from code and `test/ADVERSARIAL_COVERAGE.md`. Run the
whole matrix with `CGO_ENABLED=1 go test -race ./...`. Item 27 runs with `make integration`.

| # | Invariant | Proven by | Package |
|---|---|---|---|
| 1 | At-rest verify: a copy whose cache-bypassing read-back hash disagrees with the streamed hash is never filed, and the device original is left intact for re-pull. | `TestPullCorruptAtRestNeverFiles` | `internal/rig` |
| 2 | A streamed byte count that disagrees with the listed size discards the staging temp and files nothing. | `TestPullSizeMismatchDiscardsTemp` | `internal/rig` |
| 3 | Delete integrity: a three-way (device re-hash / recorded anchors / NAS re-hash) failure at delete time holds the frame and never removes the device original. | `TestDeleteCorruptNASHoldsNoDelete` | `internal/rig` |
| 4 | Delete TOCTOU: a source overwritten with different, never-verified content between the delete-pass hashes and the final delete is not deleted. | `TestDeleteSourceOverwrittenBetweenVerifyAndDeleteIsNotLost` | `internal/rig` |
| 5 | A both-sides cull (source gone and NAS copy gone) reaches the terminal `deleted` state, so it stops being re-selected, and raises `frame_lost` at warn, not critical. | `TestDeleteDoublyVanishedConverges` | `internal/rig` |
| 6 | An idempotent re-pull adopts the existing NAS copy without restarting the grace clock or duplicating on disk. | `TestFileIdempotentRePullPreservesPulledAt` | `internal/rig` |
| 7 | Two different frames sharing a canonical name on one rig are both kept under distinct names; the clash raises `rot_clash`. | `TestFileDifferentContentSameNameBothKept` | `internal/rig` |
| 8 | Cross-rig concurrent filing: two rigs filing the same calibration name at the same instant with different content, through a real atomic-exclusive rename over one shared local filesystem, both survive under distinct names; neither overwrites the other. | `TestCrossRigConcurrentFilingNoLoss` | `internal/rig` |
| 9 | A foreign device identity refuses all pulls and deletes on the rig, so Commander never touches another device's data. | `TestIdentityForeignRefusesAndAlerts`, `TestDeletePassMismatchingProbeDeletesNothing` | `internal/rig` |
| 10 | A missing routing-critical value quarantines the frame, and a quarantined (`unclassified`) frame is never eligible for normal cleanup. | `TestQuarantineUnclassifiedNeverDeletable` | `internal/rig` |
| 11 | A malformed header or unexpected value on one frame yields a `failed` row and an alert, never a panic that kills the worker and stops the rig draining. | `TestPerFileRecoverYieldsFailedRow`, `TestReclassifyPanicDoesNotKillWorker` | `internal/rig` |
| 12 | `VerifyRead` hashes exactly `size` bytes, discarding the sector-aligned tail past a FITS length that is not a sector multiple. | `TestVerifyReadDiscardsTail`, `TestVerifyReadTailSizes` | `internal/localfs` |
| 13 | `RenameNoReplace` is a genuine atomic-exclusive rename: it fails distinctly (`ErrExist`) when the target exists rather than replacing it. | `TestRenameNoReplace_Success`, `TestRenameNoReplace_Exists`, `TestRenameNoReplace_Race` | `internal/localfs` |
| 14 | Adversarial `OBJECT` and `INSTRUME` values sanitize to a safe path component or collapse to a quarantine signal: no path traversal, no reserved-name filing. | `TestAdversarialTargets`, `TestQuarantine`; `TestComponent`, `TestComponent_EmptySignalsQuarantine` | `internal/route`, `internal/sanitize` |
| 15 | Never-block: with a deliberately slow or hung SMB device, the HTTP read path (`/healthz`, `/events`, `/api/alerts`, `/api/config`) stays fast. It reads only the in-memory store and status hub, never SMB ([architecture §3](2026-10-17-architecture.md)); measured latency stays about two orders of magnitude below the 2 s device delay. | `TestHTTPReadPathNeverBlocksOnSlowSMB` | `test/neverblock` |
| 16 | A removable medium that is inserted but drained reads Safe to power off, while a genuinely absent one still reads Unknown: both sides of the H5 empty-mount ambiguity, so neither can be fixed by breaking the other. | `TestSafeToPowerOffDrainedRemovableIsSafe`, `TestMediumPresenceDrainedStickIsPresent`, `TestMediumPresenceAbsentStickIsAbsent`, `TestPullRemovableAbsentMediumQuiet` | `internal/rig` |
| 17 | Presence never widens delete permission: a medium with no readable marker authorizes no deletes, and a foreign marker still makes the medium unusable for cleanup (adoption and the orphan-preview sweep) and raises `media_changed`, even though the stick reads present. | `TestUnadoptedMediumNeverAuthorizesDelete`, `TestMediumPresenceForeignIsPresent`, `TestMediumUsableSwapDetected` | `internal/rig` |
| 18 | With no persisted media id there is no prior adoption to protect, so a marked medium is never judged foreign. A marker at the canonical location adopts its existing identity in place (state-loss recovery; two installs sharing a stick do not fight over it), while a marker left at the share root still mints a fresh id so a reset is not undone. | `TestMediumUsableNoPersistedIDReadopts`, `TestMediumUsablePostResetRootMarkerReadopts` | `internal/rig` |
| 19 | A deleted rig leaves no per-share `mediaid:` key behind, so a reused rig id cannot judge a genuinely different stick foreign. | `TestDeleteRigClearsMediaID` | `internal/web` |
| 20 | An unreadable destination volume fails at cycle level in both the pull and delete passes, not per frame: one warn, one deduplicated fleet-wide alert, and the truncated pass is not published as an authoritative pending snapshot. | `TestDestinationUnavailableAbortsCycleAndAlertsOnce`, `TestDestinationUnavailableDeletePassDoesNotFlood` | `internal/rig` |
| 21 | A share root that cannot be listed reads not present, so an unreadable medium can never satisfy Safe to power off. The presence oracle has no fail-open branch. | `TestMediumPresenceUnreadableRootIsNotPresent` | `internal/rig` |
| 22 | Fleet-wide destination alerts (NAS low or unreadable, raised with no rig id so one shared volume yields one row) auto-acknowledge when their condition clears and stand while it holds. Without a fleet-scoped acknowledgement the rig-scoped matcher could never match an empty rig id. | `TestFleetWideAlertsAutoAckOnRecovery`, `TestNASDiskLowStaysWhileSpaceIsLow` | `internal/rig` |
| 23 | An absent removable share is excluded from the published per-share free space and from the tightest-share capacity math. With no medium, the ASIAIR exports its own root filesystem through that share, which would otherwise publish a phantom figure and raise a false `device_disk_low`. The Safe verdict still treats any absent share as Unknown. | `TestAbsentRemovableShareExcludedFromCapacity`, `TestSafeToPowerOffDrainedRemovableIsSafe` | `internal/rig` |
| 24 | `device_disk_low` auto-acknowledges when the tightest share has room again; without that the warning would stand forever once raised. | `TestDeviceDiskLowAutoAcksOnRecovery` | `internal/rig` |
| 25 | An alert raised by a previous process whose condition is already clear at startup is still retired. The resolving-transition latches start "possibly raised", so a restart cannot strand an alert that only auto-acknowledge can clear. | `TestStaleAlertFromPreviousProcessAutoAcks` | `internal/rig` |
| 26 | The unacknowledged-alert badge is seeded from the store at startup, so a restart does not zero it while unacknowledged alerts remain. | `TestStartSeedsUnackedAlertCount` | `internal/manager` |
| 27 | A directory large enough that its TRANS2 FIND response fragments across SMB1 messages enumerates completely. A short listing returned with no error is indistinguishable from a drained device and can produce a false Safe to power off. Only a real server fragments, so this is proven against the Samba container with an exact count (§5). | `TestLargeDirectoryListsCompletely` | `test/integration` |
| 28 | The AppleDouble twin (`._<name>`) of a frame the delete pass proved and removed is removed with it, while an orphan twin whose parent Commander never owned is left alone. | `TestDeleteSweepsAppleDoubleTwin`, `TestDeleteLeavesOrphanAppleDoubleAlone` | `internal/rig` |

Notes on the list:

- **Deliberate pairs.** Items 16 and 17 belong together. Item 16 pins the reporting behaviour
  and item 17 pins that it loosened no delete guard. Items 16 and 23 do the same for capacity:
  an absent share leaves the capacity math but still suppresses Safe. The removable-slot pair
  A1/A2 and the share-disable pair D3/D4 follow the same pattern and are hosted in
  [device-identity-and-media §7.6, §8.3](2026-10-17-device-identity-and-media.md).
- **Item 17 and the delete pass.** The delete pass's own media guard is a presence check (any
  readable, parseable marker), not an identity check. A cycle whose pull phase judged the medium
  foreign still runs its delete pass against it, and the content hash is what stops a swapped
  stick ([device-identity-and-media §6.4](2026-10-17-device-identity-and-media.md)).
- **Benign counterparts.** Over-firing guards are a failure too: a culled frame stuck forever,
  or a false `frame_lost` that trains the operator to ignore the category. A vanished source
  with a good NAS copy is marked deleted cleanly (`TestDeleteSourceVanishedGoodNASDeletes`). A
  path the device reused, over an intact NAS copy, re-pulls with no false `frame_lost`
  (`TestDecideDifferBenignThenPull`).
- **Where they are proven.** Most items are proven at the seam boundaries, with the rig worker
  run against fakes. Items 8 and 13 need genuine OS-level atomicity and run against the real
  local filesystem; item 15 runs a full manager and web stack. `test/integration` (build tag
  `integration`, `make integration`) exercises the real smb1client path against a Samba NT1
  container: byte-exact reads, the H2 empty-root listing, not-found normalisation, marker
  round-trips and delete. The in-memory fakes cannot provide that coverage.

## 7. Where each invariant series lives

Each series has exactly one home. Other docs link to the home and never restate the series.
Series that clash across docs (the I-series and the bare-number series) are always cited with
the doc's name: "library-reconciliation I26", "invariant 4 in architecture §8.2".

| Series | Values | Home | Coverage map |
|---|---|---|---|
| Hardware findings | H1, H2, H4, H5, H7, H10–H13 | [asiair-hardware-notes §2](2026-10-17-asiair-hardware-notes.md) | — |
| Prometheus hazards | P1–P15 | [architecture §17.6](2026-10-17-architecture.md) | — |
| Metrics invariants | I1–I8 | [architecture §17.8](2026-10-17-architecture.md) | `test/METRICS_COVERAGE.md` |
| First-run setup invariants | 1–15 | [architecture §8.2](2026-10-17-architecture.md) | — |
| Dual-port invariants | 1–6 | [architecture §8.5](2026-10-17-architecture.md) | — |
| Validation surfacing | C1–C4 | [architecture §8.8](2026-10-17-architecture.md) | `test/ADVERSARIAL_COVERAGE.md` |
| H13 acceptance criteria | AC 1–26 | [frame-lifecycle §11.15](2026-10-17-frame-lifecycle.md) | — |
| Slice properties | I1, I2 | [frame-lifecycle §12.3, §12.6](2026-10-17-frame-lifecycle.md) | — |
| Delete-yield invariants | 1–11 | [frame-lifecycle §12.9](2026-10-17-frame-lifecycle.md) | `test/COVERAGE.md` |
| Power-off reason | B1 | [frame-lifecycle §14.3](2026-10-17-frame-lifecycle.md) | `test/ADVERSARIAL_COVERAGE.md` |
| **Data-loss invariants** | **1–28** | **§6 of this doc** | `test/ADVERSARIAL_COVERAGE.md` |
| Removable slots | A1–A5 | [device-identity-and-media §7.6](2026-10-17-device-identity-and-media.md) | `test/ADVERSARIAL_COVERAGE.md` |
| Share disable | D1–D4 | [device-identity-and-media §8.3](2026-10-17-device-identity-and-media.md) | `test/ADVERSARIAL_COVERAGE.md` |
| Reconciliation, Acknowledge, cull drain, Forget | I1–I40, with I2a, I4a, I4b, I4c, I11a, I11b | [library-reconciliation §5](2026-10-17-library-reconciliation.md) | `test/ADVERSARIAL_COVERAGE.md` |
| Cull-drain acceptance criteria | AC1, AC1b, AC2–AC9 | [library-reconciliation §11.6](2026-10-17-library-reconciliation.md) | — |
| Night invariants | INV-N1–INV-N7 | [routing-and-filing §6.9](2026-10-17-routing-and-filing.md) | `test/COVERAGE.md` |
| Retarget invariants | INV-1–INV-10 | [routing-and-filing §9.9](2026-10-17-routing-and-filing.md) | `test/COVERAGE.md` |
| Mosaic and re-file invariants | INV-M1–INV-M11 | [routing-and-filing §10](2026-10-17-routing-and-filing.md) | `test/COVERAGE.md` |
| Mosaic and re-file acceptance criteria | AC1–AC40 | [routing-and-filing §11](2026-10-17-routing-and-filing.md) | — |
| Remote invariants | 1–12 | [remote-sites §12](2026-10-17-remote-sites.md) | `test/COVERAGE.md` |
| Remote acceptance criteria | AC1–AC27 | [remote-sites §13](2026-10-17-remote-sites.md) | — |
| File browser invariants | 1–8 | [device-file-browser §8.1](2026-10-17-device-file-browser.md) | — |
| Rename and upload invariants | 1–5 | [device-file-browser §8.2](2026-10-17-device-file-browser.md) | — |
| Tier-1 concurrency invariants | 1–6 | [features §3.9](2026-10-17-features.md) | — |
| Clock-sync invariants | I1–I8 | [features §6.11](2026-10-17-features.md) | `test/COVERAGE.md` |
| Live-feed decisions (not invariants) | 1–7, "d1" | [features §4.1](2026-10-17-features.md) | — |
| File-browser decisions (not invariants) | 1–9 | [device-file-browser §2](2026-10-17-device-file-browser.md) | — |

These are the delete-adjacent series a change to a delete path is most likely to touch:
data-loss 1–28; library-reconciliation I4, I4a, I4b, I5 and I32–I37 (authorizing a culled
delete); library-reconciliation I25–I31 (Acknowledge); INV-M2 (no delete against an archive
mid-move); INV-5 and INV-6 (retarget hash sides and crash windows); delete-yield 1–11
(frame-lifecycle §12.9); remote-sites §12 invariants 1–5, 11 and 12; device-file-browser §8.1
invariants 1–5; A4 and D1–D4.

## 8. Accepted residual risks

These risks are accepted on purpose. Each entry says what bounds it and links the analysis. A
change that widens one of them needs the same deliberate decision.

| Residual | Why it is accepted, and what bounds it | Analysis |
|---|---|---|
| A byte-stable device read fault that reproduces identically on both device reads | The device is the sole source of truth and offers no checksum, so this is undetectable by design. Every other read or write fault on either side is caught | [frame-lifecycle §9.2](2026-10-17-frame-lifecycle.md), [architecture §12](2026-10-17-architecture.md) |
| A transient device read fault masked because grace was too short to evict the device's page cache | Grace is a floor, never zero, on any deleting rig. Even then the NAS copy is proven good by a cache-bypassing read, so no frame is deleted without a verified archive copy; only the second read's independence is weakened | [frame-lifecycle §9.3](2026-10-17-frame-lifecycle.md), [architecture §16](2026-10-17-architecture.md) |
| A wall-clock step in either direction. A forward step makes grace or the reconcile settle window look elapsed early; a backward step briefly pauses deletion | Forward: every delete still runs its full live check, and the settle window's exposure is the same as grace's; both run on wall-clock time deliberately, so the two timing paths stay consistent. Backward: a delete is only delayed, and the jump is not allowed to stall a capacity rig's drain. Neither direction produces a wrong delete | [architecture §12](2026-10-17-architecture.md), [library-reconciliation §6](2026-10-17-library-reconciliation.md) |
| A destination root that is never linked, because it was never writable or its marker is unreadable | Over-firing, not loss: an unlinked root never authorizes a deletion, so its device originals accumulate on the card, and Settings and a log line say why | [library-reconciliation §6.1, §6.8](2026-10-17-library-reconciliation.md) |
| A NAS-side loss (disk error, another program) is indistinguishable from an operator cull, and its device original is deleted once the mark settles | Mitigated by the settle window, Restore while the original is resident, the warn alert when both copies are gone, and the operator's grace | [library-reconciliation §6](2026-10-17-library-reconciliation.md) |
| A mount that fails *below* a healthy linked root, such as a nested dataset, defeats the root-level library marker | Covered by the settle window and the per-directory alert. Repeated passes add nothing here: the safety observations are not independent, and a persistently unmounted dataset gives the same answer every pass. A marker per night directory is not worth its cost | [library-reconciliation §6.4, §6.6a](2026-10-17-library-reconciliation.md) |
| A file renamed on the NAS so that it differs only by case is never reconciled | Safe but non-converging: it warns on every pass and is never marked, so its device original is never cull-deleted. This is an over-firing residual (§6, benign counterparts), accepted because concluding absence from a case-folded near-match could delete the original of a file that is still on the NAS | [library-reconciliation §6.7](2026-10-17-library-reconciliation.md) |
| A moved or renamed target directory reads as a mass cull; a name-normalising SMB server does the same | The settle window and the alert make it noticeable. Relinking or restoring the directory is the repair. Move and rename detection is a non-goal | [library-reconciliation §6, §12](2026-10-17-library-reconciliation.md) |
| Acknowledge reduces the settle window's many independent observations to one fresh `Stat`, and an operator who moved a directory can truthfully confirm it | The operator asserts first-hand knowledge. The dialog asks "deleted, not moved?", the group mint reports when the directory itself is gone, a group token is bound to the ids it showed, and `root_unconfirmed` is never waivable | [library-reconciliation §10.2](2026-10-17-library-reconciliation.md) |
| A culled `unclassified` mark recorded by a store whose build had no cull drain would be armed without a fresh settle window, so library-reconciliation I11b does not hold for that row, although `unclassified` frames are the ones whose deliberate cull the operator can least tell from an accidental removal | No install path produces such a mark: every build carries the drain, so every mark is recorded while the drain can act and waits its full per-frame settle window. The exception stays closed only while that holds; a change that lets a store written without the drain reach it must re-stamp those marks first. The library-marker guard and the final source re-`Stat` still catch a mass unmount. Adding the fresh window would be a store migration that re-stamps only marked `unclassified` rows | [library-reconciliation §11.4](2026-10-17-library-reconciliation.md) |
| The delete pass's media guard accepts any readable marker (presence, not identity) | Every reachable outcome is safe. A swapped stick missing the path routes to vanished-source bookkeeping, which self-heals when the original stick returns. Differing content fails the re-stat or the hash and is held. A delete fires only on byte-identical content at the same path and mtime, which means those bytes are proven on the NAS. An identity check here would need a regression test of a foreign-marked stick holding a byte-identical file at the recorded path | [device-identity-and-media §6.4](2026-10-17-device-identity-and-media.md) |
| A removable medium that mounts and lists but whose frame entries are corrupted away reads drained | No presence oracle can prevent it, because the medium really mounted and the walk really read it. The false-Safe shape of residue without a medium does not occur on real hardware: a zeroed stick exports exactly like an empty slot | [device-identity-and-media §7.3](2026-10-17-device-identity-and-media.md) |
| First-bind trust on a host-bound rig: a wrong device answering at the configured address during the first probe binds durably | The window is small; re-adopt corrects it; the content hash backstops every delete | [device-identity-and-media §4](2026-10-17-device-identity-and-media.md) |
| A spoofed `scan_air` reply on a hostile LAN | Outside the trusted-LAN threat model. The content hash is the guarantee regardless | [device-identity-and-media §5](2026-10-17-device-identity-and-media.md) |
| Retarget step 7 removes the old archive path with no check for another row sharing that `dest_path`, such as the H12 USB/eMMC pair | The device original is never touched and the edited bytes exist at the retargeted path. The sharer row is left pointing at a removed file, so it reads as culled and can later drain its own device original against an archive it no longer tracks. This is a bookkeeping gap, not a loss of the exposure | [routing-and-filing §9.8](2026-10-17-routing-and-filing.md) |
| A crash mid-transfer or mid-retarget leaves a staging temp, or an orphaned old-path archive, behind | There is no startup sweep of stale rig staging temps. Admission's disk reserve still accounts for the space, and no invariant depends on the cleanup | [routing-and-filing §9.8](2026-10-17-routing-and-filing.md), [frame-lifecycle §5.3](2026-10-17-frame-lifecycle.md) |
| A delete whose reply is lost (more frequent over a remote link) reads as a vanished source | The next pass's `Stat` finds nothing and the NAS copy is re-proven, so no data is at risk. The reporting is wrong: a warn "could not delete source" alert, `source_vanished` in place of `device_deleted`, and for a culled frame a false doubly-vanished warning | [remote-sites §7](2026-10-17-remote-sites.md) |
| The gap between the final re-`Stat` and `Delete` grows to a WAN round trip over a remote | The hash-to-re-stat window (tens of seconds) is already the larger exposure and is unchanged | [remote-sites §7](2026-10-17-remote-sites.md) |
| A file browser delete can remove a file that has no archived copy | The operator confirmed the unarchived count, and the run refuses if that count grew. `failed` and `held` rows of deleted files stay in the store | [device-file-browser §9](2026-10-17-device-file-browser.md) |
| Another SMB writer creating a folder named like a configured root at a removable share's root hides `ASIAIR/<root>` from the harvester, so Safe can read while frames sit on the stick | The file browser's layout guard closes its own path; Commander has no harvester-side guard against other writers | [device-file-browser §7](2026-10-17-device-file-browser.md) |
| A rig watching only `Udisk Images` never sees frames the ASIAIR fails over to the eMMC (H12) | The frames are invisible, not lost: they stay on the eMMC. Config validation warns when a rig watches the Udisk share without the eMMC share | [frame-lifecycle §3.4](2026-10-17-frame-lifecycle.md) |
| Missing marks and recorded library ids do not survive loss of the state DB | Missing frames are re-pulled while their originals are resident, bounded by grace. Each root auto-links again on its next verified filing | [library-reconciliation §6](2026-10-17-library-reconciliation.md) |

Night numbering has residuals of its own, such as two scopes sharing a folder minting one
ordinal twice. Those are filing residuals with no delete consequence and are recorded in
[routing-and-filing §6](2026-10-17-routing-and-filing.md).

## 9. Reviewing a change to a delete path

A change touching any path in §3, the anchors in §4, enumeration in §5, device identity or the
Udisk media marker, or reconciliation gets this review before it merges:

1. **An independent adversarial review of the actual diff**, by a reviewer who did not write
   it, asked to find a way the change deletes the wrong file or concludes absence from an
   error.
2. **The three-way check is intact.** Device-side reads compare to `sha256`/`size`/`mtime`,
   archive-side reads to `archive_sha256`/`archive_size`, the NAS read bypasses the cache, the
   compare and decision stay in Commander, and the final re-`Stat` still precedes `Delete`.
   A culled delete still re-hashes the device copy and re-evaluates from fresh state after
   the hash. The delete pass still reads the media marker and never writes, adopts or
   migrates one ([device-identity-and-media §6.4](2026-10-17-device-identity-and-media.md)).
3. **No caller of a removed symbol survives.** That includes the build-tagged packages, which
   the default build and the required `build-test` check both skip:
   `go vet -tags integration ./...` must pass, and `make integration` runs them when Docker is
   available.
4. **An exact-count listing test for any transport change** that affects enumeration (§5).
5. **Every added path is in §3**, with its guard and its specifying section, and every added
   invariant has a named test in the series' coverage map (§7).
6. **Error is not absence** (§2.3): every I/O call the change adds to the path is checked for
   which errors it treats as "gone", and only a successful operation's not-found may be.
7. **A NAS-side change creates no absence** at any live row's `dest_path` (§2.2).
8. **The guards do not over-fire.** A held or deferred frame is visible and can be resolved. A
   guard that strands frames, or raises an alert for a benign case, is a defect too (§6,
   benign counterparts).
9. **Nothing is reused across a slice or session boundary**, nothing runs between a frame's hash
   and its `Delete`, and restore still runs before delete in every cycle (§2.5,
   [frame-lifecycle §12.9](2026-10-17-frame-lifecycle.md)).
10. **A path that writes a shared archive file commits every sharer** of that `dest_path`, and
    refuses when a sharer is out of reach (§2.5).
11. **Arming a deletion class re-stamps its standing marks** with a fresh settle window
    (library-reconciliation I11b, §2.5).
