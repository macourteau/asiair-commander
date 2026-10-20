// Command asiair-commander is the ASIAIR Commander service binary: the
// composition root that wires every internal package into a running process and
// the verb dispatcher the Windows installer drives (design architecture §19).
// Verbs: run (default), install, uninstall, healthcheck, version, and remote
// (the remote role, see internal/remote/role).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	// The device clock sync resolves the IANA zone a rig reports before writing it
	// back. Windows has no system tz database outside a Go installation, and the
	// distroless runtime image is not guaranteed one, so the database is embedded
	// rather than depended on.
	_ "time/tzdata"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/imgpreview"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/manager"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/platesolve"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/role"
	"github.com/macourteau/asiair-commander/internal/remote/supervisor"
	"github.com/macourteau/asiair-commander/internal/rig"
	svc "github.com/macourteau/asiair-commander/internal/service"
	"github.com/macourteau/asiair-commander/internal/store"
	"github.com/macourteau/asiair-commander/internal/updatecheck"
	"github.com/macourteau/asiair-commander/internal/web"
)

// Version metadata injected by the Makefile via -ldflags. Defaults keep an
// untagged `go build` working.
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

// deviceFactory is the worker's device constructor seam: device.NewFactory over
// the site registry in production, an in-memory fake under `-tags demo` (see
// demo.go / demo_off.go). Declared here (untagged) so both build variants share
// the one type.
type deviceFactory = func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device

// startedAt anchors the application uptime the SPA displays. Set at process load
// (package init), so it reflects when this binary started, not host boot.
var startedAt = time.Now()

// Service identity (design architecture §19). The name must be stable — it is
// the SCM registration key the install/uninstall/run paths all share.
const (
	serviceName    = "ASIAIRCommander"
	serviceDisplay = "ASIAIR Commander"
	serviceDesc    = "Watches ASIAIR rigs over SMB and files captured frames onto the NAS."
	dataDirName    = "ASIAIRCommander"
	defaultWebPort = 8686
)

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain parses the verb and its flags, then dispatches. It returns the
// process exit code so main stays a one-liner and tests can drive it.
func realMain(argv []string) int {
	verb := "run"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		verb, argv = argv[0], argv[1:]
	}

	if verb == "version" {
		fmt.Printf("asiair-commander %s (commit %s, built %s)\n", version, commit, buildTime)
		return 0
	}
	// The remote role is dispatched before the data directory is resolved or
	// created: it keeps no state and writes no file.
	if verb == "remote" {
		return role.Main(argv, proto.Build{Version: version, Commit: commit, Date: buildTime})
	}

	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	dataDirFlag := fs.String("data-dir", "", "override the data directory (config, state, logs)")
	debug := fs.Bool("debug", false, "enable debug logging")
	pidFile := fs.String("pid-file", "", "run: write this process's PID to this file, refuse to start while it names a running process, and remove it on shutdown")
	demo := registerDemoFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if os.Getenv("ASIAIR_DEBUG") != "" {
		*debug = true
	}

	dataDir, err := resolveDataDir(*dataDirFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve data dir:", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "create data dir:", err)
		return 1
	}

	switch verb {
	case "install", "uninstall":
		return controlVerb(verb)
	case "healthcheck":
		return healthcheckVerb(dataDir)
	case "run":
		return runVerb(dataDir, *debug, *pidFile, demo)
	default:
		fmt.Fprintf(os.Stderr, "unknown verb %q; valid verbs: run, install, uninstall, healthcheck, version, remote\n", verb)
		return 2
	}
}

// resolveDataDir picks the application data directory: an explicit override, else
// %ProgramData%\ASIAIRCommander on Windows, else an XDG/HOME dev path.
func resolveDataDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			return "", errors.New("%ProgramData% is not set")
		}
		return filepath.Join(pd, dataDirName), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "asiair-commander"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "asiair-commander"), nil
}

// controlVerb installs or uninstalls the service.
func controlVerb(action string) int {
	if err := svc.Control(serviceName, serviceDisplay, serviceDesc, action); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("service %s: %s\n", serviceName, action)
	return 0
}

