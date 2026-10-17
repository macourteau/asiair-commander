package localfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// plainHash is the reference: a straightforward crypto/sha256 of the first n
// bytes of b, the digest VerifyRead's cache-bypassing tail handling must match.
func plainHash(b []byte, n int) string {
	sum := sha256.Sum256(b[:n])
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, dir string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, "frame.fit")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestVerifyReadTailSizes is the design delete-safety §6 tail-case guard: over
// a range of NON-sector-multiple lengths (and 0), VerifyRead must return the
// SHA-256 of exactly `size` bytes — identical to a plain crypto/sha256 of the
// same bytes, with no sector padding hashed in.
func TestVerifyReadTailSizes(t *testing.T) {
	fs := New()
	rng := rand.New(rand.NewSource(1))

	sizes := []int{0, 1, 100, 511, 512, 513, 4095, 4096, 4097, 5000, 8192, 8193}
	for _, size := range sizes {
		data := make([]byte, size)
		rng.Read(data)
		dir := t.TempDir()
		p := writeFile(t, dir, data)

		got, err := fs.VerifyRead(p, int64(size))
		if err != nil {
			t.Fatalf("size=%d VerifyRead: %v", size, err)
		}
		want := plainHash(data, size)
		if got != want {
			t.Errorf("size=%d hash mismatch\n got=%s\nwant=%s", size, got, want)
		}
	}
}

// A file shorter than the recorded size is a definitive corruption, reported as
// ErrShortFile so callers alert-as-lost rather than retry forever.
func TestVerifyReadShortFile(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	p := writeFile(t, dir, []byte("hello"))
	if _, err := fs.VerifyRead(p, 100); !errors.Is(err, ErrShortFile) {
		t.Fatalf("VerifyRead on a truncated file: want ErrShortFile, got %v", err)
	}
}

// TestVerifyReadDiscardsTail proves the "hash exactly size, discard tail" path
// directly: when the on-disk file is LARGER than the recorded size, VerifyRead
// hashes only the first `size` bytes, mirroring how the sector-aligned read on
// Windows would read past the recorded length and must not fold the surplus in.
func TestVerifyReadDiscardsTail(t *testing.T) {
	fs := New()
	rng := rand.New(rand.NewSource(2))
	data := make([]byte, 9000)
	rng.Read(data)
	dir := t.TempDir()
	p := writeFile(t, dir, data)

	const size = 4097
	got, err := fs.VerifyRead(p, size)
	if err != nil {
		t.Fatalf("VerifyRead: %v", err)
	}
	if want := plainHash(data, size); got != want {
		t.Errorf("tail not discarded\n got=%s\nwant=%s", got, want)
	}
}

func TestRenameNoReplace_Success(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fs.RenameNoReplace(src, dst); err != nil {
		t.Fatalf("RenameNoReplace: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after rename: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Errorf("dst content = %q, err %v", got, err)
	}
}

func TestRenameNoReplace_Exists(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := fs.RenameNoReplace(src, dst)
	if err == nil {
		t.Fatal("expected error renaming onto an existing target")
	}
	if !IsExist(err) {
		t.Errorf("IsExist(err) = false, want true; err = %v", err)
	}
	// The existing target must be untouched, not overwritten.
	if got, _ := os.ReadFile(dst); string(got) != "existing" {
		t.Errorf("dst overwritten: %q", got)
	}
}

// TestRenameNoReplace_Race is the concurrency crux of design frame-lifecycle §8:
// many goroutines each rename their own source onto one shared target; exactly
// one wins and every other loses with IsExist — no lock, no lost update, no
// double-file.
func TestRenameNoReplace_Race(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, existLosers, otherErr := 0, 0, 0

	for i := 0; i < n; i++ {
		src := filepath.Join(dir, "src"+string(rune('a'+i)))
		if err := os.WriteFile(src, []byte("frame"), 0o644); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			err := fs.RenameNoReplace(src, target)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case IsExist(err):
				existLosers++
			default:
				otherErr++
				t.Errorf("unexpected error: %v", err)
			}
		}(src)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want 1", winners)
	}
	if existLosers != n-1 {
		t.Errorf("IsExist losers = %d, want %d", existLosers, n-1)
	}
	if otherErr != 0 {
		t.Errorf("unexpected errors = %d", otherErr)
	}
}

