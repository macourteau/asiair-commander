// Package localfs is the NAS-side filesystem seam (design architecture §4,
// design frame-lifecycle §5, §6, §8): the load-bearing at-rest verification and
// the atomic exclusive filing rename.
//
// The interface is small and platform-agnostic; the two methods that cannot be
// expressed portably — VerifyRead (a cache-bypassing device read-back) and
// RenameNoReplace (an atomic rename that fails if the target exists) — plus
// FreeSpace, SameVolume and ExtendedPath are split behind build tags so the
// library builds and tests on darwin/linux (dev + CI) and runs on Windows (the
// production NAS service).
package localfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LocalFS is the destination-volume filesystem seam consumed by the rig worker.
type LocalFS interface {
	// FreeSpace reports the free and total bytes of the volume holding root
	// (GetDiskFreeSpaceEx on Windows, statfs elsewhere). It backs the design
	// architecture §7 disk_reserve pull-gate on the NAS destination volume.
	FreeSpace(root string) (free, total int64, err error)
	// VerifyRead re-reads path with the OS page cache BYPASSED and returns the
	// hex SHA-256 of exactly `size` bytes. This is the load-bearing at-rest
	// verify (design frame-lifecycle §6): an ordinary buffered read after a write
	// would be served from clean-but-resident pages and prove nothing about what
	// the disk holds. The tail past `size` (present because a cache-bypassing read
	// is sector-aligned while a FITS file's length is not) is discarded.
	VerifyRead(path string, size int64) (sha256hex string, err error)
	// VerifyReadTee makes exactly the same single cache-bypassing pass VerifyRead
	// makes and returns a second digest off it: the hex SHA-256 of the bytes from
	// dataOffset to `size`. It is how a consumer that cannot scan a stream it does
	// not own gets a FITS data unit's digest without a second whole-file read,
	// which is the one thing the digest's design forbids — and without losing the
	// cache bypass, a buffered re-read of a file just written proving nothing
	// about what the disk holds.
	//
	// dataOffset is a plain byte count: this seam parses nothing and knows no file
	// format. An offset of 0 or less means "no data unit known" and yields an empty
	// data digest; an offset at or past `size` describes an empty range and yields
	// the SHA-256 of empty input, which the caller's own identity rule refuses.
	VerifyReadTee(path string, size, dataOffset int64) (sha256hex, dataSHA256hex string, err error)
	// RenameNoReplace atomically renames oldpath to newpath and FAILS if newpath
	// already exists, rather than replacing it (MoveFileEx without
	// MOVEFILE_REPLACE_EXISTING on Windows; renameat2 RENAME_NOREPLACE on Linux;
	// renameatx_np RENAME_EXCL on darwin). The exists condition is reported
	// distinctly as ErrExist so filing can disambiguate a name clash (design
	// frame-lifecycle §8).
	RenameNoReplace(oldpath, newpath string) error
	// SameVolume reports whether every path resolves to one volume, comparing
	// the nearest existing ancestor of each (config validation, design
	// architecture §7 single-volume).
	SameVolume(paths ...string) (bool, error)
	// ExtendedPath maps an absolute path to a \\?\-prefixed extended-length path
	// on Windows so the legacy MAX_PATH limit does not apply (design
	// routing-and-filing §4); identity elsewhere.
	ExtendedPath(p string) string
	// MkdirAll creates path and all parents with 0o755.
	MkdirAll(path string) error
	// Remove deletes a single file or empty directory.
	Remove(path string) error
	// Stat returns the size and modification time of path.
	Stat(path string) (size int64, mtime time.Time, err error)
	// WriteProbe confirms dir is actually writable by the current process (the
	// service account on the NAS): it creates a uniquely-named temp file in dir,
	// writes a byte, fsyncs and closes it, then removes it. It proves real write
	// access — a permission the low-privilege service account cannot self-grant on
	// an operator-chosen destination — which a mere Stat or MkdirAll of an existing
	// dir does not. dir must already exist (call MkdirAll first).
	WriteProbe(dir string) error
	// ReadDir lists dir's immediate entries (base names + IsDir), applying
	// ExtendedPath internally. A missing directory returns an error satisfying
	// fs.ErrNotExist. Used by the out-of-band-deletion scan and the library marker.
	ReadDir(dir string) ([]DirEntry, error)
	// ReadFile reads path whole, applying ExtendedPath. A missing file returns an
	// error satisfying fs.ErrNotExist (the library marker gate depends on that
	// discrimination). Used to read the NAS library marker.
	ReadFile(path string) ([]byte, error)
	// WriteFileAtomic writes data to path via a temp-file-plus-rename in the same
	// directory, so a reader never sees a partial file. Used to write the library
	// marker.
	WriteFileAtomic(path string, data []byte) error
}

