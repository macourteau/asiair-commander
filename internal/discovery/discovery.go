// Package discovery finds ASIAIR devices on the LAN via the UDP scan_air
// broadcast protocol and maintains a GUID-keyed registry of what answered
// (design device-identity-and-media §2.1). A Scan broadcasts one scan_air
// round and merges the replies into a registry keyed by the stable device guid
// (with a cpuId secondary index), refreshed on a timer and on demand. The registry answers three questions with
// no network I/O on the hot path: Discovered() (the UI list),
// Resolve(guid,cpuId) (a rig worker's current address for its device), and
// Managed/Online flags. ProbeGUID and DeviceShares are the only
// network-touching calls, used at identity-confirmation and adopt time
// respectively.
package discovery

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

const (
	// guestUser and guestPass are the ASIAIR's fixed guest credentials: the
	// guest share accepts any password, so this constant one is used for the
	// on-demand adopt-time share listing.
	guestUser = "guest"
	guestPass = "12345678"

	// stalenessWindow is how long since a device's last reply it is still
	// considered online (and its registry address trusted without a cpuId
	// fallback).
	stalenessWindow = 135 * time.Second
	// forgetWindow is how long an unmanaged device may go unseen before it is
	// evicted from the registry entirely.
	forgetWindow = 10 * time.Minute
	// backgroundInterval is RunBackground's period when the caller passes <= 0.
	backgroundInterval = 45 * time.Second
	// maxEntries caps the registry size against a reply flood; managed guids are
	// exempt from both the cap and eviction.
	maxEntries = 256
)

// errUnresolved is returned by DeviceShares when the guid does not resolve to an
// address (offline, never seen).
var errUnresolved = errors.New("discovery: device is not reachable right now")

// sighting is a device's last scan_air reply through one site, and when it
// arrived.
type sighting struct {
	scannedDevice
	LastSeen time.Time
}

// entry is one registry record: the device's sighting on every site that has
// seen it, the path (site) its address currently resolves through, and when it
// was first seen. paths always holds at least one sighting.
type entry struct {
	FirstSeen time.Time
	path      netsite.ID
	paths     map[netsite.ID]sighting
}

// current is the sighting on the device's current path.
func (e entry) current() sighting { return e.paths[e.path] }

// lastSeen is the most recent sighting on any path, which is what the forget
// window and the size cap age by.
func (e entry) lastSeen() time.Time {
	var last time.Time
	for _, p := range e.paths {
		if p.LastSeen.After(last) {
			last = p.LastSeen
		}
	}
	return last
}

// fresh reports whether a sighting is within the staleness window.
func fresh(p sighting, now time.Time) bool { return now.Sub(p.LastSeen) < stalenessWindow }

// RigWaker is told, by rig id, that a managed rig's device answered a scan_air
// round. Discovery knows nothing about workers or poll cadences: it hands back
// the rig id its own config match produced — the same match that fills a
// DiscoveredRig's RigID — and the side that owns the workers decides whether the
// answer is worth acting on. Declaring the interface here, at the call site,
// keeps discovery free of any import of rig or manager.
type RigWaker interface {
	WakeRig(rigID string)
}

// wakerBox wraps the interface so it can live in an atomic.Pointer, which an
// interface value cannot do directly.
type wakerBox struct{ w RigWaker }

// Scanner is the GUID-keyed discovery registry. It is safe for concurrent use;
// the registry mutex never wraps network I/O.
type Scanner struct {
	tr        transport
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device
	cfg       *atomic.Pointer[model.Config]
	now       func() time.Time
	logger    *slog.Logger

	// waker is notified of every managed device that answers a round. It is
	// injected after construction — the manager needs the Scanner as its resolver,
	// so the Scanner is built first — while background rounds may already be
	// running, hence the atomic rather than a plain field.
	waker atomic.Pointer[wakerBox]

	mu      sync.Mutex
	entries map[string]entry  // key: guid
	byCPU   map[string]string // cpuId -> guid, non-empty cpuIds only
}

