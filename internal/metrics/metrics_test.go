package metrics

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"

	dto "github.com/prometheus/client_model/go"
)

// expectedAppMetricNames is the authoritative list of every asiair_commander_ family
// this package owns. TestSeedCoversEveryMetric asserts the seeded gather matches it
// exactly, so a metric added without a seed case (or a rename) fails loudly rather
// than silently hollowing out the registry-wide tests (P10).
var expectedAppMetricNames = []string{
	"asiair_commander_transfer_outcomes_total",
	"asiair_commander_transfer_bytes_total",
	"asiair_commander_transfer_bytes_wasted_total",
	"asiair_commander_frames_filed_total",
	"asiair_commander_frame_upsert_failures_total",
	"asiair_commander_frames_deleted_total",
	"asiair_commander_delete_blocked_total",
	"asiair_commander_delete_pass_yield_total",
	"asiair_commander_delete_hash_interrupted_total",
	"asiair_commander_frames_lost_total",
	"asiair_commander_nas_restores_total",
	"asiair_commander_cycles_total",
	"asiair_commander_pull_phase_refused_total",
	"asiair_commander_alerts_raised_total",
	"asiair_commander_refile_frames_total",
	"asiair_commander_transfer_duration_seconds",
	"asiair_commander_admission_wait_seconds",
	"asiair_commander_cycle_duration_seconds",
	"asiair_commander_listing_entries",
	"asiair_commander_listing_timestamp_seconds",
	"asiair_commander_hub_subscribers_dropped_total",
	"asiair_commander_log_entries_dropped_total",
	"asiair_commander_worker_commands_dropped_total",
	"asiair_commander_preview_warms_dropped_total",
	"asiair_commander_remote_heartbeats_total",
	"asiair_commander_remote_reconnect_rounds_total",
	"asiair_commander_remote_client_rebuilds_total",
	"asiair_commander_remote_warnings_total",
	"asiair_commander_remote_up",
	"asiair_commander_remote_latency_seconds",
	"asiair_commander_build_info",
	"asiair_commander_start_time_seconds",
	// state-collector gauges
	"asiair_commander_rig_up",
	"asiair_commander_rig_paused",
	"asiair_commander_rig_last_listing_timestamp_seconds",
	"asiair_commander_rig_frames_waiting_settle",
	"asiair_commander_rig_frames_tonight",
	"asiair_commander_remote_rig_info",
	"asiair_commander_device_free_bytes",
	"asiair_commander_destination_free_bytes",
	"asiair_commander_alerts_unacked",
	"asiair_commander_transfers_in_flight",
	"asiair_commander_transfer_in_flight_bytes",
	"asiair_commander_preview_cache_bytes",
	"asiair_commander_preview_cache_entries",
	"asiair_commander_preview_renders_in_flight",
	"asiair_commander_hub_subscribers",
	"asiair_commander_discovery_devices",
}

func newSeeded(t *testing.T) *Metrics {
	t.Helper()
	m := New("test-version")
	SeedAllForTest(m)
	return m
}

func gatheredAppNames(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, mf := range families {
		name := mf.GetName()
		if len(name) >= len("asiair_commander_") && name[:len("asiair_commander_")] == "asiair_commander_" {
			out[name] = mf
		}
	}
	return out
}

func TestSeedCoversEveryMetric(t *testing.T) {
	got := gatheredAppNames(t, newSeeded(t))
	want := map[string]bool{}
	for _, n := range expectedAppMetricNames {
		want[n] = true
	}
	for n := range got {
		if !want[n] {
			t.Errorf("gathered unexpected metric %q — add it to expectedAppMetricNames", n)
		}
	}
	for n := range want {
		if _, ok := got[n]; !ok {
			t.Errorf("expected metric %q not emitted by the seed — SeedAllForTest is missing a case (P10)", n)
		}
	}
}

func TestRegistryPassesPromlint(t *testing.T) {
	m := newSeeded(t)
	// Lint only our own metrics; the go_*/process_*/go_sql_* collectors are
	// third-party. promlint is a weak guard (P13) but catches suffix/unit slips.
	problems, err := testutil.GatherAndLint(m.registry, expectedAppMetricNames...)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	for _, p := range problems {
		t.Errorf("promlint: %s: %s", p.Metric, p.Text)
	}
}

func TestEveryHistogramHasExplicitBuckets(t *testing.T) {
	for name, mf := range gatheredAppNames(t, newSeeded(t)) {
		if mf.GetType() != dto.MetricType_HISTOGRAM {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if n := len(metric.GetHistogram().GetBucket()); n == 0 {
				t.Errorf("histogram %q emitted 0 classic buckets — native factor with nil Buckets (P11)", name)
			}
		}
	}
}

