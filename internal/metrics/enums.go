package metrics

// This file is the single source of truth for every metric label value. Each label
// is a defined string type with an exhaustive All* slice; the seed fixture
// (SeedAllForTest) drives every value, and TestLabelValuesAreBounded asserts the
// gathered exposition contains no value outside these sets. That is what keeps
// design architecture I1 (bounded labels) honest — the Go type does not forbid an
// arbitrary value, so the test does.

// TransferOutcome labels transfer_outcomes_total: one value per exit path of the
// rig worker's transfer function (design Phase B). The worker enumerates its exits
// against this set.
type TransferOutcome string

const (
	OutcomeDestUnavailable     TransferOutcome = "dest_unavailable"      // admission returned ErrDestinationUnavailable
	OutcomeDiskReserve         TransferOutcome = "disk_reserve"          // admission returned ErrDiskReserve (headroom exhausted)
	OutcomeAdmissionAborted    TransferOutcome = "admission_aborted"     // admission failed for another reason (ctx cancel)
	OutcomeStagingMkdirFailed  TransferOutcome = "staging_mkdir_failed"  // could not create the staging dir
	OutcomeTempCreateFailed    TransferOutcome = "temp_create_failed"    // could not create the staging temp file
	OutcomeOpenSourceTransient TransferOutcome = "open_source_transient" // network error opening the source; will retry
	OutcomeOpenSourceFailed    TransferOutcome = "open_source_failed"    // non-network error opening the source
	OutcomeCopyFailed          TransferOutcome = "copy_failed"           // io.Copy failed mid-stream
	OutcomeShortRead           TransferOutcome = "short_read"            // streamed byte count != expected size
	OutcomeRestatFailed        TransferOutcome = "restat_failed"         // could not re-stat the source after the copy
	OutcomeSourceChanged       TransferOutcome = "source_changed"        // source size/mtime moved during the pull
	OutcomeSyncFailed          TransferOutcome = "sync_failed"           // fsync of the staged temp failed
	OutcomeCloseFailed         TransferOutcome = "close_failed"          // close of the staged temp failed
	OutcomeVerifyReadFailed    TransferOutcome = "verify_read_failed"    // could not read back the staged temp to verify
	OutcomeVerifyMismatch      TransferOutcome = "verify_mismatch"       // at-rest hash != stream hash
	OutcomeFiled               TransferOutcome = "filed"                 // filing succeeded
	OutcomeFileFailed          TransferOutcome = "file_failed"           // filing (classify/route/rename) failed
	OutcomeCulledRevision      TransferOutcome = "culled_revision"       // a revision of a culled frame: terminal, and nothing filed
	OutcomePanic               TransferOutcome = "panic"                 // a panic recovered in transferGuarded
)

// AllTransferOutcomes is the closed set, in declaration order.
var AllTransferOutcomes = []TransferOutcome{
	OutcomeDestUnavailable, OutcomeDiskReserve, OutcomeAdmissionAborted,
	OutcomeStagingMkdirFailed, OutcomeTempCreateFailed, OutcomeOpenSourceTransient,
	OutcomeOpenSourceFailed, OutcomeCopyFailed, OutcomeShortRead, OutcomeRestatFailed,
	OutcomeSourceChanged, OutcomeSyncFailed, OutcomeCloseFailed, OutcomeVerifyReadFailed,
	OutcomeVerifyMismatch, OutcomeFiled, OutcomeFileFailed, OutcomeCulledRevision,
	OutcomePanic,
}

// FrameClass labels frames_filed_total: the frame type, lowercased, with the empty
// zero value and anything unrecognised mapped to "unknown" so a future IMAGETYP
// cannot leak an unbounded label.
type FrameClass string

const (
	ClassLight   FrameClass = "light"
	ClassDark    FrameClass = "dark"
	ClassFlat    FrameClass = "flat"
	ClassBias    FrameClass = "bias"
	ClassUnknown FrameClass = "unknown"
)

var AllFrameClasses = []FrameClass{ClassLight, ClassDark, ClassFlat, ClassBias, ClassUnknown}