// New returns a Scanner. tr is the UDP transport (the real caller passes
// newUDPTransport). newDevice builds a device.Device for the on-demand
// adopt-share listing only — never on the resolve/registry hot path. cfg is the
// live config, read fresh on every merge so a rig's managed guid/cpuId set is
// current. now supplies wall time (injectable for tests). A nil logger uses
// slog.Default.
func New(
	tr transport,
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device,
	cfg *atomic.Pointer[model.Config],
	now func() time.Time,
	logger *slog.Logger,
) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Scanner{
		tr:        tr,
		newDevice: newDevice,
		cfg:       cfg,
		now:       now,
		logger:    logger,
		entries:   map[string]entry{},
		byCPU:     map[string]string{},
	}
}

// NewDefault builds a Scanner with the real transport, whose broadcast rounds
// fan out over every site and whose identity probes go through the endpoint's
// site — the composition root's constructor.
func NewDefault(
	sites siteSet,
	newDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device,
	cfg *atomic.Pointer[model.Config],
	now func() time.Time,
	logger *slog.Logger,
) *Scanner {
	return New(newUDPTransport(sites, logger), newDevice, cfg, now, logger)
}

// SetWaker installs the notifier that learns which managed rigs answered a
// round; nil clears it. The composition root calls it once the manager exists.
func (s *Scanner) SetWaker(w RigWaker) {
	if w == nil {
		s.waker.Store(nil)
		return
	}
	s.waker.Store(&wakerBox{w: w})
}

// Scan runs one scan_air round on every up site (the transport call is made
// WITHOUT the lock held) and merges the replies into the registry under the
// lock. It returns the current discovered list.
func (s *Scanner) Scan(ctx context.Context) []model.DiscoveredRig {
	return s.scan(ctx, false)
}

// ScanOnDemand is Scan for a round an operator asked for — the Scan button, or
// opening the devices page. It logs every reply verbatim and then every entry
// the returned list carries, because the question those rounds exist to answer
// is "what is that device?", and the registry retains devices that did not
// answer this round. The background timer uses Scan instead: the same lines
// every 45 seconds would bury everything else in the log.
func (s *Scanner) ScanOnDemand(ctx context.Context) []model.DiscoveredRig {
	return s.scan(ctx, true)
}

func (s *Scanner) scan(ctx context.Context, verbose bool) []model.DiscoveredRig {
	replies, err := s.tr.Broadcast(ctx)
	if err != nil {
		s.logger.Warn("discovery: broadcast failed", "error", err)
	}
	s.merge(replies)
	s.wakeAnswered(replies)
	found := s.Discovered()
	if !verbose {
		s.logger.Debug("discovery: scan complete", "replies", len(replies))
		return found
	}

	for _, d := range replies {
		s.logger.Info("discovery: device answered",
			"guid", d.GUID, "cpu_id", d.CPUID, "model", d.Model, "site", string(d.Site),
			"address", d.Address, "reply", d.Raw)
	}
	// Anything in the list that did not answer is retained from an earlier round:
	// unmanaged for ten minutes, managed indefinitely. Naming those explicitly is
	// what keeps the count and the list from looking like a contradiction.
	for _, d := range found {
		if d.Online {
			continue
		}
		s.logger.Info("discovery: device did not answer, retained from an earlier scan",
			"guid", d.GUID, "site", d.Site, "address", d.Address, "managed", d.Managed, "rig", d.RigID)
	}
	s.logger.Info("discovery: scan complete",
		"answered", len(replies), "listed", len(found))
	return found
}

// wakeAnswered tells the waker about every managed rig whose device answered
// this round, at most once per rig. A device answering is the earliest evidence
// the network has that a rig is back; without it, a worker that found the rig
// unreachable sits out its whole retry interval (five minutes by default) while
// the device is plainly on the LAN.
//
// It runs after merge and WITHOUT the registry lock held: the waker reaches into
// the manager, which takes its own lock, and discovery must never hand out a
// callback from under s.mu.
func (s *Scanner) wakeAnswered(replies []scannedDevice) {
	box := s.waker.Load()
	if box == nil || len(replies) == 0 {
		return
	}
	var cfg *model.Config
	if s.cfg != nil {
		cfg = s.cfg.Load()
	}
	woken := map[string]bool{}
	for _, d := range replies {
		managed, rigID := matchManaged(cfg, d.GUID, d.CPUID)
		if !managed || rigID == "" || woken[rigID] {
			continue
		}
		woken[rigID] = true
		s.logger.Debug("discovery: managed device answered; waking its rig",
			"guid", d.GUID, "rig", rigID, "address", d.Address)
		box.w.WakeRig(rigID)
	}
}

