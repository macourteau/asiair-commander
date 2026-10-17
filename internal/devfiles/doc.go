// Package devfiles is the device file browser's service: it lists, creates
// folders on, copies between, uploads to, renames on, deletes from and
// downloads from the managed rigs' ASIAIRs, on the calling HTTP request's
// goroutine and independently of the rig workers
// (design device-file-browser).
//
// It is Commander's first general device write path, so it owns these safety
// rules, which every engine in the package keeps:
//
//  1. A copy or an upload replaces or removes no device file except a
//     destination named in that request's overwrite list, and only while it
//     still has the size and mtime the user saw. Every write lands on a fresh,
//     exclusively created temp name and reaches its final name only through the
//     device's never-replacing rename. Copy and upload reach the device through
//     one write function and one commit (the sink). A rename moves exactly one
//     entry, only through that never-replacing rename, and never one the rename
//     rule refuses (rig.RenameFromRefused, rig.RenameToRefused, the layout
//     guard, marker names): the rule covers every root configured for the rig
//     and share, enabled or disabled, and every root the store still holds rows
//     in play under, and a store read error refuses the rename.
//  2. A delete removes only files enumerated in the plan the user confirmed,
//     each only while its size and mtime still match, and folders only while
//     they are still folders and empty.
//  3. Commander's marker files (model.MediaMarkerName, model.IdentityMarkerName,
//     matched case-insensitively) are never listed, walked, copied, uploaded,
//     zipped, overwritten, renamed or deleted; a request naming one is refused.
//     A rename never moves a marker from a location Commander reads it (a share
//     root, or a removable share's ASIAIR/).
//  4. Every session re-proves the device's identity with a live probe while it
//     is open: no reply means no session, a foreign reply is refused, and a rig
//     without a GUID is read-only. Only managed rigs are served, at the endpoint
//     their worker last published.
//  5. The service writes nothing to the store, holds no harvester lock and calls
//     no worker.
//  6. A plan, summary or zip is built only from a complete walk: any listing
//     failure, after its retries, fails it outright.
//  7. Device I/O holds no lock shared with a worker, the hub or the snapshot
//     read path; the service's own locks are never held across device I/O.
//  8. At most one copy, upload, rename, delete, download or plan walk runs at a
//     time across the tool (lockOp), and at most two tool sessions are open per
//     rig (acquire). Upload staging stays outside the lock.
//
// Upload staging (staging.go) keeps two more rules:
//
//   - An upload writes to a device only from a staged file whose receive ended
//     at exactly its declared length. A short, failed or cancelled receive
//     leaves no staged file and touches no device.
//   - Staged bytes live only in the staging directory and on the destination
//     device: they are never logged, no route serves them, and only the session
//     that staged a file can plan, run or discard it. A staged file is removed
//     on discard, on expiry, on eviction at the cap and at startup; no run
//     removes it.
//
// Shared machinery for the engines: acquire and release check sessions out
// and in; withRetry and withRig are the only retry loop; lockOp is the
// operation lock; walk and listDir are the only listings; checkShare, checkDir,
// checkRel, checkName, checkStrictSegments and validateName validate every
// path; layoutGuard runs before any folder is created or entry renamed;
// classify maps device errors onto the sentinels in errors.go.
package devfiles
