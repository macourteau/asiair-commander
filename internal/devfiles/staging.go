package devfiles

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Upload staging: an upload's file crosses from the browser to Commander's
// disk first, then to the device like a copy's, so the device write can reopen
// it at byte 0 for every attempt and Retry never re-sends it from the PC
// (doc.go, the staging rules). Staging takes no operation lock and touches no
// device. A staged file lives in the staging directory under its random id
// alone, never its uploaded name, and is reachable only by the session that
// staged it; it is removed on discard, after stagingTTL unused, on eviction at
// stagingCap, and when Commander starts (StartStaging), never by a run.

const (
	// stagingTTL is how long a staged file outlives its last use.
	stagingTTL = time.Hour
	// stagingCap is the most files staged at once, tool-wide, counting those
	// still being received.
	stagingCap = 4
	// stagingReserve is the free space Commander's data volume keeps after a
	// staged file and the rest of every one still being received: the volume
	// also holds Commander's configuration, store and logs.
	stagingReserve = 1 << 30
	// stagingSweepEvery is how often expired staged files are removed, beside
	// the sweep each staging request runs.
	stagingSweepEvery = 5 * time.Minute
	// uploadNameMax caps an uploaded name so its temp name, the name plus
	// ".<16 hex>" and tmpSuffix, stays within the device's 255 bytes: the sink
	// writes temp names verbatim.
	uploadNameMax = maxNameBytes - len(".0123456789abcdef") - len(tmpSuffix)
)

// StagedUpload answers a staging request: the id the plan and run name, and
// the bytes received.
type StagedUpload struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// stagedFile is one staging record. id, owner, name, size and mtime never
// change once it is made; the rest is guarded by stagingStore.mu.
type stagedFile struct {
	id    string
	owner string // the staging session's key: compared, never logged
	name  string
	size  int64
	mtime time.Time // zero when the browser gave none

	// receiving holds until the body ended at exactly size and the file closed
	// cleanly; until then the id is answered as gone.
	receiving bool
	users     int // plans and runs using it: never expired, evicted or discarded
	lastUse   time.Time
	// discarded holds once a discard arrived while users > 0: the id is gone,
	// and the last unpin removes the record and the file.
	discarded bool
}

// stagingStore is the staging directory and its in-memory records. Nothing
// indexes the directory on disk, so a restart forgets every record and
// StartStaging empties the directory to match.
type stagingStore struct {
	dir string

	mu   sync.Mutex
	recs map[string]*stagedFile
	stop chan struct{} // closes the sweeper; nil when none runs
}

func newStagingStore(dir string) *stagingStore {
	return &stagingStore{dir: dir, recs: map[string]*stagedFile{}}
}

func (st *stagingStore) path(id string) string { return filepath.Join(st.dir, id) }

// stopSweeper ends the periodic sweep, if one runs.
func (st *stagingStore) stopSweeper() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.stop != nil {
		close(st.stop)
		st.stop = nil
	}
}

// StartStaging empties and recreates the staging directory (mode 0700) and
// starts the periodic expiry sweep. Commander calls it once, before the web
// server listens, so no staged file outlives the in-memory record it needs.
func (s *Service) StartStaging() error {
	st := s.staging
	if st.dir == "" {
		return errors.New("devfiles: no staging directory configured")
	}
	ents, err := os.ReadDir(st.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("devfiles: read staging directory: %w", err)
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(st.dir, e.Name())); err != nil {
			return fmt.Errorf("devfiles: empty staging directory: %w", err)
		}
	}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return fmt.Errorf("devfiles: create staging directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode as it was.
	if err := os.Chmod(st.dir, 0o700); err != nil {
		return fmt.Errorf("devfiles: staging directory mode: %w", err)
	}
	s.log.Info("devfiles: staging directory emptied", "dir", st.dir, "removed", len(ents))

	st.mu.Lock()
	defer st.mu.Unlock()
	st.recs = map[string]*stagedFile{}
	if st.stop != nil {
		close(st.stop)
	}
	stop := make(chan struct{})
	st.stop = stop
	go func() {
		t := time.NewTicker(stagingSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.sweepStaging()
			case <-stop:
				return
			}
		}
	}()
	return nil
}

// checkUploadName applies the name rules to an uploaded file's name: the
// shared validator, the strict segment check, and uploadNameMax.
func checkUploadName(name string) error {
	if err := validateName(name, nameFile); err != nil {
		return err
	}
	if err := checkStrictSegments(name); err != nil {
		return err
	}
	if len(name) > uploadNameMax {
		return newErr(ErrBadPath, "", "%s %q: it is longer than %d bytes, the most an upload can carry",
			nameFile, name, uploadNameMax)
	}
	return nil
}

