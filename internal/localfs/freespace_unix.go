//go:build darwin || linux

package localfs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeSpace returns the free (available to unprivileged callers) and total bytes
// of the filesystem holding root via statfs. Bavail is used rather than Bfree so
// the figure matches what a non-root service can actually write.
func (o OS) FreeSpace(root string) (int64, int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(o.ExtendedPath(root), &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", root, err)
	}
	bsize := int64(st.Bsize)
	free := int64(st.Bavail) * bsize
	total := int64(st.Blocks) * bsize
	return free, total, nil
}
