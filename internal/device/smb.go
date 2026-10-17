package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"
	"time"

	smb1 "github.com/macourteau/smb1client"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// defaultTimeout is the base of fallbackTimeout, which bounds the TCP connect
// for both Probe and Open, and each operation, when the caller supplies no
// timeout.
const defaultTimeout = 3 * time.Second

// SMBDevice is the production Device: it dials one rig over smb1client with the
// configured (typically guest) credentials, through the site its endpoint names.
type SMBDevice struct {
	sites   netsite.Sites
	ep      netsite.Endpoint
	user    string
	pass    string
	timeout time.Duration
}

// New builds a Device for ep, dialled through ep's site in sites,
// authenticating as user/pass. The guest default (guest / 12345678, H11) is
// applied by the caller's config, not here. timeout bounds the TCP connect;
// zero means fallbackTimeout: defaultTimeout plus the site's 1 round-trip
// margin.
func New(sites netsite.Sites, ep netsite.Endpoint, user, pass string, timeout time.Duration) Device {
	return &SMBDevice{sites: sites, ep: ep, user: user, pass: pass, timeout: timeout}
}

// NewFactory binds New to sites, giving the factory shape the rig worker and
// discovery take.
func NewFactory(sites netsite.Sites) func(ep netsite.Endpoint, user, pass string, timeout time.Duration) Device {
	return func(ep netsite.Endpoint, user, pass string, timeout time.Duration) Device {
		return New(sites, ep, user, pass, timeout)
	}
}

// dial opens TCP/445 through the endpoint's site. The site is looked up at
// each dial, so a site removed or gone down since the device was built fails
// here rather than reaching any network.
func (d *SMBDevice) dial(ctx context.Context) (net.Conn, error) {
	return d.sites.Site(d.ep.Site).DialTCP(ctx, d.ep.IP, netsite.SMBPort, d.dialTimeout())
}

func (d *SMBDevice) dialTimeout() time.Duration {
	if d.timeout == 0 {
		return d.fallbackTimeout()
	}
	return d.timeout
}

// opTimeout bounds each post-connect SMB operation. It reuses the device's
// configured timeout (the rig's smbTimeout), falling back to fallbackTimeout.
func (d *SMBDevice) opTimeout() time.Duration {
	if d.timeout == 0 {
		return d.fallbackTimeout()
	}
	return d.timeout
}

// fallbackTimeout is defaultTimeout plus 1 round-trip margin of the endpoint's
// site, covering the connect or the one request and reply it bounds;
// defaultTimeout exactly on the local site. A caller passing a timeout has
// already added its own margins.
func (d *SMBDevice) fallbackTimeout() time.Duration {
	return defaultTimeout + netsite.RoundTrips(d.sites.Site(d.ep.Site), 1)
}

// Probe checks TCP/445 reachability with a short timeout. It avoids the long
// failure paths inside the SMB stack when the rig is simply powered off (design
// frame-lifecycle §1).
func (d *SMBDevice) Probe(ctx context.Context) bool {
	conn, err := d.dial(ctx)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Open dials TCP/445 and authenticates via NTLMv2, capturing the server clock
// at negotiate. On any failure past the TCP dial it closes the connection so
// no socket leaks (design frame-lifecycle §2).
func (d *SMBDevice) Open(ctx context.Context) (Session, error) {
	conn, err := d.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", d.ep.IP, err)
	}
	// AllowGuest: the ASIAIR presents its removable media as a public share. It
	// answers the documented guest/12345678 login by granting a guest session
	// rather than by authenticating that name, so a dialer that refuses guest
	// logons fails every cycle and the rig reads as offline.
	sd := &smb1.Dialer{
		Initiator:  &smb1.NTLMInitiator{User: d.user, Password: d.pass},
		AllowGuest: true,
	}
	sess, err := sd.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMB authentication against %s: %w", d.ep.IP, err)
	}
	st := sess.ServerTime()
	return &smbSession{
		sess:       sess,
		conn:       conn,
		opTimeout:  d.opTimeout(),
		serverTime: clock.ServerTime{Time: st.Time, ReceivedAt: st.ReceivedAt},
	}, nil
}

type smbSession struct {
	sess       *smb1.Session
	conn       net.Conn
	opTimeout  time.Duration
	serverTime clock.ServerTime
	mounted    []*smbShare
}

func (s *smbSession) ServerTime() clock.ServerTime { return s.serverTime }

// ListShares enumerates the server's shares and keeps only storage shares:
// IPC$, print$, and any other administrative ($-suffixed) share are dropped.
func (s *smbSession) ListShares() ([]string, error) {
	names, err := s.sess.ListSharenames()
	if err != nil {
		return nil, err
	}
	storage := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasSuffix(name, "$") {
			continue
		}
		storage = append(storage, name)
	}
	return storage, nil
}

func (s *smbSession) Mount(share string) (Share, error) {
	sh, err := s.sess.Mount(share)
	if err != nil {
		return nil, adaptErr(err)
	}
	wrapped := &smbShare{name: share, share: sh, conn: s.conn, opTimeout: s.opTimeout}
	s.mounted = append(s.mounted, wrapped)
	return wrapped, nil
}

