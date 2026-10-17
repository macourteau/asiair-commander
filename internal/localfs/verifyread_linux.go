//go:build linux

package localfs

import (
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// directSectorSize / directBlockSize mirror the Windows NO_BUFFERING alignment:
// O_DIRECT requires the offset, length, and buffer to be aligned, and 4096 is a
// multiple of every common logical block size.
const directSectorSize = 4096
const directBlockSize = 1 << 20 // 1 MiB

// VerifyRead re-reads path with the page cache bypassed and returns the SHA-256
// of exactly `size` bytes. The Linux path is used by the Samba-in-Docker
// integration tests, not production; it prefers O_DIRECT (aligned reads that go
// straight to the device — the true at-rest check) and falls back, only when a
// filesystem rejects O_DIRECT with EINVAL (e.g. tmpfs), to a normal read
// followed by posix_fadvise(FADV_DONTNEED) so a subsequent verify re-reads the device
// rather than a page-cache hit. Tail padding past `size` (present on the
// aligned O_DIRECT path) is discarded by hashNTee.
func (o OS) VerifyRead(path string, size int64) (string, error) {
	sum, _, err := o.VerifyReadTee(path, size, 0)
	return sum, err
}

// VerifyReadTee makes that same cache-bypassing pass — O_DIRECT, or the
// read-then-FADV_DONTNEED fallback — and tees a second digest off it from
// dataOffset, so a caller needing a FITS data unit's identity pays one read rather
// than two and keeps the cache bypass that makes the read proof at all.
func (o OS) VerifyReadTee(path string, size, dataOffset int64) (string, string, error) {
	p := o.ExtendedPath(path)
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECT, 0)
	if err == nil {
		defer unix.Close(fd)
		r := &directReader{fd: fd, block: alignedBuffer(directBlockSize, directSectorSize)}
		return hashNTee(r, size, dataOffset)
	}
	if err != unix.EINVAL {
		return "", "", fmt.Errorf("verifyread open O_DIRECT %s: %w", path, err)
	}

	// Fallback for filesystems without O_DIRECT support.
	f, ferr := os.Open(p)
	if ferr != nil {
		return "", "", fmt.Errorf("verifyread open %s: %w", path, ferr)
	}
	defer f.Close()
	sum, dataSum, herr := hashNTee(f, size, dataOffset)
	// Best-effort: drop the pages we just read so a later verify hits the device.
	_ = unix.Fadvise(int(f.Fd()), 0, size, unix.FADV_DONTNEED)
	if herr != nil {
		return "", "", herr
	}
	return sum, dataSum, nil
}

// alignedBuffer returns a slice of length n whose backing array begins on an
// `align`-byte boundary, as O_DIRECT requires of the read buffer. The Go heap is
// non-moving, so the alignment holds for the buffer's lifetime.
func alignedBuffer(n, align int) []byte {
	raw := make([]byte, n+align)
	off := (align - int(uintptr(unsafe.Pointer(&raw[0]))%uintptr(align))) % align
	return raw[off : off+n]
}

// directReader adapts an O_DIRECT descriptor to io.Reader: it reads whole
// aligned blocks into its aligned scratch buffer and hands the bytes out through
// Read, so hashNTee consumes exactly `size` bytes without a misaligned read.
type directReader struct {
	fd    int
	block []byte // aligned scratch, len == directBlockSize
	buf   []byte // unread remainder of the last block
	eof   bool
}

func (r *directReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		n, err := unix.Read(r.fd, r.block)
		if err != nil {
			return 0, fmt.Errorf("verifyread read: %w", err)
		}
		if n == 0 {
			r.eof = true
			return 0, io.EOF
		}
		if n < len(r.block) {
			r.eof = true // the final block at EOF may be a non-aligned short read
		}
		r.buf = r.block[:n]
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
