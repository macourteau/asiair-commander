# Header edits

**Scope.** Editing a FITS card on an archived frame: the mechanism that retargeting
([routing-and-filing §9](2026-10-17-routing-and-filing.md)) introduced for `OBJECT`, generalised to
an allowlist of keywords, and its first new use, setting `FILTER`. It covers the card writer, which
both replaces a card and inserts one that is absent, including an insertion that grows the header by
a 2880-byte block; the edit overlay that records every edit on a row and replaces the
retarget-only overlay; re-applying the overlay on any re-pull; carrying the archive length through
every path an edit can change it on; superseding the archive in place when the edit does not change
its folder; and the API and the Tools page. It hosts the header-edit invariants HE-1–HE-12
(§11) and acceptance criteria (§12). Night numbering, target folding and the retarget preview's night
and spelling reports stay in routing-and-filing §6 and §9; the meaning of the frame-row anchors stays
in [frame-lifecycle §11.1](2026-10-17-frame-lifecycle.md); the delete paths stay in
[delete-safety](2026-10-17-delete-safety.md).

## 1. The problem

A mono camera shot without a filter wheel writes no `FILTER` card
([hardware notes §8.2](2026-10-17-asiair-hardware-notes.md)). Its flats and lights still belong to a
filter: the one the operator put in the light path by hand. Stacking software groups calibration by
that keyword — PixInsight's WBPP matches flats to lights on `FILTER` — so frames without it are
grouped as one filter, and flats from several filters are applied to lights they do not belong to.
The operator's only recourse on the ASIAIR is a per-session filename suffix, which nothing reads as
a filter and which has to be changed and changed back for every filter.

Commander already rewrites an archived frame's header safely for `OBJECT`. Setting `FILTER` is the
same operation with one difference that carries most of this design: the card is usually **absent**,
so it has to be inserted, not replaced, and an insertion can overflow the header's last 2880-byte
block.

The benefit does not depend on routing. The shipped flat and light templates do not name `<FILTER>`
([routing-and-filing §3](2026-10-17-routing-and-filing.md)), so under them an edit moves nothing; the
value is the card itself. An operator whose template does name `<FILTER>` gets the frame re-filed by
the same edit.

## 2. Decisions

| Decision | Choice | Section |
|---|---|---|
| Which keywords can be edited | An allowlist: `OBJECT` (retarget) and `FILTER`. The mechanism is keyword-agnostic; exposing a keyword is a design decision of its own. | §3 |
| Absent card | Inserted immediately before `END`; the header grows by one block when `END` sat in the last slot. | §4 |
| Faithfulness proof | Non-edited cards and the data unit are digested separately in each stream and must match; the edited cards are read back. Replaces the fixed 80-byte complement window. | §4.3 |
| Recording edits | One ordered `header_edits` list per row and one `edit_sha256`; `retarget_sha256` is renamed and `retarget_from` folds into the list. | §5 |
| Re-pull | The existing re-apply rule, applying the whole list. | §6 |
| Archive length | Every edit path returns the edited file's length, and that length becomes `archive_size`. | §7 |
| Same folder | A `FILTER` edit that keeps its route supersedes the archive in place through the device-revision swap, committing every row that shares the path; any edit including `OBJECT`, or changing the route, goes through the filing path. | §8 |

## 3. Which keywords can be edited

`header.EditableKeys` is the allowlist, and the API refuses any other key. Each entry names its
value rule and what the edit means for routing:

| Key | Value rule | Absent card | Routing consequence |
|---|---|---|---|
| `OBJECT` | As retargeting: sanitised (routing-and-filing §4.1), folded onto the target's canonical spelling (§6.6), at most 60 runes. | Refused, as for retargeting (§9.8): a frame without a target is not classified as one. | Re-files under the corrected target; the night is assigned in the new scope (§6.4). Owned by routing-and-filing §9. |
| `FILTER` | Trimmed, 1–60 characters, printable ASCII only (0x20–0x7E, the FITS string character set); stored verbatim, not sanitised. | Inserted (§4.1). | Re-files only when the frame's template names `<FILTER>`; the segment is sanitised when rendered, as for any frame (§4.1). Night scope is `(target, OTA)` and does not involve the filter. |

