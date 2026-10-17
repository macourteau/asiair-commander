//go:build integration

// Package integration exercises the real smb1client device path
// (internal/device.SMBDevice) against a live Samba/NT1 container that mimics an
// ASIAIR's guest-authenticated removable-media share. The in-memory device Fake
// cannot validate the actual SMB1 negotiate, share enumeration, directory
// listing, byte-exact reads, or delete/marker round-trips — this test does.
//
// It is behind the `integration` build tag so `go test ./...` skips it. Stand
// the container up with test/integration/run.sh (or `make integration`), which
// exports AC_INTEGRATION_SMB_HOST; run directly it targets 127.0.0.1 by default
// but skips if that host has no reachable SMB server.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

const (
	shareName  = "EMMC Images"
	fixtureRel = "internal/header/testdata/asiair_light_2600mm_ha.fit"
	// The seeded frame and its sidecar, relative to the share root, matching
	// the Autorun/Light/<target>/ tree baked into the Samba image.
	frameDir = "Autorun/Light/NGC7000"
	// bigDir and bigDirCount mirror the large directory seeded by the Samba
	// Dockerfile — the fragmentation case (see TestLargeDirectoryListsCompletely).
	bigDir      = "Autorun/Light/BIGNIGHT"
	bigDirCount = 400
	frameName   = "Light_NGC7000_001.fit"
	previewRel  = "Autorun/Light/NGC7000/Light_NGC7000_001_thn.jpg"
	markerName  = ".asiair-commander-rig-id"
)

// host resolves the target: AC_INTEGRATION_SMB_HOST when run.sh set it,
// otherwise 127.0.0.1. Probe gates the run so a bare `go test -tags integration`
// with no container present skips rather than fails.
func host() string {
	if h := os.Getenv("AC_INTEGRATION_SMB_HOST"); h != "" {
		return h
	}
	return "127.0.0.1"
}

// repoRoot walks up from this test file to the module root so the fixture can be
// read regardless of the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// .../test/integration/integration_test.go -> module root is two dirs up.
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

