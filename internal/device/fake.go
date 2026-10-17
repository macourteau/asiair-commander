package device

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	smb1 "github.com/macourteau/smb1client"

	"github.com/macourteau/asiair-commander/internal/clock"
)

// Fake is an in-memory Device for the rig-worker tests. It models a rig as a
// set of shares, each a flat map of relative paths to FakeFile plus a separate
// marker store and its own free/total capacity, with a settable server clock
// and per-operation error injection.
//
// It is safe for concurrent use; the rig worker touches a session from more
// than one goroutine.
type Fake struct {
	mu         sync.Mutex
	reachable  bool
	serverTime clock.ServerTime
	shares     map[string]*fakeShare

	// sessionOpenErr, when set, fails Device.Open (e.g. an unreachable rig
	// that answers TCP but rejects the SMB negotiate).
	sessionOpenErr error

	// OnStat, when set, is invoked (without the fake's lock held) at the start of
	// every Share.Stat, before the value is read. A test can use it to mutate the
	// fake between a caller's successive stats — e.g. to simulate a counter-reset
	// run overwriting a source path between the delete-pass hashes and the delete.
	OnStat func(share, path string)

	// OnListDir, when set, is invoked (without the fake's lock held) at the start of
	// every Share.ListDir, so a test can observe what the worker has published while
	// its listing walk is in progress.
	OnListDir func(share, dir string)

	// OnOpenReader, when set, is invoked (without the fake's lock held) at the start
	// of every Share.OpenReader — each content read a pull or a delete-pass re-hash
	// makes, the calls ContentReads counts. A test can use it to advance a clock per
	// device read and so simulate a slow link.
	OnOpenReader func(share, path string)

	// mutations counts device-mutating calls (Delete, WriteMarkerAt, CreateNew,
	// Mkdir, Rename, Chtimes) issued against any share, so a test can assert an
	// operation that must never touch the device original — frame retargeting,
	// INV-2 — issued zero. Guarded by mu.
	mutations int

	// failNext holds FailNext's count-limited injections, keyed by operation.
	// Guarded by mu.
	failNext map[FakeOp]*pendingFail

	// listMarkers makes ListDir return marker files as a real device lists them
	// (ListMarkers). Guarded by mu.
	listMarkers bool

	// contentReads counts Share.OpenReader calls against any share, so a test can
	// assert a gate that must decide WITHOUT streaming a ~116 MB device copy — the
	// pre-delete gate's deferral, H13 — read nothing. Guarded by mu.
	contentReads int
}

// FakeFile is one entry in a fake share. Size is derived from len(Data); use
// SetSize to resize it (for simulating a file that is still being written).
type FakeFile struct {
	Data  []byte
	Mtime time.Time
	IsDir bool
}

type fakeShare struct {
	files   map[string]*FakeFile
	markers map[string][]byte
	free    int64
	total   int64
	label   string
	serial  string

	errMount       error
	errListDir     error
	errStat        error
	errOpen        error
	errDelete      error
	errStatfs      error
	errWriteMarker error
	errCreateNew   error
	errMkdir       error
	errRename      error
	errChtimes     error
	errReadMarker  map[string]error // keyed by markerKey; a transient marker-read fault
}

// NewFake returns a reachable Fake with no shares.
func NewFake() *Fake {
	return &Fake{
		reachable: true,
		shares:    map[string]*fakeShare{},
	}
}

// FakeOp names an operation FailNext can fail.
type FakeOp string

// The operations FailNext accepts. FakeOpWrite is a Write on a writer
// CreateNew returned, so a test can fail a copy partway through its data.
const (
	FakeOpOpen       FakeOp = "open" // Device.Open
	FakeOpListDir    FakeOp = "listdir"
	FakeOpStat       FakeOp = "stat"
	FakeOpOpenReader FakeOp = "openreader"
	FakeOpCreateNew  FakeOp = "createnew"
	FakeOpWrite      FakeOp = "write"
	FakeOpMkdir      FakeOp = "mkdir"
	FakeOpRename     FakeOp = "rename"
	FakeOpChtimes    FakeOp = "chtimes"
	FakeOpDelete     FakeOp = "delete"
)

