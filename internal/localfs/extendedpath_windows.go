//go:build windows

package localfs

import (
	"path/filepath"
	"strings"
)

// ExtendedPath prefixes an absolute Windows path with \\?\ (or \\?\UNC\ for a
// UNC path) so the legacy 260-char MAX_PATH limit does not apply to the deep
// calibration trees the router can build (design routing-and-filing §4). The
// path is made absolute and cleaned first, since the \\?\ namespace does not
// normalize "." or ".." components the way a normal Win32 path does.
func (OS) ExtendedPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, `\\`) {
		// UNC: \\server\share\path -> \\?\UNC\server\share\path
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}
