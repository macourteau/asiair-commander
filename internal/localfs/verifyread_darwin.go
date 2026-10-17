//go:build darwin

package localfs

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// VerifyRead re-reads path from the device and returns the SHA-256 of exactly
// `size` bytes. F_NOCACHE is darwin's equivalent of Linux O_DIRECT: it turns
// off caching for this descriptor so reads are served from the device, not the
// unified buffer cache — the whole point of the at-rest verify (design
// frame-lifecycle §6). Unlike O_DIRECT it imposes no offset/length/buffer
// alignment requirement, so a plain sequential read of exactly `size` bytes
// suffices and there is no tail padding to discard.
func (o OS) VerifyRead(path string, size int64) (string, error) {
	sum, _, err := o.VerifyReadTee(path, size, 0)
	return sum, err
}

// VerifyReadTee makes that same F_NOCACHE pass and tees a second digest off it
// from dataOffset, so a caller needing a FITS data unit's identity pays one read
// rather than two and keeps the cache bypass that makes the read proof at all.
func (o OS) VerifyReadTee(path string, size, dataOffset int64) (string, string, error) {
	f, err := os.Open(o.ExtendedPath(path))
	if err != nil {
		return "", "", fmt.Errorf("verifyread open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1); err != nil {
		return "", "", fmt.Errorf("verifyread F_NOCACHE %s: %w", path, err)
	}
	return hashNTee(f, size, dataOffset)
}
