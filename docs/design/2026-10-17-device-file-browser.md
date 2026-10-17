# Device file browser

**Scope.** The File browser page (`/files`) and the `internal/devfiles` service behind it:
browsing the managed rigs' ASIAIRs, downloading files and folders, copying between rigs (or
between two folders of one rig), creating folders, deleting, uploading one file from the browser,
and renaming one entry in place. It covers the operation lock and the per-rig session cap; the
temp-and-rename write and the overwrite binding; the identity probe every tool session makes; and
the rules that keep the tool honest about, and harmless to, the harvester that runs beside it (the
layout guard, reserved paths and rename refusals). It hosts the file browser invariants 1–8
(§8.1) and the rename and upload invariants 1–5 (§8.2). Both are bare-number series, always
cited with their section ("design device-file-browser §8.1 invariant 3").

## 1. Problem and scope

An operator sometimes needs to move a file between ASIAIRs, or put one on or take one off a
device: a saved reference frame copied from one rig's `Preview/` into a fresh folder on another
rig, a plan file uploaded from the PC, a folder of test exposures removed. Commander already holds
every rig's credentials, its endpoint (remote sites included) and an SMB1 client, so it can relay
the bytes itself rather than routing them through the PC or a separate SMB client. A separate
client has none of Commander's safety rules: no identity check on the device it writes to, no
protection against overwriting by accident, and no knowledge of what the harvester will do with
what it writes.

Three constraints shape the design:

- **It is a device write path.** Outside this tool, Commander's only device file writes are the
  harvester's delete pass (frames, their sidecars, their AppleDouble `._` twins and orphan previews)
  and the identity-marker maintenance (writing and clearing the removable-media marker, removing any
  rig-identity marker file); the one other device write is clock sync setting the device clock over
  the control channel. The tool adds folder creation, exclusive file creation, never-replacing
  rename, mtime setting and user-confirmed deletion. Every one of these goes through the rules of
  §8.
- **The HTTP layer does not normally do SMB I/O.** The never-block rule
  ([architecture §3](2026-10-17-architecture.md)) keeps device I/O inside the rig workers. The
  tool is a scoped exception (§2 decision 2).
- **The harvester keeps running.** The tool neither pauses nor locks it. It must instead say
  what the harvester will do with files the user copies, uploads or deletes, using the
  harvester's own rules, and it must refuse the few operations that would corrupt the
  harvester's view of a share (§7).

It is a tool, used occasionally with the page open, not a core feature expected to work at all
times. An operation may end with the tab. Complexity is kept low accordingly: one attempt plus
three retries, no background jobs, no resume.

Out of scope: operations that survive navigation, resuming a copy or download at an offset, folder
upload and drag-and-drop (each could be added without touching the write path), batch upload,
rate-limiting the tool's writes to a remote rig, and a protected copy of a file the ASIAIR is
rewriting in place (§9).

## 2. The decision

1. **A package of its own, independent of the rig workers.** `internal/devfiles` is reached from
   the web layer through an optional `Deps.DeviceFiles` interface, nil-guarded with
   `503 files_unavailable`. `web.Supervisor` has no file-browser methods, so neither its test
   fakes nor the integration-tagged supervisor know about the tool.
2. **Device I/O runs on the request goroutine.** It is the second scoped exception to the
   never-block rule, beside `Scanner.DeviceShares`
   ([architecture §3.3](2026-10-17-architecture.md)). The service holds no lock shared with a
   worker, the hub or the snapshot path, and its own locks are never held across device I/O
   (§8.1 invariant 7). `TestDevfilesListingNeverBlocksReadPath` (`test/neverblock`) proves the
   dashboard keeps answering while a listing hangs.
3. **Sessions are checked out and checked back in, with one idle session parked per rig for 2
   minutes.** No two requests share a session, so the device layer's
   one-operation-per-connection deadline holds without a mutex. **One copy, upload, rename,
   delete, download or plan walk runs at a time across the tool** (server-enforced,
   `409 busy`), and **at most two tool sessions are open per rig**. With the rig's own worker a
   device sees at most three sessions. Only listings, New folder and upload staging run outside
   the operation lock (§5.3).
4. **Every session the tool opens re-proves the device's identity with a live probe while it is
   open, and fails closed when the probe gets no reply.** Only managed rigs are served: each has
   a worker that binds its guid and publishes its endpoint. A managed rig its worker has not yet
   bound to a guid (a host-bound rig) is read-only, as reset-identity refuses it
   ([device-identity-and-media §4.4](2026-10-17-device-identity-and-media.md)). An unmanaged rig
   never appears in a pane selector and the routes refuse it (`unknown_rig`).
5. **Writes never overwrite by accident.** Each file is written to a fresh, exclusively created
   temporary name, `<name>.<16 hex>.commander.tmp`, which the harvester's junk filter drops
   (`.tmp` suffix), and committed with the device's never-replacing rename. A destination that
   appeared after the plan makes the rename collide, and that file is skipped. The only path
   that removes an existing destination is an overwrite the user chose in that request, and only
   while the destination still has the size and mtime the user saw: a state change skips rather
   than overwrites.
6. **Plans are stateless and explicit.** A plan returns the exact file list; the client echoes it
   back with the user's decisions, and the run touches only those items. A delete removes only
   files whose size and mtime still match what the user confirmed. Retrying one file from the
   report is the same run endpoint with one item.
7. **Progress streams as NDJSON on the request.** Leaving the page or pressing Stop cancels the
   operation. There is no job registry, no SSE field and no server-side history.
8. **The harvester rules the tool shows live in `internal/rig` as pure exported functions**
   (`internal/rig/harvestview.go`) that call the code they restate (the enabled-source merge,
   both data layouts, the walk's junk rules, the preview split), so the Watched badge, the copy
   warning, the unarchived count and the rename refusals cannot drift from what the harvester
   does.
9. **Commander's marker files are invisible to the tool.** It never lists, walks, copies, uploads,
   zips, overwrites, renames or deletes `.asiair-commander-media-id` or the rig-identity marker name
   `.asiair-commander-rig-id` (which Commander never writes, but a file is still hidden), matched
   case-insensitively; a request naming one is refused. Without this, copying a Udisk `ASIAIR/`
   folder to the same location on another rig would plant rig A's media marker on rig B's stick, and
   the media-identity guard ([device-identity-and-media
   §6](2026-10-17-device-identity-and-media.md)) would see a foreign marker. Excluding the markers
   keeps the tool outside the marker system entirely.

## 3. What the user sees

### 3.1 Placement

The tool is its own page, **File browser**, at `/files`, with a nav entry beside Tools
(`web/src/surfaces/DeviceFiles.tsx`). It is not a card on the Tools page because it is the only
tool that acts on the devices rather than on the library, and as a card it would sit far below
the archive-maintenance tools on a phone. The page header says it acts on the devices
themselves, not on the library.

### 3.2 Panes and buttons

**Two panes, headed "Left" and "Right".** They sit side by side on wide screens and stack below
a breakpoint; the headings stay when stacked. Each pane's place (rig, share, folder) is mirrored
in the query string with `replace`, so a reload keeps the place and the screenshot script can
address a folder directly.

Each pane, top to bottom:

- **Rig selector.** "— No rig —" plus every managed rig by name. A rig the snapshot reports
  offline is listed as "‹name› (offline)" and disabled; a rig reached through a remote reads
  "‹name› (via ‹remote›)". Left defaults to the first online rig and Right to the second, or
  none. Both panes may show the same rig, for copying between two folders of one device. A pane
  whose rig goes offline, leaves the config or stops being managed keeps it selected and shows
  a pane notice saying which, rather than silently clearing it. A rig id in the URL that is not
  a managed rig is reported and not added to the selector; an invalid path, an unknown share, a
  file path and a missing mkdir parent each get their own `not_found` wording.
- **Breadcrumb** `‹rig› › ‹share› › dir › dir`, every segment a button, plus Refresh. The rig
  segment returns to the share list; the last segment carries `aria-current`, and after any
  navigation focus moves to the listing.
- **Watched notice.** In a watched folder, an info notice says Commander archives frames saved
  there and then removes them from the rig after grace (or leaves them, on a keep-originals
  rig), and that items there cannot be renamed (§3.7).
- **Toolbar**, in order: Download · Copy to ‹rig› · Copy to same location · Upload · New folder
  · Rename · Delete. Upload and New folder add to the current folder; Rename and Delete act on
  the selection. The copy button names the destination rig, with an arrow toward the other pane
  on wide screens, and reads "Copy to other folder" when both panes show the same rig: "left"
  and "right" mean nothing once the panes stack on a phone. A selection count sits beside the
  buttons ("3 selected · 1.1 GB").
- **Listing.** At the rig level the shares are shown as folders, with no checkboxes: a share
  cannot be selected, downloaded or deleted. Inside a share, folders come first, then files,
  sorted case-insensitively; each row has a checkbox, an icon, the name, the size and the device
  mtime. A watched folder carries a "Watched" badge. A header checkbox selects every row and
  shows indeterminate for a partial selection; navigating clears the selection. One listing is
  fetched per folder opened, with no prefetch and no browser cache. Junk such as `._*` files is
  shown, because a file manager shows what is there; only the markers are hidden (§2
  decision 9). After 3 s of loading the spinner adds "Still trying ‹rig›…", since a dead rig
  takes about 16 s to fail (§5.4). Below the breakpoint names wrap and the mtime moves under the
  name.
- **Unreachable or refused.** A failed listing replaces the rows with a problem empty state, the
  message and "Try again"; the breadcrumb stays. A listing answering `not_found` for the current
  folder (deleted from the other pane, or by the ASIAIR) reads "‹folder› no longer exists on
  ‹rig›", with no retry. The pane does not walk up by itself; one click on a parent segment
  does.

**Buttons.** Enable rules and the reason a button is disabled come from one pure function,
`toolbarState` (`web/src/util/deviceFiles.ts`). `busy` covers the whole page. The listing says
whether the rig accepts writes (`writable: false` for a host-bound rig), which disables every
write button with "Commander is still identifying this ASIAIR; writes are disabled until it
has". A `not_found` pane disables New folder, Upload and every selection button. A pane-level
reason (not writable; the other pane has no rig) is a visible muted line under the toolbar,
because touch devices never show a `title` and a disabled button cannot take focus; per-button
reasons stay on `title` and `aria-describedby`.

| Button | Enabled when | Action |
| --- | --- | --- |
| Download | inside a share, ≥ 1 selected | One file comes down as itself; a folder or several items as one zip. |
| Copy to ‹rig› | ≥ 1 selected; the other pane has a rig, is inside a share and is writable; the destination is not this folder; no selected folder is the destination or its ancestor (same rig and share) | Copies into the other pane's current folder. |
| Copy to same location | ≥ 1 selected; the other pane's rig is writable and **different**; it has a share with this share's name | Copies to the same share and path on the other rig, creating the parent folders. |
| Upload | inside a share; the folder exists; writable; not busy | Picks one file and uploads it into the current folder (§3.7). |
| New folder | inside a share; the folder exists; writable | Opens the New folder dialog (§3.4). |
| Rename | inside a share; exactly one item selected; the folder exists; writable; not busy; the listing does not refuse that entry | Opens the Rename dialog (§3.7). |
| Delete | inside a share, ≥ 1 selected, writable | Opens the delete dialog (§3.6). |

Byte sizes everywhere use the app's one formatter (`formatBytes`, binary values labelled KB, MB,
GB) rather than a second set of MiB labels for this page.

### 3.3 Download

Clicking Download first fetches the download URL with `check=1`, which walks the selection and
returns `{filename, files, bytes, zipped}` or an ordinary JSON error. While it runs the button
shows "Preparing…" with the elapsed seconds, beside a Cancel that aborts the check. An
unreachable rig, a vanished file, a busy tool or an over-cap selection is therefore reported in
the page, not as a browser "Failed - Server problem". When the check succeeds the page triggers
the same URL without `check` through a hidden `<a download>`, and the browser shows its own
progress. A failure after the first byte aborts the response (§5.9), so the browser marks the
download failed instead of saving a truncated file as complete.

### 3.4 New folder

A dialog "New folder in ‹share›/‹dir›" with one field. The name validator (`validateName`, in
util and mirrored by the server, §6.1) rejects a bad name with its message on submit. A taken
name answers `409 exists` inline ("A file or folder named ‹name› already exists here."). On
success the pane opens the folder, since the next step is usually to copy into it. A folder
named like a configured capture root at a removable share's root is refused while `ASIAIR/`
holds any configured root (the layout guard, §7).

### 3.5 The copy dialog

One large dialog carries the whole lifecycle: Checking, Plan or Plan failed, Progress, Report.
Closing and focus follow the state. While Checking, Esc, the scrim and the header X act as
Cancel. During Progress they do nothing: Stop is the only way out, so a stray tap cannot cancel
a multi-GB copy. On the Report the scrim does nothing (the report and its retry rows exist
nowhere else), while Esc, X and Close close it. Entering Progress moves focus to Stop; entering
Plan failed or Report moves it to the dialog title, and the outcome is announced through a live
region. A jsdom test keeps focus inside the dialog across every transition.

1. **Checking.** A spinner, "Checking ‹N› selected items on ‹source rig›…", the elapsed seconds,
   and Cancel, which aborts the plan request.
   - **Plan failed** keeps the dialog and title, shows the error naming which side failed
     (source or destination rig), and offers Try again and Close. `busy` names the running
     operation; `too_many` gives the limit; a reserved-path refusal (§6.3) leads with its own
     sentence.
