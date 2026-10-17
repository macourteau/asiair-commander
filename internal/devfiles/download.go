package devfiles

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// downloadBufSize is the read size for streaming a device file: the device
// seam asks for at least 512 KiB, since each read is a round trip.
const downloadBufSize = 512 << 10

// ErrMidStream marks a download that failed after its first byte reached the
// DownloadWriter. The response can no longer carry an error body or be
// retried, so the handler must abort it (panic(http.ErrAbortHandler)) for the
// browser to mark the download failed rather than save a truncated file. The
// *Error carrying it also unwraps to the underlying cause's sentinel, so the
// handler tests ErrMidStream first.
var ErrMidStream = errors.New("devfiles: download failed after its first byte")

// DownloadInfo describes a download: the name the browser saves it under, the
// files and bytes it carries, and whether it is a zip. For a single file Bytes
// is the exact body length (Content-Length); for a zip it is the files' total.
type DownloadInfo struct {
	Filename string `json:"filename"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Zipped   bool   `json:"zipped"`
}

// DownloadWriter receives a download's body. Begin is called exactly once,
// just before the first Write, with the response's metadata, so the handler
// sets its headers there; an error returned before Begin means nothing was
// sent and is an ordinary error response.
type DownloadWriter interface {
	Begin(info DownloadInfo)
	io.Writer
}

// DownloadCheck walks the selection names in dir on share under the operation
// lock and describes the download it would make, so the client can report a
// failure (unreachable, not found, busy, too many) before the browser takes
// over. It reads no file content.
func (s *Service) DownloadCheck(ctx context.Context, rigID, share, dir string, names []string) (*DownloadInfo, error) {
	const op = "Download check"
	unlock, err := s.lockOp(ctx, op, rigID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := s.walk(ctx, op, rigID, share, dir, names)
	if err != nil {
		return nil, err
	}
	info := s.downloadInfo(rigID, share, dir, entries)
	s.log.Debug("devfiles: download check", "rig", rigID, "share", share, "op", op, "dir", dir,
		"files", info.Files, "bytes", info.Bytes, "zipped", info.Zipped)
	return &info, nil
}

// Download sends the selection names in dir on share to w under the operation
// lock: one selected file as itself, anything else as a stored (uncompressed)
// zip whose entries are named relative to dir and dated with the device
// mtime, folders included as directory entries and markers left out. The
// whole selection is walked first, so a listing failure is an ordinary error.
// Each file is opened with retry; once its bytes flow nothing is retried, and
// a failure after the first byte reached w is an *Error of kind ErrMidStream.
// A cancel returns ErrCancelled.
func (s *Service) Download(ctx context.Context, rigID, share, dir string, names []string, w DownloadWriter) error {
	const op = "Download"
	unlock, err := s.lockOp(ctx, op, rigID)
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := s.walk(ctx, op, rigID, share, dir, names)
	if err != nil {
		return err
	}
	info := s.downloadInfo(rigID, share, dir, entries)
	s.log.Info("devfiles: download started", "rig", rigID, "share", share, "op", op, "dir", dir,
		"files", info.Files, "bytes", info.Bytes, "zipped", info.Zipped)

	out := &startWriter{w: w, info: info}
	if info.Zipped {
		err = s.sendZip(ctx, rigID, share, dir, entries, out)
	} else {
		err = s.sendFile(ctx, rigID, share, joinRel(dir, entries[0].Rel), out)
	}
	if err == nil {
		out.begin()
		s.log.Info("devfiles: download finished", "rig", rigID, "share", share, "op", op,
			"files", info.Files, "bytes", out.n)
		return nil
	}
	switch {
	case errors.Is(err, ErrCancelled):
		s.log.Info("devfiles: download cancelled", "rig", rigID, "share", share, "op", op, "sent", out.n)
		return err
	case out.started:
		s.log.Warn("devfiles: download aborted after its first byte", "rig", rigID, "share", share,
			"op", op, "sent", out.n, "error", err)
		return &Error{Kind: ErrMidStream, Rig: rigID, Msg: "download failed after it started: " + err.Error(), Err: err}
	}
	return err
}

// downloadInfo names and sizes the download of a walked selection. A single
// selected file comes down as itself; anything else is ‹rig›-‹folder›.zip,
// the folder being the one selected folder, or the current folder (the share
// at its root) for several items.
func (s *Service) downloadInfo(rigID, share, dir string, entries []entry) DownloadInfo {
	var info DownloadInfo
	for _, e := range entries {
		if !e.Dir {
			info.Files++
			info.Bytes += e.Size
		}
	}
	if len(entries) == 1 && !entries[0].Dir {
		info.Filename = entries[0].Rel
		return info
	}
	info.Zipped = true
	folder := path.Base(dir)
	if dir == "" {
		folder = share
	}
	tops := 0
	for _, e := range entries {
		if !strings.Contains(e.Rel, "/") {
			tops++
		}
	}
	if tops == 1 {
		folder = entries[0].Rel
	}
	info.Filename = s.nameOf(rigID) + "-" + folder + ".zip"
	return info
}

// sendFile streams one file. Its byte count is taken from a Stat in the same
// unit as the open, so Begin's Bytes is the body length; a file that changes
// length while it streams fails rather than sending a body that disagrees.
func (s *Service) sendFile(ctx context.Context, rigID, share, p string, out *startWriter) error {
	return s.streamDeviceFile(ctx, rigID, share, p, func(size int64) (io.Writer, error) {
		out.info.Bytes = size
		return out, nil
	}, func(n, size int64) error {
		if n != size {
			return fmt.Errorf("%s changed while it was sent: %d of %d bytes", displayPath(share, p), n, size)
		}
		return nil
	})
}

// sendZip writes the walked entries as a stored zip. Each file's source is
// opened, with retry, before its header is written.
func (s *Service) sendZip(ctx context.Context, rigID, share, dir string, entries []entry, out *startWriter) error {
	zw := zip.NewWriter(out)
	for _, e := range entries {
		if ctx.Err() != nil {
			return cancelled(ctx)
		}
		if e.Dir {
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: e.Rel + "/", Method: zip.Store, Modified: e.Mtime}); err != nil {
				return err
			}
			continue
		}
		err := s.streamDeviceFile(ctx, rigID, share, joinRel(dir, e.Rel), func(int64) (io.Writer, error) {
			return zw.CreateHeader(&zip.FileHeader{Name: e.Rel, Method: zip.Store, Modified: e.Mtime})
		}, nil)
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// streamDeviceFile opens share/p on a checked-out session, with retry, then
// copies it into the writer dest returns, checking ctx before each read. The
// open (and a Stat for the size) is the unit of retry; once dest has been
// asked for, nothing is retried, since bytes may already be on their way. done,
// when set, checks the copied count against the Stat'ed size.
func (s *Service) streamDeviceFile(ctx context.Context, rigID, share, p string,
	dest func(size int64) (io.Writer, error), done func(n, size int64) error) error {
	err := s.withRig(ctx, "Download", share, rigID, func(c *session) error {
		sh, err := c.share(share)
		if err != nil {
			return err
		}
		fi, err := sh.Stat(p)
		if err != nil {
			return classify(err, c.rigID, c.name, displayPath(share, p))
		}
		rc, err := sh.OpenReader(p)
		if err != nil {
			return classify(err, c.rigID, c.name, displayPath(share, p))
		}
		w, err := dest(fi.Size)
		if err == nil {
			var n int64
			n, err = io.CopyBuffer(w, &ctxReader{ctx: ctx, r: rc}, make([]byte, downloadBufSize))
			if err == nil && done != nil {
				err = done(n, fi.Size)
			}
		}
		closeErr := rc.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			if ctx.Err() != nil {
				return cancelled(ctx)
			}
			// Past the open nothing is retried: hide a network cause from
			// withRetry, but let checkin close the session it broke.
			c.markBroken(err)
			return &streamFailure{err: classify(err, c.rigID, c.name, displayPath(share, p))}
		}
		return nil
	})
	var sf *streamFailure
	if errors.As(err, &sf) {
		return sf.err
	}
	return err
}

// streamFailure is a failure after a file's bytes began to flow. It has no
// Unwrap, so withRetry never sees its network cause and never retries it.
type streamFailure struct{ err error }

func (e *streamFailure) Error() string { return e.err.Error() }

// ctxReader checks ctx before each read, so a cancel stops a stream at the
// next chunk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// startWriter calls the DownloadWriter's Begin before the first byte it
// passes on, and records whether the response has started. It deliberately
// has no ReadFrom, so copies go through the device-sized buffer.
type startWriter struct {
	w       DownloadWriter
	info    DownloadInfo
	started bool
	n       int64
}

func (s *startWriter) begin() {
	if !s.started {
		s.started = true
		s.w.Begin(s.info)
	}
}

func (s *startWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.begin()
	n, err := s.w.Write(p)
	s.n += int64(n)
	return n, err
}
