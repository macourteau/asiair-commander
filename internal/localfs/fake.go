package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Fake is a temp-dir-backed LocalFS for the rig-worker tests (design architecture
// §4). It is real where realism matters — MkdirAll, Remove, Stat, and
// RenameNoReplace operate on the actual filesystem, so exclusive-rename and
// directory semantics behave as in production — and injectable where a test needs
// to drive a branch: FreeSpace and SameVolume return settable values (for the
// disk_reserve gate and single-volume validation), and CorruptAfterWrite lets a
// test simulate at-rest corruption to drive the design delete-safety §6 data-loss
// test.
//
// Paths passed to a Fake are ordinary filesystem paths (typically under a
// t.TempDir()); Root records the backing directory for the caller's convenience
// but the methods act on whatever absolute path they are given.
type Fake struct {
	Root string

	// FreeBytes and TotalBytes are what FreeSpace returns regardless of root, so
	// the disk_reserve pull-gate can be exercised without filling a real disk.
	FreeBytes  int64
	TotalBytes int64

	// SameVol is what SameVolume returns regardless of the paths.
	SameVol bool

	// FreeSpaceErr, when non-nil, makes FreeSpace fail instead of reporting
	// FreeBytes/TotalBytes — an unreadable destination (unmounted bind mount,
	// dropped share) rather than a full one.
	FreeSpaceErr error

	// CorruptAfterWrite, when non-nil, is consulted by VerifyRead to simulate an
	// at-rest fault: it receives the path and the honest SHA-256 of the bytes on
	// disk and returns the hash VerifyRead should report. Returning a different
	// string forces the diskHash != streamHash mismatch that drives the design
	// delete-safety §6 data-loss test; returning the honest hash unchanged is a
	// no-op.
	CorruptAfterWrite func(path, honestHash string) string

	// ProbeErr, when non-nil, is returned by WriteProbe for dir instead of running
	// the real probe, so a test can simulate an unwritable destination (the
	// low-privilege service account denied by NTFS) without doctoring permissions.
	ProbeErr func(dir string) error

	// RemoveErr, when non-nil, is consulted by Remove before touching the real
	// filesystem: a non-nil return is returned as-is (so a test can force a
	// deterministic non-NotExist failure, e.g. the dismiss purge-failure path),
	// while a nil return falls through to the real os.Remove.
	RemoveErr func(path string) error

	// ReadDirErr, when non-nil, is consulted by ReadDir before touching the real
	// filesystem: a non-nil return is returned as-is, letting a test force a
	// transient (non-NotExist) listing error that the reconciliation scan must
	// treat as "conclude nothing".
	ReadDirErr func(dir string) error

	// RenameErr, when non-nil, is consulted by RenameNoReplace before touching the
	// real filesystem: a non-nil return is returned as-is (so a test can drive a
	// deliberate ErrExist, ErrCrossDevice or arbitrary failure, and choose whether
	// the move happens at all), while a nil return falls through to the real
	// link-plus-remove. Without it the re-file commit-failure and
	// rename-back-fails paths cannot be exercised, since the real rename either
	// succeeds or fails for reasons a test cannot arrange.
	RenameErr func(oldpath, newpath string) error
}

// NewFake returns a Fake backed by root (e.g. a t.TempDir()) with a large
// default free/total space and SameVolume true.
func NewFake(root string) *Fake {
	return &Fake{
		Root:       root,
		FreeBytes:  1 << 40, // 1 TiB
		TotalBytes: 1 << 40,
		SameVol:    true,
	}
}

func (f *Fake) FreeSpace(root string) (int64, int64, error) {
	// FreeSpaceErr models a destination that cannot be statted at all — an
	// unmounted bind mount or a dropped network share — as opposed to one that is
	// merely full (FreeBytes below the reserve).
	if f.FreeSpaceErr != nil {
		return 0, 0, f.FreeSpaceErr
	}
	return f.FreeBytes, f.TotalBytes, nil
}

func (f *Fake) VerifyRead(path string, size int64) (string, error) {
	sum, _, err := f.VerifyReadTee(path, size, 0)
	return sum, err
}

// VerifyReadTee reads path once and reports both digests, routing through the same
// hashNTee the platform implementations use so the Fake's tail handling matches
// theirs. CorruptAfterWrite applies to the whole-file digest exactly as it does for
// VerifyRead — the tee rides the same read, so a test that corrupts a file at rest
// sees the tee report the corruption too rather than a second, honest answer.
func (f *Fake) VerifyReadTee(path string, size, dataOffset int64) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("fake verifyread open %s: %w", path, err)
	}
	defer file.Close()
	honest, data, err := hashNTee(file, size, dataOffset)
	if err != nil {
		return "", "", err
	}
	if f.CorruptAfterWrite != nil {
		return f.CorruptAfterWrite(path, honest), data, nil
	}
	return honest, data, nil
}

// RenameNoReplace claims newpath exclusively with a hard link plus a remove of
// the source: os.Link fails if newpath exists (mapped to ErrExist) and is atomic,
// so two concurrent callers targeting the same name cannot both win — the exact
// property the worker's race-free filing depends on.
//
// Note the link-plus-remove is NOT atomic the way a real rename is: a successful
// link followed by a failed remove leaves the destination present alongside a
// non-ErrExist error. A test that needs a specific outcome should drive RenameErr
// rather than rely on that.
func (f *Fake) RenameNoReplace(oldpath, newpath string) error {
	if f.RenameErr != nil {
		if err := f.RenameErr(oldpath, newpath); err != nil {
			return err
		}
	}
	if err := os.Link(oldpath, newpath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("fake rename %s -> %s: %w", oldpath, newpath, ErrExist)
		}
		return fmt.Errorf("fake rename %s -> %s: %w", oldpath, newpath, err)
	}
	if err := os.Remove(oldpath); err != nil {
		return fmt.Errorf("fake rename cleanup %s: %w", oldpath, err)
	}
	return nil
}

func (f *Fake) SameVolume(paths ...string) (bool, error) { return f.SameVol, nil }

func (f *Fake) ExtendedPath(p string) string { return p }

func (f *Fake) MkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

func (f *Fake) Remove(path string) error {
	if f.RemoveErr != nil {
		if err := f.RemoveErr(path); err != nil {
			return err
		}
	}
	err := os.Remove(path)
	if notEmpty(err) {
		return fmt.Errorf("fake remove %s: %w", path, ErrNotEmpty)
	}
	return err
}

func (f *Fake) Stat(path string) (int64, time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	return fi.Size(), fi.ModTime(), nil
}

// WriteProbe runs the real temp-file probe against dir unless ProbeErr is set, in
// which case it returns that injected error — letting a test drive the
// unwritable-destination branch.
func (f *Fake) WriteProbe(dir string) error {
	if f.ProbeErr != nil {
		return f.ProbeErr(dir)
	}
	return writeProbe(dir)
}

func (f *Fake) ReadDir(dir string) ([]DirEntry, error) {
	if f.ReadDirErr != nil {
		if err := f.ReadDirErr(dir); err != nil {
			return nil, err
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

func (f *Fake) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (f *Fake) WriteFileAtomic(path string, data []byte) error {
	return writeFileAtomic(f, path, data)
}