The writer enforces what it can see on its own: the allowlist, sanitising and the length of `OBJECT`,
the character set and length of `FILTER`, and the card fitting its value field. The worker enforces
what needs the store: folding `OBJECT` onto its canonical spelling and the no-op check.

`FILTER` is stored verbatim because the card is for stacking software, which matches the string as
written; sanitising it would change what WBPP groups on for no routing gain, since rendering already
sanitises. `OBJECT` keeps retargeting's sanitised value, because there the card and the folder must
agree (routing-and-filing §9.3 step 5).

Keywords are excluded for one of three reasons, recorded so the allowlist is extended deliberately:

| Class | Examples | Why not |
|---|---|---|
| Structural | `SIMPLE`, `BITPIX`, `NAXIS*`, `BZERO`, `BSCALE`, `EXTEND`, WCS cards | Change how the data unit is interpreted. The faithfulness proof shows the data bytes are unchanged, not that they still mean the same thing. Never editable. |
| Measured or routing-critical | `IMAGETYP`, `DATE-OBS`, `EXPTIME`, `GAIN`, `OFFSET`, `SET-TEMP`, `INSTRUME`, `XBINNING` | Each re-files under its own rules — `IMAGETYP` moves a frame between templates and ledgers, `DATE-OBS` renumbers nights — and the measured ones are facts calibration matching depends on. Each would need its own section here before it is exposed. |
| Descriptive | `TELESCOP`, `FOCALLEN`, `OBSERVER`, site keywords | Commander does not read them, and the site and observer keywords are on `header.PIIDenylist`. No present use. |

## 4. The card writer

`header.EditCards(src, dst string, edits []CardEdit) (EditResult, error)` replaces
`header.RewriteObject`. A `CardEdit` is a key and a value. It streams `src` to `dst`, never decodes
the data unit, and `fsync`s `dst` before returning, as `RewriteObject` did.

### 4.1 Layout

The writer reads the header blocks up to and including the block holding `END`, as `RewriteObject`
did, and builds the output header:

1. For each edit whose key has **exactly one** value card (columns 9–10 are `= `), that card is
   replaced in place by the canonical card for the key and value. More than one value card refuses,
   as for `OBJECT`.
2. For each edit whose key has **no** value card and whose allowlist entry permits insertion (§3),
   the canonical card is **inserted** immediately before `END`, in the order of `edits`; a key that
   does not permit insertion refuses.
3. `END` follows the last card, and the rest of its block is blank cards.

The canonical card generalises `buildObjectCard`: the key left-justified in columns 1–8, `= ` in
9–10, the value quoted with doubled-quote escaping and blank-padded to the eight-character minimum,
the card space-padded to 80, any comment dropped. For `OBJECT` it produces the same bytes the
frozen builder did, which `TestBuildObjectCard_GoldenBytes` keeps proving; the format is frozen for
every key for the same reason (§6: a re-apply must reproduce an archive byte for byte).

Inserting `k` cards when the `END` block has `f` free slots after `END` leaves the header's length
unchanged when `k ≤ f`, and otherwise grows it by `ceil((k − f) / 36)` blocks. The data unit is
copied after the new header verbatim, including its trailing padding, so a growth shifts every data
byte by the growth and changes nothing else. `data_sha256` is derived from the `END` position
(`header.DataOffset`), so it is the same for the source and the edit
([frame-lifecycle §11](2026-10-17-frame-lifecycle.md)); this is the property the ASIAIR's own
in-place revision relies on (H13).

The writer refuses before writing anything when:

- a key is not on the allowlist, or appears twice in `edits`;
- a value fails its key's rule (§3), or its canonical card does not fit the 70-column value field;
- a key has more than one value card;
- an edit inserts a card and the bytes after `END` in its block are not all blanks — the insertion
  overwrites that padding, and the writer does not discard bytes it did not create (a replace-only
  edit leaves the padding where it is and does not check it, so every frame retargeting accepts stays
  editable);
- there is no `END` within `maxHeaderBlocks`.