// UploadStage receives one file for owner, the requesting session's key: name
// and mtime (zero for none) as the browser gave them, and body, which must
// carry exactly size bytes (size < 0: no declared length). The name rules, the
// cap and Commander's free space are checked before the first byte is read.
// Only a body that ends at exactly size and a file that closes cleanly yield
// an id; any other end removes the partial file and its record. A body failure
// is ErrStageBody, a cancel ErrCancelled. It takes no lock and calls no device.
func (s *Service) UploadStage(ctx context.Context, owner, name string, size int64, mtime time.Time, body io.Reader) (StagedUpload, error) {
	st := s.staging
	if st.dir == "" {
		return StagedUpload{}, errors.New("devfiles: upload staging is not configured")
	}
	if size < 0 {
		s.log.Warn("devfiles: staging refused", "name", name, "reason", "no declared length")
		return StagedUpload{}, newErr(ErrLengthRequired, "", "The upload did not declare its length.")
	}
	if err := checkUploadName(name); err != nil {
		s.log.Warn("devfiles: staging refused", "name", name, "bytes", size, "reason", err)
		return StagedUpload{}, err
	}
	if mtime.UnixMilli() <= 0 {
		// FAT cannot hold the epoch or earlier: treat it as no time at all.
		mtime = time.Time{}
	}
	s.sweepStaging()
	rec, err := s.admitStaged(owner, name, size, mtime)
	if err != nil {
		return StagedUpload{}, err
	}
	// Deferred, so a panic in the receive also removes the record and the
	// partial file instead of leaving them receiving forever; the panic then
	// carries on.
	received := false
	defer func() {
		if received {
			return
		}
		st.mu.Lock()
		delete(st.recs, rec.id)
		st.mu.Unlock()
		s.removeStagedFile(rec, "receive failed")
	}()

	n, err := s.receiveStaged(ctx, rec, body)
	if err != nil {
		s.log.Warn("devfiles: staging receive cut short", "staging_id", rec.id, "name", name,
			"received", n, "expected", size, "error", err)
		return StagedUpload{}, err
	}
	st.mu.Lock()
	rec.receiving = false
	rec.lastUse = s.now()
	received = true
	st.mu.Unlock()
	s.log.Info("devfiles: file staged", "staging_id", rec.id, "name", name, "bytes", size)
	return StagedUpload{ID: rec.id, Size: size}, nil
}

// admitStaged checks Commander's free space and the cap and, under the same
// lock, records the new file as receiving, so concurrent staging requests
// count each other's declared lengths. The free space must keep
// stagingReserve after this file and the declared length of every file still
// being received; the bytes those have already written count twice, which
// errs only towards refusing. At the cap the least recently used idle file is
// evicted; with none idle the request is refused. The free space is read
// before the lock, and an evicted file is removed after it: both are
// filesystem calls, and the free space is only a hint: files being received
// keep filling the volume after any read of it.
func (s *Service) admitStaged(owner, name string, size int64, mtime time.Time) (*stagedFile, error) {
	st := s.staging
	if s.lfs == nil {
		return nil, errors.New("devfiles: upload staging has no local filesystem")
	}
	free, _, err := s.lfs.FreeSpace(st.dir)
	if err != nil {
		s.log.Warn("devfiles: staging refused", "name", name, "bytes", size, "reason", "free space unreadable",
			"error", err)
		return nil, fmt.Errorf("devfiles: staging free space: %w", err)
	}

	var victim *stagedFile
	st.mu.Lock()
	defer func() {
		st.mu.Unlock()
		if victim != nil {
			s.removeStagedFile(victim, "evicted")
		}
	}()
	var receiving int64
	for _, r := range st.recs {
		if r.receiving {
			receiving += r.size
		}
	}
	if size > free-receiving-stagingReserve {
		s.log.Warn("devfiles: staging refused", "name", name, "bytes", size, "reason", "disk space",
			"free", free, "receiving", receiving)
		return nil, newErr(ErrStagingFull, "",
			"Commander's disk doesn't have room for %s: holding it would leave less than 1 GiB free for Commander's own records. Free some space on Commander's disk and try again.",
			name)
	}

	if len(st.recs) >= stagingCap {
		for _, r := range st.recs {
			if !r.receiving && r.users == 0 && (victim == nil || r.lastUse.Before(victim.lastUse)) {
				victim = r
			}
		}
		if victim == nil {
			s.log.Warn("devfiles: staging refused", "name", name, "bytes", size, "reason", "cap",
				"held", len(st.recs))
			return nil, newErr(ErrStagingFull, "",
				"Commander is already holding %d uploads in progress. Try again when one finishes.", stagingCap)
		}
		delete(st.recs, victim.id)
	}

	rec := &stagedFile{
		id: newStagingID(), owner: owner, name: name, size: size, mtime: mtime,
		receiving: true, lastUse: s.now(),
	}
	st.recs[rec.id] = rec
	return rec, nil
}