// runVerb sets up logging and drives the app under the service manager.
func runVerb(dataDir string, debug bool, pidFile string, demo *demoFlags) int {
	logger, appLog, closeLog, err := setupLogging(dataDir, debug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup logging:", err)
		return 1
	}
	defer closeLog()

	logger.Info("asiair-commander starting", "version", version, "commit", commit, "data_dir", dataDir)

	// Claimed before anything binds, so a second instance is refused here rather
	// than after it has wandered to a fallback port.
	if pidFile != "" {
		release, err := claimPIDFile(pidFile, logger)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer release()
	}

	app := &commanderApp{dataDir: dataDir, logger: logger, appLog: appLog, demo: demo}
	if err := svc.Run(serviceName, serviceDisplay, serviceDesc, app, logger); err != nil {
		logger.Error("service exited with error", "err", err)
		return 1
	}
	logger.Info("asiair-commander stopped")
	return 0
}

// commanderApp is the service App: its Run is the full composition.
type commanderApp struct {
	dataDir string
	logger  *slog.Logger
	appLog  *web.LogBuffer
	demo    *demoFlags
}

func (a *commanderApp) Run(ctx context.Context) error {
	// A no-op outside a `-tags demo` build; in one, ctx ends at the demo's
	// maximum lifetime (see demo_lifetime.go).
	ctx, cancel := a.demo.limitLifetime(ctx, a.logger)
	defer cancel()
	return runStack(ctx, a.dataDir, a.logger, a.appLog, nil)
}

