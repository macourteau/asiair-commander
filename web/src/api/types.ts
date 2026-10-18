/**
 * Wire types — TypeScript mirrors of the Go `internal/model` JSON shapes. These
 * are the single source of truth for the SPA; every field name matches a Go
 * `json:"..."` tag. Keep this file in lockstep with the Go models.
 *
 * Durations are Go-duration strings ("45s", "5m", "1h") — see the `Duration`
 * alias. Timestamps are unix SECONDS unless a field name says nanos.
 */

/** A Go time.Duration serialized as a human string, e.g. "5m", "1h30m", "45s". */
export type Duration = string;

/** The write-only placeholder GET /api/config returns for every secret (model.RedactedSentinel). */
export const REDACTED_SENTINEL = "••••••••";

// ---- status.go -----------------------------------------------------------

/** Conservative power-off status. Render exactly as computed — never invent "safe". */
export type SafeToPowerOff = "safe" | "working" | "unknown";

/** Coarse rig-worker activity. */
export type RigState =
  | "idle"
  | "pulling"
  | "deleting"
  | "offline"
  | "paused"
  | "error";

/** The delete pass's position while it runs; absent outside the pass. index counts
 * every eligible frame the pass considers, including any it keeps, so it is how
 * far the pass has got rather than how many originals it removed. */
export interface CleanupProgress {
  file: string;
  index: number;
  total: number;
}

export interface CurrentFile {
  file: string;
  /** The copying file's share + full path, so the live feed can match it to its row
   * by the same rig+share+path key as every other input (file alone is ambiguous). */
  source_share?: string;
  source_path?: string;
  pct: number;
  /** Live throughput sampled during the copy, bytes/sec. Absent/0 when idle. */
  bytes_per_sec?: number;
}

/**
 * A frame discovered on the device but not yet transferred this cycle (still
 * settling, or its transfer deferred/retrying). Ephemeral: it exists only on the
 * live status and is never persisted. The live feed merges it as a "pending" row.
 * Keyed identically to Frame.source_path / CurrentFile.source_path.
 */
export interface PendingFrame {
  source_share: string;
  source_path: string;
  size: number;
}

export interface ShareFree {
  share: string;
  free: number;
}

/**
 * Next-frame cadence estimate the backend computes and the UI ticks down. Present
 * only while `state` is "predicting"; an absent `prediction` on the rig means
 * Unknown. All timing is backend-computed (offset-corrected, capture-time gated) —
 * the client only counts down `next_expected_ts` against its own clock.
 */
export interface FramePrediction {
  /** "predicting" while a live cadence is known; "unknown" otherwise. */
  state: "predicting" | "unknown";
  /** Predicted capture-to-capture cadence in seconds (median of recent gaps). */
  interval_sec: number;
  /** The newest light's exposure — the "600s" label and liveness-window basis. */
  last_exposure_sec: number;
  /** Estimated next-frame arrival, HOST-domain unix seconds. Counts down client-side. */
  next_expected_ts: number;
  /** Recent captures the estimate rests on; 1 = low-confidence exposure-only fallback. */
  sample_count: number;
}

/** What an operator last asked a rig for (the hub's requested state). */
export type RequestedState = "running" | "paused" | "run_until_idle";

