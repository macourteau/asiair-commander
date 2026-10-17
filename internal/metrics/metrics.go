// Package metrics owns the Prometheus surface for ASIAIR Commander. It is a
// near-leaf package: its only non-stdlib imports are client_golang and
// internal/model (the state-provider interfaces traffic in model.Snapshot). Every
// other subsystem is a writer that imports this package, never the reverse, so the
// dependency graph stays acyclic (design architecture §17.1).
//
// Every recording method is nil-safe: a nil *Metrics is a complete no-op. That is
// what lets the hundreds of existing test constructors keep working untouched and
// keeps instrumented code free of `if m != nil` guards (design architecture I3).
package metrics

import (
	"database/sql"
	"runtime"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "asiair_commander"

// Metrics is the whole exported surface. It owns a private registry so tests and
// parallel processes never collide on the default one.
type Metrics struct {
	registry *prometheus.Registry

	// Event counters (written at call sites; read zero until Phase B wires them).
	transferOutcomes    *prometheus.CounterVec // rig, outcome
	transferBytes       *prometheus.CounterVec // rig
	transferBytesWasted *prometheus.CounterVec // rig
	framesFiled         *prometheus.CounterVec // rig, class, disposition
	frameUpsertFailures *prometheus.CounterVec // rig
	framesDeleted       *prometheus.CounterVec // rig, manner
	deleteBlocked       *prometheus.CounterVec // rig, reason
	deletePassYields    *prometheus.CounterVec // rig
	deleteHashAborts    *prometheus.CounterVec // rig
	framesLost          *prometheus.CounterVec // rig, cause
	nasRestores         *prometheus.CounterVec // rig, result
	cycles              *prometheus.CounterVec // rig, result
	pullPhaseRefused    *prometheus.CounterVec // rig, reason
	alertsRaised        *prometheus.CounterVec // category, severity
	refileFrames        *prometheus.CounterVec // result

	// Event histograms.
	transferDuration *prometheus.HistogramVec // rig
	admissionWait    *prometheus.HistogramVec // rig
	cycleDuration    *prometheus.HistogramVec // rig, phase

	// Per-share listing gauges, set once per cycle by the worker (not the scrape
	// collector — the raw entry count is not in the hub snapshot). Paired so a
	// consumer can tell a fresh count from a stale one (design architecture §17.7).
	listingEntries   *prometheus.GaugeVec // rig, share
	listingTimestamp *prometheus.GaugeVec // rig, share

	// Drop counters — each site loses a different unit of work, so they are separate
	// metrics rather than one {sink} counter (design architecture §17.7).
	hubSubscribersDropped prometheus.Counter
	logEntriesDropped     prometheus.Counter
	workerCommandsDropped *prometheus.CounterVec // rig
	previewWarmsDropped   prometheus.Counter

	// Per-remote series, labelled by remote id, never by name (design
	// remote-sites §8.2). A rig's remote is not a label on
	// the per-rig series; the state collector's remote_rig_info carries it.
	remoteHeartbeats      *prometheus.CounterVec // remote, result
	remoteReconnectRounds *prometheus.CounterVec // remote, result
	remoteRebuilds        *prometheus.CounterVec // remote
	remoteWarnings        *prometheus.CounterVec // remote
	remoteUp              *prometheus.GaugeVec   // remote
	remoteLatency         *prometheus.GaugeVec   // remote

	// Build/runtime identity.
	buildInfo *prometheus.GaugeVec // version, go_version

	// rigLabelled is every Vec carrying a "rig" label, so ForgetRig can purge a
	// deleted rig's series from all of them in one place (design architecture §17.4).
	rigLabelled []*prometheus.CounterVec
	rigGauges   []*prometheus.GaugeVec
}

// New builds and registers the metric set, the Go/process/build-info collectors, and
// the build_info and start_time gauges. version is the build-time app version.
func New(version string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	m.transferOutcomes = counterVec(reg, "transfer_outcomes_total",
		"Transfer attempts by terminal outcome.", "rig", "outcome")
	m.transferBytes = counterVec(reg, "transfer_bytes_total",
		"Bytes streamed from a rig over SMB, counted on every exit where bytes moved.", "rig")
	m.transferBytesWasted = counterVec(reg, "transfer_bytes_wasted_total",
		"Bytes streamed then discarded because an identical NAS copy already existed.", "rig")
	m.framesFiled = counterVec(reg, "frames_filed_total",
		"Frames filed to the NAS, by class and how they reached their filed row.", "rig", "class", "disposition")
	m.frameUpsertFailures = counterVec(reg, "frame_upsert_failures_total",
		"Frames that transferred successfully but whose DB row failed to persist.", "rig")
	m.framesDeleted = counterVec(reg, "frames_deleted_total",
		"Source frames closed out, by manner (a real device delete vs a source that had vanished).", "rig", "manner")
	m.deleteBlocked = counterVec(reg, "delete_blocked_total",
		"Delete-pass frames that were held or deferred rather than deleted.", "rig", "reason")
	m.deletePassYields = counterVec(reg, "delete_pass_yield_total",
		"Delete slices that reached their deadline and yielded to the next cycle with eligible frames left.", "rig")
	m.deleteHashAborts = counterVec(reg, "delete_hash_interrupted_total",
		"Pre-delete device re-hashes abandoned mid-stream for a pause or shutdown; the frame is deferred.", "rig")
	m.framesLost = counterVec(reg, "frames_lost_total",
		"Doubly-vanished frames (source gone and NAS copy missing/truncated/corrupt).", "rig", "cause")
	m.nasRestores = counterVec(reg, "nas_restores_total",
		"NAS-copy restore attempts triggered during the delete pass.", "rig", "result")
	m.cycles = counterVec(reg, "cycles_total",
		"Rig worker cycles by result.", "rig", "result")
	m.pullPhaseRefused = counterVec(reg, "pull_phase_refused_total",
		"Pull passes refused (which also skips the delete pass) by reason.", "rig", "reason")
	m.alertsRaised = counterVec(reg, "alerts_raised_total",
		"Persisted alert-raise attempts by category and severity (after the store's dedup).", "category", "severity")
	m.refileFrames = counterVec(reg, "refile_frames_total",
		"Archived files moved to where the current folder templates put them, by outcome. Counted per file, not per row.", "result")

	m.transferDuration = histogramVec(reg, "transfer_duration_seconds",
		"Per-frame transfer duration, measured after admission and observed only on a filed frame.", BucketsNetwork, "rig")
	m.admissionWait = histogramVec(reg, "admission_wait_seconds",
		"Time a pull spent waiting in the admission gate.", BucketsNetwork, "rig")
	m.cycleDuration = histogramVec(reg, "cycle_duration_seconds",
		"Rig cycle duration by phase.", BucketsCycle, "rig", "phase")

	m.listingEntries = gaugeVec(reg, "listing_entries",
		"Entries returned by the last completed listing pass for a share.", "rig", "share")
	m.listingTimestamp = gaugeVec(reg, "listing_timestamp_seconds",
		"Unix time of the last completed listing pass for a share (pairs with listing_entries to spot a stale count).", "rig", "share")

	m.hubSubscribersDropped = counter(reg, "hub_subscribers_dropped_total",
		"Status-hub subscribers dropped because their event queue overflowed.")
	m.logEntriesDropped = counter(reg, "log_entries_dropped_total",
		"Log entries dropped because a log-buffer subscriber's channel was full.")
	m.workerCommandsDropped = counterVec(reg, "worker_commands_dropped_total",
		"Worker commands dropped because the command channel was full.", "rig")
	m.previewWarmsDropped = counter(reg, "preview_warms_dropped_total",
		"Preview warm renders shed because the render pool was busy.")

	m.remoteHeartbeats = counterVec(reg, "remote_heartbeats_total",
		"Heartbeats (hello calls) to a remote, by result.", "remote", "result")
	m.remoteReconnectRounds = counterVec(reg, "remote_reconnect_rounds_total",
		"Broadcast rounds run on a remote after it (re)connected, by result.", "remote", "result")
	m.remoteRebuilds = counterVec(reg, "remote_client_rebuilds_total",
		"Tunnel clients rebuilt for a remote whose heartbeat failed with no bytes moving.", "remote")
	m.remoteWarnings = counterVec(reg, "remote_warnings_total",
		"Warnings a remote reported, each counted once when first seen.", "remote")
	m.remoteUp = gaugeVec(reg, "remote_up",
		"1 while a remote's site carries traffic (connected and its reconnect round done), else 0.", "remote")
	m.remoteLatency = gaugeVec(reg, "remote_latency_seconds",
		"A remote's smoothed round-trip latency, as the timeouts on its site use it.", "remote")

	m.buildInfo = gaugeVec(reg, "build_info",
		"Build identity; constant 1, joined against other series by version.", "version", "go_version")
	m.buildInfo.WithLabelValues(version, runtime.Version()).Set(1)

	startTime := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "start_time_seconds",
		Help: "Unix time at which this process constructed its metrics.",
	})
	startTime.SetToCurrentTime()
	reg.MustRegister(startTime)

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)

	m.rigLabelled = []*prometheus.CounterVec{
		m.transferOutcomes, m.transferBytes, m.transferBytesWasted, m.framesFiled,
		m.frameUpsertFailures, m.framesDeleted, m.deleteBlocked, m.deletePassYields, m.deleteHashAborts, m.framesLost,
		m.nasRestores, m.cycles, m.pullPhaseRefused, m.workerCommandsDropped,
	}
	m.rigGauges = []*prometheus.GaugeVec{m.listingEntries, m.listingTimestamp}
	return m
}

