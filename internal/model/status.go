package model

// SafeToPowerOff is the conservative, non-DB-derived power-off status shown on
// the dashboard (design architecture §8/design frame-lifecycle §14). It may read
// "safe" only when a fresh, successful listing of every configured share proves
// zero un-pulled files and every listed frame is verified on the NAS; otherwise
// "working" or (offline/stale listing) "unknown".
type SafeToPowerOff string

const (
	SafeYes     SafeToPowerOff = "safe"
	SafeWorking SafeToPowerOff = "working"
	SafeUnknown SafeToPowerOff = "unknown"
)

// RigState is the coarse activity of a rig worker for the dashboard.
type RigState string

const (
	RigStateIdle     RigState = "idle"
	RigStatePulling  RigState = "pulling"
	RigStateDeleting RigState = "deleting"
	RigStateOffline  RigState = "offline"
	RigStatePaused   RigState = "paused"
	RigStateError    RigState = "error"
)

// RequestedState is what an operator last asked a rig for, recorded by the hub the
// moment the command is queued. The worker applies a command only between files,
// so the card compares this with the state the worker publishes and shows a
// pending note while the two differ.
type RequestedState string

const (
	RequestRunning      RequestedState = "running"
	RequestPaused       RequestedState = "paused"
	RequestRunUntilIdle RequestedState = "run_until_idle"
)

// CurrentFile is the in-progress transfer shown with a progress percent and the
// live throughput sampled while the bytes flow.
//
// Share and Source carry the full identity of the copying file (its ASIAIR share
// and full path) so the live feed can match it to a row by the same
// rig+share+path key everything else uses; File alone is a basename and collides
// (dual-cam/H12 captures share a basename across folders).
type CurrentFile struct {
	File        string `json:"file"`
	Share       string `json:"source_share,omitempty"`
	Source      string `json:"source_path,omitempty"`
	Pct         int    `json:"pct"`
	BytesPerSec int64  `json:"bytes_per_sec,omitempty"`
}

// Progressed returns the marker at a new percent and throughput, carrying the file
// identity (name, share, source path) through unchanged. Every mid-transfer sample
// rebuilds Current as a fresh value (never mutates in place, to avoid racing the
// SSE marshal); routing them all through here means a sample can never blank the
// identity the live feed matches the transferring row on. The value receiver
// copies the struct, so the caller's marker is untouched.
func (c CurrentFile) Progressed(pct int, bytesPerSec int64) *CurrentFile {
	c.Pct = pct
	c.BytesPerSec = bytesPerSec
	return &c
}

// CleanupProgress is the delete pass's current frame and its position, counted
// across the whole drain rather than one slice: Index is the drain's position
// (the frames it has already deleted, plus this one), and Total is those deletes
// so far plus every frame still eligible on an enabled share, including any this
// drain ends up keeping (a failed check, a deferral). Both therefore carry over
// from one slice to the next and grow if a frame becomes eligible mid-drain. The
// readout clears between slices (the field is nil) and, once a drain ends, the
// next one restarts Index at 1.
type CleanupProgress struct {
	File  string `json:"file"`
	Index int    `json:"index"`
	Total int    `json:"total"`
}

// PendingFrame is a frame the rig has discovered but not transferred this cycle —
// still settling, or its transfer deferred/retrying. It exists only in memory and
// is never persisted (a frames row is created only at a terminal outcome); the
// live feed merges it in client-side. Keyed identically to Frame.source_path and
// CurrentFile.source_path so all three inputs share one join key.
type PendingFrame struct {
	Share  string `json:"source_share"`
	Source string `json:"source_path"`
	Size   int64  `json:"size"`
}

// ShareFree is one watched share's own free space (design architecture §10:
// tracked per share so a full eMMC is never masked by a roomy USB).
type ShareFree struct {
	Share string `json:"share"`
	Free  int64  `json:"free"`
}

