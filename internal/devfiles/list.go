package devfiles

import (
	"context"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// Listing is one folder of a share, or a rig's share list (Share ""). Watched
// is whether the folder is one Commander's harvester watches (rig.WatchedDir);
// Writable is false while the rig has no GUID, so the SPA disables writes.
type Listing struct {
	Share    string      `json:"share"`
	Dir      string      `json:"dir"`
	Watched  bool        `json:"watched"`
	Writable bool        `json:"writable"`
	Entries  []ListEntry `json:"entries"`
}

// ListEntry is one listed item. Dir is true for a folder (a share, in a share
// list); Watched is true for a folder the harvester watches and always false
// for a file. Mtime is unix seconds in the device's clock, uncorrected: it is
// display only. Size is 0 for a folder. RenameRefused is set when Rename
// would refuse the entry whatever its new name (never in a share list).
type ListEntry struct {
	Name          string         `json:"name"`
	Dir           bool           `json:"dir"`
	Size          int64          `json:"size"`
	Mtime         int64          `json:"mtime"`
	Watched       bool           `json:"watched"`
	RenameRefused *RenameVerdict `json:"rename_refused,omitempty"`
}

// RenameVerdict is a listed entry's rename refusal: Rule is "watched" (R1, the
// entry's folder is watched), "reserved" (R2, the folder is or holds a
// reserved path) or "unchecked" (the store could not be read to check), and
// Message the sentence to show, the one Rename answers with.
type RenameVerdict struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// renameUnchecked is the verdict rule for a listing whose store read failed.
const renameUnchecked = "unchecked"

// renameVerdict is the entry's verdict: the rename rule's old-name half
// (rig.RenameFromRefused), or a refusal of every entry when the store's
// resident leads could not be read (storeErr), rather than offering a rename
// that could not be checked.
func renameVerdict(sources []model.Source, roots []string, storeErr error, share, dir, name string, isDir bool) *RenameVerdict {
	if storeErr != nil {
		return &RenameVerdict{Rule: renameUnchecked, Message: renameUncheckedText}
	}
	r, ok := rig.RenameFromRefused(sources, roots, share, dir, name, isDir)
	if !ok {
		return nil
	}
	return &RenameVerdict{Rule: string(r.Rule), Message: renameRefusalText(r, share, name, false)}
}

// List lists rigID's shares (share "", dir "") or one folder of a share. It
// takes no operation lock: a listing runs beside a copy or delete on the
// rig's second slot. Entries is never nil.
func (s *Service) List(ctx context.Context, rigID, share, dir string) (*Listing, error) {
	if share == "" {
		if dir != "" {
			return nil, newErr(ErrBadPath, rigID, "a folder needs a share")
		}
		return s.listShares(ctx, rigID)
	}
	if err := checkShare(share); err != nil {
		return nil, err
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	// One store read per listing, outside the retried attempt.
	leads, leadsErr := s.store.ResidentLeads(ctx, rigID, share)
	if leadsErr != nil {
		s.log.Warn("devfiles: store read for rename failed; every entry refuses rename", "rig", rigID,
			"share", share, "op", "List", "dir", dir, "error", leadsErr)
	}
	out := &Listing{Share: share, Dir: dir, Entries: []ListEntry{}}
	err := s.withRig(ctx, "List", share, rigID, func(c *session) error {
		fis, err := s.listOn(c, share, dir)
		if err != nil {
			return err
		}
		roots := storeKeptRoots(c.profile.Sources, share, leads)
		out.Watched = rig.WatchedDir(c.profile, share, dir)
		out.Writable = c.writable
		out.Entries = make([]ListEntry, 0, len(fis))
		for _, fi := range fis {
			e := ListEntry{Name: fi.Name, Dir: fi.IsDir, Mtime: unixOrZero(fi.Mtime)}
			if fi.IsDir {
				e.Watched = rig.WatchedDir(c.profile, share, joinRel(dir, fi.Name))
			} else {
				e.Size = fi.Size
			}
			e.RenameRefused = renameVerdict(c.profile.Sources, roots, leadsErr, share, dir, fi.Name, fi.IsDir)
			out.Entries = append(out.Entries, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// listShares lists the rig's storage shares as folders.
func (s *Service) listShares(ctx context.Context, rigID string) (*Listing, error) {
	out := &Listing{Entries: []ListEntry{}}
	err := s.withRig(ctx, "List", "", rigID, func(c *session) error {
		names, err := c.sess.ListShares()
		if err != nil {
			return classify(err, c.rigID, c.name, "The share list")
		}
		out.Writable = c.writable
		out.Entries = make([]ListEntry, 0, len(names))
		for _, n := range names {
			out.Entries = append(out.Entries, ListEntry{Name: n, Dir: true, Watched: rig.WatchedDir(c.profile, n, "")})
		}
		s.log.Debug("devfiles: listed shares", "rig", rigID, "share", "", "op", c.op, "entries", len(names))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// unixOrZero is t in unix seconds, 0 for an unset time.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
