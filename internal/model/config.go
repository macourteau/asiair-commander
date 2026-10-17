package model

import "time"

// CurrentSchemaVersion is the config schema version this binary writes. A newer
// binary meeting an older config forward-migrates; an older binary meeting a
// newer config refuses to start (design architecture §7). v2 added the preview_*
// fields; v3 added the plate-solve fields; v4 added reconcile_settle (backfilled
// to its default so an upgraded config passes the settle validator).
const CurrentSchemaVersion = 4

// RemotesSchemaVersion is the schema version of a config that holds at least
// one remote. Save writes it only then, so a config with no remote stays at
// CurrentSchemaVersion and every older binary keeps reading it, while one with
// a remote is refused by a binary that would silently drop its pairing keys.
// Load accepts both and migrates neither.
//
// The two tracks share one number line, so RemotesSchemaVersion must always sit
// above CurrentSchemaVersion (TestSchemaVersionTracksStayOrdered pins it). A
// bump of CurrentSchemaVersion must move RemotesSchemaVersion above it too, and
// its migration must handle a file stamped with the old RemotesSchemaVersion: such
// a file is "the old current plus remotes", not a step on the linear migration
// path, so migrate must map it back onto that path rather than read it as a
// version the registry upgrades from. See migrate in internal/config/migrate.go.
const RemotesSchemaVersion = 5

// MaxSchemaVersion is the newest schema version this binary reads.
const MaxSchemaVersion = RemotesSchemaVersion

// RedactedSentinel is the write-only placeholder GET /api/config returns in place
// of every secret. A PUT carrying it leaves the stored secret untouched (design
// architecture §8).
const RedactedSentinel = "••••••••"

