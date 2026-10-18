// Package rig implements the frame lifecycle — the heart of the app and the
// carrier of the "never lose a frame" guarantee (design frame-lifecycle). One
// managed rig is driven by exactly one Worker goroutine, a select loop that on
// each fixed-delay tick runs a full cycle: probe, connect, verify identity, list,
// decide-to-pull, pull + stream-hash + verify-at-rest, classify, route,
// atomically file, and — in a separate short session — run the three-way
// delete-pass integrity check before removing a proven original from the device.
//
// Per-device seriality is structural: because there is one goroutine per device,
// two SMB sessions against one rig cannot overlap. Parallelism across rigs is
// bounded only by the global transfer gate the Worker reaches through the
// Admission seam (the manager owns the actual semaphore + disk reserve).
//
// Every SMB and local-FS boundary is an interface so the whole lifecycle tests
// against in-memory fakes; the data-loss paths (verify-at-rest, the delete-pass
// three-way check, the exclusive-rename disambiguation) are proven directly in
// the package tests.
package rig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/control"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/imgpreview"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	// mtimeTol is the ±2s FAT-granularity mtime tolerance used for every
	// device-timestamp comparison (decide-to-pull, re-stat, settle, pre-delete).
	mtimeTol = 2 * time.Second
	// liveCounterRefresh throttles the live re-query of the transferred-tonight and
	// resident-frame counts during a pull cycle, so a hundreds-of-files burst does
	// not spam the COUNT queries while the counters still climb visibly.
	liveCounterRefresh = 2 * time.Second
)

// smbTimeout is the SMB budget handed to the device factory for a session on
// site: it bounds the dial and each SMB operation, and is a streaming read's
// stall window (deadlineReader). netsite.SMBBudget is the site's stall window
// (30 s on the local site) plus 2 round-trip margins: the operation's request
// and reply, and one retransmission. Every device read is one copy buffer, so
// under a rate cap it also allows for one buffer's read at the capped rate, or a
// low cap would trip the stall window on a transfer still moving. On the local
// site, latency 0 and never capped, it is 30 s exactly.
func smbTimeout(site netsite.Site) time.Duration {
	return netsite.SMBBudget(site) + netsite.CappedReadAllowance(site, header.CopyBufSize)
}

// SMBTimeout is smbTimeout for callers outside the worker, so a session the
// device file browser opens gets the same dial, operation and stall budget.
func SMBTimeout(site netsite.Site) time.Duration { return smbTimeout(site) }

// Resolver maps a rig's durable identity to the device's current endpoint and
// corroborates a guid against the device actually at an endpoint (design
// device-identity-and-media §4). discovery.Scanner satisfies it. Resolve does
// no network I/O (a registry snapshot); ProbeGUID sends a live unicast scan_air
// to the endpoint, through the endpoint's site.
type Resolver interface {
	Resolve(guid, cpuID string) (ep netsite.Endpoint, online bool)
	ProbeGUID(ctx context.Context, ep netsite.Endpoint) (guid, cpuID string, ok bool)
}

// Admission is the transfer gate (design architecture §3 concurrency + design
// architecture §7 disk_reserve + in-flight reserve). Acquire blocks honoring ctx
// until req may start and the NAS volume has room for projectedSize beyond
// disk_reserve; the returned release is called exactly once at the frame's
// TERMINAL outcome (filed or aborted), never at end-of-pull. Which slot req
// waits for is the implementation's policy. The manager implements it; the
// Worker only depends on it, so it is fully testable.
type Admission interface {
	Acquire(ctx context.Context, req Requester, projectedSize int64) (release func(), err error)
}

// Requester is who asks Admission for a slot: the rig, and the site whose
// network the work moves bytes over. A pull, delete or restore runs over the
// cycle's site; work that touches only the NAS names the local site, since that
// is the network it runs on.
type Requester struct {
	Rig  string
	Site netsite.ID
}

// ErrDestinationUnavailable is the sentinel an Admission returns when the
// destination volume cannot be statted at all — most often a NAS mount that is not
// ready yet (a bind mount at container start, a dropped network share). It is
// distinct from a full volume: the destination is unreadable, not out of room, so
// retrying the next frame is pointless and every pending frame would fail
// identically. The worker treats it as a CYCLE-level condition — abort the
// remaining frames, log once, raise one deduped alert — rather than a per-frame
// error, so a stalled mount cannot flood the log with one warning per queued frame.
//
// It lives here, beside the interface it belongs to, rather than in the manager
// package that implements it: the worker must errors.Is against it, and rig cannot
// import manager (manager imports rig).
var ErrDestinationUnavailable = errors.New("destination volume unreadable; pulls paused")

// ErrDiskReserve is returned by an Admission when the destination lacks room for the
// projected pull beyond disk_reserve_bytes — a soft skip, retried next cycle. Like
// ErrDestinationUnavailable it lives here (not in manager) so the worker can
// errors.Is against it to distinguish a headroom skip from a ctx-cancel abort, which
// rig cannot do while manager owns the sentinel (manager imports rig, not vice versa).
var ErrDiskReserve = errors.New("manager: destination free space below disk reserve; pull skipped")

// Command is a control message the manager drives onto a Worker's buffered
// command channel (design architecture §3). Commands are drained between files
// inside a cycle so pause/reload take effect within one file's time, never a
// whole cycle.
type Command int

const (
	// Pause stops further pulls; the worker keeps ticking but runs no cycle work.
	Pause Command = iota
	// Resume clears a pause.
	Resume
	// RunNow forces an immediate cycle. On a paused rig it runs until idle
	// instead: the rig stays paused but works every cycle until nothing more
	// needs doing, then goes back to being simply paused.
	RunNow
	// Reclassify re-runs classification over this rig's unclassified frames and
	// re-files any that now resolve — dispatched onto the worker goroutine so the
	// moves never race live filing (design routing-and-filing §2).
	Reclassify
	// ReloadConfig signals a global config swap; the worker re-reads the atomic
	// pointer at the next cycle boundary.
	ReloadConfig
	// ResetIdentity re-confirms this rig's device and clears its removable-media
	// marker (and persisted media id) from every watched share, so a deliberately
	// swapped stick re-adopts cleanly; a leftover legacy rig-identity marker is
	// swept best-effort at the same time. An explicit operator action.
	ResetIdentity
	// StopRun ends a run until idle at the next frame boundary, leaving the rig
	// paused. On a rig with no run in progress it does nothing.
	StopRun
)

// maxNoProgressCycles ends a run until idle after this many consecutive cycles
// that neither copied a frame nor moved the delete cursor, or, behind a listing
// that failed, neither copied nor deleted one: a destination that stays
// unreadable, a share that never lists, a device whose identity never confirms.
// Such a run is idle in effect, possibly with errors, and would otherwise run for
// as long as the fault lasts.
const maxNoProgressCycles = 3

// reasonOfflineDuringRun is the Safe verdict's reason when a run until idle
// ended because the rig went offline: the pill reads Paused, so nothing claims
// the device is still offline, and this says why the verdict is unknown.
const reasonOfflineDuringRun = "offline_during_run"