export interface RigStatus {
  id: string;
  name: string;
  /** Resolved/last-known device IP; published even while offline. Absent until resolved. */
  address?: string;
  state: RigState;
  online: boolean;
  paused: boolean;
  /** What the operator last asked for, recorded by the hub the moment the command is
   * queued; while it differs from the published state the change is pending. */
  requested: RequestedState;
  /** A Run now on a paused rig is running until idle; the rig stays paused. */
  run_until_idle?: boolean;
  current?: CurrentFile | null;
  cleanup?: CleanupProgress | null;
  frames_tonight: number;
  shares: ShareFree[];
  /** Human phrase, e.g. "drained", "3 resident", or "3 kept" (keep-originals). */
  drain: string;
  /** The rig never deletes originals from the device, so it never drains/frees space. */
  keep_originals: boolean;
  safe_to_power_off: SafeToPowerOff;
  /** Explains an "unknown" safe-to-power-off verdict so the badge can be truthful and
   * actionable. Set only when the verdict is "unknown"; empty for safe/working. One of:
   * paused, offline, offline_during_run, identity, missing_on_device, removable_absent,
   * listing_failed, remote (the tunnel of the rig's remote is not up). */
  power_off_reason?: string;
  /** Unix seconds of the listing the safe-to-power-off verdict came from; absent when no
   * listing produced it. A paused rig keeps its last run's verdict, and this dates it. */
  safe_as_of?: number;
  /** Shares whose removable slot the last listing STRUCTURALLY confirmed empty (no
   * medium). Gates the per-share disable toggle — only a slot listed here may be
   * disabled. Empty/absent on an offline or partial cycle (never stale). */
  empty_slots?: string[];
  /** Unix seconds of the last successful listing. 0 = never listed. */
  listing_ts: number;
  /** Unix seconds of the worker's next scheduled cycle. Absent/0 when unknown. */
  next_poll_at?: number;
  /** Whole-second poll cadence backing next_poll_at, so the countdown can render as
   * a depleting fraction (remaining ÷ interval). Absent/0 when unknown. */
  poll_interval_sec?: number;
  /** A delete backlog is draining in slices: the next cycle follows in about a
   * second, so the "next check" countdown does not apply. Absent when false. */
  draining?: boolean;
  /** Frames seen on the device but not yet past the settle gate (still saving). Absent at 0. */
  waiting_to_settle?: number;
  /** Every pull-candidate the rig reached no terminal outcome for this cycle — the
   * live feed's ephemeral pending set. Absent when empty. */
  pending?: PendingFrame[];
  /** True only on the one authoritative end-of-listing snapshot of a fully-listed,
   * non-paused cycle. The feed reconciles/removes pending rows only when this is true;
   * an omitted `pending` under it is a genuinely-empty set. Absent otherwise. */
  pending_listed?: boolean;
  /** Next-frame cadence estimate; absent means Unknown (no live cadence). */
  prediction?: FramePrediction | null;
  /**
   * Non-empty when safe_to_power_off was downgraded to "unknown" because library
   * files are missing while a live source could still supply them. Explains the
   * downgrade in plain language.
   */
  missing_warning?: string;
  /** Effective settle window (seconds) before an out-of-band-missing frame is acted on. */
  reconcile_settle_sec?: number;
  /** The most recent library reconciliation scan, when one has run. */
  last_library_scan?: LibraryScanResult;
  last_error?: string;
  /** Id of the remote the rig's last resolved endpoint goes through; absent on the
   * local network. Kept while the rig is powered off, so a rig keeps its remote. */
  site?: string;
  /** Display name of that remote; absent on the local network or for a site no
   * paired remote has. */
  remote_name?: string;
  /** The worker holds a live path to the device (from its probe to the end of the
   * cycle). */
  in_session?: boolean;
  /** Set while the tunnel of the rig's remote is not up; absent otherwise. */
  remote?: RigRemote;
  /** The rig's last listing verdict, published only while `remote.down`. */
  last_known?: LastKnownState;
  /** Forwards the rig's remote refused for this rig's address, oldest first. */
  refused_forwards?: RemoteWarning[];
}

/** How a rig's remote stands while its tunnel is not up (model.RigRemote). */
export interface RigRemote {
  state: RemoteState;
  /** Unix seconds of the remote's last successful hello; absent if none. */
  last_hello_at?: number;
  /** The worker wrote the rig's remote-down state: the card reads the remote's
   * state instead of the rig's. False for a rig with a live session or a paused
   * one, which show their own state reached via the remote. */
  down: boolean;
}

/** What a rig's last listing concluded and when (model.LastKnownState). */
export interface LastKnownState {
  safe_to_power_off: SafeToPowerOff;
  power_off_reason?: string;
  /** Unix seconds of that listing. */
  at: number;
  /** Frames that listing found still on the device awaiting work. */
  pending: number;
}

// ---- remotes ----------------------------------------------------------------

/** A remote's connection state (model.RemoteState). */
export type RemoteState =
  | "waiting"
  | "connected"
  | "reconnecting"
  | "unreachable"
  | "needs_update";

/** One of a remote's recent warnings (model.RemoteWarning). */
export interface RemoteWarning {
  /** Unix seconds on Commander's clock. */
  at: number;
  /** "broadcast_failed" | "refused_forward" | "rejected_client". */
  kind: string;
  proto?: string;
  /** A refused flow's ip:port. */
  target?: string;
  client_key?: string;
  reason?: string;
}

/** One paired remote's published state (model.RemoteStatus). Never carries a secret. */
export interface RemoteStatus {
  id: string;
  name: string;
  state: RemoteState;
  /** The path the remote last reported: true direct, false relayed. */
  direct: boolean;
  /** The relay region the remote last reported reaching Commander through. */
  relay_region?: string;
  /** The relay region fixed at pairing. */
  region: string;
  /** Smoothed round trip in ms; 0 before any hello. */
  latency_ms: number;
  version?: string;
  build_date?: string;
  /** The remote process's uptime as of its last hello (seconds). */
  uptime_s: number;
  restarts: number;
  fingerprint: string;
  /** Unix seconds of the last successful hello under any pairing; absent if none. */
  last_hello_at?: number;
  ever_connected: boolean;
  warnings?: RemoteWarning[];
  /** "remote" | "commander" while a digest algorithm differs. */
  needs_update?: string;
  /** The remote's build trails Commander's by more than about two weeks. */
  build_stale?: boolean;
  pairing_shared?: boolean;
  /** Bandwidth cap in MB/s; absent or 0 when off. */
  rate_limit_mbps?: number;
  created_at: number;
  /** Unix seconds the current pairing was made: the add or the last re-pair. */
  paired_at: number;
}

/** A relay region a remote can be paired on (model.RemoteRegion). */
export interface RemoteRegion {
  id: number;
  code: string;
  name: string;
  /** The region measured as nearest: the default. */
  nearest?: boolean;
}