// FrameClassOf maps a model frame-type string (e.g. "LIGHT", "" for unknown) to a
// bounded FrameClass. Unrecognised inputs, including the empty zero value, become
// ClassUnknown.
func FrameClassOf(frameType string) FrameClass {
	switch frameType {
	case "LIGHT", "light":
		return ClassLight
	case "DARK", "dark":
		return ClassDark
	case "FLAT", "flat":
		return ClassFlat
	case "BIAS", "bias":
		return ClassBias
	default:
		return ClassUnknown
	}
}

// FrameDisposition labels frames_filed_total: how the frame reached its filed row.
type FrameDisposition string

const (
	DispositionFresh        FrameDisposition = "fresh"        // a real pull, fresh rename
	DispositionAdopted      FrameDisposition = "adopted"      // pulled over SMB then discarded for an identical existing copy
	DispositionReclassified FrameDisposition = "reclassified" // local rename of an existing NAS file, no network bytes
)

var AllFrameDispositions = []FrameDisposition{DispositionFresh, DispositionAdopted, DispositionReclassified}

// DeleteManner labels frames_deleted_total.
type DeleteManner string

const (
	MannerDeviceDeleted  DeleteManner = "device_deleted"  // a proven original removed from the device
	MannerSourceVanished DeleteManner = "source_vanished" // source already gone; NAS copy verified, row closed (no delete issued)
	MannerCulled         DeleteManner = "culled"          // original removed because its NAS copy was deleted out of band and the mark settled
	MannerCulledAcked    DeleteManner = "culled_acked"    // original removed because the operator acknowledged the cull, waiving the settle window
)

var AllDeleteManners = []DeleteManner{MannerDeviceDeleted, MannerSourceVanished, MannerCulled, MannerCulledAcked}

// DeleteBlockedReason labels delete_blocked_total. The first three correspond to the
// model.HoldReason values (a held frame + critical alert); the rest are quiet paths
// that suppress deletion without an alert.
//
// The two quiet source-changed reasons are kept apart because they count different
// things at different rates, not because either establishes a cause: the pre-delete
// one is a benign post-verify race that fires at most once per frame, while the
// deferred one fires on every delete pass within the gate's bound, many times over
// for a single frame. Summed into one series, one deferring frame would read as a
// score of benign races and the older series would stop meaning what it means.
// Neither says WHY the source disagrees: that gate reads size and mtime only,
// deliberately, so a frame the device is still writing never re-hashes its copy
// there.
type DeleteBlockedReason string

const (
	BlockedSourceChanged          DeleteBlockedReason = "source_changed"
	BlockedThreeWayMismatch       DeleteBlockedReason = "three_way_mismatch"
	BlockedRestoreMismatch        DeleteBlockedReason = "restore_mismatch"
	BlockedMediaUnconfirmed       DeleteBlockedReason = "media_unconfirmed"         // removable medium not confirmed; all deletes for the share deferred
	BlockedSourceChangedPreDelete DeleteBlockedReason = "source_changed_pre_delete" // source moved just before the delete; benign skip
	BlockedSourceChangedDeferred  DeleteBlockedReason = "source_changed_deferred"   // source disagrees with the row; deferred rather than held, up to a bound
	BlockedIdentityUnconfirmed    DeleteBlockedReason = "identity_unconfirmed"      // the delete session's identity probe got no answer; the whole slice deleted nothing
	// The pre-delete digest measured in another session than the delete's
	// (a remote's own) re-proves identity there. A different device answering
	// also raises the critical identity_mismatch alert; no answer is quiet.
	// Either way the slice ends there and deletes nothing more.
	BlockedHashIdentityMismatch    DeleteBlockedReason = "hash_identity_mismatch"
	BlockedHashIdentityUnconfirmed DeleteBlockedReason = "hash_identity_unconfirmed"
)

