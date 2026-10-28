// Package web is the REST + SSE + auth layer of ASIAIR Commander (design
// architecture §8). It serves the operator's only window into the running
// service: the paginated history and config REST API, the live dashboard SSE
// stream, the login/session machinery, and the embedded SPA with a history
// fallback. It does NOT build the React SPA (that ships separately into dist/);
// it embeds whatever dist/ holds.
//
// Security posture (design architecture §8), all enforced here: a bcrypt shared
// password; a session cookie signed with the config session secret (HMAC-SHA256),
// HttpOnly + SameSite=Strict, with an absolute expiry and an idle timeout; a
// double-submit CSRF token (readable XSRF-TOKEN cookie echoed in X-CSRF-Token)
// backstopped by a Host allowlist and Origin/Referer validation on every mutation
// and on the SSE handshake; a per-source login backoff plus a global cap; secret
// redaction and optimistic-concurrency 409 on the config; a token-gated
// first-run setup window. HTTPS is served alongside plain HTTP on the next port
// up, but Secure is left unset so one session works on both ports — documented,
// not hidden — so a login POST and cookie sent to the plain port traverse the LAN
// in cleartext.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/imgpreview"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// distFS holds the built SPA that `make spa` stages into dist/, which is
// untracked apart from dist/.gitkeep. The all: prefix keeps the dotfiles and
// underscore assets Vite emits, and lets the .gitkeep alone satisfy the embed,
// so the package builds without a prior SPA build.
//
//go:embed all:dist
var distFS embed.FS

// placeholderHTML is served in place of index.html when the static tree has
// none: a binary built without the SPA. It lives outside dist/ so staging a
// build never overwrites a tracked file.
//
//go:embed placeholder.html
var placeholderHTML []byte

// ErrRigNotFound is the sentinel a Supervisor returns for a per-rig command
// against an unknown id; the command handler maps it to HTTP 404. Deps.RigNotFound
// defaults to this; the composition root sets it to manager.ErrRigNotFound so the
// real supervisor's error matches (this package does not import manager).
var ErrRigNotFound = errors.New("web: rig not found")

// ErrFrameNotMissing is the sentinel a Supervisor returns when a restore targets a
// frame that is not marked missing; the command handler maps it to HTTP 409.
// Deps.FrameNotMissing defaults to this; the composition root sets it to
// rig.ErrFrameNotMissing so the real supervisor's error matches.
var ErrFrameNotMissing = errors.New("web: frame not missing")

// Supervisor is the slice of the RigManager the web layer drives — small on
// purpose so tests fake it. manager.Manager satisfies it (its per-rig commands
// return error; ApplyConfig returns nothing).
type Supervisor interface {
	Pause(id string) error
	Resume(id string) error
	RunNow(id string) error
	StopRun(id string) error
	Reclassify(id string) error
	ResetIdentity(id string) error
	DiscardFrame(id string, frameID int64) error
	PauseAll()
	ResumeAll()
	ApplyConfig(*model.Config)
	// Out-of-band deletion reconciliation.
	LinkLibrary(id, root string) error
	RescanLibrary(id string) error
	RestoreFrame(id string, frameID int64) error
	CancelRestore(id string, frameID int64) error
	AcknowledgeMissing(id string, frameID int64) error
	AcknowledgeMissingIDs(id string, dir string, ids []int64) (int, error)
	// Forgetting leftovers (design library-reconciliation §13.4): retire marked
	// `deleted` rows. dir and olderThanDays ride along only for the audit line.
	ForgetFrame(id string, frameID int64) error
	ForgetLeftovers(id, dir string, olderThanDays int, ids []int64) (int, error)
	// Frame retargeting (design routing-and-filing §9.6): PreviewRetarget is a
	// read-only projection; RetargetFrame moves one frame's archive to the new target
	// (a ~116 MB read+write per frame, so the Manager bounds it with a generous
	// timeout).
	RetargetFrame(id string, frameID int64, newTarget string) (model.RetargetResult, error)
	PreviewRetarget(id string, ids []int64, newTarget string) ([]model.RetargetPreview, error)
	// Re-filing archived frames to the current folder templates. These two are the
	// only methods here that take a context, deliberately: the operator's cancel has
	// to reach the worker, so both derive theirs from the request rather than the
	// detached one the Manager mints for every other command.
	PreviewRefile(ctx context.Context, rigID, scope string) (model.RefilePreview, error)
	RefileGroup(ctx context.Context, rigID, toDir string, frameIDs []int64) ([]model.RefileResult, model.RefileSummary, error)
}