// Config is the whole application configuration — the in-memory form of
// config.yaml (design architecture §7). It is held behind an atomic.Pointer and
// swapped whole on edit; workers read an immutable snapshot at each cycle
// boundary.
type Config struct {
	SchemaVersion int `yaml:"schema_version" json:"schema_version"`
	WebPort       int `yaml:"web_port" json:"web_port"`
	// PasswordHash is a bcrypt hash; SessionSecret is base64 random bytes
	// (DPAPI-wrapped on Windows). Both are redacted from GET /api/config.
	PasswordHash  string `yaml:"password_hash" json:"password_hash,omitempty"`
	SessionSecret string `yaml:"session_secret" json:"session_secret,omitempty"`

	StripTargetSpaces bool `yaml:"strip_target_spaces" json:"strip_target_spaces"`
	// MosaicDetection opts into recognising the ASIAIR mosaic planner's
	// "<base>_<row>-<col>" pane names and routing such a frame through
	// Templates.Mosaic. Off by default: recognition is a producer-format parse
	// with an accepted residual false-positive class (a catalogue designation
	// spelled with an underscore, "Abell_2-1"), so only an operator who shoots
	// mosaics carries its risk. While it is off, routing is identical to what it
	// would be without the feature and Templates.Mosaic is neither shown nor
	// validated.
	MosaicDetection bool `yaml:"mosaic_detection" json:"mosaic_detection"`

	Roots                   Roots    `yaml:"roots" json:"roots"`
	ExcludeExtensions       []string `yaml:"exclude_extensions" json:"exclude_extensions"`
	DeleteSidecarExtensions []string `yaml:"delete_sidecar_extensions" json:"delete_sidecar_extensions"`
	DiskReserveBytes        int64    `yaml:"disk_reserve_bytes" json:"disk_reserve_bytes"`
	// MaxConcurrentTransfers sizes the GLOBAL transfer semaphore (design
	// architecture §3). It is apply-on-restart — a change is persisted and
	// validated but only takes effect at the next service start.
	MaxConcurrentTransfers int `yaml:"max_concurrent_transfers" json:"max_concurrent_transfers"`

	// Preview generation. All three are apply-on-restart: the previewer is built
	// from a config snapshot at startup, like MaxConcurrentTransfers.
	// PreviewsEnabled false disables the feature (a nil previewer is injected).
	// PreviewConcurrency sizes the decode/encode semaphore. PreviewCacheMaxBytes
	// bounds the on-disk preview cache (LRU-evicted).
	PreviewsEnabled      bool  `yaml:"previews_enabled" json:"previews_enabled"`
	PreviewConcurrency   int   `yaml:"preview_concurrency" json:"preview_concurrency"`
	PreviewCacheMaxBytes int64 `yaml:"preview_cache_max_bytes" json:"preview_cache_max_bytes"`

	// Tier-1 plate solving. PlateSolveEnabled is apply-on-restart (the provisioner
	// and solve worker start once from a snapshot, like the preview_* fields);
	// PlateSolveRadiusDeg is live (read per solve from the config pointer). Which
	// star catalogs are installed is disk state, not config.
	PlateSolveEnabled   bool `yaml:"plate_solve_enabled" json:"plate_solve_enabled"`
	PlateSolveRadiusDeg int  `yaml:"plate_solve_radius_deg" json:"plate_solve_radius_deg"`

	// MissingDisplayPeriod is how long a frame marked missing by out-of-band
	// deletion reconciliation stays expanded in the missing panel before the UI
	// collapses its directory group by age. Zero or negative means never collapse.
	// It has no per-rig override.
	MissingDisplayPeriod Duration `yaml:"missing_display_period" json:"missing_display_period"`

	Defaults RigDefaults `yaml:"defaults" json:"defaults"`
	Web      WebSettings `yaml:"web" json:"web"`
	SMB      SMBSettings `yaml:"smb" json:"smb"`
	// DateFormat is the Go layout for the <DATE> token (default "2006-01-02").
	DateFormat string `yaml:"date_format" json:"date_format"`

	// AliasTable maps a case-folded IMAGETYP value to a canonical type string.
	AliasTable map[string]string `yaml:"alias_table" json:"alias_table"`
	// SensorMap maps an INSTRUME value to a short sensor name.
	SensorMap map[string]string `yaml:"sensor_map" json:"sensor_map"`
	Templates Templates         `yaml:"templates" json:"templates"`
	Rigs      []RigProfile      `yaml:"rigs" json:"rigs"`

	// Remotes are the paired remote sites. Only the remote supervisor writes
	// them (through /api/remotes): a PUT of the whole config keeps the stored
	// value whatever it carries. Absent when no remote was ever added, so such
	// a config.yaml is unchanged.
	Remotes []RemoteProfile `yaml:"remotes,omitempty" json:"remotes,omitempty"`
	// RemoteSeq is the last remote id number handed out. Ids are never reused,
	// so a late write naming a removed remote's site can never be taken for a
	// later remote's.
	RemoteSeq int `yaml:"remote_seq,omitempty" json:"remote_seq,omitempty"`

	// UpdatedAt is the optimistic-concurrency token (unix nanos) the GET returns
	// and a PUT must echo; a stale write is rejected 409 (design architecture §8).
	// Not persisted as a meaningful field beyond change detection.
	//
	// Serialized as a JSON string: as unix nanoseconds it exceeds JavaScript's
	// safe integer range (2^53), so a browser round-tripping it as a JSON number
	// would silently round it and echo back a value that no longer matches — every
	// browser config write would then 409. YAML keeps the numeric form.
	UpdatedAt int64 `yaml:"updated_at" json:"updated_at,string"`
}

// Roots are the managed destination roots. Staging, light, and cal MUST resolve
// to one volume (validated, design architecture §7); logs may live elsewhere.
type Roots struct {
	Light   string `yaml:"light" json:"light"`
	Cal     string `yaml:"cal" json:"cal"`
	Staging string `yaml:"staging" json:"staging"`
	Logs    string `yaml:"logs" json:"logs"`
}