var AllDeleteBlockedReasons = []DeleteBlockedReason{
	BlockedSourceChanged, BlockedThreeWayMismatch, BlockedRestoreMismatch,
	BlockedMediaUnconfirmed, BlockedSourceChangedPreDelete, BlockedSourceChangedDeferred,
	BlockedIdentityUnconfirmed, BlockedHashIdentityMismatch, BlockedHashIdentityUnconfirmed,
}

// FrameLostCause labels frames_lost_total (doubly-vanished frames).
type FrameLostCause string

const (
	LostNASMissing   FrameLostCause = "nas_missing"
	LostNASTruncated FrameLostCause = "nas_truncated"
	LostNASCorrupt   FrameLostCause = "nas_corrupt"
)

var AllFrameLostCauses = []FrameLostCause{LostNASMissing, LostNASTruncated, LostNASCorrupt}

// NASRestoreResult labels nas_restores_total.
type NASRestoreResult string

const (
	RestoreRestored       NASRestoreResult = "restored"        // re-read the file over the network and re-filed
	RestoreAlreadyPresent NASRestoreResult = "already_present" // a concurrent restore had already placed it
	RestoreHeld           NASRestoreResult = "held"            // device content changed during restore; frame held
	RestoreDeviceError    NASRestoreResult = "device_error"    // transient device-side failure; deferred
	RestoreLocalError     NASRestoreResult = "local_error"     // local FS failure; deferred
)

var AllNASRestoreResults = []NASRestoreResult{
	RestoreRestored, RestoreAlreadyPresent, RestoreHeld, RestoreDeviceError, RestoreLocalError,
}

// CycleResult labels cycles_total.
type CycleResult string

const (
	CycleOK           CycleResult = "ok"
	CycleUnmanaged    CycleResult = "unmanaged"     // profile gone / not managed (early return before the recover defer)
	CycleCtxCancelled CycleResult = "ctx_cancelled" // context cancelled mid-cycle
	CycleOffline      CycleResult = "offline"       // rig unreachable this cycle
	CyclePanic        CycleResult = "panic"         // a panic recovered in runCycle
)

var AllCycleResults = []CycleResult{CycleOK, CycleUnmanaged, CycleCtxCancelled, CycleOffline, CyclePanic}

// CyclePhase labels cycle_duration_seconds.
type CyclePhase string

const (
	PhasePull CyclePhase = "pull"
	// PhaseDelete is the duration of one delete slice: a cycle's deletePhase call,
	// bounded by its deadline, not the whole backlog's drain.
	PhaseDelete CyclePhase = "delete"
	PhaseTotal  CyclePhase = "total"
)

var AllCyclePhases = []CyclePhase{PhasePull, PhaseDelete, PhaseTotal}

// RefuseReason labels pull_phase_refused_total. A refused pull phase makes the caller
// skip the delete pass entirely.
type RefuseReason string

const (
	RefuseSessionOpenFailed   RefuseReason = "session_open_failed"
	RefuseIdentityForeign     RefuseReason = "identity_foreign"
	RefuseIdentityUnconfirmed RefuseReason = "identity_unconfirmed"
)

var AllRefuseReasons = []RefuseReason{RefuseSessionOpenFailed, RefuseIdentityForeign, RefuseIdentityUnconfirmed}

// RefileResult labels refile_frames_total: the outcome of moving one archived
// FILE to where the current templates say it belongs. One increment per file, not
// per row — rows sharing a dest_path share a single move.
type RefileResult string

const (
	RefileMoved   RefileResult = "moved"   // renamed to the new path and committed
	RefileSkipped RefileResult = "skipped" // deliberately not moved (already in place, refused, unplaceable)
	RefileFailed  RefileResult = "failed"  // the move or its commit failed
)

var AllRefileResults = []RefileResult{RefileMoved, RefileSkipped, RefileFailed}

// RemoteCallResult labels the per-remote call counters: whether a heartbeat or
// a reconnect broadcast round succeeded.
type RemoteCallResult string

const (
	RemoteCallOK     RemoteCallResult = "ok"
	RemoteCallFailed RemoteCallResult = "failed"
)

var AllRemoteCallResults = []RemoteCallResult{RemoteCallOK, RemoteCallFailed}