// Scanner is the slice of discovery the web layer needs. *discovery.Scanner
// satisfies it.
type Scanner interface {
	Scan(ctx context.Context) []model.DiscoveredRig
	// ScanOnDemand is the round an operator asked for, which logs what answered.
	// Only the /api/scan handler calls it — the background timer must stay on
	// Scan, or the log fills with a round every 45 seconds.
	ScanOnDemand(ctx context.Context) []model.DiscoveredRig
	Discovered() []model.DiscoveredRig
	// DeviceShares opens a guest SMB session to the device a guid resolves to and
	// lists its shares, to prefill the adopt dialog's watched-share list (adopt-time
	// only, design architecture §9). Whether a device is already managed comes from
	// Discovered, not here.
	DeviceShares(ctx context.Context, guid string) (shares []string, err error)
}

var _ Scanner = (*discovery.Scanner)(nil)

// CatalogProvisioner is the slice of the Tier-1 provisioner the catalog-download
// endpoint drives. *platesolve.Provisioner satisfies it. The download runs under
// the app context the provisioner captured at startup, not the request context, so
// the endpoint returns immediately while the download continues in the background.
type CatalogProvisioner interface {
	CatalogInstalled(name string) bool
	Downloading() (name string, busy bool)
	DownloadCatalog(ctx context.Context, name string) error
	// ResolveFailed re-queues failed frames for another solve attempt and returns the
	// count, for the manual "re-solve" action (e.g. after installing a deeper catalog).
	ResolveFailed(ctx context.Context) (int, error)
}

// Remotes manages the paired remote sites, for /api/remotes. The remote
// supervisor (*supervisor.Supervisor) satisfies it and is the only writer of
// the config's remotes: each call below saves the config itself, under the
// config guard, so a handler never edits remotes through saveConfig, which
// keeps the stored remotes whatever a PUT carries. An id no remote has yields
// an error wrapping Deps.RemoteNotFound.
type Remotes interface {
	// List is every remote's state, in id order; the same value the status
	// snapshot carries.
	List() []model.RemoteStatus
	// Regions lists the relay regions a pairing can use, the nearest first.
	Regions(ctx context.Context) ([]model.RemoteRegion, error)
	// Add pairs a new remote on a relay region (0 for the nearest). The env
	// value in its result is shown once and never returned again.
	Add(ctx context.Context, name string, regionID int) (model.RemotePairing, error)
	// Repair mints a fresh pairing for a remote, on a relay region (0 keeps
	// its current one); the old env stops working at once.
	Repair(ctx context.Context, id string, regionID int) (model.RemotePairing, error)
	// Rename changes a remote's display name only.
	Rename(id, name string) error
	// SetRateLimit sets a remote's bandwidth cap in MB/s; 0 removes it.
	SetRateLimit(id string, mbps float64) error
	// Remove unpairs a remote: it is never dialled again, and its sightings and
	// its rigs' last-known endpoints through it are cleared.
	Remove(id string) error
}

