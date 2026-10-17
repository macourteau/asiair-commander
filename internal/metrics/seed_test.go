package metrics

import "github.com/macourteau/asiair-commander/internal/model"

// seedStateProviders returns providers that yield one rig with one share and
// non-zero values everywhere, so a scrape of a seeded registry emits every
// state-collector series (an unseeded series would make the registry-wide tests
// vacuous — hazard P10).
type seedStateProviders struct{}

func (seedStateProviders) Snapshot() model.Snapshot {
	return model.Snapshot{
		NASFree:       1 << 40,
		UnackedAlerts: 2,
		Rigs: []model.RigStatus{{
			ID: "seed-rig", Online: true, Paused: false,
			ListingTS: 1_700_000_000, WaitingToSettle: 1, FramesTonight: 3,
			Shares: []model.ShareFree{{Share: "sd", Free: 1 << 30}},
			Site:   "seed-remote",
		}},
	}
}

type seedAdmission struct{}

func (seedAdmission) InFlight() (int, int64) { return 1, 1 << 20 }

type seedPreview struct{}

func (seedPreview) CacheStats() (int, int64) { return 4, 1 << 24 }
func (seedPreview) InFlight() int            { return 1 }

type seedHub struct{}

func (seedHub) SubscriberCount() int { return 2 }

type seedDiscovery struct{}

func (seedDiscovery) Count() int { return 3 }

// SeedAllForTest drives every recording method across every enum value and registers
// a state collector backed by fully-populated fake providers, so a subsequent
// Gather() emits every metric family this package owns. It is the single fixture
// behind the registry-wide tests (promlint, explicit-buckets, bounded-labels): an
// empty Vec emits no family, so those tests would be vacuous without it (P10).
func SeedAllForTest(m *Metrics) {
	const rig = "seed-rig"
	const share = "sd"

	for _, o := range AllTransferOutcomes {
		m.RecordTransferOutcome(rig, o)
	}
	m.AddTransferBytes(rig, 1<<20)
	m.AddTransferBytesWasted(rig, 1<<19)
	m.ObserveTransferDuration(rig, 1.5)
	m.ObserveAdmissionWait(rig, 0.25)

	for _, c := range AllFrameClasses {
		for _, d := range AllFrameDispositions {
			m.RecordFrameFiled(rig, c, d)
		}
	}
	m.RecordFrameUpsertFailure(rig)

	for _, mn := range AllDeleteManners {
		m.RecordFrameDeleted(rig, mn)
	}
	for _, r := range AllDeleteBlockedReasons {
		m.RecordDeleteBlocked(rig, r)
	}
	m.RecordDeletePassYield(rig)
	m.RecordDeleteHashInterrupted(rig)
	for _, c := range AllFrameLostCauses {
		m.RecordFrameLost(rig, c)
	}
	for _, r := range AllNASRestoreResults {
		m.RecordNASRestore(rig, r)
	}
	for _, r := range AllCycleResults {
		m.RecordCycle(rig, r)
	}
	for _, p := range AllCyclePhases {
		m.ObserveCycleDuration(rig, p, 2)
	}
	for _, r := range AllRefuseReasons {
		m.RecordPullPhaseRefused(rig, r)
	}

	for _, r := range AllRefileResults {
		m.RecordRefileFrame(r)
	}

	for _, cat := range AllSeedCategories {
		for _, sev := range AllSeedSeverities {
			m.RecordAlertRaised(cat, sev)
		}
	}

	m.SetListingEntries(rig, share, 42, 1_700_000_000)

	const remote = "seed-remote"
	for _, r := range AllRemoteCallResults {
		m.RecordRemoteHeartbeat(remote, r)
		m.RecordRemoteReconnectRound(remote, r)
	}
	m.RecordRemoteRebuild(remote)
	m.RecordRemoteWarning(remote)
	m.SetRemoteUp(remote, true)
	m.SetRemoteLatency(remote, 0.12)

	m.RecordHubSubscriberDropped()
	m.RecordLogEntryDropped()
	m.RecordWorkerCommandDropped(rig)
	m.RecordPreviewWarmDropped()

	m.RegisterStateCollector(StateProviders{
		Snapshot:  seedStateProviders{},
		Admission: seedAdmission{},
		Preview:   seedPreview{},
		Hub:       seedHub{},
		Discovery: seedDiscovery{},
	})
}

// AllSeedCategories / AllSeedSeverities are the alert label sets the seed drives.
// They live here (test-only) rather than in metrics.go because the category/severity
// vocabulary is owned by internal/model; this is the metrics-side allowlist the
// bounded-labels test checks against.
var AllSeedCategories = []model.Category{
	model.AlertTransferFailed, model.AlertVerifyMismatch, model.AlertDeleteMismatch,
	model.AlertUnclassified, model.AlertRotClash, model.AlertBacklog, model.AlertNASDiskLow,
	model.AlertDeviceDiskLow, model.AlertUnreachable, model.AlertFrameLost,
	model.AlertIdentityMismatch, model.AlertIdentityUnconfirmed, model.AlertMediaChanged,
	model.AlertDestinationUnavailable, model.AlertClockSkew, model.AlertConfig,
	model.AlertLibraryReconciled, model.AlertLibraryUnavailable, model.AlertLibraryMismatch,
	model.AlertRefile,
	model.AlertRemoteUnreachable, model.AlertRemoteStalled, model.AlertRemotePairing,
	model.AlertRemoteForeignKey, model.AlertRemoteBuildStale,
}

var AllSeedSeverities = []model.Severity{
	model.SeverityInfo, model.SeverityWarn, model.SeverityCritical,
}