func TestSameVolume(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	// b does not exist yet — SameVolume must resolve it via its existing ancestor.
	b := filepath.Join(dir, "sub", "deep", "b")
	if err := os.WriteFile(a, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	same, err := fs.SameVolume(a, b, dir)
	if err != nil {
		t.Fatalf("SameVolume: %v", err)
	}
	if !same {
		t.Error("paths under one temp dir reported as different volumes")
	}
}

func TestFreeSpace(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	free, total, err := fs.FreeSpace(dir)
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if free <= 0 || total <= 0 {
		t.Errorf("free=%d total=%d, want both > 0", free, total)
	}
	if free > total {
		t.Errorf("free %d > total %d", free, total)
	}
}

func TestStatAndMkdirAndRemove(t *testing.T) {
	fs := New()
	dir := t.TempDir()
	nested := filepath.Join(dir, "x", "y", "z")
	if err := fs.MkdirAll(nested); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(nested, "f")
	if err := os.WriteFile(p, []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, mtime, err := fs.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != 4 {
		t.Errorf("size = %d, want 4", size)
	}
	if mtime.IsZero() {
		t.Error("mtime is zero")
	}
	if err := fs.Remove(p); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file still present after Remove: %v", err)
	}
}

// --- Fake ---

func TestFake_CorruptAfterWrite(t *testing.T) {
	dir := t.TempDir()
	f := NewFake(dir)
	data := []byte("verified-on-disk")
	p := writeFile(t, dir, data)
	honest := plainHash(data, len(data))

	// No hook: VerifyRead returns the honest hash.
	got, err := f.VerifyRead(p, int64(len(data)))
	if err != nil {
		t.Fatalf("VerifyRead: %v", err)
	}
	if got != honest {
		t.Errorf("unhooked hash = %s, want %s", got, honest)
	}

	// Hook simulates at-rest corruption: reported hash diverges from the truth.
	f.CorruptAfterWrite = func(_ string, honest string) string {
		return "deadbeef" + honest[8:]
	}
	got, err = f.VerifyRead(p, int64(len(data)))
	if err != nil {
		t.Fatalf("VerifyRead (corrupt): %v", err)
	}
	if got == honest {
		t.Error("CorruptAfterWrite did not change the reported hash")
	}
}

func TestFake_InjectedFreeSpace(t *testing.T) {
	f := NewFake(t.TempDir())
	f.FreeBytes = 42
	f.TotalBytes = 100
	free, total, err := f.FreeSpace("anywhere")
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if free != 42 || total != 100 {
		t.Errorf("free=%d total=%d, want 42/100", free, total)
	}
}

func TestFake_RenameExclusive(t *testing.T) {
	dir := t.TempDir()
	f := NewFake(dir)
	target := filepath.Join(dir, "target")

	srcA := filepath.Join(dir, "a")
	srcB := filepath.Join(dir, "b")
	if err := os.WriteFile(srcA, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcB, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := f.RenameNoReplace(srcA, target); err != nil {
		t.Fatalf("first rename: %v", err)
	}
	err := f.RenameNoReplace(srcB, target)
	if !IsExist(err) {
		t.Errorf("second rename: IsExist = false, want true; err = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "A" {
		t.Errorf("target = %q, want A (not overwritten)", got)
	}
}

// TestWriteProbeSucceedsAndCleansUp verifies the OS write-probe reports writable
// on a real directory and leaves no probe file behind.
func TestWriteProbeSucceedsAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	if err := New().WriteProbe(dir); err != nil {
		t.Fatalf("WriteProbe on a writable dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left %d file(s) behind, want 0", len(entries))
	}
}

// TestWriteProbeMissingDir reports an error when the directory does not exist —
// callers MkdirAll first, so a failure here is a genuine access/path fault.
func TestWriteProbeMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := New().WriteProbe(missing); err == nil {
		t.Fatal("WriteProbe on a missing dir: want error, got nil")
	}
}

// TestFakeWriteProbeInjectedError confirms the Fake surfaces an injected ProbeErr,
// the seam the roots-check endpoint test drives to simulate an unwritable root.
func TestFakeWriteProbeInjectedError(t *testing.T) {
	f := NewFake(t.TempDir())
	f.ProbeErr = func(string) error { return errors.New("access is denied") }
	if err := f.WriteProbe(t.TempDir()); err == nil {
		t.Fatal("Fake.WriteProbe with ProbeErr: want error, got nil")
	}
}

// TestRemoveNonEmptyDirIsErrNotEmpty pins the portable mapping the re-file prune
// depends on: removing a directory that still holds entries must be reportable by
// name, not as a bare wrapped errno that only one platform spells the same way.
// Both implementations are checked because the prune runs against the Fake in
// tests and the real OS in production.
func TestRemoveNonEmptyDirIsErrNotEmpty(t *testing.T) {
	for name, fsys := range map[string]LocalFS{"os": New(), "fake": NewFake(t.TempDir())} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "night")
			if err := fsys.MkdirAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "unmanaged.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := fsys.Remove(dir)
			if !IsNotEmpty(err) {
				t.Fatalf("Remove of a non-empty dir = %v, want ErrNotEmpty", err)
			}
			// The removal must be inert: refusing is the whole guarantee.
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("a refused removal must leave the directory alone: %v", err)
			}
			if err := os.Remove(filepath.Join(dir, "unmanaged.txt")); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Remove(dir); err != nil {
				t.Fatalf("Remove of the now-empty dir: %v", err)
			}
		})
	}
}

// TestFakeRenameInjectedError confirms the Fake surfaces an injected RenameErr and
// does NOT move the file when it does — the seam the re-file commit-failure and
// rename-back-fails cases drive, neither of which the real rename can be made to
// produce on demand.
func TestFakeRenameInjectedError(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, []byte("frame"))
	dst := filepath.Join(dir, "moved.fit")

	f := NewFake(dir)
	f.RenameErr = func(string, string) error { return ErrCrossDevice }
	if err := f.RenameNoReplace(src, dst); !IsCrossDevice(err) {
		t.Fatalf("RenameNoReplace with RenameErr = %v, want ErrCrossDevice", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("an injected rename failure must not move the file")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the source must survive an injected rename failure: %v", err)
	}

	// A nil return falls through to the real link-plus-remove.
	f.RenameErr = func(string, string) error { return nil }
	if err := f.RenameNoReplace(src, dst); err != nil {
		t.Fatalf("RenameNoReplace with a nil-returning RenameErr: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("the fall-through path must still rename: %v", err)
	}
}