// Deps are the collaborators the Server needs, injected by the composition root.
type Deps struct {
	// ConfigPath is the YAML config path GET reads through the live pointer and
	// PUT saves to.
	ConfigPath string
	// Config is the live config pointer: GET redacts what it Loads; PUT validates,
	// saves, applies, and Stores the new value.
	Config *atomic.Pointer[model.Config]
	// Guard serializes every config write (PUT, rig CRUD, logout rotation, setup,
	// and the manager's migration applier). Optional: a nil value is replaced with
	// a fresh Guard, so tests that omit it still serialize their own writes.
	Guard *config.Guard

	// Supervisor drives per-rig commands and receives applied configs.
	Supervisor Supervisor
	// RigNotFound is the sentinel Supervisor returns for an unknown rig id; the
	// command handler maps it to 404. Defaults to ErrRigNotFound.
	RigNotFound error
	// FrameNotMissing is the sentinel Supervisor.RestoreFrame returns when the frame
	// is not marked missing; the handler maps it to 409. Defaults to ErrFrameNotMissing.
	FrameNotMissing error

	// Remotes manages the paired remote sites. Optional: nil until a build
	// wires the remote supervisor. RemoteNotFound is the sentinel its calls
	// wrap for an unknown remote id.
	//
	// RemoteRegionUnknown is the sentinel for a relay region the relay map does
	// not list (400); RelayUnavailable for a relay service that cannot be
	// reached (502).
	Remotes             Remotes
	RemoteNotFound      error
	RemoteRegionUnknown error
	RelayUnavailable    error

	Store   store.Store
	Hub     hub.Hub
	Scanner Scanner
	// LocalFS backs the config VolumeResolver (single-volume root check) and NAS
	// free-space reads. Optional: a nil value skips the volume check in validation.
	LocalFS localfs.LocalFS
	// Header reads FITS headers off the NAS for the config template-editor live
	// preview (design architecture §9). The composition root wires header.New().
	// Optional: a nil value makes POST /api/config/preview return 503 rather than
	// panicking.
	Header header.Reader

	// Previewer renders/serves FITS image previews. Optional: a nil value (previews
	// disabled) makes GET /api/frames/{frameId}/image return 503.
	Previewer imgpreview.Previewer

	// Prov drives the Tier-1 star-catalog download action. Optional: a nil value
	// (plate solving disabled) makes POST /api/platesolve/catalogs/{name} return 503.
	// The read path — provisioning progress — rides the status snapshot, not here.
	Prov CatalogProvisioner

	// DeviceFiles is the device file browser's service. Optional: a nil value
	// makes every /files route answer 503 files_unavailable.
	DeviceFiles DeviceFiles

	// AppLog is the in-memory ring feeding GET /api/app-logs (design architecture
	// §18).
	AppLog *LogBuffer
	// AppLogPath is the current (unrotated) app-log file on disk, served verbatim
	// by GET /api/app-logs/download. Empty disables the download endpoint (a 404),
	// which is what tests constructing a zero-value Deps get.
	AppLogPath string
	// Metrics is the Prometheus surface the subsystems write to. Optional: a nil
	// value is a complete no-op — /metrics then serves an empty exposition.
	Metrics *metrics.Metrics
	Logger  *slog.Logger

	// DataDir is the service data directory. The one-time setup token is written
	// there as <data-dir>/setup-token so a local process with rights on that
	// directory can read it without parsing logs. Optional: an empty value
	// disables the file channel entirely (the log channel still carries the
	// token), which is what tests constructing a zero-value Deps get.
	DataDir string

	// Loopback reports whether a request's RemoteAddr is loopback. /metrics is its
	// only consumer — first-run setup is gated on the one-time token alone, from
	// every peer. Optional: nil uses a 127.0.0.1/::1 check.
	Loopback func(remoteAddr string) bool
	// DevDir, when non-empty, serves the SPA live from this directory instead of
	// the embedded dist/ (dev convenience).
	DevDir string
	// Now is the clock, injectable for tests. nil uses time.Now.
	Now func() time.Time

	// Version is the build-time app version, announced once per SSE connection in
	// the server_info event so the SPA can show it. Empty renders nothing.
	Version string
	// StartedAt is the application's process-start time; the SPA ticks the uptime
	// off the unix seconds announced in server_info. Zero omits the uptime.
	StartedAt time.Time
	// LatestVersion reports a published release newer than Version, or "" for
	// none (design features §7). It rides server_info; nil reports none.
	LatestVersion func() string
}

// Server is the configured web layer. Construct with New; serve via Handler
// (tests) or ListenAndServe.
type Server struct {
	deps     Deps
	log      *slog.Logger
	now      func() time.Time
	loopback func(remoteAddr string) bool
	staticFS fs.FS
	mux      *http.ServeMux
	limiter  *loginLimiter
	// promHandler is the promhttp handler built once over the metrics registry (nil
	// when Metrics is nil). Built once rather than per-scrape, so a scrape
	// does not reconstruct it.
	promHandler http.Handler

	// setupToken is the one-time token minted at construction when no password is
	// set: the sole authority for the first-run window, delivered by the service
	// log and by <data-dir>/setup-token. It is written once at mint and cleared
	// from the config-guard goroutine while arbitrary request goroutines read it,
	// so it is atomic rather than a plain string. A nil pointer means "no live
	// token" — the state both before mint and after the window closes.
	setupToken atomic.Pointer[string]

	// setupRejects rate-limits the WARN for a rejected setup attempt.
	setupRejects setupRejectionLog

	// ackTokens holds the short-lived, per-target acknowledge-confirmation tokens
	// (design library-reconciliation §10.7), keyed by target so re-challenging one
	// overwrites rather than accumulates. In-memory and mutex-guarded: a pending
	// confirmation must not survive a restart.
	ackMu     sync.Mutex
	ackTokens map[string]ackToken
}

// SetupToken returns the live one-time setup token, or "" once the first-run
// window has closed. The composition root logs it after the bind and polls it to
// stop re-emitting that line.
func (s *Server) SetupToken() string {
	if tok := s.setupToken.Load(); tok != nil {
		return *tok
	}
	return ""
}

