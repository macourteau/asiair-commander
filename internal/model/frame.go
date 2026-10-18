package model

// FrameState is the lifecycle state of a frame row (design architecture §7 DDL).
type FrameState string

const (
	// StateVerified: filed and read back OK on the NAS. Deletion-eligible once
	// grace elapses.
	StateVerified FrameState = "verified"
	// StateDeleted: source removed after the three-way integrity check.
	StateDeleted FrameState = "deleted"
	// StateUnclassified: filed to the quarantine area, never deleted (unknown
	// IMAGETYP, missing routing-critical header, or FITS-parse failure).
	StateUnclassified FrameState = "unclassified"
	// StateHeld: a delete-time integrity mismatch — kept, alerted, never deleted.
	StateHeld FrameState = "held"
	// StateFailed: a transfer/verify/parse failure; retried with backoff (design
	// frame-lifecycle §4).
	StateFailed FrameState = "failed"
	// StateDismissed: an operator discarded a permanently-stuck unclassified frame.
	// The row is a tombstone (kept so the file is not re-pulled while unchanged); its
	// NAS quarantine copy has been purged; it no longer counts toward the backlog.
	StateDismissed FrameState = "dismissed"
)

// ProvenArchived reports whether a frame in this state has been pulled and
// byte-verified onto the NAS — `verified` (filed to its real home) or
// `unclassified` (byte-verified but quarantined). Both carry a recorded sha256
// and both have an untouched device original that still hashes to `f.SHA256`, so
// both are eligible for the culled-original drain and for per-frame acknowledge.
// `held` (device bytes mismatched) and `deleted` (no original left) are excluded:
// they are never cull-deletable or acknowledgeable.
func (s FrameState) ProvenArchived() bool {
	return s == StateVerified || s == StateUnclassified
}

// Sidecar is a non-transferred companion file (e.g. an ASIAIR _thn.jpg preview)
// associated to a frame, deleted from the device alongside it (design
// frame-lifecycle §3). Serialized as JSON in the frames.sidecars column.
type Sidecar struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // unix seconds
}

