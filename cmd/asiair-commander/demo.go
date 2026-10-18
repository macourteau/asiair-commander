//go:build demo

// Demo mode (`go build -tags demo`) runs the whole server against in-memory fakes
// instead of real ASIAIR devices, so every surface — especially the live transfer
// feed — can be exercised and screenshotted end-to-end with no hardware. It stubs
// the device at the same seams the tests use (the newDevice factory, the rig
// Resolver, and the header Reader), then a generator goroutine drips new frames
// onto the fake devices so they flow pending → transferring → filed → deleted for
// real. Reads are throttled so the progress bar is visible rather than instant.
//
// The frames carry real-looking sky: tools/starfield renders each rig's targets
// once from cached Gaia DR3 cones, and every generated frame is that render behind
// its own ASIAIR-style header, so previews, History thumbnails and the plate solve
// all work. When a render cannot be had (no network on first run, the archive
// down), that rig falls back loudly to random bytes under a synthetic header.
//
// This file is compiled ONLY under `-tags demo`; the production binary never
// contains it (see demo_off.go for the no-op seam).
package main

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/demofiles"
	"github.com/macourteau/asiair-commander/internal/demoscenario"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/tools/starfield"
)

const (
	demoShare      = "EMMC Images"
	demoRoot       = "Autorun"
	demoFrameBytes = 3 << 20 // a fallback frame: 3 MiB of random bytes
	// The throttle's read granularity: a rendered 18 MB frame streams in about a
	// dozen paced reads, a few seconds, so the progress bar is visible.
	demoReadChunk = 1536 << 10
	demoReadDelay = 380 * time.Millisecond
	// demoSkyWait bounds how long the generator holds its first frame for the sky
	// renders; a first run fetches each field's catalog cone, seconds apiece.
	demoSkyWait = 3 * time.Minute
	// A rendered frame arrives as a real camera's would: one exposure plus this
	// much overhead (download, dither) after the last, so DATE-OBS gaps match the
	// cadence and the rig card's next-frame prediction counts down rather than
	// reading overdue.
	demoSkyOverhead = 8 * time.Second
	// demoSkyBacklog caps the rendered frames a rig's fake holds at once, should
	// the throttled pipeline fall behind the cadence, so the in-memory device
	// cannot grow without bound.
	demoSkyBacklog = 3
	demoInterval   = 7 * time.Second // one new frame per rig every ~7s
	// The share's statfs: an ASIAIR's 32 GB eMMC with room to spare, so the
	// dashboard shows a real figure and the File browser's upload plan warns
	// about free space only for a file that would not fit.
	demoShareFree  = 21 << 30
	demoShareTotal = 29 << 30
)

// installDemo swaps in the fakes and starts the generator. It keys one fake device
// per rig (by the address the resolver hands out, which is the rig's GUID) so
// multiple rigs never share a device.
func installDemo(
	ctx context.Context,
	_ deviceFactory,
	_ rig.Resolver,
	_ header.Reader,
	cfg *atomic.Pointer[model.Config],
	_ localfs.LocalFS,
	logger *slog.Logger,
) (deviceFactory, rig.Resolver, header.Reader) {
	reg := &demoRegistry{fakes: map[string]*device.Fake{}, skyReady: make(chan struct{})}
	logger.Info("DEMO MODE: real ASIAIR devices are stubbed by in-memory fakes")
	go reg.prepareSky(ctx, logger)

	factory := func(ep netsite.Endpoint, _, _ string, _ time.Duration) device.Device {
		key, delay := demoDeviceAt(ep)
		return slowDevice{Device: reg.fakeFor(key), delay: delay}
	}
	reg.preload()
	go reg.generate(ctx, cfg, logger)
	return factory, demoResolver{}, demoHeader{}
}

// demoRegistry hands out one persistent fake per address, so a worker sees the same
// device (and its accumulating files) across cycles.
type demoRegistry struct {
	mu    sync.Mutex
	fakes map[string]*device.Fake
	seq   atomic.Int64

	// sky holds each target's render, written once by prepareSky before it closes
	// skyReady and only read after.
	sky      map[string]starfield.Frame
	skyReady chan struct{}
	skySeq   map[string]int       // per-target frame number; guarded by mu
	skyTurn  map[string]int       // per-rig position in its target rotation; guarded by mu
	skyPaths map[string][]string  // per-rig rendered frames possibly still on the fake; guarded by mu
	skyNext  map[string]time.Time // per-rig time the next exposure ends; guarded by mu
}