// New builds the Server: it resolves defaults, mints the setup token if this is a
// first run, prepares the static file system, and wires the router.
func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.RigNotFound == nil {
		deps.RigNotFound = ErrRigNotFound
	}
	if deps.FrameNotMissing == nil {
		deps.FrameNotMissing = ErrFrameNotMissing
	}
	if deps.Guard == nil {
		deps.Guard = &config.Guard{}
	}
	loopback := deps.Loopback
	if loopback == nil {
		loopback = defaultLoopback
	}
	m := deps.Metrics
	var promHandler http.Handler
	if reg := m.Registry(); reg != nil {
		// ProcessStartTime lets a scraper detect a restart between scrapes. Built
		// once here, not per-request.
		promHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{ProcessStartTime: time.Now()})
	}

	var staticFS fs.FS
	if deps.DevDir != "" {
		staticFS = os.DirFS(deps.DevDir)
	} else {
		sub, err := fs.Sub(distFS, "dist")
		if err != nil {
			// The embed guarantees dist/ exists, so this is a programming error.
			panic("web: embedded dist/ missing: " + err.Error())
		}
		staticFS = sub
	}

	s := &Server{
		deps:        deps,
		log:         deps.Logger,
		now:         deps.Now,
		loopback:    loopback,
		staticFS:    staticFS,
		limiter:     newLoginLimiter(),
		promHandler: promHandler,
		ackTokens:   map[string]ackToken{},
	}

	// Remove any token file unconditionally, before minting — not only when a
	// password exists. That makes "the file exists" proof that *this* process
	// wrote it, so the installer's hand-off needs no mtime arithmetic to tell a
	// fresh token from a previous process's leftover on an upgrade.
	s.removeSetupTokenFile()
	if cfg := deps.Config.Load(); cfg == nil || cfg.PasswordHash == "" {
		tok := genToken()
		s.setupToken.Store(&tok)
		// The actionable first-run line is logged by the composition root after the
		// listener binds: only there is the bound port, the scheme, and the token
		// file's path known, and a hint naming a port the service did not get is
		// what sent operators looking for a link that was never there.
	}

	s.routes()
	return s
}

// Handler returns the root http.Handler (tests mount it on an httptest server).
func (s *Server) Handler() http.Handler { return s.mux }