/** POST /api/remotes and :repair — the only response that ever carries the env value. */
export interface RemotePairing {
  remote: RemoteStatus;
  env_value: string;
  /** The .env file content, ready to save. */
  env_file: string;
  compose_file: string;
  /** Base64 zip of an asiair-remote/ folder holding .env and docker-compose.yml. */
  setup_zip: string;
  fingerprint: string;
}

/** Commander's own reachability of the relay regions (model.RelayStatus). */
export interface RelayStatus {
  state: "up" | "down" | "unknown";
  /** Unix seconds of the last change. */
  since: number;
}

// ---- library reconciliation ---------------------------------------------

/** One library reconciliation scan across a rig's linked roots (model.LibraryScanResult). */
export interface LibraryScanResult {
  /** Unix seconds the scan completed. */
  at: number;
  roots: RootScanResult[];
}

/** One destination root's outcome within a library scan (model.RootScanResult). */
export interface RootScanResult {
  root: string;
  /** Whether the root is linked to a live filesystem the scan could walk. */
  linked: boolean;
  /** Why the root was skipped, when not linked; absent otherwise. */
  reason?: string;
  /** Frames newly found missing this scan. */
  newly_missing: number;
  /** Previously-missing frames whose files reappeared and were cleared. */
  cleared: number;
  dirs_scanned: number;
  directories: string[];
  /** Directories present but not walked this scan (bounded listing). */
  unscanned_dirs: number;
}

/**
 * One destination root's link state (web rootLinkStatus). `linked` is true when the
 * root carries this installation's matching library marker; `reason` explains why it
 * is not, and drives the Settings library section's badge and Re-link control.
 */
export interface RootLinkStatus {
  root: string;
  linked: boolean;
  /**
   * Absent/"" when linked. Otherwise one of: "not linked" (never linked — links
   * automatically on the next filed frame), "marker missing" (recorded but the
   * marker file is gone — a deleted sentinel or an unmounted NAS; re-link to fix),
   * "different library", "marker corrupt", "marker unreadable".
   */
  reason?: string;
}

/** GET /api/library/status — every configured root's current link state. */
export interface LibraryStatus {
  roots: RootLinkStatus[];
}

/** One directory's missing-frame tally in the missing-frames envelope (model.MissingGroup). */
export interface MissingGroup {
  dir: string;
  count: number;
}

/**
 * Process constants announced once per SSE connection (the `server_info` event),
 * not part of the per-frame snapshot. A server update drops the stream, so the
 * reconnect re-announces these — letting the SPA pick up a new version and reset
 * the uptime without a manual refresh.
 */
export interface ServerInfo {
  /** Build-time app version, e.g. "v1.4.2" or "dev" for an untagged build. */
  version: string;
  /** Unix seconds of the server process start; 0 when unknown. */
  started_at: number;
  /**
   * The newest published release when it is newer than `version`, or "" when
   * there is none, none is known yet, or the update check is switched off.
   * Re-announced whenever it changes.
   */
  latest_version: string;
}

/** One Tier 1 star catalog's install state (model.CatalogState). */
export interface CatalogState {
  name: string;
  installed: boolean;
  size_bytes: number; // pinned expected size, so uninstalled rows still show a size
}

/** Tier 1 plate-solver provisioning status (model.ProvisionStatus). */
export interface ProvisionStatus {
  state: string; // "disabled" | "provisioning" | "ready" | "failed"
  detail?: string;
  catalogs: CatalogState[]; // all catalogs (d05/d20/d50), installed-or-not
  downloading?: string; // catalog name currently downloading
  got_bytes?: number;
  total_bytes?: number;
}

export interface Snapshot {
  rigs: RigStatus[];
  nas_free: number;
  /** Total bytes copied to the NAS across the fleet in the trailing 12 hours. */
  bytes_last_12h: number;
  unacked_alerts: number;
  generated_at: number;
  /** Tier 1 plate-solver provisioning status; absent on older backends. */
  solver_status?: ProvisionStatus;
  /** Every paired remote's state; absent when none is paired. */
  remotes?: RemoteStatus[];
  /** Commander's own reachability of the relay; absent when no remote is paired. */
  relay?: RelayStatus;
}

export interface DiscoveredRig {
  /** Stable scan_air device identity — the key the registry and binding use. */
  guid: string;
  /** Hardware SoC serial, camelCase to match the wire protocol. */
  cpuId: string;
  /** Device model string, e.g. "ZWO AirPlus-RK3568 (Linux)". */
  model: string;
  /**
   * The device's own Wi-Fi name. Empty on firmware that omits it. This is the
   * only scan field that distinguishes two devices of the same generation to
   * their owner, so naming and the found-device card lead with it.
   */
  ssid: string;
  address: string;
  /** Id of the remote the device was seen through; "" on the local network. */
  site?: string;
  /** Enumerated storage shares — empty until fetched at adopt time. */
  shares: string[];
  /** Answered a scan within the staleness window. */
  online: boolean;
  /** Already bound to a managed rig profile. */
  managed: boolean;
  /** The managing rig's id when managed, else "". */
  rig_id: string;
  first_seen: number;
}

/** POST /api/discovered/{guid}/shares — adopt-time share enumeration. */
export interface DeviceShares {
  shares: string[];
}

// ---- alert.go ------------------------------------------------------------