// initSky makes the per-rig and per-target bookkeeping, before any is used.
func (r *demoRegistry) initSky() {
	r.sky = map[string]starfield.Frame{}
	r.skySeq, r.skyTurn = map[string]int{}, map[string]int{}
	r.skyPaths, r.skyNext = map[string][]string{}, map[string]time.Time{}
}

// prepareSky renders every target the demo rigs shoot, from the starfield cache
// when present. A target that fails is logged and left out; a rig with no
// rendered target emits fallback frames.
func (r *demoRegistry) prepareSky(ctx context.Context, logger *slog.Logger) {
	defer close(r.skyReady)
	r.initSky()
	dir, err := starfield.DefaultCacheDir()
	if err != nil {
		logger.Warn("DEMO: no cache dir for the starfield renders; frames carry random bytes", "error", err)
		return
	}
	logger.Info("DEMO: preparing starfield frames (a first run fetches Gaia DR3 cones)", "cache", dir)
	sky := &starfield.Sky{Dir: dir, Log: logger}
	for _, t := range starfield.Targets {
		f, err := sky.Render(ctx, t)
		if err != nil {
			logger.Warn("DEMO: starfield render failed; this target's frames carry random bytes", "target", t.Name, "error", err)
			continue
		}
		r.sky[t.Name] = f
	}
	logger.Info("DEMO: starfield frames ready", "targets", len(r.sky), "of", len(starfield.Targets))
}

// skyTargets are the rendered targets rig guid shoots: the FRA300 the wide
// clusters, the Newtonian's long focal length the globular. A remote scenario's
// rigs (tools/seedremote) keep the small random-byte frames its timings assume.
func (r *demoRegistry) skyTargets(guid string) []starfield.Frame {
	var want starfield.Optics
	switch guid {
	case demofiles.RigFRA:
		want = starfield.FRA300
	case demofiles.RigNewt:
		want = starfield.N200
	default:
		return nil
	}
	var out []starfield.Frame
	for _, t := range starfield.Targets {
		if f, ok := r.sky[t.Name]; ok && t.Optics == want {
			out = append(out, f)
		}
	}
	return out
}

func (r *demoRegistry) fakeFor(addr string) *device.Fake {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.fakes[addr]
	if !ok {
		f = device.NewFake()
		f.AddShare(demoShare)
		f.SetStatfs(demoShare, demoShareFree, demoShareTotal)
		stageDeviceFiles(f, addr)
		r.fakes[addr] = f
	}
	return f
}

// stageDeviceFiles puts the device file browser's staged files on the fake for
// rig guid (see internal/demofiles). A remote scenario stages nothing: its rigs
// have no held row for the Autorun frame, so it would be harvested as a frame
// that scenario never preloaded.
func stageDeviceFiles(f *device.Fake, guid string) {
	if demoScenario != nil {
		return
	}
	for _, sf := range demofiles.Files {
		if sf.Rig != "" && sf.Rig != guid {
			continue
		}
		f.AddFile(demofiles.Share, sf.Path, sf.Content(), sf.Mtime)
	}
}

// generate drips a fresh light frame onto each configured rig's fake on a timer, so
// the pipeline continuously produces pending → transferring → filed → deleted rows.
func (r *demoRegistry) generate(ctx context.Context, cfg *atomic.Pointer[model.Config], logger *slog.Logger) {
	targets := []string{"NGC 7000", "M31", "IC 1805", "M42", "Rosette", "M27"}
	filters := []string{"Ha", "OIII", "SII", "L", "R", "G"}
	// Hold the first frame until the sky renders are ready, so every frame the feed
	// shows carries them; prepareSky closes skyReady on failure too.
	select {
	case <-r.skyReady:
	case <-ctx.Done():
		return
	case <-time.After(demoSkyWait):
		logger.Warn("DEMO: starfield frames not ready; generating without them", "waited", demoSkyWait)
	}
	t := time.NewTicker(demoInterval)
	defer t.Stop()
	// One frame straight away so the feed is populated on first load.
	r.emit(cfg, targets, filters)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.emit(cfg, targets, filters)
		}
	}
}