func TestLabelValuesAreBounded(t *testing.T) {
	// Enum labels whose full value set is closed. Identity labels (rig, share,
	// version, go_version, db_name) are bounded by construction, not enumerated.
	allowed := map[string]map[string]bool{
		"outcome":  boundSet(stringify(AllTransferOutcomes)),
		"class":    boundSet(stringify(AllFrameClasses)),
		"manner":   boundSet(stringify(AllDeleteManners)),
		"cause":    boundSet(stringify(AllFrameLostCauses)),
		"phase":    boundSet(stringify(AllCyclePhases)),
		"severity": boundSet([]string{"info", "warn", "critical"}),
	}
	// "disposition", "reason", "result", "category" are reused across metrics with
	// different value sets, so check them per-metric below.
	perMetric := map[string]map[string]map[string]bool{
		"asiair_commander_frames_filed_total":            {"disposition": boundSet(stringify(AllFrameDispositions))},
		"asiair_commander_delete_blocked_total":          {"reason": boundSet(stringify(AllDeleteBlockedReasons))},
		"asiair_commander_nas_restores_total":            {"result": boundSet(stringify(AllNASRestoreResults))},
		"asiair_commander_cycles_total":                  {"result": boundSet(stringify(AllCycleResults))},
		"asiair_commander_pull_phase_refused_total":      {"reason": boundSet(stringify(AllRefuseReasons))},
		"asiair_commander_alerts_raised_total":           {"category": boundSet(stringify(AllSeedCategories))},
		"asiair_commander_refile_frames_total":           {"result": boundSet(stringify(AllRefileResults))},
		"asiair_commander_remote_heartbeats_total":       {"result": boundSet(stringify(AllRemoteCallResults))},
		"asiair_commander_remote_reconnect_rounds_total": {"result": boundSet(stringify(AllRemoteCallResults))},
	}

	for name, mf := range gatheredAppNames(t, newSeeded(t)) {
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				ln, lv := lp.GetName(), lp.GetValue()
				if set, ok := allowed[ln]; ok && !set[lv] {
					t.Errorf("%s label %s=%q outside its enum", name, ln, lv)
				}
				if pm, ok := perMetric[name]; ok {
					if set, ok := pm[ln]; ok && !set[lv] {
						t.Errorf("%s label %s=%q outside its enum", name, ln, lv)
					}
				}
			}
		}
	}
}

// lockyAdmission mirrors the real Admission's P2 fix: InFlight reads a mirrored
// atomic and never the mutex. A goroutine holding mu (simulating Acquire blocked in
// FreeSpace) must not stall a scrape.
type lockyAdmission struct {
	mu    sync.Mutex
	bytes atomic.Int64
}

func (l *lockyAdmission) InFlight() (int, int64) { return 0, l.bytes.Load() }

