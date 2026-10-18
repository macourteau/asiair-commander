package platesolve

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/store"
)

// catalogPin is one star catalog's download provenance and coverage. The catalogs
// are static upstream (unchanged since 2023), pinned to their stable SourceForge
// URLs with a SHA-256 computed once; a new pin is a manual bump like any other
// external. SHA256 "" would fall back to size-only verification, but every shipped
// catalog carries a real checksum.
type catalogPin struct {
	Name   string
	URL    string
	Size   int64   // pinned zip size (bytes); verified against the final file
	SHA256 string  // hex; "" ⇒ size-verified only
	Floor  float64 // smallest field of view (deg) this catalog can solve
	Label  string  // human coverage note for the Settings list
}

// catalogPins is ordered shallow → deep (largest floor first), which is exactly the
// order CatalogFor walks to pick the shallowest (fastest) covering catalog. d05 is
// the wide-field auto-default; d20/d50 are on-demand. (d80 is intentionally omitted
// — its upstream URL does not resolve, and d50's ~0.2° floor already covers the
// realistic long-focal-length range.)
var catalogPins = []catalogPin{
	{Name: "d05", URL: catalogURL("d05"), Size: 102151943, SHA256: "39d59b73cb960fa0956c35162e70b09a8af10ef20a1e8f5cae7b77d48be5ce31", Floor: 0.6, Label: "solves fields ≳0.6° (wide-field default)"},
	{Name: "d20", URL: catalogURL("d20"), Size: 399561156, SHA256: "c1cb092cb47563ebf095988358746e0c905a16f6a9b860152bc1caf10b788252", Floor: 0.3, Label: "solves fields ≳0.3°"},
	{Name: "d50", URL: catalogURL("d50"), Size: 901308858, SHA256: "ca89a958ca0808185f0e546541b4ac73addc40fee4ad8bdd8bb79981c1dd8b43", Floor: 0.2, Label: "solves fields ≳0.2°"},
}

// defaultCatalog is auto-downloaded on first run when no catalog is installed.
const defaultCatalog = "d05"

// downloadFailAlertAfter is how many consecutive auto-download attempts may fail
// before an alert is raised, so a persistently-stuck catalog download is visible
// rather than silently leaving solving paused forever.
const downloadFailAlertAfter = 3

func catalogURL(name string) string {
	return "https://sourceforge.net/projects/astap-program/files/star_databases/" +
		name + "_star_database.zip/download"
}

// Provisioner makes the astap_cli binary and a star catalog available, and owns the
// solve/download mutual-exclusion gate. Which catalogs are installed is disk state
// (the presence of .1476 tiles under <dataDir>/astap/<name>/), tracked in memory
// and refreshed after each download.
type Provisioner struct {
	dataDir  string
	astapDir string // <dataDir>/astap
	cfg      *model.Config
	st       store.Store
	notifier notify.Notifier
	lfs      localfs.LocalFS
	hub      *hub.StatusHub
	nudge    chan<- int64
	log      *slog.Logger
	client   *http.Client

	// download and retryBackoff are the auto-download's catalog fetch and its wait
	// between attempts: DownloadCatalog and backoff, replaced only by tests.
	download     func(ctx context.Context, name string) error
	retryBackoff func(attempt int) time.Duration

	// gate serializes solving and downloading (a size-1 channel semaphore, not a
	// Mutex, so acquire can be select-ed against ctx.Done()).
	gate chan struct{}
	// downloadWaiting is set for the whole span a DownloadCatalog is queued-or-running
	// so the solve worker yields the backlog to it (the download preempts after the
	// current solve, rather than waiting for the whole backlog to drain). Read
	// lock-free by the worker via DownloadPending.
	downloadWaiting atomic.Bool

	mu          sync.Mutex
	appCtx      context.Context // captured from Start; downloads run under it, not a request ctx
	binaryPath  string
	installed   map[string]bool // catalog name → installed
	downloading string          // catalog name in flight (for dedup + status)

	// state/detail mirror what SetSolverStatus last published, so a progress update
	// can re-publish the whole ProvisionStatus without recomputing.
	state  string
	detail string
}