// FramePrediction is the per-rig "next frame expected in ~t-XXs" estimate the
// backend computes and the UI ticks down client-side. It is driven entirely by
// device-side capture time (never the host filed time), so a backlog burst filed
// at once never reads as a fast cadence. State is a two-value machine
// ("predicting"/"unknown"); the field is published only while predicting
// (omitted otherwise), so an absent Prediction means Unknown.
type FramePrediction struct {
	// State is "predicting" while a live cadence is known; "unknown" otherwise.
	State string `json:"state"`
	// IntervalSec is the predicted capture-to-capture cadence — the median of the
	// recent frames' gaps (device-domain, so offset-immune), or the exposure alone
	// when only one frame seeds it.
	IntervalSec float64 `json:"interval_sec"`
	// LastExposureSec is the newest light's exposure (the readout's "600s" label and
	// the basis for the liveness/expiry window).
	LastExposureSec float64 `json:"last_exposure_sec"`
	// NextExpectedTS is the estimated arrival of the next frame in the HOST clock
	// domain (unix seconds), so the browser counts down against its own clock. The
	// device-domain anchor is corrected by the per-session clock offset server-side.
	NextExpectedTS int64 `json:"next_expected_ts"`
	// SampleCount is the number of recent light captures the estimate rests on; 1
	// flags the low-confidence exposure-only fallback the UI renders softly.
	SampleCount int `json:"sample_count"`
}

// DeviceFrames counts the frames still on a rig's device by why each one stays
// (design frame-lifecycle §14.5). The SPA picks the words; the verdict never reads
// these counts.
type DeviceFrames struct {
	// InGrace are verified, not culled, and still inside the grace period.
	InGrace int `json:"in_grace"`
	// Due are verified, not culled, and past grace: the next delete pass takes them.
	Due int `json:"due"`
	// Culled are verified or unclassified frames marked missing from the library,
	// leaving through the cull drain.
	Culled int `json:"culled"`
	// Stuck are never removed automatically: held, failed, and unclassified not
	// culled (the backlog alert's frames).
	Stuck int `json:"stuck"`
	// Kept is, on a keep-originals rig, what InGrace, Due and Culled would count;
	// those three are then zero.
	Kept int `json:"kept"`
	// Drained is true when every count is zero, nothing awaits copying, and the
	// rig deletes: the device's space is free.
	Drained bool `json:"drained"`
}

