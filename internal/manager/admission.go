// Package manager is the RigManager supervisor and the global transfer admission
// gate (design architecture §3, §7). The supervisor owns one Worker goroutine
// per managed rig behind a guarded map so config edits and operator commands
// from HTTP handlers never touch the worker set concurrently — the one
// structural defence against a fatal concurrent-map write. The admission gate is
// the single chokepoint every transfer passes through: a concurrency slot chosen
// by its Policy plus the disk_reserve pull-gate on the destination volume, with
// in-flight reserve accounting so several admissions granted at once can never
// collectively overrun the reserve.
package manager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// ErrDiskReserve is returned by Acquire when the destination volume does not hold
// room for the projected pull beyond disk_reserve_bytes. It is a soft skip, not a
// hard failure: pulls PAUSE below the reserve (design architecture §12) and the
// worker treats this sentinel as "skip this frame, try again next cycle" rather
// than a frame error. The concurrency slot is released before it is returned, so
// a denied acquire consumes nothing.
//
// The canonical sentinel lives in package rig (beside ErrDestinationUnavailable) so
// the worker can errors.Is against it without importing manager; this is an alias so
// existing manager.ErrDiskReserve references keep working.
var ErrDiskReserve = rig.ErrDiskReserve

// Policy decides when a requester may start a transfer. Wait blocks, honoring
// ctx, until req holds a slot, and returns the function that gives the slot
// back; Admission calls it exactly once. The disk reserve is not the policy's:
// it is one gate Admission applies to every rig after the policy admits.
//
// Deciding by site is admission's to do (design remote-sites §12 invariant 12);
// the rig only names its site, so a fairness-based policy can replace siteSlots
// without a caller changing.
type Policy interface {
	Wait(ctx context.Context, req rig.Requester) (release func(), err error)
}

// siteSlots is the first policy: work on the local site shares the global cap
// (max_concurrent_transfers, FIFO across rigs), and every other site has one
// slot of its own outside that cap, so one remote's narrow link carries one
// transfer at a time and never holds a LAN rig's slot while it trickles.
type siteSlots struct {
	global *semaphore.Weighted

	mu    sync.Mutex
	sites map[netsite.ID]*semaphore.Weighted
}

// newSiteSlots builds the policy; maxConcurrent below 1 is clamped to 1 so the
// local site always admits at least one transfer.
func newSiteSlots(maxConcurrent int) *siteSlots {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &siteSlots{
		global: semaphore.NewWeighted(int64(maxConcurrent)),
		sites:  map[netsite.ID]*semaphore.Weighted{},
	}
}

// slot is the semaphore req waits on. A site's slot is created at its first
// request and kept: sites are few, and a slot dropped while held would let a
// second transfer onto the site.
func (p *siteSlots) slot(req rig.Requester) *semaphore.Weighted {
	// Any site other than the local one gets its own one-transfer slot, a
	// supervised remote or not.
	if req.Site == netsite.Local {
		return p.global
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sites[req.Site]
	if !ok {
		s = semaphore.NewWeighted(1)
		p.sites[req.Site] = s
	}
	return s
}

func (p *siteSlots) Wait(ctx context.Context, req rig.Requester) (func(), error) {
	sem := p.slot(req)
	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() { sem.Release(1) }, nil
}

// Admission is the transfer gate implementing rig.Admission (design
// architecture §3 concurrency + design architecture §7 disk_reserve + in-flight
// reserve accounting). One instance is shared by every Worker; its Policy picks
// the slot each request waits for, and the mutex-guarded inFlightBytes makes
// the one reserve check, shared by every rig on every site, correct under
// concurrent admissions.
type Admission struct {
	policy           Policy
	lfs              localfs.LocalFS
	stagingRoot      string
	diskReserveBytes int64

	mu            sync.Mutex
	inFlightBytes int64

	// Mirrors of the in-flight accounting for lock-free scrape reads. The metrics
	// state collector must never take a.mu — Acquire holds it across FreeSpace, so a
	// scrape reading under the lock would block behind a stalled NAS (hazard P2).
	// These are written at the same two points as inFlightBytes and read without the
	// lock by InFlight. inFlightCount also serves as the semaphore-occupancy gauge,
	// which x/sync/semaphore cannot report directly (hazard P9).
	inFlightBytesAtomic atomic.Int64
	inFlightCount       atomic.Int64
}

// InFlight reports the number of transfers currently holding an admission
// slot, on any site, and the bytes they have reserved. Lock-free: it reads
// mirrored atomics, never a.mu.
func (a *Admission) InFlight() (count int, bytes int64) {
	return int(a.inFlightCount.Load()), a.inFlightBytesAtomic.Load()
}

// NewAdmission builds the gate with the siteSlots policy. maxConcurrent sizes
// the local site's FIFO semaphore (max_concurrent_transfers, apply-on-restart —
// the manager never resizes it); values below 1 are clamped to 1 so the gate
// always admits at least one local pull.
// stagingRoot is the path whose volume the disk_reserve is measured against
// (staging/light/cal share one volume by config validation, design architecture
// §7).
func NewAdmission(maxConcurrent int, lfs localfs.LocalFS, stagingRoot string, diskReserveBytes int64) *Admission {
	return &Admission{
		policy:           newSiteSlots(maxConcurrent),
		lfs:              lfs,
		stagingRoot:      stagingRoot,
		diskReserveBytes: diskReserveBytes,
	}
}

// Acquire blocks on the slot the policy picks for req honoring ctx, then admits
// the transfer only if the destination volume has room for projectedSize
// beyond the reserve, accounting for bytes already reserved by other in-flight
// pulls. It returns a release closure that MUST be called exactly once at the
// frame's terminal outcome; the closure is idempotent so a double call (defer
// plus explicit) is a no-op. On any error the returned release is nil and the
// caller must not call it (the slot and reserve are already given back).
func (a *Admission) Acquire(ctx context.Context, req rig.Requester, projectedSize int64) (release func(), err error) {
	// 1. The policy's concurrency slot. A cancelled ctx (shutdown / pause) aborts
	// the queue.
	releaseSlot, err := a.policy.Wait(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("manager: admission blocked: %w", err)
	}

	// 2. Disk-reserve pull-gate with in-flight accounting.
	a.mu.Lock()
	free, _, ferr := a.lfs.FreeSpace(a.stagingRoot)
	if ferr != nil {
		a.mu.Unlock()
		releaseSlot()
		return nil, fmt.Errorf("%w: %w", rig.ErrDestinationUnavailable, ferr)
	}
	if free-a.inFlightBytes-projectedSize < a.diskReserveBytes {
		a.mu.Unlock()
		releaseSlot()
		return nil, ErrDiskReserve
	}
	a.inFlightBytes += projectedSize
	a.inFlightBytesAtomic.Add(projectedSize)
	a.inFlightCount.Add(1)
	a.mu.Unlock()

	// 3. Idempotent release: give back the reserve, then the slot.
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.inFlightBytes -= projectedSize
			a.inFlightBytesAtomic.Add(-projectedSize)
			a.inFlightCount.Add(-1)
			a.mu.Unlock()
			releaseSlot()
		})
	}, nil
}