// RigDefaults are the cadence/grace knobs each rig inherits unless it overrides
// them (design architecture §7).
type RigDefaults struct {
	PollInterval          Duration    `yaml:"poll_interval" json:"poll_interval"`
	RetryInterval         Duration    `yaml:"retry_interval" json:"retry_interval"`
	SettleCycles          int         `yaml:"settle_cycles" json:"settle_cycles"`
	MinFileAge            Duration    `yaml:"min_file_age" json:"min_file_age"`
	Grace                 Duration    `yaml:"grace" json:"grace"`
	FailedRetry           FailedRetry `yaml:"failed_retry" json:"failed_retry"`
	UnreachableAlertAfter Duration    `yaml:"unreachable_alert_after" json:"unreachable_alert_after"`
	// ReconcileSettle is the settle window before a device original may be deleted
	// for a frame the operator culled on the NAS: how long after the first
	// recorded, alerted observation the delete pass waits, giving time to reverse.
	// Per-device like Grace (a small ASIAIR card culled aggressively wants it
	// short; a cautious operator wants it long).
	ReconcileSettle Duration `yaml:"reconcile_settle" json:"reconcile_settle"`
}

// FailedRetry is the exponential backoff for a failed frame (design
// frame-lifecycle §4).
type FailedRetry struct {
	Base        Duration `yaml:"base" json:"base"`
	Factor      float64  `yaml:"factor" json:"factor"`
	Max         Duration `yaml:"max" json:"max"`
	MaxAttempts int      `yaml:"max_attempts" json:"max_attempts"`
}

// WebSettings are the auth/session knobs (design architecture §7/§8).
type WebSettings struct {
	BcryptCost         int      `yaml:"bcrypt_cost" json:"bcrypt_cost"`
	SessionIdleTimeout Duration `yaml:"session_idle_timeout" json:"session_idle_timeout"`
	SessionMaxAge      Duration `yaml:"session_max_age" json:"session_max_age"`
	LoginBackoffBase   Duration `yaml:"login_backoff_base" json:"login_backoff_base"`
	LoginBackoffCap    Duration `yaml:"login_backoff_cap" json:"login_backoff_cap"`
	LoginGlobalCap     int      `yaml:"login_global_cap" json:"login_global_cap"`
	// AllowedHosts is the Host-header allowlist (loopback + NAS hostname + operator
	// additions). Seeded with the hostname, not a raw IP, so DHCP churn is harmless.
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
	// MetricsExposeLAN opts /metrics out of its loopback-only default (design
	// architecture §8).
	MetricsExposeLAN bool `yaml:"metrics_expose_lan" json:"metrics_expose_lan"`
}

// SMBSettings holds SMB transport knobs (design architecture §7).
type SMBSettings struct {
	RemoveRetryBudget int `yaml:"remove_retry_budget" json:"remove_retry_budget"`
}

// Templates are the path templates. Light has one; calibration has one per
// canonical type (design routing-and-filing §3). Values are relative paths
// under the light/cal roots.
type Templates struct {
	Light string               `yaml:"light" json:"light"`
	Cal   map[FrameType]string `yaml:"cal" json:"cal"`
	// Mosaic routes a LIGHT whose target parsed as a mosaic pane, and only
	// while Config.MosaicDetection is on. It is the one template that may name
	// <TARGET_BASE>, <PANEL>, <PANEL_ROW> and <PANEL_COL>, and — because a
	// template rendering one directory for every pane would put ten distinct
	// OBJECT values in a single NIGHT_n folder and destroy night-ordinal
	// recovery — it must distinguish panes before its <n> segment. An empty
	// value normalises to the default rather than failing validation.
	Mosaic string `yaml:"mosaic" json:"mosaic"`
}

