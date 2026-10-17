//go:build !windows

package config

import "os"

// fsyncDir flushes a directory entry so a rename survives a crash (POSIX
// durability). It opens the directory and syncs its handle.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
