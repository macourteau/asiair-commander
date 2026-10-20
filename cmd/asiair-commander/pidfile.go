package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The -pid-file contract: the process records its own PID, so a supervisor
// (make demo-down, a script) never has to infer it from `$!`, which names
// whatever was backgrounded — for a `(cd dir && cmd) &` chain, the subshell, not
// the server.
//
// A file naming a live process refuses the start rather than being locked
// against. A lock (flock, LockFileEx) would need its fd held open for the
// process's life and an unlink-vs-lock inode dance on removal, and still leave
// readers to parse a PID out of a file nobody may be holding; refusing keeps the
// file meaning exactly one thing — the PID of the running owner — and stays a
// plain read for every tool that consumes it. Its one weakness is PID reuse: a
// file left by a crash whose PID has since gone to an unrelated process refuses
// the start, which fails loud (the error names the PID and the file) rather than
// running two servers.

// errPIDFileHeld is returned when the pid file names a live process.
var errPIDFileHeld = errors.New("pid file names a running process")

// claimPIDFile records this process's PID at path and returns the function that
// removes it again. The file appears atomically with its full contents (written
// to a temp file, then hard-linked into place, which fails if path exists), so
// a concurrent starter never reads a half-written PID. A file naming a dead
// process is stale: it is logged, removed and the claim retried once.
func claimPIDFile(path string, logger *slog.Logger) (release func(), err error) {
	self := os.Getpid()
	for attempt := 0; ; attempt++ {
		err := linkPIDFile(path, self)
		if err == nil {
			logger.Info("pid file written", "path", path, "pid", self)
			return func() { releasePIDFile(path, self, logger) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("write pid file %s: %w", path, err)
		}
		owner, rerr := readPIDFile(path)
		if rerr != nil {
			// Something that is not a PID file sits at the path. Refuse rather
			// than delete a file this process cannot vouch for.
			return nil, fmt.Errorf("pid file %s exists and is unreadable (%v); remove it if no server is running", path, rerr)
		}
		if processAlive(owner) {
			logger.Error("refusing to start: the pid file names a running process",
				"path", path, "owner_pid", owner)
			return nil, fmt.Errorf("%w: %s holds PID %d; stop that server first, or remove the file if PID %d is not asiair-commander",
				errPIDFileHeld, path, owner, owner)
		}
		if attempt > 0 {
			return nil, fmt.Errorf("pid file %s reappeared naming dead PID %d after it was removed", path, owner)
		}
		logger.Warn("removing a stale pid file: its process is gone", "path", path, "stale_pid", owner)
		// Re-read just before removing, so a concurrent starter that replaced the
		// stale file in between keeps its claim.
		if again, err := readPIDFile(path); err == nil && again == owner {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("remove stale pid file %s: %w", path, err)
			}
		}
	}
}

// linkPIDFile creates path holding pid, failing with fs.ErrExist if it exists.
func linkPIDFile(path string, pid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := fmt.Fprintf(tmp, "%d\n", pid)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Link(tmp.Name(), path)
}

// readPIDFile parses the PID recorded at path.
func readPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("content %q is not a PID", strings.TrimSpace(string(b)))
	}
	return pid, nil
}

// releasePIDFile removes path on shutdown if it still names this process; a
// file someone replaced is left for its new owner.
func releasePIDFile(path string, self int, logger *slog.Logger) {
	owner, err := readPIDFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		logger.Warn("pid file already gone at shutdown", "path", path)
	case err != nil:
		logger.Warn("pid file unreadable at shutdown; leaving it", "path", path, "err", err)
	case owner != self:
		logger.Warn("pid file names another process at shutdown; leaving it", "path", path, "owner_pid", owner)
	default:
		if err := os.Remove(path); err != nil {
			logger.Error("could not remove the pid file", "path", path, "err", err)
			return
		}
		logger.Info("pid file removed", "path", path)
	}
}
