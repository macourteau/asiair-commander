package header

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/macourteau/asiair-commander/internal/model"
)

// WholeDigestVersion and DataDigestVersion name the algorithms DigestTee
// computes: the whole-file digest and the data-unit digest. A remote measures a
// file's digests with this package and reports both versions, and Commander
// trusts a remote's digest only when its version equals its own, so any change
// that could make either digest differ for the same bytes (the hash function,
// the END-card scan, where the data unit starts) bumps its version.
const (
	WholeDigestVersion = 1
	DataDigestVersion  = 1
)

// CopyBufSize is the buffer a device file is streamed with, whether copied or
// read through a DigestTee for its digests, in Commander and on a remote alike:
// 512 KiB or more keeps smb1client on its pipelined read path.
const CopyBufSize = 512 * 1024

// EmptyDataDigest is the hex SHA-256 of zero bytes, which is what a FITS file
// carrying no data unit hashes to. It is named because every such file hashes to
// it, so it is an identity that matches between two frames that have nothing to do
// with each other — see IsUsableDataDigest.
const EmptyDataDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// IsUsableDataDigest reports whether d can serve as an exposure's identity.
//
// Two values cannot: the empty string, which is what a header that does not parse
// yields, and EmptyDataDigest, which every FITS file with a zero-length data unit
// yields. The second is the precondition design frame-lifecycle §11.4 puts in
// front of every identity test: a data-unit match is argued only probabilistically
// — read noise makes two exposures with identical pixels vanishingly improbable
// rather than impossible — and nothing probabilistic reaches the deterministic
// case where two unrelated header-only frames agree. Since one of those identity
// tests authorizes overwriting an archived exposure, the answer has to be refused
// outright rather than weighed.
//
// It exists as one exported predicate so that every consumer refuses both values
// by the same rule instead of each re-deriving it, and so that a consumer reading
// only its own call site still meets the reason.
func IsUsableDataDigest(d string) bool {
	return d != "" && d != EmptyDataDigest
}

// DigestTee is an io.Writer that yields both the whole-file SHA-256 and the
// SHA-256 of the file's FITS data unit from a single pass over the bytes. It is
// the device-side half of the data-unit identity: a consumer that already holds
// the bytes as they flow — the SMB streams of the delete pass — drops it into the
// io.MultiWriter it already builds and pays no extra open and no second read.
//
// It finds the data unit by scanning the passing bytes for the END card, so it
// needs no prior knowledge of the file and no seek. Consumers that cannot scan a
// stream they do not own take the offset from DataOffset instead.
//
// It never fails. A stream whose header does not parse — not FITS, truncated
// before END, or no END within the header-block budget — yields the whole-file
// digest and an EMPTY data-unit digest rather than an error. That contract is
// load-bearing: these digests are teed off reads whose own failure means a lost
// frame, and turning an unparseable header into an I/O failure there would demote
// the critical alert a rotted archive copy deserves into a warning and an
// indefinite deferral. An empty digest instead fails every identity test by
// construction, which is the safe direction.
type DigestTee struct {
	whole hash.Hash
	data  hash.Hash
	scan  headerScanner
	pos   int64 // bytes written so far
}

// NewDigestTee returns a DigestTee ready to receive the first byte of a file.
func NewDigestTee() *DigestTee {
	return &DigestTee{whole: sha256.New(), data: sha256.New(), scan: newHeaderScanner()}
}

// Write feeds p to the whole-file digest, advances the END-card scan while the
// header is still passing, and feeds the data digest whatever part of p lies at or
// past the data offset. It never returns an error.
func (t *DigestTee) Write(p []byte) (int, error) {
	t.whole.Write(p)
	if !t.scan.done {
		t.scan.feed(p)
	}
	if t.scan.dataOff >= 0 {
		if start := t.scan.dataOff - t.pos; start < int64(len(p)) {
			if start < 0 {
				start = 0
			}
			t.data.Write(p[start:])
		}
	}
	t.pos += int64(len(p))
	return len(p), nil
}

// Sums returns the hex whole-file digest and the hex data-unit digest of
// everything written so far. The data digest is the empty string when the header
// did not parse: either no END card was seen, or the stream ended inside the
// header block that carries it, which is where ReadHeader errors too — a header is
// a whole number of blocks, so a partial one is not a header. It is
// EmptyDataDigest when the header parsed but no data byte followed, which
// IsUsableDataDigest refuses in its own right.
func (t *DigestTee) Sums() (whole, data string) {
	whole = hex.EncodeToString(t.whole.Sum(nil))
	if t.scan.dataOff < 0 || t.pos < t.scan.dataOff {
		return whole, ""
	}
	return whole, hex.EncodeToString(t.data.Sum(nil))
}