// DirEntry is one directory entry: its base name and whether it is a directory.
type DirEntry struct {
	Name  string
	IsDir bool
}

// ErrExist is the sentinel RenameNoReplace wraps when the destination already
// exists. Callers test it via IsExist.
var ErrExist = errors.New("localfs: rename target already exists")

// IsExist reports whether err is (or wraps) the rename-target-exists condition
// raised by RenameNoReplace.
func IsExist(err error) bool { return errors.Is(err, ErrExist) }

// ErrCrossDevice is the sentinel RenameNoReplace wraps when the source and the
// destination sit on different volumes, which no rename can span. It is mapped
// explicitly because the condition is not portably detectable otherwise: Windows
// reports ERROR_NOT_SAME_DEVICE from a MoveFileEx issued without
// MOVEFILE_COPY_ALLOWED, and nothing in the standard library relates that to the
// POSIX EXDEV. Callers test it via IsCrossDevice.
var ErrCrossDevice = errors.New("localfs: rename across volumes")

// IsCrossDevice reports whether err is (or wraps) the rename-across-volumes
// condition raised by RenameNoReplace.
func IsCrossDevice(err error) bool { return errors.Is(err, ErrCrossDevice) }

// ErrNotEmpty is the sentinel Remove wraps when the target is a directory that
// still holds entries. It is the inert, expected outcome of pruning a directory
// the operator keeps unmanaged files in, so a caller must be able to name it as a
// skip reason rather than reporting a bare I/O error.
var ErrNotEmpty = errors.New("localfs: directory not empty")

// IsNotEmpty reports whether err is (or wraps) the directory-not-empty condition
// raised by Remove.
func IsNotEmpty(err error) bool { return errors.Is(err, ErrNotEmpty) }

// notEmpty reports whether a failed removal failed because the target directory
// still holds entries. Every platform routes its own errno to fs.ErrExist for
// exactly this case and no other — ENOTEMPTY on unix, ERROR_DIR_NOT_EMPTY on
// Windows — and a removal has no other way to fail "already exists", so this is
// the portable test and needs no build tags.
func notEmpty(err error) bool { return err != nil && errors.Is(err, fs.ErrExist) }

// OS is the real, platform-backed LocalFS. Its constructor is New.
type OS struct{}

// New returns the real OS-backed LocalFS.
func New() LocalFS { return OS{} }

var _ LocalFS = OS{}
var _ LocalFS = (*Fake)(nil)

func (o OS) MkdirAll(path string) error { return os.MkdirAll(o.ExtendedPath(path), 0o755) }

func (o OS) Remove(path string) error {
	err := os.Remove(o.ExtendedPath(path))
	if notEmpty(err) {
		return fmt.Errorf("remove %s: %w", path, ErrNotEmpty)
	}
	return err
}

func (o OS) Stat(path string) (int64, time.Time, error) {
	fi, err := os.Stat(o.ExtendedPath(path))
	if err != nil {
		return 0, time.Time{}, err
	}
	return fi.Size(), fi.ModTime(), nil
}

func (o OS) WriteProbe(dir string) error { return writeProbe(o.ExtendedPath(dir)) }