ASIAIR frames written without a filter wheel carry two-block headers with 32 to 35 blank slots after
`END` ([hardware notes §8.4](2026-10-17-asiair-hardware-notes.md)), so inserting `FILTER` into them
leaves the length unchanged. Growth is still a supported path, not an error: a header that ends on a
block boundary — `END` in the 36th slot — is a valid FITS file that other producers, other ASIAIR
models and firmware, and a light whose header the device has grown with its WCS (H13) can present,
and refusing it would make the tool's reach depend on a card count the operator cannot see.

### 4.2 Result

`EditResult` carries the output length `OutputSize`, the whole-output `OutputSHA256`, and the proof
digests of §4.3. `OrigCard`, `NewCard` and `Offset` go: they describe one card at one offset, and a
multi-card edit with an insertion has neither.

### 4.3 The faithfulness proof

`RewriteObject` proved faithfulness with complement digests over every byte outside one fixed
80-byte window. An insertion moves every following byte, so a byte window cannot describe it. The
proof is restated in terms of cards and the data unit, computed in the same pass:

- **Card complement.** For each stream, a SHA-256 over the header's cards **in order**, skipping
  every value card whose key is in `edits` and stopping at `END`. Equal digests prove that every
  other card survived, unchanged and in its order.
- **Data digest.** For each stream, the data-unit digest exactly as `data_sha256` computes it (from
  the `END`-derived offset to end of file). Equal digests prove the data unit, padding included,
  survived.

Both digests are computed by the writer over its own buffers, as `RewriteObject`'s complements were.
They are a self-consistency check of the writer, not an independent one, so the worker proves the
result again from the files at rest, with its own reads:

- **Source identity.** The pre-edit archive verify (routing-and-filing §9.3 step 3) is a
  `VerifyReadTee` at the archive's `DataOffset`, so it yields the source's data digest from the bytes
  just proven to be the archive. The row's `data_sha256` is not used: a row the backfill has not
  reached carries none, and such a frame must stay editable.
- **At rest.** A cache-bypassing `VerifyReadTee` of the temp at `OutputSize` must equal
  `OutputSHA256` and its data digest must equal the source's.
- **Length.** The temp's length on disk equals `OutputSize`, and the growth
  `DataOffset(temp) − DataOffset(source)` equals `OutputSize − source length` and lies between 0 and
  `ceil(k / 36) × 2880` for `k` insertions. This pins the growth without trusting the writer's report
  of it.
- **Raw cards.** Both headers are read as raw 80-byte card images, from the first card through the
  block holding `END` — not through `header.ReadHeader`, whose parsed map collapses repeated keys
  (`COMMENT`, `HISTORY`), drops blank cards, trims comments and padding and re-parses numbers, so it
  cannot see a dropped `COMMENT` or a reformatted value. With each replaced value card removed from
  both lists and each inserted card removed from the temp's, the remaining images are equal byte for
  byte and in order, the `END` card included. Each replaced card keeps its index; the inserted cards
  sit, in `edits` order, at the index of the source's `END`, which pins §4.1's layout and keeps an
  insertion out of the leading `SIMPLE`/`BITPIX`/`NAXIS` cards. Each edited card's 80 bytes equal
  the canonical card the worker builds itself, value and comment columns alike.
- **Padding.** The bytes after the `END` card to `DataOffset(temp)` are covered by no digest, so they
  are checked directly. A replace-only edit must leave them equal to the source's, byte for byte, so a
  frame retargeting accepts today stays editable whatever its padding holds; an insertion, whose
  source padding the writer required to be blank, must leave them all blank.
- **Read-back.** For `OBJECT`, the re-classified target equals the sanitised target, as before.

The worker still calls the writer through the injectable `rewriter` seam, so a test can substitute a
deliberately corrupting writer — one that drops a card, reorders two, alters a card's comment,
corrupts a data byte, mis-reports its growth, or pads the grown block with non-blanks — and prove each
is refused before commit by the worker's own checks, with the writer's digests made to agree.

## 5. The edit overlay

### 5.1 Columns

The retarget overlay (routing-and-filing §9.2) records one edit of one keyword. A frame can carry
both a retarget and a filter, and its archive is one file with one hash, so the overlay becomes one
record of every edit:

