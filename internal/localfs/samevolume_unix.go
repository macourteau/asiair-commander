//go:build darwin || linux

package localfs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// SameVolume reports whether every path lives on one filesystem, comparing the
// st_dev of each path's nearest existing ancestor (a destination directory may
// not exist yet, but the volume it will live on does). Fewer than two paths are
// trivially one volume. This backs the design architecture §7 single-volume
// validation that keeps the filing rename same-volume and atomic.
func (o OS) SameVolume(paths ...string) (bool, error) {
	if len(paths) < 2 {
		return true, nil
	}
	var first uint64
	for i, p := range paths {
		anc, err := existingAncestor(p)
		if err != nil {
			return false, err
		}
		var st unix.Stat_t
		if err := unix.Stat(anc, &st); err != nil {
			return false, fmt.Errorf("stat %s: %w", anc, err)
		}
		dev := uint64(st.Dev)
		if i == 0 {
			first = dev
			continue
		}
		if dev != first {
			return false, nil
		}
	}
	return true, nil
}
