# Metrics reference

ASIAIR Commander exposes Prometheus metrics at `GET /metrics`. Every metric is
namespaced `asiair_commander_*`. The design and rationale live in
[`docs/design/2026-10-17-architecture.md`](design/2026-10-17-architecture.md) §17;
this file is the operator-facing reference.

## Access

`/metrics` is **loopback-only by default**. To let a Prometheus server on your LAN
scrape it, set `web.metrics_expose_lan: true` in the config. The endpoint is
unauthenticated (a scraper cannot present a session), so the loopback gate is the
control — think before exposing per-rig labels to the whole network.

Histograms are **dual-emitted**: classic buckets for any Prometheus, plus native
(sparse) buckets that a Prometheus 2.40+ server with native histograms enabled will
ingest automatically. No scraper configuration is required either way.

The Go runtime (`go_*`), process (`process_*`), and SQLite pool (`go_sql_*`) metrics
are also exported. Note `go_sql_*` does **not** carry the `asiair_commander_`
namespace — that prefix is fixed by the Prometheus client library, not us.

A freshly started process with no rig connected exports roughly 15 application series
(the runtime gauges and drop counters); the per-rig counters begin emitting once a rig
does work. A 5-rig fleet under load lands near 600 series including `go_*`/`process_*`.

## Label discipline

Every label value is a closed enum or a bounded identity (`rig` = the rig **ID**, never
its display name; `remote` = the remote's **ID**, never its name; `share` = the share
name). No filename, target name, error string, or source address is ever a label. A rig
deleted through the API has its counter series removed; its state-gauge series simply
stop appearing. A removed remote likewise has every `remote`-labelled series removed.