type pendingFail struct {
	n   int
	err error
}

// NetworkError builds an injectable error that IsNetwork classifies as a
// network failure.
func NetworkError(msg string) error {
	return fmt.Errorf("%s: %w", msg, net.ErrClosed)
}

// NotFoundError builds an injectable error that IsNotFound classifies as a
// missing file, matching fs.ErrNotExist.
func NotFoundError(msg string) error {
	return fmt.Errorf("%s: %w", msg, fs.ErrNotExist)
}

// SharingViolationError builds STATUS_SHARING_VIOLATION in smb1client's shape
// for a one-path operation such as a delete: an *os.PathError whose cause is
// os.ErrPermission joined with the status as *smb1.ResponseError. IsInUse and
// IsPermission both see it, as they do the device's.
func SharingViolationError(op, path string) error {
	return &os.PathError{Op: op, Path: path, Err: sharingViolation()}
}

// SharingViolationLinkError is SharingViolationError for a two-path operation
// (a rename): smb1client reports those as *os.LinkError.
func SharingViolationLinkError(op, oldPath, newPath string) error {
	return &os.LinkError{Op: op, Old: oldPath, New: newPath, Err: sharingViolation()}
}

func sharingViolation() error {
	return fmt.Errorf("%w: %w", os.ErrPermission, &smb1.ResponseError{Code: statusSharingViolation})
}

// DirNotEmptyError builds the refusal smb1client's Remove returns for a
// non-empty directory: an *os.PathError wrapping *smb1.ResponseError with
// STATUS_DIRECTORY_NOT_EMPTY.
func DirNotEmptyError(path string) error {
	return &os.PathError{Op: "remove", Path: path, Err: &smb1.ResponseError{Code: statusDirectoryNotEmpty}}
}

// existError is the collision the device answers a CreateNew or Mkdir onto an
// existing name with.
func existError(op, path string) error {
	return &os.PathError{Op: op, Path: path, Err: &smb1.ResponseError{Code: statusObjectNameCollision}}
}

// normPath normalises a fake path: backslashes to forward slashes, trimmed of
// surrounding whitespace and slashes. The empty string is the share root.
func normPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimSpace(p)
	return strings.Trim(p, "/")
}

// markerKey is the markers-map key for a marker at path.Join(dir, name). Reads,
// writes, and deletes all normalise through it so a marker relocated into a
// subfolder (ASIAIR/) round-trips against the same forward-slash key.
func markerKey(dir, name string) string {
	return normPath(path.Join(dir, name))
}

// share returns the named share, creating it (with default capacity) if absent.
// Caller holds f.mu.
func (f *Fake) share(name string) *fakeShare {
	s := f.shares[name]
	if s == nil {
		s = &fakeShare{
			files:   map[string]*FakeFile{},
			markers: map[string][]byte{},
		}
		f.shares[name] = s
	}
	return s
}

// --- test-driving API -------------------------------------------------------

// SetReachable sets the Probe result.
func (f *Fake) SetReachable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reachable = v
}

// Mutations returns the count of device-mutating calls (Delete, WriteMarkerAt,
// CreateNew, Mkdir, Rename, Chtimes) issued against any share so far, failed
// ones included.
func (f *Fake) Mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutations
}

// ContentReads returns the count of Share.OpenReader calls issued against any
// share so far.
func (f *Fake) ContentReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contentReads
}

// SetServerTime sets the clock returned at Open.
func (f *Fake) SetServerTime(st clock.ServerTime) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverTime = st
}

// AddShare ensures a (possibly empty) share exists — an empty share mounts and
// lists nothing, the H5 missing-USB-stick case.
func (f *Fake) AddShare(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(name)
}

// AddFile adds (or overwrites) a file, creating the share if needed. It stores
// a copy of data.
func (f *Fake) AddFile(share, path string, data []byte, mtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	buf := append([]byte(nil), data...)
	f.share(share).files[normPath(path)] = &FakeFile{Data: buf, Mtime: mtime}
}