// routes registers every pattern using the Go 1.22 method+pattern mux. Read
// endpoints are wrapped with guardRead (Host + session); mutations with guardWrite
// (Host + Origin + session + CSRF). The `:action` command URLs (design
// architecture §8) arrive as a single path segment captured by {idAction} and
// split on ':'.
func (s *Server) routes() {
	mux := http.NewServeMux()

	// Session.
	mux.HandleFunc("POST /api/session", s.guardLogin(s.handleLogin))
	mux.HandleFunc("DELETE /api/session", s.guardWrite(s.handleLogout))

	// Setup (first-run wizard).
	mux.HandleFunc("GET /api/setup", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.guardSetup(s.handleSetup))

	// Config.
	mux.HandleFunc("GET /api/config", s.guardRead(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.guardWrite(s.handlePutConfig))
	mux.HandleFunc("POST /api/config/preview", s.guardWrite(s.handleConfigPreview))

	// Destination-folder writability probe (service-account permission check).
	mux.HandleFunc("POST /api/roots/check", s.guardWrite(s.handleRootsCheck))

	// Rigs.
	mux.HandleFunc("GET /api/rigs", s.guardRead(s.handleListRigs))
	mux.HandleFunc("POST /api/rigs", s.guardWrite(s.handleCreateRig))
	mux.HandleFunc("GET /api/rigs/{id}", s.guardRead(s.handleGetRig))
	mux.HandleFunc("PUT /api/rigs/{id}", s.guardWrite(s.handleUpdateRig))
	mux.HandleFunc("DELETE /api/rigs/{id}", s.guardWrite(s.handleDeleteRig))
	// Commands arrive as "{id}:{action}" in one segment (e.g. rig-01:pause).
	mux.HandleFunc("POST /api/rigs/{idAction}", s.guardWrite(s.handleRigCommand))
	// Global (fleet-wide) pause/resume.
	mux.HandleFunc("POST /api/pause", s.guardWrite(s.handlePauseAll))
	mux.HandleFunc("POST /api/resume", s.guardWrite(s.handleResumeAll))

	// Frames + summary.
	mux.HandleFunc("GET /api/frames", s.guardRead(s.handleTransferHistory))
	mux.HandleFunc("GET /api/rigs/{id}/frames", s.guardRead(s.handleFrames))
	mux.HandleFunc("DELETE /api/rigs/{id}/frames/{frameId}", s.guardWrite(s.handleDiscardFrame))

	// Out-of-band deletion reconciliation.
	mux.HandleFunc("POST /api/rigs/{id}/library/link", s.guardWrite(s.handleLinkLibrary))
	mux.HandleFunc("POST /api/rigs/{id}/library/scan", s.guardWrite(s.handleRescanLibrary))
	mux.HandleFunc("POST /api/rigs/{id}/frames/{frameId}/restore", s.guardWrite(s.handleRestoreFrame))
	mux.HandleFunc("DELETE /api/rigs/{id}/frames/{frameId}/restore", s.guardWrite(s.handleCancelRestore))
	// Acknowledge a culled frame / directory: a two-step, server-issued confirmation
	// (design library-reconciliation §10.7). An empty/tokenless body is the
	// challenge; a body carrying the minted token is the confirm. The single-segment
	// {idAction} command route cannot shadow these deeper paths.
	mux.HandleFunc("POST /api/rigs/{id}/frames/{frameId}/acknowledge", s.guardWrite(s.handleAcknowledgeFrame))
	mux.HandleFunc("POST /api/rigs/{id}/missing-groups/acknowledge", s.guardWrite(s.handleAcknowledgeGroup))
	mux.HandleFunc("POST /api/rigs/{id}/frames/{frameId}/forget", s.guardWrite(s.handleForgetFrame))
	mux.HandleFunc("POST /api/rigs/{id}/missing-leftovers/forget", s.guardWrite(s.handleForgetLeftovers))
	mux.HandleFunc("GET /api/library/status", s.guardRead(s.handleLibraryStatus))
	// Frame retargeting Tools page (design routing-and-filing §9.6): a read-only
	// preview projection and an apply that streams one NDJSON RetargetResult per
	// frame as it completes.
	mux.HandleFunc("POST /api/rigs/{id}/retarget/preview", s.guardWrite(s.handleRetargetPreview))
	mux.HandleFunc("POST /api/rigs/{id}/retarget/apply", s.guardWrite(s.handleRetargetApply))
	// Re-file: a read-only grouped diff and an apply that streams one NDJSON line
	// per file plus a final summary. The preview is a POST (it carries a scope), so
	// it takes guardWrite like every other mutation-shaped request.
	mux.HandleFunc("POST /api/rigs/{id}/refile/preview", s.guardWrite(s.handleRefilePreview))
	mux.HandleFunc("POST /api/rigs/{id}/refile/apply", s.guardWrite(s.handleRefileApply))
	// FITS image preview: named size (cached) or arbitrary ?w=&h= (uncached).
	mux.HandleFunc("GET /api/frames/{frameId}/image", s.guardRead(s.handleFrameImage))
	// Native dimensions, for the lightbox zoom srcSet.
	mux.HandleFunc("GET /api/frames/{frameId}/image/meta", s.guardRead(s.handleFrameImageMeta))
	mux.HandleFunc("GET /api/rigs/{id}/summary", s.guardRead(s.handleSummary))
	mux.HandleFunc("GET /api/rigs/{id}/logs", s.guardRead(s.handleRigLogs))

	// Discovery.
	mux.HandleFunc("POST /api/scan", s.guardWrite(s.handleScan))
	mux.HandleFunc("GET /api/discovered", s.guardRead(s.handleDiscovered))
	// Adopt-time share enumeration for one device (bounded guest SMB).
	mux.HandleFunc("POST /api/discovered/{guid}/shares", s.guardWrite(s.handleDeviceShares))

	// Tier-1 plate solving: the star-catalog download action (progress is push-only
	// via the status snapshot) and the manual re-solve action.
	mux.HandleFunc("POST /api/platesolve/catalogs/{name}", s.guardWrite(s.handleDownloadCatalog))
	mux.HandleFunc("POST /api/platesolve/resolve", s.guardWrite(s.handleResolveFailed))

	// Device file browser (the File browser at /files). The plan routes write
	// nothing but take guardWrite like refile/preview; the copy routes name two
	// rigs in the body, the upload plan and run their one rig, and upload staging
	// none, since it writes only Commander's own staging directory.
	mux.HandleFunc("GET /api/rigs/{id}/files", s.guardRead(s.handleFilesList))
	mux.HandleFunc("GET /api/rigs/{id}/files/download", s.guardRead(s.handleFilesDownload))
	mux.HandleFunc("POST /api/rigs/{id}/files/mkdir", s.guardWrite(s.handleFilesMkdir))
	mux.HandleFunc("POST /api/rigs/{id}/files/delete/plan", s.guardWrite(s.handleFilesDeletePlan))
	mux.HandleFunc("POST /api/rigs/{id}/files/delete", s.guardWrite(s.handleFilesDelete))
	mux.HandleFunc("POST /api/files/copy/plan", s.guardWrite(s.handleFilesCopyPlan))
	mux.HandleFunc("POST /api/files/copy", s.guardWrite(s.handleFilesCopy))
	mux.HandleFunc("POST /api/rigs/{id}/files/rename", s.guardWrite(s.handleFilesRename))
	mux.HandleFunc("POST /api/files/upload/stage", s.guardWrite(s.handleFilesUploadStage))
	mux.HandleFunc("POST /api/files/upload/plan", s.guardWrite(s.handleFilesUploadPlan))
	mux.HandleFunc("POST /api/files/upload", s.guardWrite(s.handleFilesUpload))
	mux.HandleFunc("POST /api/files/upload/discard", s.guardWrite(s.handleFilesUploadDiscard))

	// Remotes. The :repair command rides {idAction} like the rig commands.
	mux.HandleFunc("GET /api/remotes", s.guardRead(s.handleListRemotes))
	mux.HandleFunc("GET /api/remotes/regions", s.guardRead(s.handleRemoteRegions))
	mux.HandleFunc("POST /api/remotes", s.guardWrite(s.handleCreateRemote))
	mux.HandleFunc("PATCH /api/remotes/{id}", s.guardWrite(s.handleRenameRemote))
	mux.HandleFunc("PUT /api/remotes/{id}/rate-limit", s.guardWrite(s.handleSetRemoteRateLimit))
	mux.HandleFunc("POST /api/remotes/{idAction}", s.guardWrite(s.handleRemoteCommand))
	mux.HandleFunc("DELETE /api/remotes/{id}", s.guardWrite(s.handleDeleteRemote))

	// Alerts.
	mux.HandleFunc("GET /api/alerts", s.guardRead(s.handleListAlerts))
	mux.HandleFunc("POST /api/alerts/{idAction}", s.guardWrite(s.handleAlertCommand))

	// App logs.
	mux.HandleFunc("GET /api/app-logs", s.guardRead(s.handleAppLogs))
	// Dedicated SSE tail for the app log (does its own auth, like /events).
	mux.HandleFunc("GET /api/app-logs/stream", s.handleAppLogStream)
	mux.HandleFunc("GET /api/app-logs/download", s.guardRead(s.handleAppLogDownload))

	// Diagnostics.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /metrics", s.metricsHandler())
	mux.HandleFunc("POST /api/diagnostics/pprof", s.guardWrite(s.handlePprof))
	mux.HandleFunc("POST /api/diagnostics/heap", s.guardWrite(s.handleHeapProfile))

	// SSE.
	mux.HandleFunc("GET /events", s.handleEvents)

	// Any other /api path is a 404 (never the SPA index).
	mux.HandleFunc("/api/", s.notFoundJSON)

	// SPA with history fallback for everything else.
	mux.HandleFunc("/", s.serveSPA)

	s.mux = mux
}

// registeredRoutes mirrors every pattern routes() registers, one entry per line
// and in the same order. It exists because http.ServeMux exposes no route
// enumeration, and the no-CORS-header invariant is load-bearing: the setup
// token's sufficiency rests on it being unreadable cross-origin, which holds
// only while no route emits an Access-Control-Allow-* header. A test driven from
// a list that silently stops covering routes added later would be worse than no
// test, so KEEP THIS IN SYNC WITH routes().
//
// The two catch-all patterns are registered without a method; they are listed
// with GET because a driver has to issue something.
var registeredRoutes = []string{
	"POST /api/session",
	"DELETE /api/session",
	"GET /api/setup",
	"POST /api/setup",
	"GET /api/config",
	"PUT /api/config",
	"POST /api/config/preview",
	"POST /api/roots/check",
	"GET /api/rigs",
	"POST /api/rigs",
	"GET /api/rigs/{id}",
	"PUT /api/rigs/{id}",
	"DELETE /api/rigs/{id}",
	"POST /api/rigs/{idAction}",
	"POST /api/pause",
	"POST /api/resume",
	"GET /api/frames",
	"GET /api/rigs/{id}/frames",
	"DELETE /api/rigs/{id}/frames/{frameId}",
	"POST /api/rigs/{id}/library/link",
	"POST /api/rigs/{id}/library/scan",
	"POST /api/rigs/{id}/frames/{frameId}/restore",
	"DELETE /api/rigs/{id}/frames/{frameId}/restore",
	"POST /api/rigs/{id}/frames/{frameId}/acknowledge",
	"POST /api/rigs/{id}/missing-groups/acknowledge",
	"POST /api/rigs/{id}/frames/{frameId}/forget",
	"POST /api/rigs/{id}/missing-leftovers/forget",
	"GET /api/library/status",
	"POST /api/rigs/{id}/retarget/preview",
	"POST /api/rigs/{id}/retarget/apply",
	"POST /api/rigs/{id}/refile/preview",
	"POST /api/rigs/{id}/refile/apply",
	"GET /api/frames/{frameId}/image",
	"GET /api/frames/{frameId}/image/meta",
	"GET /api/rigs/{id}/summary",
	"GET /api/rigs/{id}/logs",
	"POST /api/scan",
	"GET /api/discovered",
	"POST /api/discovered/{guid}/shares",
	"POST /api/platesolve/catalogs/{name}",
	"POST /api/platesolve/resolve",
	"GET /api/rigs/{id}/files",
	"GET /api/rigs/{id}/files/download",
	"POST /api/rigs/{id}/files/mkdir",
	"POST /api/rigs/{id}/files/delete/plan",
	"POST /api/rigs/{id}/files/delete",
	"POST /api/files/copy/plan",
	"POST /api/files/copy",
	"POST /api/rigs/{id}/files/rename",
	"POST /api/files/upload/stage",
	"POST /api/files/upload/plan",
	"POST /api/files/upload",
	"POST /api/files/upload/discard",
	"GET /api/remotes",
	"GET /api/remotes/regions",
	"POST /api/remotes",
	"PATCH /api/remotes/{id}",
	"PUT /api/remotes/{id}/rate-limit",
	"POST /api/remotes/{idAction}",
	"DELETE /api/remotes/{id}",
	"GET /api/alerts",
	"POST /api/alerts/{idAction}",
	"GET /api/app-logs",
	"GET /api/app-logs/stream",
	"GET /api/app-logs/download",
	"GET /healthz",
	"GET /metrics",
	"POST /api/diagnostics/pprof",
	"POST /api/diagnostics/heap",
	"GET /events",
	"GET /api/",
	"GET /",
}

// Platform categories the first-run help wording is chosen from. These are wire
// values returned by GET /api/setup, so the SPA switches on them.
const (
	PlatformContainer  = "container"
	PlatformKubernetes = "kubernetes"
	PlatformOther      = "other"
)

// containerMarkerEnv, when set, declares this process containerized. An image
// can bake it to make detection exact rather than inferred.
const containerMarkerEnv = "ASIAIR_COMMANDER_CONTAINER"

// PlatformCategory classifies where this process runs, so the first-run log line
// and the wizard's help panel can name a channel the operator actually has. It
// is exported because both consumers need the same answer: two copies would
// drift, and a wrong answer is worse than none — it sends the operator to a host
// log path that does not exist for them.
func PlatformCategory() string {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return PlatformKubernetes
	}
	if inContainer() {
		return PlatformContainer
	}
	return PlatformOther
}