func TestStateCollectorDoesNotBlock(t *testing.T) {
	adm := &lockyAdmission{}
	adm.bytes.Store(123)
	m := New("test-version")
	m.RegisterStateCollector(StateProviders{Admission: adm})

	held := make(chan struct{})
	go func() {
		adm.mu.Lock()
		close(held)
		time.Sleep(2 * time.Second) // hold the lock across a long "I/O"
		adm.mu.Unlock()
	}()
	<-held

	done := make(chan struct{})
	go func() {
		if _, err := m.registry.Gather(); err != nil {
			t.Errorf("gather: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("scrape blocked behind a held provider lock — collector must read lock-free (P2)")
	}
}

// dupShareSnapshot returns a rig whose Shares slice carries the same share name
// twice — the exact shape that would emit a duplicate {rig,share} label set.
type dupShareSnapshot struct{}

func (dupShareSnapshot) Snapshot() model.Snapshot {
	return model.Snapshot{Rigs: []model.RigStatus{{
		ID:     "r",
		Shares: []model.ShareFree{{Share: "sd", Free: 1}, {Share: "sd", Free: 2}},
	}}}
}

func TestStateCollectorDeduplicatesLabelSets(t *testing.T) {
	m := New("test-version")
	m.RegisterStateCollector(StateProviders{Snapshot: dupShareSnapshot{}})
	if _, err := m.registry.Gather(); err != nil {
		t.Fatalf("duplicate share label set failed the gather (P12): %v", err)
	}
}

// stateGaugeNames are emitted by the scrape-time collector from the live snapshot,
// not from the directly-registered Vecs. ForgetRig does not (and must not) touch
// them: a deleted rig stops appearing because it drops out of the hub snapshot, so
// the seed's fake snapshot provider legitimately keeps emitting "seed-rig" here.
var stateGaugeNames = map[string]bool{
	"asiair_commander_rig_up":                             true,
	"asiair_commander_rig_paused":                         true,
	"asiair_commander_rig_last_listing_timestamp_seconds": true,
	"asiair_commander_rig_frames_waiting_settle":          true,
	"asiair_commander_rig_frames_tonight":                 true,
	"asiair_commander_remote_rig_info":                    true,
	"asiair_commander_device_free_bytes":                  true,
	"asiair_commander_destination_free_bytes":             true,
	"asiair_commander_alerts_unacked":                     true,
	"asiair_commander_transfers_in_flight":                true,
	"asiair_commander_transfer_in_flight_bytes":           true,
	"asiair_commander_preview_cache_bytes":                true,
	"asiair_commander_preview_cache_entries":              true,
	"asiair_commander_preview_renders_in_flight":          true,
	"asiair_commander_hub_subscribers":                    true,
	"asiair_commander_discovery_devices":                  true,
}

func TestForgetRigRemovesSeries(t *testing.T) {
	m := newSeeded(t)
	m.ForgetRig("seed-rig")
	for name, mf := range gatheredAppNames(t, m) {
		if stateGaugeNames[name] {
			continue // snapshot-driven, not ForgetRig's responsibility
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "rig" && lp.GetValue() == "seed-rig" {
					t.Errorf("%s still carries a series for the forgotten rig", name)
				}
			}
		}
	}
}

func TestForgetRemoteRemovesSeries(t *testing.T) {
	m := newSeeded(t)
	m.ForgetRemote("seed-remote")
	for name, mf := range gatheredAppNames(t, m) {
		if stateGaugeNames[name] {
			continue // snapshot-driven: a removed remote's rigs drop it from the snapshot
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "remote" && lp.GetValue() == "seed-remote" {
					t.Errorf("%s still carries a series for the forgotten remote", name)
				}
			}
		}
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics // nil
	// None of these must panic (design architecture I3).
	m.RecordTransferOutcome("r", OutcomeFiled)
	m.AddTransferBytes("r", 1)
	m.AddTransferBytesWasted("r", 1)
	m.ObserveTransferDuration("r", 1)
	m.ObserveAdmissionWait("r", 1)
	m.RecordFrameFiled("r", ClassLight, DispositionFresh)
	m.RecordFrameUpsertFailure("r")
	m.RecordFrameDeleted("r", MannerDeviceDeleted)
	m.RecordDeleteBlocked("r", BlockedSourceChanged)
	m.RecordDeletePassYield("r")
	m.RecordDeleteHashInterrupted("r")
	m.RecordFrameLost("r", LostNASMissing)
	m.RecordNASRestore("r", RestoreRestored)
	m.RecordCycle("r", CycleOK)
	m.ObserveCycleDuration("r", PhaseTotal, 1)
	m.RecordPullPhaseRefused("r", RefuseSessionOpenFailed)
	m.RecordAlertRaised(model.AlertConfig, model.SeverityInfo)
	m.SetListingEntries("r", "sd", 1, 1)
	m.RecordHubSubscriberDropped()
	m.RecordLogEntryDropped()
	m.RecordWorkerCommandDropped("r")
	m.RecordPreviewWarmDropped()
	m.RecordRemoteHeartbeat("r1", RemoteCallOK)
	m.RecordRemoteReconnectRound("r1", RemoteCallFailed)
	m.RecordRemoteRebuild("r1")
	m.RecordRemoteWarning("r1")
	m.SetRemoteUp("r1", true)
	m.SetRemoteLatency("r1", 1)
	m.ForgetRemote("r1")
	m.ForgetRig("r")
	m.RegisterStateCollector(StateProviders{})
	m.RegisterDBStats(nil, "x")
	if m.Registry() != nil {
		t.Fatal("nil Metrics should have a nil registry")
	}
}

// --- helpers ---

func stringify[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func boundSet(vs []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// remoteRigSnapshot has one rig behind a remote and one on the local network.
type remoteRigSnapshot struct{}

func (remoteRigSnapshot) Snapshot() model.Snapshot {
	return model.Snapshot{Rigs: []model.RigStatus{
		{ID: "rig-remote", Site: "r1", RemoteName: "Observatory"},
		{ID: "rig-local"},
	}}
}

// TestRemoteRigInfo: each rig reached through a remote has one info series
// naming the remote by id, never by name; a local rig has none.
func TestRemoteRigInfo(t *testing.T) {
	m := New("test-version")
	m.RegisterStateCollector(StateProviders{Snapshot: remoteRigSnapshot{}})
	mf := gatheredAppNames(t, m)["asiair_commander_remote_rig_info"]
	if mf == nil || len(mf.GetMetric()) != 1 {
		t.Fatalf("remote_rig_info: want one series, got %v", mf)
	}
	got := map[string]string{}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if got["rig"] != "rig-remote" || got["remote"] != "r1" || len(got) != 2 {
		t.Fatalf("remote_rig_info labels %v, want rig=rig-remote remote=r1", got)
	}
	if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 1 {
		t.Fatalf("remote_rig_info = %v, want 1", v)
	}
}
