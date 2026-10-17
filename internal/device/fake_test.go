package device

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
)

func mustOpen(t *testing.T, f *Fake) Session {
	t.Helper()
	sess, err := f.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return sess
}

func mustMount(t *testing.T, sess Session, share string) Share {
	t.Helper()
	sh, err := sess.Mount(share)
	if err != nil {
		t.Fatalf("Mount(%q): %v", share, err)
	}
	return sh
}

func TestProbeReachable(t *testing.T) {
	f := NewFake()
	if !f.Probe(context.Background()) {
		t.Fatal("fresh fake should be reachable")
	}
	f.SetReachable(false)
	if f.Probe(context.Background()) {
		t.Fatal("SetReachable(false) should make Probe false")
	}
}

func TestServerTime(t *testing.T) {
	f := NewFake()
	now := time.Date(2026, 7, 16, 3, 30, 0, 0, time.UTC)
	st := clock.ServerTime{Time: now, ReceivedAt: now.Add(-2 * time.Second)}
	f.SetServerTime(st)
	got := mustOpen(t, f).ServerTime()
	if !got.Time.Equal(st.Time) || !got.ReceivedAt.Equal(st.ReceivedAt) {
		t.Fatalf("ServerTime = %+v, want %+v", got, st)
	}
}

func TestListSharesSorted(t *testing.T) {
	f := NewFake()
	f.AddShare("TF Images")
	f.AddShare("EMMC Images")
	shares, err := mustOpen(t, f).ListShares()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"EMMC Images", "TF Images"}
	if len(shares) != len(want) {
		t.Fatalf("shares = %v, want %v", shares, want)
	}
	for i := range want {
		if shares[i] != want[i] {
			t.Fatalf("shares = %v, want %v", shares, want)
		}
	}
}

func TestListStatOpenDeleteRoundTrip(t *testing.T) {
	f := NewFake()
	mtime := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	f.AddFile("EMMC Images", "Light_001.fit", []byte("frame-data"), mtime)

	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	entries, err := sh.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListDir root = %v, want 1 entry", entries)
	}
	got := entries[0]
	if got.Name != "Light_001.fit" || got.Size != int64(len("frame-data")) || got.IsDir {
		t.Fatalf("entry = %+v", got)
	}
	if !got.Mtime.Equal(mtime) {
		t.Fatalf("mtime = %v, want %v", got.Mtime, mtime)
	}

	fi, err := sh.Stat("Light_001.fit")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != int64(len("frame-data")) {
		t.Fatalf("stat size = %d", fi.Size)
	}

	rc, err := sh.OpenReader("Light_001.fit")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "frame-data" {
		t.Fatalf("read %q", data)
	}

	if err := sh.Delete("Light_001.fit"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := sh.Stat("Light_001.fit"); !IsNotFound(err) {
		t.Fatalf("Stat after delete err = %v, want not-found", err)
	}
	if entries, _ := sh.ListDir(""); len(entries) != 0 {
		t.Fatalf("ListDir after delete = %v, want empty", entries)
	}
}

func TestListDirHierarchy(t *testing.T) {
	f := NewFake()
	mtime := time.Now()
	f.AddFile("EMMC Images", "Autorun/Light/frame.fit", []byte("x"), mtime)
	f.AddFile("EMMC Images", "top.fit", []byte("y"), mtime)
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	root, err := sh.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(root) != 2 {
		t.Fatalf("root = %v, want Autorun dir + top.fit", root)
	}
	var sawDir, sawFile bool
	for _, e := range root {
		switch e.Name {
		case "Autorun":
			sawDir = e.IsDir
		case "top.fit":
			sawFile = !e.IsDir
		}
	}
	if !sawDir || !sawFile {
		t.Fatalf("root entries wrong: %+v", root)
	}

	// Implicit directory is statable as a directory.
	di, err := sh.Stat("Autorun")
	if err != nil || !di.IsDir {
		t.Fatalf("Stat(Autorun) = %+v, %v", di, err)
	}

	sub, err := sh.ListDir("Autorun/Light")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Name != "frame.fit" {
		t.Fatalf("Autorun/Light = %+v", sub)
	}
}

func TestMarkerReadWriteAndAbsent(t *testing.T) {
	f := NewFake()
	f.AddShare("EMMC Images")
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	if _, err := sh.ReadMarker(".asiair-commander-media-id"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent marker err = %v, want fs.ErrNotExist", err)
	}
	if !IsNotFound(markerErr(t, sh, ".asiair-commander-media-id")) {
		t.Fatal("absent marker should be IsNotFound")
	}

	payload := []byte(`{"id":"abc"}`)
	if err := sh.WriteMarkerAt("", ".asiair-commander-media-id", payload); err != nil {
		t.Fatal(err)
	}
	got, err := sh.ReadMarker(".asiair-commander-media-id")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("marker = %q, want %q", got, payload)
	}

	// Markers never surface as directory entries.
	if entries, _ := sh.ListDir(""); len(entries) != 0 {
		t.Fatalf("markers leaked into ListDir: %v", entries)
	}
}