// AddDir adds an explicit directory entry, creating the share if needed.
func (f *Fake) AddDir(share, path string, mtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).files[normPath(path)] = &FakeFile{Mtime: mtime, IsDir: true}
}

// RemoveFile deletes a file from the share if present.
func (f *Fake) RemoveFile(share, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.shares[share]; s != nil {
		delete(s.files, normPath(path))
	}
}

// SetMtime updates a file's modification time — advance it to simulate a file
// still being written.
func (f *Fake) SetMtime(share, path string, mtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if file := f.fileLocked(share, path); file != nil {
		file.Mtime = mtime
	}
}

// SetSize resizes a file's data buffer, growing it with zero bytes or
// truncating it, so its reported Size changes — the other half of simulating a
// file mid-write.
func (f *Fake) SetSize(share, path string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file := f.fileLocked(share, path)
	if file == nil {
		return
	}
	switch {
	case size < 0:
		return
	case int64(len(file.Data)) >= size:
		file.Data = file.Data[:size]
	default:
		file.Data = append(file.Data, make([]byte, size-int64(len(file.Data)))...)
	}
}

func (f *Fake) fileLocked(share, path string) *FakeFile {
	s := f.shares[share]
	if s == nil {
		return nil
	}
	return s.files[normPath(path)]
}

// SetStatfs sets a share's free and total capacity.
func (f *Fake) SetStatfs(share string, free, total int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.share(share)
	s.free, s.total = free, total
}

// SetVolumeInfo sets a share's volume label and serial.
func (f *Fake) SetVolumeInfo(share, label, serial string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.share(share)
	s.label, s.serial = label, serial
}

// SetMarker seeds a share's marker store directly. name may be a share-root
// marker or a forward-slash subpath (e.g. "ASIAIR/<marker>").
func (f *Fake) SetMarker(share, name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).markers[markerKey("", name)] = append([]byte(nil), data...)
}

// FailSessionOpen makes Device.Open return err (nil clears it).
func (f *Fake) FailSessionOpen(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionOpenErr = err
}

// FailMount makes Mount(share) return err.
func (f *Fake) FailMount(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errMount = err
}

// FailListDir makes ListDir on the share return err.
func (f *Fake) FailListDir(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errListDir = err
}

// FailStat makes Stat on the share return err.
func (f *Fake) FailStat(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errStat = err
}

// FailOpen makes OpenReader on the share return err.
func (f *Fake) FailOpen(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errOpen = err
}

// FailDelete makes Delete on the share return err on every call — used to
// exercise the caller's sharing-violation retry budget.
func (f *Fake) FailDelete(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errDelete = err
}

// FailStatfs makes Statfs on the share return err.
func (f *Fake) FailStatfs(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errStatfs = err
}

// FailWriteMarker makes WriteMarkerAt on the share return err — used to exercise
// a read-only removable root where a marker write is refused.
func (f *Fake) FailWriteMarker(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errWriteMarker = err
}

// FailReadMarker makes ReadMarker for one marker location (name may be a
// forward-slash subpath such as "ASIAIR/<marker>") return err — used to exercise
// a transient marker read that must not fall through to a different location.
func (f *Fake) FailReadMarker(share, name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.share(share)
	if s.errReadMarker == nil {
		s.errReadMarker = map[string]error{}
	}
	s.errReadMarker[markerKey("", name)] = err
}

// FailCreateNew makes CreateNew on the share return err.
func (f *Fake) FailCreateNew(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errCreateNew = err
}

// FailMkdir makes Mkdir on the share return err.
func (f *Fake) FailMkdir(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errMkdir = err
}

// FailRename makes Rename on the share return err.
func (f *Fake) FailRename(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errRename = err
}

// FailChtimes makes Chtimes on the share return err.
func (f *Fake) FailChtimes(share string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.share(share).errChtimes = err
}

// FailNext makes the next n calls of op, on any share, return err; the call
// after them behaves normally again. It takes precedence over the persistent
// Fail* hooks while it lasts. Arming op again replaces its pending count, and
// n <= 0 clears it. A failed call has no effect on the fake's contents.
func (f *Fake) FailNext(op FakeOp, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= 0 {
		delete(f.failNext, op)
		return
	}
	if f.failNext == nil {
		f.failNext = map[FakeOp]*pendingFail{}
	}
	f.failNext[op] = &pendingFail{n: n, err: err}
}

