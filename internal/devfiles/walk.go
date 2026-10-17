package devfiles

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
)

// entry is one walked file or folder. Rel is relative to the walked folder;
// Mtime is the device's clock as listed, uncorrected.
type entry struct {
	Rel   string
	Size  int64
	Mtime time.Time
	Dir   bool
}

// listDir lists dir on share through withRetry, one listing being the unit of
// retry. Commander's marker files are dropped, matched case-insensitively;
// nothing else is filtered (a file manager shows what is there).
func (s *Service) listDir(ctx context.Context, op, rigID, share, dir string) ([]device.FileInfo, error) {
	var out []device.FileInfo
	err := s.withRig(ctx, op, share, rigID, func(c *session) error {
		var err error
		out, err = s.listOn(c, share, dir)
		return err
	})
	return out, err
}

// listOn lists dir on share through a checked-out session, markers dropped,
// sorted by name.
func (s *Service) listOn(c *session, share, dir string) ([]device.FileInfo, error) {
	sh, err := c.share(share)
	if err != nil {
		return nil, err
	}
	raw, err := sh.ListDir(dir)
	if err != nil {
		// Listing a file fails as a missing folder (or a server's own status),
		// which would tell the user it is gone when it is right there: say what
		// it is instead.
		if dir != "" && !device.IsNetwork(err) {
			if fi, serr := sh.Stat(dir); serr == nil && !fi.IsDir {
				return nil, &Error{Kind: ErrNotFound, Rig: c.rigID, Err: err,
					Msg: fmt.Sprintf("%s is a file, not a folder", displayPath(share, dir))}
			}
		}
		return nil, classify(err, c.rigID, c.name, displayPath(share, dir))
	}
	out := make([]device.FileInfo, 0, len(raw))
	for _, fi := range raw {
		if isMarker(fi.Name) {
			continue
		}
		out = append(out, fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	s.log.Debug("devfiles: listed", "rig", c.rigID, "share", share, "op", c.op, "dir", dir, "entries", len(out))
	return out, nil
}

// walk lists the selection names in dir on share recursively, in pre-order
// (a folder before its contents), with sizes and mtimes. Markers are dropped
// and nothing else is. Any listing failure, after its retries, fails the whole
// walk: a plan, summary or zip is never built from a partial listing. More
// than s.maxFiles files fails with ErrTooMany. The context is checked between
// listings. The caller holds the operation lock.
func (s *Service) walk(ctx context.Context, op, rigID, share, dir string, names []string) ([]entry, error) {
	if err := checkShare(share); err != nil {
		return nil, err
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, newErr(ErrBadPath, rigID, "nothing selected")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := checkName(n); err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, newErr(ErrBadPath, rigID, "%q is selected twice", n)
		}
		seen[n] = true
	}

	top, err := s.listDir(ctx, op, rigID, share, dir)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]device.FileInfo, len(top))
	for _, fi := range top {
		byName[fi.Name] = fi
	}

	var out []entry
	files := 0
	var visit func(rel string, fi device.FileInfo) error
	visit = func(rel string, fi device.FileInfo) error {
		if !fi.IsDir {
			files++
			if files > s.maxFiles {
				return newErr(ErrTooMany, rigID,
					"This selection has more than %d files. Select fewer files or a subfolder.", s.maxFiles)
			}
			out = append(out, entry{Rel: rel, Size: fi.Size, Mtime: fi.Mtime})
			return nil
		}
		out = append(out, entry{Rel: rel, Mtime: fi.Mtime, Dir: true})
		if ctx.Err() != nil {
			return cancelled(ctx)
		}
		kids, err := s.listDir(ctx, op, rigID, share, joinRel(dir, rel))
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := visit(joinRel(rel, k.Name), k); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range names {
		fi, ok := byName[n]
		if !ok {
			return nil, newErr(ErrNotFound, rigID, "%s no longer exists on %s",
				displayPath(share, joinRel(dir, n)), s.nameOf(rigID))
		}
		if err := visit(n, fi); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// displayPath is share/dir for messages.
func displayPath(share, dir string) string {
	if dir == "" {
		return share
	}
	return share + "/" + dir
}