// Source is one SMB share plus the roots within it that a rig watches (design
// architecture §7). A USB-primary rig watches both "Udisk Images" and "EMMC
// Images" (H12 failover).
type Source struct {
	Share string   `yaml:"share" json:"share"`
	Roots []string `yaml:"roots" json:"roots"`
	// Disabled opts a share out of every watch/list/pull/verdict/restore/delete
	// path — the operator's lever to stop watching a slot they don't use so a
	// perpetually-empty removable slot does not suppress "safe to power off".
	// Disabled (not Enabled) so the zero value is enabled: an existing config with
	// no field loads with every share watched, needing no migration.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// RigProfile is one managed device (design architecture §7). ID is the stable
// internal key, never the host. Overridable cadence knobs are pointers: nil means
// "inherit Defaults".
type RigProfile struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// GUID is the stable scan_air device identity a rig binds to; CPUID is the
	// hardware SoC serial kept as a secondary key so a device whose guid changed
	// (factory reset) still resolves. Host is retained only for a not-yet-migrated
	// rig (deprecated) — resolution prefers GUID/CPUID, then a cached last-known
	// IP, then Host. The last-known IP is store state, not config.
	GUID     string   `yaml:"guid" json:"guid"`
	CPUID    string   `yaml:"cpuId" json:"cpuId"`
	Host     string   `yaml:"host" json:"host"`
	User     string   `yaml:"user" json:"user"`
	Password string   `yaml:"password" json:"password,omitempty"` // redacted on GET
	OTA      string   `yaml:"ota" json:"ota"`
	Session  string   `yaml:"session" json:"session"`
	Sources  []Source `yaml:"sources" json:"sources"`

	Managed             bool `yaml:"managed" json:"managed"`
	CapacityConstrained bool `yaml:"capacity_constrained" json:"capacity_constrained"`
	PullLogs            bool `yaml:"pull_logs" json:"pull_logs"`
	// KeepOriginals, when true, skips this rig's delete pass entirely: frames are
	// still pulled, byte-verified, and filed, but the proven original is never
	// removed from the device. Deletion eligibility is derived from verified state
	// plus grace, so flipping this back to false lets the very next delete pass
	// drain the whole held backlog with no migration. Default false preserves the
	// delete-after-grace behavior.
	KeepOriginals bool `yaml:"keep_originals" json:"keep_originals"`
	// SyncClock lets Commander correct this device's clock when it is more than
	// five minutes away from host time. Opt-in (the zero value is off) and only
	// ever applied to a configured, guid-bound rig — never to a merely discovered
	// device. An absent field loads as false, which is the intended default, so
	// there is no schema bump and no migration (as with Source.Disabled).
	SyncClock bool `yaml:"sync_clock" json:"sync_clock"`

	// Per-rig overrides (nil → inherit Defaults). MaxConcurrentTransfers is NOT
	// here — it is global.
	Grace           *Duration `yaml:"grace,omitempty" json:"grace,omitempty"`
	PollInterval    *Duration `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`
	RetryInterval   *Duration `yaml:"retry_interval,omitempty" json:"retry_interval,omitempty"`
	SettleCycles    *int      `yaml:"settle_cycles,omitempty" json:"settle_cycles,omitempty"`
	MinFileAge      *Duration `yaml:"min_file_age,omitempty" json:"min_file_age,omitempty"`
	ReconcileSettle *Duration `yaml:"reconcile_settle,omitempty" json:"reconcile_settle,omitempty"`
}

// EnabledSources returns the rig's watched sources with the operator-disabled
// ones filtered out. Every watch/list/pull/verdict/restore/delete site builds
// what it acts on from this, so a disabled share is never mounted, listed, pulled,
// counted toward capacity/verdict, cleaned, or restored.
func (p *RigProfile) EnabledSources() []Source {
	out := make([]Source, 0, len(p.Sources))
	for _, s := range p.Sources {
		if s.Disabled {
			continue
		}
		out = append(out, s)
	}
	return out
}

// DisabledShares returns the set of share names the operator has disabled — a
// share carrying a disabled source and no enabled source of the same name. The
// delete and restore passes iterate frames from the store (not the watch set), so
// they consult this to skip a disabled share's frames: its device originals are
// never cleaned and its frames never restored. A share removed from config
// entirely (not merely disabled) is absent here, so its already-recorded frames
// still drain — matching the pre-Fix-D behaviour for a removed share.
func (p *RigProfile) DisabledShares() map[string]bool {
	enabled := map[string]bool{}
	for _, s := range p.Sources {
		if !s.Disabled {
			enabled[s.Share] = true
		}
	}
	disabled := map[string]bool{}
	for _, s := range p.Sources {
		if s.Disabled && !enabled[s.Share] {
			disabled[s.Share] = true
		}
	}
	return disabled
}

