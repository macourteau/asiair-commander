package devfiles

import (
	"context"
	"fmt"
	"strings"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// Rename renames the entry name in dir on share to newName, in place, through
// the device's never-replacing rename. It is refused, before any device call
// when the configuration and the store decide it, where the harvester keys a
// path by name (rig.RenameFromRefused and rig.RenameToRefused, ErrWatched), by
// the layout guard (ErrLayout), and for a marker name (ErrBadPath). The source
// must be listed under exactly name, which rules out 8.3 aliases and case
// variants. A store read error refuses it, unclassified. It holds the
// operation lock: it moves an entry a running copy, delete or download may
// name. A retry after a lost reply finds the source gone and answers
// ErrNotFound, whose wording allows for a rename that went through.
func (s *Service) Rename(ctx context.Context, rigID, share, dir, name, newName string) error {
	const op = "Rename"
	if err := checkShare(share); err != nil {
		return err
	}
	if err := checkDir(dir); err != nil {
		return err
	}
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkStrictSegments(dir); err != nil {
		return err
	}
	if err := checkStrictSegments(name); err != nil {
		return err
	}
	if err := validateName(newName, nameAny); err != nil {
		return err
	}
	if newName == name {
		return newErr(ErrBadPath, rigID, "That is already its name.")
	}

	// R1 and R2's new-name half from the configuration and the store, with no
	// device I/O.
	profile, err := managedProfile(s.cfg.Load(), rigID)
	if err != nil {
		return err
	}
	leads, err := s.store.ResidentLeads(ctx, rigID, share)
	if err != nil {
		s.log.Warn("devfiles: store read for rename failed", "rig", rigID, "share", share, "op", op,
			"error", err)
		return fmt.Errorf("devfiles: resident frames on %s: %w", share, err)
	}
	roots := storeKeptRoots(profile.Sources, share, leads)
	if r, ok := rig.RenameToRefused(profile.Sources, roots, share, dir, newName); ok {
		return s.renameRefused(rigID, share, op, dir, name, newName, r, renameRefusalText(r, share, newName, true))
	}

	unlock, err := s.lockOp(ctx, op, rigID)
	if err != nil {
		return err
	}
	defer unlock()

	from, to := joinRel(dir, name), joinRel(dir, newName)
	return s.withRig(ctx, op, share, rigID, func(c *session) error {
		if err := s.requireWritable(c, share); err != nil {
			return err
		}
		fis, err := s.listOn(c, share, dir)
		if err != nil {
			return err
		}
		var entry *device.FileInfo
		for i := range fis {
			if fis[i].Name == name {
				entry = &fis[i]
				break
			}
		}
		if entry == nil {
			return newErr(ErrNotFound, rigID, "%s", renameGoneText(name))
		}
		// Again on this session's configuration, now that the entry's kind is
		// known: R2's old-name half applies to a folder only.
		roots := storeKeptRoots(c.profile.Sources, share, leads)
		if r, ok := rig.RenameFromRefused(c.profile.Sources, roots, share, dir, name, entry.IsDir); ok {
			return s.renameRefused(rigID, share, op, dir, name, newName, r, renameRefusalText(r, share, name, false))
		}
		if r, ok := rig.RenameToRefused(c.profile.Sources, roots, share, dir, newName); ok {
			return s.renameRefused(rigID, share, op, dir, name, newName, r, renameRefusalText(r, share, newName, true))
		}
		sh, err := c.share(share)
		if err != nil {
			return err
		}
		if err := s.layoutGuardAs(c, sh, share, dir, newName, "Renaming to "+newName); err != nil {
			return err
		}
		if err := sh.Rename(from, to); err != nil {
			switch {
			case device.IsNetwork(err):
				return err
			case device.IsExist(err):
				return &Error{Kind: ErrExists, Rig: rigID, Err: err, Msg: fmt.Sprintf(
					"%s already has an item named %s here (names that differ only in letter case count as the same). Nothing was replaced.",
					c.name, newName)}
			case device.IsInUse(err):
				return &Error{Kind: ErrInUse, Rig: rigID, Err: err, Msg: fmt.Sprintf(
					"%s is in use on %s (the ASIAIR may be writing it). Try again in a moment.", name, c.name)}
			case device.IsNotFound(err):
				return &Error{Kind: ErrNotFound, Rig: rigID, Err: err, Msg: renameGoneText(name)}
			}
			return classify(err, c.rigID, c.name, displayPath(share, from))
		}
		s.log.Info("devfiles: renamed", "rig", rigID, "share", share, "op", c.op, "from", from, "to", to,
			"folder", entry.IsDir)
		return nil
	})
}

// renameRefused logs and returns a refusal by the rename rule.
func (s *Service) renameRefused(rigID, share, op, dir, name, newName string, r rig.RenameRefusal, msg string) error {
	s.log.Warn("devfiles: rename refused", "rig", rigID, "share", share, "op", op,
		"from", joinRel(dir, name), "to", joinRel(dir, newName),
		"rule", string(r.Rule), "cause", string(r.Cause), "root", r.Root, "message", msg)
	return newErr(ErrWatched, rigID, "%s", msg)
}

// renameGoneText is the answer for a source no longer listed: a lost reply
// whose retry finds the source already moved reads the same, so it allows for
// a rename that went through.
func renameGoneText(name string) string {
	return name + " isn't in this folder any more. If the connection dropped, the rename may already have " +
		"gone through; the list now shows the folder as it is."
}

// renameUncheckedText is every listed entry's refusal when the store could not
// be read to derive the roots it keeps in play.
const renameUncheckedText = "Commander couldn't read its records to check renames here. Try again."

// renameRefusalText renders a refusal by the rename rule: name is the entry's
// current name, or the typed new name when toName (R2's new-name half). A
// store-kept root reads as one sentence under either rule, since no Watched
// badge or configured source explains it.
func renameRefusalText(r rig.RenameRefusal, share, name string, toName bool) string {
	if r.Cause == rig.RenameCauseStore {
		return r.Root + " still has frames Commander is archiving or tracking; renaming here would create " +
			"false records. It is refused until they have drained."
	}
	if r.Rule == rig.RenameInWatched {
		msg := "Commander archives this folder by name; renaming here would make it archive these frames again."
		if r.Cause == rig.RenameCauseDisabled {
			// No Watched badge shows a turned-off source's folder: say why.
			msg += " (" + r.Root + " belongs to a capture source that is turned off; it still counts while it is configured.)"
		}
		return msg
	}
	const depends = "a folder Commander depends on (a configured capture source, even one turned off, or the ASIAIR's own data folder)"
	held := share + "/" + r.Root
	if toName {
		return fmt.Sprintf("%s would be, or would hold, %s, %s. Renaming to it would disrupt archiving on %s, so it is refused.",
			name, held, depends, share)
	}
	return fmt.Sprintf("%s is, or holds, %s, %s. Renaming it would disrupt archiving on %s, so it is refused.",
		name, held, depends, share)
}

// storeKeptRoots derives, from the store's resident leads on share
// (FrameStore.ResidentLeads), the roots the store still keeps in play there
// although the configuration no longer names them: a removed source whose
// recorded frames have not drained yet. Rename refuses to move them as it
// refuses a configured root, since a rename there closes undrained rows as
// vanished.
//
// A lead at, under or above a root the configuration still names (sources are
// every configured source of the rig, enabled or disabled; either layout; any
// case) is dropped first, so a configured root is matched exactly as configured
// and never widened to its first segment. A lead above one arises when the root
// has more segments than the lead was cut to (Udisk root Autorun/Light, lead
// ASIAIR/Autorun): its widened root would hold the configured one. Of the rest, a two-segment lead under the
// literal ASIAIR prefix (keys carry it in exactly that case) is a nested root
// as it stands; any other lead's root is its first segment, which keeps a
// removed multi-segment root Foo/Bar as Foo and so refuses a little more than
// the root itself.
func storeKeptRoots(sources []model.Source, share string, leads []string) []string {
	var roots []string
	seen := map[string]bool{}
	for _, lead := range leads {
		lead = strings.Trim(lead, "/")
		if lead == "" || rig.UnderWatchedRoot(sources, share, lead) || rig.HoldsWatchedRoot(sources, share, lead) {
			continue
		}
		segs := strings.Split(lead, "/")
		root := segs[0]
		if len(segs) == 2 && segs[0] == nestedDataPrefix {
			root = lead
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}