// RigStatus is one rig's live card on the dashboard (design architecture §8
// snapshot payload). It is an immutable value; the hub rebuilds the whole
// snapshot on each publish.
type RigStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Address is the rig's resolved/last-known device IP, published even while
	// offline so the UI can offer a click-to-open SMB shortcut. Best-effort:
	// empty until the address has been resolved at least once. It is the bare IP
	// on the network Site names, never an endpoint's stored form.
	Address       string       `json:"address,omitempty"`
	State         RigState     `json:"state"`
	Online        bool         `json:"online"`
	Paused        bool         `json:"paused"`
	Current       *CurrentFile `json:"current,omitempty"`
	FramesTonight int          `json:"frames_tonight"`
	Shares        []ShareFree  `json:"shares"`
	// DeviceFrames says what is still on the device and why, one count per reason
	// (design frame-lifecycle §14.5); nil until a listing completes.
	DeviceFrames   *DeviceFrames  `json:"device_frames"`
	KeepOriginals  bool           `json:"keep_originals"` // rig never deletes from the device
	SafeToPowerOff SafeToPowerOff `json:"safe_to_power_off"`
	// PowerOffReason explains an Unknown safe-to-power-off verdict so the UI can show
	// a truthful, actionable line instead of one hardcoded "offline or stale" string.
	// It is set only when the verdict is SafeUnknown; empty for safe or working. One
	// of: paused, offline, offline_during_run (a run until idle ended because the rig
	// went offline), identity, missing_on_device, removable_absent, listing_failed,
	// remote (the tunnel of the rig's remote is not up).
	PowerOffReason string `json:"power_off_reason"`
	// SafeAsOf is the unix-seconds time of the listing the SafeToPowerOff verdict was
	// computed from, and 0 when no listing produced it (a Pause or a suspended
	// listing's paused verdict, an offline cycle, a refused identity). A run until idle that ends leaves its last listing's verdict
	// on a paused rig, which runs no listing, so the card says how old it is.
	SafeAsOf int64 `json:"safe_as_of,omitempty"`
	// EmptySlots names the shares whose removable slot the most recent listing
	// STRUCTURALLY confirmed empty (no medium, so no frames to strand). It gates the
	// per-share disable guard, which permanently drops a share from the verdict, so
	// it must never include a frame-bearing share hit by a transient read error —
	// only the structural-absence branch populates it, never mediumUsable's
	// present=false. Per-cycle like the verdict: rebuilt each listing and empty on an
	// offline/partial cycle, never a stale membership.
	EmptySlots []string `json:"empty_slots"`
	ListingTS  int64    `json:"listing_ts"` // unix seconds of the last successful listing
	// NextPollAt is the unix-seconds wall time the worker will run its next cycle,
	// letting the UI show a live "next check in Xs" countdown while a rig is idle.
	// Best-effort; 0/omitted when the next cycle time is not known.
	NextPollAt int64 `json:"next_poll_at,omitempty"`
	// PollIntervalSec is the whole-second poll cadence backing NextPollAt, so the UI
	// can render the next-check countdown as a depleting fraction (remaining ÷
	// interval). Best-effort; 0/omitted when the next cycle time is not known.
	PollIntervalSec int `json:"poll_interval_sec,omitempty"`
	// Draining is true while a delete backlog drains in slices: the last delete
	// slice stopped at its deadline with eligible frames left, so the next cycle
	// follows in about a second rather than one poll interval, and a "next check"
	// countdown would be meaningless. Cleared by any cycle that runs no yielding
	// slice, and at once by a pause.
	Draining bool `json:"draining,omitempty"`
	// WaitingToSettle is the number of frames discovered on the device this cycle but
	// not yet past the settle/quiescence gate (min_file_age + stable size/mtime), so
	// the UI can show a plain-language "still saving" liveness line. Omitted at 0.
	WaitingToSettle int `json:"waiting_to_settle,omitempty"`
	// Pending is every pull-candidate the rig reached no terminal outcome for this
	// cycle (settling, admission-deferred, or transiently retrying) — the live feed's
	// ephemeral pending set. Never persisted. Omitted when empty.
	Pending []PendingFrame `json:"pending,omitempty"`
	// PendingListed is true only on the one authoritative end-of-listing snapshot of a
	// fully-listed, non-paused cycle. It gates client reconciliation: because omitempty
	// makes a complete-empty Pending indistinguishable from an absent one, the client
	// reconciles/removes pending rows only when this is true. False on every mid-cycle,
	// paused, offline, or partially-listed publish.
	PendingListed bool `json:"pending_listed,omitempty"`
	// Prediction is the next-frame cadence estimate, present only while a live
	// cadence is known (Predicting); absent means Unknown (no usable recent frames,
	// or the newest capture is outside the liveness window).
	Prediction *FramePrediction `json:"prediction,omitempty"`
	LastError  string           `json:"last_error,omitempty"`
	// MissingWarning explains why SafeToPowerOff was downgraded to Unknown because
	// a frame is missing from the NAS library while its device source is still
	// listed (the card holds the only copy). Empty when no such frame exists. It
	// rides its own field, not LastError, which publishListingStatus clears.
	MissingWarning string `json:"missing_warning,omitempty"`
	// ReconcileSettleSec is this rig's effective settle window (seconds) before a
	// device original may be deleted for a missing frame, so the UI can show the
	// per-row deletion deadline. Per-device, mirroring grace.
	ReconcileSettleSec int64 `json:"reconcile_settle_sec,omitempty"`
	// LastLibraryScan is the most recent out-of-band-deletion scan result, per
	// destination root. Replaced wholesale on each publish, never mutated in place.
	LastLibraryScan *LibraryScanResult `json:"last_library_scan,omitempty"`
	// RunUntilIdle is true while a Run now on a paused rig is running until no more
	// work needs doing: the rig stays paused, yet lists, pulls and deletes each cycle
	// like an unpaused one until the run ends. Not persisted: a restart leaves the rig
	// simply paused.
	RunUntilIdle bool `json:"run_until_idle,omitempty"`
	// Requested is what the operator last asked for, set by the hub, never the
	// worker, so it shows the instant a command is queued. The worker honours a
	// command only between files, and on a slow link one file can take a minute or
	// more; while Requested differs from the published state the card shows the
	// change as pending rather than offering the same action again.
	Requested RequestedState `json:"requested"`
	// Cleanup is the delete pass's position while it runs, so a long pass (each
	// frame re-reads its whole device original to hash it) reads as progress
	// rather than as a stall. Absent outside the delete pass.
	Cleanup *CleanupProgress `json:"cleanup,omitempty"`
	// Site is the id of the site the rig's last resolved endpoint names, "" for
	// the local network. It comes from the rig's last-known endpoint, so a rig
	// keeps its remote while its ASIAIR is powered off.
	Site string `json:"site,omitempty"`
	// RemoteName is the display name of the remote Site names, "" on the local
	// network or for a site no paired remote has.
	RemoteName string `json:"remote_name,omitempty"`
	// InSession is true from the probe that reached the device until the end of
	// that cycle: the worker holds a live path to the device.
	InSession bool `json:"in_session,omitempty"`
	// Remote is attached by the hub while the tunnel of the rig's remote is not
	// up (Reconnecting, Unreachable or Waiting), and nil otherwise; it is
	// display only, and never changes what the worker wrote.
	Remote *RigRemote `json:"remote,omitempty"`
	// LastKnown is what the rig's last listing concluded. The hub publishes it
	// only while the card reads its remote's state instead of the rig's
	// (Remote.Down), so the card can say what was true when it last knew.
	LastKnown *LastKnownState `json:"last_known,omitempty"`
	// RefusedForwards are the refused forwards the rig's remote reports for
	// this rig's address, oldest first.
	RefusedForwards []RemoteWarning `json:"refused_forwards,omitempty"`
}

