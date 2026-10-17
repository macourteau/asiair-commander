package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// fakeStore is a minimal hand-rolled stand-in for the two store.Store methods
// Raise uses. It is not the real sqlite store — unit tests here must not
// depend on it.
type fakeStore struct {
	mu           sync.Mutex
	raiseErr     error
	unackedErr   error
	unackedCount int
	raised       []model.Alert
	nextID       int64
}

func (s *fakeStore) RaiseAlert(_ context.Context, a model.Alert) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.raiseErr != nil {
		return 0, s.raiseErr
	}
	s.nextID++
	s.raised = append(s.raised, a)
	return s.nextID, nil
}

func (s *fakeStore) UnackedCount(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unackedErr != nil {
		return 0, s.unackedErr
	}
	return s.unackedCount, nil
}

// fakeHub is a minimal hand-rolled stand-in for hub.Hub. It records Emit and
// SetUnackedAlerts calls for assertions.
type fakeHub struct {
	mu            sync.Mutex
	emitted       []hub.Event
	unackedAlerts []int
}

func (h *fakeHub) PublishRig(model.RigStatus)                                         {}
func (h *fakeHub) RemoveRig(string)                                                   {}
func (h *fakeHub) SetRequested(string, model.RequestedState, ...model.RequestedState) {}
func (h *fakeHub) SetNASFree(int64)                                                   {}
func (h *fakeHub) SetBytesLast12h(int64)                                              {}
func (h *fakeHub) SetSolverStatus(model.ProvisionStatus)                              {}
func (h *fakeHub) SetRemotes([]model.RemoteStatus)                                    {}
func (h *fakeHub) SetRelay(*model.RelayStatus)                                        {}

func (h *fakeHub) SetUnackedAlerts(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unackedAlerts = append(h.unackedAlerts, n)
}

func (h *fakeHub) Snapshot() model.Snapshot { return model.Snapshot{} }

func (h *fakeHub) Emit(evt hub.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emitted = append(h.emitted, evt)
}

func (h *fakeHub) Subscribe() (int64, <-chan model.Snapshot, <-chan hub.Event, func()) {
	return 0, nil, nil, func() {}
}

var _ hub.Hub = (*fakeHub)(nil)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRaisePersistsEmitsAndRefreshesUnackedCount(t *testing.T) {
	fs := &fakeStore{unackedCount: 3}
	fh := &fakeHub{}
	n := &notifier{store: fs, hub: fh, log: discardLogger()}

	a := model.Alert{RigID: "rig-1", Severity: model.SeverityWarn, Category: model.AlertTransferFailed, Message: "boom"}
	n.Raise(context.Background(), a)

	if len(fs.raised) != 1 {
		t.Fatalf("store.RaiseAlert calls = %d, want 1", len(fs.raised))
	}
	if fs.raised[0].RigID != "rig-1" || fs.raised[0].Message != "boom" {
		t.Fatalf("persisted alert = %+v, want rig-1/boom", fs.raised[0])
	}

	if len(fh.emitted) != 1 {
		t.Fatalf("hub.Emit calls = %d, want 1", len(fh.emitted))
	}
	evt := fh.emitted[0]
	if evt.Name != "alert.raised" {
		t.Fatalf("event name = %q, want alert.raised", evt.Name)
	}
	got, ok := evt.Data.(model.Alert)
	if !ok {
		t.Fatalf("event data type = %T, want model.Alert", evt.Data)
	}
	if got.ID != 1 {
		t.Fatalf("event alert ID = %d, want 1 (from RaiseAlert's returned id)", got.ID)
	}
	if got.RigID != "rig-1" || got.Message != "boom" {
		t.Fatalf("event alert = %+v, want rig-1/boom", got)
	}

	if len(fh.unackedAlerts) != 1 || fh.unackedAlerts[0] != 3 {
		t.Fatalf("hub.SetUnackedAlerts calls = %v, want [3]", fh.unackedAlerts)
	}
}

func TestRaiseStoreErrorIsSwallowed(t *testing.T) {
	fs := &fakeStore{raiseErr: errors.New("disk full")}
	fh := &fakeHub{}
	n := &notifier{store: fs, hub: fh, log: discardLogger()}

	// Must not panic.
	n.Raise(context.Background(), model.Alert{RigID: "rig-1", Category: model.AlertVerifyMismatch, Message: "x"})

	if len(fh.emitted) != 0 {
		t.Fatalf("hub.Emit calls = %d, want 0 (store error should short-circuit before emit)", len(fh.emitted))
	}
	if len(fh.unackedAlerts) != 0 {
		t.Fatalf("hub.SetUnackedAlerts calls = %d, want 0", len(fh.unackedAlerts))
	}
}

