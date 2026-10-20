//go:build demo

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/macourteau/asiair-commander/internal/demofiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/tools/starfield"
)

// skyRegistry is a registry whose sky is ready, with every preset "rendered" as a
// tiny stand-in frame so the test needs neither the network nor the cache.
func skyRegistry(t *testing.T) *demoRegistry {
	t.Helper()
	r := newDemoRegistry()
	r.initSky()
	for _, tg := range starfield.Targets {
		data := starfield.DataUnit(starfield.Image{Width: 4, Height: 4, Pix: make([]uint16, 16)})
		r.sky[tg.Name] = starfield.Frame{Target: tg, Data: data}
	}
	close(r.skyReady)
	return r
}

// skyFiles lists the rendered frames on rig guid's fake.
func skyFiles(t *testing.T, r *demoRegistry, guid string) []string {
	t.Helper()
	sess, err := r.fakeFor(guid).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sh, err := sess.Mount(demoShare)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := sh.ListDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := dir + "/" + e.Name
			if e.IsDir {
				walk(p)
			} else if strings.Contains(p, "_533MM_") {
				out = append(out, p)
			}
		}
	}
	walk(demoRoot + "/Light")
	return out
}

func TestSkyFramesRotateTargetsPerRigAndCapTheBacklog(t *testing.T) {
	r := skyRegistry(t)
	// The first frame is due at once; the next waits out an exposure and overhead.
	r.addSkyFrame(demofiles.RigFRA)
	r.addSkyFrame(demofiles.RigFRA)
	if got := skyFiles(t, r, demofiles.RigFRA); len(got) != 1 {
		t.Fatalf("a second frame arrived inside the first's exposure: %v", got)
	}
	for range demoBacklog + 2 {
		clear(r.skyNext) // as if each exposure had ended
		if !r.addSkyFrame(demofiles.RigFRA) || !r.addSkyFrame(demofiles.RigNewt) {
			t.Fatal("addSkyFrame reported no rendered target for a demo rig")
		}
	}
	fra := skyFiles(t, r, demofiles.RigFRA)
	if len(fra) != demoBacklog {
		t.Fatalf("FRA300 fake holds %d rendered frames, want the cap of %d: %v", len(fra), demoBacklog, fra)
	}
	// The FRA300's frames are its first three targets, not every other one.
	for _, tg := range starfield.Targets[:demoBacklog] {
		want := "/Light/" + tg.Name + "/Light_" + tg.Name + "_60.0s_Bin1_533MM_gain100_"
		found := false
		for _, p := range fra {
			found = found || strings.Contains(p, want)
		}
		if !found {
			t.Errorf("no frame under %s in %v", want, fra)
		}
	}
	for _, p := range skyFiles(t, r, demofiles.RigNewt) {
		if !strings.Contains(p, "/Light/M13/Light_M13_30.0s_") {
			t.Errorf("the Newtonian shot %s, want only M13", p)
		}
	}

	// Once the delete pass removes a frame, the next tick replaces it.
	r.fakeFor(demofiles.RigFRA).RemoveFile(demoShare, fra[0])
	r.addSkyFrame(demofiles.RigFRA)
	if got := skyFiles(t, r, demofiles.RigFRA); len(got) != demoBacklog {
		t.Errorf("after a delete the FRA300 holds %d frames, want %d", len(got), demoBacklog)
	}
}

func TestSkyFrameFallsBackWhenNothingRendered(t *testing.T) {
	r := skyRegistry(t)
	r.sky = map[string]starfield.Frame{}
	if r.addSkyFrame(demofiles.RigFRA) {
		t.Error("addSkyFrame claimed a frame with no render available")
	}
	// A remote scenario's rig keeps the random-byte frames.
	if skyRegistry(t).addSkyFrame("hl0redcat01") {
		t.Error("a remote scenario rig was given a rendered frame")
	}
}