// Worker owns one managed rig's lifecycle. Construct with NewWorker and drive
// with Run; feed control messages with Send.
type Worker struct {
	profileID string
	cfg       *atomic.Pointer[model.Config]
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device
	resolver  Resolver
	// sites looks up the site an endpoint names, for its costs: latency margins,
	// stall window, rate cap. Dials go through the device factory and dialControl.
	// A cycle resolves its site here once and carries it in its cycle value; no
	// worker field holds it, so nothing outside a cycle can read a stale one.
	sites netsite.Sites
	// dialControl opens the device's JSON-RPC control channel for the optional
	// clock sync. Defaulted in NewWorker rather than passed positionally: the
	// constructor already takes seventeen arguments and one of its call sites sits
	// behind the integration build tag, where a signature change compiles green
	// and only breaks after merge. Tests override the field directly.
	dialControl control.Dialer
	// lookupHost resolves a legacy Host to IP addresses (injectable for hermetic
	// tests, defaulting to net.LookupHost).
	lookupHost func(host string) ([]string, error)
	// syncTemp flushes a staged temp to disk (injectable so a test can fail the
	// flush, which a real temp file cannot be made to do; defaults to File.Sync).
	syncTemp func(*os.File) error

	lfs localfs.LocalFS
	hdr header.Reader
	// rewriter rewrites a FITS OBJECT card (design routing-and-filing §9.1),
	// injected so a test can substitute a deliberately-corrupting writer; defaults
	// to header.RewriteObject.
	rewriter func(src, dst, newTarget string) (header.RewriteResult, error)
	// crashAfterRename, when non-nil, is consulted inside fileFrom immediately after a
	// successful RenameNoReplace and before the DB commit. Returning true makes fileFrom
	// abandon the filing there, modelling a process crash in that window (design
	// routing-and-filing §9.3 crash-window table, INV-6). nil in production — a
	// test-only seam, like rewriter.
	crashAfterRename func(destPath string) bool
	// crashAfterRefileRename is the same seam for the re-file move, which cannot
	// borrow the one above: crashAfterRename is consulted inside fileFrom, and
	// re-file must not call fileFrom (design routing-and-filing §8.1). Returning
	// true abandons the move between the rename and the commit, leaving the file
	// at its new path, the row at the old one and the intent open — the window
	// RecoverRefileIntents resolves.
	crashAfterRefileRename func(destPath string) bool
	// crashAfterSupersedeStep is the same seam for the four-step name claim, which
	// has four crash windows rather than one: it is consulted after each step with
	// that step's number, and returning true abandons the claim there, leaving the
	// intent open — the windows RecoverSupersedeIntents resolves. One hook rather
	// than four fields, because the four windows are one event modelled four ways.
	crashAfterSupersedeStep func(step int, destPath string) bool
	store                   store.Store
	hub                     hub.Hub
	notify                  notify.Notifier
	adm                     Admission
	// metrics records pipeline outcomes; nil-safe (a nil handle no-ops every call).
	metrics *metrics.Metrics
	// previewer warms a thumbnail when a frame files and evicts previews when a
	// frame is dismissed. Optional: nil means previews are disabled.
	previewer imgpreview.Previewer
	// nudge wakes the Tier-1 solve worker when a hinted light with no embedded WCS
	// is filed, for low-latency solving. Non-blocking; a full channel is fine (the
	// solve worker's from-zero re-sweep is the queue of record). nil when plate
	// solving is disabled.
	nudge chan<- int64
	log   *slog.Logger

	siteLoc *time.Location
	now     func() time.Time

	cmds   chan Command
	paused bool
	// pauseRequested is set by Send(Pause) before the command is queued, so a
	// device re-hash already streaming can abandon its read between buffers instead
	// of holding the Pause for a whole file (design frame-lifecycle §12
	// design frame-lifecycle §12.8). It is the one field Send writes from another
	// goroutine. It must never outlive the Pause it announces, or every later
	// re-hash would abort and deletes would stall: setPaused(true), a dropped
	// enqueue and Send(Resume) clear it.
	pauseRequested atomic.Bool
	// runUntilIdle is set while a Run now on a paused rig runs until idle (design
	// frame-lifecycle §12.6 Run state). The rig stays paused, but a rig
	// is suspended, and runs no cycle work, only while paused with no run in
	// progress. Mirrored to st.RunUntilIdle by setRunUntilIdle; not persisted.
	runUntilIdle bool
	// The run's progress record, re-based when a run starts. listings counts the
	// cycles that listed the device and attempted a pull; runSweep and runListing
	// are the sweep (in sweeps' terms) and the listing count at the run's last copy
	// or delete, or at its start. noProgress counts consecutive cycles without
	// progress, as trackRun defines it.
	listings   int
	runSweep   int
	runListing int
	noProgress int

	// dismissReqs carries synchronous discard-frame requests onto the worker
	// goroutine (design frame-lifecycle §13): frame mutations happen on exactly
	// this goroutine, so dismiss cannot race the pull/delete passes. Unbuffered — the
	// HTTP handler blocks on the reply until the worker services it.
	dismissReqs chan dismissReq

	// Out-of-band deletion reconciliation (touched only from the worker goroutine
	// except the channels, which are the cross-goroutine path). lastScan gates the
	// automatic library scan; rescanReqs is an operator's manual rescan (cap 1,
	// non-blocking send: a queued request means a scan is already coming);
	// restoreReqs carries synchronous restore/cancel requests; lastRestoreAttempt
	// backs off failed restores per frame without touching the pull-retry columns.
	rescanReqs  chan struct{}
	restoreReqs chan restoreReq
	linkReqs    chan linkReq
	// retargetReqs carries synchronous per-frame retarget requests onto the worker
	// goroutine, serviced ONLY in the Run select (not drainCommands): retarget acquires
	// an admission slot and does heavy NAS work, so servicing it inside deleteOne's
	// drainCommands — which already holds a slot — would deadlock a single-slot config,
	// the same reason LinkLibrary is excluded (design routing-and-filing §9.3).
	retargetReqs chan retargetReq
	// refileReqs carries a whole selected re-file GROUP onto the worker goroutine,
	// serviced ONLY in the Run select. The reason is latency rather than the
	// admission-slot deadlock that keeps retarget out of drainCommands — a re-file
	// takes no slot — but a per-frame request would wait a full poll cycle each
	// time, so a 500-frame migration would stretch across a night of imaging while
	// frames piled up on the ASIAIR (design routing-and-filing §8.8).
	refileReqs chan refileReq
	// wakeReqs carries "this rig's device just answered a discovery scan" onto the
	// worker goroutine (cap 1, non-blocking send: a queued wake already means a
	// probe is coming). It is deliberately NOT a Command, so a wake that runs no
	// cycle can never touch Run's ticker: a device answering every 45 s must not
	// postpone the scheduled cycle. Nor is it serviced in drainCommands —
	// mid-cycle the rig is already being probed.
	wakeReqs           chan struct{}
	lastScan           time.Time
	lastRestoreAttempt map[int64]time.Time
	// restoreInconclusive counts the consecutive passes a frame's restore has spent
	// on restoreOne's inconclusive arm, so a deferral that can never resolve itself
	// escalates instead of staying quiet. Keyed and reset exactly as the backoff above
	// is, the two describing the same episode of one frame's restore.
	restoreInconclusive map[int64]int
	// ensuredRoots caches which destination roots this worker has already
	// auto-linked (or found already linked) this session, so the common filed-frame
	// path costs one map lookup, not a store read, after the first frame per root.
	ensuredRoots map[string]bool

	// remoteWakeReqs carries "this rig's remote changed state" (cap 1,
	// non-blocking): the cycle it runs writes the rig's remote-down state, or
	// its own again, at once rather than at the next scheduled cycle.
	remoteWakeReqs chan struct{}

	settle *settleTracker

	// cross-cycle status/bookkeeping (touched only from the worker goroutine).
	wasOnline          bool
	unreachableSince   time.Time
	unreachableAlerted bool
	// wakeNotBefore and wakeGap rate-limit discovery-driven probes. A device whose
	// UDP responder answers while its SMB server stays down would otherwise buy a
	// probe every discovery round, forever: the first wake of an offline episode is
	// free, and each one that still finds the rig unreachable doubles the gap from
	// the poll interval up to the retry interval — past which a wake buys nothing
	// over the normal retry cadence. markOnline clears both, so the next episode
	// starts free again.
	wakeNotBefore        time.Time
	wakeGap              time.Duration
	pendingReclassify    bool
	pendingResetIdentity bool
	// noConfirmStreak counts consecutive cycles the device gave no scan_air reply
	// to the identity probe; at 3 it raises one degraded alert, reset to 0 on any
	// definite reply (confirmed OR foreign).
	noConfirmStreak    int
	prevListingSig     string
	lastLiveCounterPub time.Time
	// backlogRaised tracks whether the aggregate never-deletable backlog alert is
	// currently up, so the per-cycle auto-ack scan runs only on the resolving
	// transition rather than every healthy cycle.
	//
	// Both latches below START TRUE: at construction we have not read the alert
	// table, so an alert raised by a PREVIOUS process may be standing. Starting
	// false would mean a rig that boots with the condition already clear never
	// takes the resolving branch, leaving that stale alert up forever — it can
	// only be dismissed by hand. Starting true costs one alert scan on the first
	// healthy cycle and then latches off as normal.
	backlogRaised bool
	// deviceLowRaised does the same for the device-disk-low alert, which likewise
	// has no other path that retires it once the condition clears.
	deviceLowRaised bool
	// stallRaised does the same for the stalled-progress alert, retired by the
	// next pass that makes progress. stallPasses counts the consecutive passes a
	// remote rig failed with its tunnel up, and stallBlocked the delete attempts
	// that failed over them (trackStall).
	stallRaised  bool
	stallPasses  int
	stallBlocked int
	// cycleCandidates and terminalThisCycle build the live feed's ephemeral pending
	// set (design features §4.3). cycleCandidates collects
	// every pull-candidate seen this listing; terminalThisCycle records the settle
	// keys that reached a terminal outcome (frame.done/frame.failed) this cycle. The
	// published Pending set is candidates minus terminals — frames with no terminal
	// outcome (settling, admission-deferred, transiently retrying). Reset each cycle.
	cycleCandidates   []model.PendingFrame
	terminalThisCycle map[string]bool
	// destUnavailable latches when admission reports the destination volume
	// unreadable (ErrDestinationUnavailable). Every queued frame would fail the
	// same way, so the listing loop abandons the rest of the cycle instead of
	// failing each one. Reset each cycle alongside the accumulators above.
	destUnavailable bool
	// destWriteErr is the last failure to write the staging area — a volume that
	// still answers a free-space query but refuses writes or flushes (a disk that
	// dropped out from under its drive letter). Unlike destUnavailable it is NOT
	// reset each cycle: it stands until a write probe passes, so a broken volume
	// costs one probe per cycle instead of a whole frame streamed and discarded.
	destWriteErr error
	// devicePending is the latest count of frames still on the device awaiting
	// work (uncopied, settling, held or failed). It keeps the drain line from
	// reading "drained" while frames not yet copied sit on the device.
	devicePending int
	// cycleOffset is the device-minus-host clock offset measured at this cycle's
	// SMB negotiate, reset alongside the accumulators above. clockPhase gates on
	// it, so a cycle that never opened a session reads zero and cannot authorise
	// a clock write (design features §6.5).
	cycleOffset time.Duration
	// offsetOutOfBand latches whether the offset is currently past the threshold,
	// so the log reports the transition rather than repeating a standing condition
	// once per poll interval. Updated only where an offset was actually measured:
	// an offline cycle must not read as a return to tolerance.
	offsetOutOfBand bool
	// clockRetryAt and clockFailLogged back off after a failed clock-sync attempt,
	// so an unreachable control port is not retried, and logged, every cycle.
	clockRetryAt    time.Time
	clockFailLogged bool
	// syncClockWas is the toggle's last-seen value. Flipping it clears the three
	// latches above: enabling the feature on a rig that is already out of band must
	// re-announce the condition rather than stay silent, and must not serve out a
	// cooldown the operator has just acted on.
	syncClockWas bool

	// Delete-slice state (design frame-lifecycle §12.3), owned by the
	// worker goroutine and never persisted: a restart starts the next slice at the
	// oldest eligible frame and counts a fresh drain.
	//
	// deleteCursor is the last frame a slice attempted; the next slice resumes
	// after it, so a head frame that hashes but never concludes cannot eat every
	// slice.
	deleteCursor deleteCursor
	// drainDone counts the deletes of the drain in progress, so the cleanup
	// readout's position and total carry across slices instead of restarting at 1
	// with every slice.
	drainDone int
	// ownDeletes holds the listingKey of every device original a slice removed
	// since the previous listing, so a listing that differs only by those reads
	// as no device activity (design frame-lifecycle §12.4).
	ownDeletes map[string]bool
	// listedState is the pill state, Idle or Pulling, that publishListingStatus
	// last set on an unsuspended listing. A slice's end
	// writes it back rather than the state it saw at its start, so a Pause and a
	// Resume served before the slice cannot leave a stale Paused on an unpaused rig.
	listedState model.RigState
	// sliceActive is true while a slice holds the Deleting pill, so a Resume
	// applied mid-slice republishes Deleting at once instead of leaving Paused
	// until the slice ends.
	sliceActive bool
	// deviceTight reports that the tightest share is below the delete-pressure
	// threshold (or unmeasurable); it biases slice length and is logged on yield.
	deviceTight bool
	// deleteDebt is the link time pulls have taken while the device is tight and
	// later slices have not yet repaid, so a long pull phase is repaid over several
	// capped slices rather than one slice as long as the pull (design
	// frame-lifecycle §12.1).
	deleteDebt time.Duration
	// prevListingFrames are the frames behind prevListingSig, kept so the next
	// listing can discount this worker's own deletes from the idle test (design
	// frame-lifecycle §12.4).
	prevListingFrames []listedFrame
	// sweeps counts completed sweeps: passes of the cursor from the head of the
	// eligible list to its tail (design frame-lifecycle §12.6 Run state). A slice
	// passing the tail, an empty eligible list and a KeepOriginals cycle each
	// complete one.
	sweeps int
	// drainSweep is the sweep that held the drain's last delete, in sweeps' terms;
	// the drain ends once a whole sweep has run since it (wholeSweepSince).
	drainSweep int

	st model.RigStatus
}