// Registry exposes the registry so the web layer can build a promhttp handler and
// so RegisterStateCollector / RegisterDBStats can add to it.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

// RegisterDBStats wires one sql.DB's connection-pool stats (go_sql_* — note these do
// NOT carry the asiair_commander namespace, P14) into the registry under name.
func (m *Metrics) RegisterDBStats(db *sql.DB, name string) {
	if m == nil || db == nil {
		return
	}
	m.registry.MustRegister(collectors.NewDBStatsCollector(db, name))
}

// ForgetRig removes every series carrying rig as its rig label, so a rig deleted
// through the API leaves no residual series (design architecture §17.4).
func (m *Metrics) ForgetRig(rig string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{"rig": rig}
	for _, c := range m.rigLabelled {
		c.DeletePartialMatch(labels)
	}
	for _, g := range m.rigGauges {
		g.DeletePartialMatch(labels)
	}
	m.transferDuration.DeletePartialMatch(labels)
	m.admissionWait.DeletePartialMatch(labels)
	m.cycleDuration.DeletePartialMatch(labels)
}

// ForgetRemote removes every series labelled with remote, so a removed remote
// leaves none behind.
func (m *Metrics) ForgetRemote(remote string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{"remote": remote}
	for _, c := range []*prometheus.CounterVec{m.remoteHeartbeats, m.remoteReconnectRounds, m.remoteRebuilds, m.remoteWarnings} {
		c.DeletePartialMatch(labels)
	}
	m.remoteUp.DeletePartialMatch(labels)
	m.remoteLatency.DeletePartialMatch(labels)
}