func (r *demoRegistry) emit(cfg *atomic.Pointer[model.Config], targets, filters []string) {
	c := cfg.Load()
	for _, rp := range c.Rigs {
		if !rp.Managed || rp.GUID == "" {
			continue
		}
		if !r.addSkyFrame(rp.GUID) {
			r.addFrame(rp.GUID, targets, filters)
		}
	}
}

// addSkyFrame puts the next rendered frame of rig guid's targets on its fake,
// named and headed as an ASIAIR Autorun light that finished exposing just now,
// once the rig's previous exposure and its overhead have elapsed and unless the
// rig already holds demoSkyBacklog of them. It reports false when the rig has no
// rendered target or the renders are not ready, so the caller falls back to a
// random-byte frame.
func (r *demoRegistry) addSkyFrame(guid string) bool {
	select {
	case <-r.skyReady:
	default:
		return false
	}
	frames := r.skyTargets(guid)
	if len(frames) == 0 {
		return false
	}
	end := time.Now().UTC()
	r.mu.Lock()
	due := !end.Before(r.skyNext[guid])
	r.mu.Unlock()
	fake := r.fakeFor(guid)
	if !due || r.skyBacklog(guid, fake) >= demoSkyBacklog {
		return true
	}
	r.mu.Lock()
	f := frames[r.skyTurn[guid]%len(frames)]
	exposure := time.Duration(f.Target.Exposure * float64(time.Second))
	r.skyTurn[guid]++
	r.skySeq[f.Target.Name]++
	n := r.skySeq[f.Target.Name]
	r.skyNext[guid] = end.Add(exposure + demoSkyOverhead)
	r.mu.Unlock()
	e := starfield.Exposure{
		Start:   end.Add(-exposure),
		SetTemp: -10, CCDTemp: -10, Seq: n,
	}
	path := demoRoot + "/Light/" + f.Target.Name + "/" + starfield.FrameName(f.Target, e)
	// The device stamps the file when the exposure ends. Unlike the fallback's
	// backdated mtime, a current one keeps the rig card's next-frame prediction
	// honest; the live demo config's min_file_age is 0.
	fake.AddFile(demoShare, path, f.FITS(e), end)
	r.mu.Lock()
	r.skyPaths[guid] = append(r.skyPaths[guid], path)
	r.mu.Unlock()
	return true
}

// skyBacklog counts the rendered frames still on rig guid's fake, forgetting the
// ones the delete pass has removed. A fake that cannot be read counts as empty.
func (r *demoRegistry) skyBacklog(guid string, fake *device.Fake) int {
	sess, err := fake.Open(context.Background())
	if err != nil {
		return 0
	}
	defer sess.Close()
	sh, err := sess.Mount(demoShare)
	if err != nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.skyPaths[guid][:0]
	for _, p := range r.skyPaths[guid] {
		if _, err := sh.Stat(p); err == nil {
			kept = append(kept, p)
		}
	}
	r.skyPaths[guid] = kept
	return len(kept)
}

// preload puts the frames the remote scenario asks for on their devices before
// the first cycle lists them.
func (r *demoRegistry) preload() {
	if demoScenario == nil {
		return
	}
	targets, filters := []string{"M33"}, []string{"L"}
	for _, rg := range demoScenario.Rigs {
		for range rg.Preload {
			r.addFrame(rg.GUID, targets, filters)
		}
	}
}

func (r *demoRegistry) addFrame(guid string, targets, filters []string) {
	n := r.seq.Add(1)
	tgt := targets[int(n)%len(targets)]
	flt := filters[int(n)%len(filters)]
	path := demoRoot + "/Light/" + tgt + "/Light_" + flt + "_" + pad(n) + ".fit"
	data := make([]byte, demoFrameBytes)
	_, _ = rand.Read(data[:1024]) // vary the head so each file hashes distinctly
	// mtime a couple minutes back so it clears min_file_age immediately.
	r.fakeFor(guid).AddFile(demoShare, path, data, time.Now().Add(-2*time.Minute))
}

