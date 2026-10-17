package rig

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/store"
)

// RecoverSupersedeIntents resolves every open supersede intent left by a crash,
// and must run to completion BEFORE any worker starts — an archive copy left under
// its reserved sibling name is a frame whose canonical path is absent, one settle
// window away from authorising the deletion of a device original (INV-M2). It takes
// its dependencies explicitly and is package-level for the same reason
// RecoverRefileIntents is: at startup there is no Worker to be a method on.
//
// It follows that function's error policy exactly. A per-intent problem — an
// unreadable path, a commit that fails again — leaves that intent OPEN and
// continues with the rest, rather than failing startup: an open intent is a
// deferral reason in absentDeferReasons, so the frame it names stays out of
// DeletionEligible until a later run resolves it. Only a failure to read the intent
// table at all is fatal, because then nothing is known about what is in flight.
func RecoverSupersedeIntents(ctx context.Context, st store.Store, lfs localfs.LocalFS, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	intents, err := st.ListSupersedeIntents(ctx)
	if err != nil {
		return fmt.Errorf("list supersede intents: %w", err)
	}
	for _, in := range intents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		recoverSupersedeIntent(ctx, st, lfs, logger, in)
	}
	if len(intents) > 0 {
		logger.Info("rig: supersede intent recovery complete", "intents", len(intents))
	}
	return nil
}

// recoverSupersedeIntent resolves one intent by observation, reading the swap's
// progress off the RESERVED SIBLING first.
//
// The order is the mirror of recoverRefileIntent's, and for the mirrored reason.
// That one checks its source first because a file at the destination proves nothing
// until the source is known to be empty: in a flat panel layout the ASIAIR's
// filenames collide by design and all lights from one camera and binning are
// identical in size, so neither name nor size distinguishes siblings. Here the
// unambiguous name is the sibling's, not the canonical one — it is derived from the
// content hash of the copy set aside, so nothing else in the library can be sitting
// under it — and it is the presence of that file, not of the canonical one, that
// establishes the swap ever began.
//
// Its absence is therefore the one state that needs no action, and it is safe
// precisely because step 3 precedes step 4: with the sibling gone, either step 1
// never ran (the canonical name still holds the copy the row describes) or step 4
// ran (so step 3 committed, and the row describes the revised copy the canonical
// name now holds). Both are consistent, so the intent is dropped and nothing is
// written. Swapping the commit and the unlink would make this arm ambiguous.
func recoverSupersedeIntent(ctx context.Context, st store.Store, lfs localfs.LocalFS, logger *slog.Logger, in store.SupersedeIntent) {
	drop := func(msg string) {
		if err := st.ClearSupersedeIntent(ctx, in.RigID, in.DestPath); err != nil {
			logger.Error("rig: could not clear a resolved supersede intent",
				"rig", in.RigID, "dest", in.DestPath, "error", err)
			return
		}
		logger.Info(msg, "rig", in.RigID, "dest", in.DestPath, "aside", in.AsidePath)
	}

	if _, _, err := lfs.Stat(in.AsidePath); err != nil {
		if isNotExist(err) {
			drop("rig: supersede intent resolved; the swap either never began or had finished")
			return
		}
		logger.Error("rig: cannot stat the set-aside copy of an open supersede intent; leaving it open",
			"rig", in.RigID, "aside", in.AsidePath, "error", err)
		return
	}

	size, _, err := lfs.Stat(in.DestPath)
	if err != nil {
		if isNotExist(err) {
			// Step 1 ran and step 2 did not: the bytes are under the reserved name and
			// the canonical name is free. Put them back — the row was never committed,
			// so it describes exactly the copy this restores.
			if rerr := lfs.RenameNoReplace(in.AsidePath, in.DestPath); rerr != nil {
				logger.Error("rig: could not roll back an interrupted supersede; leaving the intent open",
					"rig", in.RigID, "dest", in.DestPath, "aside", in.AsidePath, "error", rerr)
				return
			}
			logger.Warn("rig: rolled back an interrupted supersede; the archive copy is unchanged",
				"rig", in.RigID, "dest", in.DestPath)
			drop("rig: supersede intent resolved; the interrupted swap was rolled back")
			return
		}
		logger.Error("rig: cannot stat the canonical path of an open supersede intent; leaving it open",
			"rig", in.RigID, "dest", in.DestPath, "error", err)
		return
	}

	// Both present: step 2 landed, and the commit may or may not have. Before
	// concluding that the file at the canonical name is the one step 2 installed,
	// check it against the length the intent recorded for those bytes. Nothing of
	// ours writes that path while recovery runs, so a disagreement means something
	// outside the application put a different file there — and committing the
	// intent's anchors against it would anchor every sharing row to bytes that are
	// not there. It costs a Stat, not a read.
	if size != in.ArchiveSize {
		logger.Error("rig: the canonical path of an open supersede intent holds an unexpected file; leaving it open",
			"rig", in.RigID, "dest", in.DestPath, "size", size, "want", in.ArchiveSize)
		return
	}

	revised, sharers, err := st.CommitSupersede(ctx, in)
	if err != nil {
		logger.Error("rig: could not finish an interrupted supersede commit; leaving the intent open",
			"rig", in.RigID, "dest", in.DestPath, "error", err)
		return
	}
	// The commit is idempotent: it may already have landed before the crash, and a
	// revised row that has since been pruned leaves the sharers to describe the file.
	logger.Warn("rig: finished an interrupted supersede commit", "rig", in.RigID,
		"dest", in.DestPath, "revised", revised, "sharers", sharers)
	if err := lfs.Remove(in.AsidePath); err != nil && !isNotExist(err) {
		logger.Error("rig: could not remove a superseded archive copy; leaving the intent open",
			"rig", in.RigID, "aside", in.AsidePath, "error", err)
		return
	}
	drop("rig: supersede intent resolved; the interrupted swap was completed")
}