// RemoteProfile is one paired remote site. Address and ClientKey are pairing
// secrets: plaintext in memory, wrapped like SessionSecret on disk, and redacted
// from every GET.
type RemoteProfile struct {
	// ID is the remote's site id (a netsite.ID): never reused.
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// RemotePairingConfig is what the remote's tunnel client is built from;
	// its keys sit inline among the profile's own.
	RemotePairingConfig `yaml:",inline"`
	// Region is the display code of the relay region fixed at pairing, and
	// RegionName its display name; the address embeds the region itself.
	Region     string `yaml:"region" json:"region"`
	RegionName string `yaml:"region_name,omitempty" json:"region_name,omitempty"`
	// RateLimitMBps caps reads on the remote's connections in megabytes per
	// second; 0 is no cap.
	RateLimitMBps float64 `yaml:"rate_limit_mbps,omitempty" json:"rate_limit_mbps,omitempty"`
	// CreatedAt is when the remote was added (unix seconds).
	CreatedAt int64 `yaml:"created_at" json:"created_at"`
	// PairedAt is when the current pairing was minted, by the add or the last
	// re-pair (unix seconds); 0 in a config written before it was recorded,
	// which reads as CreatedAt.
	PairedAt int64 `yaml:"paired_at,omitempty" json:"paired_at,omitempty"`
}

// RemotePairingConfig is every field a remote's tunnel client is built from.
// The supervisor compares it whole: any difference rebuilds the client as a
// re-pair, so a field added here (a self-hosted relay override, say) takes
// effect on save rather than at the next restart. A field the client does not
// read belongs on RemoteProfile instead.
type RemotePairingConfig struct {
	// Address is the remote's tailcat address, which carries the pre-shared
	// key. ClientKey is Commander's private key for this remote.
	Address   string `yaml:"address" json:"address,omitempty"`
	ClientKey string `yaml:"client_key" json:"client_key,omitempty"`
}

// Remote returns the remote profile with the given id, or nil.
func (c *Config) Remote(id string) *RemoteProfile {
	for i := range c.Remotes {
		if c.Remotes[i].ID == id {
			return &c.Remotes[i]
		}
	}
	return nil
}

// Rig returns the profile with the given id, or nil.
func (c *Config) Rig(id string) *RigProfile {
	for i := range c.Rigs {
		if c.Rigs[i].ID == id {
			return &c.Rigs[i]
		}
	}
	return nil
}

// EffectiveGrace resolves the grace for a rig (override or default).
func (c *Config) EffectiveGrace(r *RigProfile) time.Duration {
	if r.Grace != nil {
		return r.Grace.D()
	}
	return c.Defaults.Grace.D()
}

// EffectiveReconcileSettle resolves the settle window for a rig (override or
// default), mirroring EffectiveGrace. A non-positive resolved value is left as-is
// here; the delete pass applies defaultReconcileSettle as the floor so a
// hand-edited zero cannot collapse the safety window.
func (c *Config) EffectiveReconcileSettle(r *RigProfile) time.Duration {
	if r.ReconcileSettle != nil {
		return r.ReconcileSettle.D()
	}
	return c.Defaults.ReconcileSettle.D()
}

// EffectivePollInterval resolves the poll interval for a rig.
func (c *Config) EffectivePollInterval(r *RigProfile) time.Duration {
	if r.PollInterval != nil {
		return r.PollInterval.D()
	}
	return c.Defaults.PollInterval.D()
}

// EffectiveRetryInterval resolves the retry interval for a rig.
func (c *Config) EffectiveRetryInterval(r *RigProfile) time.Duration {
	if r.RetryInterval != nil {
		return r.RetryInterval.D()
	}
	return c.Defaults.RetryInterval.D()
}

// EffectiveSettleCycles resolves the settle-cycle count for a rig.
func (c *Config) EffectiveSettleCycles(r *RigProfile) int {
	if r.SettleCycles != nil {
		return *r.SettleCycles
	}
	return c.Defaults.SettleCycles
}

// EffectiveMinFileAge resolves the min-file-age for a rig.
func (c *Config) EffectiveMinFileAge(r *RigProfile) time.Duration {
	if r.MinFileAge != nil {
		return r.MinFileAge.D()
	}
	return c.Defaults.MinFileAge.D()
}
