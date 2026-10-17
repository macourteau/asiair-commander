# Metrics invariant coverage

This maps each metrics invariant (`docs/design/2026-10-17-architecture.md` §17.8) to the
named test that enforces it. It is the auditable counterpart to that
section's table. (Named separately from `ADVERSARIAL_COVERAGE.md` to avoid two
near-identical coverage files.)

| # | Invariant | Test(s) |
|---|---|---|
| **I1** | Every label value comes from a closed enum or a bounded identity (rig ID, share name). No filenames, target names, error strings, or source addresses. | `TestLabelValuesAreBounded`, `TestSeedCoversEveryMetric` — `internal/metrics` |
| **I2** | No user-authored free text or PII reaches a label. Rig labels use `RigStatus.ID`, never `.Name`; alert labels come from the closed `model.Category`/`model.Severity` sets. | `TestLabelValuesAreBounded` — `internal/metrics` (the seed drives rig IDs and the alert enums; no `Name` is ever passed) |
| **I3** | Instrumentation never blocks, fails, or alters the operation it measures. A nil `*Metrics` is a complete no-op. | `TestNilMetricsIsSafe` — `internal/metrics`; plus every subsystem test, which constructs its types with a nil metrics handle and still passes |
| **I4** | The scrape-time collector performs no I/O and takes no lock held across I/O (P2). | `TestStateCollectorDoesNotBlock` — `internal/metrics` (collector contract, both clauses); `TestAdmissionInFlightIsLockFree` — `internal/manager` (the real `Admission.InFlight` while `a.mu` is held across `FreeSpace`). These pin the hazard directly, so `test/neverblock` has no separate scrape case. |
| **I5** | Every histogram declares explicit classic buckets in addition to its native factor (P11). | `TestEveryHistogramHasExplicitBuckets` — `internal/metrics`; enforced at construction by `newHistogramVec`, which panics on nil buckets |
| **I6** | `/metrics` remains loopback-only unless `web.metrics_expose_lan` is set. | `TestMetricsLoopbackGate` — `internal/web` |
| **I7** | Metric names and help text follow Prometheus conventions. (promlint is a weak guard — P13.) | `TestRegistryPassesPromlint` — `internal/metrics` |
| **I8** | A scrape never fails. Duplicate label sets are deduplicated; a removed rig leaves no residual series (best-effort — a rig deleted mid-cycle may leave one cycle's series; see `docs/design/2026-10-17-architecture.md` §17.4). | `TestStateCollectorDeduplicatesLabelSets`, `TestForgetRigRemovesSeries` — `internal/metrics` (Vec cleanup); `TestRemoveRig` — `internal/hub` (state-gauge cleanup); `TestMetricsEndpointServesValidExposition` — `internal/web` |