// merge folds a round's replies into the registry, each as a sighting on the
// site it arrived through, applies the path rule to every device, and enforces
// the size/age bounds. It holds the lock but does no network I/O.
func (s *Scanner) merge(replies []scannedDevice) {
	now := s.now()
	managedGUIDs, managedCPUs := s.managedSets()

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, d := range replies {
		e, ok := s.entries[d.GUID]
		if !ok {
			// A device's first path is simply the first site that saw it.
			e = entry{FirstSeen: now, path: d.Site, paths: map[netsite.ID]sighting{}}
		}
		e.paths[d.Site] = sighting{scannedDevice: d, LastSeen: now}
		s.entries[d.GUID] = e
		if d.CPUID != "" {
			s.byCPU[d.CPUID] = d.GUID
		}
	}

	for guid := range s.entries {
		s.selectPathLocked(guid, now)
	}

	// Evict devices unseen past forgetWindow, exempting any guid/cpuId a managed
	// profile currently binds.
	for guid, e := range s.entries {
		cpu := e.current().CPUID
		if managedGUIDs[guid] || (cpu != "" && managedCPUs[cpu]) {
			continue
		}
		if now.Sub(e.lastSeen()) >= forgetWindow {
			s.removeLocked(guid)
		}
	}

	// Cap the registry: drop the oldest-seen unmanaged entries until at or under
	// maxEntries. Managed guids are never cap-dropped.
	for len(s.entries) > maxEntries {
		oldestGUID, oldest := "", time.Time{}
		for guid, e := range s.entries {
			cpu := e.current().CPUID
			if managedGUIDs[guid] || (cpu != "" && managedCPUs[cpu]) {
				continue
			}
			if last := e.lastSeen(); oldestGUID == "" || last.Before(oldest) {
				oldestGUID, oldest = guid, last
			}
		}
		if oldestGUID == "" {
			break // everything left is managed
		}
		s.removeLocked(oldestGUID)
	}
}

// removeLocked drops a guid entry and its cpuId index. Caller holds s.mu.
func (s *Scanner) removeLocked(guid string) {
	if e, ok := s.entries[guid]; ok {
		for _, p := range e.paths {
			if p.CPUID != "" && s.byCPU[p.CPUID] == guid {
				delete(s.byCPU, p.CPUID)
			}
		}
	}
	delete(s.entries, guid)
}

// pathSwitchRatio is the path rule's hysteresis margin: a fresh path takes
// over from the current fresh one only when its minimum recent latency is less
// than 1/pathSwitchRatio of the current path's. Paths that differ in kind —
// local against tunnelled, direct WireGuard against DERP-relayed — differ by
// multiples, while two paths of the same kind differ by tens of percent, which
// is not worth a move: every session re-probes identity on its own path, so a
// switch is safe but restarts the rig's sessions on a new network. A ratio
// rather than an absolute margin keeps the local site (latency 0) winning over
// any remote, however close that remote is.
const pathSwitchRatio = 2

// selectPathLocked applies the path rule to one device and stores the result:
// a fresh current path is kept unless another fresh path is clearly lower in
// minimum recent latency; a stale current path gives way to the best fresh one.
// Non-current sightings older than forgetWindow are pruned first. A switch is
// logged once, here. Caller holds s.mu.
func (s *Scanner) selectPathLocked(guid string, now time.Time) entry {
	e := s.entries[guid]
	for id, p := range e.paths {
		if id != e.path && now.Sub(p.LastSeen) >= forgetWindow {
			delete(e.paths, id)
		}
	}
	next, reason := choosePath(e, now)
	if next == e.path {
		return e
	}
	from, hadFrom := e.paths[e.path]
	to := e.paths[next]
	attrs := []any{"guid", guid, "to_site", string(next), "to_address", to.Address,
		"to_min_latency", to.PathLatency, "reason", reason}
	if hadFrom {
		attrs = append(attrs, "from_site", string(e.path), "from_address", from.Address,
			"from_min_latency", from.PathLatency)
	} else {
		attrs = append(attrs, "from_site", string(e.path))
	}
	s.logger.Info("discovery: device path switched", attrs...)
	e.path = next
	s.entries[guid] = e
	return e
}

