//go:build windows

package localfs

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// SameVolume reports whether every path lives on one volume, comparing the
// volume mount point (GetVolumePathName) of each path's nearest existing
// ancestor. A destination directory may not exist yet, but the volume it will
// live on does. Comparison is case-insensitive per Windows path semantics. This
// backs the design architecture §7 single-volume validation that keeps filing
// same-volume and atomic.
func (o OS) SameVolume(paths ...string) (bool, error) {
	if len(paths) < 2 {
		return true, nil
	}
	var first string
	for i, p := range paths {
		anc, err := existingAncestor(p)
		if err != nil {
			return false, err
		}
		vol, err := volumeMountPoint(anc)
		if err != nil {
			return false, err
		}
		if i == 0 {
			first = vol
			continue
		}
		if !strings.EqualFold(vol, first) {
			return false, nil
		}
	}
	return true, nil
}

func volumeMountPoint(p string) (string, error) {
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p16, &buf[0], uint32(len(buf))); err != nil {
		return "", fmt.Errorf("GetVolumePathName %s: %w", p, err)
	}
	return windows.UTF16ToString(buf), nil
}