// --- Recording methods (all nil-safe) ---

func (m *Metrics) RecordRemoteHeartbeat(remote string, r RemoteCallResult) {
	if m == nil {
		return
	}
	m.remoteHeartbeats.WithLabelValues(remote, string(r)).Inc()
}

func (m *Metrics) RecordRemoteReconnectRound(remote string, r RemoteCallResult) {
	if m == nil {
		return
	}
	m.remoteReconnectRounds.WithLabelValues(remote, string(r)).Inc()
}

func (m *Metrics) RecordRemoteRebuild(remote string) {
	if m == nil {
		return
	}
	m.remoteRebuilds.WithLabelValues(remote).Inc()
}

func (m *Metrics) RecordRemoteWarning(remote string) {
	if m == nil {
		return
	}
	m.remoteWarnings.WithLabelValues(remote).Inc()
}

// SetRemoteUp and SetRemoteLatency are set by the remote supervisor as they
// change, like the listing gauges, rather than read from the snapshot.
func (m *Metrics) SetRemoteUp(remote string, up bool) {
	if m == nil {
		return
	}
	v := 0.0
	if up {
		v = 1
	}
	m.remoteUp.WithLabelValues(remote).Set(v)
}

func (m *Metrics) SetRemoteLatency(remote string, seconds float64) {
	if m == nil {
		return
	}
	m.remoteLatency.WithLabelValues(remote).Set(seconds)
}

func (m *Metrics) RecordTransferOutcome(rig string, o TransferOutcome) {
	if m == nil {
		return
	}
	m.transferOutcomes.WithLabelValues(rig, string(o)).Inc()
}

func (m *Metrics) AddTransferBytes(rig string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.transferBytes.WithLabelValues(rig).Add(float64(n))
}

func (m *Metrics) AddTransferBytesWasted(rig string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.transferBytesWasted.WithLabelValues(rig).Add(float64(n))
}

func (m *Metrics) ObserveTransferDuration(rig string, seconds float64) {
	if m == nil {
		return
	}
	m.transferDuration.WithLabelValues(rig).Observe(seconds)
}

func (m *Metrics) ObserveAdmissionWait(rig string, seconds float64) {
	if m == nil {
		return
	}
	m.admissionWait.WithLabelValues(rig).Observe(seconds)
}

func (m *Metrics) RecordFrameFiled(rig string, class FrameClass, disp FrameDisposition) {
	if m == nil {
		return
	}
	m.framesFiled.WithLabelValues(rig, string(class), string(disp)).Inc()
}

