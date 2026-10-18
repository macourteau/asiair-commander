package platesolve

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
)

func TestParsePLTSOLVD(t *testing.T) {
	cases := []struct {
		name        string
		ini         string
		wantSolved  bool
		wantPresent bool
	}{
		{"solved", "PLTSOLVD=T\nCRVAL1= 3.1E2\n", true, true},
		{"failed", "CRPIX1= 1\nPLTSOLVD=F\n", false, true},
		{"spaces", "PLTSOLVD= T \n", true, true},
		{"absent", "CRVAL1= 1\nCRVAL2= 2\n", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			solved, ok := parsePLTSOLVD([]byte(c.ini))
			if solved != c.wantSolved || ok != c.wantPresent {
				t.Fatalf("parsePLTSOLVD=(%v,%v), want (%v,%v)", solved, ok, c.wantSolved, c.wantPresent)
			}
		})
	}
}

// TestWithNAXISOverlay pins the design features §3.3 quirk: a .wcs-style header (no
// NAXIS1/2) yields FOV 0, and injecting the source NAXIS1/2 makes FromHeader report
// a real FOV while leaving the center unchanged.
func TestWithNAXISOverlay(t *testing.T) {
	// A synthetic tangent-plane header with CRPIX at image center, no NAXIS1/2 — the
	// shape astap's .wcs has.
	cards := []model.Card{
		{Key: "CTYPE1", Value: "RA---TAN"},
		{Key: "CTYPE2", Value: "DEC--TAN"},
		{Key: "CRVAL1", Value: 313.6957},
		{Key: "CRVAL2", Value: 43.5807},
		{Key: "CRPIX1", Value: 3036.5},
		{Key: "CRPIX2", Value: 2021.5},
		{Key: "CD1_1", Value: -1.7278e-4},
		{Key: "CD1_2", Value: -1.1221e-3},
		{Key: "CD2_1", Value: 1.1221e-3},
		{Key: "CD2_2", Value: -1.7285e-4},
	}
	h := model.NewHeader(cards)

	bare := astrometry.FromHeader(h).Solution
	if bare == nil {
		t.Fatal("expected a solution from the bare header")
	}
	if bare.FOVWidth != 0 || bare.FOVHeight != 0 {
		t.Fatalf("bare header FOV = %vx%v, want 0 (no NAXIS)", bare.FOVWidth, bare.FOVHeight)
	}

	over := astrometry.FromHeader(withNAXIS(h, 6072, 4042)).Solution
	if over == nil {
		t.Fatal("expected a solution after the NAXIS overlay")
	}
	if over.FOVHeight <= 0 || over.FOVWidth <= 0 {
		t.Fatalf("overlaid FOV = %vx%v, want > 0", over.FOVWidth, over.FOVHeight)
	}
	// The center (CRVAL, CRPIX at image center) is unaffected by the overlay.
	if over.CenterRA != bare.CenterRA || over.CenterDec != bare.CenterDec {
		t.Fatalf("overlay shifted the center: %v/%v vs %v/%v", over.CenterRA, over.CenterDec, bare.CenterRA, bare.CenterDec)
	}
}

func TestSmallestCovering(t *testing.T) {
	cases := []struct {
		fov  float64
		want string
		ok   bool
	}{
		{5.0, "d05", true},
		{0.6, "d05", true},
		{0.5, "d20", true},
		{0.3, "d20", true},
		{0.25, "d50", true},
		{0.2, "d50", true},
		{0.1, "", false},
	}
	for _, c := range cases {
		got, ok := SmallestCovering(c.fov)
		if got != c.want || ok != c.ok {
			t.Errorf("SmallestCovering(%v)=(%q,%v), want (%q,%v)", c.fov, got, ok, c.want, c.ok)
		}
	}
}

func TestCatalogForAndDeepestFloor(t *testing.T) {
	// A provisioner with d05 and d50 installed (d20 not).
	p := &Provisioner{astapDir: "/x/astap", installed: map[string]bool{"d05": true, "d50": true}}

	if dir, ok := p.CatalogFor(5.0); !ok || dir != filepath.Join("/x/astap", "d05") {
		t.Errorf("CatalogFor(5.0)=(%q,%v), want d05", dir, ok)
	}
	// 0.5° is below d05's 0.6 floor; d20 not installed; d50 covers it.
	if dir, ok := p.CatalogFor(0.5); !ok || dir != filepath.Join("/x/astap", "d50") {
		t.Errorf("CatalogFor(0.5)=(%q,%v), want d50", dir, ok)
	}
	// 0.1° below every floor.
	if _, ok := p.CatalogFor(0.1); ok {
		t.Error("CatalogFor(0.1) should not be covered")
	}
	if floor, ok := p.DeepestInstalledFloor(); !ok || floor != 0.2 {
		t.Errorf("DeepestInstalledFloor=(%v,%v), want 0.2", floor, ok)
	}
}

func TestVerifyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	content := []byte("hello catalog")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	hexsum := hex.EncodeToString(sum[:])

	if err := verifyFile(path, int64(len(content)), hexsum); err != nil {
		t.Fatalf("verifyFile good: %v", err)
	}
	if err := verifyFile(path, int64(len(content))+1, hexsum); err == nil {
		t.Fatal("verifyFile should reject a size mismatch")
	}
	if err := verifyFile(path, int64(len(content)), "deadbeef"); err == nil {
		t.Fatal("verifyFile should reject a checksum mismatch")
	}
	if err := verifyFile(path, int64(len(content)), ""); err != nil {
		t.Fatalf("verifyFile size-only: %v", err)
	}
}

// makeCatalogZip returns an in-memory zip holding one flat .1476 tile (plus a
// subdir-pathed entry, to exercise the flatten/zip-slip guard).
func makeCatalogZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"d05_0101.1476", "nested/d05_0202.1476"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("tile:" + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnzipFlat(t *testing.T) {
	zipBytes := makeCatalogZip(t)
	src := filepath.Join(t.TempDir(), "cat.zip")
	if err := os.WriteFile(src, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := unzipFlat(src, dst); err != nil {
		t.Fatalf("unzipFlat: %v", err)
	}
	for _, base := range []string{"d05_0101.1476", "d05_0202.1476"} {
		if _, err := os.Stat(filepath.Join(dst, base)); err != nil {
			t.Errorf("expected flattened tile %s: %v", base, err)
		}
	}
	// The nested path must have been flattened, never written under a subdir.
	if _, err := os.Stat(filepath.Join(dst, "nested")); err == nil {
		t.Error("zip subdir was not flattened")
	}
	if !catalogInstalled(dst) {
		t.Error("catalogInstalled should see the extracted tiles")
	}
}

// TestUnzipFlatStaysInsideDestination feeds traversal-shaped entry names (relative
// escapes, an absolute path, a Windows-separator escape) and checks that nothing
// lands outside the destination directory.
func TestUnzipFlatStaysInsideDestination(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := []string{"../escape1.1476", "../../escape2.1476", "/abs/escape3.1476", `..\escape4.1476`, ".."}
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("tile")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	src := filepath.Join(t.TempDir(), "cat.zip")
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(parent, "out")
	if err := unzipFlat(src, dst); err != nil {
		t.Fatalf("unzipFlat: %v", err)
	}
	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "out" {
		t.Fatalf("entries escaped the destination: %v", ents)
	}
	for _, base := range []string{"escape1.1476", "escape2.1476", "escape3.1476"} {
		if _, err := os.Stat(filepath.Join(dst, base)); err != nil {
			t.Errorf("expected flattened tile %s: %v", base, err)
		}
	}
}

// TestDownloadResumable serves a catalog zip with Range support and verifies both a
// fresh download and a resume-from-partial path install the catalog, and that the
// resume actually sends a Range header rather than restarting.
func TestDownloadResumable(t *testing.T) {
	zipBytes := makeCatalogZip(t)
	sum := sha256.Sum256(zipBytes)
	pin := catalogPin{Name: "d05", Size: int64(len(zipBytes)), SHA256: hex.EncodeToString(sum[:]), Floor: 0.6}

	var sawRange bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			sawRange = true
		}
		// http.ServeContent honors Range and sets 206/200/416 appropriately.
		http.ServeContent(w, r, "cat.zip", time.Unix(0, 0), bytes.NewReader(zipBytes))
	}))
	defer srv.Close()
	pin.URL = srv.URL

	newProv := func() *Provisioner {
		return &Provisioner{
			astapDir:  filepath.Join(t.TempDir(), "astap"),
			lfs:       freeSpaceStub{},
			hub:       nil,
			client:    &http.Client{},
			installed: map[string]bool{},
		}
	}

	// Fresh download.
	p := newProv()
	if err := p.downloadCatalogResumable(context.Background(), pin); err != nil {
		t.Fatalf("fresh download: %v", err)
	}
	if !catalogInstalled(p.catalogDir("d05")) {
		t.Fatal("fresh download did not install the catalog")
	}

	// Resume: pre-seed a half-written .part, then download.
	p = newProv()
	if err := os.MkdirAll(p.astapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.catalogDir("d05")+".part", zipBytes[:len(zipBytes)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	sawRange = false
	if err := p.downloadCatalogResumable(context.Background(), pin); err != nil {
		t.Fatalf("resumed download: %v", err)
	}
	if !sawRange {
		t.Error("resume did not send a Range header")
	}
	if !catalogInstalled(p.catalogDir("d05")) {
		t.Fatal("resumed download did not install the catalog")
	}
}

// TestDownloadRejectsChecksumMismatch pins that a corrupt download is rejected and
// its .part deleted (a retained corrupt partial would fail forever).
func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	zipBytes := makeCatalogZip(t)
	pin := catalogPin{Name: "d05", Size: int64(len(zipBytes)), SHA256: "00" + hex.EncodeToString(sha256Sum(zipBytes)[1:]), Floor: 0.6}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "cat.zip", time.Unix(0, 0), bytes.NewReader(zipBytes))
	}))
	defer srv.Close()
	pin.URL = srv.URL

	p := &Provisioner{astapDir: filepath.Join(t.TempDir(), "astap"), lfs: freeSpaceStub{}, client: &http.Client{}, installed: map[string]bool{}}
	if err := p.downloadCatalogResumable(context.Background(), pin); err == nil {
		t.Fatal("expected a checksum-mismatch error")
	}
	if _, err := os.Stat(p.catalogDir("d05") + ".part"); !os.IsNotExist(err) {
		t.Error(".part was not deleted after a checksum mismatch")
	}
	if catalogInstalled(p.catalogDir("d05")) {
		t.Error("a corrupt download must not install the catalog")
	}
}

