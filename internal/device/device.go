// Package device is the SMB seam over smb1client: one configured rig's
// storage access. A fresh Session is opened per polling cycle (design
// frame-lifecycle §10), used to list shares, mount them, and read/delete
// frames, then closed. The real implementation wraps smb1client; a hermetic
// in-memory Fake drives the rig worker's tests.
//
// smb1client's error vocabulary does not escape this package: consumers ask
// IsNotFound / IsNetwork instead of importing the transport, and not-found
// conditions are normalised onto fs.ErrNotExist so callers can classify with
// errors.Is.
package device

import (
	"context"
	"errors"
	"io"
	"time"

	smb1 "github.com/macourteau/smb1client"

	"github.com/macourteau/asiair-commander/internal/clock"
)

// Device is one configured rig's SMB access.
type Device interface {
	// Probe dials host:445 with a short timeout; true if reachable (design
	// frame-lifecycle §1).
	Probe(ctx context.Context) bool
	// Open connects (TCP + NTLMv2 guest) and returns a Session, reading the
	// server clock at negotiate for the clock offset (design frame-lifecycle §2).
	// On failure it leaves no connection open.
	Open(ctx context.Context) (Session, error)
}

// Session is one authenticated SMB session against a rig.
type Session interface {
	// ServerTime is the device clock captured at negotiate (design frame-lifecycle
	// §2).
	ServerTime() clock.ServerTime
	// ListShares returns the storage shares, with IPC$/print$ and other
	// administrative ($-suffixed) shares filtered out.
	ListShares() ([]string, error)
	// Mount connects to a storage share.
	Mount(share string) (Share, error)
	// Close umounts every share mounted through this session and logs off
	// (design frame-lifecycle §10).
	Close() error
}

// Share is a mounted storage share. Paths are relative to the share root; the
// root itself lists as "" (H2), never ".". Junk filtering is the caller's job.
type Share interface {
	Name() string
	// ListDir lists the entries directly under dir ("" for the share root).
	ListDir(dir string) ([]FileInfo, error)
	Stat(path string) (FileInfo, error)
	// OpenReader opens path for reading; the caller streams it with a buffer
	// of at least 512 KiB.
	OpenReader(path string) (io.ReadCloser, error)
	// Delete removes a file or an empty directory. A non-empty directory is
	// refused (IsDirNotEmpty) and left in place. The transport retries a
	// transient sharing violation for directories only; a file held open
	// elsewhere fails at once (IsInUse).
	Delete(path string) error
	// CreateNew creates path exclusively (O_CREATE|O_EXCL, SMB FILE_CREATE) for
	// writing; an existing path reports IsExist and is left untouched. There is
	// deliberately no truncating create: this write primitive cannot destroy an
	// existing file. Each Write re-arms the idle deadline and Close clears it,
	// as OpenReader's reader does for reads.
	CreateNew(path string) (io.WriteCloser, error)
	// Mkdir creates one directory level; an existing name reports IsExist.
	Mkdir(path string) error
	// Rename moves oldPath to newPath on the same share and never replaces an
	// existing newPath: a collision reports IsExist
	// (STATUS_OBJECT_NAME_COLLISION).
	Rename(oldPath, newPath string) error
	// Chtimes sets path's access and modification times (a SET_PATH_INFORMATION
	// basic-info write). A zero time leaves that timestamp unchanged.
	Chtimes(path string, atime, mtime time.Time) error
	// Statfs reports this share's own free and total capacity (design architecture
	// §4).
	Statfs() (free, total int64, err error)
	// VolumeInfo reports the volume label and serial — diagnostic only; the
	// serial is synthetic on some media (H10).
	VolumeInfo() (label, serial string, err error)
	// ReadMarker reads a small identity/media marker. name may be a share-root
	// file or a forward-slash subpath (e.g. "ASIAIR/<marker>"); it is passed
	// straight to the transport. It reports a fs.ErrNotExist-matching error when
	// the marker is absent.
	ReadMarker(name string) ([]byte, error)
	// WriteMarkerAt writes (creating or truncating) a small marker at
	// path.Join(dir, name); dir is "" for the share root. The relocated media
	// marker lives under the removable share's ASIAIR/ folder. That share's root
	// is guest-writable as well: a guest Mkdir there succeeds on a real ASIAIR.
	WriteMarkerAt(dir, name string, data []byte) error
	Umount() error
}

// FileInfo is a single directory entry in the device's clock domain; the
// caller wraps Mtime via clock.DeviceTime.
type FileInfo struct {
	Name  string
	Size  int64
	Mtime time.Time
	IsDir bool
}

// IsNotFound reports whether err denotes a missing file or path. It
// re-dispatches to smb1client's classifier so consumers need not import the
// transport; it also matches the fs.ErrNotExist this package normalises to.
func IsNotFound(err error) bool {
	return err != nil && smb1.IsNotFoundError(err)
}

// IsNetwork reports whether err is a network-level failure (dial failure,
// reset, timeout, closed connection).
func IsNetwork(err error) bool {
	return err != nil && smb1.IsNetworkError(err)
}

// NT_STATUS codes the predicates and the Fake's injected errors carry.
// smb1client keeps its status table internal, so the values are restated here.
const (
	statusObjectNameCollision = 0xC0000035
	statusSharingViolation    = 0xC0000043
	statusDirectoryNotEmpty   = 0xC0000101
)

// IsExist reports whether err denotes a name that already exists: CreateNew,
// Mkdir or a Rename target collision (STATUS_OBJECT_NAME_COLLISION).
func IsExist(err error) bool {
	return err != nil && smb1.IsExistError(err)
}

// IsInUse reports a sharing violation (STATUS_SHARING_VIOLATION): another open
// of the file excludes the operation, which may succeed once that handle
// closes. errors.Is(err, os.ErrPermission) stays true for it, so callers test
// IsInUse before IsPermission.
func IsInUse(err error) bool {
	return err != nil && smb1.IsSharingViolation(err)
}

// IsDirNotEmpty reports a directory delete refused because the directory still
// has entries (STATUS_DIRECTORY_NOT_EMPTY): the *os.PathError wrapping a
// *smb1.ResponseError that smb1client's Remove returns for it.
func IsDirNotEmpty(err error) bool {
	var re *smb1.ResponseError
	return errors.As(err, &re) && re.Code == statusDirectoryNotEmpty
}

// IsPermission reports an access denial. It is true for a sharing violation
// too, which is why IsInUse is tested first.
func IsPermission(err error) bool {
	return err != nil && smb1.IsPermissionError(err)
}