// receiveStaged streams body into rec's file and reports the bytes written.
// It fails unless body ends at exactly rec.size and the file closes cleanly.
func (s *Service) receiveStaged(ctx context.Context, rec *stagedFile, body io.Reader) (int64, error) {
	f, err := os.OpenFile(s.staging.path(rec.id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("devfiles: create staged file: %w", err)
	}
	// Closed below to read its error; this covers a panic, after which the
	// caller removes the file, which Windows refuses while it is open.
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	br := &bodyReader{ctx: ctx, rd: body}
	n, err := io.CopyN(f, br, rec.size)
	tooLong := false
	if err == nil {
		// The body must end here: one more byte means it did not end where it
		// said it would.
		var b [1]byte
		m, _ := io.ReadFull(br, b[:])
		tooLong = m > 0
	}
	cerr := f.Close()
	closed = true
	bodyFailed := br.err != nil && br.err != io.EOF
	switch {
	case ctx.Err() != nil:
		return n, cancelled(ctx)
	case tooLong:
		return n, &Error{Kind: ErrStageBody,
			Msg: fmt.Sprintf("The upload of %s ran past its %d bytes.", rec.name, rec.size)}
	case bodyFailed || (n < rec.size && br.err == io.EOF):
		return n, &Error{Kind: ErrStageBody, Err: br.err,
			Msg: fmt.Sprintf("The upload of %s broke off after %d of %d bytes.", rec.name, n, rec.size)}
	case err != nil:
		return n, fmt.Errorf("devfiles: write staged file: %w", err)
	case cerr != nil:
		return n, fmt.Errorf("devfiles: close staged file: %w", cerr)
	}
	return n, nil
}

// bodyReader checks ctx before every read and keeps the body's own error, so
// a failed receive tells the body's end from the local disk's.
type bodyReader struct {
	ctx context.Context
	rd  io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		b.err = err
		return 0, err
	}
	n, err := b.rd.Read(p)
	if err != nil {
		b.err = err
	}
	return n, err
}

// pinStaged resolves id for owner and pins it against expiry, eviction and
// discard until unpin, which also counts as a use. An id that is unknown,
// still receiving, or another session's is ErrStagingGone alike, so an id
// reveals nothing.
func (s *Service) pinStaged(owner, id string) (rec *stagedFile, unpin func(), err error) {
	st := s.staging
	st.mu.Lock()
	defer st.mu.Unlock()
	rec = st.recs[id]
	if rec == nil || rec.receiving || rec.discarded || rec.owner != owner {
		s.log.Info("devfiles: staged file not held", "staging_id", id)
		return nil, nil, stagingGone()
	}
	rec.users++
	rec.lastUse = s.now()
	return rec, func() {
		st.mu.Lock()
		rec.users--
		rec.lastUse = s.now()
		remove := rec.discarded && rec.users == 0
		if remove {
			delete(st.recs, rec.id)
		}
		st.mu.Unlock()
		if remove {
			s.removeStagedFile(rec, "discarded")
		}
	}, nil
}

func stagingGone() error {
	return newErr(ErrStagingGone, "",
		"Commander no longer holds the file you sent (it expired, made room for newer uploads, or Commander restarted). Send it again.")
}

// UploadDiscard removes the file owner staged under id. While a plan or run
// is using it, the id is gone at once and the file is removed when the last
// of them lets go. An unknown, foreign or already discarded id is
// ErrStagingGone, as for a plan or run.
func (s *Service) UploadDiscard(owner, id string) error {
	st := s.staging
	st.mu.Lock()
	rec := st.recs[id]
	if rec == nil || rec.receiving || rec.discarded || rec.owner != owner {
		st.mu.Unlock()
		s.log.Info("devfiles: staged file not held", "staging_id", id)
		return stagingGone()
	}
	if rec.users > 0 {
		rec.discarded = true
		st.mu.Unlock()
		s.log.Debug("devfiles: staged file in use, removed when released", "staging_id", id)
		return nil
	}
	delete(st.recs, id)
	st.mu.Unlock()
	s.removeStagedFile(rec, "discarded")
	return nil
}

// sweepStaging removes every staged file unused for stagingTTL. A file being
// received, planned or run never expires.
func (s *Service) sweepStaging() {
	st := s.staging
	now := s.now()
	var expired []*stagedFile
	st.mu.Lock()
	for id, r := range st.recs {
		if !r.receiving && r.users == 0 && now.Sub(r.lastUse) >= stagingTTL {
			expired = append(expired, r)
			delete(st.recs, id)
		}
	}
	held := len(st.recs)
	st.mu.Unlock()
	for _, r := range expired {
		s.removeStagedFile(r, "expired")
	}
	s.log.Debug("devfiles: staging sweep", "held", held, "removed", len(expired))
}

// removeStagedFile deletes a staged file whose record is already gone.
func (s *Service) removeStagedFile(rec *stagedFile, why string) {
	if err := os.Remove(s.staging.path(rec.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("devfiles: staged file could not be removed", "staging_id", rec.id, "name", rec.name,
			"why", why, "error", err)
		return
	}
	s.log.Info("devfiles: staged file removed", "staging_id", rec.id, "name", rec.name, "why", why)
}

// newStagingID is 128 random bits in hex: unguessable, and a file name that
// says nothing about the upload.
func newStagingID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails; it crashes the program instead
	return hex.EncodeToString(b[:])
}
