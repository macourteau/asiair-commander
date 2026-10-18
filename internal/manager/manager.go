package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/imgpreview"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
	"sync/atomic"
)

// ErrRigNotFound is returned by a per-rig command for an id the manager does not
// currently supervise (unknown, unmanaged, or already removed).
var ErrRigNotFound = errors.New("manager: rig not found")

const (
	// maintenanceTick is the base cadence of the maintenance loop: NAS free-space
	// publish plus the gates below. Small enough that the dashboard's NAS figure
	// stays fresh, large enough to add negligible load.
	maintenanceTick = 30 * time.Second
	// checkpointInterval is how often the SQLite WAL is checkpointed from the
	// maintenance loop (design architecture §12 durability).
	checkpointInterval = 5 * time.Minute
	// retentionInterval is how often deleted frames and acked alerts are pruned.
	retentionInterval = 1 * time.Hour
	// retentionAge is how long a deleted frame / acked alert is retained before it
	// becomes eligible for pruning.
	retentionAge = 7 * 24 * time.Hour
	// pruneBatch bounds one prune DELETE so a big first sweep never holds the write
	// connection in one long transaction.
	pruneBatch = 500
	// bytesWindow is the rolling window for the dashboard's "transferred" total.
	bytesWindow = 12 * time.Hour
)

// handle owns one supervised Worker and the cancel that stops its goroutine.
type handle struct {
	worker *rig.Worker
	cancel context.CancelFunc
}

// Manager is the RigManager supervisor (design architecture §3). It owns the
// worker set behind a mutex so that all mutations — from config edits and from
// HTTP-driven operator commands — are serialized through Manager methods; the
// worker map is never touched from a handler goroutine directly. Config is held
// behind an atomic.Pointer read by workers at their cycle boundary; a global
// change is fanned to every surviving worker via rig.ReloadConfig.
type Manager struct {
	cfg       *atomic.Pointer[model.Config]
	adm       *Admission
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device
	resolver  rig.Resolver
	// sites routes each worker's dials and probes to the network its endpoint
	// names; every worker shares it, so a site added or removed is seen by all.
	sites netsite.Sites
	// guard serializes every config write with the web layer's writers; configPath
	// is where the applier saves a migrated bind (design device-identity-and-media
	// §3.4).
	guard      *config.Guard
	configPath string

	lfs       localfs.LocalFS
	hdr       header.Reader
	store     store.Store
	hub       hub.Hub
	notify    notify.Notifier
	previewer imgpreview.Previewer
	// nudge is handed to every worker so a freshly-filed hinted light wakes the
	// Tier-1 solve worker (design features §3.7/§3.8). nil when plate solving is
	// disabled.
	nudge chan<- int64

	siteLoc *time.Location
	metrics *metrics.Metrics
	log     *slog.Logger

	mu           sync.Mutex
	handles      map[string]*handle
	baseCtx      context.Context
	cancel       context.CancelFunc
	started      bool
	globalPaused bool
	// nasFreeClear records that the destination free-space read succeeded since its
	// alert was last resolved, so a healthy NAS resolves once, not every tick. It
	// starts false so the first good read retires an alert a previous process left.
	// Owned by maintenanceLoop.
	nasFreeClear bool
	pausedRigs   map[string]bool // individually-paused rigs (in addition to global)
	wg           sync.WaitGroup
}

// pauseStateKey is the meta key under which the pause state is persisted so it
// survives a restart (a maintenance pause shouldn't silently resume on reboot).
const pauseStateKey = "pause_state"

type pauseState struct {
	Global bool     `json:"global"`
	Rigs   []string `json:"rigs"`
}

