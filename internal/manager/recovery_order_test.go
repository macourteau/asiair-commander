package manager

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// workerStartedMsg is the line startWorkerLocked emits immediately after launching
// a worker goroutine. It is what "a worker has started" means here, because the
// goroutine itself is unobservable from outside the package and m.handles is behind
// the very lock Start holds for the whole of its recovery work.
const workerStartedMsg = "manager: worker started"

// gateFS holds one startup recovery open: its first Stat of gatePath blocks until
// the test releases it. Every intent recovery's first act is a Stat of the path its
// intent names, so this is the seam that lets a test ask what ELSE the manager has
// done while a recovery is still half-finished — which is the only way to assert an
// ordering rather than merely that both things happened.
type gateFS struct {
	localfs.LocalFS
	gatePath string
	entered  chan struct{}
	release  chan struct{}
	arrive   sync.Once
	let      sync.Once
}

func newGateFS(dir, gatePath string) *gateFS {
	return &gateFS{
		LocalFS:  localfs.NewFake(dir),
		gatePath: gatePath,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (g *gateFS) Stat(p string) (int64, time.Time, error) {
	if p == g.gatePath {
		g.arrive.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.LocalFS.Stat(p)
}

func (g *gateFS) letThrough() { g.let.Do(func() { close(g.release) }) }

// recordingHandler collects every log message the manager and its workers emit, so
// a test can ask what had happened by a given instant. It is written from the
// manager goroutine and from each worker's, hence the lock.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) seen(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// startGated runs Start on its own goroutine, waits until the gated recovery is
// inside its first Stat, and returns a function that releases the gate and reports
// Start's error. Both the release and the Stop are registered as cleanups in the
// order that unwinds safely even when the test fails while the gate is held.
//
// It watches for Start returning as well as for the gate, so a change that stops the
// recovery reaching that Stat at all — an earlier error, a recovery that no longer
// reads the intent's path first — is reported as itself rather than hanging the
// whole package until the test binary's timeout.
func startGated(t *testing.T, m *Manager, g *gateFS) func() error {
	t.Helper()
	errc := make(chan error, 1)
	t.Cleanup(m.Stop)
	t.Cleanup(g.letThrough) // registered last, so it runs first and Stop cannot deadlock
	go func() { errc <- m.Start(context.Background()) }()
	select {
	case <-g.entered:
	case err := <-errc:
		t.Fatalf("Start returned before the gated recovery was reached: %v", err)
	}
	return func() error {
		g.letThrough()
		return <-errc
	}
}

// TestStartResolvesSupersedeIntentsBeforeAnyWorkerStarts is the ordering clause of
// design frame-lifecycle AC13 asserted at its call site rather
// than at the function's: recovery must run to COMPLETION before any worker starts.
// An archive copy left under its reserved sibling name is a frame whose canonical
// path is absent, and a worker that reaches it first sees a frame one settle window
// away from authorising the deletion of its device original. The in-flight deferral
// guards are defence in depth behind this ordering, not a substitute for it.
//
// The gate is what makes this an ordering assertion: with recovery held open
// mid-Stat, an implementation that started its workers first would have logged so by
// the time the gate is reached.
func TestStartResolvesSupersedeIntentsBeforeAnyWorkerStarts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	canonical := filepath.Join(dir, "Light_0001.fit")
	aside := filepath.Join(dir, "Light_0001.fit.sha-aside")
	// The crash window the intent describes: step 1 ran and step 2 did not, so the
	// bytes are under the reserved name and the canonical name is free.
	if err := os.WriteFile(aside, []byte("the archive copy set aside"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newGateFS(dir, aside)
	rec := &recordingHandler{}
	m, st := newManagerWithSeams(t, testConfig(t, testRig("a", true)), g, slog.New(rec))
	if err := st.PutSupersedeIntent(ctx, store.SupersedeIntent{
		RigID: "a", DestPath: canonical, AsidePath: aside, FrameID: 1,
		SHA256: "device-revised", Size: 1, Mtime: 1,
		ArchiveSHA256: "archive-revised", ArchiveSize: 1,
		State: model.StateVerified, PulledAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed the open intent: %v", err)
	}

	finish := startGated(t, m, g)
	if rec.seen(workerStartedMsg) {
		t.Fatal("a worker started while a supersede intent was still open; recovery must complete first")
	}
	if err := finish(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The recovery ran to completion, and only then did the workers start.
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("recovery must restore the canonical file before starting workers: %v", err)
	}
	if _, err := os.Stat(aside); err == nil {
		t.Fatal("recovery must leave no copy under the reserved name")
	}
	open, err := st.ListSupersedeIntents(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("the resolved intent must be dropped; got %d open (err %v)", len(open), err)
	}
	if !rec.seen(workerStartedMsg) {
		t.Fatal("the workers must start once recovery is done")
	}
	if got := m.workerIDs(); !eq(got, []string{"a"}) {
		t.Fatalf("workers = %v, want [a]", got)
	}
}

// TestStartResolvesRefileIntentsBeforeAnyWorkerStarts is the same ordering for the
// re-file recovery that runs beside it (design routing-and-filing AC32's
// INV-M2): a row still naming a path the rename already vacated is the same
// settle window away from the same authorisation. Both recoveries are asserted,
// because an ordering proven for the later one alone still passes when the
// earlier one is moved.
func TestStartResolvesRefileIntentsBeforeAnyWorkerStarts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	from := filepath.Join(dir, "Light_0002.fit")
	to := filepath.Join(dir, "moved", "Light_0002.fit")
	// The crash window the intent describes: the rename never happened, so the file
	// is still at the path the row names.
	if err := os.WriteFile(from, []byte("the archive copy that never moved"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newGateFS(dir, from)
	rec := &recordingHandler{}
	m, st := newManagerWithSeams(t, testConfig(t, testRig("a", true)), g, slog.New(rec))
	if err := st.PutRefileIntent(ctx, store.RefileIntent{
		RigID: "a", FromPath: from, ToPath: to, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed the open intent: %v", err)
	}

	finish := startGated(t, m, g)
	if rec.seen(workerStartedMsg) {
		t.Fatal("a worker started while a re-file intent was still open; recovery must complete first")
	}
	if err := finish(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	open, err := st.ListRefileIntents(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("the resolved intent must be dropped; got %d open (err %v)", len(open), err)
	}
	if !rec.seen(workerStartedMsg) {
		t.Fatal("the workers must start once recovery is done")
	}
}