// NewProvisioner constructs the provisioner. notifier (alert on repeated failure),
// lfs (free-space precheck), hub (status), st + nudge (event-driven re-queue after a
// deeper catalog lands) are all real dependencies.
func NewProvisioner(dataDir string, cfg *model.Config, st store.Store, notifier notify.Notifier,
	lfs localfs.LocalFS, h *hub.StatusHub, nudge chan<- int64, logger *slog.Logger) *Provisioner {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Provisioner{
		dataDir:   dataDir,
		astapDir:  filepath.Join(dataDir, "astap"),
		cfg:       cfg,
		st:        st,
		notifier:  notifier,
		lfs:       lfs,
		hub:       h,
		nudge:     nudge,
		log:       logger.With("component", "platesolve"),
		client:    &http.Client{Timeout: 0}, // no total cap (catalogs are large); ctx bounds it
		gate:      make(chan struct{}, 1),
		installed: map[string]bool{},
		state:     "provisioning",
	}
	p.download = p.DownloadCatalog
	p.retryBackoff = backoff
	return p
}

// Start resolves the binary, scans installed catalogs, and — if none is installed —
// kicks off the background auto-download of the default catalog. It captures the app
// context so a later DownloadCatalog outlives the HTTP request that triggered it and
// is cancelled only by shutdown. When plate solving is disabled it publishes a
// "disabled" status and returns.
func (p *Provisioner) Start(ctx context.Context) {
	p.mu.Lock()
	p.appCtx = ctx
	p.mu.Unlock()

	if p.cfg == nil || !p.cfg.PlateSolveEnabled {
		p.setStatus("disabled", "plate solving is turned off")
		return
	}
	if err := os.MkdirAll(p.WorkDir(), 0o755); err != nil {
		p.log.Warn("could not create work dir", "error", err)
	}

	bin := resolveBinary(p.dataDir)
	p.mu.Lock()
	p.binaryPath = bin
	p.mu.Unlock()
	if bin == "" {
		p.setStatus("failed", "astap_cli not found — bundle it beside the service binary or install it on PATH")
		p.log.Warn("astap_cli not resolved; plate solving paused (bundle it or add it to PATH)")
	} else {
		p.log.Info("astap_cli resolved", "path", bin)
	}

	p.refreshInstalled()

	if p.Ready() {
		p.setStatus("ready", "")
		return
	}
	if bin == "" {
		return // no point downloading a catalog we can't use yet; status already "failed"
	}
	// Binary present but no catalog: auto-download the default in the background.
	p.setStatus("provisioning", "downloading the "+defaultCatalog+" star catalog")
	go p.autoDownloadDefault(ctx)
}

// autoDownloadDefault downloads the default catalog, retrying with a growing
// backoff so a transient network blip self-heals. From downloadFailAlertAfter
// consecutive failures on, a persistent problem is visible as one standing alert:
// its message carries no attempt count or error (those are logged), so every
// retry lands on the same row and an acknowledgement holds while it keeps
// failing; a successful download resolves it (design architecture §10).
func (p *Provisioner) autoDownloadDefault(ctx context.Context) {
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		err := p.download(ctx, defaultCatalog)
		if err == nil {
			p.resolveDownloadAlert(ctx)
			return
		}
		p.log.Warn("auto catalog download failed", "catalog", defaultCatalog, "attempt", attempt, "error", err)
		if attempt >= downloadFailAlertAfter {
			p.notifier.Raise(ctx, model.Alert{
				Severity: model.SeverityWarn,
				Category: model.AlertConfig,
				Message:  downloadAlertPrefix + "; plate solving is paused, and Commander keeps retrying. Check that it can reach the internet; the log has each attempt's error.",
				Standing: true,
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.retryBackoff(attempt)):
		}
	}
}