// New constructs the supervisor. The dependency set mirrors the Worker's because
// the manager is the thing that constructs Workers; the atomic config pointer is
// shared with every worker so a swap is seen at the next cycle boundary. adm is
// the shared global gate; its capacity (max_concurrent_transfers) is fixed at
// construction and never resized (apply-on-restart, design architecture §3).
func New(
	cfg *atomic.Pointer[model.Config],
	adm *Admission,
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device,
	lfs localfs.LocalFS,
	hdr header.Reader,
	st store.Store,
	h hub.Hub,
	ntf notify.Notifier,
	prev imgpreview.Previewer,
	nudge chan<- int64,
	siteLoc *time.Location,
	logger *slog.Logger,
	resolver rig.Resolver,
	sites netsite.Sites,
	guard *config.Guard,
	configPath string,
	mx *metrics.Metrics,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if siteLoc == nil {
		siteLoc = time.Local
	}
	if guard == nil {
		guard = &config.Guard{}
	}
	return &Manager{
		cfg:        cfg,
		adm:        adm,
		newDevice:  newDevice,
		resolver:   resolver,
		sites:      sites,
		guard:      guard,
		configPath: configPath,
		lfs:        lfs,
		hdr:        hdr,
		store:      st,
		hub:        h,
		notify:     ntf,
		previewer:  prev,
		nudge:      nudge,
		siteLoc:    siteLoc,
		metrics:    mx,
		log:        logger.With("component", "manager"),
		handles:    make(map[string]*handle),
	}
}

// Start gates the workers on the night-counter rebuild (design architecture §7
// startup: night numbering must be authoritative before any frame is filed), then
// launches a worker goroutine for every managed rig and the maintenance loop. The
// passed ctx is the parent of every worker's context; Stop (or ctx cancellation)
// tears them all down. Start is idempotent-guarded: a second call is a no-op
// error.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("manager: already started")
	}

	cfg := m.cfg.Load()
	if cfg == nil {
		m.mu.Unlock()
		return errors.New("manager: nil config")
	}

	// Create the destination folders up front so a fresh install just works
	// instead of failing every free-space read and silently aborting pulls.
	m.ensureRoots(ctx, cfg)

	// Seed the unacked-alert badge from the store. The hub counter is in-memory
	// and only ever updated when an alert is raised or acked, so without this it
	// reads zero after every restart while unacked alerts sit in the database —
	// the badge silently under-reports until the next raise.
	if n, err := m.store.UnackedCount(ctx); err == nil {
		m.hub.SetUnackedAlerts(n)
	} else {
		m.log.Warn("manager: could not seed unacked-alert count", "error", err)
	}

	// Restore the persisted pause state so a rig paused for maintenance stays
	// paused across a restart (each worker is paused as it starts, below).
	m.loadPauseStateLocked(ctx)

	// Resolve any re-file intent a crash left open BEFORE the rebuild: a file the
	// rename already moved under the new tree is exactly the two-tree night-counter
	// collision design routing-and-filing §8.6 describes, and the row still naming
	// the vacated path is one settle window away from authorising the deletion of a
	// device original (INV-M2). A failure here is fatal for the same reason the
	// rebuild is — starting workers over an unresolved move is the failure, not the
	// symptom.
	if err := rig.RecoverRefileIntents(ctx, m.store, m.lfs, m.log); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("manager: re-file intent recovery: %w", err)
	}

	// Resolve any supersede intent a crash left open, on the same terms: an archive
	// copy still under the reserved sibling name of an unfinished four-step claim is
	// a frame whose canonical path is absent, and a row still anchored to bytes the
	// canonical file no longer holds is a critical delete_mismatch the next pass
	// cannot clear. Both are resolved before a worker can observe either.
	if err := rig.RecoverSupersedeIntents(ctx, m.store, m.lfs, m.log); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("manager: supersede intent recovery: %w", err)
	}

	// Rebuild night counters BEFORE any worker can file a frame (design
	// architecture §7). Run it without the lock held would race Start's own map
	// init, so keep it here — no worker exists yet, so nothing contends.
	if err := rig.RebuildNightCounters(ctx, cfg, m.store, m.hdr, m.lfs, m.siteLoc, m.log); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("manager: night-counter rebuild: %w", err)
	}

	m.baseCtx, m.cancel = context.WithCancel(ctx)
	m.started = true

	for i := range cfg.Rigs {
		p := &cfg.Rigs[i]
		if p.Managed {
			m.startWorkerLocked(p.ID)
		}
	}

	m.wg.Add(1)
	go m.maintenanceLoop()

	m.mu.Unlock()
	m.log.Info("manager: started", "workers", len(m.handles))
	return nil
}