export type Severity = "info" | "warn" | "critical";

/** Alert category keys (model.Alert* constants). */
export type AlertCategory =
  | "transfer_failed"
  | "verify_mismatch"
  | "delete_mismatch"
  | "unclassified"
  | "rot_clash"
  | "backlog"
  | "nas_disk_low"
  | "destination_unavailable"
  | "device_disk_low"
  | "unreachable"
  | "frame_lost"
  | "identity_mismatch"
  | "identity_unconfirmed"
  | "library_reconciled"
  | "library_unavailable"
  | "library_mismatch"
  | "clock_skew"
  | "config"
  | "refile"
  | "remote_unreachable"
  | "remote_stalled"
  | "remote_pairing"
  | "remote_foreign_key"
  | "remote_build_stale"
  | (string & {}); // tolerate categories added server-side

export interface Alert {
  id: number;
  rig_id: string;
  severity: Severity;
  category: AlertCategory;
  message: string;
  created_at: number;
  /** Unix seconds; 0 = unacknowledged. */
  acked_at: number;
}

// ---- frame.go ------------------------------------------------------------

export type FrameState =
  | "verified"
  | "deleted"
  | "unclassified"
  | "held"
  | "failed"
  | "dismissed";

/** Named preview sizes served by GET /api/frames/{id}/image?size=. */
export type PreviewSize = "thumb" | "card" | "screen" | "full";

export interface Sidecar {
  path: string;
  size: number;
  mtime: number;
}

/** Lifecycle discriminator (model.AstroStatus*). header/none are Tier 0; solving/solved/failed/unsolvable are Tier 1. */
export type WCSStatus =
  | "header"
  | "none"
  | "solving"
  | "solved"
  | "failed"
  | "unsolvable";

/** Displayable astrometric summary derived from a light frame's embedded WCS (model.WCSSolution). */
export interface WCSSolution {
  center_ra: number; // deg, [0,360)
  center_dec: number; // deg, [-90,90]
  pos_angle: number; // deg, [0,180) — sensor-axis orientation; 213° and 33° fold to the same framing
  pixel_scale: number; // arcsec/px
  fov_w: number; // deg (0 when unknown)
  fov_h: number; // deg (0 when unknown)
  flipped: boolean; // mirror parity (det(CD) > 0)
}

/** Raw solve seeds read from the header (model.AstroHints); each field absent when its card is missing. */
export interface AstroHints {
  ra?: number; // deg (object)
  dec?: number; // deg (object)
  focal_len_mm?: number;
  pixel_size_um?: number;
}

/**
 * Per-frame pointing/orientation (model.Astrometry). Undefined on a Frame means the
 * backfill has not processed it yet ("pending"); a present object with no `solution`
 * means it was processed but carries no embedded WCS.
 */
export interface Astrometry {
  status: WCSStatus;
  solution?: WCSSolution;
  hints?: AstroHints;
  /** Why a Tier 1 solve failed or was deemed unsolvable; absent otherwise. */
  reason?: string;
  /** Unix seconds the Tier 1 solve completed; absent when not solved. */
  solved_at?: number;
  /** Tier 1 solve attempts made so far; absent when never attempted. */
  attempts?: number;
  /** Field-of-view estimate the solver searched with, degrees; absent when unknown. */
  fov_deg?: number;
}

/**
 * One frame row, mirroring `frameDTO` in internal/web/frames_api.go — the single
 * shape reused by both the paginated REST lists and the live SSE feed.
 *
 * Every field the DTO tags `omitempty` is optional here, because Go drops it from
 * the JSON at its zero value: a calibration frame has no `target`, an unpulled one
 * no `sha256`/`pulled_at`. Reading such a field without a `?? ""` / `?? 0` default
 * is a runtime crash, so keep the optionality in step with the Go tags — the
 * typecheck is what catches the unguarded reads.
 */
export interface Frame {
  id: number;
  rig_id: string;
  source_share: string;
  source_path: string;
  sha256?: string;
  size: number;
  mtime: number;
  frame_type: string;
  target?: string;
  filter?: string;
  exptime: number;
  set_temp: number;
  gain: number;
  offset: number;
  binning?: string;
  sensor?: string;
  date_obs?: string;
  night: number;
  dest_path?: string;
  sidecars?: Sidecar[] | null;
  state: FrameState;
  attempts: number;
  next_retry_at?: number;
  pulled_at?: number;
  deleted_at?: number;
  /** Unix seconds the frame's filed copy was first seen missing from the NAS (out-of-band deletion). Absent while present. */
  missing_seen_at?: number;
  /** Unix seconds a re-pull was requested for a missing frame. Absent when no restore is pending. */
  restore_requested_at?: number;
  /** Unix seconds the operator confirmed this frame's NAS copy was culled on purpose. Meaningful only alongside a live missing_seen_at (design library-reconciliation §10.5). */
  missing_acked_at?: number;
  error?: string;
  /** Pointing/orientation from the embedded WCS; undefined until the backfill reaches a legacy frame. */
  astrometry?: Astrometry;
}

// ---- config.go -----------------------------------------------------------

export interface Roots {
  light: string;
  cal: string;
  staging: string;
  logs: string;
}