// A rendered frame classifies from its own header; a random-byte fallback frame
// still gets the synthetic one.
func TestDemoHeaderPrefersTheFramesOwnHeader(t *testing.T) {
	dir := t.TempDir()
	tg, _ := starfield.TargetByName("M13")
	f := starfield.Frame{Target: tg, Data: starfield.DataUnit(starfield.Image{Width: 4, Height: 4, Pix: make([]uint16, 16)})}
	real := filepath.Join(dir, "real.fit")
	if err := os.WriteFile(real, f.FITS(starfield.ReferenceExposure), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := demoHeader{}.ReadHeader(real)
	if err != nil {
		t.Fatal(err)
	}
	if obj, _ := h.Str("OBJECT"); obj != "M13" {
		t.Errorf("OBJECT = %q, want the frame's own M13", obj)
	}
	if _, ok := h.Float("FOCALLEN"); !ok {
		t.Error("the frame's own header lost FOCALLEN")
	}

	fallback := filepath.Join(dir, "Light", "NGC 7000", "Light_OIII_0001.fit")
	if err := os.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err = demoHeader{}.ReadHeader(fallback)
	if err != nil {
		t.Fatal(err)
	}
	if obj, _ := h.Str("OBJECT"); obj != "NGC 7000" {
		t.Errorf("fallback OBJECT = %q, want the folder's NGC 7000", obj)
	}
}

// lightFiles lists every file under the watched Light folder of dev.
func lightFiles(t *testing.T, dev device.Device) []string {
	t.Helper()
	sess, err := dev.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sh, err := sess.Mount(demoShare)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := sh.ListDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := dir + "/" + e.Name
			if e.IsDir {
				walk(p)
			} else {
				out = append(out, p)
			}
		}
	}
	walk(demoRoot + "/Light")
	return out
}

// pull reads path from dev to its end, as a transfer does.
func pull(t *testing.T, dev device.Device, path string) {
	t.Helper()
	sess, err := dev.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sh, err := sess.Mount(demoShare)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := sh.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatal(err)
	}
}

// generatedFrames runs the generator for rounds ticks against one random-byte rig,
// pulling every new frame after each tick when pulling is set, and returns the most
// generated frames the device held at once and how many distinct frames appeared.
func generatedFrames(t *testing.T, rp model.RigProfile, rounds int, pulling bool) (held, seen int) {
	t.Helper()
	r := newDemoRegistry()
	r.initSky()
	close(r.skyReady) // no renders: every frame is a random-byte fallback
	var cfg atomic.Pointer[model.Config]
	cfg.Store(&model.Config{Rigs: []model.RigProfile{rp}})
	dev := r.throttled(rp.GUID, 0)
	staged := map[string]bool{}
	for _, p := range lightFiles(t, dev) {
		staged[p] = true
	}
	seenAt := map[string]bool{}
	for range rounds {
		r.emit(&cfg, []string{"M33"}, []string{"L"})
		n := 0
		for _, p := range lightFiles(t, dev) {
			if staged[p] {
				continue
			}
			n++
			if !seenAt[p] {
				seenAt[p] = true
				if pulling {
					pull(t, dev, p)
				}
			}
		}
		held = max(held, n)
	}
	return held, len(seenAt)
}

// A rig that keeps its originals never deletes a frame, so the generator must not
// leave every frame it made on the in-memory device: the pulled ones go, as an
// observer clears an archived card, and new frames keep arriving.
func TestGeneratedFramesStayBoundedOnARigThatKeepsOriginals(t *testing.T) {
	const rounds = 60
	held, seen := generatedFrames(t, model.RigProfile{GUID: "hl0redcat01", Managed: true, KeepOriginals: true}, rounds, true)
	if held > 2*demoBacklog {
		t.Errorf("the device held %d generated frames at once over %d ticks, want at most %d", held, rounds, 2*demoBacklog)
	}
	if seen < rounds/2 {
		t.Errorf("only %d frames appeared over %d ticks: the generator stalled", seen, rounds)
	}
}

// A pipeline that does not keep up (a slow or unreachable rig) must not let the
// generator pile frames onto the in-memory device.
func TestGeneratedFramesWaitForAPipelineThatFallsBehind(t *testing.T) {
	for _, keep := range []bool{false, true} {
		held, _ := generatedFrames(t, model.RigProfile{GUID: "hl0redcat01", Managed: true, KeepOriginals: keep}, 60, false)
		if held > demoBacklog {
			t.Errorf("keep_originals=%v: an unpulled device held %d generated frames, want at most %d", keep, held, demoBacklog)
		}
	}
}