// TestWriteMarkerAtSubpathRoundTrip proves a marker written under a subfolder
// (ASIAIR/, the relocated media-marker home) round-trips through the same
// forward-slash path.Join key on read and delete, without leaking into the
// files map or a directory listing.
func TestWriteMarkerAtSubpathRoundTrip(t *testing.T) {
	f := NewFake()
	f.AddShare("Udisk Images")
	sh := mustMount(t, mustOpen(t, f), "Udisk Images")

	const name = ".asiair-commander-media-id"
	payload := []byte(`{"id":"under-asiair"}`)
	if err := sh.WriteMarkerAt("ASIAIR", name, payload); err != nil {
		t.Fatalf("WriteMarkerAt: %v", err)
	}

	// The root location is still absent — the marker lives only under ASIAIR/.
	if _, err := sh.ReadMarker(name); !IsNotFound(err) {
		t.Fatalf("root marker should be absent, err = %v", err)
	}
	got, err := sh.ReadMarker("ASIAIR/" + name)
	if err != nil {
		t.Fatalf("ReadMarker(ASIAIR/): %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("marker = %q, want %q", got, payload)
	}

	// Markers never surface as directory entries, at the root or under ASIAIR/.
	if entries, _ := sh.ListDir(""); len(entries) != 0 {
		t.Fatalf("markers leaked into root ListDir: %v", entries)
	}

	if err := sh.Delete("ASIAIR/" + name); err != nil {
		t.Fatalf("Delete(ASIAIR/): %v", err)
	}
	if _, err := sh.ReadMarker("ASIAIR/" + name); !IsNotFound(err) {
		t.Fatalf("marker should be gone after delete, err = %v", err)
	}
}

// markerErr returns the error from a marker read, for classifier tests.
func markerErr(t *testing.T, sh Share, name string) error {
	t.Helper()
	_, e := sh.ReadMarker(name)
	return e
}

func TestInjectedNetworkError(t *testing.T) {
	f := NewFake()
	f.AddShare("EMMC Images")
	f.FailListDir("EMMC Images", NetworkError("link down"))
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	_, e := sh.ListDir("")
	if !IsNetwork(e) {
		t.Fatalf("err = %v, want IsNetwork", e)
	}
	if IsNotFound(e) {
		t.Fatal("network error must not classify as not-found")
	}
}

func TestInjectedNotFoundError(t *testing.T) {
	f := NewFake()
	f.AddFile("EMMC Images", "a.fit", []byte("x"), time.Now())
	f.FailOpen("EMMC Images", NotFoundError("vanished"))
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	_, e := sh.OpenReader("a.fit")
	if !IsNotFound(e) {
		t.Fatalf("err = %v, want IsNotFound", e)
	}
	if IsNetwork(e) {
		t.Fatal("not-found must not classify as network")
	}
}

func TestSessionOpenAndMountErrors(t *testing.T) {
	f := NewFake()
	f.FailSessionOpen(NetworkError("no route"))
	if _, e := f.Open(context.Background()); !IsNetwork(e) {
		t.Fatalf("Open err = %v, want IsNetwork", e)
	}

	f.FailSessionOpen(nil)
	sess := mustOpen(t, f)

	// Mounting a share that was never added is not-found.
	if _, e := sess.Mount("Ghost"); !IsNotFound(e) {
		t.Fatalf("Mount ghost err = %v, want IsNotFound", e)
	}

	f.AddShare("EMMC Images")
	f.FailMount("EMMC Images", NetworkError("tree connect failed"))
	if _, e := sess.Mount("EMMC Images"); !IsNetwork(e) {
		t.Fatalf("Mount err = %v, want IsNetwork", e)
	}
}

// TestPersistentDeleteFailure exercises the sharing-violation path the caller's
// retry budget guards: Delete keeps failing.
func TestPersistentDeleteFailure(t *testing.T) {
	f := NewFake()
	f.AddFile("EMMC Images", "stuck.fit", []byte("x"), time.Now())
	sentinel := errors.New("STATUS_SHARING_VIOLATION")
	f.FailDelete("EMMC Images", sentinel)
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	for i := 0; i < 3; i++ {
		if e := sh.Delete("stuck.fit"); !errors.Is(e, sentinel) {
			t.Fatalf("attempt %d: Delete err = %v, want sentinel", i, e)
		}
	}
	// The file is untouched by the failed deletes.
	if _, e := sh.Stat("stuck.fit"); e != nil {
		t.Fatalf("file should survive failed deletes: %v", e)
	}
}

// TestEmptyMount is finding H5: a share mounts but lists nothing, which is
// indistinguishable from "all transferred" without the media marker.
func TestEmptyMount(t *testing.T) {
	f := NewFake()
	f.AddShare("Udisk Images")
	sh := mustMount(t, mustOpen(t, f), "Udisk Images")
	entries, err := sh.ListDir("")
	if err != nil {
		t.Fatalf("empty mount ListDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty mount = %v, want no entries", entries)
	}
}

// TestDualShareSameFile is finding H12: the same logical file appears under two
// shares. Deleting from one leaves the other intact.
func TestDualShareSameFile(t *testing.T) {
	f := NewFake()
	mtime := time.Now()
	data := []byte("same-bytes")
	f.AddFile("EMMC Images", "Light_001.fit", data, mtime)
	f.AddFile("TF Images", "Light_001.fit", data, mtime)

	sess := mustOpen(t, f)
	emmc := mustMount(t, sess, "EMMC Images")
	tf := mustMount(t, sess, "TF Images")

	for _, sh := range []Share{emmc, tf} {
		fi, err := sh.Stat("Light_001.fit")
		if err != nil {
			t.Fatalf("%s stat: %v", sh.Name(), err)
		}
		if fi.Size != int64(len(data)) {
			t.Fatalf("%s size = %d", sh.Name(), fi.Size)
		}
	}

	if err := emmc.Delete("Light_001.fit"); err != nil {
		t.Fatal(err)
	}
	if _, err := emmc.Stat("Light_001.fit"); !IsNotFound(err) {
		t.Fatalf("EMMC copy should be gone: %v", err)
	}
	if _, err := tf.Stat("Light_001.fit"); err != nil {
		t.Fatalf("TF copy should remain: %v", err)
	}
}

func TestStatfsAndVolumeInfo(t *testing.T) {
	f := NewFake()
	f.AddShare("EMMC Images")
	f.SetStatfs("EMMC Images", 5<<30, 32<<30)
	f.SetVolumeInfo("EMMC Images", "ASIAIR", "1A2B3C4D")
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	free, total, err := sh.Statfs()
	if err != nil {
		t.Fatal(err)
	}
	if free != 5<<30 || total != 32<<30 {
		t.Fatalf("Statfs = %d/%d", free, total)
	}

	label, serial, err := sh.VolumeInfo()
	if err != nil {
		t.Fatal(err)
	}
	if label != "ASIAIR" || serial != "1A2B3C4D" {
		t.Fatalf("VolumeInfo = %q/%q", label, serial)
	}
}

// TestStillWritingFile simulates a file whose mtime advances and size grows
// across polls — the quiescence check the rig worker drives.
func TestStillWritingFile(t *testing.T) {
	f := NewFake()
	t0 := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	f.AddFile("EMMC Images", "growing.fit", make([]byte, 1000), t0)
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	first, _ := sh.Stat("growing.fit")

	f.SetSize("EMMC Images", "growing.fit", 4000)
	f.SetMtime("EMMC Images", "growing.fit", t0.Add(30*time.Second))

	second, _ := sh.Stat("growing.fit")
	if second.Size <= first.Size {
		t.Fatalf("size did not grow: %d -> %d", first.Size, second.Size)
	}
	if !second.Mtime.After(first.Mtime) {
		t.Fatalf("mtime did not advance: %v -> %v", first.Mtime, second.Mtime)
	}
}

// TestOnOpenReaderHook: the hook sees every content read with its share and path,
// runs before the read so it can change what is read, and runs outside the fake's
// lock so it can call back into the fake.
func TestOnOpenReaderHook(t *testing.T) {
	f := NewFake()
	t0 := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	f.AddFile("EMMC Images", "a.fit", []byte("original"), t0)
	sh := mustMount(t, mustOpen(t, f), "EMMC Images")

	var calls []string
	f.OnOpenReader = func(share, path string) {
		calls = append(calls, share+"|"+path)
		if path == "a.fit" {
			f.AddFile(share, path, []byte("rewritten"), t0) // re-enters the fake's lock
		}
	}
	rc, err := sh.OpenReader("a.fit")
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "rewritten" {
		t.Fatalf("read %q, want the hook's rewrite to be what is read", got)
	}
	if _, err := sh.OpenReader("missing.fit"); err == nil {
		t.Fatal("opening a missing file should fail")
	}
	want := []string{"EMMC Images|a.fit", "EMMC Images|missing.fit"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("hook calls = %v, want %v", calls, want)
	}
	if n := f.ContentReads(); n != 2 {
		t.Fatalf("ContentReads = %d, want 2 (the hook fires for every read it counts)", n)
	}
}

func TestClassifierHelpersIgnoreNil(t *testing.T) {
	if IsNotFound(nil) {
		t.Fatal("IsNotFound(nil) should be false")
	}
	if IsNetwork(nil) {
		t.Fatal("IsNetwork(nil) should be false")
	}
}