// PowerOffRemote is the PowerOffReason of a rig whose verdict is Unknown
// because its remote's tunnel is not up.
const PowerOffRemote = "remote"

// RigRemote is how a rig's remote stands while its tunnel is not up.
type RigRemote struct {
	// State is the remote's: reconnecting, unreachable or waiting.
	State RemoteState `json:"state"`
	// LastHelloAt is the remote's last successful hello (unix seconds), 0 if
	// none: "unreachable since", "last seen".
	LastHelloAt int64 `json:"last_hello_at,omitempty"`
	// Down is true once the worker has written the rig's remote-down state
	// (offline, Safe to power off Unknown for PowerOffRemote): the card reads
	// the remote's state instead of the rig's, and LastKnown says what the rig
	// last reported. A rig with a live session, or a paused one, is not Down
	// and shows its own state, reached via the remote.
	Down bool `json:"down"`
}

// LastKnownState is the verdict of a rig's last listing and when it was made.
type LastKnownState struct {
	Safe SafeToPowerOff `json:"safe_to_power_off"`
	// Reason explains an Unknown verdict, as PowerOffReason does.
	Reason string `json:"power_off_reason,omitempty"`
	// At is the listing's time (unix seconds).
	At int64 `json:"at"`
	// Pending is the frames that listing found still on the device awaiting
	// work: not copied yet, settling, held or failed.
	Pending int `json:"pending"`
}

// LibraryScanResult is one rig's most recent library scan, per destination root.
type LibraryScanResult struct {
	At    int64            `json:"at"`
	Roots []RootScanResult `json:"roots"`
}

// RootScanResult is one destination root's outcome in a library scan.
type RootScanResult struct {
	Root          string   `json:"root"`
	Linked        bool     `json:"linked"`
	Reason        string   `json:"reason,omitempty"`
	NewlyMissing  int      `json:"newly_missing"`
	Cleared       int      `json:"cleared"`
	DirsScanned   int      `json:"dirs_scanned"`
	Directories   []string `json:"directories"` // newly-marked only, first 50, most recent first
	UnscannedDirs int      `json:"unscanned_dirs"`
}