| Column | Meaning |
|---|---|
| `edit_sha256` | The hash the most recent edit or re-apply produced. Non-empty means "this frame's archive is an edit of its device copy" and is what the re-apply rule keys on. Renamed from `retarget_sha256`, whose meaning it keeps. |
| `header_edits` | JSON array of `{"key", "value", "from"}` in the order each key was **first** edited. `from` is the device copy's original value, or `null` when the device copy has no such card. Editing a key again updates its `value` in place and keeps its position and `from`. |

`retarget_from` folds into the `OBJECT` entry's `from`. `header_edits` is authoritative for what is
applied: every edit, including a retarget, writes the folded, sanitised value into its key's entry,
and the re-apply (§6) applies the entries, where retargeting's applied `target`. `target` and
`filter` keep holding the current, edited values, written from the re-classified edited header; the
worker asserts after each edit that they equal the entries, so the two cannot drift apart.

The order is load-bearing once more than one key can be inserted. An archive edited in two steps —
`FILTER` inserted, later another insertable key — has its inserted cards in that order, and a re-apply from the device copy (§6) applies the
whole list in one pass; listing the entries in first-edit order is what makes the one-pass result
byte-identical to the two-step one. Replacements are positional and do not depend on order.

The columns keep every rule of the retarget overlay: written only from explicit inputs to the filing
path, never by a sticky expression; reset only when re-pulled content is genuinely new; not listed by
the general row update, so a `deleted` frame keeps its overlay.

### 5.2 Migration

One store migration, in one transaction:

1. `ALTER TABLE frames ADD COLUMN header_edits TEXT`.
2. For every row with a non-empty `retarget_sha256`, set `header_edits` to
   `[{"key":"OBJECT","value":<target>,"from":<retarget_from>}]`.
3. Rename `frames.retarget_sha256` and `supersede_intents.retarget_sha256` to `edit_sha256`, and
   drop `frames.retarget_from`.
4. Add `kind` (`NOT NULL DEFAULT 'revision'`, so an intent left open across the upgrade recovers as
   the revision it is), `header_edits` and `filter` to `supersede_intents` (§8).

An upgraded store opens under a binary that knows the new schema only; the store already refuses a
schema newer than its binary, and releases are fixed forward, never rolled back.

## 6. Re-applying edits on a re-pull

The re-apply rule (routing-and-filing §9.4) is unchanged in **when** it fires — the prior row has an
overlay and the incoming file is the same exposure by data digest or whole-file hash — and changes in
**what** it applies: `rig.reapplyEditsIfNeeded` (renamed from `reapplyRetargetIfNeeded`) passes the
row's whole `header_edits` list to the writer, proves the result as §4.3 does against the incoming
file, and files it with the overlay preserved.

Two properties follow from the frozen format and the first-edit order:

- **A byte-identical re-pull reproduces the archive.** Applying the list to the device copy yields the
  same bytes the archive holds, so the differ and the restore paths recognise the healthy archive
  (`archiveHash(f)`), as they do for a retargeted frame.
- **A device-side revision re-derives the archive.** The ASIAIR injecting WCS cards before `END` (H13)
  puts them ahead of the re-inserted `FILTER`; the result differs from the old archive, and the
  re-apply records the new hash and length, as a re-applied retarget already does after a revision.

## 7. The archive length

`archive_size` is the length every archive-side comparison reads at (`VerifyRead`, `sameArchiveAt`,
`provenArchived`, supersede recovery). A comparison at a stale, shorter length does not error: it
hashes a prefix and fails closed against the archive hash — which, on the delete pass, holds the
frame with a critical `delete_mismatch` and pins its device original forever. Retargeting never
changed the length, and four places rely on that. Each takes the edited length instead:

| Place | Before | After |
|---|---|---|
| Edit operation's filing call | `archiveSize: f.ArchiveSize` | `EditResult.OutputSize` |
| Pull path re-apply (`classifyAndFile`) | `srcSize := lf.size` | the size returned by `reapplyEditsIfNeeded` |
| Restore source after a re-apply (`readRestoreSource`) | `archiveSize: n` (device length) | the size returned by `reapplyEditsIfNeeded` |
| Supersede arguments after a re-apply (`resolveRevision`) | the source length | the same returned size, through `classifyAndFile`'s argument; no change of its own |

