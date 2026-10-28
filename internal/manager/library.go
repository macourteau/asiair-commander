package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// workerFor returns the running worker for a rig, or ErrRigNotFound.
func (m *Manager) workerFor(rigID string) (worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[rigID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRigNotFound, rigID)
	}
	return h.worker, nil
}

// worker is the subset of *rig.Worker the out-of-band-deletion commands use.
type worker interface {
	LinkLibrary(ctx context.Context, root string) error
	RescanLibrary() error
	RestoreFrame(ctx context.Context, frameID int64) error
	CancelRestore(ctx context.Context, frameID int64) error
	AcknowledgeMissing(ctx context.Context, frameID int64) error
	AcknowledgeMissingIDs(ctx context.Context, dir string, ids []int64) (int, error)
	ForgetFrame(ctx context.Context, frameID int64) error
	ForgetLeftovers(ctx context.Context, dir string, olderThanDays int, ids []int64) (int, error)
	RetargetFrame(ctx context.Context, frameID int64, newTarget string) (model.RetargetResult, error)
	PreviewRetarget(ctx context.Context, ids []int64, newTarget string) ([]model.RetargetPreview, error)
	PreviewRefile(ctx context.Context, scope string) (model.RefilePreview, error)
	RefileGroup(ctx context.Context, wantDir string, frameIDs []int64) ([]model.RefileResult, model.RefileSummary, error)
}

// LinkLibrary records a destination root as this installation's library. The root
// must be one of the configured Light/Cal roots.
func (m *Manager) LinkLibrary(rigID, root string) error {
	cfg := m.cfg.Load()
	if cfg == nil || (root != cfg.Roots.Light && root != cfg.Roots.Cal) {
		return fmt.Errorf("root %q is not a configured library root", root)
	}
	w, err := m.workerFor(rigID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.LinkLibrary(ctx, root)
}

// RescanLibrary asks a rig to run a library scan on its next cycle.
func (m *Manager) RescanLibrary(rigID string) error {
	w, err := m.workerFor(rigID)
	if err != nil {
		return err
	}
	return w.RescanLibrary()
}

// RestoreFrame flags a missing frame for re-pull from a resident device original.
func (m *Manager) RestoreFrame(rigID string, frameID int64) error {
	w, err := m.workerFor(rigID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.RestoreFrame(ctx, frameID)
}

// CancelRestore clears a pending restore request.
func (m *Manager) CancelRestore(rigID string, frameID int64) error {
	w, err := m.workerFor(rigID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.CancelRestore(ctx, frameID)
}

// AcknowledgeMissing waives the reconcile-settle window for one culled frame,
// arming its device original for deletion on the next pass.
func (m *Manager) AcknowledgeMissing(rigID string, frameID int64) error {
	w, err := m.workerFor(rigID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.AcknowledgeMissing(ctx, frameID)
}

// AcknowledgeMissingIDs waives the settle window for a bound set of culled frames
// under one directory, returning the number of rows actually acknowledged. dir is
// carried only for the worker's audit log line.
func (m *Manager) AcknowledgeMissingIDs(rigID, dir string, ids []int64) (int, error) {
	w, err := m.workerFor(rigID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.AcknowledgeMissingIDs(ctx, dir, ids)
}

// ForgetFrame retires one leftover row: a marked frame whose device original is
// already gone (design library-reconciliation §13.4).
func (m *Manager) ForgetFrame(rigID string, frameID int64) error {
	w, err := m.workerFor(rigID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.ForgetFrame(ctx, frameID)
}

// ForgetLeftovers retires a token-bound set of leftover rows, returning the number
// actually forgotten. dir and olderThanDays are carried only for the worker's
// audit log line.
func (m *Manager) ForgetLeftovers(rigID, dir string, olderThanDays int, ids []int64) (int, error) {
	w, err := m.workerFor(rigID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.ForgetLeftovers(ctx, dir, olderThanDays, ids)
}

// RetargetFrame re-labels one frame to newTarget, moving its archive to the new
// path. One frame is a ~116 MB read+write, so the timeout is generous — the HTTP
// handler streams these one at a time and shows live progress, so a slow frame does
// not stall the whole batch behind a short deadline.
func (m *Manager) RetargetFrame(rigID string, frameID int64, newTarget string) (model.RetargetResult, error) {
	w, err := m.workerFor(rigID)
	if err != nil {
		return model.RetargetResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return w.RetargetFrame(ctx, frameID, newTarget)
}

// PreviewRetarget projects where each frame would move under newTarget, without any
// NAS/device I/O, so the usual short timeout suffices.
func (m *Manager) PreviewRetarget(rigID string, ids []int64, newTarget string) ([]model.RetargetPreview, error) {
	w, err := m.workerFor(rigID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.PreviewRetarget(ctx, ids, newTarget)
}

// refilePreviewTimeout bounds a re-file preview. It is longer than the 10 s the
// retarget preview gets because it is a different shape of work: that one projects
// a hand-picked id list in memory, while this one scans every eligible row of the
// library and issues one ReadDir per directory the migration would vacate — on a
// network mount. It is only a backstop; the operator's own cancellation arrives
// through the request context.
const refilePreviewTimeout = 2 * time.Minute

// refileGroupTimeout sizes a group's deadline against the GROUP rather than a
// frame, since one request carries many: 10 s per frame, floored at 5 minutes and
// capped at 30. That is deliberately generous — a move is a rename, on the order of
// milliseconds — so a timeout here means something is badly wrong. Firing stops the
// group between frames exactly as a cancel does, so it never drives a frame into
// the commit-failure path.
func refileGroupTimeout(frames int) time.Duration {
	d := time.Duration(frames) * 10 * time.Second
	switch {
	case d < 5*time.Minute:
		return 5 * time.Minute
	case d > 30*time.Minute:
		return 30 * time.Minute
	default:
		return d
	}
}

// PreviewRefile projects the whole migration for one rig, grouped old dir → new
// dir. The context derives from the REQUEST, not context.Background(): a cancelled
// request must reach the worker rather than leaving a whole-library scan running
// for nobody.
func (m *Manager) PreviewRefile(ctx context.Context, rigID, scope string) (model.RefilePreview, error) {
	w, err := m.workerFor(rigID)
	if err != nil {
		return model.RefilePreview{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, refilePreviewTimeout)
	defer cancel()
	return w.PreviewRefile(ctx, scope)
}

// RefileGroup moves one selected group's frames to where the current templates say
// they belong, returning one result per file plus the prune summary. Like the
// preview, its context derives from the request, so cancelling the apply stops the
// migration between frames; there is no resume state because re-running the diff
// is idempotent by construction.
func (m *Manager) RefileGroup(ctx context.Context, rigID, toDir string, frameIDs []int64) ([]model.RefileResult, model.RefileSummary, error) {
	w, err := m.workerFor(rigID)
	if err != nil {
		return nil, model.RefileSummary{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, refileGroupTimeout(len(frameIDs)))
	defer cancel()
	return w.RefileGroup(ctx, toDir, frameIDs)
}