func pad(n int64) string {
	s := "0000" + itoa(n)
	return s[len(s)-4:]
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// demoResolver confirms every rig against its own fake. A local rig's address IS
// its GUID, on the local site, so ProbeGUID echoes it back; a rig the remote
// scenario places behind a remote is found at its scenario address on that
// remote's site, and ProbeGUID maps that endpoint back to its GUID. Identity
// always confirms either way.
type demoResolver struct{}

func (demoResolver) Resolve(guid, _ string) (netsite.Endpoint, bool) {
	if r := demoRemoteRig(guid); r != nil {
		return netsite.Endpoint{Site: netsite.ID(r.Site), IP: r.IP}, true
	}
	return netsite.LocalEndpoint(guid), guid != ""
}
func (demoResolver) ProbeGUID(_ context.Context, ep netsite.Endpoint) (string, string, bool) {
	key, _ := demoDeviceAt(ep)
	return key, "", key != ""
}

// demoRemoteRig is the scenario's rig with this GUID, nil for a local one.
func demoRemoteRig(guid string) *demoscenario.Rig {
	if demoScenario == nil {
		return nil
	}
	for i, r := range demoScenario.Rigs {
		if r.GUID == guid {
			return &demoScenario.Rigs[i]
		}
	}
	return nil
}

// demoDeviceAt names the fake at an endpoint, by its GUID, and the read delay
// its throttle uses.
func demoDeviceAt(ep netsite.Endpoint) (string, time.Duration) {
	if ep.Site != netsite.Local && demoScenario != nil {
		for _, r := range demoScenario.Rigs {
			if netsite.ID(r.Site) == ep.Site && r.IP == ep.IP {
				delay := demoReadDelay
				if r.ReadDelayMS > 0 {
					delay = time.Duration(r.ReadDelayMS) * time.Millisecond
				}
				return r.GUID, delay
			}
		}
	}
	return ep.IP, demoReadDelay
}

// demoHeader reads a rendered frame's own header. A fallback frame is random bytes
// with no header, so for it a synthetic Light header is made up, taking the target
// and filter from the file path so generated frames route to distinct folders;
// classification reads only this header, and the byte-verify/delete paths are
// content-agnostic.
type demoHeader struct{}

func (demoHeader) ReadHeader(path string) (model.Header, error) {
	if h, err := header.New().ReadHeader(path); err == nil {
		return h, nil
	}
	tgt, flt := "M31", "HA"
	// .../Light/<target>/Light_<filter>_NNNN.fit
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "Light" && i+1 < len(parts) {
			tgt = parts[i+1]
		}
	}
	if base := parts[len(parts)-1]; strings.HasPrefix(base, "Light_") {
		if seg := strings.SplitN(strings.TrimPrefix(base, "Light_"), "_", 2); len(seg) == 2 {
			flt = seg[0]
		}
	}
	return model.NewHeader([]model.Card{
		{Key: "IMAGETYP", Value: "Light"},
		{Key: "INSTRUME", Value: "ZWO ASI2600MM Pro"},
		{Key: "EXPTIME", Value: float64(300)},
		{Key: "SET-TEMP", Value: float64(-10)},
		{Key: "GAIN", Value: int64(100)},
		{Key: "OFFSET", Value: int64(50)},
		{Key: "XBINNING", Value: int64(1)},
		{Key: "YBINNING", Value: int64(1)},
		{Key: "FILTER", Value: flt},
		{Key: "OBJECT", Value: tgt},
		{Key: "DATE-OBS", Value: time.Now().UTC().Format("2006-01-02T15:04:05.000000")},
	}), nil
}

// --- throttled device wrappers -------------------------------------------------
// Each embeds the underlying interface so only the read path is overridden; every
// other method delegates for free. The throttle makes a copy take a few seconds so
// the live progress bar is visible instead of completing instantly.

type slowDevice struct {
	device.Device
	delay time.Duration
}

func (d slowDevice) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	if err != nil {
		return nil, err
	}
	return slowSession{Session: s, delay: d.delay}, nil
}

type slowSession struct {
	device.Session
	delay time.Duration
}

func (s slowSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return slowShare{Share: sh, delay: s.delay}, nil
}

type slowShare struct {
	device.Share
	delay time.Duration
}

func (s slowShare) OpenReader(path string) (io.ReadCloser, error) {
	rc, err := s.Share.OpenReader(path)
	if err != nil {
		return nil, err
	}
	return &slowReader{rc: rc, delay: s.delay}, nil
}

// slowReader caps each read and sleeps first, so io.CopyBuffer streams in paced
// chunks (it never sees a WriteTo shortcut) and the progress sampler fires.
type slowReader struct {
	rc    io.ReadCloser
	delay time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	if len(p) > demoReadChunk {
		p = p[:demoReadChunk]
	}
	return r.rc.Read(p)
}

func (r *slowReader) Close() error { return r.rc.Close() }
