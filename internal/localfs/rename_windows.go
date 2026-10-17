//go:build windows

package localfs

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// RenameNoReplace claims newpath with MoveFileEx WITHOUT MOVEFILE_REPLACE_EXISTING:
// the rename is atomic across goroutines and processes and fails if the target
// exists rather than overwriting it — the race-free exclusive filing claim of
// design frame-lifecycle §8. MOVEFILE_WRITE_THROUGH flushes the metadata change
// before returning. The "target exists" error is mapped to ErrExist.
// MOVEFILE_COPY_ALLOWED is also withheld, so a cross-volume move fails with
// ERROR_NOT_SAME_DEVICE instead of silently degrading into a copy; that is
// mapped to ErrCrossDevice, the Windows spelling of POSIX EXDEV.
func (o OS) RenameNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(o.ExtendedPath(oldpath))
	if err != nil {
		return fmt.Errorf("rename source %s: %w", oldpath, err)
	}
	to, err := windows.UTF16PtrFromString(o.ExtendedPath(newpath))
	if err != nil {
		return fmt.Errorf("rename dest %s: %w", newpath, err)
	}
	err = windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
	if err == windows.ERROR_ALREADY_EXISTS || err == windows.ERROR_FILE_EXISTS {
		return fmt.Errorf("rename %s -> %s: %w", oldpath, newpath, ErrExist)
	}
	if err == windows.ERROR_NOT_SAME_DEVICE {
		return fmt.Errorf("rename %s -> %s: %w", oldpath, newpath, ErrCrossDevice)
	}
	if err != nil {
		return fmt.Errorf("rename %s -> %s: %w", oldpath, newpath, err)
	}
	return nil
}