// MissingGroup is a true per-directory count of one missing-panel section's
// frames, for the folder stack (not page-local). Ackable is how many of them the
// group-acknowledge challenge would bind, so the button and the challenge agree
// (design library-reconciliation §13.2).
type MissingGroup struct {
	Dir     string `json:"dir"`
	Count   int    `json:"count"`
	Ackable int    `json:"ackable"`
}

// Snapshot is the single immutable whole-fleet value pushed over SSE
// latest-wins (design architecture §8). Handlers and SSE only ever read this
// via an atomic pointer; no shared mutable map escapes the hub.
type Snapshot struct {
	Rigs          []RigStatus `json:"rigs"`
	NASFree       int64       `json:"nas_free"`
	BytesLast12h  int64       `json:"bytes_last_12h"`
	UnackedAlerts int         `json:"unacked_alerts"`
	// SolverStatus carries the Tier-1 plate-solver provisioning state (binary +
	// catalog download progress). Absent when plate solving is disabled and the
	// provisioner never published.
	SolverStatus *ProvisionStatus `json:"solver_status,omitempty"`
	// Remotes is every paired remote's state, in id order. Absent when none is
	// paired.
	Remotes []RemoteStatus `json:"remotes,omitempty"`
	// Relay is Commander's own reachability of the remotes' relay regions,
	// apart from any remote's state. Absent when no remote is paired.
	Relay *RelayStatus `json:"relay,omitempty"`
	// GeneratedAt is the unix-seconds stamp the snapshot was built (diagnostic).
	GeneratedAt int64 `json:"generated_at"`
}

// CatalogState is one star-catalog's presence and size for the Settings list.
// SizeBytes is the pinned expected size, so an uninstalled row can still show
// "872 MB" before anything is downloaded.
type CatalogState struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	SizeBytes int64  `json:"size_bytes"`
}

// ProvisionStatus is the Tier-1 solver's observable provisioning state, defined
// here (not in internal/platesolve) so the hub can carry it without a
// hub↔platesolve import cycle. It rides the existing status stream, so the UI
// needs no dedicated read endpoint.
type ProvisionStatus struct {
	State       string         `json:"state"`                 // "disabled"|"provisioning"|"ready"|"failed"
	Detail      string         `json:"detail,omitempty"`      // human note (e.g. a download error)
	Catalogs    []CatalogState `json:"catalogs"`              // all catalogs, installed-or-not, for the Settings list
	Downloading string         `json:"downloading,omitempty"` // catalog name in flight
	GotBytes    int64          `json:"got_bytes,omitempty"`
	TotalBytes  int64          `json:"total_bytes,omitempty"`
}

// DiscoveredRig is a device found by the scan_air discovery registry (design
// architecture §3), offered in the UI. Devices are keyed by their stable guid;
// Managed/RigID are set by matching the live config so an already-managed rig is
// shown as managed rather than re-offered. The camelCase cpuId JSON tag matches
// the wire protocol and the SPA's types; everything else stays snake_case.
type DiscoveredRig struct {
	GUID  string `json:"guid"`
	CPUID string `json:"cpuId"`
	Model string `json:"model"`
	// SSID is the device's own Wi-Fi name. It is the only field in a scan_air
	// reply that distinguishes two devices of the same generation to their
	// owner, so the UI names a found rig by it.
	SSID    string `json:"ssid"`
	Address string `json:"address"`
	// Site is the id of the site the device was seen through, "" for the local
	// network; Address is the bare IP on that site.
	Site      string   `json:"site"`
	Shares    []string `json:"shares"` // enumerated storage shares (filled on adopt)
	Online    bool     `json:"online"`
	Managed   bool     `json:"managed"`
	RigID     string   `json:"rig_id"`
	FirstSeen int64    `json:"first_seen"`
}
