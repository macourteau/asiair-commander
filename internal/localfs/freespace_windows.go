//go:build windows

package localfs

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// FreeSpace returns the bytes available to the caller and the total bytes of the
// volume holding root via GetDiskFreeSpaceEx — the NAS destination free space the
// design architecture §7 disk_reserve pull-gate checks.
func (o OS) FreeSpace(root string) (int64, int64, error) {
	p16, err := windows.UTF16PtrFromString(o.ExtendedPath(root))
	if err != nil {
		return 0, 0, fmt.Errorf("freespace path %s: %w", root, err)
	}
	var freeAvail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p16, &freeAvail, &total, &totalFree); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", root, err)
	}
	return int64(freeAvail), int64(total), nil
}
