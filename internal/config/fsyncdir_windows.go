//go:build windows

package config

// fsyncDir is a no-op on Windows: FlushFileBuffers on a read-only directory
// handle fails with access-denied, and Windows does not expose directory-entry
// fsync the way POSIX does. Durability of the config write comes from the file's
// own fsync plus the atomic MoveFileEx rename, so there is nothing to flush here.
func fsyncDir(string) error { return nil }
