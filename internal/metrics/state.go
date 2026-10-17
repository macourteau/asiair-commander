package metrics

import (
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// State providers. Each is read at scrape time by stateCollector.Collect. Every one
// is optional; a nil provider contributes no series. The scrape path MUST NOT do I/O
// or take a lock held across I/O (design architecture I4 / hazard P2), so every
// provider here is a lock-free or pure-memory read — see the audit table in the
// design.

// SnapshotProvider yields the current fleet snapshot (the status hub; a lock-free
// atomic.Pointer read).
type SnapshotProvider interface {
	Snapshot() model.Snapshot
}

// AdmissionProvider yields in-flight transfer accounting. It MUST be backed by
// mirrored atomics, never a mutex held across FreeSpace (P2).
type AdmissionProvider interface {
	InFlight() (count int, bytes int64)
}

// PreviewProvider yields preview cache size and render occupancy.
type PreviewProvider interface {
	CacheStats() (entries int, bytes int64)
	InFlight() int
}

// SubscriberCountProvider yields the live status-hub subscriber count.
type SubscriberCountProvider interface {
	SubscriberCount() int
}

// DiscoveryProvider yields the discovery registry size.
type DiscoveryProvider interface {
	Count() int
}

// StateProviders bundles the optional providers the collector reads.
type StateProviders struct {
	Snapshot  SnapshotProvider
	Admission AdmissionProvider
	Preview   PreviewProvider
	Hub       SubscriberCountProvider
	Discovery DiscoveryProvider
}

// RegisterStateCollector registers a scrape-time collector reading the given
// providers. Call it once, after every subsystem exists.
func (m *Metrics) RegisterStateCollector(p StateProviders) {
	if m == nil {
		return
	}
	m.registry.MustRegister(newStateCollector(p))
}

type stateCollector struct {
	p StateProviders

	rigUp             *prometheus.Desc
	rigPaused         *prometheus.Desc
	rigLastListing    *prometheus.Desc
	rigWaitingSettle  *prometheus.Desc
	rigFramesTonight  *prometheus.Desc
	remoteRigInfo     *prometheus.Desc
	deviceFree        *prometheus.Desc
	destinationFree   *prometheus.Desc
	alertsUnacked     *prometheus.Desc
	transfersInFlight *prometheus.Desc
	transferInFlightB *prometheus.Desc
	previewCacheBytes *prometheus.Desc
	previewCacheEnt   *prometheus.Desc
	previewInFlight   *prometheus.Desc
	hubSubscribers    *prometheus.Desc
	discoveryDevices  *prometheus.Desc
}

func newStateCollector(p StateProviders) *stateCollector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", name), help, labels, nil)
	}
	return &stateCollector{
		p:                 p,
		rigUp:             d("rig_up", "1 if the rig is currently online, else 0.", "rig"),
		rigPaused:         d("rig_paused", "1 if the rig is currently paused, else 0.", "rig"),
		rigLastListing:    d("rig_last_listing_timestamp_seconds", "Unix time of the rig's last successful listing (absent until the first).", "rig"),
		rigWaitingSettle:  d("rig_frames_waiting_settle", "Frames discovered this cycle not yet past the settle gate.", "rig"),
		rigFramesTonight:  d("rig_frames_tonight", "Frames filed for the rig tonight.", "rig"),
		remoteRigInfo:     d("remote_rig_info", "Constant 1 for each rig reached through a remote, labelled by the remote's id; absent for a rig on the local network.", "rig", "remote"),
		deviceFree:        d("device_free_bytes", "Free bytes on a watched device share.", "rig", "share"),
		destinationFree:   d("destination_free_bytes", "Free bytes on the NAS destination volume."),
		alertsUnacked:     d("alerts_unacked", "Unacknowledged alert count."),
		transfersInFlight: d("transfers_in_flight", "Transfers currently holding an admission slot."),
		transferInFlightB: d("transfer_in_flight_bytes", "Bytes reserved by in-flight transfers."),
		previewCacheBytes: d("preview_cache_bytes", "Preview cache size in bytes (may drift below on-disk bytes; see docs)."),
		previewCacheEnt:   d("preview_cache_entries", "Preview cache entry count."),
		previewInFlight:   d("preview_renders_in_flight", "Preview renders currently executing."),
		hubSubscribers:    d("hub_subscribers", "Live status-hub subscribers."),
		discoveryDevices:  d("discovery_devices", "Devices currently in the discovery registry."),
	}
}

// Describe is intentionally unchecked (sends nothing): the providers come and go, so
// the collector must not pin a fixed descriptor set. This trades away the registry's
// duplicate-descriptor guard, which is why Collect deduplicates its own label sets
// (P12) — a duplicate would otherwise 500 the whole scrape.
func (c *stateCollector) Describe(chan<- *prometheus.Desc) {}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	if c.p.Snapshot != nil {
		snap := c.p.Snapshot.Snapshot()
		gauge(c.destinationFree, float64(snap.NASFree))
		gauge(c.alertsUnacked, float64(snap.UnackedAlerts))

		seenRig := make(map[string]bool, len(snap.Rigs))
		for _, rig := range snap.Rigs {
			if seenRig[rig.ID] {
				continue // dedup rig-level series (P12)
			}
			seenRig[rig.ID] = true
			gauge(c.rigUp, b2f(rig.Online), rig.ID)
			gauge(c.rigPaused, b2f(rig.Paused), rig.ID)
			gauge(c.rigWaitingSettle, float64(rig.WaitingToSettle), rig.ID)
			gauge(c.rigFramesTonight, float64(rig.FramesTonight), rig.ID)
			// A rig's remote is an info series rather than a label on the per-rig
			// series, so a rig moving between sites does not split their history
			// (design remote-sites §8.2). Site is the remote's
			// id, never its name, and "" on the local network.
			if rig.Site != "" {
				gauge(c.remoteRigInfo, 1, rig.ID, rig.Site)
			}
			// Omit the last-listing timestamp until a rig has listed at least once:
			// a zero would make time()-ts read ~1.7e9 and fire the staleness alert
			// for every rig at every startup (design architecture §17.2 rules).
			if rig.ListingTS != 0 {
				gauge(c.rigLastListing, float64(rig.ListingTS), rig.ID)
			}
			seenShare := make(map[string]bool, len(rig.Shares))
			for _, sh := range rig.Shares {
				if seenShare[sh.Share] {
					continue // Shares is a slice, not a map — a duplicate name would 500 the gather (P12)
				}
				seenShare[sh.Share] = true
				gauge(c.deviceFree, float64(sh.Free), rig.ID, sh.Share)
			}
		}
	}
	if c.p.Admission != nil {
		count, bytes := c.p.Admission.InFlight()
		gauge(c.transfersInFlight, float64(count))
		gauge(c.transferInFlightB, float64(bytes))
	}
	if c.p.Preview != nil {
		entries, bytes := c.p.Preview.CacheStats()
		gauge(c.previewCacheBytes, float64(bytes))
		gauge(c.previewCacheEnt, float64(entries))
		gauge(c.previewInFlight, float64(c.p.Preview.InFlight()))
	}
	if c.p.Hub != nil {
		gauge(c.hubSubscribers, float64(c.p.Hub.SubscriberCount()))
	}
	if c.p.Discovery != nil {
		gauge(c.discoveryDevices, float64(c.p.Discovery.Count()))
	}
}