// downloadAlertPrefix starts the auto-download failure alert's message, which is
// how the alert is found among the fleet-wide config alerts to resolve it.
const downloadAlertPrefix = "Couldn't download the " + defaultCatalog + " star catalog after repeated attempts"

// resolveDownloadAlert resolves a standing auto-download failure alert once the
// catalog has downloaded, and refreshes the badge.
func (p *Provisioner) resolveDownloadAlert(ctx context.Context) {
	if p.st == nil {
		return
	}
	n, err := p.st.ResolveAlerts(ctx, "", model.AlertConfig, downloadAlertPrefix, time.Now().Unix())
	if err != nil {
		p.log.Warn("could not resolve the catalog download alert", "error", err)
		return
	}
	if n == 0 {
		return
	}
	p.log.Info("catalog downloaded; resolved its failure alert", "catalog", defaultCatalog)
	if c, err := p.st.UnackedCount(ctx); err == nil && p.hub != nil {
		p.hub.SetUnackedAlerts(c)
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt) * 30 * time.Second
	if d > 10*time.Minute {
		return 10 * time.Minute
	}
	return d
}

// Ready reports the binary is resolved and at least one catalog is installed.
func (p *Provisioner) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.binaryPath != "" && len(p.installed) > 0
}

// BinaryPath returns the resolved astap_cli path ("" if unresolved).
func (p *Provisioner) BinaryPath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.binaryPath
}

// WorkDir is the per-solve scratch area astap writes its output files into.
func (p *Provisioner) WorkDir() string { return filepath.Join(p.astapDir, "work") }

// catalogDir is the install path for a named catalog.
func (p *Provisioner) catalogDir(name string) string { return filepath.Join(p.astapDir, name) }

// CatalogFor returns the shallowest (fastest) installed catalog whose floor ≤ the
// frame's field of view. ok=false means no installed catalog covers the field.
func (p *Provisioner) CatalogFor(fovDeg float64) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range catalogPins { // shallow → deep
		if p.installed[c.Name] && c.Floor <= fovDeg {
			return p.catalogDir(c.Name), true
		}
	}
	return "", false
}

// SmallestCovering returns the name of the shallowest catalog (installed or not)
// whose floor ≤ fovDeg — the one to recommend downloading when CatalogFor misses.
func SmallestCovering(fovDeg float64) (string, bool) {
	for _, c := range catalogPins {
		if c.Floor <= fovDeg {
			return c.Name, true
		}
	}
	return "", false
}

// DeepestInstalledFloor returns the smallest floor among installed catalogs (the
// deepest catalog's coverage), for the startup below-floor re-queue safety net.
func (p *Provisioner) DeepestInstalledFloor() (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	floor, ok := 0.0, false
	for _, c := range catalogPins {
		if p.installed[c.Name] && (!ok || c.Floor < floor) {
			floor, ok = c.Floor, true
		}
	}
	return floor, ok
}

// CatalogInstalled reports whether a named catalog is installed (for the download
// endpoint's already-installed no-op).
func (p *Provisioner) CatalogInstalled(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.installed[name]
}

// Downloading reports the catalog currently downloading, if any (for the endpoint's
// already-in-flight 409).
func (p *Provisioner) Downloading() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.downloading, p.downloading != ""
}

// DownloadPending reports that a catalog download is queued or running, so the solve
// worker should yield the gate to it rather than starting another solve.
func (p *Provisioner) DownloadPending() bool { return p.downloadWaiting.Load() }

// ResolveFailed re-queues the `failed` frames for another solve attempt (the manual
// "re-solve" action, e.g. after installing a deeper catalog) and wakes the solver.
// Returns the number re-queued.
func (p *Provisioner) ResolveFailed(ctx context.Context) (int, error) {
	n, err := p.st.RequeueSolveFailures(ctx)
	if err == nil && n > 0 {
		p.wake()
	}
	return n, err
}