// startWorkerLocked constructs and launches a worker for rigID. The caller holds
// m.mu. A worker already present is left untouched.
func (m *Manager) startWorkerLocked(rigID string) {
	if _, ok := m.handles[rigID]; ok {
		return
	}
	wctx, cancel := context.WithCancel(m.baseCtx)
	w := rig.NewWorker(
		rigID, m.cfg, m.newDevice, m.resolver, m.sites, m.lfs, m.hdr, m.store, m.hub, m.notify, m.adm,
		m.previewer, m.nudge, m.siteLoc, time.Now, m.metrics, m.log,
	)
	m.handles[rigID] = &handle{worker: w, cancel: cancel}
	if m.globalPaused || m.pausedRigs[rigID] {
		w.Send(rig.Pause) // globally paused, or this rig was paused before the restart
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		w.Run(wctx)
	}()
	m.log.Info("manager: worker started", "rig", rigID)
}

// PauseAll pauses every managed rig (the global "I'm doing something unusual"
// switch) and remembers the state so a rig added while paused also starts paused.
func (m *Manager) PauseAll() {
	m.mu.Lock()
	m.globalPaused = true
	for _, h := range m.handles {
		h.worker.Send(rig.Pause)
	}
	m.mu.Unlock()
	m.persistPauseState()
	m.log.Info("manager: paused all rigs")
}

// ResumeAll clears a global pause and resumes every managed rig.
func (m *Manager) ResumeAll() {
	m.mu.Lock()
	m.globalPaused = false
	m.pausedRigs = nil // a fleet-wide resume clears individual pauses too
	for _, h := range m.handles {
		h.worker.Send(rig.Resume)
	}
	m.mu.Unlock()
	m.persistPauseState()
	m.log.Info("manager: resumed all rigs")
}

// loadPauseStateLocked restores the persisted pause state. The caller holds m.mu
// (it runs inside Start before any worker exists).
func (m *Manager) loadPauseStateLocked(ctx context.Context) {
	v, ok, err := m.store.GetMeta(ctx, pauseStateKey)
	if err != nil || !ok {
		return
	}
	var st pauseState
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return
	}
	m.globalPaused = st.Global
	if len(st.Rigs) > 0 {
		m.pausedRigs = make(map[string]bool, len(st.Rigs))
		for _, id := range st.Rigs {
			m.pausedRigs[id] = true
		}
	}
}

// persistPauseState writes the current pause state so it survives a restart. It
// snapshots under the lock, then writes outside it so DB I/O never blocks the map.
func (m *Manager) persistPauseState() {
	m.mu.Lock()
	st := pauseState{Global: m.globalPaused}
	for id := range m.pausedRigs {
		st.Rigs = append(st.Rigs, id)
	}
	ctx := m.baseCtx
	m.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.store.SetMeta(ctx, pauseStateKey, string(b)); err != nil {
		m.log.Warn("manager: could not persist pause state", "error", err)
	}
}

// stopWorkerLocked cancels and removes rigID's worker. The caller holds m.mu. The
// goroutine drains itself on cancellation; the WaitGroup accounts for it.
func (m *Manager) stopWorkerLocked(rigID string) {
	h, ok := m.handles[rigID]
	if !ok {
		return
	}
	h.cancel()
	delete(m.handles, rigID)
	// A rig leaving the managed set (deleted or set unmanaged) should leave no
	// residual metric series. This is best-effort: the cancelled worker may still be
	// mid-cycle and publish/record once more before it observes cancellation, so a
	// rig deleted mid-transfer can leave one cycle's series until the next removal or
	// a restart. A fully race-free purge would require joining this worker's
	// goroutine here, which the shared WaitGroup does not support per rig.
	m.hub.RemoveRig(rigID)
	m.metrics.ForgetRig(rigID)
	m.log.Info("manager: worker stopped", "rig", rigID)
}

// ApplyConfig swaps the global config pointer and reconciles the worker set to it
// (design architecture §3 fan-out). max_concurrent_transfers is deliberately NOT
// applied: the admission semaphore is fixed at construction (apply-on-restart),
// so the new value is persisted and takes effect only at the next service start.
func (m *Manager) ApplyConfig(newCfg *model.Config) {
	m.cfg.Store(newCfg)
	m.Reconcile()
}