// Frame is one row of the frames table (design architecture §7 DDL). Nullable
// columns use a 0 sentinel that the store maps to/from SQL NULL: Night >= 1
// when set, and the timestamps are unix seconds (never legitimately 0).
type Frame struct {
	ID          int64  `json:"id"`
	RigID       string `json:"rig_id"`
	SourceShare string `json:"source_share"`
	SourcePath  string `json:"source_path"`

	// SHA256, Size and Mtime describe the DEVICE copy as last proven: its
	// whole-file at-rest hash (design frame-lifecycle §6), length and modification
	// time. They are mutable — the ASIAIR revises a finished frame in place and a
	// supersede advances all three — and only device-side checks read them (design
	// frame-lifecycle §11.1).
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"` // unix seconds

	// DataSHA256 is the exposure's identity: the SHA-256 of the file from its FITS
	// data offset to EOF. The device's revision edits only the header, so this
	// survives it — which is what lets a revision of an exposure already pulled be
	// told apart from a different exposure written at the same path. It is
	// side-independent (both copies carry the same data unit) and fixed for the
	// exposure's life, so UpsertFrame preserves a recorded value rather than
	// overwriting it. "" = not recorded (design frame-lifecycle §11.3).
	DataSHA256 string `json:"data_sha256"`

	// ArchiveSHA256 and ArchiveSize describe the ARCHIVE copy: what the file at
	// DestPath must hash to, and the length EVERY archive-side comparison against
	// it uses — VerifyRead's argument among them, and equally the short-circuit
	// sameArchiveAt makes before spending a read-back and the Stat compare inside
	// assertRetargetEdit, neither of them a VerifyRead argument. They are one value
	// with SHA256/Size only while the archive copy is a byte-for-byte image of the
	// device copy, which a device-side revision falsifies: on a row sharing its
	// archive file with another row, Size stays put while ArchiveSize advances
	// (design frame-lifecycle §11.1).
	ArchiveSHA256 string `json:"archive_sha256"`
	ArchiveSize   int64  `json:"archive_size"`

	// DeleteDeferCount is how many consecutive delete passes have deferred this
	// frame because its source no longer agrees with the row. The bound is what
	// keeps a frame mid-revision from being held and alerted on the first sighting
	// while the pull path is still waiting out min_file_age, and holding it on the
	// row rather than in memory is what stops a rig that restarts often from
	// deferring forever. Every write that brings the frame's source back into
	// agreement with its row resets it, so the bound is per episode of
	// disagreement rather than per row lifetime (design frame-lifecycle §11.13).
	DeleteDeferCount int `json:"delete_defer_count"`

	// RetargetSHA256 and RetargetFrom are the retarget AUDIT record: that a
	// re-label happened, and what the frame was labelled before it. "" (SQL NULL)
	// = never retargeted. RetargetSHA256 is the content hash the edit produced, so
	// it advances wherever the edited file is rewritten — an audit record that
	// silently stops matching the file it describes is why this pair carries no
	// archive-side fallback. The live archive-side expectation is
	// ArchiveSHA256, never this pair: archive-side checks do NOT fall back to
	// SHA256, which is mutable (a supersede advances it) and would otherwise move
	// the archive-side expectation of every frame carrying no overlay
	// (design routing-and-filing §9.2, design frame-lifecycle §11.1).
	RetargetSHA256 string `json:"retarget_sha256"`
	// RetargetFrom is the original target string retained for UI/audit across
	// repeated retargets; "" when the frame was never retargeted. The Target column
	// holds the classified target after retargeting (design routing-and-filing
	// §9.2).
	RetargetFrom string `json:"retarget_from"`

	FrameType string  `json:"frame_type"`
	Target    string  `json:"target"`
	Filter    string  `json:"filter"`
	Exptime   float64 `json:"exptime"`
	SetTemp   float64 `json:"set_temp"`
	Gain      int64   `json:"gain"`
	Offset    int64   `json:"offset"`
	Binning   string  `json:"binning"`
	Sensor    string  `json:"sensor"`
	DateObs   string  `json:"date_obs"`
	Night     int64   `json:"night"` // 0 = not a light / unassigned

	// SetTempAbsent records that the frame carried no SET-TEMP card (an uncooled
	// capture), stored as a NULL set_temp. It is inverted so the zero value means
	// present: every row written before absence was recorded holds a number, and a
	// re-file must render <SET-TEMP> from the stored value, never as uncooled, or
	// it would move a cooled 0C frame into the uncooled bucket.
	SetTempAbsent bool `json:"set_temp_absent,omitempty"`

	DestPath string    `json:"dest_path"`
	Sidecars []Sidecar `json:"sidecars"`

	State       FrameState `json:"state"`
	Attempts    int        `json:"attempts"`
	NextRetryAt int64      `json:"next_retry_at"` // unix seconds; 0 = none

	PulledAt  int64  `json:"pulled_at"`  // unix seconds; the grace anchor. 0 = unset
	DeletedAt int64  `json:"deleted_at"` // unix seconds; 0 = not deleted
	Error     string `json:"error"`

	// MissingSeenAt is the unix second at which a library scan first confirmed
	// this frame's NAS copy absent; the settle clock runs from it. 0 = present or
	// never observed missing. The scan clears it when the file reappears
	// (design library-reconciliation).
	MissingSeenAt int64 `json:"missing_seen_at"`
	// RestoreRequestedAt is the unix second at which the operator asked to re-pull
	// this frame's NAS copy from a still-resident device original. 0 = no request.
	// A standing request holds the device original: the delete pass defers on it.
	RestoreRequestedAt int64 `json:"restore_requested_at"`

	// MissingAckedAt is the unix second at which the operator confirmed this frame's
	// NAS copy was culled on purpose. Non-zero waives the reconcile-settle window for
	// this frame — and nothing else; every other delete gate still applies.
	// Meaningful ONLY alongside a live missing mark: read it after testing
	// MissingSeenAt != 0, never on its own (design library-reconciliation §10.5).
	MissingAckedAt int64 `json:"missing_acked_at"`

	// PanelBase, PanelRow and PanelCol are the mosaic pane coordinates parsed from
	// the RAW stored Target — the same string the target column holds, never the
	// space-stripped/trimmed one routing parses, so that the value a filed frame
	// records and the value the store migration backfills agree by construction
	// (design routing-and-filing §7.4). "" / 0 / 0 means the target carries no pane
	// suffix. They are populated whether or not mosaic detection is on: they are
	// derived data nothing routes on, and gating them would make a column's meaning
	// depend on the setting in force when the frame happened to be filed.
	PanelBase string `json:"panel_base"`
	PanelRow  int    `json:"panel_row"`
	PanelCol  int    `json:"panel_col"`

	// Astrometry is the frame's pointing/orientation (frames.astrometry blob).
	// nil = not yet processed; see the Astrometry doc for the tri-state.
	Astrometry *Astrometry `json:"astrometry,omitempty"`
}

// CurrentStoreSchemaVersion is the operational-state (SQLite) schema version this
// binary understands. It is deliberately separate from CurrentSchemaVersion (the
// config schema): the two schemas evolve independently, so welding them to one
// constant would force a store-only migration to spuriously bump the config
// schema (and vice versa), breaking the other's load guard.
const CurrentStoreSchemaVersion = 11