// TestGateMutualExclusion pins the solve/download gate: a held gate blocks a second
// acquire until released, and a cancelled ctx unblocks the acquire.
func TestGateMutualExclusion(t *testing.T) {
	p := &Provisioner{gate: make(chan struct{}, 1)}

	release, err := p.BeginSolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// A second acquire under a cancelled ctx returns the ctx error immediately.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.BeginSolve(cctx); err == nil {
		t.Fatal("BeginSolve on a held gate with a cancelled ctx should return an error")
	}

	// A second acquire blocks until the first releases.
	acquired := make(chan struct{})
	go func() {
		rel, err := p.BeginSolve(context.Background())
		if err == nil {
			rel()
		}
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second acquire returned while the gate was still held")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second acquire did not proceed after release")
	}
}

// TestDownloadQueuedBeforeGate pins that while a download waits for the gate
// (held by an in-flight solve), it immediately reports DownloadPending and publishes
// a queued status (downloading=name, 0 bytes) — so the UI shows the pending download
// rather than nothing.
func TestDownloadQueuedBeforeGate(t *testing.T) {
	h := hub.New(nil)
	nudge := make(chan int64, 4)
	appCtx, cancel := context.WithCancel(context.Background())
	p := &Provisioner{
		astapDir:  filepath.Join(t.TempDir(), "astap"),
		hub:       h,
		gate:      make(chan struct{}, 1),
		installed: map[string]bool{},
		nudge:     nudge,
		appCtx:    appCtx,
	}
	// Simulate an in-flight solve holding the gate.
	p.gate <- struct{}{}

	done := make(chan error, 1)
	go func() { done <- p.DownloadCatalog(context.Background(), "d20") }()

	// The download should report pending + publish a queued status before the gate.
	deadline := time.Now().Add(2 * time.Second)
	for !p.DownloadPending() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !p.DownloadPending() {
		t.Fatal("DownloadPending stayed false while the download was queued")
	}
	st := h.Snapshot().SolverStatus
	if st == nil || st.Downloading != "d20" || st.GotBytes != 0 {
		t.Fatalf("queued status not published: %+v", st)
	}

	// Cancel the app ctx so the gate-select returns without a real download.
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the ctx-cancelled download to return an error")
	}
	if p.DownloadPending() {
		t.Error("DownloadPending should be cleared after the download returns")
	}
	// The deferred wake nudged the worker to resume.
	select {
	case <-nudge:
	default:
		t.Error("expected a resume nudge after the download returned")
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// freeSpaceStub satisfies localfs.LocalFS with an embedded nil interface; only
// FreeSpace is used by the download precheck, and any other call would panic (a
// stray call is thus caught, not silently tolerated).
type freeSpaceStub struct{ localfs.LocalFS }

func (freeSpaceStub) FreeSpace(string) (int64, int64, error) { return 1 << 60, 1 << 60, nil }