export interface FailedRetry {
  base: Duration;
  factor: number;
  max: Duration;
  max_attempts: number;
}

export interface RigDefaults {
  poll_interval: Duration;
  retry_interval: Duration;
  settle_cycles: number;
  min_file_age: Duration;
  grace: Duration;
  reconcile_settle: Duration;
  failed_retry: FailedRetry;
  unreachable_alert_after: Duration;
}

export interface WebSettings {
  bcrypt_cost: number;
  session_idle_timeout: Duration;
  session_max_age: Duration;
  login_backoff_base: Duration;
  login_backoff_cap: Duration;
  login_global_cap: number;
  allowed_hosts: string[];
  metrics_expose_lan: boolean;
}

export interface SMBSettings {
  remove_retry_budget: number;
}

export interface Templates {
  light: string;
  /** Keyed by canonical frame type, e.g. { DARK: "...", FLAT: "..." }. */
  cal: Record<string, string>;
  /**
   * Routes a light frame whose target parsed as a mosaic panel, and only while
   * `mosaic_detection` is on. It is the one template that may name <TARGET_BASE>,
   * <PANEL>, <PANEL_ROW> and <PANEL_COL>. An empty value is filled with the
   * shipped default server-side rather than rejected.
   */
  mosaic: string;
}

export interface Source {
  share: string;
  roots: string[];
  /** Operator opt-out: a disabled share is never watched, listed, pulled, counted
   * toward the verdict, cleaned, or restored. Absent/false means watched. */
  disabled?: boolean;
}

export interface RigProfile {
  id: string;
  name: string;
  /** Stable scan_air device identity a rig binds to (adoption sets it). */
  guid: string;
  /** Hardware SoC serial kept as a secondary binding key. */
  cpuId: string;
  /** Deprecated: retained only for a not-yet-migrated rig; adoption leaves it "". */
  host: string;
  user: string;
  /** Redacted to REDACTED_SENTINEL on GET; send the sentinel to leave unchanged. */
  password?: string;
  ota: string;
  session: string;
  sources: Source[];
  managed: boolean;
  capacity_constrained: boolean;
  pull_logs: boolean;
  /** When true the rig never deletes from the device; frames are kept after verify. */
  keep_originals: boolean;
  /** Opt in to correcting this device's clock when it drifts; needs a guid-bound rig. */
  sync_clock: boolean;
  // Per-rig overrides — omitted/absent means "inherit Defaults".
  grace?: Duration;
  reconcile_settle?: Duration;
  poll_interval?: Duration;
  retry_interval?: Duration;
  settle_cycles?: number;
  min_file_age?: Duration;
}

export interface Config {
  schema_version: number;
  web_port: number;
  /** Redacted on GET. */
  password_hash?: string;
  /** Redacted on GET. */
  session_secret?: string;
  strip_target_spaces: boolean;
  /**
   * Opt in to recognising the ASIAIR mosaic planner's "<base>_<row>-<col>" panel
   * names and routing such a frame through `templates.mosaic`. Off means routing
   * is exactly what it would be without the feature, and the mosaic template is
   * neither shown nor validated.
   */
  mosaic_detection: boolean;
  /**
   * Stops the hourly request to api.github.com that asks whether a newer release
   * exists. Off (the default) means checking is on. Applies live.
   */
  update_check_disabled: boolean;
  roots: Roots;
  exclude_extensions: string[];
  delete_sidecar_extensions: string[];
  disk_reserve_bytes: number;
  /** How long a frame found missing from the NAS is shown before it ages out of the missing view. */
  missing_display_period: Duration;
  max_concurrent_transfers: number;
  /** FITS preview generation (apply-on-restart). */
  previews_enabled: boolean;
  preview_concurrency: number;
  preview_cache_max_bytes: number;
  /** Tier 1 plate solving (apply-on-restart). */
  plate_solve_enabled: boolean;
  /** How far around the pointing hint the solver searches, degrees. */
  plate_solve_radius_deg: number;
  defaults: RigDefaults;
  web: WebSettings;
  smb: SMBSettings;
  date_format: string;
  alias_table: Record<string, string>;
  sensor_map: Record<string, string>;
  templates: Templates;
  rigs: RigProfile[];
  /**
   * Optimistic-concurrency token (unix nanos), sent as a string. Echo it back on
   * PUT; stale → 409. It is a string because as a number it exceeds JavaScript's
   * safe integer range and would be silently rounded, breaking the echo.
   */
  updated_at: string;
}

// ---- REST envelopes & SSE event payloads ---------------------------------

export interface FramesPage {
  frames: Frame[];
  page: number;
  page_size: number;
  total: number;
}

/**
 * GET /api/rigs/{id}/frames?missing=true — the same paged envelope plus per-directory
 * missing tallies. `missing=true` cannot be combined with a state or filter (400).
 */
export interface MissingFramesPage extends FramesPage {
  groups: MissingGroup[];
}

/** GET /api/rigs/{id}/summary — the header-derived morning summary (shape TBD server-side). */
export interface RigSummary {
  rig_id: string;
  night: number;
  lights: number;
  calibration: number;
  unclassified: number;
  failed: number;
  held: number;
  targets: string[];
  /** Free-form additional counts the backend may include. */
  [k: string]: unknown;
}