// Reconcile brings the live worker set in line with the current config pointer:
// start workers for newly-managed rigs, cancel+remove workers for rigs that became
// unmanaged or were deleted, and signal every survivor to re-read the global
// config at its next cycle boundary. Safe to call whether or not the config
// pointer was swapped through ApplyConfig.
func (m *Manager) Reconcile() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	ctx := m.baseCtx
	m.mu.Unlock()

	// A config change may have introduced new destination roots (e.g. the operator
	// just finished the setup wizard) — make sure they exist. Do this WITHOUT the
	// lock held: it touches the (possibly slow or detached) destination filesystem,
	// and must not block Pause/Resume/RunNow, which contend on m.mu.
	m.ensureRoots(ctx, m.cfg.Load())

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	cfg := m.cfg.Load()
	desired := make(map[string]struct{})
	if cfg != nil {
		for i := range cfg.Rigs {
			if cfg.Rigs[i].Managed {
				desired[cfg.Rigs[i].ID] = struct{}{}
			}
		}
	}

	// Remove workers no longer wanted.
	for id := range m.handles {
		if _, want := desired[id]; !want {
			m.stopWorkerLocked(id)
		}
	}
	// Add workers newly wanted; signal survivors to reload.
	for id := range desired {
		if _, ok := m.handles[id]; ok {
			m.handles[id].worker.Send(rig.ReloadConfig)
		} else {
			m.startWorkerLocked(id)
		}
	}
}

// send delivers a command to rigID's worker, returning ErrRigNotFound if it is not
// supervised. Send itself is non-blocking, so the call returns immediately.
func (m *Manager) send(rigID string, c rig.Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[rigID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrRigNotFound, rigID)
	}
	h.worker.Send(c)
	return nil
}

// Pause halts further pulls on rigID (the worker keeps ticking). The paused state
// is persisted so it survives a restart.
func (m *Manager) Pause(rigID string) error {
	m.mu.Lock()
	h, ok := m.handles[rigID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrRigNotFound, rigID)
	}
	if m.pausedRigs == nil {
		m.pausedRigs = make(map[string]bool)
	}
	m.pausedRigs[rigID] = true
	h.worker.Send(rig.Pause)
	m.mu.Unlock()
	m.persistPauseState()
	return nil
}

// Resume clears a pause on rigID (persisted).
func (m *Manager) Resume(rigID string) error {
	m.mu.Lock()
	h, ok := m.handles[rigID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrRigNotFound, rigID)
	}
	delete(m.pausedRigs, rigID)
	h.worker.Send(rig.Resume)
	m.mu.Unlock()
	m.persistPauseState()
	return nil
}

// RunNow forces an immediate cycle on rigID; on a paused rig it runs until idle.
func (m *Manager) RunNow(rigID string) error { return m.send(rigID, rig.RunNow) }

// StopRun ends rigID's run until idle, leaving it paused. Nothing is persisted:
// the pause the run ran under already is, and a run never outlives a restart.
func (m *Manager) StopRun(rigID string) error { return m.send(rigID, rig.StopRun) }

// WakeRig reports that rigID's device answered a discovery scan_air round; it
// satisfies discovery.RigWaker, which the composition root injects into the
// Scanner. The manager only routes: the worker is the goroutine that knows
// whether the rig is offline, paused, or inside its wake backoff, so it decides
// whether the answer is worth a probe. An id we do not supervise is not an error
// — unmanaged and removed devices answer every round — so this is silent and
// non-blocking, unlike the operator commands above.
func (m *Manager) WakeRig(rigID string) {
	m.mu.Lock()
	h, ok := m.handles[rigID]
	m.mu.Unlock()
	if !ok {
		return
	}
	h.worker.WakeFromDiscovery()
}

// WakeForRemote reports that the remote rigID is reached through changed state;
// the remote supervisor calls it for every rig on that remote's site. Like
// WakeRig it only routes, and an id we do not supervise is silently ignored.
func (m *Manager) WakeForRemote(rigID string) {
	m.mu.Lock()
	h, ok := m.handles[rigID]
	m.mu.Unlock()
	if !ok {
		return
	}
	h.worker.WakeForRemote()
}