// inContainer probes several signals rather than /.dockerenv alone, which misses
// podman, containerd, and LXC.
func inContainer() bool {
	if os.Getenv(containerMarkerEnv) != "" {
		return true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false // not Linux, or PID 1 is not ours: assume a plain host
	}
	cgroups := string(data)
	for _, marker := range []string{"docker", "kubepods", "containerd", "lxc", "podman", "/ecs/"} {
		if strings.Contains(cgroups, marker) {
			return true
		}
	}
	return false
}

// serveSPA serves an embedded asset when the path maps to a real file, otherwise
// index.html so client-side routes survive a reload (design architecture §8
// SPA-history fallback).
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" || p == "." {
		p = "index.html"
	}
	f, err := s.staticFS.Open(p)
	if err != nil {
		s.serveIndex(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.serveIndex(w, r)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		s.serveIndex(w, r)
		return
	}
	http.ServeContent(w, r, p, st.ModTime(), rs)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.staticFS.Open("index.html")
	if errors.Is(err, fs.ErrNotExist) {
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(placeholderHTML))
		return
	}
	if err != nil {
		http.Error(w, "SPA index not found", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "SPA index not seekable", http.StatusInternalServerError)
		return
	}
	st, _ := f.Stat()
	var mod time.Time
	if st != nil {
		mod = st.ModTime()
	}
	http.ServeContent(w, r, "index.html", mod, rs)
}