`reapplyEditsIfNeeded` therefore returns `(path, hash, size)`; it returned no size before. Inside it,
the proof (`proveEdit`) and the at-rest `VerifyRead` of the re-applied temp also take
the output length rather than the incoming one: at the incoming length they refuse every re-apply
that grows the file, which would turn every re-pull of an edited filterless frame into a failure. The
edit operation's admission slot is sized at `archive_size` plus one block, the most a single
insertion grows the file by. The device anchor `sha256` and `size` describe the device copy and are
untouched by every edit path (HE-1).

## 8. An edit that does not change the folder

Retargeting changes the folder whenever the template names the target. A filter edit under a template
without `<FILTER>` — the shipped templates — routes to the folder the frame is already in, and the
edited file's claim of its own name collides with the file it replaces. The filing ladder then
appends the `DATE-OBS` token, so every edited frame would be renamed, which an operator and their
stacking scripts would read as a different file.

The filing ladder is also the wrong tool for a second reason: candidate 0 is the frame's own file
with different content, which the ladder reports as a `rot_clash` alert ("possible rot") on every
such edit, after a full read of the old archive to learn that it differs.

Replacing the bytes under a name the row already owns is what the **supersede** primitive does
([frame-lifecycle §11.9](2026-10-17-frame-lifecycle.md)): the proven archive copy is renamed aside to
a reserved name derived from its hash, the new file claims the freed name with `RenameNoReplace`,
every row sharing the path is committed, and the aside copy is unlinked, under an intent record with
startup recovery and a `supersede_in_flight` deferral in the delete pass. So the edit operation
chooses its commit after the proof of §4.3:

- **In place** — every edited key derives nothing else on the row, and the edited classification
  routes to the directory `dest_path` is in: the edited temp supersedes the archive, keeping its name.
  `FILTER` derives nothing, so this is every `FILTER` edit under a template without `<FILTER>`.
- **Filing path** — otherwise: retargeting's filing path (routing-and-filing §9.3 steps 6–7), as
  today. Every edit that includes `OBJECT` takes it, whether or not its route changes, because the
  target derives the night ordinal (assigned in the new `(target, OTA)` scope) and the panel columns,
  which the filing path writes and the supersede commit does not.

The edit is a supersede **variant**, and differs from the device-revision one in what the commit
writes. The intent gains a `kind` (`revision` or `edit`), the edited `header_edits` and the edited
`filter`. For an `edit`:

| Column | Revision commit | Edit commit |
|---|---|---|
| `sha256`, `size`, `mtime` | the revised device copy's | **unchanged**: the device was not read |
| `archive_sha256`, `archive_size` | the new file's, every sharer | the edited file's, every sharer |
| `edit_sha256` | advanced only on rows that already carry an overlay | set on **every** sharer: each now describes an edited file |
| `header_edits`, `filter` | untouched | the intent's values, every sharer |
| `state`, `pulled_at`, `delete_defer_count` | proven state; grace restarts; defer count reset | **not written**: no device read happened, so the grace clock and the defer count have no reason to change, and a `deleted` row stays `deleted` |

The edit commit's SQL omits the unchanged columns rather than writing back values captured in the
intent, so a recovery replaying it days later cannot restore stale ones.

Startup recovery commits from the intent, so it applies whichever kind the intent records; its
"which file is where" arms are unchanged, because the four steps are. The worker's outcome rules
tighten for an edit:

- **Refusals.** The cross-rig refusal (`supersedeRefusal`: the archive is named by another rig's
  frame) refuses the edit, and so does an open supersede or re-file intent at `dest_path`
  (the intents the delete pass's `supersede_in_flight` and `refile_in_flight` deferrals consult): a second edit over a left-open intent would
  overwrite the intent and orphan the first aside copy where nothing names it. Both report `skipped`
  with the reason.
- **No row committed.** The hourly prune runs off the worker and can remove a `deleted` row mid-swap;
  `supersedeArchive` then leaves the edited file in place and returns no error. The edit re-reads the
  row, as retargeting's step 7 does, and reports `failed` unless it carries the edited `edit_sha256`.
- **Aside not removed.** A failed step-4 unlink leaves the intent open, and the frame deferred by
  `supersede_in_flight` until the next startup's recovery. The edit reports `edited` with a reason
  saying so, and logs it at error, so the operator learns why the frame is not yet released.

A rollback restores the aside copy under the name, as it does for a revision; its precondition is that
the aside copy is intact, which holds for an edit as for a revision, so a `deleted` frame, whose
archive is the only copy, is safe in every window. The edit is re-runnable.

Committing every sharer closes, for this path, the H12 edge retargeting records
(routing-and-filing §9.8): a frame held on both shares has both rows advanced to the edited file in
one transaction, and neither is left naming a file that no longer holds its bytes.

## 9. Supervisor, API and Tools page

### 9.1 Server

`RetargetFrame` and `PreviewRetarget` become `EditFrame(rig, frameID, key, value)` and
`PreviewEdit(rig, ids, key, value)`, keeping their timeouts, their worker channel serviced only in the
`Run` select, and their goroutines (routing-and-filing §9.6).

| Endpoint | Behaviour |
|---|---|
| `POST /api/rigs/{id}/edit/preview` | Body `{ids, key, value}`. One `EditPreview` per id: `frame_id`, `current_value` (empty with `current_present: false` when the card is absent), `old_path`, `new_path`, `eligible`, `reason`, and, for `OBJECT` only, retargeting's `night_approx` and `existing_spellings`. |
| `POST /api/rigs/{id}/edit/apply` | Body `{ids, key, value}`. Streams one `EditResult` per id as NDJSON: `frame_id`, `status` (`edited`, `skipped`, `failed`), `reason`, `old_path`, `new_path`. |

Both are write-guarded; a key off the allowlist or a value failing its rule is a 400 before any
frame is touched. The `/retarget/*` endpoints are removed rather than aliased: the SPA, embedded in
the same binary, is their only client.

The preview rebuilds the classification from the row with the edited value, as retargeting's does,
and must route what apply routes. For `FILTER` that means setting `FilterPresent` from the edited
value, and the row-to-classification rebuild in re-file (which treats an empty stored filter as
present) and in the preview (which treats it as absent) are unified on the preview's reading, which
matches what classification does with an absent card. This changes re-file, not only the preview: an
empty `<FILTER>` that is present renders as a quarantine or a dangling separator where an absent one
collapses (routing-and-filing §3.4), so re-file today can route a filterless frame differently from
the live pull that filed it. Unifying fixes that, with its own regression test
(`TestRefile_FilterlessFrameRoutesAsFiled`).

The preview also reports which commit apply will use (§8): `in_place` for an in-place supersede.

A filter edit is a no-op, reported `skipped`, when the stored filter already equals the value
exactly. Case is not folded: unlike a target, the filter is not part of night identity, and whether
stacking software treats `Ha` and `HA` as one group is the software's decision, not Commander's. The
preview reports the rig's existing filter spellings that equal the value case-insensitively but not
exactly, as routing-and-filing §6.7 does for targets, so a mismatch is visible before apply.

### 9.2 Scope

Eligible: `verified` and `deleted` frames with a `dest_path`, of any frame type, as for retargeting
(routing-and-filing §9.8). Ineligible: `unclassified`, `held`, `failed`, `dismissed`, mid-transfer,
and any frame whose header the writer refuses (§4.1).

### 9.3 Tools page

The Retarget card becomes **Edit header**: a field selector (`Target (OBJECT)`, `Filter (FILTER)`)
above the value input, with the live validation of the selected key. The filter input offers the
distinct filters in the loaded frame list as suggestions, the same page-bounded list the Find frames
filters work on (routing-and-filing §9.8). Preview, apply, the
confirmation and the progress view are retargeting's, with the confirmation naming the keyword and
stating that the device original is not modified. The Find frames list is shared and unchanged, and
a frame without a `FILTER` card shows `—` there.

## 10. Known edges

- **Rows sharing a `dest_path` (H12) on the filing path.** The in-place commit advances every sharer
  (§8). The filing-path commit is retargeting's and updates only the row being edited,
  so a sibling on the other share keeps naming the removed old path and enters reconciliation, as
  routing-and-filing §9.8 records for retargeting.
- **Stray temps and orphans** are not swept, as for retargeting (routing-and-filing §9.8).
- **Removing a card** is out of scope: no use needs it, and it would need its own layout rule.
- **The Tools filters still run in the browser** over the loaded page (routing-and-filing §9.8).

## 11. Invariants HE-1–HE-12

Byte-level and delete-path invariants use real, header-carrying fixtures built with
`internal/header/fitstest`, including one whose `END` sits in the last slot of a block.

| ID | Invariant | Test |
|---|---|---|
| HE-1 | No edit path mutates `sha256` or `size`, or reads-modifies-writes or deletes the device original. | `TestEdit_PreservesDeviceAnchor`, `TestEdit_DeviceOriginalUntouched` |
| HE-2 | An edit refuses unless the archive hashes to `archiveHash(f)` at `archive_size`. | `TestEdit_RefusesDriftedArchive` |
| HE-3 | Every non-edited card is byte-identical and in its order, and the data unit is byte-identical, between source and edit, proven by the worker's own reads; a writer that drops, reorders or alters a card or a card's comment, alters a data byte, mis-reports its growth, or writes non-blank padding is refused before commit even when its own digests agree. | `TestEditCards_OnlyEditedCardsChange`, `TestEdit_RefusesCorruptingWriters` |
| HE-4 | An insertion with a free slot leaves the length unchanged; an insertion into a full last block grows the file by exactly 2880 bytes and leaves `data_sha256` unchanged. | `TestEditCards_InsertWithFreeSlot`, `TestEditCards_InsertGrowsFullBlock` |
| HE-5 | The `OBJECT` canonical card is byte-identical to the frozen retarget card. | `TestBuildObjectCard_GoldenBytes` |
| HE-6 | `archive_size` equals the archive file's length after every edit, re-apply, restore and supersede; no archive-side comparison of a grown file reads a prefix. | `TestEdit_GrownArchiveSizeRecorded`, `TestReapplyEdits_GrownSizeOnDiffer`, `TestRestore_ReappliesGrownEdit`, `TestSupersede_GrownEditSize` |
| HE-7 | The delete pass's three-way check passes for an edited, grown archive, and releases the device original only when the device copy hashes to `sha256`. | `TestDelete_EditedGrownThreeWay` |
| HE-8 | Re-applying the whole edit list to a byte-identical device copy reproduces the archive byte for byte, for an edit list built in several steps. | `TestReapplyEdits_ReproducesMultiStepArchive` |
| HE-9 | The overlay resets only for genuinely new content; an identical re-pull and a device-side revision both preserve and re-apply it. | `TestDiffer_ResetsOverlayOnNewContent`, `TestDiffer_PreservesOverlayOnIdenticalContent`, `TestEditedFrameSurvivesARevision` |
| HE-10 | A `FILTER` edit whose route is unchanged supersedes the archive in place: the frame keeps its path, no `rot_clash` is raised, every sharer is committed to the edited anchors and overlay, and `state`, `pulled_at`, `delete_defer_count` and the device anchor are unchanged; it refuses over an open intent at its path and reports `failed` when no row committed; a crash after any of the four steps recovers to one consistent state, never deletion-eligible against a file that does not hold the row's bytes. An edit including `OBJECT` always takes the filing path. | `TestEdit_InPlaceKeepsPath`, `TestEdit_InPlaceCommitsSharers`, `TestEdit_RefusesOpenIntent`, `TestEdit_InPlacePrunedRowFails`, `TestEdit_InPlaceCrashWindows`, `TestEdit_InPlaceCrashWindowsOnDeletedFrame`, `TestRecoverSupersedeIntent_EditKind`, `TestEdit_ObjectTakesFilingPath` |
| HE-11 | The API and the writer refuse a key off the allowlist and a value outside its rule. | `TestEditCards_RefusesUnlistedKey`, `TestEditAPI_RejectsBadInput` |
| HE-12 | The migration carries every retargeted row's overlay into `edit_sha256` and an `OBJECT` entry of `header_edits`, with `from` equal to the old `retarget_from`, and a supersede intent open across the upgrade recovers as a revision. | `TestMigrateHeaderEdits` |

The retarget invariants INV-1–INV-10 (routing-and-filing §9.9) keep their tests, renamed where the
symbol is.

## 12. Acceptance criteria

1. A filterless ASIAIR flat, edited to `FILTER = 'L'`, archives as a file of the same length whose
   header carries one `FILTER` value card before `END`, whose other cards and data unit are unchanged,
   and which a FITS reader reads with `FILTER = L`; a synthetic frame whose header fills its last block
   archives 2880 bytes longer under the same checks.
2. Under the shipped flat template, the edited flat keeps its folder and its filename.
3. Under a flat template naming `<FILTER>`, the edited flat is re-filed into the `L` folder.
4. The edited frame's device original is deleted by the normal delete pass after its grace period,
   and is never deleted while the archive fails the three-way check.
5. A frame both retargeted and filter-edited, re-pulled after a device-mtime change with identical
   bytes, keeps both edits and its archive hash.
6. An existing install's retargeted frames upgrade with their overlay intact and remain deletable.
7. The Tools page edits `FILTER` on a selection spanning two rigs, shows the preview, applies, and
   shows per-frame progress, on mobile and desktop (`scripts/screenshots.mjs`, Tools surface).

## 13. Changes to other docs

Made in the same change as the implementation:

- **routing-and-filing §9:** §9.1 and §9.2's overlay paragraphs and §9.4 point here for the writer,
  the overlay and the re-apply; §9.6 names the edit endpoints; the scope line names this doc.
- **frame-lifecycle §11.1:** the overlay columns are `edit_sha256` and `header_edits`, and
  `archive_size` can differ from `size` after an edit that inserts a card.
- **frame-lifecycle §11.9:** the four-step claim has a second caller, the in-place edit, whose intent
  kind and commit columns are §8's.
- **delete-safety:** the anchor description says the archive length, like the archive hash, can be
  changed by an edit.
- **hardware notes §8.4:** records the observed header layout of frames written without a filter
  wheel: two header blocks, 37 to 40 cards including `END`, blank padding after `END`.
- **README.md** (design index) and `CLAUDE.md`'s list of docs gain this one.

## 14. Rejected alternatives

| Alternative | Rejected because |
|---|---|
| Record the filter in the database only, leaving the file alone | Stacking software reads the file, not Commander's database; the point of the feature is the card. |
| Free-form editing of any keyword | Structural keywords corrupt interpretation of data the proof shows is byte-identical, and routing-critical ones each carry their own filing rules (§3). |
| Insert only when the last block has a free slot | Covers the observed ASIAIR frames, but makes whether a frame is editable depend on its card count, which varies with producer, firmware and a device-side WCS injection, and which the operator cannot see; the growth path is the same proof with one more length rule. |
| Grow the header by rewriting from the data unit's start, with a new byte-window complement | The data shifts by the growth, so a byte window needs two offsets and a length per stream; the card-and-data digests prove the same thing without reasoning about offsets, and the data digest is the one H13 already relies on. |
| Per-key overlay columns (`filter_from`, …) beside `retarget_*` | A schema change for every key added to the allowlist, and nowhere to record the order of insertions that byte-identical re-apply depends on. |
| Keep the column names `retarget_sha256`/`retarget_from` | The overlay is no longer a retarget's; a misnamed safety column is read wrongly by the next change to it. The rename is mechanical. |
| For an unchanged route, file under a disambiguated name, remove the old file, then move the edited file back with re-file's intent-guarded move | Raises a `rot_clash` per frame; the move-back lands on the path an H12 sibling still names, which then holds different bytes at a different length, so the sibling is held with a critical `delete_mismatch` and its restore cannot clear it — a pinned device original. The in-place supersede has neither problem and commits the sibling. |
| Route every edit in place, then re-file it when its route changed | Re-file writes only `dest_path`, so a retarget would lose its target fold, night assignment and panel columns, which the filing path writes. |
| Edit the archive in place | `localfs` offers no replace, SMB has no atomic one, and an in-place write leaves no intact copy across a crash (routing-and-filing §9.3). |
| Fold filter case the way target case is folded | The filter is not night identity, and folding would rewrite a value the operator typed for software whose matching rules are not Commander's. |