// runStack wires the whole application and serves until ctx is cancelled, then
// shuts down gracefully. ready, when non-nil, is called with the actual served
// base URL once the listener is bound — tests use it to reach /healthz without
// racing the port fallback.
func runStack(ctx context.Context, dataDir string, logger *slog.Logger, appLog *web.LogBuffer, ready func(url string)) error {
	configPath := filepath.Join(dataDir, "config.yaml")

	cfg, err := config.Load(configPath)
	switch {
	case err == nil:
		logger.Info("config loaded", "path", configPath)
	case errors.Is(err, config.ErrNotFound):
		logger.Warn("no config found — starting with defaults; complete first-run setup in the web UI with the setup token", "path", configPath)
		cfg = config.Defaults()
	case errors.Is(err, config.ErrSchemaTooNew):
		return fmt.Errorf("config %s was written by a newer version of asiair-commander; upgrade the binary: %w", configPath, err)
	default:
		return fmt.Errorf("load config %s: %w", configPath, err)
	}

	cfgPtr := &atomic.Pointer[model.Config]{}
	cfgPtr.Store(cfg)

	st, err := store.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer st.Close()

	// The metrics surface is built here (after the store exists so its pool stats can
	// register) and injected into every subsystem below. The state collector is
	// registered once, further down, after all its providers are constructed.
	metricsObj := metrics.New(version)
	store.RegisterPoolMetrics(st, metricsObj)
	appLog.SetMetrics(metricsObj)

	statusHub := hub.New(logger)
	statusHub.SetMetrics(metricsObj)
	notifier := notify.New(st, statusHub, metricsObj, logger)
	lfs := localfs.New()

	// Build the discovery scanner and warm the registry with one synchronous
	// scan_air round BEFORE the manager starts, so a worker's first cycle can
	// already resolve/correlate its device. The config guard serializes every
	// config write (web + the manager's migration applier).
	//
	// The site registry routes every device dial and identity probe to the
	// network its endpoint names; it holds only the local site until a remote is
	// registered. The scanner, the device factory and every worker share it.
	sites := netsite.NewRegistry(logger)
	prodDevice := device.NewFactory(sites)
	scanner := discovery.NewDefault(sites, prodDevice, cfgPtr, time.Now, logger)
	guard := &config.Guard{}

	// The remote supervisor registers one site per paired remote (down until
	// its tunnel connects and its first broadcast round completes) and is the
	// only writer of the config's remotes. It starts before the warm-up scan so
	// every remote's site exists by then; a remote still connecting simply
	// contributes nothing to that round. Deferred before the manager's Stop, so
	// it runs after it: the workers end before their tunnels close.
	remoteOpts := supervisor.Options{
		Sites:      sites,
		Discovery:  scanner,
		Store:      st,
		Hub:        statusHub,
		Config:     cfgPtr,
		Guard:      guard,
		ConfigPath: configPath,
		Build:      proto.Build{Version: version, Commit: commit, Date: buildTime},
		Metrics:    metricsObj,
		Notify:     notifier,
		Alerts:     st,
		Rigs:       statusHub,
		Logger:     logger,
		ForgetEndpoints: func(ctx context.Context, rigIDs []string, site netsite.ID) (int, error) {
			return rig.ForgetLastEndpointsOnSite(ctx, st, rigIDs, site)
		},
	}
	// installDemoRemotes is a no-op in a normal build; in a `-tags demo` build
	// whose data dir holds a tools/seedremote scenario it swaps each remote's
	// tunnel client for an in-memory remote (see demo_remote.go).
	remoteOpts = installDemoRemotes(ctx, dataDir, remoteOpts, logger)
	remotes, err := supervisor.New(remoteOpts)
	if err != nil {
		return fmt.Errorf("remote supervisor: %w", err)
	}
	remotes.Start(ctx)
	defer remotes.Stop()

	scanner.Scan(ctx)

	adm := manager.NewAdmission(cfg.MaxConcurrentTransfers, lfs, cfg.Roots.Staging, cfg.DiskReserveBytes)
	// installDemo is a no-op in a normal build; in a `-tags demo` build it swaps the
	// device, resolver, and header for in-memory fakes and starts a frame generator,
	// so the whole server runs end-to-end with no real ASIAIR (see demo.go).
	newDevice, workerResolver, workerHdr := installDemo(ctx, prodDevice, scanner, header.New(), cfgPtr, lfs, logger)

	// Preview renderer: one process-wide instance from a config snapshot (all three
	// preview_* fields are apply-on-restart). Disabled → nil, guarded at every call
	// site (the worker skips warming, the web handler returns 503).
	var previewer imgpreview.Previewer
	if cfg.PreviewsEnabled {
		previewer = imgpreview.New(filepath.Join(dataDir, "previews"),
			cfg.PreviewConcurrency, cfg.PreviewCacheMaxBytes, header.New(), metricsObj, logger)
	}

	// Tier-1 plate solving (design features §3). Provisioned and
	// solved only when enabled. The nudge channel wakes the solve worker on
	// freshly-filed hinted lights and after a catalog download; nil disables the
	// ingest hook and the solver. The DB is the queue of record, so a full channel is
	// harmless.
	var nudge chan int64
	var prov *platesolve.Provisioner
	if cfg.PlateSolveEnabled {
		nudge = make(chan int64, 256)
		prov = platesolve.NewProvisioner(dataDir, cfg, st, notifier, lfs, statusHub, nudge, logger)
		prov.Start(ctx)
	}

	mgr := manager.New(cfgPtr, adm, newDevice, lfs, workerHdr, st, statusHub, notifier, previewer, nudge, time.Local, logger,
		workerResolver, sites, guard, configPath, metricsObj)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start rig manager: %w", err)
	}
	// Close the loop from discovery back to the workers: a managed device
	// answering a scan round wakes its rig, so one that is offline re-probes at
	// once instead of waiting out its retry interval. Injected after Start rather
	// than at construction because the Scanner is built first — the manager takes
	// it as its resolver.
	scanner.SetWaker(mgr)
	// Likewise a remote changing state wakes the rigs reached through it, so each
	// worker writes the card that goes with the remote's state at once.
	remotes.SetWaker(mgr)
	defer mgr.Stop()

	if cfg.PlateSolveEnabled {
		solver := platesolve.NewASTAP(prov, header.New(), logger)
		// Heal any solve interrupted by a crash (solving → none) and, as a safety net,
		// re-queue below-floor rejects the deepest installed catalog now covers (the
		// live trigger is a catalog download completing).
		if n, err := st.ReclaimSolvingFrames(ctx); err != nil {
			logger.Warn("reclaim interrupted solves failed", "error", err)
		} else if n > 0 {
			logger.Info("reclaimed interrupted solves", "n", n)
		}
		if f, ok := prov.DeepestInstalledFloor(); ok {
			if n, err := st.RequeueFloorUnsolvable(ctx, f); err == nil && n > 0 {
				logger.Info("re-queued below-floor frames at startup", "n", n)
			}
		}
		go rig.RunSolver(ctx, st, header.New(), solver, prov, statusHub, nudge, cfgPtr, logger)
	}

	// Backfill astrometry for already-filed frames once, in the background. It is
	// display-only and NULL-guarded (idempotent across restarts), so its ordering
	// relative to the workers does not matter; the app context bounds it. It nudges
	// the solve worker for each hinted light it marks `none`.
	go rig.BackfillAstrometry(ctx, st, header.New(), nudge, logger)

	// Backfill the exposure identity of rows filed before that column existed, so
	// device-side revision detection covers them too. It is paced by a duty cycle
	// against the destination volume rather than by a population argument, and
	// nothing waits on it: any row that needs its value before the cursor arrives
	// has it computed on demand under the same proof.
	go rig.BackfillDataDigests(ctx, st, lfs, logger)

	// The warm-up round already ran synchronously above, so RunBackground skips its
	// own eager first scan and just refreshes on the timer (0 → backgroundInterval).
	go scanner.RunBackground(ctx, 0)

	// Register the scrape-time state collector now that every provider exists. It
	// reads live state at scrape time (no 5s ticker, no snapshot copy), so the gauges
	// are never stale. Each provider is a lock-free or pure-memory read (hazard P2).
	metricsObj.RegisterStateCollector(metrics.StateProviders{
		Snapshot:  statusHub,
		Admission: adm,
		Preview:   previewer, // nil when previews are disabled → collector skips those series
		Hub:       statusHub,
		Discovery: scanner,
	})

	// The device file browser dials through the same factory and identity
	// resolver as the workers (the demo fakes under `-tags demo`), reads each
	// rig's endpoint from the hub snapshot its worker publishes, and stats NAS
	// copies through the workers' local filesystem. Closed after the web server
	// has shut down, so no request is left holding a session. Uploads are staged
	// under the data directory, which is emptied here, before the web server
	// listens: the staged files' records live in memory only, so none may
	// outlive a restart.
	files := devfiles.New(devfiles.Deps{
		Config:     cfgPtr,
		Store:      st,
		LocalFS:    lfs,
		Status:     statusHub,
		Prober:     workerResolver,
		Sites:      sites,
		NewDevice:  newDevice,
		StagingDir: filepath.Join(dataDir, "upload-staging"),
		Logger:     logger,
	})
	defer files.Close()
	if err := files.StartStaging(); err != nil {
		return fmt.Errorf("upload staging: %w", err)
	}

	// The update checker reads its switch from the live config pointer, so a
	// Settings change applies at once. srv is assigned before Run starts, so
	// OnChange (only ever called from Run) always sees it.
	var srv *web.Server
	updates := updatecheck.New(updatecheck.Options{
		Version:  version,
		Enabled:  func() bool { return !cfgPtr.Load().UpdateCheckDisabled },
		OnChange: func() { srv.AnnounceServerInfo() },
		Logger:   logger,
		// demoReleasesURL is "" outside a `-tags demo` build (see demo_off.go).
		BaseURL: demoReleasesURL(),
	})

	srv = web.New(web.Deps{
		ConfigPath:          configPath,
		DataDir:             dataDir,
		Config:              cfgPtr,
		Guard:               guard,
		Supervisor:          mgr,
		RigNotFound:         manager.ErrRigNotFound,
		FrameNotMissing:     rig.ErrFrameNotMissing,
		Remotes:             remotes,
		RemoteNotFound:      supervisor.ErrUnknownRemote,
		RemoteRegionUnknown: supervisor.ErrUnknownRegion,
		RelayUnavailable:    supervisor.ErrRelayUnavailable,
		Store:               st,
		Hub:                 statusHub,
		Scanner:             scanner,
		LocalFS:             lfs,
		AppLog:              appLog,
		AppLogPath:          appLogPath(dataDir),
		Metrics:             metricsObj,
		Header:              header.New(),
		Previewer:           previewer,
		Prov:                prov,
		DeviceFiles:         files,
		Logger:              logger,
		Now:                 time.Now,
		Version:             version,
		StartedAt:           startedAt,
		LatestVersion:       updates.Latest,
	})
	go updates.Run(ctx)

	return serveHTTP(ctx, dataDir, cfg, srv, logger, ready)
}