// ErrStreamStopped is StreamDigests' error for a stream abandoned because its
// stop hook reported true.
var ErrStreamStopped = errors.New("digest stream stopped")

// StreamDigests reads r once, in CopyBufSize buffers, through a DigestTee and
// returns both digests. It is the one streaming digest read of a device file:
// Commander's pre-delete re-hash and a remote's measurement both run it, so the
// two cannot drift apart.
//
// Before every read it asks stop, when set, and ends with ErrStreamStopped once
// it reports true, so an abandoned stream waits out at most one buffer. After
// every read that returned bytes it hands progress, when set, the running total.
func StreamDigests(r io.Reader, stop func() bool, progress func(n int64)) (whole, data string, err error) {
	tee := NewDigestTee()
	src := &hookedReader{r: r, stop: stop, progress: progress}
	if _, err := io.CopyBuffer(tee, src, make([]byte, CopyBufSize)); err != nil {
		return "", "", err
	}
	whole, data = tee.Sums()
	return whole, data, nil
}

// hookedReader is StreamDigests' reader: it consults stop before each read and
// reports the running byte count after it, passing the bytes through untouched.
// It deliberately implements nothing but Read, so io.CopyBuffer uses the buffer
// it is given rather than a WriterTo of the underlying reader.
type hookedReader struct {
	r        io.Reader
	n        int64
	stop     func() bool
	progress func(int64)
}

func (h *hookedReader) Read(p []byte) (int, error) {
	if h.stop != nil && h.stop() {
		return 0, ErrStreamStopped
	}
	n, err := h.r.Read(p)
	if n > 0 {
		h.n += int64(n)
		if h.progress != nil {
			h.progress(h.n)
		}
	}
	return n, err
}

// DataOffset reports the byte offset at which path's FITS data unit begins: the
// end of the header block that holds the END card. It is the archive-side half of
// the data-unit identity, for consumers that read through a seam owning its own
// descriptor and so cannot scan the stream — they read the offset here and hand it
// to that seam, which stays format-blind. It reads only the header blocks, so it
// is not a whole-file pass.
//
// A header that does not parse yields offset 0 and NO error, for the reason
// DigestTee documents: 0 means "no data unit known", and the digest seam handed a
// zero offset reports an empty data digest, which fails every identity test.
// A genuine I/O failure is still an error — it is not a statement about the
// header, and the caller's own read of the same file is about to meet it too.
func DataOffset(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("header: open %s: %w", path, err)
	}
	defer f.Close()

	s := newHeaderScanner()
	buf := make([]byte, blockSize)
	var read int64
	for !s.done {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			s.feed(buf[:n])
			read += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break // shorter than its own header: no data unit known
			}
			return 0, fmt.Errorf("header: read %s: %w", path, err)
		}
	}
	// A header is a whole number of blocks, so a file that ends inside the block
	// carrying END has no complete header — the point at which ReadHeader errors,
	// and the rule this must agree with. Reporting the offset anyway would name a
	// data unit that begins past EOF.
	if s.dataOff < 0 || s.dataOff > read {
		return 0, nil
	}
	return s.dataOff, nil
}

// headerScanner walks 80-column card images looking for the END card and derives
// the data unit's offset from it. It is the one END-card scanner the streaming tee
// and the offset lookup share, so the two cannot disagree about where a data unit
// begins, and it honours the same maxHeaderBlocks budget the reader does — a file
// with no END must not drag the scan into a 120 MB data unit.
type headerScanner struct {
	card    [cardSize]byte
	cardLen int   // bytes of card filled; a card can span two writes
	cardIdx int64 // index of the card being filled, from the start of the file
	dataOff int64 // offset of the first data byte, or -1 while unknown
	done    bool  // END was found, or the block budget was spent
}

func newHeaderScanner() headerScanner { return headerScanner{dataOff: -1} }

// feed consumes p until the END card is found or the header-block budget is spent,
// ignoring anything past that point.
func (s *headerScanner) feed(p []byte) {
	for len(p) > 0 && !s.done {
		n := copy(s.card[s.cardLen:], p)
		s.cardLen += n
		p = p[n:]
		if s.cardLen < cardSize {
			return // the rest of this card is in a later write
		}
		s.cardLen = 0
		if model.NormalizeKey(string(s.card[:keyLen])) == "END" {
			// The header is a whole number of blocks, so the data unit starts at
			// the block boundary at or after the END card's last byte.
			end := (s.cardIdx + 1) * cardSize
			s.dataOff = (end + blockSize - 1) / blockSize * blockSize
			s.done = true
			return
		}
		s.cardIdx++
		if s.cardIdx*cardSize >= maxHeaderBlocks*blockSize {
			s.done = true // no END within the budget: this is not a header
		}
	}
}