// Reclassify re-runs classification over rigID's unclassified frames on the worker
// goroutine so the re-filing never races live filing (design routing-and-filing
// §2).
func (m *Manager) Reclassify(rigID string) error { return m.send(rigID, rig.Reclassify) }

// ResetIdentity clears rigID's identity marker from every watched share so a stale
// marker (a previous install, or a re-purposed device) stops refusing pulls; the
// next cycle re-binds to the current profile. An explicit operator override
// (design device-identity-and-media §4).
func (m *Manager) ResetIdentity(rigID string) error { return m.send(rigID, rig.ResetIdentity) }

// DiscardFrame dismisses one permanently-stuck unclassified frame on rigID's
// worker goroutine and returns the outcome synchronously (design
// frame-lifecycle §13). It returns ErrRigNotFound for an unsupervised rig,
// and passes through the worker's rig.ErrFrameNotFound / rig.ErrNotDismissable
// unchanged. The 10 s timeout bounds this caller so a busy worker fails the HTTP
// request rather than blocking it indefinitely.
func (m *Manager) DiscardFrame(rigID string, frameID int64) error {
	m.mu.Lock()
	h, ok := m.handles[rigID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrRigNotFound, rigID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.worker.DiscardFrame(ctx, frameID)
}

// Stop cancels every worker and the maintenance loop and waits for all of them to
// return. It is safe to call once; a manager that never started is a no-op.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	m.cancel() // cancels baseCtx: every worker + the maintenance loop.
	m.handles = make(map[string]*handle)
	m.mu.Unlock()

	m.wg.Wait()
	m.log.Info("manager: stopped")
}

// maintenanceLoop is the home for global periodic work so worker cycles never
// block on it (design architecture §3, §12): publish NAS free space, prune
// retained rows, and checkpoint the store. It ticks at maintenanceTick and gates
// the heavier work by its own interval.
func (m *Manager) maintenanceLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(maintenanceTick)
	defer ticker.Stop()

	var lastRetention, lastCheckpoint time.Time
	m.publishNASFree()
	m.publishBytesLast12h(time.Now())

	for {
		select {
		case <-m.baseCtx.Done():
			return
		case now := <-ticker.C:
			m.publishNASFree()
			m.publishBytesLast12h(now)
			m.applyPendingBinds(m.baseCtx)
			if now.Sub(lastCheckpoint) >= checkpointInterval {
				lastCheckpoint = now
				if err := m.store.Checkpoint(m.baseCtx); err != nil {
					m.log.Warn("manager: checkpoint failed", "error", err)
				}
			}
			if now.Sub(lastRetention) >= retentionInterval {
				lastRetention = now
				m.runRetention(now)
			}
		}
	}
}

// applyPendingBinds is the manager side of the host-bound → guid migration
// (design device-identity-and-media §3.4). For each managed rig it reads
// pendingguid:<rigID>; a valid bind on a still host-bound rig is applied through
// the config guard; a key found on an already-guid-bound rig (a late worker
// write after a bind) is cleared. This is the ONLY config writer the manager
// runs, and it never holds m.mu when calling guard.Do (lock order is always
// configWriteMu → m.mu).
func (m *Manager) applyPendingBinds(ctx context.Context) {
	cfg := m.cfg.Load()
	if cfg == nil {
		return
	}
	for i := range cfg.Rigs {
		p := &cfg.Rigs[i]
		if !p.Managed {
			continue
		}
		key := rig.PendingGUIDKey(p.ID)
		val, ok, err := m.store.GetMeta(ctx, key)
		if err != nil || !ok || val == "" {
			continue
		}
		guid, cpuID, parseOK := parsePendingGUID(val)
		if !parseOK {
			// Garbled value: nothing bindable — clear it.
			m.clearPending(ctx, key)
			continue
		}
		if p.GUID != "" {
			// Already bound; drop the late worker write.
			m.clearPending(ctx, key)
			continue
		}
		m.bindPendingGUID(ctx, p.ID, guid, cpuID, key)
	}
}