// serveHTTP binds the web listener with the design architecture §7 port fallback,
// optionally wraps it in TLS, serves until ctx is cancelled, then shuts down
// gracefully. It takes the server rather than its handler because the first-run
// hand-off needs the token and the token-file writer, and both only become useful
// once the bind has fixed the port and the scheme.
func serveHTTP(ctx context.Context, dataDir string, cfg *model.Config, srv *web.Server, logger *slog.Logger, ready func(url string)) error {
	ln, err := bindListener(cfg.WebPort, logger)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE stream is a long-lived response.
		IdleTimeout: 120 * time.Second,
	}

	boundPort := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://localhost:%d/", boundPort)

	// HTTPS on the next port up, over the same handler. Derived from the port that
	// actually bound rather than cfg.WebPort, so the pair stays adjacent in fact
	// even after the design architecture §7 fallback moved HTTP. Best-effort
	// throughout: losing encryption must never cost the operator the app.
	tlsSrv, tlsLn, tlsURL := bindTLS(dataDir, srv.Handler(), boundPort, logger)

	if tlsURL != "" {
		logger.Info("web server listening", "url", url, "tls_url", tlsURL, "requested_port", cfg.WebPort)
	} else {
		logger.Info("web server listening", "url", url, "requested_port", cfg.WebPort)
	}
	if ready != nil {
		ready(url)
	}

	if srv.SetupToken() != "" {
		// The file channel is best-effort: a read-only or oddly-permissioned data
		// directory must not prevent setup, because the log still carries the token.
		if err := srv.WriteSetupTokenFile(url); err != nil {
			logger.Warn("could not write the setup token file; the service log still carries the token",
				"path", filepath.Join(dataDir, web.SetupTokenFileName), "err", err)
		}
		logFirstRunSetup(logger, srv, url, boundPort, dataDir)
		// Log rotation can evict the startup line on a long-idle container, so
		// re-emit it while the window is open. serveHTTP holds cfg as a snapshot and
		// cannot observe a password appearing, so the token itself is the signal:
		// the setup handler clears it on success.
		go reemitFirstRunSetup(ctx, logger, srv, url, boundPort, dataDir)
	}

	// Only the HTTP listener reports here: a TLS failure is logged instead, so it
	// can never terminate the app. That also keeps this a single-producer channel,
	// which matters because nothing drains it once the select below returns.
	serveErr := make(chan error, 1)
	go func() {
		if e := httpSrv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			serveErr <- e
		}
	}()
	if tlsSrv != nil {
		go func() {
			// Certs live in tlsSrv.TLSConfig, so the file args are empty.
			if e := tlsSrv.ServeTLS(tlsLn, "", ""); e != nil && !errors.Is(e, http.ErrServerClosed) {
				// A TLS-only failure must not take the app down: report and let plain
				// HTTP carry on.
				logger.Error("TLS listener stopped — plain HTTP is unaffected", "err", e)
			}
		}()
	}

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case e := <-serveErr:
		logger.Error("web server error", "err", e)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if tlsSrv != nil {
		if err := tlsSrv.Shutdown(shutCtx); err != nil {
			logger.Warn("graceful TLS shutdown incomplete", "err", err)
			_ = tlsSrv.Close()
		}
	}
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		logger.Warn("graceful shutdown incomplete", "err", err)
		return httpSrv.Close()
	}
	return nil
}