// choosePath is the path rule as a pure function of the device's sightings:
// it returns the path the device should resolve through and, when that is not
// the current one, why.
func choosePath(e entry, now time.Time) (netsite.ID, string) {
	best, found := bestFreshPath(e, now)
	cur, hasCur := e.paths[e.path]
	switch {
	case !hasCur:
		// The current path's sightings were dropped (its site was removed).
		if found {
			return best, "current path removed"
		}
		return mostRecentPath(e), "current path removed"
	case !fresh(cur, now):
		if found {
			return best, "current path stale"
		}
		return e.path, ""
	case found && pathSwitchRatio*e.paths[best].PathLatency < cur.PathLatency:
		return best, "clearly lower latency"
	}
	return e.path, ""
}

// bestFreshPath returns the fresh path, other than the current one, with the
// lowest minimum recent latency; ties go to the most recently seen, then to
// the lower site id, so the choice never depends on map order.
func bestFreshPath(e entry, now time.Time) (netsite.ID, bool) {
	var best netsite.ID
	found := false
	for id, p := range e.paths {
		if id == e.path || !fresh(p, now) {
			continue
		}
		if found {
			b := e.paths[best]
			switch {
			case p.PathLatency > b.PathLatency:
				continue
			case p.PathLatency == b.PathLatency && p.LastSeen.Before(b.LastSeen):
				continue
			case p.PathLatency == b.PathLatency && p.LastSeen.Equal(b.LastSeen) && id > best:
				continue
			}
		}
		best, found = id, true
	}
	return best, found
}

// mostRecentPath returns the most recently seen path, ties to the lower id.
func mostRecentPath(e entry) netsite.ID {
	var best netsite.ID
	found := false
	for id, p := range e.paths {
		if found {
			b := e.paths[best]
			if p.LastSeen.Before(b.LastSeen) || (p.LastSeen.Equal(b.LastSeen) && id > best) {
				continue
			}
		}
		best, found = id, true
	}
	return best
}

// DropSite removes every sighting made through site id. A device seen only
// there leaves the registry, managed or not; one also seen elsewhere moves to
// its best remaining path. Removing a site calls it, so nothing resolves to a
// site that no longer exists.
func (s *Scanner) DropSite(id netsite.ID) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	dropped, removed := 0, 0
	for guid, e := range s.entries {
		if _, ok := e.paths[id]; !ok {
			continue
		}
		dropped++
		if len(e.paths) == 1 {
			s.removeLocked(guid)
			removed++
			continue
		}
		delete(e.paths, id)
		s.selectPathLocked(guid, now)
	}
	s.logger.Info("discovery: dropped a site's sightings",
		"site", string(id), "sightings", dropped, "devices_removed", removed)
}

// managedSets computes the set of guids and cpuIds bound by managed profiles in
// the live config. It only reads the atomic pointer, so it is safe to call
// without the registry lock held.
func (s *Scanner) managedSets() (guids, cpus map[string]bool) {
	guids, cpus = map[string]bool{}, map[string]bool{}
	if s.cfg == nil {
		return guids, cpus
	}
	cfg := s.cfg.Load()
	if cfg == nil {
		return guids, cpus
	}
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if !r.Managed {
			continue
		}
		if r.GUID != "" {
			guids[r.GUID] = true
		}
		if r.CPUID != "" {
			cpus[r.CPUID] = true
		}
	}
	return guids, cpus
}

// Resolve returns the endpoint on the current path (path rule) of a device
// identified by guid, with a cpuId fallback. It looks up the guid entry; if
// that entry is absent or offline (unseen for at least stalenessWindow) and
// cpuID is non-empty, it falls back to the entry the cpuId currently indexes
// (so a device whose guid changed still resolves with no config rewrite). A
// miss returns a zero endpoint and false. No network I/O.
func (s *Scanner) Resolve(guid, cpuID string) (ep netsite.Endpoint, online bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[guid]
	if ok {
		e = s.selectPathLocked(guid, now)
	}
	if !(ok && fresh(e.current(), now)) && cpuID != "" {
		if g, has := s.byCPU[cpuID]; has {
			if _, cok := s.entries[g]; cok {
				e, ok = s.selectPathLocked(g, now), true
			}
		}
	}
	if !ok {
		return netsite.Endpoint{}, false
	}
	cur := e.current()
	return cur.endpoint(), fresh(cur, now)
}

