package model

// Severity ranks an alert. The UI is the only notification surface (design
// architecture §10); alerts are problem-only.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// Category names an alert class. It is a defined type so the value can be used as
// a bounded metric label without laundering user-authored text into a label — the
// full set is closed to the constants below by convention (the type alone does not
// forbid Category("anything"); the metrics seed test enforces the closed set).
type Category string

// Alert categories (design architecture §10). The category drives grouping and
// the capacity-rig escalation.
const (
	AlertTransferFailed   Category = "transfer_failed"
	AlertVerifyMismatch   Category = "verify_mismatch"   // at-rest verify failed
	AlertDeleteMismatch   Category = "delete_mismatch"   // delete-pass three-way check failed → held
	AlertUnclassified     Category = "unclassified"      // unknown IMAGETYP / missing routing-critical / parse fail
	AlertRotClash         Category = "rot_clash"         // canonical-name content clash (possible rot, design frame-lifecycle §8)
	AlertBacklog          Category = "backlog"           // never-deletable backlog; escalates on capacity rigs
	AlertNASDiskLow       Category = "nas_disk_low"      // NAS destination below disk_reserve_bytes
	AlertDeviceDiskLow    Category = "device_disk_low"   // a device's own free space low
	AlertUnreachable      Category = "unreachable"       // managed rig unreachable longer than expected
	AlertFrameLost        Category = "frame_lost"        // doubly-vanished frame (source + NAS copy gone)
	AlertIdentityMismatch Category = "identity_mismatch" // a different device answered at this rig's address (probe)

	AlertIdentityUnconfirmed    Category = "identity_unconfirmed"    // no scan_air reply, so device identity can't be confirmed
	AlertMediaChanged           Category = "media_changed"           // removable medium swapped: its media marker no longer matches the adopted one
	AlertDestinationUnavailable Category = "destination_unavailable" // NAS destination unreadable (unmounted/dropped), not merely full
	AlertClockSkew              Category = "clock_skew"              // large wall/monotonic divergence
	AlertConfig                 Category = "config"                  // configuration problem surfaced at runtime

	AlertLibraryReconciled  Category = "library_reconciled"  // frames removed outside the app, marked missing (one per directory)
	AlertLibraryUnavailable Category = "library_unavailable" // a NAS destination root unlinked or failing for 6 h
	AlertLibraryMismatch    Category = "library_mismatch"    // a root's marker parses but its id differs from the recorded id

	AlertRefile Category = "refile" // a re-file move left a frame's file and row disagreeing (rename-back failed)

	// Remote bridge alerts. The remote-level ones carry no rig id; their message
	// starts with RemoteAlertPrefix, which names the remote, so the one alert per
	// remote is found again to auto-acknowledge it after a rename.
	AlertRemoteUnreachable Category = "remote_unreachable" // a remote's tunnel has been down past unreachable_alert_after
	AlertRemoteStalled     Category = "remote_stalled"     // a remote rig keeps failing pulls or deletes while its tunnel is up
	AlertRemotePairing     Category = "remote_pairing"     // another remote is answering on this remote's pairing
	AlertRemoteForeignKey  Category = "remote_foreign_key" // the remote rejected a client key other than Commander's: the address leaked
	AlertRemoteBuildStale  Category = "remote_build_stale" // the remote's build trails Commander's by more than about two weeks
)

// RemoteAlertPrefix starts the message of every remote-level alert of the
// remote with this id.
func RemoteAlertPrefix(id string) string { return "remote " + id + ": " }

// HoldReason identifies why a frame was held during the delete pass. It is a
// bounded metric-label value carried alongside the free-text alert message.
type HoldReason string

const (
	HoldSourceChanged    HoldReason = "source_changed"     // source present but size/mtime changed since filing
	HoldThreeWayMismatch HoldReason = "three_way_mismatch" // device/NAS/recorded hash disagreement
	HoldRestoreMismatch  HoldReason = "restore_mismatch"   // device content changed during a NAS restore
)

// FailReason identifies why a pull failed terminally. Also a bounded metric label.
type FailReason string

const (
	FailPanic      FailReason = "panic"       // a panic recovered during transfer
	FailOpenSource FailReason = "open_source" // a non-network failure opening the source
)

// Alert is one row of the alerts table (design architecture §7). AckedAt == 0
// means unacknowledged.
type Alert struct {
	ID        int64    `json:"id"`
	RigID     string   `json:"rig_id"`
	Severity  Severity `json:"severity"`
	Category  Category `json:"category"`
	Message   string   `json:"message"`
	CreatedAt int64    `json:"created_at"` // unix seconds
	AckedAt   int64    `json:"acked_at"`   // unix seconds; 0 = unacked

	// Standing marks a raise of a condition that is re-checked every cycle and
	// retired by its owner's resolve path (Store.ResolveAlerts). Its row stays the
	// one alert for that condition until it is resolved, acknowledged or not, so
	// acknowledging it silences it while the condition persists (design
	// architecture §10). It steers the raise only and is not persisted.
	Standing bool `json:"-"`
}
