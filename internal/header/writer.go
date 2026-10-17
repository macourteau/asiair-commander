package header

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/sanitize"
)

// maxTargetLen is the conservative ceiling on a sanitized OBJECT value. The FITS
// fixed-format value field is 70 columns (11-80); after the two quotes and the
// doubled-quote escape a value can still fit up to ~68 columns, but 60 leaves
// headroom for the escape expansion and matches the API-boundary budget (design
// routing-and-filing §9.1).
const maxTargetLen = 60

// minValueChars is the FITS fixed-format minimum for a string value: the value
// is blank-padded so the closing quote lands at column >= 20 (design
// routing-and-filing §9.1).
const minValueChars = 8

// RewriteResult reports the outcome of a single OBJECT-card rewrite. OrigCard and
// NewCard are the 80-byte card images before and after; Offset is the card's byte
// offset in the file. ComplementIn and ComplementOut are SHA-256 sums over every
// byte OUTSIDE the [Offset, Offset+80) window of the input and output streams
// respectively — they are equal iff the edit changed nothing but the OBJECT card,
// which is the caller's faithfulness proof (design routing-and-filing §9.1, INV-4).
// OutputSHA256 is the SHA-256 over EVERY byte written to dst (the full file, OBJECT
// window included), computed in the same streaming pass so the caller can prove the
// at-rest read-back matches what was streamed — the safety anchor for retarget_sha256
// (design routing-and-filing §9.3 step 6, INV-4).
type RewriteResult struct {
	OrigCard      [cardSize]byte
	NewCard       [cardSize]byte
	Offset        int64
	ComplementIn  [sha256.Size]byte
	ComplementOut [sha256.Size]byte
	OutputSHA256  [sha256.Size]byte
}