2. **Plan.** Title "Copy 12 files (1.1 GB) to ‹dest rig›" (or "…to the same location on ‹dest
   rig›"). Every count the plan shows comes from the decision reducer (`conflictDecisions`), so
   it counts what will actually be sent: type-conflicted subtrees and files set to Skip drop out,
   and the watched counts read "up to" once any file is skipped. The body, in order:
   - **Destination**: the path in mono, and "3 folders will be created" when any are, the missing
     ancestors of a mirrored destination included.
   - **Watched warning**, shown only when files land in folders the destination harvester will
     act on: N frames will be archived as the destination rig's frames, filed under its
     telescope and session, then removed from it after grace; and M previews (jpg and other
     excluded types) are not archived and may be removed. On a keep-originals rig both sentences
     say the files stay on the device. **Neither sentence appears for a folder Commander does
     not watch**, such as an unconfigured `Preview/`: there the plan has zero watched counts and
     no `dst_ledger`, and the dialog shows no archiving wording anywhere
     (`TestCopyPlanUnwatchedDestination`, plus a jsdom case).
   - **Conflicts**, when there are any: "4 files already exist on ‹dest rig›", Overwrite all and
     Skip all, then a table of file, copying (size, time), existing (size, time) and a per-row
     Overwrite / Skip radio group, **defaulting to Skip**. The user compares the two size/time
     columns. A row whose existing file is a frame the destination harvester tracks carries a
     hint: either "Not archived yet: overwriting loses it", or that replacing an archived,
     tracked frame with different content raises an integrity alert (the revision
     discriminator's genuine-clash path,
     [frame-lifecycle §11.4](2026-10-17-frame-lifecycle.md)). Type conflicts (a folder where a
     file would go, or the reverse) are listed separately as skipped and cannot be overwritten;
     a folder row gives its file count. Below the breakpoint the table becomes stacked rows.
   - **Footer**: Cancel and a primary button labelled from the decisions ("Copy 12 files",
     "Copy 8 files, skip 4", "Create 3 folders"), disabled when nothing is left to copy or
     create, so a selection of empty folders can still be copied.

   The plan is shown even when nothing needs deciding: its title confirms the transfer size,
   which matters over a tunnel, and one flow is simpler than conditional dialogs.
3. **Progress.** "File 3 of 8 · 412 MB of 1.1 GB", a bar and the current file name. Only the
   file counter and the final outcome are live regions; the bar is a `progressbar` queried on
   demand rather than announced every second. When no `progress` line has arrived for 5 s the
   counts line adds "Waiting for ‹rig› to respond…" (both rigs on a cross-rig copy), cleared by
   the next line; this is a client timer, and retries are not on the wire. Cancel becomes Stop,
   with "Keep this page open; leaving it or locking the phone stops the copy." While a run is
   active, `beforeunload` and the router's blocker ask before the user leaves.
4. **Report, or a toast.** However the run ends, both panes re-list, and again after each
   report-row action. When nothing was skipped or failed and no copied file carries a note, the
   dialog closes with a toast ("Copied 8 files (1.1 GB) to ‹dest rig›"). Otherwise it becomes
   the report, titled "Copied 6 of 8 files to ‹dest rig›" (with a suffix when the run aborted on
   an unreachable or unconfirmed rig), whose denominator is every planned file, chosen skips
   included. Status pills count copied, skipped and failed; a table lists every non-copied row
   and every copied row with a note, with file, outcome, reason and action. Skips by choice come
   from the client's own plan, so every skip is listed. Reasons and actions come from
   `reasonText` (table below). Actions:
   - **Overwrite**, on a row skipped because the file exists (`chose_skip`, `appeared`,
     `changed`). It first opens an inline confirmation when the row is `chose_skip` with
     `dst_ledger` `unarchived`, or when the archive status is unknown (`appeared`, `changed`);
     otherwise it starts the one-item run at once (`overwriteNeedsConfirm`). The confirmation
     text (`overwriteConfirmText`) mentions archiving only when the item's destination folder is
     watched (the plan item's `watched` flag, kept by the client and looked up by `rel`); for an
     unwatched folder it says only that the file changed after the check. The run reads no
     ledger for these rows.
   - **Retry**, on a reason that can resolve on its own (`unreachable`, `denied`, `in_use`,
     `error`, `not_attempted`, `stopped`). `identity`, `source_gone` and `type_conflict` have no
     action. "Retry N failed and not attempted" appears when two or more rows have Retry.
   - Each action is a one-item run, and the row updates in place, with a spinner and the
     5-second waiting text; other row actions are disabled meanwhile, since the run holds the
     operation lock.

#### Stopped or interrupted

A stream that ends without a `summary` line, because the user pressed Stop or because the
connection dropped (a locked phone, a suspended tab, a network failure), means the server has
cancelled the run; it writes nothing after a cancel. The run processes items in the order they
were echoed (a copy's files in plan order; a delete's files, then its folders deepest-first) and
each `progress` line's `current` names the item starting, so the client builds the report
itself:

- rows already received keep their outcome;
- the last `current` item becomes a `stopped` row with Retry. For a copy it reads "Stopped while
  copying. The destination holds either the complete file or nothing under its name; Retry
  copies it again.", which the temp-and-rename commit makes true. For a delete it reads
  "Stopped while deleting; it may already be gone."; a retry of a file already gone counts as
  deleted;
- every later planned item is `not_attempted`, with Retry;
- earlier items with no row count as done;
- both panes re-list.

The title is "Copy stopped" / "Delete stopped" after Stop, and "Copy interrupted — lost
connection to Commander" / "Delete interrupted — …" otherwise; that one flag is the only
difference. A temp file left behind by a failed cleanup is not named in the report; it stays
visible in the pane, where the user can delete it.

#### Report reasons

| Code | Shown as | Action |
| --- | --- | --- |
| `chose_skip` | Already exists: the user chose Skip. | Overwrite |
| `appeared` | Appeared on ‹rig› after the check, so it was not overwritten. | Overwrite |
| `type_conflict` | A file and a folder cannot replace each other. | none |
| `source_gone` | No longer on ‹rig›. | none |
| `changed` | Changed on ‹rig› since it was confirmed: not deleted (delete) / not overwritten (copy, upload). | none (delete); Overwrite (copy, upload) |
| `already_gone` | Counted as deleted, not listed. | — |
| `not_empty` | Folder not removed: ‹rig› reports it still contains files. | none |
| `kept_marker` | Kept: it holds Commander's device-identity file. | none |
| `denied` | ‹rig› refused: access denied. | Retry |
| `in_use` | ‹name› is in use on ‹rig› (the ASIAIR or Commander may be reading or writing it). Never retried automatically. | Retry |
| `unreachable` | ‹rig› is unreachable (tried 4 times). | Retry |
| `identity` | The device at ‹address› is not ‹rig›. Commander will not touch it. | none |
| `watched` | A file would stand where a watched root must go (§6.3). | none |
| `mtime_not_preserved` | Copied; timestamp not preserved, so it shows the copy time. Counts as copied. | none |
| `stopped` | Client-built: stopped while copying / deleting / uploading. | Retry |
| `not_attempted` | Not attempted: ‹rig› became unreachable / Commander could not confirm it was talking to ‹rig› / the operation was stopped / the connection was lost. Server-sent only after a run's `aborted`; client-built after Stop or a lost connection. | Retry |
| `error` | The server's message. | Retry |

### 3.6 The delete dialog

It follows the same lifecycle.

- **Title** "Delete from ‹rig›?", with the selection and its place under it ("'M42' and 2 other
  items in ‹rig› › EMMC Images › Preview").
- **Summary** "3 folders, 120 files, 1.2 GB" ("2 empty folders" when there are no files), then
  one line per extension, largest total first ("10 fits — 1.1 GB", "4 (no extension) — 2 KB").
  Extensions are lower-cased and kept as written, so `fit` and `fits` are separate lines. A
  folder that holds a marker is not predicted here; the run keeps it and its report row says why
  (`kept_marker`).
- **Not archived.** Only when N > 0, a problem notice says N files have no archived copy in the
  library and deleting them removes the only copy, followed by a required checkbox "I
  understand these N files will be lost"; Delete stays disabled until it is ticked. When the
  selection touches watched folders and N = 0, a muted line says every frame in them is
  archived. When K selected files lie outside watched folders, a neutral line says Commander
  keeps no copy of them.
- **Watched root.** When a selected folder is, or contains, a watched root (§5.8): "‹folder› is a
  folder Commander watches. Until it exists again, nothing on ‹share› is archived from ‹rig›
  and Commander will not report it safe to power off. Recreate it with New folder." It is a
  warning, not a refusal (§10).
- **Footer**: Cancel and a danger button labelled from what is deleted ("Delete 120 files",
  "Delete 120 files and 3 folders", "Delete 2 folders").

Progress reads "Deleted 41 of 123 items". The report lists failures and skips with the actions
of the reason table; a `changed` row has no action, since a fresh Delete shows a fresh summary.
A delete with nothing to report ends with a toast, and both panes re-list either way, because
the other pane may show the same rig. When the run answers `409 unarchived`, the dialog returns
to its confirmation with the checkbox unticked, its label carrying the fresh N, and a notice
giving the fresh and the confirmed counts and saying nothing was deleted.

### 3.7 Rename and upload

#### Rename

Rename's toolbar reasons: nothing selected ("Select one file or folder to rename."), several
("Rename works on one item at a time."), or the entry's refusal sentence (§6.5). In a watched
folder under an enabled source, where nothing can be renamed, the refusal is said once, in the
folder's watched notice ("Items here can't be renamed."), with Rename disabled and that reason
on hover. Every other per-entry refusal (R2's old-name half, R4, and an R1 caused only by a
disabled source or by a root only the store keeps in play, where no watched notice shows)
appears as a visible line under the toolbar while that one item is selected. The pane's polite
live region carries the same refusal with the selection count, and in a watched folder says
"Items here can't be renamed." once, on entering it.

The dialog, "Rename ‹name›" with "in ‹rig› › ‹share› › ‹dir›" under it, has one field, "New
name", holding the current name with focus. For a file the stem before the last dot is
selected, so typing keeps the extension; for a folder, a dotfile or a name without a dot, the
whole name (`renameSelection`). Validation runs on submit, as in New folder, so a name passing
through an invalid state while typed is never announced; an unchanged name reads "That is
already its name." A case-only change is sent to the device's never-replacing rename like any
other. While submitting, Esc and the scrim are blocked and after 5 s the waiting text appears.
On success the dialog closes, both panes re-list, the renamed entry is selected and scrolled
into view, and a toast says "Renamed ‹old› to ‹new›". The other pane does not follow a renamed
folder: a pane showing it shows the "no longer exists" state.

Answers shown inline, the field staying editable: `exists` in one wording whatever the
collision ("‹rig› already has an item named ‹new› here (names that differ only in letter case
count as the same). Nothing was replaced."); `watched` and `layout` the refusal sentence;
`in_use`; and the shared `busy`, `unreachable`, `identity`, `host_bound` and `denied` wordings.
`not_found` reads "‹name› isn't in this folder any more. If the connection dropped, the rename
may already have gone through", followed by a note that the listing shows the folder as it
is. It re-lists both panes and leaves only Close, since a second submit could only answer `not_found`
again. The neutral wording covers a lost reply whose retry finds the source already
moved, without a check-before-retry branch.

#### Upload

Upload clicks a hidden single-file `<input type="file">` (no `multiple`, out of the tab order and
the accessibility tree) inside the click handler, since mobile browsers open a picker only from
a user gesture. Cancelling the picker does nothing, and the input is reset after each pick so
the same file can be picked again. Before any request the client checks the picked name with
the shared validator and upload's 224-byte cap (§6.1); a failure opens the dialog straight into
Plan failed, saying the file must be renamed on the computer, with Close only. The server
repeats every check.

The upload dialog is the copy dialog with a source parameter, not a twin component, preceded by
a staging step: **Sending → Checking → Plan (or Plan failed) → Progress → result or toast**, with
the copy dialog's focus, Esc and scrim rules per state. In Sending, the header Close and Esc do
exactly what Stop does, since nothing has reached the device; the scrim stays inert. Every close
returns focus to the pane's Upload button when it is enabled, else to the listing.

1. **Sending** (browser → Commander). "Sending ‹name› to Commander", bytes sent of total, the
   bar and Stop, driven by `XMLHttpRequest.upload.onprogress` at most once a second, with
   "Nothing is written to ‹rig› until you confirm the upload." Stop aborts the request and
   closes the dialog with a toast saying nothing was written; nothing is staged. A staging
   refusal (§6.2) opens Plan failed with its sentence. A network error offers Send again and
   Close. Navigation is guarded from here on.
2. **Checking.** "Checking ‹name› against ‹rig› › ‹share› › ‹dir›…", elapsed seconds, Cancel.
3. **Plan.** "Upload ‹name› (412 MB) to ‹rig›" and the destination in mono; the copy's watched
   warning (`copyWatchedLines`) only for a watched destination; one space warning, never a
   refusal, when the file exceeds the share's free space (`Statfs`); a conflict with the two
   size/time pairs and Overwrite / Skip defaulting to Skip, with the same inline confirmation
   and `dst_ledger` hints as copy; a type conflict ("a folder has that name") with nothing to
   choose. The footer's "Upload" ("Upload and overwrite" when Overwrite is chosen) is disabled
   while Skip is chosen or a folder holds the name, with a line saying why. There is no size
   limit and no FAT32 4 GiB warning: an oversized file, or a full device, fails as an `error`
   row with the device's message and a Retry; the plan's free-space warning is the only advance
   notice.
4. **Progress** (Commander → ASIAIR). Copy's progress state, worded for upload ("Writing ‹name›
   to ‹rig›"), with Stop and the waiting text.
5. **Result or toast.** Success closes with "Uploaded ‹name› (412 MB) to ‹rig›". Any other answer
   is a single result row worded from the reason table: Overwrite on `appeared` and `changed`
   (with the copy's confirmation rules); Retry on `unreachable`, `in_use`, `denied`, `error` and
   `stopped`; none on `type_conflict`; `mtime_not_preserved` as a note. Retry and Overwrite run
   the write again from the same staged file, without re-sending it. Closing the dialog, a
   success's close included, asks Commander to discard the staged file, best-effort (expiry
   covers a lost request). `source_gone` never occurs for an upload.

When a plan or run answers that the staged file is gone (expired, evicted, or lost to a
restart), the dialog, which still holds the `File`, returns to Sending, stages it again and
repeats the request. A Retry that finds the file already complete on the device (a run stopped
just after its commit, or whose commit reply was lost) answers the existing `appeared`, or
`changed` for a bound overwrite, whose confirmed Overwrite never replaces without the user's
say; the stopped wording warns of this in advance.

Interruptions split in two. A drop **while sending** touches nothing on the device: Commander
sees the body end short, deletes the partial staged file and drops its record. **Stop while
writing** is copy's Stop: the run is cancelled, its temp removed, and the result row reads
`stopped` ("‹rig› holds either the complete file or nothing under its name; Retry writes it
again from Commander…", adding "The old file may already have been removed." for an overwrite),
titled "Upload stopped". **A lost connection while writing** is copy's interrupted path, titled
"Upload interrupted — lost connection to Commander, or your sign-in ended". Either way the
staged file is kept for Retry, even when the server reported `copied` and the browser saw
`stopped`. The post-run re-list surfaces an expired session's `401` through the app's usual
sign-in redirect; a fresh sign-in cannot reach the old staged file (§6.2), so the user picks the
file again and the old one expires. **Refusals before any device write** (`busy`, a reserved
path, and the pre-write check's answers, §6.4, or a non-JSON status from a proxy) return the
dialog to the Plan with the reason as its notice; nothing was written and the staged file is
kept. A non-JSON status is worded as "Commander, or a proxy in front of it, refused the upload
(HTTP ‹status›)."

## 4. API

Routes are registered in `routes()` and listed in `registeredRoutes` (`internal/web/server.go`),
which a route-list test keeps in step. GETs use `guardRead`; POSTs use `guardWrite` (session,
Origin and CSRF, [architecture §8.3](2026-10-17-architecture.md)), including the read-only plan
routes. When `Deps.DeviceFiles` is nil, every route answers `503 files_unavailable`.

| Route | Purpose |
| --- | --- |
| `GET /api/rigs/{id}/files?share=&dir=` | List the shares (no `share`) or one folder. |
| `GET /api/rigs/{id}/files/download?share=&dir=&name=…[&check=1]` | One file, or a streamed zip; with `check`, its JSON preview. |
| `POST /api/rigs/{id}/files/mkdir` | Create one folder. |
| `POST /api/rigs/{id}/files/rename` | Rename one entry in place. |
| `POST /api/rigs/{id}/files/delete/plan` | Walk a selection; return the summary and item list. |
| `POST /api/rigs/{id}/files/delete` | Delete echoed items (NDJSON). |
| `POST /api/files/copy/plan` | Walk a selection and check the destination. Two rigs are named, so the route is not under `{id}`. |
| `POST /api/files/copy` | Copy echoed items (NDJSON). |
| `POST /api/files/upload/stage?name=&mtime_ms=` | Stage one file on Commander; the raw body is the file. |
| `POST /api/files/upload/plan` | Plan a staged file's write. |
| `POST /api/files/upload` | Write a staged file (NDJSON). |
| `POST /api/files/upload/discard` | Discard a staged file. |

**List** answers `{share, dir, watched, writable, entries:[{name, dir, size, mtime, watched,
rename_refused?}]}`. `entries` is never `null`. `mtime` is unix seconds as listed, in the device
clock with no offset correction, because it is display only. `rename_refused` is `{rule,
message}`, the message rendered by the server (§6.5).

**Copy plan** request `{from:{rig, share, dir, names[]}, to:{rig, share, dir}, mirror}`; with
`mirror`, `to.share` and `to.dir` equal `from`'s. The response:

```json
{"files":12,"bytes":201326592,
 "dirs":["M42","M42/flats"],
 "items":[{"rel":"ref.fit","size":16777216,"watched":false}],
 "conflicts":[{"rel":"ref.fit","kind":"file","src_size":16777216,"src_mtime":1760000000,
               "dst_size":16777216,"dst_mtime":1759990000,"dst_ledger":"archived"}],
 "to_dir_missing":0,
 "watched":{"frames":8,"previews":2,"keeps_originals":false}}
```

Each item's `watched` is `WatchedDir` of its destination folder on the destination rig; the
client uses it only to word a report row's Overwrite confirmation and leaves it out of the run.
`kind` is `file` (overwrite allowed) or `type` (skip only). `dst_ledger` is `""`, `unarchived`
or `archived`, set only for a destination the destination harvester treats as a frame. `dirs`
lists, relative to `to.dir` and parents first, the folders the run will create; `to_dir_missing`
counts the levels of `to.dir` itself that do not exist yet (a mirror), which the run creates
first. The plan writes nothing.

**Delete plan** request `{share, dir, names[]}`; response `{dirs, files, bytes, by_ext:[{ext,
files, bytes}], unarchived, unwatched_files, watched_roots[], items:[{rel, size, mtime}],
dirs_deepest_first:[rel…]}`.

**Upload.** The staging request carries the file as its raw body, with `Content-Length`
required (`411 length_required` without it), and the name and the browser's `lastModified` (as
`mtime_ms`) as query parameters, so every refusal is decided before the body is read. It answers
`{id, size}`. The plan request is `{id, to:{rig, share, dir}}` and answers the copy plan's exact
shape plus `free_bytes`. The run request is `{id, to, dst_size?, dst_mtime?}`, where the pair,
both or neither, binds an overwrite to the destination state the user saw. The discard request
names the id.

**Errors** use `writeErrorCode`:

| Code | Status | Meaning |
| --- | --- | --- |
| `unknown_rig` | 404 | The rig is not configured, or not managed. |
| `bad_path`, `too_many`, `into_itself`, `same_folder` | 400 | The request is invalid. |
| `type_conflict` | 409 | A file stands where a folder must go (a mirror's ancestor, an upload's folder). |
| `not_found` | 404 | The path or share is gone. |
| `exists` | 409 | The name is taken (mkdir, rename). |
| `no_share` | 409 | The other rig has no share by that name. |
| `layout` | 409 | The folder or name would hide the share's `ASIAIR/`-nested watched root (§7). |
| `watched` | 409 | A rename refusal (R1, R2), or a file at a reserved path (§6.3). |
| `busy` | 409 | Another operation holds the lock; the message names it, its rigs and its start time (RFC 3339 with offset, since the server's zone need not be the reader's). A holder already cancelled but waiting on a silent device reads "‹op› on ‹rig› is still stopping: waiting up to ‹SMBTimeout› for ‹rig› to respond." |
| `unarchived` | 409 | The unarchived count grew since the user confirmed. |
| `identity` | 409 | A different device answered at the rig's address. |
| `host_bound` | 409 | The rig has no guid yet, so writes are refused. |
| `denied` | 403 | The device refused (access denied; a sharing violation is `in_use`). |
| `in_use` | 409 | The device reports the file in use (`STATUS_SHARING_VIOLATION`); mostly a run item's reason (delete, the commit's rename, an overwrite's delete of the destination). A route whose single device call meets it, such as rename or new folder, answers this code directly. |
| `unreachable` | 503 | Retries were exhausted. |
| `length_required` | 411 | A staging request without `Content-Length`. |
| `staging_gone` | 410 | No staged file by that id for this session. |
| `staging_full` | 507 | The staging cap or Commander's disk reserve refuses the file. |
| `files_unavailable` | 503 | No service is wired in, or it is shutting down. |

`TestFilesSentinelCodes` and `TestFilesEverySentinelAnswers` (`internal/web`) map every sentinel
to its code.

### 4.1 Paths and the size cap

`share` is a share name. `dir` and every item path are share-relative, `/`-separated, with no
leading slash; `""` is the share root. **Run items are relative to the selection's folder**: a
copy item `rel` reads from `from.dir/rel` and writes to `to.dir/rel`, and a delete item is
`dir/rel`. A nested file from a copied folder therefore retries to the right place
(`TestCopyRetryNestedItem`), and run bodies stay small. Every request, echoed runs included, is
refused with 400 for a `..` or empty component, a backslash, a NUL, a leading `/`, a `name`
containing `/`, or a marker name (`checkShare`, `checkDir`, `checkRel`, `checkName`). Rename and
upload check more strictly (§6.1).

A plan is refused with `400 too_many` when it exceeds 10,000 files or when its run body would
exceed 3 MiB, so the run request stays well under the web layer's 4 MiB JSON cap. The message
states the limit and says to select fewer files or a subfolder.

### 4.2 Download

One selected file is sent as `application/octet-stream` with `Content-Length`,
`X-Content-Type-Options: nosniff` and a `Content-Disposition` built with
`mime.FormatMediaType("attachment", …)`, which encodes non-ASCII names per RFC 2231. Anything
else is sent as `application/zip`, named `‹rig›-‹folder›.zip`. `check=1` answers `{filename,
files, bytes, zipped}`.

### 4.3 Run lines

The copy run request is `{from:{rig, share, dir}, to:{rig, share, dir}, dirs[], items:[{rel,
size}], overwrite:[{rel, dst_size, dst_mtime}]}`. Each overwrite echoes the destination's size
and mtime as the user saw them (the plan's conflict row, or a report row's `item` line), and
binds the overwrite to that state. The client leaves out skipped items and type conflicts with
their subtrees. The delete run request is `{share, dir, items[], dirs[],
acknowledged_unarchived}`; the acknowledgement is a count, not a boolean, so a confirmation of 3
cannot cover 40 (§5.8).

Copy, upload and delete runs answer lazily-begun NDJSON; a 4xx found before the first line is an
ordinary JSON error.

```json
{"type":"progress","done":3,"total":8,"bytes_done":412000000,"bytes_total":1180000000,"current":"M42/a.fit"}
{"type":"item","rel":"M42/b.fit","outcome":"skipped","code":"appeared","dst_size":16777216,"dst_mtime":1760000400,"message":"…"}
{"type":"summary","done":6,"skipped":1,"failed":1,"aborted":""}
```

Only skips, failures and copied files with a note (`outcome:"copied"`,
`code:"mtime_not_preserved"`) get an `item` line; successes are counted. An `appeared` or
`changed` item carries the destination's size and mtime as the run found them, so its Overwrite
can echo them; it carries no ledger value. Retries are not on the wire. `progress` is emitted
when each item starts (`current` names it) and at most once a second within a file. `aborted` is
`""`, `unreachable` or `identity` (§5.4). After a cancel the server writes nothing more, so a run
that ends normally ends with the summary, and a stream without one is a stopped or interrupted
run that the client reports itself (§3.5).

## 5. Backend

**The device seam.** `device.Share` carries the tool's write primitives, each bounded by the rig's
idle deadline: `CreateNew` (exclusive create, `O_CREATE|O_EXCL` → SMB `FILE_CREATE`; an existing
path reports `IsExist`), `Mkdir` (one level; an existing name reports `IsExist`), `Rename` (same
share, never replaces an existing target: a collision reports `IsExist`), and `Chtimes`; `Delete`
removes a file or an empty directory. A directory delete goes through `SMB_COM_DELETE_DIRECTORY`,
which the library retries for about 6.5 s on a sharing violation; a file delete is not retried, so a
file in use comes straight back as `in_use` after one device call. **There is no truncating
create**, so the tool's only write primitive cannot destroy an existing file. The predicates
`IsExist`, `IsInUse` (`STATUS_SHARING_VIOLATION`; it also satisfies `os.ErrPermission`, so callers
test it before `IsPermission`), `IsDirNotEmpty` (`STATUS_DIRECTORY_NOT_EMPTY` inside the path error)
and `IsPermission` classify the answers (`TestIsInUse`, `TestIsDirNotEmpty`). The writer that
`CreateNew` returns re-arms the idle deadline on each `Write` and bounds the SMB CLOSE, which
commits the file, by the same deadline before clearing it, so a silent device cannot hold a copy and
the operation lock indefinitely; session teardown bounds each umount and the logoff likewise. The
behaviours these rest on, never-replacing rename and its collision status, a non-empty folder
refusing deletion, the sharing-violation status, and the library's directory delete retrying a
sharing violation for about 6.5 s, are device and library facts ([asiair-hardware-notes
§9](2026-10-17-asiair-hardware-notes.md)); smb1client must remove a directory with
`SMB_COM_DELETE_DIRECTORY` ([asiair-hardware-notes §9.5](2026-10-17-asiair-hardware-notes.md)): a
client using delete-on-close reports success for a non-empty folder it leaves in place, which would
make a delete run's `not_empty` read as deleted. `TestDevfilesDeleteNonEmptyRefused` and
`TestDevfilesServiceDeleteNonEmptyRefused` (`test/integration`) fail on such a client.

`device.Fake` implements the same semantics in memory (exclusive create, no-replace rename, a
`Delete` that refuses a non-empty directory with the device's error shape (a stored marker
counting as content, so `kept_marker` is testable), `Chtimes` that sets
the mtime `Stat` and `ListDir` report), plus count-limited fault injection (`FailNext`) that
builds errors in the library's shape, and opt-in marker listing (`ListMarkers`), opt-in because
the fake derives implicit folders from stored paths and a listed `ASIAIR/` marker would change
existing worker fixtures (`TestFakeCreateNewExclusive`).

**The harvest view** (`internal/rig/harvestview.go`, pure, no worker state) exports what the tool
needs to restate the harvester: `HarvestCandidate` (the pull listing would treat the path as a
frame: an enabled source on the share, under a root in either layout, no junk component, not a
junk file, extension not excluded), `HarvestPreview` (the same for an excluded-extension file),
`WatchedDir` (a root or inside one; the badge), `WatchedRoot` (exactly a root),
`HoldsWatchedRoot` and the rename rule (§6.5), `ReservedPath` (§6.3), `HarvestStoreKey` (§5.2),
and `ArchivedCopy` (§5.1); the service also uses the rig package's `SMBTimeout` (`worker.go`) and
`IdentityMatches` (`pull.go`). `TestHarvestViewTable`
and `TestHarvestViewAgreesWithWalk` hold them to the pull walk for both layouts, mixed case, junk
folders, excluded extensions and disabled sources.

**The service** (`internal/devfiles`) depends on the live config, a read-only store surface
(`FrameStore`: `GetFrame`, `GetMeta`, `ResidentLeads`), a local-filesystem read seam (`LocalFS`:
`Stat`, `FreeSpace`), the status hub's snapshot, the `scan_air` prober, the site registry, the
same device factory the manager gets, and a logger. In the `-tags demo` build those are the demo
factory and a resolver that echoes the guid, so the demo needs no extra wiring. Its methods map
onto the routes, plus `Close` at shutdown. Errors are sentinels that the handler maps to the
codes of §4.

### 5.1 NAS presence

`ArchivedCopy(row, size, mtime)` is true only when the frame row is proven archived (verified or
unclassified), not culled on the NAS (`missing_seen_at` = 0), and its size and mtime match the
device file as `decide` compares them. That proves only what the store last observed: the
library scan sets the missing mark, so a frame culled since the last scan still reads archived.
The delete plan, the delete run's recount and the copy and upload plans' `dst_ledger` therefore
also `Stat` each hit's `dest_path` through the same local-filesystem seam the delete pass uses,
and count the frame as **not archived** when the file is missing, its size differs from
`archive_size`, or the stat fails for any reason (the conservative direction for a warning). It
is one local stat per hit, bounded by the 10,000-file cap, with no hash, and a read of the NAS,
not a write (`TestDeleteUnarchivedNASMissing`).

`ArchivedCopy` is deliberately **stricter than the pull tally**. The tally stops counting a frame
as pending once its missing mark is set, and the safely-on-NAS test includes `dismissed`, both so
that a rig does not freeze at Pulling; neither proves a NAS copy exists. A culled or dismissed
frame's device file is the only copy left, so deleting it through the tool must count it, and
deleting one also raises the doubly-vanished warning
([frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md)). `held` is excluded too, because its NAS
copy is suspect. `TestArchivedCopyImpliesNoPull` asserts that `ArchivedCopy` implies `decide`
would not pull the file, so the two can disagree only in the conservative direction;
`TestArchivedCopyTable` covers each state.

### 5.2 Path case

The predicates match the `ASIAIR` prefix, the root segments and the share name case-insensitively
(`strings.EqualFold`), because a FAT device may list `AUTORUN` for a configured `Autorun`, and a
case-sensitive match would read real frames as non-candidates (no unarchived count and no
confirmation checkbox in the delete dialog, no badge). Store lookups are exact-key, and the
harvester keys its rows by the literal `ASIAIR` prefix and the configured root's case, followed by
the listed names, so the tool rewrites a device path's leading segments to that form
(`HarvestStoreKey`) before every lookup (`TestHarvestStoreKey`,
`TestHarvestStoreKeyFindsHarvesterRow`). Destination folders in a plan match names with `EqualFold`
too, since ASIAIR media are FAT. The plan's `EqualFold` matching is what decides conflicts, so
mixed-case names on the case-preserving fake test it; a case variant that appears on the device at
run time is caught by the device's case-insensitive rename collision (`IsExist`), so the fake needs
no case folding.

### 5.3 Sessions

**Checkout** (`acquire`):

- Read the profile; a rig that is not managed is `unknown_rig`. Take the endpoint from the rig's
  entry in the hub snapshot (an atomic load): the site and address its worker published from
  its own resolution ([device-identity-and-media §3](2026-10-17-device-identity-and-media.md)).
  The tool resolves nothing itself, so it dials exactly where the worker last found the device;
  an address gone stale since that cycle is caught by the identity probe. With no published
  address it fails `unreachable` ("Commander doesn't know where ‹rig› is yet"), without
  retrying. A remote whose tunnel is down needs no special case: its dial fails fast with
  `netsite.UnavailableError`, which `withRetry` retries like any network error.
- Take one of the rig's two slots, a semaphore that waits on the request context. A request that
  needs two rigs (a copy plan or run) takes the operation lock first and only then its rigs'
  slots, in any order: every two-slot request holds the global lock, so two of them never wait
  on each other, and a listing or mkdir takes a single slot and waits on nothing else.
- Reuse the rig's parked session only if it was opened under the same endpoint and the same
  config pointer as the current ones; otherwise close it. Every config save, including a
  credential edit and the worker's guid/cpuId bind, replaces the pointer, so a rebind or a
  credential change never reuses a session proved against the old identity, at the cost of a
  redial after an unrelated save.
- Otherwise open one with the profile's credentials and `rig.SMBTimeout(site)`, bounding the
  open by a short budget (3 s plus three round trips of the site), so a dead local address fails
  in seconds rather than after the worker's 30 s operation budget. Then probe the guid at the
  endpoint while the session is open:
  - a foreign answer closes the session and returns `identity`, never retried;
  - no reply closes it and returns a retryable failure;
  - a reply for a rig with no guid yet is accepted, and the session is read-only.

  The tool never records a guid bind; binding stays the worker's job. `writable` is the probe's
  verdict together with a non-empty profile guid.
- The checked-out session caches one mount per share, because the device layer adds a tree
  connect on every `Mount`, so a parked session must not remount per request.
- No cancel hook is armed on the session: an SMB session does not observe a context after the
  dial, so an in-flight call blocked on a silent device returns only when its idle deadline
  expires (§5.10).

**Checkin** (`release`) closes the session on a network error, a cancelled request or a closed
service; otherwise it parks it as the rig's idle session (closing the one it replaces) for 2
minutes. The slot is freed last, after any close. Every stream is closed before checkin, which
clears its deadline, so a parked session never carries an armed deadline into the client's
background receive loop.

**One checkout per rig per request.** A same-rig copy reads and writes through one session on
one goroutine, alternating `Read` and `Write`, each re-arming the deadline, so the deadline is
never contended. A cross-rig copy holds one session on each rig.

**Bounds.** The operation lock covers every run and every walk (copy and delete plans, the
download `check`, the upload plan, rename), so between them they hold at most one session per
rig. The lock records the holder's operation, rigs, start time and request context, so a refusal
can say when the holder has been cancelled and is still waiting on a device. With the slot cap
of 2 a listing or mkdir can always run beside an operation; a second simultaneous listing on the
same rig waits for the first, which takes milliseconds. The worst case per device is the worker,
one operation and one listing: three sessions. Three concurrent sessions are supported by the
device ([asiair-hardware-notes §9](2026-10-17-asiair-hardware-notes.md)). `TestSessionLifecycle`,
`TestOperationLockAndCap` and `TestBusyWhileStopping` cover reuse, expiry, the config-pointer
rule, the third checkout waiting, lock order and the busy wording.

### 5.4 Failures and retries

`withRetry` is the only retry loop. It runs a function on freshly checked-out sessions of one or
two rigs and retries exactly two kinds of failure, a network error (tunnel errors and
`netsite.UnavailableError` included) and an unanswered identity probe, up to three more times
after 0.5 s, 1 s and 2 s, every attempt on fresh sessions (the failed ones are closed). It checks
the request context before classifying an error and waits out each backoff on it, so a cancel
returns `cancelled`, never a retry. Any other error returns at once. Exhaustion returns
`unreachable` naming the rig whose side failed last. A refused logon is an SMB status, not a
network error, so it returns at once with the server's message; such a rig's worker cannot log
in either, so it would not show online. Each retry is logged at Warn.

- **Unit of retry**: one listing, one mkdir, one rename, one file deleted, one file written (from
  opening the source to the commit; the `Chtimes` after it is outside the unit and never
  retried), and one zip entry opened.
- **Every write retry starts over** from byte 0 on a fresh temp name, including after a network
  failure at the commit step. The item remembers every temp it created; each retry first deletes
  earlier attempts' temps on its fresh session, and the item's final exit deletes any that
  remain. If a commit rename reached the device but its reply was lost, the retry's commit finds
  the destination present: a plain write reports `appeared`, an overwrite `changed`. The device
  then holds the user's complete copy and the row's confirmed Overwrite finishes it; the outcome
  can be a spurious skip, never an overwrite. A New folder whose reply was lost retries into
  `exists`, and the re-listed pane shows the folder.
- On a cross-rig copy, reader and writer errors are tagged with their rig, so the message names
  the right device; both sessions are replaced on retry.
- **The first `unreachable` in a multi-item run ends the run.** Every remaining item is emitted
  as `not_attempted` with no I/O, and the summary says `aborted: "unreachable"`, so the run does
  not spend four attempts per file against a dead rig. An item that fails `identity` or
  `host_bound` ends the run the same way with `aborted: "identity"`, since every remaining item
  would re-dial the same wrong or unidentified device. In a delete, this changes only when the
loop stops, never what it deletes (`TestRetryPolicy`,
  `TestUnreachableStopsRunCopy`, `TestUnreachableStopsRunDelete`).
- **Worst case for one interactive call.** Against a dead local rig, four opens of about 3 s plus
  3.5 s of backoff come to about 16 s. When a parked session died silently, the first attempt can
  wait out one operation timeout (30 s locally) before the fast reopens. That is accepted for a
  tool.

### 5.5 Walk

`walk(share, dir, names)` lists the selection recursively with `ListDir`, in pre-order, with
sizes and mtimes. It drops marker files (by name, case-insensitively) and applies no other
filter. It ends at 10,000 files with `too_many`. **Any listing failure, after its retries, fails
the whole walk**: a plan, summary or zip is never built from a partial listing
(`TestWalkFailsWhole`). The listing-completeness rule behind this is
[delete-safety §5](2026-10-17-delete-safety.md).

### 5.6 The copy plan

The plan runs under the operation lock: `checkCopyPlaces` → `lockOp` → the destination's
refusals (`destCheck`: host-bound, no such share) → `walk` → the sink's plan (`planDest`, §6.3)
→ the run-body bound.

1. Walk the source.
2. List each destination folder the plan writes into **once**, so the cost is per folder, not per
   file, over a tunnel. A folder not found is created by the run. For a mirror, `to.dir`'s own
   ancestors are checked the same way from the share root: missing levels count in
   `to_dir_missing`, and a file standing where one would go refuses the plan with
   `409 type_conflict` (`TestMirrorCreatesAncestors`).
3. A source folder meeting a destination folder merges into it. A file meeting a file is a `file`
   conflict; a file meeting a folder, or the reverse, is a `type` conflict whose subtree is
   excluded.
4. For each `file` conflict whose destination is a `HarvestCandidate` on the destination rig, read
   the row and set `dst_ledger` with the NAS check of §5.1 (`TestCopyPlanDstLedger`).
5. The `watched` counts are the planned files that are `HarvestCandidate` (frames) and
   `HarvestPreview` (previews) at their destination on the destination rig. Every such preview
   leaves the device unarchived, deleted with its frame or swept as an orphan
   ([frame-lifecycle §3.3](2026-10-17-frame-lifecycle.md)), so the count needs no `_thn` pairing
   (`TestCopyPlanPreviewCount`). Both predicates are false outside a watched root, so a copy into
   an unwatched folder has zero counts. `keeps_originals` is the destination rig's setting.
6. Refuse `into_itself` (same rig and share, destination inside a selected folder),
   `same_folder` and `no_share`; refuse a file at a reserved path (§6.3) and a folder creation
   the layout guard forbids (§7).

### 5.7 The copy run

Validate, take the operation lock, then ensure `to.dir` from the share root and create `dirs`
parents-first, through a per-run cached `ensureDir`. `ensureDir` `Stat`s each level and runs the
layout guard and `Mkdir` only when the level is missing, so an existing folder never meets the
guard; a file in the way fails that subtree as `type_conflict` (a library `MkdirAll` would
swallow that collision). A planned folder that cannot be made gets its own `item` line, counted in
the summary as failed (skipped for `type_conflict`), and fails the items under it.

Each file is then written through the sink's per-item write (§6.3), inside `withRetry` over the
source and destination rigs:

1. Open the source reader; not-found is `source_gone`.
2. `CreateNew` the temp `dst.<16 random hex>.commander.tmp`. A collision with a 64-bit random
   suffix does not happen in practice; if it did, `IsExist` is an ordinary error and the item
   fails as `error`, and the exclusive create means nothing is destroyed.
3. Stream in 512 KiB chunks through a reader that checks the context per chunk and counts bytes
   for `progress`; close the writer.
4. **Commit.**
   - If the item is in `overwrite`: `Stat(dst)`. A file whose size equals the echoed `dst_size`
     and whose mtime is within 2 s of `dst_mtime` (FAT's resolution) is deleted, then
     `Rename(tmp, dst)`. A file that has moved is skipped as `changed`, and a folder as
     `type_conflict` (a library `Remove` would otherwise remove an empty folder at that name);
     either way the temp is deleted. Not-found is a plain `Rename(tmp, dst)`.
   - Otherwise `Rename(tmp, dst)`.
   - On `IsExist`, something appeared or came back after the check: delete the temp and
     `Stat(dst)`. A file there skips the item with `appeared`, carrying the file's size and mtime;
     a folder there skips it with `type_conflict`. The run reads the store nowhere. **The
     device's never-replacing rename enforces the no-accidental-overwrite rule; there is no
     window between a check and a write.**
   - On `IsInUse` from the overwrite's `Delete(dst)` or from the rename, delete the temp and fail
     the item as `in_use`, not retried. A refused `Delete(dst)` leaves the destination untouched.
     If the overwrite's delete succeeded and the rename then fails, the destination holds
     neither file: the user had chosen to replace it, the source is intact, and the row says the
     old file was removed and a retry finishes it.
5. **Preserve the mtime, best-effort.** After a successful commit, `Chtimes(dst, srcMtime,
   srcMtime)` once with the source mtime from the walk, so no device-clock read is needed. A
   failure keeps the file, which counts as copied with an `item` line `mtime_not_preserved` and a
   Warn; the file keeps the copy time. `Chtimes` is never retried: the file is committed,
   retrying the unit would recopy it, and the property is cosmetic. A network-class `Chtimes`
   error still marks the session broken, so the next item opens a fresh one. A cancel between
   the commit and `Chtimes` skips it; the file counts as copied with the note. Folder mtimes the
   copy touches take the device's clock, which may be far off
   ([asiair-hardware-notes §7](2026-10-17-asiair-hardware-notes.md)); they are only displayed.
6. On a final failure or a cancel, delete the item's remaining temps best-effort on a fresh
   session within a 10 s budget, after the failed session is checked in (closed) so that its slot
   is free; the request's own context may be the thing that was cancelled. A temp left behind by
   a dead link is junk to the harvester, visible in the pane and deletable there; the cleanup
   failure is logged at Warn and the report does not name it.

There is no read-back verification: exclusive create plus a clean close and a successful rename
is the check (§10). `TestCopySkipsAppearedDestination`, `TestCopyCommitCollisionSkips`,
`TestOverwriteSkipsChangedDestination`, `TestCopyRetryLeavesNoTemp`, `TestCopyPreservesMtime`,
`TestCopyChtimesFailureKeepsFile` and `TestInUseReportedCopy` cover these steps, with
`TestDevfilesRenameNeverReplaces` and `TestDevfilesCreateNewExclusive` against real Samba.

### 5.8 Delete plan and run

**Plan.** Walk the selection under the operation lock, then compute:

- `dirs`, `files` and `bytes`;
- `by_ext`, keyed by the lower-cased extension, `""` shown as "(no extension)";
- `unarchived`: the files where `HarvestCandidate` is true and the row (looked up by store key)
  is not an archived copy under §5.1. Point lookups on the unique index, so 10,000 take well
  under a second. Previews and junk are not counted: a preview is never a frame row and goes with
  its frame (`TestDeleteUnarchivedCount`);
- `unwatched_files`: selected files that are neither a `HarvestCandidate` nor a
  `HarvestPreview`;
- `watched_roots`: every walked folder, the selected ones included, for which `WatchedRoot` is
  true in either layout, so selecting `ASIAIR/` or any ancestor of a root warns too. A root
  missing in both layouts fails the share's whole listing, not just that folder, so every watched
  folder on the share stops being archived and Safe to power off is withheld until it exists
  again ([frame-lifecycle §14](2026-10-17-frame-lifecycle.md)) (`TestDeletePlanWatchedRoots`);
- `items` and `dirs_deepest_first`.

**Run**, under the operation lock:

1. Recount `unarchived` over the echoed items (the ledger may have moved since the plan, for
   example a pull that failed). If the fresh count exceeds `acknowledged_unarchived`, answer
   `409 unarchived` with the fresh count and delete nothing (`TestDeleteRunUnarchivedRecount`).
2. For each echoed file, `Stat` it. Not found counts as deleted (`already_gone`; the delete pass
   may have got there first). A size that differs, or an mtime more than 2 s off, is skipped as
   `changed`. Otherwise `Delete`; `IsInUse` fails the file as `in_use`, not retried
   (`TestInUseReportedDelete`).
3. For each echoed folder, deepest first: `Stat` it and skip it as `changed` unless it is a
   folder (a library `Remove` would delete a file that has taken the folder's name), then
   `Delete`. The device refuses a non-empty folder, and `IsDirNotEmpty` classifies the refusal,
   so **a refused folder delete is the `not_empty` signal**: a file that arrived after the plan
   is caught by the device itself, with no listing-to-delete window. Only after such a refusal
   does the run list the folder, markers included, and only to word the row: markers alone make
   it `kept_marker`, anything else `not_empty`. The listing is needed for that one distinction,
   a folder holding only Commander's markers, which the device's status cannot express; it
   guards nothing, so if it fails the row reads `not_empty`. A folder holding a marker is never deleted either way, because the walk
   never echoes the marker and the device refuses the folder (`TestDeleteRunDirTypeCheck`,
   `TestDeleteRunNotEmptyFromDeviceError`).
4. No file outside the echoed list is touched (`TestDeleteRunOnlyEchoedItems`).
5. There is no content-hash check, because this is a delete the user confirmed, and the store is
   not written. Where the deleted file was an archived frame, the harvester's delete pass later
   resolves the row through its vanished-source branch
   ([frame-lifecycle §9.4](2026-10-17-frame-lifecycle.md)); the three-way check of the
   harvester's own deletes is untouched ([delete-safety §3.1](2026-10-17-delete-safety.md)).

### 5.9 Download

1. Take the operation lock, or answer `409 busy`.
2. Walk the whole selection, with retries, before sending any header; a listing failure is an
   ordinary JSON error.
3. A single file is opened with retry, then streamed in 512 KiB reads.
4. A zip uses `zip.Store`, because FITS and JPEG gain nothing from Deflate and Commander may run
   on a weak NAS CPU. Entry names are relative to `dir`, `Modified` is the device mtime, empty
   folders become directory entries, markers are left out, Zip64 records are written as needed,
   and each entry's source is opened with retry **before** its header is written.
5. **A failure after the first byte is not retried**, because a sent stream cannot be rewound.
   The handler logs it and panics with `http.ErrAbortHandler`, which makes `net/http` reset the
   connection rather than end the chunked body cleanly, so the browser marks the download failed.
   A zip has no `Content-Length` to betray a truncation, so this is the only honest ending. The
   `check=1` request already does the walk, so only mid-stream failures reach this path.

`TestDownloadZip`, `TestDownloadAbortAfterFirstByte` and `TestDownloadCheck` cover these.

### 5.10 Cancellation

The request context flows into every engine. Between items each engine checks it, the write
path's chunk reader checks it before each chunk, and the walk checks it between listings. A read
or write already blocked on a silent device returns when its idle deadline (`SMBTimeout`: about
30 s locally, longer for a remote site) expires; there is no abort from another goroutine. The
engine then deletes its temps, closes the session (cancelled, so never parked), logs the cancel
at Info, and releases the operation lock. A write to a disconnected client ends the request, so a
browser that vanishes cancels the operation like Stop. It writes no further stream lines: the client has
already aborted the fetch and builds the report itself. Until the lock is released, another
operation gets `409 busy` with the "still stopping" wording. Cancel only ever stops work, so a
slow stop cannot write or delete anything; the browser shows the Stop report at once
(`TestCancelStopsCopy`, `TestBusyWhileStopping`).

### 5.11 Logging

`slog` through the injected logger, prefix `devfiles:`. Every line carries `rig`, `share` and
`op`. Device paths are logged; credentials and file contents never are.

| Level | Events |
| --- | --- |
| Info | session checked out (site, ip, reused=false) and close (reason: idle, error, cancelled, shutdown, stale, replaced); identity confirmed, or not yet bound (read-only); operation start (counts, bytes, source, destination, overwrites) and end (done, skipped, failed, aborted, duration), or a cancel (items done, the item in flight, how long the stop waited); **every file written, overwritten or deleted, and every folder created**, the audit trail of the write path; each `appeared` skip; a delete refused by the unarchived recount; a copy into watched folders (counts) |
| Warn | each retry (attempt, backoff, error); identity foreign or unconfirmed; a write refused on a host-bound rig; a delete, commit rename or overwrite delete refused as `in_use`; temp cleanup failed; a copied file's mtime not preserved; a download aborted after its first byte; a busy refusal (noting a holder still stopping) |
| Error | retries exhausted; an unexpected error mapped to 500 |
| Debug | each listing (entry count); each plan (counts, conflicts); a parked session reused; a frame counted unarchived because its NAS copy is unreadable or its length differs |

## 6. Rename and upload

Rename moves one entry in place. Upload puts one file from the browser into a pane's folder,
reusing the copy flow: plan, conflict choice, progress, result and Retry. Both obey every rule of
the copy and delete paths; this section states what they add.

### 6.1 Names

New folder, rename and upload share one validator, `validateName(name, kind)` in Go and in util,
`kind` choosing only the wording ("Invalid folder name", "Invalid file name", "Invalid name"). A
name must not be empty, `.` or `..`; contains none of `/ \ : * ? " < > |` and no Unicode Cc or
Cf character; has no leading space and no trailing space or dot (FAT); is at most 255 bytes; and
is not a marker name (`TestValidateName`).

Upload caps a name at **224 bytes**: 255 less the 31 bytes of the temp suffix
`.<16 hex>.commander.tmp`, so every upload's temp name fits the device's limit and the sink keeps
writing temp names verbatim. A longer name is refused by the staging request before any byte is
written. Copy has no such cap; a copied name long enough to overflow its temp fails with the
device's error.

**Strict segments.** Rename and upload check request paths more strictly than the listing does
(`checkStrictSegments`, `TestCheckStrictSegments`): a `dir` or `name` segment containing any of
the validator's forbidden characters, or ending in a dot or space, is `bad_path`. FAT stores none
of these and the page only sends listed names, but `:` is SMB stream syntax, SMB1's rename treats
its source as a pattern, and the Linux vfat and exFAT drivers strip trailing dots on lookup, so a
crafted request such as `Autorun.` could reach a watched path that the predicates compare as
different text. The check refuses these spellings before any device call, so what the device's
Samba would do with them is never reached. A rename's source must also appear under exactly that
name in a listing of its folder, which rules out 8.3 aliases and case variants for the item
renamed (`TestRenameNotListedExactly`). A `dir` written as an 8.3 alias stays a residual (§9).

### 6.2 Staging

An HTTP request body cannot be read twice, so upload first stages the file on Commander's disk.
The staged file is then a source that can be reopened, like a copy's device file, and upload
inherits copy's retries, progress and Stop unchanged. Phase 1 (browser → Commander) takes no
operation lock and touches no ASIAIR; phase 2 (Commander → ASIAIR) is a one-file run through the
sink.

- **Where.** `<data-dir>/upload-staging/`, mode 0700, each file mode 0600 and named by its
  staging id alone, never by the uploaded name. The id is 128 random bits in hex. The records
  (owner, name, size, mtime, state, last use) live in memory; nothing indexes the directory on
  disk.
- **Receiving.** Before reading the body, the staging request checks the name rules, the cap and
  Commander's free space: it is refused unless the data directory's volume keeps at least 1 GiB
  free after this file **and** the rest of every staged file still being received. The check
  subtracts from the volume's free space the declared length of every record still receiving,
  read under the records' lock, and the incoming record joins them under the same lock before its
  first byte is read. Bytes a receiving file has already written are counted twice (in the lower
  free space and in its declared length), which errs only towards refusing, so parallel uploads
  together never fill the disk that holds Commander's config, store and logs. The refusal is
  `staging_full` with its own sentence. A record becomes usable only when the body ended at
  exactly `Content-Length` and the file closed cleanly; until then its id is not answered. A body
  read error, a short body or a write error removes the partial file and its record.
- **After a run.** No run deletes the staged file, whatever its outcome: a run that reports
  `copied` may be one the browser saw as stopped or interrupted, and its Retry must still write
  from Commander. The file is removed only on discard (the dialog's close, success included), on
  expiry, by eviction at the cap, or at startup.
- **Expiry.** A staged file is removed 1 hour after its last use (the end of its receive, plan or
  run, or a Retry). A file being received, planned or run never expires. The sweep runs at each
  staging request and every 5 minutes.
- **Startup.** The staging directory is emptied and recreated before the web server listens; the
  records start empty, so no file outlives its record.
- **Cap.** At most 4 staged files at once, tool-wide, counting files still being received. An
  upload arriving at the cap evicts the least recently used staged file that is not being received,
  planned or run, and logs it; with all four in use it is refused as `staging_full`. An evicted
  file's dialog re-stages from the `File` it holds.
- **Session scoping.** Each record carries an owner key: the SHA-256 of the session's XSRF
  claim, which is minted at login, kept across idle-window refreshes and different for every
  login ([architecture §8.1](2026-10-17-architecture.md)). A plan, run or discard is answered
  only for the session that staged the id; any other session gets `staging_gone`, exactly as for
  an unknown id, so an id reveals nothing. Logging out rotates the session secret, which ends
  every session; their staged files are then unreachable and expire. The owner key is never
  logged.
- **Never logged or served.** Staged content is never logged and no route reads it back: the
  download route serves device paths only, and the staging directory is outside the SPA's file
  system (`TestFilesNoRouteServesStagedBytes`).

The staging handler requires `Content-Length` (`411` without it; the browser's `send(file)`
always sets one) and **drains the body before answering a refusal**, bounded by
`Content-Length`: on HTTP/1.1 Go otherwise closes the connection after a short discard, and the
browser reports a network error instead of the refusal. Refusals made by `guardWrite` before the
handler runs are not drained, so an unauthenticated body is never read; their `401` surfaces
through the app's redirect on the next request. A body that breaks off mid-read panics with
`http.ErrAbortHandler` after the partial file is removed, rather than writing an answer nobody
reads. A reverse proxy in front of Commander can refuse or spool a large body before Commander
sees it: nginx's default `client_max_body_size` is 1 MB, and request buffering defeats the
upload's progress. `DOCKER_README.md` notes the body-size and buffering settings uploads need.

`TestStageShortBody`, `TestStageBodyReadError`, `TestStageCancel` and the `TestStaging*` tests
(complete, kept by runs, discard, expiry, startup emptying, cap, reserve, name cap, no content or
owner key in logs) cover the lifecycle; `TestFilesUploadStageRequiresLength`,
`TestFilesUploadStageDrainsBeforeRefusal`, `TestFilesUploadStageAbortsBrokenBody`,
`TestFilesUploadClientAbortLeavesNothingStaged` and `TestFilesUploadSessionsIsolated` cover the
handler.

### 6.3 The sink: one write path for copy and upload

The sink (`internal/devfiles/sink.go`) is the destination half of every device file write: the
plan's check of the destination, and the run's folders, temps and commit. It knows nothing of
where the bytes come from; a caller names its operation and destination and hands over each
item's bytes through a small source description. Copy and upload call the same plan function and
the same per-item write function, so **every device file write by copy and upload passes through
one write function and one commit**, and a reviewer can check that mechanically.

**Plan side.** `destCheck` runs the destination's refusals (host-bound, no such share) and
`planDest` checks the entries against the destination: lazy per-folder listing with `EqualFold`
matching, file and type conflicts with subtree exclusion, `dst_ledger` through the NAS-checked
archive test (§5.1), the watched counts and `keeps_originals`, and the layout guard for folders
the run would create. Copy's plan is §5.6. Upload's plan resolves the staging id, checks the
place, takes the operation lock as "Upload check", runs `destCheck`, builds one file entry from
the staged record (name, size, mtime), and calls `planDest` without mirroring, plus the share's
free space for the plan's space warning. A missing folder is `not_found`; an upload creates no
folder. `TestSinkPlanUploadMatchesCopy` feeds the same destination and entries through both
callers and gets equal plans.

**Reserved paths.** A file whose destination path is, or holds, a reserved path at any depth
(`Autorun` as a file at an eMMC root, a file at `ASIAIR/Autorun` or at `Foo/Bar` for a root
`Foo/Bar`, `ASIAIR` as a file at a removable share's root) refuses the **whole plan**, in copy and
upload alike, with `watched`. Such a file makes the harvester's layout probe or root walk fail
the share's listing and blocks the firmware's own mkdir (§7). The refusal lists every offending
path (for a copy, about ten and then "and N more"), each with its operation's fix: rename the
file on the computer for an upload, leave it out of the selection or rename it on the source rig
for a copy. The reserved set (`rig.ReservedPath`) is the destination rig's configured roots on
that share, enabled and disabled, in either layout, compared ignoring case, plus `ASIAIR` at a
removable share's root, with **no store read**: a file can stand at a removed root's path only
once that folder is gone, and with it the rows the store-kept roots of §6.5 exist for. The write
function re-checks the rule; there it is reachable only after a config change between plan and
run, and it fails that item as `watched` with the same sentence and no action, while the run goes
on. An upload's run checks it before taking the lock (`TestPlanReservedRootName`).

**Run side.** The per-item write (`writeItem`) runs within `withRetry` over the source's rigs (the
destination last): it drops earlier attempts' temps, opens the source, creates a fresh temp,
streams in 512 KiB chunks checking the context per chunk, re-checks the context, commits
(overwrite binding, the same-as-seen check, never-replacing rename) and sets the mtime
best-effort; then removes any temps left, on a fresh session. The source description supplies
the rigs, an `open` called once per attempt that returns a reader at byte 0 and the mtime to set
(or a skip such as copy's `source_gone`), a read-failure mapping, the progress hook, extra log
fields, and one declared property, `zeroMtimeIsNone`, which only the staged source sets. The run
counters and the `progress`/`item`/`summary` lines stay with the caller, so an upload emits
copy's line types. The sink carries the operation's words, so upload's log and audit lines say
`op=Upload` and its messages never read as a copy's.

**Retry and commit rules** are copy's, unchanged, for both sources (§5.4, §5.7): every attempt
starts over from byte 0 of the staged file on a fresh temp; a failure during the commit after a
complete stream is retried the same way; four failures give `unreachable` with Retry. A
staged-file read error is Commander's own (`error`, not retried, not `unreachable`). An upload's
`Chtimes` takes the browser's `lastModified`, best-effort; an absent or zero value skips the
call, since FAT cannot hold a 1970 timestamp (`TestUploadRetry`, `TestUploadMtime`,
`TestUploadAppeared`, `TestUploadOverwriteBound`).

### 6.4 The upload service and its pre-write check

The service has four calls, each taking the requesting session's owner key as an opaque string:

- `UploadStage`: the name checks (validator, strict segments, 224-byte cap), the cap and the
  disk reserve, all before the first byte is read; then it streams the body to a fresh staging
  file, requiring exactly the declared size, and answers the id and size.
- `UploadPlan`: resolves the staging id (else `staging_gone`), then the plan of §6.3.
- `Upload`: resolves the id; validates the place and the strict segments; checks the reserved-path
  rule as a `watched` refusal; takes the operation lock as "Upload"; builds a one-file run with
  the overwrite binding; runs the pre-write check; and calls the sink's write with the staged
  file as source on the destination rig alone, emitting copy's lines. It never deletes the staged
  file.
- `UploadDiscard`: deletes a staged file the session owns. While a plan or run is using it, the
  id is gone at once and the file is deleted when the last user lets go. A repeat discard answers
  `staging_gone` with no side effect; the client ignores a discard's answer.

**The pre-write check** runs after the lock and before the first stream line, in one `withRig` on
the destination rig: check out the session (the identity probe), require it writable, mount the
share, and `Stat` the folder (skipped at the share root), which must exist and be a folder. It
creates nothing. Its failures are plain JSON refusals before any stream line, so the dialog
returns to the Plan with the reason, nothing was written and the staged file is kept:

| Failure | Code |
| --- | --- |
| The identity probe finds another device | `identity` |
| The rig is still host-bound | `host_bound` |
| The share is gone | `not_found` |
| The folder is gone since the plan | `not_found` |
| A file stands at the folder's path | `type_conflict` |
| The rig stays unreachable through every attempt | `unreachable` |
| Access denied, or a cancel | `denied`, or copy's Stop |

The lock is held through to the write, so nothing Commander runs can remove the folder in
between; another SMB client still can, and a folder removed in that instant fails the write as an
`error` row with Retry, as a copy into a folder vanished after its own folder pass does
(`TestUploadRefusals`).

### 6.5 The rename service

`Rename(rig, share, dir, name, newName)`, in order:

1. **Validate**: share, dir, `checkName(name)`, the strict segments of `dir` and `name`,
   `validateName(newName)`, and `newName != name` exactly (`bad_path` "That is already its
   name.").
2. **Refuse from the config and the store**, with no device I/O: read `ResidentLeads`, derive the
   store-kept roots, and apply R1 and R2's new-name half (`rig.RenameToRefused`) over every
   configured source and those roots. A store read error refuses the rename, unclassified, with a
   Warn (`TestRenameStoreErrorRefuses`).
3. **Take the operation lock** as "Rename". Unlike New folder, rename moves an existing entry that
   a running copy, upload, delete or download may name: without the lock a running delete would
   count a renamed file as already gone and a copy would fail into a vanished folder. The lock
   does not span the user's review of a delete plan (plan and run take it separately), so a
   rename between them makes that delete count the file as gone; the delete run's own re-checks
   keep it from removing anything else. The lock costs nothing for an instant call.
4. **On the device**, in `withRig`: require writable; list `dir` and find the entry named exactly
   `name` (else `not_found`), which also tells a folder from a file; apply R1 and R2's old-name
   half (`rig.RenameFromRefused`) and the new-name half again on the session's config; run the
   layout guard for R3; then the never-replacing `Rename(dir/name, dir/newName)`.
5. **Map the answer**: `IsExist` → `exists` in its one wording; `IsInUse` → `in_use`, not retried;
   not-found → `not_found` with the neutral wording; anything else through `classify`. A network
   failure is retried by `withRig`; a retry after a lost reply finds the source gone and answers
   `not_found`, which the wording and the re-list cover.

#### The rename refusals

"Watched root" here means, for that rig and share, in either layout (flat, or nested under
`ASIAIR/`), compared ignoring case, a root that is **configured** (a source enabled or disabled)
**or still has frame rows in play in the store**. The **reserved paths** of a rig and share are
every watched root, plus `ASIAIR` at a removable share's root whatever sources are configured. A
configured root counts whatever its rows: disabling a source keeps its rows undrained (the delete
pass skips a disabled share), and a rename around its root would surface as vanished rows,
re-pulls and listing failures once it is enabled again. A root no longer configured counts while
the store holds a row in play under it: a share removed from the config still drains its recorded
frames, and renaming an undrained frame there gives the same false "deleted" record or
`frame_lost` alert R1 exists to prevent. The refusal lifts once those rows drain; on a
keep-originals rig its verified rows never drain, so the refusal stays.

| # | Refused when | Why |
| --- | --- | --- |
| R1 | The item's folder lies in a watched root: a frame, a preview or a subfolder in `Autorun/`. A folder the harvester's walk prunes as junk is not watched, and neither is what it holds. | The harvester keys a frame row by its device path. The old row closes as vanished (a false "deleted" record, `frame_lost` when the NAS copy is unhealthy), the renamed path is pulled and filed a second time under its target name, and a preview unpairs from its frame. |
| R2 | A reserved path is involved, in either direction. Old name: the item is a **folder** that is, or holds, a reserved path (`Autorun` itself, Udisk `ASIAIR`, `Foo` for a root `Foo/Bar`). New name: the target would be, or would hold, a reserved path, file or folder. A *file* named like a reserved path stays renamable. | A missing root fails the share's listing until it exists again, so nothing on the share is archived and Safe to power off is withheld. A name that becomes a root creates a harvested folder or switches the share's data layout. Udisk `ASIAIR` also holds the media marker: moved out of the reader's sight, the next pass mints a fresh media identity and the medium-swap guard silently resets. Renaming a stray *file* away repairs the first hazard, so it stays allowed. |
| R3 | The layout guard: at a removable share's root, the target equals any configured root while `ASIAIR/` holds any configured root as a folder. | The guard of §7, its verb generalised ("Renaming to ‹new› here would hide ‹share›/ASIAIR/‹root› from Commander's harvester."). R2 already refuses every name R3 would, with no device I/O; R3 is defence in depth against the two rules drifting apart. |
| R4 | The old or target name is a marker name. | §8.1 invariant 3, in `checkName` and the validator. |

R2 is one rule over one set: it covers a folder holding a root, a name becoming a root, and Udisk
`ASIAIR` in both directions, so every harvester and marker hazard is refused by one rule with one
sentence per direction. It goes further than "inside folders Commander watches" in both
directions, because each case is the same harvester pollution or share-wide stop as R1, and a
refusal with its reason is simpler than a warning step the rename dialog does not have.

**Causes and sentences.** The rule (`rig.RenameFromRefused`, `rig.RenameToRefused`) reports the
rule and its cause, tested in this order: an enabled source, a disabled source, a removable
share's data folder (R2 only), a store-kept root. The cause picks the sentence:

- R1: "Commander archives this folder by name; renaming here would make it archive these frames
  again." When only a disabled source's root causes it, the server appends that the root belongs
  to a capture source that is turned off and still counts while configured, because no Watched
  badge shows there.
- R2, by direction: "‹name› is, or holds, ‹share›/‹root›, a folder Commander depends on (a
  configured capture source, even one turned off, or the ASIAIR's own data folder). Renaming it
  would disrupt archiving on ‹share›, so it is refused.", and the same for a target that "would
  be, or would hold" the reserved path.
- A store-kept root, under either rule: "‹root› still has frames Commander is archiving or
  tracking; renaming here would create false records. It is refused until they have drained."

**The store-kept roots.** Rows in play are the store's resident states (verified, unclassified,
held, failed), the set it already counts as still on the device; deleted and dismissed rows are
not in play. A removed source's roots are no longer in the config, so devfiles derives them from
those rows' source paths, which the harvester writes in its key form:

- **The query.** `ResidentLeads(rig, share)` returns, distinct, the *lead* of every resident row of
  that rig on that share (share matched ignoring case): its `source_path` cut to its first two
  segments when it has three or more, else to its first segment (`Autorun/Light/M31/a.fit` →
  `Autorun/Light`, `ASIAIR/Autorun/a.fit` → `ASIAIR/Autorun`, `Autorun/a.fit` → `Autorun`). The
  `DISTINCT` is computed in SQL, so a keep-originals rig with years of resident rows answers a
  handful of strings (`TestResidentLeads`).
- **The derivation** (`storeKeptRoots`, pure): first every lead at, under or above a root the
  config still names (that share, either layout, any case, enabled or disabled) is dropped, so a
  configured root is matched exactly as configured and never widened. A lead lies above a
  configured root when the root has more segments than the lead was cut to (Udisk root
  `Autorun/Light` gives the lead `ASIAIR/Autorun`; eMMC root `A/B/C` the lead `A/B`). Of the
  rest, a two-segment lead whose first segment is exactly `ASIAIR` (keys carry it literally) is a
  root as it stands; any other lead's root is its first segment, which keeps a removed
  multi-segment root `Foo/Bar` as `Foo` and so refuses a little more than the root itself.
- **When.** Devfiles reads `ResidentLeads` once per listing of a share folder (for the per-entry
  verdicts) and once per rename. On a store read error the listing still answers but marks every
  entry's `rename_refused` with "Commander couldn't read its records to check renames here. Try
  again." and logs a Warn, rather than offering a rename it could not check.

`internal/rig` stays pure: devfiles computes the store-kept roots and passes them in with the
configured sources, while the badge keeps passing only the enabled sources. R1, R2's old-name
half and R4 depend only on the listed entry, so the listing reports them per entry as `{rule,
message}` and the toolbar shows them before the dialog opens; the client derives "caused only by
a disabled source or a store-kept root" as R1 on a folder whose `watched` flag is false. R2's
new-name half and R3 depend on the typed name and come back on submit; R3 needs a device `Stat`,
so it is server-only. The client's knowledge only saves a round trip: the server decides every
refusal.

`TestRenameRefusalTable`, `TestRenameRefusals` (both layouts, mixed case, disabled, removed and
drained sources, zero device calls for config- and store-decided rows), `TestListRenameRefused`
(each entry's verdict equals the route's) and `TestRenameRuleKeepsRootResolution` (renaming any
folder the rule allows leaves the harvester's root resolution unchanged) guard the rule;
`TestRenameFile`, `TestRenameFolder`, `TestRenameExists` and `TestRenameDeviceAnswers` the device
step, with `TestDevfilesServiceRename` and `TestDevfilesRenameCaseOnly` against real Samba.

### 6.6 Logging

Every line carries the fields of §5.11. File contents, credentials and the owner key are never
logged.

- **Info:** file staged (staging id, name, bytes); staged file removed (staging id; discarded,
  expired or evicted); staging directory emptied at startup (files removed); upload started
  (staging id, name, bytes, overwrite, watched counts); the sink's "file written" with
  `op=Upload` and the staging id in place of copy's source fields; renamed (from, to, folder or
  file).
- **Warn:** staging refused (the reason, and the free bytes for a disk refusal); a staging
  receive cut short (bytes received and expected); a staged file that could not be removed; a
  reserved-path refusal of a plan or an item (operation, paths); rename refused (rule, cause and
  message); the store read for rename failing; temp cleanup failed; each retry.
- **Debug:** the upload plan (counts, conflicts, free space); each expiry sweep (files held,
  files removed); bytes drained before a staging refusal.

## 7. Harvester interaction

There is no locking and no pausing. Each case follows from the harvester's ordinary rules, and
none needs a special case in harvester code: the pull, delete and reconciliation paths and the content-hash
three-way check are untouched by the tool (§8.1 invariant 5).

- **Badges** come from `WatchedDir`: an enabled source, either layout, junk folders excluded.
  Disabled sources show no badge. On a Udisk that holds both `Autorun/` and `ASIAIR/Autorun/`,
  both are badged; over-badging is the safe direction for a warning.
- **The layout guard.** The harvester picks one data layout per share per cycle
  (`resolveDataPrefix`): it probes the configured roots in config order, and the first root
  that lists at the share root picks the flat layout for the whole share. A folder named like a
  configured root created at a removable share's root therefore makes the harvester walk that
  folder and never list `ASIAIR/<root>`, while the listing still counts as complete, so the rig
  could report Safe to power off with unarchived frames on the stick
  ([delete-safety §5](2026-10-17-delete-safety.md)). On some devices a guest can create folders at
  the Udisk share root ([asiair-hardware-notes §3.6](2026-10-17-asiair-hardware-notes.md)), so
  the hazard is reachable. Any mkdir, any folder a copy would create, and any rename target at a
  removable share's root whose name matches (ignoring case) any configured root of the rig, a
  disabled source's included, while `ASIAIR/` holds any configured root as a folder, is refused
  before any write, in the plan and again in the run. The rule is "any configured root", not
  only the matching nested one, because of the ordering: with roots `[Plan, Autorun]`, a flat
  `Plan` hides `ASIAIR/Autorun` although `ASIAIR/Plan` does not exist
  (`TestLayoutGuardRefusesFlatRoot`). The guard closes the tool's own path; another SMB writer
  can still create such a folder (§9).
- **A file standing where a watched root must go** (`Autorun` as a file on an eMMC share whose
  root folder does not exist yet, `ASIAIR` as a file on a Udisk, a file `Autorun` inside Udisk
  `ASIAIR/`, a file `Bar` inside `Foo` for a root `Foo/Bar`) makes `resolveDataPrefix`'s probe or
  `listShareRoots`' walk fail the share's listing, and blocks the firmware's own mkdir. Writing a
  file creates no folder at that path, so the layout guard, which sees only folders, never meets
  it. The reserved-path rule (§6.3) is its counterpart for files: it refuses such a plan, for
  upload and copy, at any depth, with `watched`.
- **Rename never runs inside, on, around or into a watched root** (R1, R2), configured or kept in
  play by the store, so the harvester never sees a tracked path move. Outside watched folders (an
  unconfigured `Preview/` with no rows in play) a rename is invisible to it. R2 and R3 keep a
  rename from flipping the share's data layout in either direction, on every share, not only
  removable ones.
- **Markers.** Identity markers are files at a share root or in a removable share's `ASIAIR/`
  ([device-identity-and-media §6.2](2026-10-17-device-identity-and-media.md)), and `checkName`
  refuses a marker name anywhere. A removable share's `ASIAIR/` is reserved by R2 whatever
  sources are configured. A rename therefore never moves a marker from a location
  Commander reads; a marker file another client left elsewhere moves with its folder, harmlessly.
- **Copying or uploading into a watched folder.** Frames are classified from their headers and
  filed under the destination rig's OTA and session, then deleted from the device after grace
  unless the rig keeps originals. Previews without their frame are swept as orphans, also gated
  by keep-originals. The temp-and-rename commit means a partial file is never listed, so it can
  never be pulled or treated as a changed file. The write carries the source's mtime (or the
  browser's `lastModified`), which is usually old, so the file passes `min_file_age` as soon as
  the device clock agrees and waits only for `settle_cycles`
  ([frame-lifecycle §4.4](2026-10-17-frame-lifecycle.md)); the watched warning already says the
  files will be archived and removed, and how soon does not change what the user agreed to. If a
  listing falls between the commit and `Chtimes`, the settle check sees the mtime change, so the
  file is never pulled in that window. A file whose `Chtimes` failed keeps the copy time and
  waits out `min_file_age` like a fresh capture; a device clock running behind only delays the
  harvest.
- **Overwriting a frame the harvester tracks** sends different content to a known path. The
  revision discriminator either recognises a revision or treats it as a genuine clash: it files
  a token-suffixed copy, keeps both and raises `rot_clash`; a clash the delete pass reaches first
  defers to a later `delete_mismatch` ([frame-lifecycle §11](2026-10-17-frame-lifecycle.md)). The
  conflict row's `dst_ledger` hint says so before the user chooses. The tool does not prevent it,
  because the user decides.
- **Deleting.** An archived frame's device file resolves through the delete pass's
  vanished-source branch: verified good on the NAS, marked deleted, no alert. An unarchived frame
  was confirmed by the checkbox; a culled one raises the doubly-vanished Warn. `failed` and `held`
  rows of deleted files stay in the store (§9). Deleting a watched root, or a folder containing
  one, fails that share's whole listing until the folder exists again; the dialog says so and
  suggests New folder.
- **Reads that hold a file open** (a download, or a copy from a watched folder) do not pass
  `FILE_SHARE_DELETE`, so a harvester delete of that frame during the read fails with a sharing
  violation, raises its existing Warn and is retried on a later pass. That is benign and is the
  visible side effect of "no locking". The converse, a tool delete or commit meeting a file the
  harvester or the ASIAIR holds open, is reported as `in_use` with Retry.
- **Identity.** Every tool session re-proves the device, so a DHCP handoff cannot redirect a
  delete or a write to another ASIAIR. An IP handoff during an open session breaks the TCP
  connection rather than redirecting it, so one probe per session is enough
  ([device-identity-and-media §4.2](2026-10-17-device-identity-and-media.md)).
- **Staging is invisible to the harvester.** Phase 1 writes only under Commander's data directory
  and touches no device.

## 8. Invariants

### 8.1 File browser invariants 1–8

These restate the rules in the `internal/devfiles` package comment (`doc.go`), which every engine
in the package keeps. The package comment groups them differently: its rule 1 also carries the
sink and rename rules (§8.2 invariants 1 and 4 here), its rule 3 the marker-location sentence,
and its two staging bullets are among §8.2's invariants.

1. No device file is replaced or removed by a copy or an upload except a destination named in
   that request's overwrite list, and only while it still has the size and mtime the user saw.
   Every write lands on a fresh, exclusively created temp name and reaches its final name only
   through a never-replacing rename. *Guarded by* `TestCopySkipsAppearedDestination`,
   `TestCopyCommitCollisionSkips`, `TestOverwriteSkipsChangedDestination`,
   `TestCopyRetryLeavesNoTemp`, `TestFakeCreateNewExclusive`, `TestUploadAppeared`,
   `TestUploadOverwriteBound`, and against real Samba `TestDevfilesRenameNeverReplaces` and
   `TestDevfilesCreateNewExclusive`.
2. A delete run removes only files enumerated in the plan the user confirmed, each only while its
   size and mtime still match, and folders only while they are still folders and empty. *Guarded
   by* `TestDeleteRunOnlyEchoedItems`, `TestDeleteRunDirTypeCheck`,
   `TestDeleteRunNotEmptyFromDeviceError` and `TestDeleteRunUnarchivedRecount`.
3. Commander's media marker and the rig-identity marker name are never listed, walked,
   copied, uploaded, zipped, overwritten, renamed or deleted by the tool; a request naming one is
   refused. *Guarded by* `TestMarkersInvisible` and `TestValidateName`.
4. Every SMB session the tool opens re-proves the device's identity with a live probe while it is
   open. No reply means no session, a foreign reply is refused, and a rig without a guid is never
   written to. Only managed rigs are served, at the endpoint their worker last published.
   *Guarded by* `TestSessionIdentity`.
5. The tool writes nothing to the store, holds no harvester lock and calls no worker. The pull,
   delete and reconciliation paths and the content-hash three-way check are unchanged by it.
   *Guarded by* the read-only `FrameStore` and `LocalFS` interfaces (the service has no write
   method to call), `TestArchivedCopyImpliesNoPull` and `TestHarvestViewAgreesWithWalk`.
6. A plan, summary or zip is built only from a complete walk; a listing failure fails it
   outright. *Guarded by* `TestWalkFailsWhole`.
7. The tool's device I/O holds no lock shared with a worker, the hub or the snapshot read path,
   and the service's own locks are never held across device I/O. *Guarded by*
   `TestDevfilesListingNeverBlocksReadPath`.
8. At most one copy, upload, rename, delete, download or plan walk runs at a time across the
   tool, and the tool holds at most two sessions per rig. Mkdir, listings and upload staging stay
   outside the lock. *Guarded by* `TestOperationLockAndCap` and `TestSessionLifecycle`, with the
   rename and upload busy cases in `TestRenameDeviceAnswers` and `TestUploadRefusals`.

### 8.2 Rename and upload invariants 1–5

1. Copy and upload reach the device through one write function and one commit, so §8.1
   invariant 1 holds for both by construction. *Guarded by* `TestSinkPlanUploadMatchesCopy`,
   `TestUploadRetry`, `TestUploadAppeared` and `TestUploadOverwriteBound`.
2. An upload writes to a device only from a staged file whose receive ended at exactly its
   declared length. A short, failed or cancelled receive leaves no staged file and touches no
   device. *Guarded by* `TestStageShortBody`, `TestStageBodyReadError`, `TestStageCancel`,
   `TestStagingComplete` and `TestFilesUploadClientAbortLeavesNothingStaged`.
3. Staged bytes live only in the staging directory and on the destination device: they are never
   logged, no route serves them, and only the session that staged a file can plan, run or discard
   it. A staged file is removed on discard, on expiry, on eviction at the cap and at startup; no
   run removes it. *Guarded by* `TestStagingNeverLogsContentOrOwner`,
   `TestFilesNoRouteServesStagedBytes`, `TestFilesUploadSessionsIsolated`,
   `TestStagingKeptByRuns`, `TestStagingDiscard`, `TestStagingExpiry`, `TestStagingCap` and
   `TestStagingStartEmpties`.
4. A rename touches exactly one entry, only through the device's never-replacing rename, and
   never one R1–R4 refuses. R1 and R2 cover every root configured for that rig and share, enabled
   or disabled, and every root the store still holds rows in play under; a store read error
   refuses the rename. It never moves a marker from a location Commander reads (a share root, or
   a removable share's `ASIAIR/`). *Guarded by* `TestRenameRefusalTable`, `TestRenameRefusals`,
   `TestRenameStoreErrorRefuses`, `TestListRenameRefused`, `TestRenameRuleKeepsRootResolution`,
   `TestRenameExists` and `TestRenameNotListedExactly`.
5. Rename and upload hold the operation lock of §8.1 invariant 8; upload staging does not.
   *Guarded by* the busy cases in `TestRenameDeviceAnswers` and `TestUploadRefusals`.

## 9. Residual risks

- **Another SMB writer can still flip a share's layout.** The layout guard and R2/R3 close the
  tool's own paths; a folder named like a configured root created at a Udisk root by any other
  client still makes the harvester walk it and never list `ASIAIR/<root>`, with a listing that
  reads complete. This is a harvester-side gap outside the tool.
- **Stale `failed` and `held` rows** remain after a tool delete of a not-yet-archived file, as
  after any out-of-band delete. The tool neither widens the gap in kind nor fixes it.
- **A torn copy of a file rewritten in place.** A copy of a file the ASIAIR rewrites mid-copy
  (H13) can be torn. Frames being captured are not the tool's use case; Retry re-copies the file.
- **A paused download.** A consumer that stalls longer than the rig's `SMBTimeout` between reads
  (in practice a browser download the user paused) lets the armed deadline expire under the SMB
  client's receive loop, which kills that session. The download fails mid-stream through the
  abort path and the user downloads again; inside a cross-rig copy the item fails with a network
  error and is retried on fresh sessions. The worker's pull read path is deliberately not changed
  to avoid this.
- **A lost commit reply** gives a spurious `appeared` or `changed` skip; the device holds the
  complete file, and the confirmed Overwrite resolves it.
- **An overwrite half done.** If an overwrite's delete succeeds and its rename fails, the
  destination holds neither file until a retry; the source is intact and the row says so.
- **Writes to a remote rig are not rate-limited.** The tool's dials go through the site registry,
  whose read limiter applies, but writes are not limited and the tool takes no transfer
  admission slot, so a large copy to a remote rig competes with that site's pulls. Use is
  occasional and user-initiated, and the Info log records each copy's start, so the overlap is
  visible.
- **A removed root that is an ancestor of a configured root** (removed `Autorun` while
  `Autorun/Light` stays configured) yields no store-kept root, because its leads hold the
  configured root and are dropped; a file directly in `Autorun/` or in a sibling such as
  `Autorun/Dark` is then renamable while its frames are undrained, which can produce the false
  record R1 exists to prevent. R2 still refuses renaming `Autorun` itself. Accepted: it needs a
  removed root that is an ancestor of a configured one.
- **An 8.3 alias in `dir`** stays reachable by a crafted request from an authenticated user. The
  usual roots (`Autorun`, `Plan`, `ASIAIR`) are 8.3 names already and have no alias.
- **Sequential transfers.** An upload crosses the browser link in full before its first byte
  reaches the ASIAIR, so it takes the two transfers' sum rather than the slower one's time: the
  cost of staging.
- **Disk space on Commander.** Up to four files at once sit in the data directory, each until its
  dialog closes or for up to an hour after its last use. The reserve check counts every receiving
  file's declared size, so parallel uploads cannot fill the volume between them; it cannot see
  growth by something else on the volume (Commander's own store and logs, or another process)
  during a receive.
- **Staging versus pull admission.** If the data directory shares a volume with the archive or
  staging roots, staged files reduce the free space pull admission checks
  ([frame-lifecycle §5.1](2026-10-17-frame-lifecycle.md)), which can delay pulls; bounded by the
  cap and the expiry.
- **A conflict or reserved-path refusal shows after the send**, because the plan follows staging;
  the staged file then serves Overwrite without a re-send.
- **A staging refusal waits for the body**, since the drain reads it before answering; the
  client's own name check makes a name refusal there rare.
- **A suspended tab holds a staging slot.** With no read deadline on the staging body, a phone
  that suspends the tab mid-send keeps its slot and partial file until the connection closes. It
  counts toward the cap and is never evicted while receiving, but touches no device and holds no
  lock.
- **Mobile backgrounding.** A phone that locks or backgrounds the tab suspends the request: while
  sending, nothing reaches the device and the user sends again; while writing or copying, the
  "keep this page open" contract applies and the run reports as interrupted.
- **Unverified on hardware:** a case-only self-rename (the integration test asserts only that the
  file is never lost) and upload throughput through a remote site's tunnel.

## 10. Rejected alternatives

| Alternative | Why not |
| --- | --- |
| Run browsing and copying on the rig worker goroutine (supervisor methods, request channels) | A multi-GB copy would stall that rig's pulls, a cross-rig copy would need two workers to cooperate, and it would widen `web.Supervisor` and every implementer, the build-tagged one included. |
| List every configured rig, unmanaged ones included, through an endpoint resolver lifted out of the worker | An unmanaged rig has no worker to bind its guid, so it could never be written to, and reaching it needs a second copy of the worker's endpoint resolution. |
| Background jobs with ids and SSE progress | A job registry, a snapshot field, server-held results for retry and cleanup rules. A request-scoped NDJSON stream gives progress and cancel; "keep the page open" is acceptable for a tool. |
| One long-lived session per rig behind a mutex, or the SMB library's connection pool | A copy would hold the rig's listing lock for minutes; two rigs need lock ordering and a same-rig copy deadlocks unless special-cased. The pool knows nothing of endpoint changes or identity re-proof. |
| A cached browse session plus dedicated operation sessions | Two lifecycles, two timeout budgets and a redial rule for each. Checkout and checkin with one idle slot and a cap is one lifecycle. |
| One session per pane | Pane ids are global, so two tabs or devices would share and fight over one session, and one tab's copy would block another's listings. |
| A fresh session per request | Every folder click would pay dial, negotiate, auth and a UDP identity probe, about a second over a tunnel. |
| Probe identity per operation | An IP handoff breaks the TCP session, so one probe per session open already ties the session to the device. |
| A parked-session key of endpoint, user, password hash, guid and cpuId | Keying on the config pointer closes the session on every one of those changes without holding a secret's hash. |
| Taking two rigs' slots in rig-id order | Every two-slot request already holds the operation lock, so no ordering is needed. |
| Abort in-flight I/O on cancel (a session abort hook) | It widens the device session interface for a faster lock release after Stop. Cancel only stops work; a stalled call returns at `SMBTimeout`, and `busy` says the holder is still stopping. |
| Clear the idle deadline after every read and write | It changes the worker's pull read path to save a paused browser download, which the user can repeat (§9). |
| An auth predicate and a tunnel-down fast-fail in the retry policy | Both change only how fast an error is reported; non-network errors already return at once, and a down tunnel fails each dial fast. |
| Write straight to the final name | A partial file would sit under its final name. In a watched folder a stranded partial is size-stable, passes the settle check, and is archived as a truncated frame, then deleted from the device; "appeared after the check" would become a race between a stat and a write. |
| A fixed temp name opened with a truncating create | It would destroy a user file that happens to have that name, and two copies of one name would collide. A random suffix opened exclusively cannot destroy anything. |
| Stat, then create, for the run-time conflict check | A window between check and write, and a truncating create would overwrite a file appearing in it. |
| Rename the old file aside until the commit | Two more renames and their own failure states, for a file the user chose to replace while the source is intact; the report states the half-done case instead. |
| Pick another temp suffix on a clash, and name a leftover temp in the report | A 64-bit random suffix does not collide in practice, the exclusive create cannot destroy a file, and a leftover temp is visible in the pane. |
| Leave the copy time as the destination mtime (no `Chtimes`) | The source mtime is what the user expects to see. Setting it best-effort, never retried, meets the objections (an extra write verb; an old mtime passing `min_file_age` at once in a watched folder, which the watched warning covers). |
| Read-back hash, or a post-copy size `Stat` | It doubles the transfer, or costs a round trip per file. Exclusive create plus a clean close and rename is the check. |
| Check the device before recopying after a lost commit reply | Extra branches in the commit path to avoid a spurious skip that the confirmed Overwrite resolves. |
| A ledger lookup in the copy run for `appeared` and `changed` rows | Those rows always confirm an Overwrite, so the value would change nothing the user sees. |
| A "same size and time" hint in the conflict table | Informational only; the two columns sit side by side. |
| Pair `_thn` previews with planned frames for the preview count | Every preview in a watched folder leaves the device unarchived either way; pairing only changes when. |
| A delete run that re-walks the selection | It would delete files that appeared after the summary, including frames still being captured, under a confirmation that never counted them. |
| A boolean "allow unarchived" acknowledgement | It does not bind to the number the user saw. |
| Retry a sharing violation automatically | The holder (the ASIAIR writing, the harvester pulling) releases the file on its own schedule, so a short automatic retry mostly adds delay; the library already retries directory deletes for about 6.5 s. |
| Refuse to delete watched roots | A confirmed delete is the user's call; the dialog's warning states the share-wide effect and the fix. |
| An advance "will be emptied but kept" line for marker folders in the delete summary | The report's `kept_marker` row explains the rare case afterwards. |
| A `_download-errors.txt` entry in a partly failed zip | A zip either completes or aborts, and the `check` request reports walk errors before the download starts. |
| A status endpoint or list-response field showing the operation lock | Listings are not polled, so it would go stale; `busy` names the operation, its rigs and its start, and plans meet it at Checking. |
| An expandable list of unarchived names in the delete dialog | Beyond the count and checkbox the confirmation needs, for a tool kept low in complexity. |
| Grouping conflicts by folder | Overwrite all and Skip all already cover the bulk choice. |
| Shift+click range selection | Not required, and the tool is kept low in complexity. |
| A streamed walk count while a plan is prepared | It would turn the plan routes into streams; the dialog shows elapsed seconds and Cancel instead. |
| A per-file size check after each copy to catch torn copies | Kept out for simplicity; §9 records the residual case of a file the ASIAIR rewrites mid-copy. |
| Deflate in zips | CPU on low-end hosts for no gain on FITS and JPEG. |
| Take a transfer admission slot | A user's copy would queue silently behind the harvester's backlog, and the disk reserve it protects is the NAS's, which the tool does not write. |
| Separate byte units (MiB) for this page | A second formatter beside `formatBytes`, contradicting every other page. |
| A folder tree sidebar | Expanded nodes, per-node loading and selection across levels, for no requirement. A breadcrumb is lazy by construction and works on mobile. |
| Walk a pane up to the nearest existing ancestor when its folder is gone | An empty state and the breadcrumb do the same with no re-list loop. |
| Server-sent `not_attempted` lines and a `cancelled` summary after a cancel; a separate Interrupted report | The client has aborted the fetch, so those lines are never read; one client-built report covers Stop and a lost connection. |
| A `retry` stream line, and byte progress in a retrying report row | A client timer ("Waiting for ‹rig› to respond…" after 5 s) gives the same signal with no wire format. |
| Stream the upload straight from the browser to the ASIAIR, staging nothing | A request body cannot be read twice, so a streamed upload cannot be retried once its first byte is sent, and it needs machinery of its own: a read-idle bound on the body below `SMBTimeout` so a stalled browser cannot hold the operation lock, a full-chunk reader so short reads do not defeat the SMB client's write pipelining, wording for a slow browser, error marks that keep a body read error from being retried as a rig failure, and a drain after a device failure. Staging costs disk space for one file and sequential transfers. |
| Batch upload (several files per pick) | The use case is one reference file, and copy covers many files between ASIAIRs. A batch adds a sequencer, a multi-file conflict table, rules for which refusals end the batch, a pick-size limit, a case-clash check across picked files, and handling for a share filling partway; it can be added later without touching the write path. |
| Chunked or resumable uploads | The temp, the overwrite binding and the lock would live across requests: server-side job state. |
| A multipart form for the staging request | A body parser for one file the raw body carries directly, with the name and `lastModified` as query parameters. |
| `fetch` with the file or a stream as the body | No upload progress, and streamed request bodies need HTTP/2 and half-duplex mode. |
| Keep the sink inside the copy code and extract only the source behind an interface | Smaller, but a separate sink makes "every device write goes through one function" a mechanical check. |
| A standalone sink that also owns the run counters and NDJSON lines | Upload's one-file run reuses copy's line types where they are; moving them enlarges the safety-critical code for nothing. |
| Refuse every further upload at the cap | A crashed tab's staged files would block uploads for up to an hour; evicting an idle one costs only a re-send from a dialog that still holds the `File`. |
| Leave staged files unscoped, or scope them by client address | Any signed-in browser could run or discard another's file by id, and addresses change behind proxies and on mobile networks; the session's XSRF claim is per login and stable. |
| Keep staged files across a restart with an on-disk index | A second store to keep consistent for a Retry that a restart has usually outlived; the dialog re-stages from its `File`. |
| Answer a staging refusal without draining the body | On HTTP/1.1 the browser often sees a reset instead of the refusal and reports a lost connection. |
| Drain `guardWrite`'s refusals too | It would read an unauthenticated body to its end; the next request surfaces the `401`. |
| A fixed size limit on staged files | The real constraint is Commander's disk, which the free-space check with its 1 GiB reserve answers for any size. |
| Receive one staged file at a time, or a single free-space check | One at a time blocks parallel uploads from several tabs; a single check lets four concurrent receives fill the volume together. Summing the receiving records' declared lengths is exact and needs no extra state. |
| Delete the staged file when a run reports `copied` | A Stop or drop just after the commit reports `copied` while the browser shows stopped, so Retry would have to re-send the whole file; keeping the file until the dialog closes is one rule fewer. |
| Detect on Retry that the file is already complete (same size and time) | A heuristic for a rare race; the existing `appeared`/`changed` answer never replaces without confirmation, and the stopped wording warns of it. |
| A dedicated reason code for a failure partway through a file | A code end to end for what `error` with Retry already expresses. |
| Probe a `File` after a network error to tell a changed PC file from a lost connection | Browser behaviour varies by engine and it adds a reason code; the interrupted result's Retry, or a fresh pick, covers it. |
| Probe the session after a network error and hold the sign-in redirect while the result is open | An auth-flow change for one dialog, for an expiry that needs the session's maximum age or a logout to land inside an upload. |
| Retry only the commit after a failure following a complete stream | The bytes are at hand, so the item is retried from byte 0 on a fresh temp, exactly as a copy's. |
| Let the upload run's device refusals become result rows | `host_bound` and a vanished folder would read as a generic `error` naming the temp path; the pre-write check gives a refusal the dialog can explain, and never offers a Retry that cannot work. |
| Recognise a full device through a disk-full predicate, and warn about FAT32's 4 GiB limit | A library change and hardware probes for a warning that is false on an exFAT stick; the device's own message reports the full disk. |
| List a file named like a reserved path as a skip-only conflict of its own kind | A plan schema addition rendered by two dialogs and the report; refusing the whole plan is stricter and the case is rare. |
| Shorten temp names so 255-byte names upload | A behaviour change in the shared sink; a 224-byte cap refuses instead. |
| Warn instead of refusing a rename inside a watched folder | A warning step the dialog does not have, for an operation whose every effect there is harmful (§6.5). |
| Decide rename refusals from the listing's `watched` flags alone | The listing cannot show an ancestor of a root (Udisk `ASIAIR` reads `watched: false`) or a name that becomes one. |
| Server-only rename refusal, no per-entry verdict in the listing | The user would type a name before learning the item cannot be renamed; the per-entry verdict is one pure call per entry with no I/O. |
| Separate rename rules, each with its own sentence, and a disabled-only flag in the listing | Their union refuses exactly what one reserved-path rule refuses, so only the wording differs; the client derives the disabled-only case from the `watched` flag. |
| Lift the rename refusal as soon as a source is removed from the config | A removed share's frames still drain, so renaming an undrained one gives a false "deleted" record at once; one rule keyed on rows in play covers enabled, disabled and removed sources alike. |
| Walk a selected folder's subtree to find watched content before a rename | `HoldsWatchedRoot` answers from the profile with no I/O; a walk can fail or be partial. |
| Rename as copy then delete | Not atomic, doubles the I/O, and bypasses the device's never-replacing rename. |
| Case-aware `exists` wordings naming the listed spelling | Three sentences and a listing lookup for wording only; a rename never replaces either way. |
| Move the other pane along when it shows a renamed folder | A pane-place and query-string rewrite for a corner case; the pane shows the existing "no longer exists" state. |
| Check the destination after a lost rename reply before answering | The neutral `not_found` wording and the re-list cover a rename that went through. |
| Refuse wildcard and trailing-dot segments in `checkRel` for every operation | It would change the delete path's validation for no gain there; rename and upload are the operations a crafted spelling could misdirect. |