// bindTLS brings up the HTTPS listener on httpPort+1 over the same handler,
// returning nils when it cannot. Every failure is a warning, never an error: the
// plain-HTTP listener is already up by the time this runs and must survive
// whatever happens here.
//
// There is deliberately no port-fallback chain. A TLS listener that wandered to
// an unpredictable port would be worse than none, because the operator could not
// point a proxy at it.
func bindTLS(dataDir string, h http.Handler, httpPort int, logger *slog.Logger) (*http.Server, net.Listener, string) {
	tlsCfg, err := ensureTLSConfig(dataDir)
	if err != nil {
		logger.Warn("no HTTPS listener: could not prepare the self-signed certificate", "err", err)
		return nil, nil, ""
	}

	port := httpPort + 1
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		logger.Warn("no HTTPS listener: could not bind the TLS port", "port", port, "err", err)
		return nil, nil, ""
	}

	return &http.Server{
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE stream is a long-lived response.
		IdleTimeout: 120 * time.Second,
	}, ln, fmt.Sprintf("https://localhost:%d/", port)
}

// firstRunReemitInterval is how often the first-run line is repeated while no
// password is set.
const firstRunReemitInterval = 30 * time.Minute

// logFirstRunSetup emits the one line an operator needs to finish setup: the
// token, and where to find it again on this platform.
//
// url is the loopback address the service bound and is NOT an address a remote
// operator can use, so it is named console_url rather than presented as a link,
// which would point a remote operator at an address they cannot reach.
func logFirstRunSetup(logger *slog.Logger, srv *web.Server, url string, boundPort int, dataDir string) {
	tok := srv.SetupToken()
	if tok == "" {
		return
	}
	logger.Warn("first-run setup: no password is set yet",
		"console_url", url, "port", boundPort, "setup_token", tok,
		"token_file", filepath.Join(dataDir, web.SetupTokenFileName),
		"hint", "open Commander in a browser and paste this token when the wizard "+
			"asks for it; find it again with: "+tokenHintForPlatform(dataDir))
}

