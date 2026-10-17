package rig

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/store"
)

// RecoverRefileIntents resolves every open re-file intent left by a crash, and
// must run to completion BEFORE any worker starts — a row still pointing at a path
// the rename already vacated is one settle window away from authorising the
// deletion of a device original (INV-M2). It takes its dependencies explicitly and
// is package-level for the same reason RebuildNightCounters is: at startup there
// is no Worker to be a method on.
//
// A per-intent problem — an unreadable path, a commit that fails again — leaves
// that intent OPEN and continues with the rest, rather than failing startup: an
// open intent is a deferral reason in absentDeferReasons, so the frame it names
// stays out of DeletionEligible until a later run resolves it, and the guarantee
// holds without the process refusing to start. Only a failure to read the intent
// table at all is fatal, because then nothing is known about what is in flight.
func RecoverRefileIntents(ctx context.Context, st store.Store, lfs localfs.LocalFS, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	intents, err := st.ListRefileIntents(ctx)
	if err != nil {
		return fmt.Errorf("list refile intents: %w", err)
	}
	for _, in := range intents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		recoverRefileIntent(ctx, st, lfs, logger, in)
	}
	if len(intents) > 0 {
		logger.Info("rig: re-file intent recovery complete", "intents", len(intents))
	}
	return nil
}

// recoverRefileIntent resolves one intent by observation, checking `from` FIRST.
//
// The order is not cosmetic. Checking `to` first and accepting a match would let
// recovery bind the row to a SIBLING's file: in a flat panel layout the ASIAIR's
// filenames collide by design, and all lights from one camera and binning are
// identical in size, so neither name nor size distinguishes them. Establishing
// that `from` is empty is what makes a file at `to` mean anything — and it is why
// no size or content heuristic is needed here.
func recoverRefileIntent(ctx context.Context, st store.Store, lfs localfs.LocalFS, logger *slog.Logger, in store.RefileIntent) {
	drop := func(msg string) {
		if err := st.ClearRefileIntent(ctx, in.RigID, in.FromPath); err != nil {
			logger.Error("rig: could not clear a resolved re-file intent", "rig", in.RigID, "from", in.FromPath, "error", err)
			return
		}
		logger.Info(msg, "rig", in.RigID, "from", in.FromPath, "to", in.ToPath)
	}

	if _, _, err := lfs.Stat(in.FromPath); err == nil {
		drop("rig: re-file intent resolved; the rename never happened")
		return
	} else if !isNotExist(err) {
		logger.Error("rig: cannot stat the source of an open re-file intent; leaving it open",
			"rig", in.RigID, "from", in.FromPath, "error", err)
		return
	}

	rows, err := st.FramesByDestPath(ctx, in.RigID, in.FromPath)
	if err != nil {
		logger.Error("rig: cannot read the rows of an open re-file intent; leaving it open",
			"rig", in.RigID, "from", in.FromPath, "error", err)
		return
	}
	if len(rows) == 0 {
		// Either the commit landed and only the intent write was lost, or the row was
		// pruned/dismissed while the intent was open. Both drop the intent; the
		// second query only decides which one the operator is told about.
		if at, aerr := st.FramesByDestPath(ctx, in.RigID, in.ToPath); aerr == nil && len(at) > 0 {
			drop("rig: re-file intent resolved; the commit had already landed")
			return
		}
		drop("rig: re-file intent resolved; no row references either path (pruned or dismissed mid-move)")
		return
	}

	if _, _, err := lfs.Stat(in.ToPath); err != nil {
		if isNotExist(err) {
			// The file is at neither path: it is genuinely gone, and normal
			// missing-frame handling applies to the row that still names it.
			drop("rig: re-file intent resolved; the file is at neither path")
			return
		}
		logger.Error("rig: cannot stat the destination of an open re-file intent; leaving it open",
			"rig", in.RigID, "to", in.ToPath, "error", err)
		return
	}

	// The rename landed and the commit was lost: finish it. An intent names one
	// dest_path, not one row, so this commits EVERY row sharing it, never a subset.
	// TolerateNoRows is scoped to recovery and only to recovery — this is a commit
	// that may already have happened, whereas on the live path a zero-row update
	// means a row vanished mid-move and must be reported.
	n, err := st.MoveDestPath(ctx, in.RigID, in.FromPath, in.ToPath, store.TolerateNoRows)
	if err != nil {
		logger.Error("rig: could not finish an interrupted re-file commit; leaving the intent open",
			"rig", in.RigID, "from", in.FromPath, "to", in.ToPath, "error", err)
		return
	}
	logger.Warn("rig: finished an interrupted re-file commit", "rig", in.RigID,
		"from", in.FromPath, "to", in.ToPath, "rows", n)
	drop("rig: re-file intent resolved; the interrupted commit was completed")
}