// Close umounts every share opened through this session and logs off. It
// reports the first error but always attempts the whole teardown, closing the
// underlying connection via Logoff (design frame-lifecycle §10). Each request
// is bounded by the operation timeout, so a silent device cannot hold the
// teardown; the connection is closed at the end, so the deadline needs no
// clearing.
func (s *smbSession) Close() error {
	arm := func() { _ = s.conn.SetDeadline(time.Now().Add(s.opTimeout)) }
	var firstErr error
	for _, sh := range s.mounted {
		arm()
		if err := sh.share.Umount(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.mounted = nil
	arm()
	if err := s.sess.Logoff(); err != nil && firstErr == nil {
		firstErr = err
	}
	// Always close the TCP connection as a backstop. A failed Logoff (a wedged
	// link) is exactly when Logoff would not have closed the socket, so without
	// this the connection leaks once per cycle per rig. A successful Logoff has
	// already closed it, so an "already closed" error here is the normal path and
	// must not be surfaced.
	if err := s.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// smbShare adapts *smb1.Share to the Share seam. It carries the session's
// connection and per-operation timeout so each op can bound itself against a
// half-dead link (link up, packets dropped) instead of blocking on TCP
// keepalive — which would hang the worker goroutine and service shutdown.
type smbShare struct {
	name      string
	share     *smb1.Share
	conn      net.Conn
	opTimeout time.Duration
}

func (o *smbShare) Name() string { return o.name }

func (o *smbShare) ListDir(dir string) ([]FileInfo, error) {
	var entries []os.FileInfo
	err := o.withDeadline(func() (e error) { entries, e = o.share.ReadDir(dir); return })
	if err != nil {
		return nil, adaptErr(err)
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, FileInfo{
			Name:  e.Name(),
			Size:  e.Size(),
			Mtime: e.ModTime(),
			IsDir: e.IsDir(),
		})
	}
	return out, nil
}

func (o *smbShare) Stat(path string) (FileInfo, error) {
	var fi os.FileInfo
	err := o.withDeadline(func() (e error) { fi, e = o.share.Stat(path); return })
	if err != nil {
		return FileInfo{}, adaptErr(err)
	}
	return FileInfo{
		Name:  fi.Name(),
		Size:  fi.Size(),
		Mtime: fi.ModTime(),
		IsDir: fi.IsDir(),
	}, nil
}

// OpenReader bounds the open call, then wraps the file so each Read re-arms an
// idle deadline: the bulk transfer is not killed by a single fixed deadline,
// but a stall still errors out. The deadline is cleared on Close.
func (o *smbShare) OpenReader(path string) (io.ReadCloser, error) {
	var f *smb1.File
	err := o.withDeadline(func() (e error) { f, e = o.share.Open(path); return })
	if err != nil {
		return nil, adaptErr(err)
	}
	if o.conn == nil {
		return f, nil
	}
	return &deadlineReader{r: f, closer: f, setDeadline: o.conn.SetDeadline, timeout: o.opTimeout}, nil
}

// Delete removes a file or an empty directory. smb1client sends a directory
// through SMB_COM_DELETE_DIRECTORY, so a non-empty one comes back as
// STATUS_DIRECTORY_NOT_EMPTY (IsDirNotEmpty) rather than a reported success,
// and retries a sharing violation for directories only, for about 5 s; a file
// delete is not retried.
func (o *smbShare) Delete(path string) error {
	return adaptErr(o.withDeadline(func() error { return o.share.Remove(path) }))
}

// CreateNew opens path with FILE_CREATE (O_CREATE|O_EXCL), so an existing file
// is never truncated: the server refuses with STATUS_OBJECT_NAME_COLLISION. The
// open is bounded like any short op, then the file is wrapped so each Write
// re-arms the idle deadline and Close clears it. O_RDWR is the access mode verified against a
// real ASIAIR.
func (o *smbShare) CreateNew(path string) (io.WriteCloser, error) {
	var f *smb1.File
	err := o.withDeadline(func() (e error) {
		f, e = o.share.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		return
	})
	if err != nil {
		return nil, adaptErr(err)
	}
	if o.conn == nil {
		return f, nil
	}
	return &deadlineWriter{w: f, closer: f, setDeadline: o.conn.SetDeadline, timeout: o.opTimeout}, nil
}

func (o *smbShare) Mkdir(path string) error {
	return adaptErr(o.withDeadline(func() error { return o.share.Mkdir(path, 0o755) }))
}

// Rename relies on SMB_COM_RENAME's own refusal to replace an existing target;
// smb1client does not emulate os.Rename's replace semantics.
func (o *smbShare) Rename(oldPath, newPath string) error {
	return adaptErr(o.withDeadline(func() error { return o.share.Rename(oldPath, newPath) }))
}

func (o *smbShare) Chtimes(path string, atime, mtime time.Time) error {
	return adaptErr(o.withDeadline(func() error { return o.share.Chtimes(path, atime, mtime) }))
}

func (o *smbShare) Statfs() (free, total int64, err error) {
	var info smb1.FileFsInfo
	err = o.withDeadline(func() (e error) { info, e = o.share.Statfs(""); return })
	if err != nil {
		return 0, 0, err
	}
	// Statfs reports allocation-unit counts; a unit is FragmentSize (sectors per
	// unit) × BlockSize (bytes per sector).
	unit := info.FragmentSize() * info.BlockSize()
	return int64(info.FreeBlockCount() * unit), int64(info.TotalBlockCount() * unit), nil
}

func (o *smbShare) VolumeInfo() (label, serial string, err error) {
	var info *smb1.VolumeInfo
	err = o.withDeadline(func() (e error) { info, e = o.share.VolumeInfo(); return })
	if err != nil {
		return "", "", err
	}
	return info.Label, fmt.Sprintf("%08X", info.SerialNumber), nil
}

// ReadMarker reads a small marker file at the share root, returning a
// fs.ErrNotExist-matching error when it is absent so mediaid can classify the
// AbsentMarker verdict.
func (o *smbShare) ReadMarker(name string) ([]byte, error) {
	// Marker files are tiny, so the whole open/read/close runs under one
	// deadline window rather than the streaming idle deadline.
	var data []byte
	err := o.withDeadline(func() error {
		f, err := o.share.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err = io.ReadAll(f)
		return err
	})
	if err != nil {
		return nil, adaptErr(err)
	}
	return data, nil
}

// WriteMarkerAt writes (creating or truncating) a small marker file at
// path.Join(dir, name) — the share root when dir is "", or a subfolder such as
// the removable share's ASIAIR/. Verified against a real ASIAIR (Samba 4.9.5,
// guest, SMB1): Create on ASIAIR/<name> succeeds through smb1client with a
// forward slash, matching the existing recursive walk's path convention.
func (o *smbShare) WriteMarkerAt(dir, name string, data []byte) error {
	return adaptErr(o.withDeadline(func() error {
		f, err := o.share.Create(path.Join(dir, name))
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}))
}

func (o *smbShare) Umount() error {
	return o.withDeadline(o.share.Umount)
}

// withDeadline bounds a short SMB operation with an absolute connection
// deadline, clearing it afterward so a later op is not tripped by a stale one.
// Operations on one session run sequentially in a single worker goroutine, so
// sharing the connection's deadline across ops is safe. A nil conn (only in
// tests that construct smbShare directly) disables bounding.
func (o *smbShare) withDeadline(fn func() error) error {
	if o.conn != nil {
		_ = o.conn.SetDeadline(time.Now().Add(o.opTimeout))
		defer o.conn.SetDeadline(time.Time{})
	}
	return fn()
}

// deadlineReader wraps a streaming SMB file so each Read re-arms an idle
// deadline on the shared connection: a stall (no bytes for timeout) errors out,
// while a slow-but-progressing pull keeps extending its window. Close clears the
// deadline and closes the underlying file. A nil setDeadline disables bounding.
type deadlineReader struct {
	r           io.Reader
	closer      io.Closer
	setDeadline func(time.Time) error
	timeout     time.Duration
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.setDeadline != nil {
		_ = d.setDeadline(time.Now().Add(d.timeout))
	}
	return d.r.Read(p)
}

func (d *deadlineReader) Close() error {
	if d.setDeadline != nil {
		_ = d.setDeadline(time.Time{})
	}
	if d.closer != nil {
		return d.closer.Close()
	}
	return nil
}

// deadlineWriter is deadlineReader's write-side twin: each Write re-arms an
// idle deadline on the shared connection, so a stalled upload errors out while
// a progressing one keeps extending its window. Close bounds the file's SMB
// CLOSE by the same idle deadline, then clears it. A nil setDeadline disables
// bounding.
type deadlineWriter struct {
	w           io.Writer
	closer      io.Closer
	setDeadline func(time.Time) error
	timeout     time.Duration
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.setDeadline != nil {
		_ = d.setDeadline(time.Now().Add(d.timeout))
	}
	return d.w.Write(p)
}

func (d *deadlineWriter) Close() error {
	if d.setDeadline != nil {
		// The CLOSE is a request and reply like any write: unbounded, a silent
		// device would hold the copy, and the operation lock, forever.
		_ = d.setDeadline(time.Now().Add(d.timeout))
		defer d.setDeadline(time.Time{})
	}
	if d.closer != nil {
		return d.closer.Close()
	}
	return nil
}

// adaptErr normalises smb1client's not-found shapes onto fs.ErrNotExist — the
// seam's contract, which mediaid and the frame lifecycle classify against with
// errors.Is. Not every smb1client operation maps its NT_STATUS not-found codes
// to os.ErrNotExist itself, and consumers cannot import smb1client to ask.
func adaptErr(err error) error {
	if err == nil || errors.Is(err, fs.ErrNotExist) || !smb1.IsNotFoundError(err) {
		return err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return &os.PathError{Op: pe.Op, Path: pe.Path, Err: fs.ErrNotExist}
	}
	return fs.ErrNotExist
}