func (o OS) ReadDir(dir string) ([]DirEntry, error) {
	ents, err := os.ReadDir(o.ExtendedPath(dir))
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

func (o OS) ReadFile(path string) ([]byte, error) { return os.ReadFile(o.ExtendedPath(path)) }

func (o OS) WriteFileAtomic(path string, data []byte) error {
	return writeFileAtomic(o, path, data)
}

// writeFileAtomic writes data to a temp file in path's directory, fsyncs it, and
// renames it over path (replacing). Shared by OS and Fake so their atomic-write
// semantics match. The temp file is created through the given fs's ExtendedPath so
// a long Windows path does not hit MAX_PATH.
func writeFileAtomic(fsys LocalFS, path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(fsys.ExtendedPath(dir), ".aac-atomic-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := os.Rename(name, fsys.ExtendedPath(path)); err != nil {
		return fmt.Errorf("rename temp over %s: %w", path, err)
	}
	remove = false
	return nil
}

// writeProbe creates, writes, fsyncs, and removes a temp file in dir, returning
// the first error. It is the shared probe both OS and Fake use so their write-test
// semantics match. A leftover temp file on a mid-probe failure is best-effort
// removed; the returned error still reflects the real fault.
func writeProbe(dir string) error {
	f, err := os.CreateTemp(dir, ".aac-writecheck-*")
	if err != nil {
		return fmt.Errorf("create probe file in %s: %w", dir, err)
	}
	name := f.Name()
	defer os.Remove(name)

	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		return fmt.Errorf("write probe file in %s: %w", dir, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync probe file in %s: %w", dir, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close probe file in %s: %w", dir, err)
	}
	return nil
}

// ErrShortFile reports that a file is shorter than the expected byte count — a
// definitive corruption (truncated write, disk-full, rot), not a transient I/O
// error. Callers must distinguish it so a genuinely lost frame is alerted rather
// than retried forever.
var ErrShortFile = errors.New("file shorter than expected size")

// hashNTee returns the hex SHA-256 of exactly n bytes read from r, discarding
// anything r yields past n, and a second digest teed off the same bytes covering
// [dataOffset, n). It is the single "hash exactly size bytes" helper every
// platform's VerifyRead and VerifyReadTee route through — each of them exactly
// once per call — so the tail handling and the ErrShortFile discrimination stay
// identical by construction rather than by three parallel edits, and so a caller
// that wants both digests cannot be served by two passes over the file. n==0
// yields the SHA-256 of the empty input; a source shorter than n yields
// ErrShortFile. dataOffset <= 0 means "no second digest": the returned data digest
// is the empty string, not a hash of nothing, so a caller can tell "not known"
// from "known to be empty".
func hashNTee(r io.Reader, n, dataOffset int64) (whole, data string, err error) {
	h := sha256.New()
	var w io.Writer = h
	var d hash.Hash
	if dataOffset > 0 {
		skip := dataOffset
		if skip > n {
			skip = n // an offset past the recorded size describes an empty range
		}
		d = sha256.New()
		w = &splitWriter{whole: h, data: d, skip: skip}
	}
	if n > 0 {
		if _, err := io.CopyN(w, r, n); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", "", fmt.Errorf("read %d bytes: %w", n, ErrShortFile)
			}
			return "", "", fmt.Errorf("hash %d bytes: %w", n, err)
		}
	}
	whole = hex.EncodeToString(h.Sum(nil))
	if d == nil {
		return whole, "", nil
	}
	return whole, hex.EncodeToString(d.Sum(nil)), nil
}

// splitWriter feeds every byte to whole and, once the first `skip` bytes have
// passed, the remainder to data as well. It is what makes the tee one pass: the
// caller's reader is consumed exactly once and both digests advance together.
type splitWriter struct {
	whole, data hash.Hash
	skip        int64
}

func (w *splitWriter) Write(p []byte) (int, error) {
	w.whole.Write(p)
	if w.skip >= int64(len(p)) {
		w.skip -= int64(len(p))
		return len(p), nil
	}
	w.data.Write(p[w.skip:])
	w.skip = 0
	return len(p), nil
}

// existingAncestor returns the nearest existing ancestor of p (p itself if it
// exists), made absolute. SameVolume needs it because a destination directory
// may not exist yet, but the volume it will live on does.
func existingAncestor(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs, nil // reached the volume root
		}
		abs = parent
	}
}
