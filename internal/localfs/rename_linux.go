//go:build linux

package localfs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// RenameNoReplace uses renameat2 with RENAME_NOREPLACE: an atomic rename that
// fails with EEXIST if the destination exists, needing no hard-link support so
// the destination filesystem is unconstrained (design frame-lifecycle §8). EEXIST
// maps to ErrExist and EXDEV to ErrCrossDevice.
func (o OS) RenameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, o.ExtendedPath(oldpath),
		unix.AT_FDCWD, o.ExtendedPath(newpath), unix.RENAME_NOREPLACE)
	if err == unix.EEXIST {
		return fmt.Errorf("rename %s -> %s: %w", oldpath, newpath, ErrExist)
	}
	if err == unix.EXDEV {
		return fmt.Errorf("rename %s -> %s: %w", oldpath, newpath, ErrCrossDevice)
	}
	if err != nil {
		return fmt.Errorf("rename %s -> %s: %w", oldpath, newpath, err)
	}
	return nil
}