// NewWorker builds a Worker for the profile identified by profileID. cfg is read
// via Load at each cycle boundary and the profile resolved by ID; if the profile
// is gone or unmanaged the worker idles (the manager cancels ctx to stop it).
// newDevice is device.NewFactory over the site registry in production and a fake
// factory in tests; sites is that registry, through which the control channel
// is dialled. now supplies wall time for grace/pulled_at/retry timing (time.Now
// in production, a controllable func in tests); siteLoc is the local zone for
// observing-date derivation (time.Local in production). Nil now/siteLoc/logger
// fall back to sensible defaults.
func NewWorker(
	profileID string,
	cfg *atomic.Pointer[model.Config],
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device,
	resolver Resolver,
	sites netsite.Sites,
	lfs localfs.LocalFS,
	hdr header.Reader,
	st store.Store,
	h hub.Hub,
	ntf notify.Notifier,
	adm Admission,
	prev imgpreview.Previewer,
	nudge chan<- int64,
	siteLoc *time.Location,
	now func() time.Time,
	mx *metrics.Metrics,
	logger *slog.Logger,
) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if siteLoc == nil {
		siteLoc = time.Local
	}
	w := &Worker{
		profileID:           profileID,
		cfg:                 cfg,
		newDevice:           newDevice,
		resolver:            resolver,
		sites:               sites,
		dialControl:         control.NewDialer(sites),
		lookupHost:          net.LookupHost,
		syncTemp:            (*os.File).Sync,
		lfs:                 lfs,
		hdr:                 hdr,
		rewriter:            header.RewriteObject,
		store:               st,
		hub:                 h,
		notify:              ntf,
		adm:                 adm,
		previewer:           prev,
		nudge:               nudge,
		metrics:             mx,
		log:                 logger.With("rig", profileID),
		siteLoc:             siteLoc,
		now:                 now,
		cmds:                make(chan Command, 8),
		dismissReqs:         make(chan dismissReq),
		rescanReqs:          make(chan struct{}, 1),
		restoreReqs:         make(chan restoreReq),
		linkReqs:            make(chan linkReq),
		retargetReqs:        make(chan retargetReq),
		refileReqs:          make(chan refileReq),
		wakeReqs:            make(chan struct{}, 1),
		remoteWakeReqs:      make(chan struct{}, 1),
		lastRestoreAttempt:  map[int64]time.Time{},
		restoreInconclusive: map[int64]int{},
		ensuredRoots:        map[string]bool{},
		ownDeletes:          map[string]bool{},
		settle:              newSettleTracker(mtimeTol),
		// Non-nil so a terminal emit from a reclassify command dispatched before the
		// first pull cycle writes safely; pullPhase replaces it each cycle.
		terminalThisCycle: map[string]bool{},
		// Assume both auto-acked alerts may be standing from a previous process, so
		// the first healthy cycle retires them; see the field comments.
		backlogRaised:   true,
		deviceLowRaised: true,
		stallRaised:     true,
	}
	w.st = model.RigStatus{
		ID:             profileID,
		State:          model.RigStateIdle,
		SafeToPowerOff: model.SafeUnknown,
		Drain:          "unknown",
		// Non-nil so a rig that has not completed a listing serializes shares as []
		// rather than null; the dashboard maps over it directly.
		Shares: []model.ShareFree{},
	}
	if p := cfg.Load().Rig(profileID); p != nil {
		w.st.Name = p.Name
		w.st.KeepOriginals = p.KeepOriginals
	}
	return w
}

// Send delivers a control command. It is non-blocking up to the channel's buffer;
// a full buffer (a wedged worker) drops the command with a warning rather than
// stalling the caller (an HTTP handler).
//
// It records what the command asks for on the hub the moment it is queued: the
// worker applies it only between files, and until then the card would otherwise
// keep offering the same action. After queueing, so a dropped command records
// nothing.
//
// Whether the rig is paused, or running until idle, is read from the request the
// hub already holds rather than from the worker: the request follows the queue,
// the worker lags it by up to a file. A Run now queued behind a Pause is served
// on a paused rig and starts a run, so it must record run until idle although
// the worker has not yet applied the Pause.
func (w *Worker) Send(c Command) {
	// Set before the enqueue: otherwise the worker could apply this Pause, clearing
	// the flag in setPaused, before the flag is set, and the late set would outlive
	// its Pause.
	if c == Pause {
		w.pauseRequested.Store(true)
	}
	select {
	case w.cmds <- c:
	default:
		if c == Pause {
			// No Pause will be applied, so nothing else would ever clear the flag.
			w.pauseRequested.Store(false)
		}
		w.metrics.RecordWorkerCommandDropped(w.profileID)
		w.log.Warn("rig: command channel full; dropping command", "command", c)
		return
	}
	switch c {
	case Pause:
		w.hub.SetRequested(w.profileID, model.RequestPaused)
	case Resume:
		// Covers a Pause and a Resume both still queued: the Pause's setPaused(true)
		// would clear the flag too, but only once served, and until then a re-hash
		// would abort for a pause the Resume has already taken back.
		w.pauseRequested.Store(false)
		w.hub.SetRequested(w.profileID, model.RequestRunning)
	case RunNow:
		// Only a paused rig runs until idle; on a running one Run now is a check now,
		// and during a run it takes back a pending stop.
		w.hub.SetRequested(w.profileID, model.RequestRunUntilIdle, model.RequestPaused)
	case StopRun:
		// Only a run in progress, or one already requested, has anything to stop.
		w.hub.SetRequested(w.profileID, model.RequestPaused, model.RequestRunUntilIdle)
	}
}

// WakeFromDiscovery reports that this rig's device answered a scan_air round, so
// a worker that believes the rig unreachable re-probes now instead of waiting out
// its retry interval. It never blocks: whether the answer is worth a probe is
// decided on the worker goroutine (the only one that knows whether the rig is
// offline, paused, or inside its wake backoff), and a wake already queued means a
// probe is coming anyway.
func (w *Worker) WakeFromDiscovery() {
	select {
	case w.wakeReqs <- struct{}{}:
	default:
	}
}

// WakeForRemote reports that the remote this rig is reached through changed
// state, so the worker runs a cycle now: the rig's card reads its remote's state,
// or its own again, as soon as the remote's does. It never blocks; a wake already
// queued covers this one.
func (w *Worker) WakeForRemote() {
	select {
	case w.remoteWakeReqs <- struct{}{}:
	default:
	}
}

// dismissReq is a synchronous discard-frame request: the worker services it on
// its own goroutine and returns the outcome on reply.
type dismissReq struct {
	frameID int64
	reply   chan error
}

// restoreReqKind selects which mutation a restoreReq carries. Restore/cancel and
// the two acknowledge variants all funnel through the one channel so every frame
// mutation runs on the worker goroutine and cannot race a pull/delete pass.
type restoreReqKind int

const (
	restoreRequest restoreReqKind = iota
	restoreCancel
	restoreAcknowledge
	restoreAcknowledgeIDs
)

// restoreReply carries the outcome back to the caller. n is the number of rows
// acknowledged (restoreAcknowledgeIDs only; zero on every other kind).
type restoreReply struct {
	n   int
	err error
}

// restoreReq carries a synchronous restore/cancel/acknowledge request onto the
// worker goroutine, so the row flags are set/cleared without racing the delete
// pass. reply MUST be buffered (cap 1): the acknowledge timeout path (design
// library-reconciliation §10.7) abandons the reply while the worker still sends
// it, so an unbuffered channel would block the worker goroutine forever.
type restoreReq struct {
	kind    restoreReqKind
	frameID int64
	ids     []int64 // restoreAcknowledgeIDs only
	dir     string  // restoreAcknowledgeIDs only, for the audit log line
	reply   chan restoreReply
}

// linkReq carries a synchronous link-library request onto the worker goroutine.
type linkReq struct {
	root  string
	reply chan error
}

// retargetReq carries a synchronous per-frame retarget request onto the worker
// goroutine (design routing-and-filing §9.3/§9.6). reply MUST be buffered (cap 1) so a
// caller that abandons the wait on ctx cancellation never blocks the worker's send.
type retargetReq struct {
	frameID   int64
	newTarget string
	reply     chan retargetReply
}

// retargetReply carries the outcome of one retarget back to the caller.
type retargetReply struct {
	result model.RetargetResult
	err    error
}

// refileReq carries one selected re-file group — a whole group, never a single
// frame — onto the worker goroutine (design routing-and-filing §8.8).
//
// The request's own context travels with it because cancelling the HTTP request
// has to stop the migration: refileGroup checks it between frames, and the
// worker's lifetime context is not the operator's cancel. reply MUST be buffered
// (cap 1) so a caller that abandons the wait never blocks the worker's send.
type refileReq struct {
	ctx      context.Context
	wantDir  string
	frameIDs []int64
	reply    chan refileReply
}