// RewriteObject streams src to dst, replacing exactly the effective OBJECT value
// card with the canonical form OBJECT  = '<newTarget>' and copying every other
// byte — all other header cards and the entire data unit — verbatim. It never
// decodes the data unit; the cost is one streamed copy.
//
// In the same pass it computes the two complement digests (see RewriteResult) so
// the caller can byte-prove the science data survived the edit at zero extra I/O.
// dst is fsync'd before returning so the durable file precedes any rename.
//
// It refuses (returns an error) when: newTarget sanitizes empty; the sanitized,
// quoted, min-padded value will not fit the value field; there is no OBJECT value
// card before END; or there is more than one OBJECT value card (pathological for
// ASIAIR frames — refuse rather than guess).
func RewriteObject(src, dst string, newTarget string) (RewriteResult, error) {
	target := sanitize.Component(newTarget)
	if target == "" {
		return RewriteResult{}, fmt.Errorf("header: retarget value is empty after sanitize")
	}
	if len([]rune(target)) > maxTargetLen {
		return RewriteResult{}, fmt.Errorf("header: retarget value %q exceeds %d characters", target, maxTargetLen)
	}
	newCard, err := buildObjectCard(target)
	if err != nil {
		return RewriteResult{}, err
	}

	in, err := os.Open(src)
	if err != nil {
		return RewriteResult{}, fmt.Errorf("header: open %s: %w", src, err)
	}
	defer in.Close()

	// Scan and buffer the leading header blocks up to END, locating the single
	// OBJECT value card. The header is block-bounded (< maxHeaderBlocks), so
	// buffering it is cheap and lets us validate before touching dst.
	var headerBytes []byte
	var origCard [cardSize]byte
	offset := int64(-1)
	objectCount := 0
	foundEnd := false
	buf := make([]byte, blockSize)
	for block := 0; block < maxHeaderBlocks && !foundEnd; block++ {
		if _, err := io.ReadFull(in, buf); err != nil {
			return RewriteResult{}, fmt.Errorf("header: %s: end of file before END card: %w", src, err)
		}
		base := int64(block) * blockSize
		headerBytes = append(headerBytes, buf...)
		for i := 0; i+cardSize <= blockSize; i += cardSize {
			card := buf[i : i+cardSize]
			key := model.NormalizeKey(string(card[:keyLen]))
			if key == "END" {
				foundEnd = true
				break
			}
			// A value card carries the "= " value indicator in columns 9-10; a
			// commentary card that happens to key OBJECT is not a value and is skipped.
			if key == "OBJECT" && card[keyLen] == '=' && card[keyLen+1] == ' ' {
				objectCount++
				offset = base + int64(i)
				copy(origCard[:], card)
			}
		}
	}
	if !foundEnd {
		return RewriteResult{}, fmt.Errorf("header: %s: no END card within %d header blocks", src, maxHeaderBlocks)
	}
	if objectCount == 0 {
		return RewriteResult{}, fmt.Errorf("header: %s: no OBJECT value card before END", src)
	}
	if objectCount > 1 {
		return RewriteResult{}, fmt.Errorf("header: %s: %d OBJECT value cards; refusing to guess", src, objectCount)
	}

	out, err := os.Create(dst)
	if err != nil {
		return RewriteResult{}, fmt.Errorf("header: create %s: %w", dst, err)
	}
	// On any error past this point the partial dst is the caller's to clean up
	// (it owns the temp path), matching restoreNAS's defer-remove discipline.

	// Write the header with the OBJECT window replaced, and feed both complement
	// digests every header byte OUTSIDE that window. Because the bytes outside the
	// window are identical between input and output, the two digests match here;
	// they can only diverge if a non-OBJECT byte is corrupted.
	modHeader := make([]byte, len(headerBytes))
	copy(modHeader, headerBytes)
	copy(modHeader[offset:offset+cardSize], newCard[:])

	hIn := sha256.New()
	hOut := sha256.New()
	// hFull digests EVERY byte written to dst — the full file including the OBJECT
	// window — so the caller can compare the at-rest read-back to this stream-derived
	// expectation and refuse a write fault before it becomes retarget_sha256.
	hFull := sha256.New()
	winStart, winEnd := int(offset), int(offset)+cardSize
	hIn.Write(headerBytes[:winStart])
	hIn.Write(headerBytes[winEnd:])
	hOut.Write(modHeader[:winStart])
	hOut.Write(modHeader[winEnd:])
	hFull.Write(modHeader)

	if _, err := out.Write(modHeader); err != nil {
		out.Close()
		return RewriteResult{}, fmt.Errorf("header: write %s: %w", dst, err)
	}

	// Copy the data unit verbatim, feeding the digests the identical bytes. in is
	// positioned at the first data byte (right after the last header block).
	if _, err := io.CopyBuffer(io.MultiWriter(out, hIn, hOut, hFull), in, make([]byte, 32*1024)); err != nil {
		out.Close()
		return RewriteResult{}, fmt.Errorf("header: copy data unit to %s: %w", dst, err)
	}

	if err := out.Sync(); err != nil {
		out.Close()
		return RewriteResult{}, fmt.Errorf("header: fsync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return RewriteResult{}, fmt.Errorf("header: close %s: %w", dst, err)
	}

	res := RewriteResult{OrigCard: origCard, NewCard: newCard, Offset: offset}
	copy(res.ComplementIn[:], hIn.Sum(nil))
	copy(res.ComplementOut[:], hOut.Sum(nil))
	copy(res.OutputSHA256[:], hFull.Sum(nil))
	return res, nil
}

// buildObjectCard renders the canonical fixed-format OBJECT card image:
// "OBJECT  = '<value>'" with the value blank-padded to the FITS minimum (closing
// quote at column >= 20), embedded single quotes doubled, and the whole card
// space-padded to 80. Any original comment is dropped, keeping the rewrite
// deterministic. It errors if the escaped value cannot fit the value field.
//
// FROZEN FORMAT — changing these bytes breaks re-apply/restore of every previously-
// retargeted frame. The re-apply (design routing-and-filing §9.4) and restore (design
// routing-and-filing §9.4) paths reproduce the canonical card byte-for-byte and require
// the result to hash back to the stored retarget_sha256; any change to the padding,
// quoting, or layout here would make an already-retargeted frame fail to re-verify.
// TestBuildObjectCard_GoldenBytes pins the exact output so a change fails loudly rather
// than silently corrupting the anchor.
func buildObjectCard(value string) ([cardSize]byte, error) {
	escaped := strings.ReplaceAll(value, "'", "''")
	padded := escaped
	if len(padded) < minValueChars {
		padded += strings.Repeat(" ", minValueChars-len(padded))
	}

	var card [cardSize]byte
	for i := range card {
		card[i] = ' '
	}
	copy(card[0:], "OBJECT")
	card[keyLen] = '='     // column 9
	card[keyLen+1] = ' '   // column 10
	card[keyLen+2] = '\''  // opening quote, column 11
	valStart := keyLen + 3 // column 12 (0-indexed 11)
	copy(card[valStart:], padded)
	closeIdx := valStart + len(padded)
	if closeIdx > cardSize-1 {
		return [cardSize]byte{}, fmt.Errorf("header: retarget value %q does not fit the 70-column value field", value)
	}
	card[closeIdx] = '\'' // closing quote
	return card, nil
}