func (m *Metrics) RecordFrameUpsertFailure(rig string) {
	if m == nil {
		return
	}
	m.frameUpsertFailures.WithLabelValues(rig).Inc()
}

func (m *Metrics) RecordFrameDeleted(rig string, manner DeleteManner) {
	if m == nil {
		return
	}
	m.framesDeleted.WithLabelValues(rig, string(manner)).Inc()
}

func (m *Metrics) RecordDeleteBlocked(rig string, reason DeleteBlockedReason) {
	if m == nil {
		return
	}
	m.deleteBlocked.WithLabelValues(rig, string(reason)).Inc()
}

// RecordDeletePassYield counts a delete slice that stopped at its deadline with
// eligible frames left, so a rig that keeps slicing through a backlog is visible.
func (m *Metrics) RecordDeletePassYield(rig string) {
	if m == nil {
		return
	}
	m.deletePassYields.WithLabelValues(rig).Inc()
}

// RecordDeleteHashInterrupted counts a pre-delete device re-hash abandoned
// mid-stream because a Pause was requested or the worker is shutting down. Each
// one is a deferral, so a steady climb with no Pause behind it means the
// interrupt is firing when it should not and deletes are stalling.
func (m *Metrics) RecordDeleteHashInterrupted(rig string) {
	if m == nil {
		return
	}
	m.deleteHashAborts.WithLabelValues(rig).Inc()
}

func (m *Metrics) RecordFrameLost(rig string, cause FrameLostCause) {
	if m == nil {
		return
	}
	m.framesLost.WithLabelValues(rig, string(cause)).Inc()
}

func (m *Metrics) RecordNASRestore(rig string, result NASRestoreResult) {
	if m == nil {
		return
	}
	m.nasRestores.WithLabelValues(rig, string(result)).Inc()
}

func (m *Metrics) RecordCycle(rig string, result CycleResult) {
	if m == nil {
		return
	}
	m.cycles.WithLabelValues(rig, string(result)).Inc()
}

func (m *Metrics) ObserveCycleDuration(rig string, phase CyclePhase, seconds float64) {
	if m == nil {
		return
	}
	m.cycleDuration.WithLabelValues(rig, string(phase)).Observe(seconds)
}

func (m *Metrics) RecordPullPhaseRefused(rig string, reason RefuseReason) {
	if m == nil {
		return
	}
	m.pullPhaseRefused.WithLabelValues(rig, string(reason)).Inc()
}

func (m *Metrics) RecordAlertRaised(category model.Category, severity model.Severity) {
	if m == nil {
		return
	}
	m.alertsRaised.WithLabelValues(string(category), string(severity)).Inc()
}

// RecordRefileFrame records one re-filed FILE's outcome. Re-file is a move, not a
// filing, so it must never touch frames_filed_total — a migration would otherwise
// inflate the filed counts it has nothing to do with.
func (m *Metrics) RecordRefileFrame(result RefileResult) {
	if m == nil {
		return
	}
	m.refileFrames.WithLabelValues(string(result)).Inc()
}

// SetListingEntries records the entry count of a completed listing pass and stamps
// the time, so a stale count (the gauge frozen while a later pass failed) is
// distinguishable from a fresh one.
func (m *Metrics) SetListingEntries(rig, share string, entries int, atUnix int64) {
	if m == nil {
		return
	}
	m.listingEntries.WithLabelValues(rig, share).Set(float64(entries))
	m.listingTimestamp.WithLabelValues(rig, share).Set(float64(atUnix))
}

func (m *Metrics) RecordHubSubscriberDropped() {
	if m == nil {
		return
	}
	m.hubSubscribersDropped.Inc()
}

func (m *Metrics) RecordLogEntryDropped() {
	if m == nil {
		return
	}
	m.logEntriesDropped.Inc()
}

func (m *Metrics) RecordWorkerCommandDropped(rig string) {
	if m == nil {
		return
	}
	m.workerCommandsDropped.WithLabelValues(rig).Inc()
}

func (m *Metrics) RecordPreviewWarmDropped() {
	if m == nil {
		return
	}
	m.previewWarmsDropped.Inc()
}

// --- registration helpers ---

func counter(reg prometheus.Registerer, name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help})
	reg.MustRegister(c)
	return c
}

func counterVec(reg prometheus.Registerer, name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	reg.MustRegister(c)
	return c
}

func gaugeVec(reg prometheus.Registerer, name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	reg.MustRegister(g)
	return g
}

func histogramVec(reg prometheus.Registerer, name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := newHistogramVec(namespace+"_"+name, help, buckets, labels)
	reg.MustRegister(h)
	return h
}
