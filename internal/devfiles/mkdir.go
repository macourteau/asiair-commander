package devfiles

import (
	"context"
	"strings"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// nestedDataPrefix is the folder a removable Udisk nests the ASIAIR's data
// under, as the harvester's resolveDataPrefix probes it (rig's
// nestedDataPrefix, same literal case).
const nestedDataPrefix = "ASIAIR"

// Mkdir creates the folder name inside dir on share. The name follows the New
// folder rules (validateName); a host-bound rig is refused; the layout
// guard refuses a configured root's name at a removable share's root while
// the nested layout of any configured root exists; a taken name is ErrExists. It takes
// no operation lock. A retry after a lost reply meets the created folder and
// answers ErrExists; the re-listed pane shows it.
func (s *Service) Mkdir(ctx context.Context, rigID, share, dir, name string) error {
	if err := checkShare(share); err != nil {
		return err
	}
	if err := checkDir(dir); err != nil {
		return err
	}
	if err := validateName(name, nameFolder); err != nil {
		return err
	}
	target := joinRel(dir, name)
	return s.withRig(ctx, "New folder", share, rigID, func(c *session) error {
		if err := s.requireWritable(c, share); err != nil {
			return err
		}
		sh, err := c.share(share)
		if err != nil {
			return err
		}
		if err := s.layoutGuard(c, sh, share, dir, name); err != nil {
			return err
		}
		if err := sh.Mkdir(target); err != nil {
			if device.IsExist(err) {
				return &Error{Kind: ErrExists, Rig: rigID, Err: err,
					Msg: "A file or folder named " + name + " already exists here."}
			}
			if device.IsNotFound(err) {
				// Only the parent can be missing: the new folder never existed.
				return classify(err, c.rigID, c.name, displayPath(share, dir))
			}
			return classify(err, c.rigID, c.name, displayPath(share, target))
		}
		s.log.Info("devfiles: folder created", "rig", rigID, "share", share, "op", c.op, "path", target)
		return nil
	})
}

// layoutGuard refuses creating folder name inside dir when that would hide a
// nested watched root from the harvester: resolveDataPrefix walks the roots in
// config order and picks the flat layout for the whole share as soon as one
// lists at the share root, so a folder named like a configured root, created
// at a removable share's root while ASIAIR/ holds any configured root of the
// rig, makes the harvester walk the new folder and never list that nested
// root (with roots [Plan, Autorun], a flat Plan hides ASIAIR/Autorun), with a
// listing that still reads complete. Every configured root on the share
// counts, both as the name and as the nested root, a disabled source's too,
// since enabling it later would expose the same hazard. Names match
// case-insensitively (FAT), and so does the share, which Samba resolves in any
// case. It writes nothing; every path that creates a
// folder calls it first, in the plan and again in the run.
func (s *Service) layoutGuard(c *session, sh device.Share, share, dir, name string) error {
	return s.layoutGuardAs(c, sh, share, dir, name, "Creating "+name)
}

// layoutGuardAs is layoutGuard with the refusal's opening words, action, given
// by the caller: rename says "Renaming to <name>" where a create says
// "Creating <name>".
func (s *Service) layoutGuardAs(c *session, sh device.Share, share, dir, name, action string) error {
	if dir != "" || !rig.IsRemovableShare(share) {
		return nil
	}
	var roots []string
	matches := false
	for _, src := range c.profile.Sources {
		if !strings.EqualFold(src.Share, share) {
			continue
		}
		for _, root := range src.Roots {
			roots = append(roots, root)
			matches = matches || strings.EqualFold(root, name)
		}
	}
	if !matches {
		return nil
	}
	for _, root := range roots {
		nested := joinRel(nestedDataPrefix, root)
		fi, err := sh.Stat(nested)
		switch {
		case err == nil && fi.IsDir:
			s.log.Warn("devfiles: layout guard refused a folder", "rig", c.rigID, "share", share,
				"op", c.op, "name", name, "nested", nested)
			return newErr(ErrLayout, c.rigID,
				"%s here would hide %s/%s from Commander's harvester.", action, share, nested)
		case err != nil && !device.IsNotFound(err):
			return classify(err, c.rigID, c.name, displayPath(share, nested))
		}
	}
	return nil
}