// BeginSolve acquires the solve/download gate. It blocks while a catalog download
// holds the gate, and unblocks on ctx cancel so shutdown isn't wedged behind a
// multi-minute download.
func (p *Provisioner) BeginSolve(ctx context.Context) (func(), error) {
	select {
	case p.gate <- struct{}{}:
		return func() { <-p.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DownloadCatalog downloads and installs a named catalog, then re-queues the
// below-floor unsolvable frames it now covers and wakes the solver. It dedups a
// double-click under a small lock (NOT the gate) before acquiring the gate, so two
// POSTs can't both re-download and callers don't pile up on the gate. It runs under
// the app context captured at Start (not a request ctx), so the 202 returned by the
// endpoint doesn't cancel the download; app shutdown does.
func (p *Provisioner) DownloadCatalog(reqCtx context.Context, name string) error {
	pin, ok := pinFor(name)
	if !ok {
		return fmt.Errorf("unknown catalog %q", name)
	}

	p.mu.Lock()
	ctx := p.appCtx
	if ctx == nil {
		ctx = reqCtx
	}
	if p.installed[name] {
		p.mu.Unlock()
		return nil // already installed: no-op
	}
	if p.downloading != "" {
		busy := p.downloading
		p.mu.Unlock()
		return fmt.Errorf("a catalog download is already in progress (%s)", busy)
	}
	p.downloading = name
	p.mu.Unlock()

	// Resume the (possibly yielded) solve worker once everything below is torn down —
	// registered first so it runs LAST, after the gate is released and the flags are
	// cleared, so the woken worker sees a free gate and DownloadPending()==false.
	defer p.wake()
	defer func() {
		p.downloadWaiting.Store(false)
		p.mu.Lock()
		p.downloading = ""
		p.mu.Unlock()
	}()

	// Mark the download pending and show it immediately, BEFORE waiting for the gate,
	// so the UI reflects a queued download (row + "waiting for the current solve to
	// finish…" indicator) instead of showing nothing until bytes flow. The solve
	// worker yields the backlog while this is set, so the wait is at most the current
	// in-flight solve, not the whole backlog.
	p.downloadWaiting.Store(true)
	p.setQueued(name, pin.Size)

	// Acquire the gate: the current solve releases it (the worker has yielded), then
	// hold it for the whole download so no solve runs while the catalog dir is swapped.
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.gate }()

	p.setDownloading(name, 0, pin.Size)
	if err := p.downloadCatalogResumable(ctx, pin); err != nil {
		p.publishReadyOrProvisioning(fmt.Sprintf("download of %s failed: %v", name, err))
		return err
	}

	p.mu.Lock()
	p.installed[name] = true
	p.mu.Unlock()
	p.log.Info("catalog installed", "catalog", name)
	p.publishReadyOrProvisioning("")

	// Event-driven, no-restart re-queue: the frames rejected below this catalog's
	// floor are now solvable. Flip them → 'none'; the deferred wake resumes the worker.
	if n, err := p.st.RequeueFloorUnsolvable(ctx, pin.Floor); err != nil {
		p.log.Warn("requeue below-floor unsolvable failed", "error", err)
	} else if n > 0 {
		p.log.Info("re-queued below-floor frames after catalog install", "catalog", name, "count", n)
	}
	return nil
}

// downloadCatalogResumable streams the catalog zip to <name>.part with an HTTP
// Range from the partial's current size, verifies it, then unzips atomically into
// place. Any checksum/size failure deletes the .part (fresh or resumed — a retained
// corrupt partial would fail forever).
func (p *Provisioner) downloadCatalogResumable(ctx context.Context, pin catalogPin) error {
	partPath := p.catalogDir(pin.Name) + ".part"
	if err := os.MkdirAll(p.astapDir, 0o755); err != nil {
		return err
	}

	// Free-space precheck: the peak holds the .part plus the unzipped dir at once, so
	// reserve twice the pinned zip size (a conservative proxy for zip+unzipped).
	if ok, err := p.enoughSpace(p.astapDir, pin.Size*2); err != nil {
		p.log.Warn("free-space precheck failed; proceeding", "error", err)
	} else if !ok {
		return fmt.Errorf("not enough free space to download %s (needs ~%d MB)", pin.Name, pin.Size*2/(1024*1024))
	}

	var start int64
	if fi, err := os.Stat(partPath); err == nil {
		start = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.URL, nil)
	if err != nil {
		return err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var flag int
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206: server honored Range → append
		flag = os.O_APPEND | os.O_WRONLY
	case http.StatusOK: // 200: server ignored Range → restart from 0
		start = 0
		flag = os.O_TRUNC | os.O_WRONLY | os.O_CREATE
	case http.StatusRequestedRangeNotSatisfiable: // 416: .part is already the whole file
		return p.finishCatalog(pin, partPath)
	default:
		return fmt.Errorf("unexpected HTTP %d downloading %s", resp.StatusCode, pin.Name)
	}

	f, err := os.OpenFile(partPath, flag, 0o644)
	if err != nil {
		return err
	}
	got := start
	if err := p.streamWithProgress(f, resp.Body, pin, &got); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return p.finishCatalog(pin, partPath)
}

// streamWithProgress copies src into f, publishing throttled download progress
// (~1% or 500 ms, whichever comes first) so an ~870 MB download doesn't churn the
// hub mutex per chunk.
func (p *Provisioner) streamWithProgress(f *os.File, src io.Reader, pin catalogPin, got *int64) error {
	buf := make([]byte, 512*1024)
	lastPub := time.Now()
	lastPct := -1
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			*got += int64(n)
			pct := int(*got * 100 / max64(pin.Size, 1))
			if pct != lastPct && time.Since(lastPub) >= 500*time.Millisecond {
				p.setDownloading(pin.Name, *got, pin.Size)
				lastPub, lastPct = time.Now(), pct
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// finishCatalog verifies the completed .part against the pin, then unzips it into a
// temp dir and atomically renames that into place so a crash mid-unzip can't leave a
// partial catalog CatalogFor would treat as complete.
func (p *Provisioner) finishCatalog(pin catalogPin, partPath string) error {
	if err := verifyFile(partPath, pin.Size, pin.SHA256); err != nil {
		_ = os.Remove(partPath) // a retained corrupt/short .part would fail forever
		return err
	}
	tmpDir := p.catalogDir(pin.Name) + ".tmp"
	_ = os.RemoveAll(tmpDir)
	if err := unzipFlat(partPath, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("unzip %s: %w", pin.Name, err)
	}
	finalDir := p.catalogDir(pin.Name)
	_ = os.RemoveAll(finalDir)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return fmt.Errorf("install %s: %w", pin.Name, err)
	}
	_ = os.Remove(partPath)
	return nil
}

// refreshInstalled rescans the disk for installed catalogs (a dir with ≥1 .1476
// tile) and replaces the in-memory set.
func (p *Provisioner) refreshInstalled() {
	found := map[string]bool{}
	for _, c := range catalogPins {
		if catalogInstalled(p.catalogDir(c.Name)) {
			found[c.Name] = true
		}
	}
	p.mu.Lock()
	p.installed = found
	p.mu.Unlock()
}

// wake sends a non-blocking nudge (frame id 0 is a harmless "re-sweep now" signal;
// the worker re-drains from cursor 0 regardless of the id).
func (p *Provisioner) wake() {
	select {
	case p.nudge <- 0:
	default:
	}
}

// enoughSpace reports whether dir's volume has at least need bytes free.
func (p *Provisioner) enoughSpace(dir string, need int64) (bool, error) {
	free, _, err := p.lfs.FreeSpace(dir)
	if err != nil {
		return false, err
	}
	return free >= need, nil
}

// --- status publishing ---------------------------------------------------------

func (p *Provisioner) setStatus(state, detail string) {
	p.mu.Lock()
	p.state, p.detail = state, detail
	p.mu.Unlock()
	p.publish("", 0, 0)
}

func (p *Provisioner) setDownloading(name string, got, total int64) {
	p.mu.Lock()
	p.state, p.detail = "provisioning", "downloading the "+name+" star catalog"
	p.mu.Unlock()
	p.publish(name, got, total)
}

// setQueued publishes the pre-download "queued" state: the catalog is chosen
// (Downloading=name, 0 bytes) but the gate is not yet held. got=0 lets the UI show
// "Queued…" rather than "Downloading… 0%".
func (p *Provisioner) setQueued(name string, total int64) {
	p.mu.Lock()
	p.state, p.detail = "provisioning", "waiting for the current solve to finish before downloading "+name
	p.mu.Unlock()
	p.publish(name, 0, total)
}

// publishReadyOrProvisioning refreshes readiness and publishes ready (binary +
// ≥1 catalog) or provisioning with a detail (e.g. a download error).
func (p *Provisioner) publishReadyOrProvisioning(detail string) {
	state := "provisioning"
	if p.Ready() {
		state, detail = "ready", ""
	}
	p.setStatus(state, detail)
}

// publish snapshots the provisioning state into the hub. Catalogs are listed
// installed-or-not (with pinned sizes) so the Settings list can render every row.
func (p *Provisioner) publish(downloading string, got, total int64) {
	p.mu.Lock()
	cats := make([]model.CatalogState, 0, len(catalogPins))
	for _, c := range catalogPins {
		cats = append(cats, model.CatalogState{Name: c.Name, Installed: p.installed[c.Name], SizeBytes: c.Size})
	}
	st := model.ProvisionStatus{
		State:       p.state,
		Detail:      p.detail,
		Catalogs:    cats,
		Downloading: downloading,
		GotBytes:    got,
		TotalBytes:  total,
	}
	p.mu.Unlock()
	if p.hub != nil {
		p.hub.SetSolverStatus(st)
	}
}

// --- pure helpers ---------------------------------------------------------------

func pinFor(name string) (catalogPin, bool) {
	for _, c := range catalogPins {
		if c.Name == name {
			return c, true
		}
	}
	return catalogPin{}, false
}

// KnownCatalog reports whether name is a catalog the download endpoint accepts.
func KnownCatalog(name string) bool {
	_, ok := pinFor(name)
	return ok
}

// resolveBinary locates astap_cli: alongside our own executable (the bundled copy
// the installer/Docker ship), else on PATH. There is no automatic binary download —
// no reliable upstream single-binary URL exists, so the binary is delivered by
// bundling and this resolver finds it.
func resolveBinary(dataDir string) string {
	name := "astap_cli"
	if runtime.GOOS == "windows" {
		name = "astap_cli.exe"
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if fileExists(cand) {
			return cand
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// catalogInstalled reports whether dir holds at least one .1476 catalog tile.
func catalogInstalled(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".1476") {
			return true
		}
	}
	return false
}

// verifyFile checks a downloaded file's size (always) and SHA-256 (when a checksum
// is pinned). A size or checksum mismatch is a corrupt/short download.
func verifyFile(path string, wantSize int64, wantSHA string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if wantSize > 0 && fi.Size() != wantSize {
		return fmt.Errorf("size mismatch: got %d, want %d", fi.Size(), wantSize)
	}
	if wantSHA == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantSHA)
	}
	return nil
}

// unzipFlat extracts every file entry of the zip at src into dstDir, flattening any
// path to its base name (the catalog zips are flat lists of .1476 tiles). It guards
// against a Zip-Slip path by using only the base name, and refuses any base name
// that is still not a plain local file name (on Windows, a reserved device name).
func unzipFlat(src, dstDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(f.Name)
		if name == "." || !filepath.IsLocal(name) {
			continue
		}
		if err := extractOne(f, filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func extractOne(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ensure the concrete provisioner satisfies the worker's seam.
var _ SolveGate = (*Provisioner)(nil)