func newDevice(t *testing.T) device.Device {
	t.Helper()
	h := host()
	dev := device.New(netsite.NewRegistry(nil), netsite.LocalEndpoint(h), "guest", "12345678", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !dev.Probe(ctx) {
		if os.Getenv("AC_INTEGRATION_SMB_HOST") == "" {
			t.Skipf("no SMB server reachable on %s:445; start it with test/integration/run.sh", h)
		}
		t.Fatalf("Probe(%s): container expected but SMB/445 unreachable", h)
	}
	return dev
}

// TestSMBDeviceAgainstRealContainer drives every Session/Share seam method
// against the live container in one session, mirroring a polling cycle.
func TestSMBDeviceAgainstRealContainer(t *testing.T) {
	dev := newDevice(t)
	ctx := context.Background()

	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	// ServerTime — captured at negotiate, must be a real clock value.
	st := sess.ServerTime()
	if st.Time.IsZero() {
		t.Errorf("ServerTime is zero: %+v", st)
	}
	if d := time.Since(st.Time); d < -24*time.Hour || d > 24*time.Hour {
		t.Errorf("ServerTime %v is more than a day from the test clock (skew %v)", st.Time, d)
	}

	// ListShares — the storage share is present; IPC$/print$ are filtered.
	shares, err := sess.ListShares()
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if !contains(shares, shareName) {
		t.Errorf("ListShares %v does not include %q", shares, shareName)
	}
	for _, s := range shares {
		if s == "IPC$" || s == "print$" || strings.HasSuffix(s, "$") {
			t.Errorf("ListShares leaked an administrative share: %q in %v", s, shares)
		}
	}

	// Mount the space-containing share name.
	share, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount(%q): %v", shareName, err)
	}
	if share.Name() != shareName {
		t.Errorf("Share.Name() = %q, want %q", share.Name(), shareName)
	}

	// ListDir("") on the ROOT (H2: "", not "."): must return entries, including
	// the Autorun dir and the FSCK junk file left at the volume root.
	root, err := share.ListDir("")
	if err != nil {
		t.Fatalf("ListDir(\"\"): %v", err)
	}
	if len(root) == 0 {
		t.Fatal("ListDir(\"\") returned no entries")
	}
	if !hasEntry(root, "Autorun", true) {
		t.Errorf("root listing %v missing the Autorun directory", names(root))
	}

	// Recursively locate the seeded .fit under Autorun/Light/NGC7000/.
	framePath := filepath.ToSlash(filepath.Join(frameDir, frameName))
	found := findFile(t, share, "", frameName)
	if found != framePath {
		t.Fatalf("recursive walk found %q, want %q", found, framePath)
	}

	// Read the local fixture for the size + byte-exact comparison.
	fixtureBytes, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(fixtureRel)))
	if err != nil {
		t.Fatalf("read local fixture: %v", err)
	}
	wantSHA := sha256.Sum256(fixtureBytes)

	// Stat — the size over SMB matches the local fixture.
	fi, err := share.Stat(framePath)
	if err != nil {
		t.Fatalf("Stat(%q): %v", framePath, err)
	}
	if fi.Size != int64(len(fixtureBytes)) {
		t.Errorf("Stat size = %d, want %d", fi.Size, len(fixtureBytes))
	}
	if fi.IsDir {
		t.Errorf("Stat(%q) reports a directory", framePath)
	}

	// OpenReader + io.Copy + sha256: proves the SMB read is byte-exact.
	rc, err := share.OpenReader(framePath)
	if err != nil {
		t.Fatalf("OpenReader(%q): %v", framePath, err)
	}
	h := sha256.New()
	n, err := io.Copy(h, rc)
	rc.Close()
	if err != nil {
		t.Fatalf("io.Copy from SMB: %v", err)
	}
	if n != int64(len(fixtureBytes)) {
		t.Errorf("read %d bytes over SMB, want %d", n, len(fixtureBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hex.EncodeToString(wantSHA[:]) {
		t.Errorf("SMB read sha256 %s != local fixture sha256 %s", got, hex.EncodeToString(wantSHA[:]))
	}

	// Statfs — the share reports positive free/total capacity.
	free, total, err := share.Statfs()
	if err != nil {
		t.Fatalf("Statfs: %v", err)
	}
	if free <= 0 || total <= 0 {
		t.Errorf("Statfs free=%d total=%d, want both > 0", free, total)
	}
	if free > total {
		t.Errorf("Statfs free=%d exceeds total=%d", free, total)
	}

	// VolumeInfo — diagnostic; may be synthetic, but must return without error.
	label, serial, err := share.VolumeInfo()
	if err != nil {
		t.Fatalf("VolumeInfo: %v", err)
	}
	t.Logf("VolumeInfo label=%q serial=%q", label, serial)

	// WriteMarkerAt then ReadMarker round-trips the marker at the share root.
	markerData := []byte("rig-id=integration-" + time.Now().UTC().Format(time.RFC3339Nano))
	if err := share.WriteMarkerAt("", markerName, markerData); err != nil {
		t.Fatalf("WriteMarkerAt(%q): %v", markerName, err)
	}
	got, err := share.ReadMarker(markerName)
	if err != nil {
		t.Fatalf("ReadMarker(%q): %v", markerName, err)
	}
	if string(got) != string(markerData) {
		t.Errorf("marker round-trip: got %q, want %q", got, markerData)
	}

	// ReadMarker of an absent name → an IsNotFound error (H: normalised onto
	// fs.ErrNotExist inside the device seam).
	if _, err := share.ReadMarker(".asiair-commander-absent-marker"); err == nil {
		t.Error("ReadMarker of an absent marker returned nil error")
	} else if !device.IsNotFound(err) {
		t.Errorf("ReadMarker of an absent marker: IsNotFound=false for %v", err)
	}

	// Delete the JPEG preview, then Stat must classify not-found.
	if err := share.Delete(previewRel); err != nil {
		t.Fatalf("Delete(%q): %v", previewRel, err)
	}
	if _, err := share.Stat(previewRel); err == nil {
		t.Errorf("Stat after Delete(%q) returned nil error", previewRel)
	} else if !device.IsNotFound(err) {
		t.Errorf("Stat after Delete: IsNotFound=false for %v", err)
	}

	// Umount + Session.Close cleanly.
	if err := share.Umount(); err != nil {
		t.Errorf("Umount: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Session.Close: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func names(entries []device.FileInfo) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func hasEntry(entries []device.FileInfo, name string, dir bool) bool {
	for _, e := range entries {
		if e.Name == name && e.IsDir == dir {
			return true
		}
	}
	return false
}

// findFile walks the share depth-first from dir and returns the share-relative
// slash path of the first entry named target, or "" if none.
func findFile(t *testing.T, share device.Share, dir, target string) string {
	t.Helper()
	entries, err := share.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		child := e.Name
		if dir != "" {
			child = dir + "/" + e.Name
		}
		if e.IsDir {
			if got := findFile(t, share, child, target); got != "" {
				return got
			}
			continue
		}
		if e.Name == target {
			return child
		}
	}
	return ""
}

// TestLargeDirectoryListsCompletely guards against a silent directory-listing
// truncation, which would produce a false "safe to power off".
//
// An SMB1 server answers a TRANS2 FIND with as many bytes as its OWN send buffer
// holds — Samba caps each message at the negotiated max_xmit (~16.5 KiB) — and
// sends the remainder as further messages under the same MID, flagged by
// TotalDataCount > DataCount. A client that reads a single message per request
// drops those continuations. Nothing errors: the connection stays coherent, the
// search reports more to come, and the caller receives a short list it cannot
// distinguish from a complete one.
//
// Against a real ASIAIR the missing slice is the tail of each batch, so an entire
// class of frames can become invisible: Commander would pull everything it could
// see, conclude the device was drained, and report safe to power off while
// un-pulled frames sat on the rig.
//
// Only a real server can prove this: the in-memory fake returns whatever it is
// asked for in one piece, and a small directory fits in one message, so nothing
// short of a genuinely fragmenting response exercises the path. Hence a seeded
// directory sized past the fragmentation threshold, and an EXACT count — "at
// least one" or "more than a batch" would pass while still losing entries.
func TestLargeDirectoryListsCompletely(t *testing.T) {
	dev := newDevice(t)
	ctx := context.Background()

	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	share, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount(%q): %v", shareName, err)
	}

	entries, err := share.ListDir(bigDir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", bigDir, err)
	}
	if len(entries) != bigDirCount {
		t.Fatalf("ListDir(%q) returned %d entries, want exactly %d — a short count with no error is the silent truncation this test exists to catch",
			bigDir, len(entries), bigDirCount)
	}

	// Every entry must be a distinct, plausible frame: a reassembly bug that
	// duplicated or corrupted entries would still hit the count.
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir {
			t.Errorf("ListDir(%q) returned a directory %q; the seed is files only", bigDir, e.Name)
		}
		if !strings.HasPrefix(e.Name, "Light_NGC7000_") || !strings.HasSuffix(e.Name, ".fit") {
			t.Errorf("unexpected entry name %q — reassembly may have corrupted the listing", e.Name)
		}
		if seen[e.Name] {
			t.Errorf("duplicate entry %q — reassembly overlapped a fragment", e.Name)
		}
		seen[e.Name] = true
	}
	if len(seen) != bigDirCount {
		t.Errorf("got %d distinct names, want %d", len(seen), bigDirCount)
	}
}