// takeFailNext consumes one pending FailNext injection for op, if any. Caller
// holds f.mu.
func (f *Fake) takeFailNext(op FakeOp) error {
	p := f.failNext[op]
	if p == nil {
		return nil
	}
	p.n--
	if p.n <= 0 {
		delete(f.failNext, op)
	}
	return p.err
}

// ListMarkers makes ListDir return marker files, and the folders that hold
// them, as a real device lists them. It is opt-in because the fake derives
// implicit folders from stored paths: a listed ASIAIR/<media marker> would
// create an ASIAIR/ folder that changes what the worker's media checks see.
func (f *Fake) ListMarkers(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listMarkers = v
}

// --- Device / Session / Share implementation --------------------------------

func (f *Fake) Probe(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reachable
}

func (f *Fake) Open(context.Context) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.takeFailNext(FakeOpOpen); err != nil {
		return nil, err
	}
	if f.sessionOpenErr != nil {
		return nil, f.sessionOpenErr
	}
	return &fakeSession{f: f}, nil
}

type fakeSession struct {
	f *Fake
}

func (s *fakeSession) ServerTime() clock.ServerTime {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.serverTime
}

func (s *fakeSession) ListShares() ([]string, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	names := make([]string, 0, len(s.f.shares))
	for name := range s.f.shares {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *fakeSession) Mount(share string) (Share, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	sh := s.f.shares[share]
	if sh == nil {
		return nil, NotFoundError("mount " + share)
	}
	if sh.errMount != nil {
		return nil, sh.errMount
	}
	return &fakeShareHandle{f: s.f, name: share}, nil
}

func (s *fakeSession) Close() error { return nil }

type fakeShareHandle struct {
	f    *Fake
	name string
}

func (h *fakeShareHandle) Name() string { return h.name }

func (h *fakeShareHandle) data() *fakeShare {
	return h.f.shares[h.name]
}

func (h *fakeShareHandle) ListDir(dir string) ([]FileInfo, error) {
	if h.f.OnListDir != nil {
		h.f.OnListDir(h.name, dir)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.data()
	if s == nil {
		return nil, NotFoundError("listdir " + h.name)
	}
	if err := h.f.takeFailNext(FakeOpListDir); err != nil {
		return nil, err
	}
	if s.errListDir != nil {
		return nil, s.errListDir
	}
	dir = normPath(dir)

	// A non-root directory that is neither a stored entry nor an ancestor of any
	// stored path is genuinely absent: SMB answers readdir with a not-found, so the
	// fake must too. Modelling this is what lets a caller distinguish a missing path
	// from an empty directory (the share root always exists and lists empty).
	exists := s.dirExists(dir)
	if !exists && h.f.listMarkers {
		exists = s.hasMarkerBelow(dir)
	}
	if dir != "" && !exists {
		return nil, NotFoundError("listdir " + h.name + "/" + dir)
	}

	// Collect immediate children, synthesising intermediate directories that
	// exist only implicitly through a deeper file path.
	children := map[string]FileInfo{}
	for p, file := range s.files {
		var rel string
		switch {
		case dir == "":
			rel = p
		case p == dir:
			continue
		case strings.HasPrefix(p, dir+"/"):
			rel = p[len(dir)+1:]
		default:
			continue
		}
		if rel == "" {
			continue
		}
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			// An implicit directory; a real entry (below) overrides it.
			name := rel[:i]
			if _, ok := children[name]; !ok {
				children[name] = FileInfo{Name: name, IsDir: true}
			}
			continue
		}
		children[rel] = FileInfo{
			Name:  rel,
			Size:  int64(len(file.Data)),
			Mtime: file.Mtime,
			IsDir: file.IsDir,
		}
	}

	if h.f.listMarkers {
		for p, data := range s.markers {
			rel, ok := relUnder(dir, p)
			if !ok {
				continue
			}
			if i := strings.IndexByte(rel, '/'); i >= 0 {
				name := rel[:i]
				if _, ok := children[name]; !ok {
					children[name] = FileInfo{Name: name, IsDir: true}
				}
				continue
			}
			children[rel] = FileInfo{Name: rel, Size: int64(len(data))}
		}
	}

	out := make([]FileInfo, 0, len(children))
	for _, fi := range children {
		out = append(out, fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// relUnder returns p relative to dir when p lies strictly below it; dir "" is
// the share root.
func relUnder(dir, p string) (string, bool) {
	switch {
	case dir == "":
		return p, p != ""
	case strings.HasPrefix(p, dir+"/"):
		return p[len(dir)+1:], true
	}
	return "", false
}

// hasMarkerBelow reports whether a marker lives somewhere under dir. Caller
// holds f.mu.
func (s *fakeShare) hasMarkerBelow(dir string) bool {
	for p := range s.markers {
		if _, ok := relUnder(dir, p); ok {
			return true
		}
	}
	return false
}

// hasContent reports whether directory dir holds anything a device would list:
// a stored entry or a marker below it. A marker is a real file on the device,
// so it counts whether or not ListMarkers is on. Caller holds f.mu.
func (s *fakeShare) hasContent(dir string) bool {
	for p := range s.files {
		if _, ok := relUnder(dir, p); ok {
			return true
		}
	}
	return s.hasMarkerBelow(dir)
}

// occupied reports whether any file, directory (explicit or implied by a
// deeper path) or marker answers to p — the names the device refuses to create
// or rename onto. Caller holds f.mu.
func (s *fakeShare) occupied(p string) bool {
	if _, ok := s.files[p]; ok {
		return true
	}
	if _, ok := s.markers[p]; ok {
		return true
	}
	return s.hasContent(p)
}

// parentIsDir reports whether p's parent is a directory a new entry can be
// created in: the share root, an explicit directory, or one implied by a deeper
// path or marker. Caller holds f.mu.
func (s *fakeShare) parentIsDir(p string) bool {
	parent := path.Dir(p)
	if parent == "." {
		return true
	}
	if f, ok := s.files[parent]; ok {
		return f.IsDir
	}
	return s.hasContent(parent)
}

// dirExists reports whether dir names an existing directory on the share: an
// explicit directory entry, or an ancestor of some stored path (an implicit
// directory). A stored non-directory file at dir is not a directory. Caller holds
// f.mu.
func (s *fakeShare) dirExists(dir string) bool {
	if f, ok := s.files[dir]; ok {
		return f.IsDir
	}
	for p := range s.files {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func (h *fakeShareHandle) Stat(path string) (FileInfo, error) {
	if h.f.OnStat != nil {
		h.f.OnStat(h.name, path)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.data()
	if s == nil {
		return FileInfo{}, NotFoundError("stat " + path)
	}
	if err := h.f.takeFailNext(FakeOpStat); err != nil {
		return FileInfo{}, err
	}
	if s.errStat != nil {
		return FileInfo{}, s.errStat
	}
	np := normPath(path)
	if file, ok := s.files[np]; ok {
		return FileInfo{
			Name:  baseName(np),
			Size:  int64(len(file.Data)),
			Mtime: file.Mtime,
			IsDir: file.IsDir,
		}, nil
	}
	// An implicit directory: some file lives beneath np.
	for p := range s.files {
		if strings.HasPrefix(p, np+"/") {
			return FileInfo{Name: baseName(np), IsDir: true}, nil
		}
	}
	return FileInfo{}, NotFoundError("stat " + path)
}

func (h *fakeShareHandle) OpenReader(path string) (io.ReadCloser, error) {
	if h.f.OnOpenReader != nil {
		h.f.OnOpenReader(h.name, path)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.contentReads++
	s := h.data()
	if s == nil {
		return nil, NotFoundError("open " + path)
	}
	if err := h.f.takeFailNext(FakeOpOpenReader); err != nil {
		return nil, err
	}
	if s.errOpen != nil {
		return nil, s.errOpen
	}
	file, ok := s.files[normPath(path)]
	if !ok || file.IsDir {
		return nil, NotFoundError("open " + path)
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), file.Data...))), nil
}

func (h *fakeShareHandle) Delete(path string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.mutations++
	s := h.data()
	if s == nil {
		return NotFoundError("delete " + path)
	}
	if err := h.f.takeFailNext(FakeOpDelete); err != nil {
		return err
	}
	if s.errDelete != nil {
		return s.errDelete
	}
	np := normPath(path)
	// A directory with anything below it, a marker included, is refused in the
	// device's shape and left in place, as smb1client's Remove does.
	if file, ok := s.files[np]; ok {
		if file.IsDir && s.hasContent(np) {
			return DirNotEmptyError(np)
		}
		delete(s.files, np)
		return nil
	}
	// A marker is a real file on the device, so Delete removes it too — the fake
	// keeps markers in a separate map for read/write isolation. np is already the
	// markerKey form (normPath), so a subfolder marker (ASIAIR/) matches.
	if _, ok := s.markers[np]; ok {
		delete(s.markers, np)
		return nil
	}
	// An implicit directory exists only through its contents, so it is never
	// empty.
	if np != "" && s.hasContent(np) {
		return DirNotEmptyError(np)
	}
	return NotFoundError("delete " + path)
}

// CreateNew stores an empty file at once, as the device's FILE_CREATE does, so
// a concurrent CreateNew of the same name collides; Writes append to it.
// Creation stamps the fake's server time as the mtime.
func (h *fakeShareHandle) CreateNew(p string) (io.WriteCloser, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.mutations++
	s := h.data()
	if s == nil {
		return nil, NotFoundError("createnew " + p)
	}
	if err := h.f.takeFailNext(FakeOpCreateNew); err != nil {
		return nil, err
	}
	if s.errCreateNew != nil {
		return nil, s.errCreateNew
	}
	np := normPath(p)
	if np == "" || !s.parentIsDir(np) {
		return nil, NotFoundError("createnew " + p)
	}
	if s.occupied(np) {
		return nil, existError("open", np)
	}
	s.files[np] = &FakeFile{Mtime: h.f.serverTime.Time}
	return &fakeWriter{h: h, path: np}, nil
}

// fakeWriter appends to the file CreateNew stored. Like an SMB handle it keeps
// writing to that file object, so a Write after the path was deleted out from
// under it fails rather than resurrecting it.
type fakeWriter struct {
	h      *fakeShareHandle
	path   string
	closed bool
}

func (w *fakeWriter) Write(b []byte) (int, error) {
	w.h.f.mu.Lock()
	defer w.h.f.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if err := w.h.f.takeFailNext(FakeOpWrite); err != nil {
		return 0, err
	}
	s := w.h.data()
	if s == nil {
		return 0, NotFoundError("write " + w.path)
	}
	file, ok := s.files[w.path]
	if !ok || file.IsDir {
		return 0, NotFoundError("write " + w.path)
	}
	file.Data = append(file.Data, b...)
	return len(b), nil
}

func (w *fakeWriter) Close() error {
	w.h.f.mu.Lock()
	defer w.h.f.mu.Unlock()
	if w.closed {
		return os.ErrClosed
	}
	w.closed = true
	return nil
}

// Mkdir stores an explicit directory, stamped with the fake's server time.
func (h *fakeShareHandle) Mkdir(p string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.mutations++
	s := h.data()
	if s == nil {
		return NotFoundError("mkdir " + p)
	}
	if err := h.f.takeFailNext(FakeOpMkdir); err != nil {
		return err
	}
	if s.errMkdir != nil {
		return s.errMkdir
	}
	np := normPath(p)
	if np == "" || !s.parentIsDir(np) {
		return NotFoundError("mkdir " + p)
	}
	if s.occupied(np) {
		return existError("mkdir", np)
	}
	s.files[np] = &FakeFile{Mtime: h.f.serverTime.Time, IsDir: true}
	return nil
}

// Rename moves a file, or a directory with everything below it (markers
// included), and never replaces: any entry at newPath is a collision.
func (h *fakeShareHandle) Rename(oldPath, newPath string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.mutations++
	s := h.data()
	notFound := &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: fs.ErrNotExist}
	if s == nil {
		return notFound
	}
	if err := h.f.takeFailNext(FakeOpRename); err != nil {
		return err
	}
	if s.errRename != nil {
		return s.errRename
	}
	src, dst := normPath(oldPath), normPath(newPath)
	_, explicit := s.files[src]
	isDir := !explicit && s.hasContent(src)
	if src == "" || dst == "" || (!explicit && !isDir) || !s.parentIsDir(dst) {
		return notFound
	}
	if s.occupied(dst) {
		return &os.LinkError{Op: "rename", Old: src, New: dst,
			Err: &smb1.ResponseError{Code: statusObjectNameCollision}}
	}
	if _, ok := relUnder(src, dst); ok {
		// A directory cannot move into its own subtree.
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: os.ErrInvalid}
	}
	files := map[string]*FakeFile{}
	for p, file := range s.files {
		if p == src {
			files[dst] = file
		} else if rel, ok := relUnder(src, p); ok {
			files[dst+"/"+rel] = file
		} else {
			continue
		}
		delete(s.files, p)
	}
	for p, file := range files {
		s.files[p] = file
	}
	markers := map[string][]byte{}
	for p, data := range s.markers {
		if rel, ok := relUnder(src, p); ok {
			markers[dst+"/"+rel] = data
			delete(s.markers, p)
		}
	}
	for p, data := range markers {
		s.markers[p] = data
	}
	return nil
}

// Chtimes sets the stored mtime that Stat and ListDir report; the fake keeps
// no access time. A zero mtime leaves it unchanged, as on the device. An
// implicit directory becomes an explicit one so it can carry the time.
func (h *fakeShareHandle) Chtimes(p string, _, mtime time.Time) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.mutations++
	s := h.data()
	if s == nil {
		return NotFoundError("chtimes " + p)
	}
	if err := h.f.takeFailNext(FakeOpChtimes); err != nil {
		return err
	}
	if s.errChtimes != nil {
		return s.errChtimes
	}
	np := normPath(p)
	file, ok := s.files[np]
	if !ok {
		if np == "" || !s.hasContent(np) {
			return NotFoundError("chtimes " + p)
		}
		file = &FakeFile{IsDir: true}
		s.files[np] = file
	}
	if !mtime.IsZero() {
		file.Mtime = mtime
	}
	return nil
}

func (h *fakeShareHandle) Statfs() (free, total int64, err error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.data()
	if s == nil {
		return 0, 0, NotFoundError("statfs " + h.name)
	}
	if s.errStatfs != nil {
		return 0, 0, s.errStatfs
	}
	return s.free, s.total, nil
}

func (h *fakeShareHandle) VolumeInfo() (label, serial string, err error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.data()
	if s == nil {
		return "", "", NotFoundError("volumeinfo " + h.name)
	}
	return s.label, s.serial, nil
}

func (h *fakeShareHandle) ReadMarker(name string) ([]byte, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.data()
	if s == nil {
		return nil, NotFoundError("readmarker " + name)
	}
	if err := s.errReadMarker[markerKey("", name)]; err != nil {
		return nil, err
	}
	data, ok := s.markers[markerKey("", name)]
	if !ok {
		return nil, NotFoundError("readmarker " + name)
	}
	return append([]byte(nil), data...), nil
}

func (h *fakeShareHandle) WriteMarkerAt(dir, name string, data []byte) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.mutations++
	s := h.data()
	if s == nil {
		return NotFoundError("writemarker " + path.Join(dir, name))
	}
	if s.errWriteMarker != nil {
		return s.errWriteMarker
	}
	s.markers[markerKey(dir, name)] = append([]byte(nil), data...)
	return nil
}

func (h *fakeShareHandle) Umount() error { return nil }

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Ensure the fake implements the seam.
var (
	_ Device  = (*Fake)(nil)
	_ Session = (*fakeSession)(nil)
	_ Share   = (*fakeShareHandle)(nil)
)