// ProbeGUID sends a live unicast scan_air to ep, through ep's site, and returns
// the guid/cpuId the device currently at that endpoint reports (network I/O, no
// lock). It ties a guid to the exact box we are connected to right now, with no
// dependence on the possibly-stale registry — the corroboration primitive for
// migration (design device-identity-and-media §3.4) and reset (design
// device-identity-and-media §4).
func (s *Scanner) ProbeGUID(ctx context.Context, ep netsite.Endpoint) (guid, cpuID string, ok bool) {
	dev, found := s.tr.Probe(ctx, ep, scanRequest)
	if !found {
		return "", "", false
	}
	return dev.GUID, dev.CPUID, true
}

// Discovered builds the UI list from a registry snapshot with no network I/O.
// Managed/RigID are set by matching each entry against the live config by guid
// Count returns the number of devices in the registry. Read lock-free of the UDP
// I/O (the broadcast is issued outside s.mu), so the metrics state collector can
// call it on the scrape path (hazard P2).
func (s *Scanner) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// or cpuId; Online reflects the staleness window. Shares is left empty here and
// filled on demand at adoption via DeviceShares.
func (s *Scanner) Discovered() []model.DiscoveredRig {
	now := s.now()

	s.mu.Lock()
	// Copy out the current sighting rather than the entry: an entry shares its
	// paths map with the registry, which the next merge writes after unlock.
	type listed struct {
		cur       sighting
		firstSeen time.Time
	}
	snapshot := make([]listed, 0, len(s.entries))
	for guid := range s.entries {
		e := s.selectPathLocked(guid, now)
		snapshot = append(snapshot, listed{cur: e.current(), firstSeen: e.FirstSeen})
	}
	s.mu.Unlock()

	var cfg *model.Config
	if s.cfg != nil {
		cfg = s.cfg.Load()
	}

	out := make([]model.DiscoveredRig, 0, len(snapshot))
	for _, e := range snapshot {
		c := e.cur
		managed, rigID := matchManaged(cfg, c.GUID, c.CPUID)
		out = append(out, model.DiscoveredRig{
			GUID:      c.GUID,
			CPUID:     c.CPUID,
			Model:     c.Model,
			SSID:      c.SSID,
			Address:   c.Address,
			Site:      string(c.Site),
			Shares:    nil,
			Online:    fresh(c, now),
			Managed:   managed,
			RigID:     rigID,
			FirstSeen: e.firstSeen.Unix(),
		})
	}
	return out
}

// matchManaged reports whether a device (by guid or cpuId) is bound by a managed
// profile in cfg, returning that profile's id.
func matchManaged(cfg *model.Config, guid, cpuID string) (bool, string) {
	if cfg == nil {
		return false, ""
	}
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if r.GUID != "" && r.GUID == guid {
			return true, r.ID
		}
		if r.CPUID != "" && cpuID != "" && r.CPUID == cpuID {
			return true, r.ID
		}
	}
	return false, ""
}

// DeviceShares opens a guest SMB session to the device a guid currently resolves
// to and enumerates its shares (the only place discovery opens SMB, design
// architecture §3), so the adopt dialog can prefill its watched-share list.
// Whether a device is already managed is answered by the guid/cpuId registry
// (Discovered → matchManaged), not by a marker read here.
func (s *Scanner) DeviceShares(ctx context.Context, guid string) (shares []string, err error) {
	ep, _ := s.Resolve(guid, "")
	if ep.IP == "" {
		return nil, errUnresolved
	}

	budget := sharesBudget(s.tr.Site(ep.Site))
	dev := s.newDevice(ep, guestUser, guestPass, budget)
	openCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	sess, err := dev.Open(openCtx)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	return sess.ListShares()
}

// sharesBudget bounds DeviceShares' session open and each operation on it:
// probeTimeout plus 3 round-trip margins of the site, for the connect, the
// negotiate and the session setup. On the local site it is probeTimeout
// exactly.
func sharesBudget(site netsite.Site) time.Duration {
	return probeTimeout + netsite.RoundTrips(site, 3)
}

// RunBackground refreshes the registry on a timer until ctx is cancelled. It
// does NOT scan eagerly at start: the composition root runs one synchronous
// warm-up round before launching this, so startup does exactly one round.
// interval <= 0 uses backgroundInterval.
func (s *Scanner) RunBackground(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = backgroundInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Debug("discovery: background loop stopping", "reason", ctx.Err())
			return
		case <-ticker.C:
			s.Scan(ctx)
		}
	}
}