// --- shared helpers ------------------------------------------------------------

// currentConfig loads the live config, never returning nil (an empty config on a
// pre-setup pointer keeps handlers total).
func (s *Server) currentConfig() *model.Config {
	if cfg := s.deps.Config.Load(); cfg != nil {
		return cfg
	}
	return &model.Config{}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError emits a small {error, message} body at the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": http.StatusText(status), "message": msg})
}

// writeErrorCode emits {error, code, message}. The machine-readable code lets the
// SPA distinguish refusals that need different handling — a wrong setup token
// versus a window that already closed — without string-matching server prose,
// which fails silently across a version skew between SPA and binary.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{
		"error": http.StatusText(status), "code": code, "message": msg,
	})
}

func (s *Server) notFoundJSON(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "no such endpoint")
}

// decodeJSON reads a JSON body into v with a modest size cap. It takes an
// io.Reader (not the *http.Request) so a handler that must re-scan the body to
// localize a decode error — the config and rig endpoints — can buffer it first and
// decode from a bytes.Reader.
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, 4<<20)) // 4 MiB
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// defaultLoopback reports whether remoteAddr is a loopback address.
func defaultLoopback(remoteAddr string) bool {
	host := hostOnly(remoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostOnly strips a :port (or [::1]:port) suffix, returning the bare host, and
// drops the root label's trailing dot.
//
// The trailing-dot rule has to be applied on both sides of the comparison or not
// at all: checkHost and originHost both funnel through here, so without it an
// operator browsing "http://nas.local./" would have "nas.local" seeded into the
// allowlist and then fail checkOrigin against "nas.local.". Stripping in the one
// shared helper puts both sides in agreement. A trailing-dot IP literal becoming
// parseable is not a rebinding vector — rebinding forges a hostname, never a
// private literal — so the other callers (defaultLoopback, the login rate-limit
// key, isPrivateHostIP, allowedHosts) are unaffected.
func hostOnly(hostport string) string {
	h := hostport
	if bare, _, err := net.SplitHostPort(hostport); err == nil {
		h = bare
	}
	return strings.TrimSuffix(h, ".")
}

// cgnatPrefix is the RFC 6598 shared address space (100.64.0.0/10), used by
// carrier-grade NAT and by some home routers. netip.Addr.IsPrivate does not
// classify it as private, so isPrivateHostIP checks it explicitly.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// isPrivateHostIP reports whether hostport's host part is a literal IP address in
// a loopback, private, link-local, or CGNAT range. Such a Host cannot be produced
// by a DNS-rebinding attack — rebinding forges the attacker's hostname in the Host
// header, never a literal private IP — so these are auto-permitted without an
// allowlist entry, letting an operator reach the server by LAN IP. Public IP
// literals and hostnames return false and still require an allowlist entry.
func isPrivateHostIP(hostport string) bool {
	addr, err := netip.ParseAddr(hostOnly(hostport))
	if err != nil {
		return false // hostname, not an IP literal
	}
	addr = addr.Unmap()         // treat IPv4-in-IPv6 as IPv4
	return addr.IsLoopback() || // 127.0.0.0/8, ::1
		addr.IsPrivate() || // RFC1918 10/8, 172.16/12, 192.168/16; IPv6 ULA fc00::/7
		addr.IsLinkLocalUnicast() || // 169.254.0.0/16, fe80::/10
		cgnatPrefix.Contains(addr) // 100.64.0.0/10
}

// allowedHosts builds the case-folded Host allowlist for the current config:
// the configured names plus loopback, which is always permitted.
func (s *Server) allowedHosts(cfg *model.Config) map[string]bool {
	allow := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if cfg != nil {
		for _, h := range cfg.Web.AllowedHosts {
			allow[strings.ToLower(hostOnly(h))] = true
		}
	}
	return allow
}

// originHost extracts the comparable host from the Origin (or Referer) header;
// present reports whether either header was set.
func originHost(r *http.Request) (host string, present bool) {
	o := r.Header.Get("Origin")
	if o == "" {
		o = r.Header.Get("Referer")
	}
	if o == "" {
		return "", false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return "", true // present but unparseable → caller rejects
	}
	return strings.ToLower(hostOnly(u.Host)), true
}
