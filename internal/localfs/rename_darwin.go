//go:build darwin

package localfs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// RenameNoReplace uses renameatx_np with RENAME_EXCL, darwin's atomic exclusive
// rename: it fails with EEXIST if the destination exists, the direct equivalent
// of Linux's renameat2(RENAME_NOREPLACE) and Windows's MoveFileEx-without-replace
// (design frame-lifecycle §8). APFS and HFS+ both support it. EEXIST maps to
// ErrExist and EXDEV to ErrCrossDevice.
func (o OS) RenameNoReplace(oldpath, newpath string) error {
	err := unix.RenameatxNp(unix.AT_FDCWD, o.ExtendedPath(oldpath),
		unix.AT_FDCWD, o.ExtendedPath(newpath), unix.RENAME_EXCL)
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