// reemitFirstRunSetup repeats the first-run line until a password is set or ctx
// ends.
func reemitFirstRunSetup(ctx context.Context, logger *slog.Logger, srv *web.Server, url string, boundPort int, dataDir string) {
	t := time.NewTicker(firstRunReemitInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if srv.SetupToken() == "" {
				return // setup completed: the token is inert and must not be repeated
			}
			logFirstRunSetup(logger, srv, url, boundPort, dataDir)
		}
	}
}

// tokenHintForPlatform names a channel the operator running this deployment
// actually has. A container operator has no host log path, and pointing them at
// one is worse than saying nothing.
func tokenHintForPlatform(dataDir string) string {
	switch web.PlatformCategory() {
	case web.PlatformKubernetes:
		return "kubectl logs <pod>"
	case web.PlatformContainer:
		return "docker logs <container>"
	default:
		return filepath.Join(dataDir, "logs") + ", or the file " +
			filepath.Join(dataDir, web.SetupTokenFileName)
	}
}

// bindListener implements the design architecture §7 bind fallback: the
// configured port, then the default 8686, then an OS-chosen ephemeral port — so
// a bad web_port can never lock the operator out of the UI. It logs each
// fallback.
func bindListener(webPort int, logger *slog.Logger) (net.Listener, error) {
	var tryPorts []int
	if webPort != 0 {
		tryPorts = append(tryPorts, webPort)
	}
	if webPort != defaultWebPort {
		tryPorts = append(tryPorts, defaultWebPort)
	}

	for _, p := range tryPorts {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			return ln, nil
		}
		logger.Warn("web port unavailable — trying fallback", "port", p, "err", err)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("no web port available (configured, default, and ephemeral all failed): %w", err)
	}
	logger.Warn("bound an OS-chosen ephemeral port after all fixed ports failed")
	return ln, nil
}

// healthcheckVerb polls /healthz for up to 30s, exiting 0 on the first 200. The
// installer calls this to wait for the service to become ready (design
// architecture §19).
func healthcheckVerb(dataDir string) int {
	port := defaultWebPort
	if cfg, err := config.Load(filepath.Join(dataDir, "config.yaml")); err == nil {
		if cfg.WebPort != 0 {
			port = cfg.WebPort
		}
	}

	// Always the plain-HTTP port: it is served unconditionally, which keeps the
	// probe free of the self-signed cert the HTTPS port would present.
	url := fmt.Sprintf("http://localhost:%d/healthz", port)
	client := &http.Client{Timeout: 2 * time.Second}

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Println("healthy")
				return 0
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %s did not report healthy within 30s\n", url)
			return 1
		}
		time.Sleep(1 * time.Second)
	}
}