## Transfer pipeline

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `transfer_outcomes_total` | counter | `rig`, `outcome` | One increment per frame transfer attempt, at its terminal outcome. |
| `transfer_bytes_total` | counter | `rig` | Bytes streamed from a rig over SMB, counted on **every** exit where bytes moved (including failed transfers), not only on success. |
| `transfer_bytes_wasted_total` | counter | `rig` | Bytes streamed then **discarded** because an identical NAS copy already existed. A rig re-pulling files it already has drives this up. |
| `transfer_duration_seconds` | histogram | `rig` | Per-frame transfer time, measured **after** admission and observed **only** for a filed frame (so queueing and failures do not distort it). |
| `admission_wait_seconds` | histogram | `rig` | Time a pull spent waiting in the admission gate. |
| `frames_filed_total` | counter | `rig`, `class`, `disposition` | Frames filed to the NAS. |
| `frame_upsert_failures_total` | counter | `rig` | Frames that transferred and verified but whose DB row failed to persist — the worst outcome in the pipeline. Alert on any nonzero rate. |
| `frames_deleted_total` | counter | `rig`, `manner` | Source frames closed out. `device_deleted` is the ordinary post-grace delete of a proven original; `culled` is an original removed because its NAS copy was deleted out of band and the mark settled (the reconciliation path — worth its own alert); `culled_acked` is the same removal after the operator acknowledged the cull, waiving the settle window; `source_vanished` closed a row whose source was already gone, with no delete issued. |
| `delete_blocked_total` | counter | `rig`, `reason` | Delete-pass frames held or deferred instead of deleted. The first three reasons are held frames carrying a critical alert; `media_unconfirmed`, `source_changed_pre_delete`, `source_changed_deferred`, `identity_unconfirmed` and `hash_identity_unconfirmed` are quiet deferrals that raise nothing. `hash_identity_mismatch` and `hash_identity_unconfirmed` count a pre-delete digest measured in a session of its own (a remote's) whose identity probe found a different device, raising the critical `identity_mismatch` alert, or got no answer; either ends the delete slice. |
| `delete_pass_yield_total` | counter | `rig` | Delete slices that reached their deadline and yielded to the next cycle with eligible frames left. A steady rate means the rig is draining a backlog in slices between listings. |
| `delete_hash_interrupted_total` | counter | `rig` | Pre-delete device re-hashes abandoned between read buffers because a Pause was requested or the worker was shutting down; the frame is deferred and re-hashed in full on a later pass. Each Pause during cleanup adds at most one. A steady climb with no Pause behind it means the interrupt is firing when it should not, and deletes are stalling. |
| `frames_lost_total` | counter | `rig`, `cause` | Genuinely lost frames — source gone **and** the NAS copy present-but-**corrupt** (`cause=nas_corrupt`), a critical alert with no DB trace but this counter. A doubly-vanished frame whose NAS copy is merely missing or truncated is treated as a routine both-sides cull instead: it is marked deleted (`frames_deleted_total{manner="source_vanished"}`) with a warn alert, not counted here. The `nas_missing`/`nas_truncated` causes are reserved: no code path records them. |
| `nas_restores_total` | counter | `rig`, `result` | NAS-copy restores — a truncated copy re-pulled during the delete pass, or an operator-requested restore of a missing copy. A restore whose device content no longer matches records `result=held` (and `delete_blocked_total{reason="restore_mismatch"}`). |
| `refile_frames_total` | counter | `result` | Archived files moved to where the current folder templates put them (the re-file tool on the Tools page). Counted **per file, not per row** — a frame present on two device shares is two rows sharing one file and one move. A re-file is a move, not a filing, so it never touches `frames_filed_total`. |
| `cycles_total` | counter | `rig`, `result` | Rig worker cycles by result. |
| `cycle_duration_seconds` | histogram | `rig`, `phase` | Cycle duration by phase. `phase="delete"` is one delete slice, bounded by its deadline, not a whole backlog's drain. |
| `pull_phase_refused_total` | counter | `rig`, `reason` | Pull passes refused — which also **skips the delete pass**, so a rig can silently stop draining. |
| `listing_entries` | gauge | `rig`, `share` | Entries returned by the last **completed** listing pass for a share. |
| `listing_timestamp_seconds` | gauge | `rig`, `share` | Unix time of that listing. Pairs with `listing_entries`: a frozen count with an old timestamp is stale (a failed pass leaves both untouched), the visible signature of a silently truncated listing. |
| `alerts_raised_total` | counter | `category`, `severity` | Alert rows created: one per new alert, not one per raise of an alert already standing. |

### Enum values

- **`outcome`**: `dest_unavailable`, `disk_reserve`, `admission_aborted`,
  `staging_mkdir_failed`, `temp_create_failed`, `open_source_transient`,
  `open_source_failed`, `copy_failed`, `short_read`, `restat_failed`,
  `source_changed`, `sync_failed`, `close_failed`, `verify_read_failed`,
  `verify_mismatch`, `filed`, `file_failed`, `culled_revision` (the device revised a
  frame whose NAS copy the operator culled: a terminal outcome that deliberately
  files nothing), `panic`.
- **`class`**: `light`, `dark`, `flat`, `bias`, `unknown` (the unclassified filing
  route legitimately produces `unknown`).
- **`disposition`**: `fresh` (a real pull), `adopted` (streamed then discarded for an
  existing identical copy — see `transfer_bytes_wasted_total`), `reclassified` (a local
  rename, zero network bytes).
- **`manner`**: `device_deleted` (a real device delete), `culled` (an out-of-band-cull
  reconciliation delete after the mark settled), `culled_acked` (the same, after the
  operator acknowledged the cull and waived the settle window), `source_vanished` (source
  was already gone; the row was closed out but **nothing was deleted**).
- **`reason`** (delete-blocked): `source_changed`, `three_way_mismatch`,
  `restore_mismatch`, `media_unconfirmed`, `source_changed_pre_delete`,
  `source_changed_deferred` (the delete pass found the source disagreeing with the row
  and deferred rather than holding, up to a bound — the gate reads size and mtime only
  and does not establish why. A device-side revision the pull has not yet resolved is the
  commonest cause and the one the bound exists for, so a sustained rate is worth checking
  `min_file_age` against; a genuinely different file at the same path counts here too,
  until the bound escalates it to `source_changed`. One frame can contribute many
  increments, where `source_changed_pre_delete` contributes at most one),
  `identity_unconfirmed` (the delete slice's device-identity probe got no answer, so
  the whole slice deleted nothing; counted once per slice, not per frame),
  `hash_identity_mismatch` (a pre-delete digest measured in a session of its own, a
  remote's, re-proved identity there and found a different device; raises the critical
  `identity_mismatch` alert and ends the delete slice), `hash_identity_unconfirmed` (the
  same identity probe got no answer; quiet, and it also ends the slice).
- **`cause`** (lost): `nas_missing`, `nas_truncated`, `nas_corrupt`.
- **`result`** (restore): `restored`, `already_present`, `held`, `device_error`,
  `local_error`.
- **`result`** (cycle): `ok`, `unmanaged`, `ctx_cancelled`, `offline`, `panic`.
- **`result`** (re-file): `moved`, `skipped` (already in place, refused, or
  unplaceable), `failed`.
- **`phase`**: `pull`, `delete`, `total`.
- **`reason`** (refused): `session_open_failed`, `identity_foreign`,
  `identity_unconfirmed`.

## State gauges

Read live at scrape time (never stale, no background ticker). Emitted per online rig
where labelled.

| Metric | Labels | Meaning |
|---|---|---|
| `rig_up` | `rig` | 1 if online, else 0. `sum(...)` gives the fleet online count. |
| `rig_paused` | `rig` | 1 if paused. |
| `rig_last_listing_timestamp_seconds` | `rig` | Unix time of the rig's last successful listing. **Absent** until the first listing, so an alert must tolerate absence. |
| `rig_frames_waiting_settle` | `rig` | Frames discovered this cycle not yet past the settle gate. |
| `rig_frames_tonight` | `rig` | Frames filed for the rig tonight. |
| `remote_rig_info` | `rig`, `remote` | Constant 1 for each rig reached through a remote, naming the remote by id. **Absent** for a rig on the local network. A rig's remote is this info series rather than a label on the per-rig series, so join on `rig` to slice them by remote. |
| `device_free_bytes` | `rig`, `share` | Free bytes on a watched device share. |
| `destination_free_bytes` | — | Free bytes on the NAS destination volume. |
| `alerts_unacked` | — | Unacknowledged alert count. |
| `transfers_in_flight` | — | Transfers holding an admission slot. |
| `transfer_in_flight_bytes` | — | Bytes reserved by in-flight transfers. |
| `preview_cache_bytes` / `preview_cache_entries` | — | Preview cache size. |
| `preview_renders_in_flight` | — | Preview renders executing. |
| `hub_subscribers` | — | Live dashboard SSE subscribers. |
| `discovery_devices` | — | Devices in the discovery registry. |

## Remote sites

Per paired remote (see [`docs/design/2026-10-17-remote-sites.md`](design/2026-10-17-remote-sites.md)). Recorded by the remote
supervisor; a removed remote's series are removed.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `remote_heartbeats_total` | counter | `remote`, `result` | Heartbeats (*hello* calls) to a remote. A run of `failed` is a remote whose tunnel is down; the card reads *Reconnecting*, then *Unreachable*. |
| `remote_reconnect_rounds_total` | counter | `remote`, `result` | Broadcast rounds run on a remote's LAN after it (re)connected. The remote's site carries no traffic until one completes, so a climbing `failed` count holds its rigs offline. |
| `remote_client_rebuilds_total` | counter | `remote` | Tunnel clients rebuilt because the heartbeat failed with nothing moving through the tunnel. While a remote is unreachable the rebuilds back off to a capped interval. |
| `remote_warnings_total` | counter | `remote` | Warnings the remote reported (refused forwards, broadcast failures, rejected client keys), each counted once when first seen. The card lists the recent ones. |
| `remote_up` | gauge | `remote` | 1 while the remote's site carries traffic (heartbeat answered and its reconnect round done), else 0. |
| `remote_latency_seconds` | gauge | `remote` | The remote's smoothed round-trip latency, padded by its variation margin, as the timeouts on its site use it. |

### Enum values

- **`result`** (heartbeat, reconnect round): `ok`, `failed`.

## Drop counters

Each counts a different unit of lost work, so they are separate metrics rather than
one labelled counter. Any nonzero rate means the application is shedding work.

| Metric | Labels | Meaning |
|---|---|---|
| `hub_subscribers_dropped_total` | — | A dashboard SSE subscriber dropped for overflowing its event queue (it then receives nothing until it reconnects). |
| `log_entries_dropped_total` | — | A log entry dropped for a full log-stream subscriber channel. |
| `worker_commands_dropped_total` | `rig` | A control command dropped for a full worker command channel (a wedged worker). |
| `preview_warms_dropped_total` | — | A background preview render shed because the render pool was busy (latency, not data loss). |

## Runtime identity

| Metric | Labels | Meaning |
|---|---|---|
| `build_info` | `version`, `go_version` | Constant 1; join dashboards against the running version. |
| `start_time_seconds` | — | Process start (metrics construction) time. |

## Semantic caveats

These are correct-by-design and will otherwise be misread:

- **`alerts_raised_total` counts alert rows created, not raise calls.** Most conditions are
  re-raised every cycle while they hold; the store absorbs a raise into the row that still
  stands (an unacknowledged event, or a standing condition until it is resolved, even after
  it is acknowledged — design architecture §10), and only a created row counts. The dedup is
  **not severity-aware**: the same `(rig, category, message)` raised at a different severity
  lands on the first row and counts nothing, so the counter's `severity` split can disagree
  with a later severity the condition was raised at.
- **`frames_deleted_total{manner="source_vanished"}` issued no delete** — it records a
  row closed out because the source was already gone.
- **`transfer_bytes_wasted_total`** is bytes paid for over SMB and then thrown away, a
  real and otherwise-invisible failure mode (a rig repeatedly re-pulling existing files).
- **`preview_cache_bytes`** may drift slightly below actual on-disk bytes: the cache
  decrements its total when it selects an eviction victim, whether or not the later file
  removal succeeds.
- **`go_sql_*`** metrics do not carry the `asiair_commander_` namespace (library-fixed).

## Example queries and alerts

```promql
# A rig has not listed in 15 minutes (wedged worker, dead SMB session, or off the net).
# The gauge is absent until a rig's first listing, so this does not fire for a
# rig that has never listed; pair it with absent() on a specific rig to catch that.
(time() - asiair_commander_rig_last_listing_timestamp_seconds) > 900

# A rig is silently not draining: pulls are being refused.
rate(asiair_commander_pull_phase_refused_total[15m]) > 0

# Transfer failure rate (any outcome other than the three terminal filing results).
sum by (rig) (rate(asiair_commander_transfer_outcomes_total{outcome!~"filed|file_failed|culled_revision"}[15m]))

# The worst outcome: a verified frame whose row never persisted.
increase(asiair_commander_frame_upsert_failures_total[1h]) > 0

# The application is losing work anywhere.
sum(rate(asiair_commander_hub_subscribers_dropped_total[15m]))
  + sum(rate(asiair_commander_log_entries_dropped_total[15m]))
  + sum(rate(asiair_commander_worker_commands_dropped_total[15m])) > 0

# Bytes being wasted re-pulling files already on the NAS.
rate(asiair_commander_transfer_bytes_wasted_total[1h]) > 0

# NAS destination headroom (bytes).
asiair_commander_destination_free_bytes

# Fleet online count.
sum(asiair_commander_rig_up)

# A remote's tunnel has been down for 15 minutes.
max_over_time(asiair_commander_remote_up[15m]) == 0

# Rigs online, by the remote they are reached through.
sum by (remote) (asiair_commander_rig_up * on (rig) group_left (remote) asiair_commander_remote_rig_info)
```