// refileReply carries one group's per-file outcomes plus the prune summary back to
// the caller. The summary rides along because the prune runs on the worker at the
// end of the group and is the apply stream's final line.
type refileReply struct {
	results []model.RefileResult
	summary model.RefileSummary
	err     error
}

// LinkLibrary records a destination root as this installation's library,
// synchronously on the worker goroutine (so the marker write and the missing-mark
// clear do not race the scan). It is NOT serviced from drainCommands because it
// touches the NAS, and drainCommands runs between device operations.
func (w *Worker) LinkLibrary(ctx context.Context, root string) error {
	req := linkReq{root: root, reply: make(chan error, 1)}
	select {
	case w.linkReqs <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RetargetFrame relabels one archived frame's OBJECT card and re-files it under the
// corrected target, synchronously on the worker goroutine (design routing-and-filing
// §9.3/§9.6). It never touches the device original; the reply is buffered cap-1 so an
// abandoned wait cannot block the worker.
func (w *Worker) RetargetFrame(ctx context.Context, frameID int64, newTarget string) (model.RetargetResult, error) {
	req := retargetReq{frameID: frameID, newTarget: newTarget, reply: make(chan retargetReply, 1)}
	select {
	case w.retargetReqs <- req:
	case <-ctx.Done():
		return model.RetargetResult{FrameID: frameID}, ctx.Err()
	}
	select {
	case rep := <-req.reply:
		return rep.result, rep.err
	case <-ctx.Done():
		return model.RetargetResult{FrameID: frameID}, ctx.Err()
	}
}

// PreviewRetarget projects where the given frames' archives would move under newTarget.
// It is read-only (no NAS/device I/O), so it runs directly rather than being serviced on
// the worker goroutine (design routing-and-filing §9.6).
func (w *Worker) PreviewRetarget(ctx context.Context, ids []int64, newTarget string) ([]model.RetargetPreview, error) {
	return w.previewRetarget(ctx, ids, newTarget), nil
}

// RefileGroup moves one selected group's frames to where the current templates say
// they belong, synchronously on the worker goroutine. It takes no admission slot —
// a rename costs no transient space, so contending with live pulls would buy
// nothing — and it carries the whole group, so a migration is not paced by the
// poll cycle. The reply is buffered cap-1 so an abandoned wait cannot block the
// worker.
func (w *Worker) RefileGroup(ctx context.Context, wantDir string, frameIDs []int64) ([]model.RefileResult, model.RefileSummary, error) {
	req := refileReq{ctx: ctx, wantDir: wantDir, frameIDs: frameIDs, reply: make(chan refileReply, 1)}
	select {
	case w.refileReqs <- req:
	case <-ctx.Done():
		return nil, model.RefileSummary{}, ctx.Err()
	}
	select {
	case rep := <-req.reply:
		return rep.results, rep.summary, rep.err
	case <-ctx.Done():
		// The group stops itself between frames on the same cancellation, so the
		// abandoned outcomes are simply the work a re-run of the diff will offer
		// again — re-file has no resume state because it needs none.
		return nil, model.RefileSummary{}, ctx.Err()
	}
}

// PreviewRefile projects the whole migration for this rig. It runs on the caller's
// goroutine rather than the worker's, exactly as PreviewRetarget does: it is
// read-only, and putting a whole-library diff into a select with no default would
// block pulls and the poll ticker for its duration.
func (w *Worker) PreviewRefile(ctx context.Context, scope string) (model.RefilePreview, error) {
	return w.previewRefile(ctx, scope)
}

// serviceRetarget loads the frame and rig config on the worker goroutine and runs the
// design routing-and-filing §9.3 retarget operation. A missing rig/frame yields an
// in-band skipped/failed result rather than an error, so the streaming apply handler
// reports it per frame.
func (w *Worker) serviceRetarget(ctx context.Context, frameID int64, newTarget string) (model.RetargetResult, error) {
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	if profile == nil {
		return model.RetargetResult{FrameID: frameID, Status: "failed", Reason: "rig not managed"}, nil
	}
	f, ok, err := w.store.GetFrameByID(ctx, frameID)
	if err != nil {
		return model.RetargetResult{FrameID: frameID, Status: "failed", Reason: "lookup: " + err.Error()}, err
	}
	if !ok || f.RigID != w.profileID {
		return model.RetargetResult{FrameID: frameID, Status: "skipped", Reason: "frame not found"}, nil
	}
	return w.retargetFrame(ctx, cfg, profile, *f, newTarget)
}

// RescanLibrary asks the worker to run a library scan on its next cycle. The
// channel has capacity 1 with a non-blocking send: if a request is already queued,
// a scan is already coming, which is the caller's desired end state, so it returns
// nil rather than blocking.
func (w *Worker) RescanLibrary() error {
	select {
	case w.rescanReqs <- struct{}{}:
	default:
	}
	return nil
}

// RestoreFrame flags a missing frame for re-pull from a still-resident device
// original, synchronously on the worker goroutine. CancelRestore clears the flag.
func (w *Worker) RestoreFrame(ctx context.Context, frameID int64) error {
	_, err := w.restoreRequest(ctx, restoreReq{kind: restoreRequest, frameID: frameID, reply: make(chan restoreReply, 1)})
	return err
}

// CancelRestore clears a pending restore request for a frame.
func (w *Worker) CancelRestore(ctx context.Context, frameID int64) error {
	_, err := w.restoreRequest(ctx, restoreReq{kind: restoreCancel, frameID: frameID, reply: make(chan restoreReply, 1)})
	return err
}

// AcknowledgeMissing waives the reconcile-settle window for one culled frame,
// synchronously on the worker goroutine so the mutation cannot race a delete pass.
func (w *Worker) AcknowledgeMissing(ctx context.Context, frameID int64) error {
	_, err := w.restoreRequest(ctx, restoreReq{kind: restoreAcknowledge, frameID: frameID, reply: make(chan restoreReply, 1)})
	return err
}

// AcknowledgeMissingIDs waives the settle window for a bound set of culled frames
// under one directory, returning the number of rows actually acknowledged. dir is
// carried only for the audit log line.
func (w *Worker) AcknowledgeMissingIDs(ctx context.Context, dir string, ids []int64) (int, error) {
	return w.restoreRequest(ctx, restoreReq{kind: restoreAcknowledgeIDs, ids: ids, dir: dir, reply: make(chan restoreReply, 1)})
}

func (w *Worker) restoreRequest(ctx context.Context, req restoreReq) (int, error) {
	select {
	case w.restoreReqs <- req:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case rep := <-req.reply:
		return rep.n, rep.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// serviceRestore applies a restore/cancel/acknowledge request on the worker
// goroutine. n is non-zero only for the id-set acknowledge.
func (w *Worker) serviceRestore(ctx context.Context, req restoreReq) (int, error) {
	switch req.kind {
	case restoreCancel:
		return 0, w.store.ClearRestoreRequest(ctx, w.profileID, req.frameID)
	case restoreAcknowledge:
		return 0, w.serviceAcknowledge(ctx, req.frameID)
	case restoreAcknowledgeIDs:
		return w.serviceAcknowledgeIDs(ctx, req.dir, req.ids)
	default: // restoreRequest
		ok, err := w.store.RequestRestore(ctx, w.profileID, req.frameID, w.now().Unix())
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, ErrFrameNotMissing
		}
		return 0, nil
	}
}

// serviceAcknowledge stamps missing_acked_at for one culled frame, waiving the
// settle window (design library-reconciliation §10.8). The guard order is canonical
// here — MissingSeenAt is tested BEFORE MissingAckedAt is read (design
// library-reconciliation §10.5's read discipline), then idempotence, then state,
// then restore — and the store's WHERE is the durable backstop: a changed==false
// result is a race, never reported as success.
func (w *Worker) serviceAcknowledge(ctx context.Context, frameID int64) error {
	f, ok, err := w.store.GetFrameByID(ctx, frameID)
	if err != nil {
		return err
	}
	if !ok || f.RigID != w.profileID {
		return ErrFrameNotFound
	}
	if f.MissingSeenAt == 0 {
		return ErrFrameNotMissing
	}
	if f.MissingAckedAt != 0 {
		return nil // idempotent: already vouched for
	}
	if !f.State.ProvenArchived() {
		return ErrNotAcknowledgeable
	}
	if f.RestoreRequestedAt != 0 {
		return ErrRestorePending
	}
	changed, err := w.store.AcknowledgeMissing(ctx, w.profileID, frameID, w.now().Unix())
	if err != nil {
		return err
	}
	if !changed {
		// Raced between the read above and this write — NEVER report success. The
		// WHERE can fail for several distinct reasons, so re-read and classify rather
		// than telling the operator "not marked missing", which is usually false.
		return w.classifyAckRefusal(ctx, frameID)
	}
	w.log.Warn("rig: operator acknowledged a culled frame; its device original will be deleted next pass",
		"frame", f.ID, "path", f.SourcePath, "dest", f.DestPath)
	return nil
}

// classifyAckRefusal re-reads a row whose acknowledge write affected no rows and
// returns the sentinel matching whichever guard now fails, so the operator hears a
// truthful reason rather than a flatly wrong "not marked missing" (design
// library-reconciliation §10.8). Best-effort by construction — the row can
// change again.
func (w *Worker) classifyAckRefusal(ctx context.Context, frameID int64) error {
	f, ok, err := w.store.GetFrameByID(ctx, frameID)
	if err != nil {
		return err
	}
	if !ok || f.RigID != w.profileID {
		return ErrFrameNotFound
	}
	if f.MissingSeenAt == 0 {
		return ErrFrameNotMissing
	}
	if f.MissingAckedAt != 0 {
		return nil // already acknowledged — not an error
	}
	if !f.State.ProvenArchived() {
		return ErrNotAcknowledgeable
	}
	if f.RestoreRequestedAt != 0 {
		return ErrRestorePending
	}
	return ErrFrameNotMissing
}

// serviceAcknowledgeIDs waives the settle window for a bound id set, returning the
// count actually acknowledged and logging one WARN with the directory and count —
// the audit line that makes a scripted or click-through burst visible after the
// fact (design library-reconciliation §10.8).
func (w *Worker) serviceAcknowledgeIDs(ctx context.Context, dir string, ids []int64) (int, error) {
	n, err := w.store.AcknowledgeMissingByIDs(ctx, w.profileID, ids, w.now().Unix())
	if err != nil {
		return n, err
	}
	if n > 0 {
		// Only when something was actually acknowledged: on a race that left the
		// bound set empty, the "will be deleted next pass" claim would be false.
		w.log.Warn("rig: operator acknowledged a directory of culled frames; their device originals will be deleted next pass",
			"dir", dir, "count", n)
	}
	return n, nil
}

// ErrFrameNotMissing is returned when the target frame is not marked missing, so a
// restore or acknowledge has nothing to do. ErrRestorePending is returned when an
// acknowledge is refused because a restore is pending (cancel it first), and
// ErrNotAcknowledgeable when the frame is neither verified nor unclassified (a
// proven-archived state) — i.e. held or deleted. The web layer maps each to 409;
// any other error is a genuine failure (500).
var (
	ErrFrameNotMissing    = fmt.Errorf("frame is not marked missing")
	ErrRestorePending     = errors.New("rig: a restore is pending for this frame")
	ErrNotAcknowledgeable = errors.New("rig: held or deleted frames cannot be acknowledged")
)

// DiscardFrame dismisses one unclassified frame on the worker goroutine and
// returns the outcome synchronously. Callers should pass a context with a
// timeout so a busy worker fails the HTTP request rather than blocking it
// indefinitely. (The timeout bounds the caller, not an in-flight NAS syscall on
// the worker goroutine — see design frame-lifecycle §13.4.)
func (w *Worker) DiscardFrame(ctx context.Context, frameID int64) error {
	reply := make(chan error, 1)
	select {
	case w.dismissReqs <- dismissReq{frameID: frameID, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is the worker's select loop (design architecture §3). It never overlaps
// cycles: the ticker is reset AFTER each cycle returns (fixed delay, not fixed
// rate), so a long first sync of a full device simply delays the next cycle
// instead of stacking sessions. It returns when ctx is cancelled (shutdown /
// rig removed). minPollInterval floors any cadence fed to the ticker. A
// non-positive interval (from a hand-edited or partially-written config that
// bypassed API validation) would panic time.Ticker outside runCycle's recover
// and crash the whole service, so clamp defensively here in addition to config
// validation.
const minPollInterval = time.Second

func safeInterval(d time.Duration) time.Duration {
	if d < minPollInterval {
		return minPollInterval
	}
	return d
}

// stampNextPoll records the wall-clock time of the next scheduled cycle, d from
// now, onto the status, using the same clamp the ticker gets, so the dashboard's
// live "next check in Xs" countdown lines up with when the worker actually wakes.
// cadence is published beside it as the rig's configured interval, not d: a 1 s
// continuation after a delete slice is not the rig's cadence, and the disable
// guard reads PollIntervalSec as the cadence behind its listing-freshness check.
func (w *Worker) stampNextPoll(d, cadence time.Duration) {
	w.st.NextPollAt = w.now().Add(safeInterval(d)).Unix()
	// Publish the whole-second cadence too, so the UI can render the countdown as a
	// depleting fraction (remaining ÷ interval) rather than bare seconds.
	w.st.PollIntervalSec = int(safeInterval(cadence) / time.Second)
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(safeInterval(w.pollInterval()))
	defer ticker.Stop()

	// A command queued before Run started — the manager sends Pause to restore a
	// pause that outlived a restart — is applied here, before any cycle work. Left
	// to the loop below, the initial cycle would reach it first through
	// drainCommands and a rig deliberately paused would pull and delete its way
	// through one more cycle before honouring it.
	next := w.pollInterval()
	ran := false
	for drained := false; !drained; {
		select {
		case c := <-w.cmds:
			// A command that ran a cycle here (Resume, or Run now starting a run on a
			// paused rig) has already listed; the initial cycle below must not list
			// again at once, or two listings would land closer than one poll apart.
			if n, sched := w.apply(ctx, c); sched == schedRan {
				next, ran = n, true
			}
		default:
			drained = true
		}
	}

	// Run an initial cycle promptly rather than waiting a full interval.
	if !ran && !w.workSuspended() {
		next = w.runCycle(ctx)
	}
	ticker.Reset(safeInterval(next))

	for {
		select {
		case <-ctx.Done():
			w.log.Info("rig: worker stopping")
			return
		case c := <-w.cmds:
			// A command that runs no cycle (a config reload, a pause) leaves a pending
			// continuation's deadline alone, so a config save during a drain does not
			// push the next slice out to a full poll. With no continuation pending it
			// reschedules as always, so a changed poll interval applies from the reload.
			// A Run now during a run leaves the deadline alone either way.
			if next, sched := w.apply(ctx, c); sched == schedRan || sched == schedNoCycle && !w.st.Draining {
				ticker.Reset(safeInterval(next))
			}
		case req := <-w.dismissReqs:
			req.reply <- w.discardOneGuarded(ctx, req.frameID)
		case req := <-w.restoreReqs:
			n, err := w.serviceRestore(ctx, req)
			req.reply <- restoreReply{n: n, err: err}
		case req := <-w.linkReqs:
			req.reply <- w.linkLibrary(ctx, req.root)
		case req := <-w.retargetReqs:
			res, err := w.serviceRetarget(ctx, req.frameID, req.newTarget)
			req.reply <- retargetReply{result: res, err: err}
		case req := <-w.refileReqs:
			// The request's context, not the worker's: a cancelled or timed-out apply
			// must stop the group between frames. The group runs to completion without
			// yielding, which is affordable because each frame costs a Stat, a rename
			// and a small transaction.
			res, sum, err := w.refileGroup(req.ctx, req.wantDir, req.frameIDs)
			req.reply <- refileReply{results: res, summary: sum, err: err}
		case <-w.wakeReqs:
			// Only a wake that actually ran a cycle reschedules the ticker. A declined
			// one must leave it alone: resetting it on every discovery round would push
			// the scheduled cycle out by a full interval each time and the rig would
			// never poll on its own again.
			if next, ran := w.serviceDiscoveryWake(ctx); ran {
				ticker.Reset(safeInterval(next))
			}
		case <-w.remoteWakeReqs:
			// A suspended rig runs no cycle: its Paused card stands whatever its
			// remote does, and the ticker keeps its deadline.
			if w.workSuspended() {
				w.log.Debug("rig: remote changed state; the rig is paused")
				break
			}
			w.log.Debug("rig: remote changed state; running a cycle now")
			ticker.Reset(safeInterval(w.runCycle(ctx)))
		case <-ticker.C:
			// A paused rig running until idle gets its cycles, 1 s continuations
			// included, exactly as an unpaused one does.
			n := w.pollInterval()
			if !w.workSuspended() {
				n = w.runCycle(ctx)
			}
			ticker.Reset(safeInterval(n))
		}
	}
}

// applySchedule tells Run what a command served by apply does to its ticker.
type applySchedule int

const (
	// schedNoCycle: no cycle ran; Run reschedules to next (the poll interval)
	// unless a continuation is pending.
	schedNoCycle applySchedule = iota
	// schedRan: a cycle ran; Run reschedules to the delay it returned.
	schedRan
	// schedKeep: the ticker's pending deadline stands, continuation or not.
	schedKeep
)

// apply handles one control command and returns the interval to schedule next,
// the delay the cycle it ran returned or the poll interval when it ran none, and
// what Run does with it (applySchedule).
func (w *Worker) apply(ctx context.Context, c Command) (next time.Duration, sched applySchedule) {
	switch c {
	case Pause:
		w.setPaused(true)
	case Resume:
		w.setPaused(false)
		// Publish the un-paused state first, then re-check immediately: the verdict
		// still carries the pause reason, and waiting a whole poll interval to correct
		// it would tell an operator to resume a rig they just resumed.
		return w.runCycle(ctx), schedRan
	case RunNow:
		switch {
		case !w.paused:
			w.log.Info("rig: run-now")
			return w.runCycle(ctx), schedRan
		case w.runUntilIdle:
			// No cycle, and the pending ticker deadline stands whether or not a
			// continuation is pending: resetting it to a full poll on each press would
			// postpone the run's next cycle for as long as the presses kept coming, and
			// running one would start listings closer together than its own cadence.
			w.log.Info("rig: run-now during a run until idle; the run goes on")
			return w.pollInterval(), schedKeep
		default:
			w.startRun()
			return w.runCycle(ctx), schedRan
		}
	case StopRun:
		if !w.runUntilIdle {
			w.log.Info("rig: stop-run with no run in progress; ignored")
			break
		}
		w.setRunUntilIdle(false)
		w.endRun("stop run")
	case Reclassify:
		w.log.Info("rig: reclassify command")
		w.runReclassify(ctx)
	case ReloadConfig:
		w.log.Info("rig: config reload signalled")
	case ResetIdentity:
		w.log.Info("rig: reset-identity command")
		w.resetIdentity(ctx)
		return w.runCycle(ctx), schedRan // re-confirm + resume pulls immediately
	}
	return w.pollInterval(), schedNoCycle
}

// serviceDiscoveryWake handles one "the device answered a scan_air round"
// notification. A scan answer is the earliest evidence the network has that a rig
// is back; without it an unreachable rig would sit out its whole retry interval
// while its device was plainly on the LAN, answering every round. It runs a
// full cycle, because the probe is the point, but only for a rig that is
// currently unreachable and past its wake backoff. The bool reports whether a
// cycle actually ran, so Run reschedules its ticker only for a wake that
// probed.
func (w *Worker) serviceDiscoveryWake(ctx context.Context) (next time.Duration, ran bool) {
	switch {
	case w.wasOnline:
		// Already reachable: the rig is on the poll cadence, which a wake cannot better.
		w.log.Debug("rig: device answered a discovery scan; the rig is already online")
		return 0, false
	case w.paused:
		// A paused rig runs no cycles, and a device answering is not an operator action.
		w.log.Debug("rig: device answered a discovery scan; the rig is paused")
		return 0, false
	case !w.wakeNotBefore.IsZero() && w.now().Before(w.wakeNotBefore):
		w.log.Debug("rig: device answered a discovery scan, but its wake backoff has not elapsed",
			"until", w.wakeNotBefore, "gap", w.wakeGap)
		return 0, false
	}

	w.log.Info("rig: device answered a discovery scan; probing now rather than waiting out the retry interval")
	next = w.runCycle(ctx)
	if !w.wasOnline {
		// The device answers UDP but is not reachable over SMB. Widen the gap before
		// the next wake so a reachable-but-broken rig cannot draw a probe out of every
		// discovery round for as long as it stays broken.
		w.wakeGap = w.nextWakeGap()
		w.wakeNotBefore = w.now().Add(w.wakeGap)
		w.log.Debug("rig: the woken cycle still found the rig unreachable; backing off further wakes",
			"gap", w.wakeGap)
	}
	return next, true
}

// nextWakeGap doubles the discovery-wake backoff, starting at this rig's poll
// interval and capped at its retry interval — beyond which a wake buys nothing
// over the cadence the worker already keeps.
func (w *Worker) nextWakeGap() time.Duration {
	poll, retry := 45*time.Second, 5*time.Minute
	if cfg := w.cfg.Load(); cfg != nil {
		if p := cfg.Rig(w.profileID); p != nil {
			poll, retry = cfg.EffectivePollInterval(p), cfg.EffectiveRetryInterval(p)
		} else {
			poll, retry = cfg.Defaults.PollInterval.D(), cfg.Defaults.RetryInterval.D()
		}
	}
	gap := w.wakeGap * 2
	if gap <= 0 {
		gap = poll
	}
	if gap > retry {
		gap = retry
	}
	return gap
}

// setPaused records a pause or resume and publishes it. Both delivery paths go
// through here — the command select at a cycle boundary and drainCommands between
// files — because if the two diverged, with the drain flipping the flag alone, a
// pause that arrived mid-cycle would leave a rig that ran no further cycles while
// its card still read "Working — don't power off" and its Pause button still
// offered to pause it. A pause is the same pause whichever path delivers it.
func (w *Worker) setPaused(paused bool) {
	w.paused = paused
	w.st.Paused = paused
	// A Resume or a Pause ends a run until idle; neither leaves the rig running
	// until idle after it.
	if w.runUntilIdle {
		w.setRunUntilIdle(false)
		why := "resume"
		if paused {
			why = "pause"
		}
		w.log.Info("rig: run until idle ended", "why", why)
	}
	if !paused {
		w.log.Info("rig: resumed")
		w.clearPausedPill()
		w.publish()
		return
	}
	w.log.Info("rig: paused")
	// Every delivery path of a Pause ends here, so this is where the flag Send set
	// for it retires; a later Run now while paused must hash to completion.
	w.pauseRequested.Store(false)
	w.showPaused()
	// Same reasoning for the hero signal: the verdict is only as good as the
	// listing behind it, and a paused rig runs none. Left alone it freezes on
	// whatever the last cycle said — a card reading "Working — don't power off"
	// for a rig doing nothing, or worse, "Safe to power off" while frames pile up
	// unpulled. Downgrade to Unknown, which only ever lowers confidence.
	w.setSafeVerdict(model.SafeUnknown, "paused")
	w.publish()
}

// showPaused puts the card into its paused shape without publishing: the pill,
// and every per-listing field a rig that runs no listing would otherwise freeze.
// It leaves the Safe verdict alone, which each caller sets or keeps.
func (w *Worker) showPaused() {
	w.setState(model.RigStatePaused)
	// A paused rig runs no slices, so no drain continues after this.
	w.st.Draining = false
	// A paused rig runs no cycles, so it observes no new frames; drop the
	// prediction rather than freeze a countdown that can never refresh, and clear
	// the waiting-to-settle count so it shows nothing.
	w.st.Prediction = nil
	w.st.WaitingToSettle = 0
	// A paused rig runs no listing, so its structural empty-slot set is frozen and
	// must not be trusted — clear it so the disable guard sees an empty set (it also
	// refuses a paused rig, but the published set must not go stale either way).
	w.st.EmptySlots = nil
	// This publish carries no fresh listing, so it is not authoritative for the
	// live feed — clear the flag so a stale true from the last cycle can't make the
	// client reconcile against an out-of-date pending set.
	w.st.PendingListed = false
}

// clearPausedPill takes a Paused pill down once the rig is working again, by a
// Resume or a run until idle starting, without publishing.
func (w *Worker) clearPausedPill() {
	if w.st.State != model.RigStatePaused {
		return
	}
	if w.sliceActive {
		// Served mid-slice, the slice carries on deleting, so show that now rather
		// than a Paused that would stand until the slice ends.
		w.setState(model.RigStateDeleting)
	} else if w.st.Online {
		// Outside a slice no cycle may follow at once (a Resume served by the restore
		// pass's drain, then nothing to delete), so put back what the listing
		// published rather than leave Paused until the next listing. Not on a rig
		// last seen offline: its listing is not what it is doing, and the cycle that
		// follows corrects the pill once it has probed.
		w.setState(w.restorableListedState())
	}
}

// setRunUntilIdle sets the run flag and its published mirror together, so the
// card shows a run for exactly as long as the worker runs one.
func (w *Worker) setRunUntilIdle(on bool) {
	w.runUntilIdle = on
	w.st.RunUntilIdle = on
}

// startRun starts a run until idle on a paused rig and publishes it at once, so
// the hub sees the run begin even when it ends in its first cycle. The progress
// record is re-based here, so neither the idle test nor the no-progress count
// carries over from before the run.
func (w *Worker) startRun() {
	w.setRunUntilIdle(true)
	w.runSweep = w.sweepHeldNow()
	w.runListing = w.listings
	w.noProgress = 0
	w.log.Info("rig: run-now while paused; running until idle",
		"sweeps", w.sweeps, "cursor", w.deleteCursor)
	w.clearPausedPill()
	w.publish()
}

// endRun ends a run until idle whose flag the caller has cleared, at idle or on
// a Stop run: the transition setPaused(true) applies, except that the Safe
// verdict and its listing time stay as the run's last cycle wrote them, since a
// verdict from that listing is still the best the card has. The rig stays paused.
func (w *Worker) endRun(why string) {
	w.log.Info("rig: run until idle ended", "why", why,
		"safe", w.st.SafeToPowerOff, "safe_as_of", w.st.SafeAsOf)
	w.showPaused()
	w.publish()
}

// endRunForIdentity ends a run until idle when a different device answers. The
// caller shows Error with its own Unknown verdict, which only the operator's reset
// or re-adopt clears, so no run may go on over it.
func (w *Worker) endRunForIdentity() {
	if w.runUntilIdle {
		w.setRunUntilIdle(false)
		w.settleRunRequest()
		w.log.Info("rig: run until idle ended", "why", "identity")
	}
}

// settleRunRequest records the rig as paused at a run's self-end (idle, no
// progress, offline, identity error), which no command will ever record, so the
// card reads "Paused" rather than "Paused · starting run" over a finished run. It
// changes only a request still reading run until idle: a Pause, Resume or Stop run
// queued meanwhile has already recorded what the operator asked for. A Stop run or
// Pause ends a run without it, since Send recorded paused when it queued them, and
// settling there would overwrite a Run now queued behind them.
func (w *Worker) settleRunRequest() {
	w.hub.SetRequested(w.profileID, model.RequestPaused, model.RequestRunUntilIdle)
}

// sweepHeldNow is the sweep an event outside a slice (a copy, a run's start)
// belongs to, in sweeps' terms. With the cursor at the head no sweep is open: the
// next slice starts one from the head, so the first boundary it reaches already
// completes a whole sweep after the event.
func (w *Worker) sweepHeldNow() int {
	if w.deleteCursor == (deleteCursor{}) {
		return w.sweeps - 1
	}
	return w.sweeps
}

// drainCommands applies any queued commands without blocking, so a pause or
// reload signalled mid-cycle takes effect within one file's time (design
// architecture §3). It also services a waiting dismiss request so a discard issued
// during a long pull cycle is handled within one file's time rather than at the
// cycle boundary. It is called between files in the pull and delete loops.
//
// Run now and Stop run apply as they do in apply, minus the cycle: the caller is
// already in one. A Stop run ends the run only once the queue is empty, so a Run
// now queued behind it keeps the same run going, its progress record intact,
// rather than publishing an end and starting a fresh run.
func (w *Worker) drainCommands(ctx context.Context) {
	stopped := false
	for {
		select {
		case c := <-w.cmds:
			switch c {
			case Pause:
				stopped = false // setPaused ends the run itself, with the paused verdict
				w.setPaused(true)
			case Resume:
				stopped = false
				w.setPaused(false)
			case RunNow:
				switch {
				case !w.paused || w.runUntilIdle:
					// A cycle is already in progress, or so is the run.
				case stopped:
					// The later command wins: the run goes on as if never stopped.
					stopped = false
					w.setRunUntilIdle(true)
					w.log.Info("rig: run-now behind a stop-run; the run goes on")
				default:
					w.startRun()
				}
			case StopRun:
				if w.runUntilIdle {
					w.setRunUntilIdle(false)
					stopped = true
				} else {
					w.log.Info("rig: stop-run with no run in progress; ignored")
				}
			case Reclassify:
				w.pendingReclassify = true
			case ResetIdentity:
				w.pendingResetIdentity = true
			case ReloadConfig:
				// Config is re-read at the next boundary.
			}
		case req := <-w.dismissReqs:
			req.reply <- w.discardOneGuarded(ctx, req.frameID)
		case req := <-w.restoreReqs:
			n, err := w.serviceRestore(ctx, req)
			req.reply <- restoreReply{n: n, err: err}
		default:
			if stopped {
				w.endRun("stop run")
			}
			return
		}
	}
}

// requester names this rig and its cycle's site to Admission, for the work that
// moves bytes over that site: pulls, deletes and restores.
func (w *Worker) requester(site netsite.Site) Requester {
	return Requester{Rig: w.profileID, Site: site.ID()}
}

// cycle is what one online cycle resolved: the endpoint it reaches the device
// at, that endpoint's site, and the profile it logs in with. runCycle builds one
// per cycle and passes it to each phase, so a phase's site and target are its
// arguments rather than worker state that could outlive the cycle.
type cycle struct {
	ep      netsite.Endpoint
	site    netsite.Site
	profile *model.RigProfile
}

// newCycle is the cycle reaching profile's device at ep, through the site ep
// names.
func (w *Worker) newCycle(profile *model.RigProfile, ep netsite.Endpoint) cycle {
	return cycle{ep: ep, site: w.sites.Site(ep.Site), profile: profile}
}

// workSuspended reports whether pulls and deletes should be skipped: a rig is
// suspended while paused with no run until idle in progress.
func (w *Worker) workSuspended() bool { return w.paused && !w.runUntilIdle }

// pollInterval resolves this rig's poll interval from the current config, or a
// safe fallback if the profile is gone.
func (w *Worker) pollInterval() time.Duration {
	cfg := w.cfg.Load()
	if cfg == nil {
		return 45 * time.Second
	}
	if p := cfg.Rig(w.profileID); p != nil {
		return cfg.EffectivePollInterval(p)
	}
	return cfg.Defaults.PollInterval.D()
}

// runCycle runs one full lifecycle pass and returns the delay before the next
// cycle. It recovers from any panic so a malformed header or unexpected value on
// one rig can never take down the multi-rig service (design frame-lifecycle,
// design architecture §12).
func (w *Worker) runCycle(ctx context.Context) (next time.Duration) {
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	if profile == nil || !profile.Managed {
		// This early return precedes the recover defer below, so it is counted here
		// explicitly rather than in the defer (hazard P7).
		w.metrics.RecordCycle(w.profileID, metrics.CycleUnmanaged)
		w.log.Debug("rig: profile gone or unmanaged; idling")
		w.st.Draining = false // no slice runs on an unmanaged rig
		return cfg.Defaults.PollInterval.D()
	}
	w.st.Name = profile.Name
	w.st.KeepOriginals = profile.KeepOriginals
	poll := cfg.EffectivePollInterval(profile)
	retry := cfg.EffectiveRetryInterval(profile)

	cycleStart := w.now()
	result := metrics.CycleOK

	defer func() {
		// A recovered panic leaves the named return zero; more to the point, this
		// observation lives INSIDE the recover defer so it can see the panic (a defer
		// registered after it would run first, before recover — hazard P7).
		if r := recover(); r != nil {
			result = metrics.CyclePanic
			w.log.Error("rig: cycle panic recovered", "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			w.st.Draining = false // the cycle's slice, if any, did not report a yield
			w.st.InSession = false
			w.setError(fmt.Sprintf("internal error: %v", r))
		}
		w.metrics.RecordCycle(w.profileID, result)
		w.metrics.ObserveCycleDuration(w.profileID, metrics.PhaseTotal, w.now().Sub(cycleStart).Seconds())
		if w.pendingResetIdentity {
			w.pendingResetIdentity = false
			w.resetIdentity(ctx)
		}
		if w.pendingReclassify {
			w.pendingReclassify = false
			w.runReclassify(ctx)
		}
	}()

	if ctx.Err() != nil {
		result = metrics.CycleCtxCancelled
		w.st.Draining = false
		return poll
	}

	// Library scan is NAS-only and must run before the device probe, so a rig whose
	// ASIAIR is powered off still reconciles (daytime with the mount parked is when
	// operators cull). Publishes the effective settle window for the UI deadline.
	w.st.ReconcileSettleSec = int64(cfg.EffectiveReconcileSettle(profile).Seconds())
	w.runScanIfDue(ctx, cfg, profile)

	ep, ok := w.resolveAddress(ctx, profile)
	if !ok {
		result = metrics.CycleOffline
		// No endpoint, so no remote: the rig is plainly offline.
		w.setSite(w.sites.Site(netsite.Local))
		w.handleOffline(ctx, cfg, profile)
		return retry
	}
	// Record the resolved address before the probe so an offline-after-resolve
	// cycle still publishes the last-known IP for the UI's SMB shortcut.
	w.st.Address = ep.IP
	cy := w.newCycle(profile, ep)
	w.setSite(cy.site)
	if remoteTunnelDown(cy.site) {
		result = metrics.CycleOffline
		w.handleRemoteDown(ctx, cfg, profile)
		return retry
	}
	dev := w.newDevice(ep, profile.User, profile.Password, smbTimeout(cy.site))
	if !dev.Probe(ctx) {
		result = metrics.CycleOffline
		w.handleOffline(ctx, cfg, profile)
		return retry
	}
	w.markOnline(ctx)
	w.setInSession(true)

	// The pull and delete phases' device failures feed the stalled-progress
	// count; the wrappers only observe, so both phases run exactly as they would
	// on dev itself.
	var pullFails, deleteFails linkFailures
	pullStart := w.now()
	pull := w.pullPhase(ctx, cfg, cy, watchDevice(dev, &pullFails))
	w.metrics.ObserveCycleDuration(w.profileID, metrics.PhasePull, w.now().Sub(pullStart).Seconds())
	// A copy happens before this cycle's slice, so it belongs to the sweep open now.
	copySweep := w.sweepHeldNow()
	// A local, not the named return: a panic recovered below must keep returning
	// zero, as it always has.
	delay := poll
	draining := false
	var pass slicePass
	deleted := false
	if !pull.refused && ctx.Err() == nil && !w.workSuspended() {
		// Restore runs BEFORE delete so a queued restore's device original is never
		// destroyed in the same cycle; it is not gated on KeepOriginals.
		w.restorePhase(ctx, cfg, cy, dev)
		if profile.KeepOriginals {
			w.log.Debug("rig: keep-originals set; skipping delete pass")
			if ctx.Err() == nil && !w.workSuspended() {
				// Nothing on this rig is ever deleted, so the delete side is a whole
				// sweep by itself: a drain left open when KeepOriginals was set ends.
				w.sweeps++
			}
		} else if ctx.Err() == nil && !w.workSuspended() {
			// drainDone counts every delete of the drain and is reset only by
			// endDrainIfSwept below, so its growth across the slice is this slice's.
			before := w.drainDone
			pass = w.deleteSlice(ctx, cfg, cy, watchDevice(dev, &deleteFails), pullStart, poll)
			deleted = w.drainDone > before
			if pass.sweepDone {
				w.sweeps++
			}
			if pass.more {
				// A yield happens only once the deadline has passed, and every deadline is
				// at least pullStart + poll, so the next listing is already due.
				delay = minPollInterval
				draining = true
			}
		}
		w.endDrainIfSwept()
		// Last, and deliberately not gated on KeepOriginals — setting a clock is not
		// deleting a frame. It runs after the SMB session has closed, so the two
		// connections never overlap, and after every device-time comparison this
		// cycle made, so stepping the clock cannot invalidate them mid-cycle.
		if ctx.Err() == nil && !w.workSuspended() {
			w.clockPhase(ctx, cy)
		}
	}
	if pull.listed {
		w.listings++
	}
	if w.runUntilIdle && w.trackRun(pull, pass, copySweep, deleted) {
		// The rig is back to being simply paused: no continuation, and no drain
		// indication on a rig that runs no cycle.
		delay = poll
		draining = false
	}
	// A foreign device is an identity error with its own alert, not a stall.
	pullFailed := pullFails.count() > 0 || (pull.refused && w.st.State != model.RigStateError)
	deleteFails.addFrames(pass.hashFailed)
	if pass.unconfirmed {
		deleteFails.fail()
	}
	w.trackStall(ctx, cy, pull.copied || deleted, pullFailed, &deleteFails)
	// Every online cycle ends here, a refused or suspended one included, so the
	// countdown is stamped from the delay actually returned, after the delete slice
	// rather than before it, and the draining indication reflects this cycle alone.
	w.st.Draining = draining
	w.st.InSession = false
	w.stampNextPoll(delay, poll)
	w.publish()
	return delay
}

// trackRun keeps a run until idle's progress record after one of its cycles, and
// ends the run once it is idle or has stopped making progress, reporting whether
// it ended. It is idle (design frame-lifecycle I2) once, since its last copy or
// delete, a whole sweep of the delete cursor has run and a cycle has listed and
// attempted a pull: every remaining frame has been tried and nothing is ready to
// copy. A slice returning more == false is not idle by itself, since a prologue
// failure, a suspend or an unavailable destination return it too. copySweep is
// the sweep a copy in this cycle's pull phase belongs to; deleted reports that
// this cycle's slice deleted a frame, the last of them held by drainSweep.
func (w *Worker) trackRun(pull pullOutcome, pass slicePass, copySweep int, deleted bool) (ended bool) {
	switch {
	case deleted:
		w.runSweep, w.runListing = w.drainSweep, w.listings
	case pull.copied:
		w.runSweep, w.runListing = copySweep, w.listings
	}
	// A skip-budget yield advances the cursor, so it is progress: the slice moved
	// past cheap exits toward rows it can stream. Behind a listing that failed, a
	// cursor advance alone is not: a frame that defers on every visit moves the
	// cursor each cycle, and would keep the run going for as long as the fault lasts
	// with no listing ever completing. There only a copy or a delete counts.
	advanced := pass.advanced && !pull.listFailed
	if pull.copied || deleted || advanced {
		w.noProgress = 0
	} else {
		w.noProgress++
	}
	idle := w.wholeSweepSince(w.runSweep) && w.listings > w.runListing
	w.log.Debug("rig: run until idle progress", "idle", idle, "no_progress", w.noProgress, "list_failed", pull.listFailed,
		"sweeps", w.sweeps, "run_sweep", w.runSweep, "listings", w.listings, "run_listing", w.runListing)
	switch {
	case idle:
		w.setRunUntilIdle(false)
		w.settleRunRequest()
		w.endRun("idle")
	case w.noProgress >= maxNoProgressCycles:
		w.setRunUntilIdle(false)
		w.settleRunRequest()
		w.endRun("no progress")
	default:
		return false
	}
	return true
}

// deleteSlice runs one delete slice with the deadline design frame-lifecycle §12.1
// gives, and keeps the carried debt that deadline reads. When the device
// has room the slice ends one poll after this cycle's listing started. When it is
// tight, deletes get a fair share of link time: this cycle's listing, pull and
// restore time accrues as debt, and the slice runs for that debt, at least one
// poll and at most two, so one long pull phase is repaid over several slices.
func (w *Worker) deleteSlice(ctx context.Context, cfg *model.Config, cy cycle, dev device.Device, pullStart time.Time, poll time.Duration) slicePass {
	deadline := pullStart.Add(poll) // next listing is due one poll after this one started
	sliceStart := w.now()
	if w.deviceTight {
		// Every frame's space is freed only after two full device reads (the pull and
		// the delete re-hash). When the link cannot carry twice the capture rate the
		// device fills under any schedule, and the freeing rate is maximised by
		// splitting link time about 1:1 between pulls and deletes. The debt carries
		// that split across cycles, so one long pull phase is repaid over several
		// slices of at most 2×poll instead of one slice as long as the pull.
		w.deleteDebt += sliceStart.Sub(pullStart) // listing + pull + restore this cycle
		deadline = sliceStart.Add(min(max(poll, w.deleteDebt), 2*poll))
	} else {
		w.deleteDebt = 0
	}
	w.log.Debug("rig: delete slice starting", "cursor", w.deleteCursor, "deadline", deadline,
		"delete_debt", w.deleteDebt, "device_tight", w.deviceTight)
	pass := w.deletePhase(ctx, cfg, cy, dev, deadline)
	w.metrics.ObserveCycleDuration(w.profileID, metrics.PhaseDelete, w.now().Sub(sliceStart).Seconds())
	// The slice's own length repays debt; only the device leaving tight or the list
	// running out forgives the rest. A prologue return, a suspend, a ctx end or an
	// unavailable destination leaves it owed, so a missed identity probe or a Pause
	// does not cancel the fair share.
	if w.deviceTight {
		w.deleteDebt = max(0, w.deleteDebt-w.now().Sub(sliceStart))
	}
	if pass.exhausted { // the whole eligible list was iterated, or it was empty
		w.deleteDebt = 0 // nothing left to repay
	}
	return pass
}

// sweepHolding is the sweep an event happening now belongs to, in sweeps' terms.
// pastWrap reports that the slice in progress has already passed the tail, closing
// a sweep that runCycle counts only once the slice returns.
func (w *Worker) sweepHolding(pastWrap bool) int {
	if pastWrap {
		return w.sweeps + 1
	}
	return w.sweeps
}

// wholeSweepSince reports that a whole sweep has run since an event recorded as
// held (sweepHolding): the sweep count has advanced by two, the first boundary
// only closing the sweep that held the event.
func (w *Worker) wholeSweepSince(held int) bool { return w.sweeps >= held+2 }

// endDrainIfSwept ends the drain once a whole sweep has run since its last
// delete, so frames that defer on every visit do not hold the cleanup readout
// open; the next drain's readout starts again from 1.
func (w *Worker) endDrainIfSwept() {
	if w.drainDone > 0 && w.wholeSweepSince(w.drainSweep) {
		w.log.Debug("rig: delete drain ended; a whole sweep deleted nothing", "deleted", w.drainDone)
		w.drainDone = 0
	}
}

// handleOffline records an unreachable rig: the cycle is skipped silently (an
// absent rig is a normal state, not an error), but a rig unreachable longer than
// unreachable_alert_after raises AlertUnreachable once (design frame-lifecycle
// §1, design architecture §10).
func (w *Worker) handleOffline(ctx context.Context, cfg *model.Config, profile *model.RigProfile) {
	nowT := w.now()
	if w.wasOnline || w.unreachableSince.IsZero() {
		w.unreachableSince = nowT
	}
	if w.wasOnline {
		w.hub.Emit(hub.Event{Name: "rig.offline", Data: map[string]any{"rig_id": w.profileID}})
	}
	w.wasOnline = false

	after := cfg.Defaults.UnreachableAlertAfter.D()
	if after > 0 && !w.unreachableAlerted && nowT.Sub(w.unreachableSince) >= after {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertUnreachable,
			Message:  fmt.Sprintf("rig %q unreachable for over %s", profile.Name, after),
			Standing: true,
		})
		w.unreachableAlerted = true
	}
	w.goOffline(cfg, profile, "offline")
	w.log.Debug("rig: unreachable; skipping cycle")
	w.publish()
}

// goOffline sets the status of a cycle that could not reach the device; reason
// says why its Safe verdict is Unknown. The caller publishes.
func (w *Worker) goOffline(cfg *model.Config, profile *model.RigProfile, reason string) {
	w.setState(model.RigStateOffline)
	w.st.Online = false
	// An offline rig runs no slice, so no drain continues on it.
	w.st.Draining = false
	// An offline rig's power-off status is unknowable (design architecture §8).
	w.setSafeVerdict(model.SafeUnknown, reason)
	if w.runUntilIdle {
		// A run on a paused rig ends here, back at Paused rather than Offline: no later
		// cycle runs on the paused rig to report it back online, so an Offline pill
		// would stand until the operator acted. The verdict says why it is unknown,
		// and Resume or Run now checks the device again.
		w.setRunUntilIdle(false)
		w.settleRunRequest()
		w.log.Info("rig: run until idle ended", "why", "offline")
		w.showPaused()
		w.setSafeVerdict(model.SafeUnknown, reasonOfflineDuringRun)
	}
	// No fresh captures arrive while offline; drop any stale next-frame prediction
	// and waiting-to-settle count.
	w.st.Prediction = nil
	w.st.WaitingToSettle = 0
	// An offline rig ran no listing this cycle, so its structural empty-slot set is
	// stale and must not linger — clear it so the disable guard never trusts an
	// offline rig's slots (it also requires Online, but the published set stays honest).
	w.st.EmptySlots = nil
	// Not a fresh listing — clear the live-feed authority flag so the client never
	// reconciles against the last cycle's now-stale pending set.
	w.st.PendingListed = false
	w.stampNextPoll(cfg.EffectiveRetryInterval(profile), cfg.EffectiveRetryInterval(profile))
}

// markOnline records a rig that answered the probe, emitting rig.online on a
// transition and clearing the unreachable timer.
func (w *Worker) markOnline(ctx context.Context) {
	if !w.wasOnline {
		w.hub.Emit(hub.Event{Name: "rig.online", Data: map[string]any{"rig_id": w.profileID}})
		// The rig is reachable again, so a standing "unreachable" alert is resolved.
		// Auto-ack it (like the identity/media/unclassified alerts do on resolve) so it
		// clears from the badge instead of lingering until the operator dismisses it.
		w.resolveAlerts(ctx, model.AlertUnreachable)
	}
	w.wasOnline = true
	w.unreachableSince = time.Time{}
	w.unreachableAlerted = false
	// The offline episode is over, so the next one starts with a free discovery
	// wake rather than serving out a backoff this rig has already grown out of.
	w.wakeNotBefore = time.Time{}
	w.wakeGap = 0
	w.st.Online = true
	w.st.LastError = ""
	if w.st.State == model.RigStateOffline {
		// The listing walk keeps the prior pill until a copy starts, so an Offline
		// left from the last cycle would stand over a rig that is listing. Put back
		// what the last listing read and publish it before the walk begins. Error is
		// left alone: an identity error stands until the operator resets it. A
		// suspended rig lists without working, so its pill is Paused, not the
		// last listing's activity.
		restored := w.restorableListedState()
		if w.workSuspended() {
			restored = model.RigStatePaused
		}
		w.setState(restored)
		w.log.Debug("rig: back online; pill restored from the last listing", "state", w.st.State)
		w.publish()
	}
}

// resolveAddress picks the endpoint to connect to for a rig this cycle (design
// device-identity-and-media §3.2), trying in order: the registry by guid (with a
// cpuId fallback inside Resolve); a cached last-known-good endpoint; the legacy
// Host, on the local site, resolved to its first IPv4 (or the literal Host if it
// does not resolve to IPv4). Every candidate is corroborated by the live
// scan_air identity probe once the session opens, through the same site, so
// resolveAddress only needs to return the best-guess endpoint.
func (w *Worker) resolveAddress(ctx context.Context, profile *model.RigProfile) (ep netsite.Endpoint, ok bool) {
	if profile.GUID != "" && w.resolver != nil {
		if ep, online := w.resolver.Resolve(profile.GUID, profile.CPUID); online && ep.IP != "" {
			return ep, true
		}
	}
	if ep, ok := w.lastKnownIP(ctx); ok {
		return ep, true
	}
	if profile.Host != "" {
		if ip := w.firstIPv4(profile.Host); ip != "" {
			return netsite.LocalEndpoint(ip), true
		}
		// Doesn't resolve to an IPv4 yet: hand back the literal Host. Migration
		// correlation (design device-identity-and-media §3.4) simply waits until it
		// does.
		return netsite.LocalEndpoint(profile.Host), true
	}
	return netsite.Endpoint{}, false
}

// lastKnownIP reads the rig's cached last-known-good device endpoint. A legacy
// bare value names the local site. A value that does not parse is skipped
// rather than dialled anywhere: guessing its network is exactly the wrong-device
// hazard the stored site exists to close.
func (w *Worker) lastKnownIP(ctx context.Context) (netsite.Endpoint, bool) {
	v, ok, err := w.store.GetMeta(ctx, LastIPKey(w.profileID))
	if err != nil || !ok || v == "" {
		return netsite.Endpoint{}, false
	}
	ep, err := netsite.Parse(v)
	if err != nil {
		w.log.Warn("rig: ignoring unreadable last-known endpoint", "value", v, "error", err)
		return netsite.Endpoint{}, false
	}
	return ep, true
}

// firstIPv4 resolves host and returns its first IPv4 literal, or "" if the
// lookup fails or yields no IPv4 (the registry and unicast probe are IPv4).
func (w *Worker) firstIPv4(host string) string {
	if net.ParseIP(host) != nil {
		// Already a literal IP: only use it if it is IPv4.
		if ip := net.ParseIP(host); ip.To4() != nil {
			return host
		}
		return ""
	}
	lookup := w.lookupHost
	if lookup == nil {
		lookup = net.LookupHost
	}
	addrs, err := lookup(host)
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a
		}
	}
	return ""
}