/** GET /api/app-logs — one line from the app's in-memory ring (web.LogEntry). */
export interface AppLogEntry {
  /** RFC3339 timestamp string. */
  time: string;
  /** slog level name: "DEBUG" | "INFO" | "WARN" | "ERROR". */
  level: string;
  message: string;
  /** Structured attributes the backend attached to the record. */
  attrs?: Record<string, unknown>;
}

/** GET /api/rigs/{id}/logs — one pulled device-log file (web.logPullDTO). */
export interface LogPull {
  rig_id: string;
  log_type: string;
  source_path: string;
  dest_path?: string;
  size: number;
  /** Unix seconds. */
  pulled_at: number;
}

/**
 * Machine-readable refusal codes on a 403 from POST /api/setup, so the wizard can
 * tell a wrong token from a window that has already closed without matching prose.
 * These are the wire values (internal/web/setup.go).
 */
export const SETUP_TOKEN_REQUIRED = "setup_token_required";
export const SETUP_COMPLETE = "setup_complete";

/**
 * Where the server is running, as reported by GET /api/setup. The wizard words its
 * "where do I find the token" help from this, because the channel that carries the
 * token differs per platform and a wrong answer sends the operator to a log that
 * does not exist for them.
 */
export type PlatformCategory = "container" | "kubernetes" | "other";

/** GET /api/setup — first-run status (unauthenticated, unguarded). */
export interface SetupStatus {
  /** True before any password is set: the SPA must show the Setup wizard. */
  needed: boolean;
  /** Sent only while `needed` is true. */
  platform?: PlatformCategory;
  /** The server's operating system (Go's GOOS), sent only while `needed` is true. */
  os?: string;
}

/** POST /api/setup response — the seeded, redacted config after first-run completes. */
export interface SetupResponse {
  status: string;
  config: Config;
  /**
   * The address the setup request arrived on, which the server added to the
   * allowlist so this browser keeps working. Empty when nothing usable was sent.
   */
  seeded_host: string;
  /** True when seeded_host is an IP literal, which the wizard flags as fragile. */
  seeded_host_is_ip: boolean;
}

/** One destination root's writability outcome (web.rootResult). */
export interface RootCheckResult {
  path: string;
  ok: boolean;
  /** Human message when ok is false; absent/empty when writable. */
  error?: string;
}

/**
 * POST /api/roots/check response — whether the SERVICE ACCOUNT can write each
 * candidate root. `account` is the account the service runs as (DOMAIN\Name on
 * Windows), so the UI can name it in the "grant this account access" guidance.
 */
export interface RootsCheckResponse {
  account: string;
  results: RootCheckResult[];
}

// ---- retarget (Tools page, internal/model) --------------------------------

/** Outcome of retargeting one frame (model.RetargetResult). */
export type RetargetStatus = "retargeted" | "skipped" | "failed";

/**
 * One line of the streamed NDJSON body from POST /api/rigs/{id}/retarget/apply
 * (model.RetargetResult) — flushed as each frame completes.
 */
export interface RetargetResult {
  frame_id: number;
  status: RetargetStatus;
  /** Why the frame was skipped/failed; absent on success. */
  reason?: string;
  old_path?: string;
  new_path?: string;
}

/**
 * POST /api/rigs/{id}/retarget/preview response row (model.RetargetPreview).
 * `new_path` is the index-0 canonical destination, night folder included; Apply
 * may still disambiguate the filename on a collision. `night_approx` flags an
 * ordinal that anything filed before Apply runs could still shift (the preview
 * resolves the night without ever mutating night_counters).
 */
export interface RetargetPreview {
  frame_id: number;
  current_target: string;
  old_path: string;
  new_path: string;
  night_approx: boolean;
  /**
   * Raw OBJECT spellings already filed in the destination night folder that
   * differ from the target as it will be written; absent when there is none.
   */
  existing_spellings?: string[];
  eligible: boolean;
  /**
   * Why the frame is not eligible — or, on an eligible row, an informational
   * note such as a ledger read the preview could not make.
   */
  reason?: string;
}

// ---- re-file (Tools page, internal/model/refile.go) -----------------------

/** How much of the library the diff covers (model.RefilePreview.Scope). */
export type RefileScope = "mosaic" | "all";

/**
 * A directory the prune expects to be unable to remove, and why — a .DS_Store the
 * Finder wrote, an operator's stacked masters, anything the tool doesn't manage
 * (model.StrandRisk). Carried by the preview so leftover folders are explained
 * before anything moves, and by the summary for the ones actually skipped.
 */
export interface StrandRisk {
  dir: string;
  reason: string;
}

/**
 * One `old dir → new dir` migration the operator selects as a whole
 * (model.RefileGroup). A group is identified by the PAIR of directories, not by
 * `to_dir` alone: two source directories can collapse onto one destination.
 *
 * `count` counts distinct FILES, not rows — a frame present on two device shares
 * is two rows sharing one file and one move — while `frame_ids` carries every row,
 * because every row's dest_path is rewritten. So `frame_ids.length` is never a
 * progress denominator; apply emits one line per file.
 */
