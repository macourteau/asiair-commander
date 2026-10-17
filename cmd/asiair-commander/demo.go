//go:build demo

// Demo mode (`go build -tags demo`) runs the whole server against in-memory fakes
// instead of real ASIAIR devices, so every surface — especially the live transfer
// feed — can be exercised and screenshotted end-to-end with no hardware. It stubs
// the device at the same seams the tests use (the newDevice factory, the rig
// Resolver, and the header Reader), then a generator goroutine drips new frames
// onto the fake devices so they flow pending → transferring → filed → deleted for
// real. Reads are throttled so the progress bar is visible rather than instant.
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
)

const (
	demoShare      = "EMMC Images"
	demoRoot       = "Autorun"
	demoFrameBytes = 3 << 20   // 3 MiB streamed through the throttle
	demoReadChunk  = 128 << 10 // read granularity for the throttle
	demoReadDelay  = 380 * time.Millisecond
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
	reg := &demoRegistry{fakes: map[string]*device.Fake{}}
	logger.Info("DEMO MODE: real ASIAIR devices are stubbed by in-memory fakes")

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
		f.AddFile(demofiles.Share, sf.Path, make([]byte, sf.Size), sf.Mtime)
	}
}

// generate drips a fresh light frame onto each configured rig's fake on a timer, so
// the pipeline continuously produces pending → transferring → filed → deleted rows.
func (r *demoRegistry) generate(ctx context.Context, cfg *atomic.Pointer[model.Config], logger *slog.Logger) {
	targets := []string{"NGC 7000", "M31", "IC 1805", "M42", "Rosette", "M27"}
	filters := []string{"Ha", "OIII", "SII", "L", "R", "G"}
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
		r.addFrame(rp.GUID, targets, filters)
	}
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

// demoHeader returns a synthetic Light header, taking the target and filter from
// the file path so generated frames route to distinct folders — the file bytes
// themselves are random, which is fine because classification reads only this
// header, and the byte-verify/delete paths are content-agnostic.
type demoHeader struct{}

func (demoHeader) ReadHeader(path string) (model.Header, error) {
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