// errGUIDTaken signals another managed profile already holds the guid a pending
// bind names (a clone or double-adopt): skip that one bind.
var errGUIDTaken = errors.New("manager: guid already bound by another profile")

// bindPendingGUID applies one pending bind through the config guard: deep-copy
// the live config, set GUID/CPUID, clear Host, validate with a nil
// VolumeResolver, save, apply, and store — then clear the pending key. A
// guid-uniqueness collision skips+clears+alerts without touching other rigs; a
// transient validation/save error leaves the key for a later retry.
func (m *Manager) bindPendingGUID(ctx context.Context, rigID, guid, cpuID, key string) {
	err := m.guard.Do(func() error {
		cur := m.cfg.Load()
		if cur == nil {
			return errors.New("nil config")
		}
		for i := range cur.Rigs {
			if cur.Rigs[i].ID != rigID && cur.Rigs[i].Managed && cur.Rigs[i].GUID == guid {
				return errGUIDTaken
			}
		}
		next, err := deepCopyConfig(cur)
		if err != nil {
			return err
		}
		found := false
		for i := range next.Rigs {
			if next.Rigs[i].ID == rigID {
				next.Rigs[i].GUID = guid
				next.Rigs[i].CPUID = cpuID
				next.Rigs[i].Host = ""
				found = true
				break
			}
		}
		if !found {
			return nil // rig gone; nothing to bind, clear the key below
		}
		if problems := config.Validate(next, nil); config.HasErrors(problems) {
			return fmt.Errorf("migrated config invalid: %v", problems)
		}
		if err := config.Save(m.configPath, next); err != nil {
			return err
		}
		m.ApplyConfig(next)
		m.log.Info("manager: bound rig to device guid", "rig", rigID, "guid", guid)
		return nil
	})

	switch {
	case errors.Is(err, errGUIDTaken):
		m.clearPending(ctx, key)
		m.notify.Raise(ctx, model.Alert{
			RigID:    rigID,
			Severity: model.SeverityWarn,
			Category: model.AlertConfig,
			Message:  "another rig already manages this device (same guid); this rig was not bound — remove the duplicate profile",
		})
	case err != nil:
		m.log.Warn("manager: could not apply pending guid bind; will retry", "rig", rigID, "error", err)
		// Leave the pending key for a later tick.
	default:
		m.clearPending(ctx, key)
	}
}

// clearPending clears a pending-guid key (the store has no delete; an empty
// value reads back as "nothing pending").
func (m *Manager) clearPending(ctx context.Context, key string) {
	if err := m.store.SetMeta(ctx, key, ""); err != nil {
		m.log.Warn("manager: could not clear pending guid key", "key", key, "error", err)
	}
}

// parsePendingGUID splits a "guid,cpuId" pending value. The guid must be
// non-empty; the cpuId may be empty.
func parsePendingGUID(v string) (guid, cpuID string, ok bool) {
	guid, cpuID, _ = strings.Cut(v, ",")
	if guid == "" {
		return "", "", false
	}
	return guid, cpuID, true
}

// deepCopyConfig clones a config via a JSON round-trip so the applier mutates its
// own copy, never the published pointer in place (mirrors web's deepCopyConfig).
func deepCopyConfig(c *model.Config) (*model.Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	var out model.Config
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &out, nil
}

// publishNASFree reads the destination volume's free space (via the light root,
// which shares its volume with staging/cal) and pushes it into the status hub.
func (m *Manager) publishNASFree() {
	cfg := m.cfg.Load()
	// Skip on a pristine first run: the default roots are placeholders and there is
	// nothing to pull yet, so a free-space failure there is noise, not a problem.
	if cfg == nil || cfg.Roots.Light == "" || cfg.PasswordHash == "" {
		return
	}
	free, _, err := m.lfs.FreeSpace(cfg.Roots.Light)
	if err != nil {
		m.log.Warn("manager: NAS free-space read failed", "root", cfg.Roots.Light, "error", err)
		// Surface it — otherwise the dashboard just shows "0 B free" with no clue
		// why, and pulls abort silently. Standing, so the store keeps this to one
		// alert per condition across ticks and an acknowledgement holds until a
		// read succeeds (design architecture §10).
		m.notify.Raise(m.baseCtx, model.Alert{
			Severity: model.SeverityWarn,
			Category: model.AlertConfig,
			Message: fmt.Sprintf(nasFreeAlertPrefix+"%q: %v — does the folder exist and is the drive attached?",
				cfg.Roots.Light, err),
			Standing: true,
		})
		m.nasFreeClear = false
		return
	}
	m.hub.SetNASFree(free)
	if !m.nasFreeClear {
		m.resolveNASFreeAlert()
		m.nasFreeClear = true
	}
}

