package devfiles

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// Limits and timings. The open budget is sharesBudget's shape (discovery): a
// short base plus three round trips of the site, so a dead local address fails
// in seconds rather than after the worker's 30 s operation budget.
const (
	maxFiles       = 10_000
	slotsPerRig    = 2
	idleTTL        = 2 * time.Minute
	openBudgetBase = 3 * time.Second
)

// retryBackoffs are the waits before the 1st, 2nd and 3rd retry: one attempt
// plus up to three retries.
var retryBackoffs = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// FrameStore is the store surface the service reads; store.Store satisfies it.
// The service never writes the store.
type FrameStore interface {
	GetFrame(ctx context.Context, rigID, share, path string) (*model.Frame, bool, error)
	GetMeta(ctx context.Context, key string) (string, bool, error)

	// ResidentLeads is the lead of every frame of rigID on share still on the
	// device, from which rename derives the roots the store keeps in play
	// (storeKeptRoots).
	ResidentLeads(ctx context.Context, rigID, share string) ([]string, error)
}

// LocalFS is the local-filesystem read the delete count makes of a frame's NAS
// copy, the same seam the harvester's delete pass stats it through, and the
// free-space read upload staging makes of Commander's own data volume;
// localfs.LocalFS satisfies it. The service never writes the NAS.
type LocalFS interface {
	Stat(path string) (size int64, mtime time.Time, err error)
	FreeSpace(root string) (free, total int64, err error)
}

// StatusSource is the hub's snapshot read, an atomic load that shares no lock
// with the hub's writers; hub.StatusHub satisfies it.
type StatusSource interface {
	Snapshot() model.Snapshot
}

// Prober corroborates the device at an endpoint with a live scan_air probe;
// discovery.Scanner satisfies it.
type Prober interface {
	ProbeGUID(ctx context.Context, ep netsite.Endpoint) (guid, cpuID string, ok bool)
}

// DeviceFactory builds a device for an endpoint: device.NewFactory in
// production, the same factory the manager gets.
type DeviceFactory func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device

// Deps are the service's collaborators.
type Deps struct {
	Config    *atomic.Pointer[model.Config]
	Store     FrameStore
	LocalFS   LocalFS
	Status    StatusSource
	Prober    Prober
	Sites     netsite.Sites
	NewDevice DeviceFactory
	Logger    *slog.Logger

	// Upload staging. StagingDir is the directory staged uploads live in,
	// <data-dir>/upload-staging in production; "" refuses every staging
	// request. Now is the clock staging expiry reads; nil is time.Now.
	StagingDir string
	Now        func() time.Time
}

// Service runs the device file browser's operations on the calling request's
// goroutine. Its only locks are its own (the session pool and the operation
// lock); it holds none of them across device I/O.
type Service struct {
	cfg       *atomic.Pointer[model.Config]
	store     FrameStore
	lfs       LocalFS
	status    StatusSource
	prober    Prober
	sites     netsite.Sites
	newDevice DeviceFactory
	log       *slog.Logger

	// Timings, overridden by tests.
	idleTTL  time.Duration
	openBase time.Duration
	backoffs []time.Duration
	maxFiles int
	now      func() time.Time

	mu     sync.Mutex // guards pools and closed; never held across device I/O
	pools  map[string]*rigPool
	closed bool

	op opLock

	staging *stagingStore
}

// New builds a service. A nil logger uses slog.Default.
func New(d Deps) *Service {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		cfg:       d.Config,
		store:     d.Store,
		lfs:       d.LocalFS,
		status:    d.Status,
		prober:    d.Prober,
		sites:     d.Sites,
		newDevice: d.NewDevice,
		log:       logger,
		idleTTL:   idleTTL,
		openBase:  openBudgetBase,
		backoffs:  retryBackoffs,
		maxFiles:  maxFiles,
		now:       now,
		pools:     map[string]*rigPool{},
		staging:   newStagingStore(d.StagingDir),
	}
}

// Close closes every parked session and makes later checkins close theirs.
// Operations in flight finish or fail on their own; new checkouts are refused.
func (s *Service) Close() {
	s.staging.stopSweeper()
	s.mu.Lock()
	s.closed = true
	var idle []*session
	for _, p := range s.pools {
		if p.idle != nil {
			idle = append(idle, p.idle)
			p.idle = nil
		}
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
	}
	s.mu.Unlock()
	for _, c := range idle {
		s.closeSession(c, "shutdown")
	}
}

// rigName is the rig's display name for messages, falling back to its id.
func rigName(profile *model.RigProfile, rigID string) string {
	if profile != nil && profile.Name != "" {
		return profile.Name
	}
	return rigID
}

// nameOf looks up rigID's display name in the live config.
func (s *Service) nameOf(rigID string) string {
	return rigName(s.cfg.Load().Rig(rigID), rigID)
}
