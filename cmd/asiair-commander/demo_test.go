//go:build demo

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/demofiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/tools/starfield"
)

// skyRegistry is a registry whose sky is ready, with every preset "rendered" as a
// tiny stand-in frame so the test needs neither the network nor the cache.
func skyRegistry(t *testing.T) *demoRegistry {
	t.Helper()
	r := &demoRegistry{fakes: map[string]*device.Fake{}, skyReady: make(chan struct{})}
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
	for range demoSkyBacklog + 2 {
		clear(r.skyNext) // as if each exposure had ended
		if !r.addSkyFrame(demofiles.RigFRA) || !r.addSkyFrame(demofiles.RigNewt) {
			t.Fatal("addSkyFrame reported no rendered target for a demo rig")
		}
	}
	fra := skyFiles(t, r, demofiles.RigFRA)
	if len(fra) != demoSkyBacklog {
		t.Fatalf("FRA300 fake holds %d rendered frames, want the cap of %d: %v", len(fra), demoSkyBacklog, fra)
	}
	// The FRA300's frames are its first three targets, not every other one.
	for _, tg := range starfield.Targets[:demoSkyBacklog] {
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
	if got := skyFiles(t, r, demofiles.RigFRA); len(got) != demoSkyBacklog {
		t.Errorf("after a delete the FRA300 holds %d frames, want %d", len(got), demoSkyBacklog)
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