// nasFreeAlertPrefix starts the free-space-read alert's message, which is how it
// is found among the fleet-wide config alerts to resolve it.
const nasFreeAlertPrefix = "Can't read free space for the destination folder "

// resolveNASFreeAlert resolves a standing free-space-read alert once a read
// succeeds, and refreshes the badge.
func (m *Manager) resolveNASFreeAlert() {
	n, err := m.store.ResolveAlerts(m.baseCtx, "", model.AlertConfig, nasFreeAlertPrefix, time.Now().Unix())
	if err != nil {
		m.log.Warn("manager: could not resolve the NAS free-space alert", "error", err)
		return
	}
	if n == 0 {
		return
	}
	m.log.Info("manager: NAS free-space read recovered; resolved its alert", "count", n)
	if c, err := m.store.UnackedCount(m.baseCtx); err == nil {
		m.hub.SetUnackedAlerts(c)
	}
}

// publishBytesLast12h sums the bytes copied to the NAS across the fleet in the
// trailing bytesWindow and pushes the total into the status hub for the
// dashboard header.
func (m *Manager) publishBytesLast12h(now time.Time) {
	since := now.Add(-bytesWindow).Unix()
	bytes, err := m.store.BytesPulledSince(m.baseCtx, since)
	if err != nil {
		m.log.Warn("manager: 12h byte total read failed", "error", err)
		return
	}
	m.hub.SetBytesLast12h(bytes)
}

// ensureRoots creates the configured destination folders (and any missing
// parents) so a fresh install — or a root the operator just entered — works
// instead of failing every free-space read and aborting pulls. It runs only for a
// configured system (a password is set); the pristine first-run defaults are
// placeholders replaced during setup. A folder that can't be created raises a
// visible alert so the cause is obvious rather than buried in a log.
func (m *Manager) ensureRoots(ctx context.Context, cfg *model.Config) {
	if cfg == nil || cfg.PasswordHash == "" {
		return
	}
	for _, r := range []struct{ name, path string }{
		{"light frames", cfg.Roots.Light},
		{"calibration frames", cfg.Roots.Cal},
		{"staging", cfg.Roots.Staging},
		{"logs", cfg.Roots.Logs},
	} {
		if strings.TrimSpace(r.path) == "" {
			continue
		}
		if err := m.lfs.MkdirAll(r.path); err != nil {
			m.log.Error("manager: could not create destination folder", "which", r.name, "path", r.path, "error", err)
			m.notify.Raise(ctx, model.Alert{
				Severity: model.SeverityWarn,
				Category: model.AlertConfig,
				Message:  fmt.Sprintf("Couldn't create the %s folder %q: %v — check the path and that the drive is attached.", r.name, r.path, err),
			})
			continue
		}
		m.log.Info("manager: destination folder ready", "which", r.name, "path", r.path)
	}
}

// runRetention prunes deleted frames and acked alerts older than retentionAge in
// bounded batches. Failures are logged, not fatal.
func (m *Manager) runRetention(now time.Time) {
	cutoff := now.Add(-retentionAge).Unix()
	if n, err := m.store.PruneDeleted(m.baseCtx, cutoff, pruneBatch); err != nil {
		m.log.Warn("manager: prune deleted frames failed", "error", err)
	} else if n > 0 {
		m.log.Info("manager: pruned deleted frames", "count", n)
	}
	if n, err := m.store.PruneAckedAlerts(m.baseCtx, cutoff, pruneBatch); err != nil {
		m.log.Warn("manager: prune acked alerts failed", "error", err)
	} else if n > 0 {
		m.log.Info("manager: pruned acked alerts", "count", n)
	}
}
