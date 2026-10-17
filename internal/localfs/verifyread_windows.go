//go:build windows

package localfs

import (
	"fmt"
	"io"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// verifySectorSize is the alignment used for FILE_FLAG_NO_BUFFERING reads.
// NO_BUFFERING requires the file offset, the buffer address, AND the read
// length to be multiples of the volume's physical sector size. 4096 is a
// multiple of every common sector size (512-byte and 4096-byte drives), so
// aligning to it satisfies the constraint on any volume without querying the
// exact geometry — the bytes-per-sector query (GetDiskFreeSpace) is not wrapped
// in x/sys/windows, and 4096 is the safe superset. Any excess alignment is
// harmless: only the offset/length/buffer multiple matters to the API.
const verifySectorSize = 4096

// verifyBlockSize is the aligned read granularity; a multiple of the sector
// size and large enough to keep reads on the device's pipelined path.
const verifyBlockSize = 1 << 20 // 1 MiB

// VerifyRead re-reads path with FILE_FLAG_NO_BUFFERING so the read bypasses the
// page cache and goes to the M.2 controller — the only read that catches a write
// fault, a controller lie, or a bad sector (design frame-lifecycle §6). It
// returns the SHA-256 of exactly `size` bytes: reads happen in sector-aligned
// blocks into a sector-aligned buffer, and the final block (which spans EOF and
// whose length is therefore not a sector multiple) contributes only its valid
// bytes; hashNTee then stops at `size`, discarding any tail padding so the
// digest matches the stream hash rather than hashing padding and false-passing.
func (o OS) VerifyRead(path string, size int64) (string, error) {
	sum, _, err := o.VerifyReadTee(path, size, 0)
	return sum, err
}

// VerifyReadTee makes that same FILE_FLAG_NO_BUFFERING pass and tees a second
// digest off it from dataOffset, so a caller needing a FITS data unit's identity
// pays one read rather than two and keeps the cache bypass that makes the read
// proof at all.
func (o OS) VerifyReadTee(path string, size, dataOffset int64) (string, string, error) {
	p16, err := windows.UTF16PtrFromString(o.ExtendedPath(path))
	if err != nil {
		return "", "", fmt.Errorf("verifyread path %s: %w", path, err)
	}
	h, err := windows.CreateFile(p16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_NO_BUFFERING,
		0)
	if err != nil {
		return "", "", fmt.Errorf("verifyread open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)

	r := &noBufferingReader{fd: h, block: alignedBuffer(verifyBlockSize, verifySectorSize)}
	return hashNTee(r, size, dataOffset)
}

// alignedBuffer returns a slice of length n whose backing array begins on an
// `align`-byte boundary, as FILE_FLAG_NO_BUFFERING requires of the read buffer.
// The Go heap is non-moving, so the alignment established here holds for the
// buffer's lifetime.
func alignedBuffer(n, align int) []byte {
	raw := make([]byte, n+align)
	off := (align - int(uintptr(unsafe.Pointer(&raw[0]))%uintptr(align))) % align
	return raw[off : off+n]
}

// noBufferingReader adapts a NO_BUFFERING handle to io.Reader: it reads whole
// sector-aligned blocks into its aligned scratch buffer and hands the bytes out
// through Read, so hashNTee can consume exactly `size` bytes without ever issuing a
// misaligned ReadFile.
type noBufferingReader struct {
	fd    windows.Handle
	block []byte // sector-aligned scratch, len == verifyBlockSize
	buf   []byte // unread remainder of the last block
	eof   bool
}

func (r *noBufferingReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		var done uint32
		// The requested length (len(block)) is a sector multiple. On the block
		// that reaches EOF, ReadFile returns done = bytes-to-EOF, which may be a
		// non-sector count; that is expected and permitted.
		err := windows.ReadFile(r.fd, r.block, &done, nil)
		runtime.KeepAlive(r.block)
		if err != nil {
			if err == windows.ERROR_HANDLE_EOF {
				r.eof = true
				return 0, io.EOF
			}
			return 0, fmt.Errorf("verifyread ReadFile: %w", err)
		}
		if done == 0 {
			r.eof = true
			return 0, io.EOF
		}
		if int(done) < len(r.block) {
			r.eof = true // short read == reached EOF; do not issue another aligned read
		}
		r.buf = r.block[:done]
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
