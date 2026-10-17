//go:build !windows

package localfs

// ExtendedPath is the identity off Windows: there is no MAX_PATH limit to work
// around, so the \\?\ extended-length prefix has no analogue.
func (OS) ExtendedPath(p string) string { return p }