export interface RefileGroup {
  from_dir: string;
  to_dir: string;
  count: number;
  frame_ids: number[];
  strand_risk: StrandRisk[] | null;
}

/**
 * A path excluded from every group and why (model.Refusal): a held or
 * missing-marked sharing row, a cross-rig sharer, an absent dest_path, or a frame
 * the current templates would quarantine. A refusal is never a group member, so it
 * can never surface as a RefileResult.
 */
export interface Refusal {
  dest_path: string;
  reason: string;
}

/** POST /api/rigs/{id}/refile/preview response (model.RefilePreview). */
export interface RefilePreview {
  groups: RefileGroup[];
  refusals: Refusal[];
  scope: RefileScope;
}

export type RefileStatus = "moved" | "skipped" | "failed";

/**
 * One line of the streamed NDJSON body from POST /api/rigs/{id}/refile/apply
 * (model.RefileResult) — one per FILE, so an H12 frame's two rows produce a single
 * line. The whole group runs to completion on the worker without yielding, so the
 * lines arrive as one burst at the end of the group.
 */
export interface RefileResult {
  frame_id: number;
  status: RefileStatus;
  from?: string;
  to?: string;
  /** Why the file was skipped or failed; absent on success. */
  reason?: string;
}

/**
 * The final NDJSON line of an apply stream (model.RefileSummary). Prune outcomes
 * are directories rather than frames, so they can't be results.
 */
export interface RefileSummary {
  pruned_dirs: string[] | null;
  skipped_dirs: StrandRisk[] | null;
}

// ---- device files (File browser page, design device-file-browser)
// Paths are share-relative, "/"-separated, with no leading slash; "" is the share
// root. Run items are relative to the selection's folder.

/** One row of a device listing. At the rig level the entries are the shares. */
export interface DeviceFileEntry {
  name: string;
  /** A folder (or, at the rig level, a share). */
  dir: boolean;
  size: number;
  /** Unix seconds in the device's clock, display only. */
  mtime: number;
  /** Commander's harvester watches this folder. */
  watched: boolean;
  /** Why this entry can't be renamed; absent when it can (and in a share list). */
  rename_refused?: FileRenameRefusal;
}

/**
 * A listed entry's rename refusal, the sentence rendered by the server. `watched`
 * is R1 (the entry lies in a watched root), `reserved` R2 (it is or holds one),
 * `unchecked` a store read that failed, so nothing could be checked.
 */
export interface FileRenameRefusal {
  rule: "watched" | "reserved" | "unchecked";
  message: string;
}

/** GET /api/rigs/{id}/files response. `share` is "" for the share list. */
export interface DeviceFileListing {
  share: string;
  dir: string;
  watched: boolean;
  /** False for a rig Commander has not bound to a GUID yet: writes are refused. */
  writable: boolean;
  entries: DeviceFileEntry[];
}

/** GET …/files/download?check=1 response. */
export interface DownloadCheck {
  filename: string;
  files: number;
  bytes: number;
  zipped: boolean;
}

export interface FilesPlace {
  rig: string;
  share: string;
  dir: string;
}

export interface CopyPlanRequest {
  from: FilesPlace & { names: string[] };
  to: FilesPlace;
  mirror: boolean;
}

export interface CopyPlanItem {
  rel: string;
  size: number;
  /** The item's destination folder is watched on the destination rig. */
  watched: boolean;
}

/** "" when the destination is not a frame the destination harvester tracks. */
export type DstLedger = "" | "unarchived" | "archived";

export interface CopyConflict {
  rel: string;
  /** `file` may be overwritten; `type` (a file against a folder) is skip-only. */
  kind: "file" | "type";
  src_size: number;
  src_mtime: number;
  dst_size: number;
  dst_mtime: number;
  dst_ledger: DstLedger;
}

/** POST /api/files/copy/plan response. */
export interface CopyPlan {
  files: number;
  bytes: number;
  /** Folders the run creates inside to.dir, parents first. */
  dirs: string[];
  /** Every planned file, type-conflicted subtrees included (the client drops them). */
  items: CopyPlanItem[];
  conflicts: CopyConflict[];
  /** Levels of to.dir itself that do not exist yet (a mirrored destination). */
  to_dir_missing: number;
  watched: { frames: number; previews: number; keeps_originals: boolean };
}

export interface CopyOverwrite {
  rel: string;
  dst_size: number;
  dst_mtime: number;
}

/** POST /api/rigs/{id}/files/rename body; answers 204. */
export interface RenameRequest {
  share: string;
  dir: string;
  /** The entry exactly as listed. */
  name: string;
  new_name: string;
}

/** POST /api/files/upload/stage response: the id the plan and run name. */
export interface StagedUpload {
  id: string;
  size: number;
}

/** POST /api/files/upload/plan body. */
export interface UploadPlanRequest {
  id: string;
  to: FilesPlace;
}

/** POST /api/files/upload/plan response: the copy plan for the one staged file,
 * plus the share's free space when the device reported it. */
export interface UploadPlan extends CopyPlan {
  free_bytes?: number;
}