func TestRaiseUnackedCountErrorIsSwallowed(t *testing.T) {
	fs := &fakeStore{unackedErr: errors.New("db locked")}
	fh := &fakeHub{}
	n := &notifier{store: fs, hub: fh, log: discardLogger()}

	// Must not panic; the alert itself still persists and emits.
	n.Raise(context.Background(), model.Alert{RigID: "rig-1", Category: model.AlertBacklog, Message: "y"})

	if len(fs.raised) != 1 {
		t.Fatalf("store.RaiseAlert calls = %d, want 1", len(fs.raised))
	}
	if len(fh.emitted) != 1 {
		t.Fatalf("hub.Emit calls = %d, want 1", len(fh.emitted))
	}
	if len(fh.unackedAlerts) != 0 {
		t.Fatalf("hub.SetUnackedAlerts calls = %d, want 0 (UnackedCount errored)", len(fh.unackedAlerts))
	}
}

func TestRaiseDefaultsCreatedAt(t *testing.T) {
	fs := &fakeStore{}
	fh := &fakeHub{}
	n := &notifier{store: fs, hub: fh, log: discardLogger()}

	before := time.Now().Unix()
	n.Raise(context.Background(), model.Alert{RigID: "rig-1", Category: model.AlertConfig, Message: "z"})
	after := time.Now().Unix()

	if len(fs.raised) != 1 {
		t.Fatalf("store.RaiseAlert calls = %d, want 1", len(fs.raised))
	}
	got := fs.raised[0].CreatedAt
	if got < before || got > after {
		t.Fatalf("CreatedAt = %d, want within [%d, %d]", got, before, after)
	}
}

func TestRaisePreservesExplicitCreatedAt(t *testing.T) {
	fs := &fakeStore{}
	fh := &fakeHub{}
	n := &notifier{store: fs, hub: fh, log: discardLogger()}

	const explicit = int64(1700000000)
	n.Raise(context.Background(), model.Alert{RigID: "rig-1", Category: model.AlertConfig, Message: "z", CreatedAt: explicit})

	if got := fs.raised[0].CreatedAt; got != explicit {
		t.Fatalf("CreatedAt = %d, want unchanged %d", got, explicit)
	}
}

// fullFakeStore satisfies the full store.Store interface (required by New's
// signature) by embedding a nil store.Store for the methods Raise never
// calls, and delegating the two methods it does call to a fakeStore. Calling
// any other method would nil-panic, which is fine: these tests never do.
type fullFakeStore struct {
	store.Store
	inner *fakeStore
}

func (f fullFakeStore) RaiseAlert(ctx context.Context, a model.Alert) (int64, error) {
	return f.inner.RaiseAlert(ctx, a)
}

func (f fullFakeStore) UnackedCount(ctx context.Context) (int, error) {
	return f.inner.UnackedCount(ctx)
}

func TestNewDefaultsNilLogger(t *testing.T) {
	fs := &fakeStore{}
	fh := &fakeHub{}
	// Exercise the real constructor (not the notifier struct literal used by
	// the other tests) to confirm a nil logger doesn't panic and New wires
	// the store/hub through correctly.
	notif := New(fullFakeStore{inner: fs}, fh, nil, nil)
	notif.Raise(context.Background(), model.Alert{RigID: "rig-1", Category: model.AlertConfig, Message: "nil-logger"})

	if len(fs.raised) != 1 {
		t.Fatalf("store.RaiseAlert calls = %d, want 1", len(fs.raised))
	}
	if len(fh.emitted) != 1 {
		t.Fatalf("hub.Emit calls = %d, want 1", len(fh.emitted))
	}
}

func TestFakeNotifierRecordsAlerts(t *testing.T) {
	f := &Fake{}
	a1 := model.Alert{RigID: "rig-1", Category: model.AlertUnreachable, Message: "first"}
	a2 := model.Alert{RigID: "rig-2", Category: model.AlertNASDiskLow, Message: "second"}

	f.Raise(context.Background(), a1)
	f.Raise(context.Background(), a2)

	got := f.Raised()
	if len(got) != 2 {
		t.Fatalf("Raised() len = %d, want 2", len(got))
	}
	if got[0].Message != "first" || got[1].Message != "second" {
		t.Fatalf("Raised() = %+v, want [first, second] in order", got)
	}
}

func TestFakeNotifierConcurrentRaise(t *testing.T) {
	f := &Fake{}
	var wg sync.WaitGroup
	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			f.Raise(context.Background(), model.Alert{RigID: "rig-1", Category: model.AlertBacklog, Message: "concurrent"})
		}(i)
	}
	wg.Wait()

	if got := len(f.Raised()); got != n {
		t.Fatalf("Raised() len = %d, want %d", got, n)
	}
}