/** POST /api/files/upload body. `dst_size`/`dst_mtime` are both present only
 * when the user chose Overwrite, bound to the conflict as they saw it. */
export interface UploadRunRequest {
  id: string;
  to: FilesPlace;
  dst_size?: number;
  dst_mtime?: number;
}

/** POST /api/files/copy body. */
export interface CopyRunRequest {
  from: FilesPlace;
  to: FilesPlace;
  dirs: string[];
  items: { rel: string; size: number }[];
  overwrite: CopyOverwrite[];
}

export interface ExtTotal {
  /** Lower-cased; "" is a file with no extension. */
  ext: string;
  files: number;
  bytes: number;
}

export interface DeleteItem {
  rel: string;
  size: number;
  mtime: number;
}

/** POST /api/rigs/{id}/files/delete/plan response. */
export interface DeletePlan {
  /** Folder count; the folders themselves are `dirs_deepest_first`. */
  dirs: number;
  files: number;
  bytes: number;
  by_ext: ExtTotal[];
  unarchived: number;
  unwatched_files: number;
  watched_roots: string[];
  items: DeleteItem[];
  dirs_deepest_first: string[];
}

/** POST /api/rigs/{id}/files/delete body. */
export interface DeleteRunRequest {
  share: string;
  dir: string;
  items: DeleteItem[];
  dirs: string[];
  acknowledged_unarchived: number;
}

/** Why an item was skipped, failed or noted; the report words each one. */
export type FilesReasonCode =
  | "chose_skip"
  | "appeared"
  | "type_conflict"
  | "source_gone"
  | "changed"
  | "already_gone"
  | "not_empty"
  | "kept_marker"
  | "denied"
  | "in_use"
  | "unreachable"
  | "identity"
  | "mtime_not_preserved"
  | "stopped"
  | "not_attempted"
  | "watched"
  | "error";

export interface FilesProgressLine {
  type: "progress";
  done: number;
  total: number;
  bytes_done: number;
  bytes_total: number;
  current: string;
}

export interface FilesItemLine {
  type: "item";
  rel: string;
  outcome: "copied" | "deleted" | "skipped" | "failed";
  code: FilesReasonCode;
  dst_size?: number;
  dst_mtime?: number;
  message?: string;
}

export interface FilesSummaryLine {
  type: "summary";
  done: number;
  skipped: number;
  failed: number;
  /** Why the server ended the run early: an unreachable rig, or a device that is
   * not the rig or a rig not yet identified (`identity`). */
  aborted: "" | "unreachable" | "identity";
}

/** One line of a copy, upload or delete run's NDJSON stream. */
export type FilesRunLine = FilesProgressLine | FilesItemLine | FilesSummaryLine;

/**
 * A validation finding returned in a 400 body's `problems` array (config.Problem).
 * The Go struct carries NO json tags, so its keys are the CAPITALIZED field names.
 */
export interface Problem {
  Severity: "error" | "warning";
  Field: string;
  Message: string;
}

/** 400 error body carrying the validation problems (config PUT / setup / rig CRUD). */
export interface ProblemsResponse {
  error: string;
  problems: Problem[];
}

/**
 * POST /api/config/preview request — dry-run one or more templates against a real
 * recent frame per canonical type, to show where each would land before saving.
 */
export interface PreviewRequest {
  templates: Templates;
  strip_target_spaces: boolean;
  /** The candidate value of the Settings toggle, not the stored one — without it
   * the mosaic sample row renders through the light template. */
  mosaic_detection: boolean;
  date_format: string;
  /** Scope the sample frames to one rig; omitted uses any recent frame. */
  rig_id?: string;
}

/** One preview row: where a frame of `type` (from `source`) would be filed. */
export interface PreviewSample {
  type: string;
  source: string;
  dest: string;
  /** True when no real frame existed and a synthetic stand-in was used. */
  synthetic: boolean;
  /** Non-empty when a token could not resolve — the whole point of the preview. */
  error: string;
}

export interface PreviewResponse {
  samples: PreviewSample[];
}

// ---- SSE discrete-event payloads ----------------------------------------
// The snapshot rides `event: snapshot`; these are the small deltas.

// frame.done / frame.failed / frame.deleted carry the full persisted Frame
// (enriched server-side to the same DTO shape as the REST endpoints), so the live
// feed renders/updates a row without a refetch. The payload keys on
// `source_share` + `source_path` like every other feed input. Older consumers that
// only read `rig_id` to decide whether to refetch stay compatible — Frame carries it.
export type FrameDoneEvent = Frame;
export type FrameFailedEvent = Frame;
export type FrameDeletedEvent = Frame;

export interface RigPresenceEvent {
  rig_id: string;
  name?: string;
}

/**
 * A reclassify pass finished on a rig. `examined` is how many frames were held
 * when it started, `moved` how many of them resolved and were re-filed — the pass
 * is asynchronous (it runs on the rig's worker, at a cycle boundary if one is in
 * flight), so this is what reports the result.
 */
export interface RigReclassifiedEvent {
  rig_id: string;
  examined: number;
  moved: number;
}

export interface AlertRaisedEvent {
  id: number;
  rig_id: string;
  severity: Severity;
  category: AlertCategory;
  message: string;
}
