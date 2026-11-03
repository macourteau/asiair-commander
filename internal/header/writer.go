package header

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
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

// maxFilterLen bounds a FILTER value at the same 60 characters as OBJECT, for the
// same headroom reason (design header-edits §3).
const maxFilterLen = 60

// minValueChars is the FITS fixed-format minimum for a string value: the value
// is blank-padded so the closing quote lands at column >= 20 (design
// routing-and-filing §9.1).
const minValueChars = 8

// cardsPerBlock is how many card images one header block holds.
const cardsPerBlock = blockSize / cardSize

// EditableKey is one entry of the EditableKeys allowlist: whether the writer may
// insert the card when the header lacks it, and the value rule an edit must pass.
type EditableKey struct {
	// Insertable permits EditCards to insert the card before END when the header
	// carries no value card for the key (design header-edits §4.1 step 2).
	Insertable bool
	normalize  func(string) (string, error)
}

// Normalize applies the key's value rule (design header-edits §3) and returns the
// value exactly as it will be written into the card, or an error when the value
// falls outside the rule. The API boundary and the writer share it, so a value
// the API accepts is a value the writer accepts.
func (k EditableKey) Normalize(value string) (string, error) { return k.normalize(value) }

// EditableKeys is the allowlist of header keywords an edit may set (design
// header-edits §3). A key absent from it is refused by EditCards and by the API;
// the excluded classes (structural, measured or routing-critical, descriptive) and
// why each is excluded are recorded in that section, so extend this deliberately.
var EditableKeys = map[string]EditableKey{
	// OBJECT keeps retargeting's sanitised value because the card and the folder
	// must agree (design routing-and-filing §9.3 step 5). A frame without an OBJECT
	// card is not classified as a target, so the card is never inserted (§9.8).
	"OBJECT": {Insertable: false, normalize: normalizeObject},
	// FILTER is stored verbatim for stacking software, which groups on the string
	// as written; rendering sanitises the folder segment on its own. A mono camera
	// without a filter wheel writes no FILTER card, so it is inserted when absent.
	"FILTER": {Insertable: true, normalize: normalizeFilter},
}

func normalizeObject(v string) (string, error) {
	target := sanitize.Component(v)
	if target == "" {
		return "", fmt.Errorf("header: OBJECT value is empty after sanitize")
	}
	if len([]rune(target)) > maxTargetLen {
		return "", fmt.Errorf("header: OBJECT value %q exceeds %d characters", target, maxTargetLen)
	}
	return target, nil
}

func normalizeFilter(v string) (string, error) {
	f := strings.TrimSpace(v)
	if f == "" {
		return "", fmt.Errorf("header: FILTER value is empty")
	}
	if len(f) > maxFilterLen {
		return "", fmt.Errorf("header: FILTER value %q exceeds %d characters", f, maxFilterLen)
	}
	// Printable ASCII is the FITS string character set; anything else would make
	// a card other readers reject.
	for i := 0; i < len(f); i++ {
		if f[i] < 0x20 || f[i] > 0x7E {
			return "", fmt.Errorf("header: FILTER value %q has a character outside printable ASCII", f)
		}
	}
	return f, nil
}

// CardEdit sets the value card Key to Value. Value is the operator's input; the
// key's rule in EditableKeys turns it into what is written.
type CardEdit struct {
	Key   string
	Value string
}

// EditResult reports the outcome of EditCards (design header-edits §4.2).
//
// OutputSize is the length of dst and OutputSHA256 the SHA-256 over every byte
// written to it, so the caller can prove the at-rest read-back matches what was
// streamed. The proof digests of design header-edits §4.3 are computed in the same
// pass over the writer's own buffers: CardComplementIn/Out hash each stream's cards
// in order up to END, skipping every value card whose key was edited, and
// DataIn/Out hash each stream's data unit exactly as the data digest does (from the
// END-derived offset to end of file). They are a self-consistency check of the
// writer; the caller re-proves the edit from the files at rest.
type EditResult struct {
	OutputSize        int64
	OutputSHA256      [sha256.Size]byte
	CardComplementIn  [sha256.Size]byte
	CardComplementOut [sha256.Size]byte
	DataIn            [sha256.Size]byte
	DataOut           [sha256.Size]byte
}

// plannedEdit is one validated edit: its normalized key and canonical card.
type plannedEdit struct {
	key  string
	card [cardSize]byte
}

// EditCards streams src to dst with the allowlisted cards in edits set (design
// header-edits §4.1). Each key with exactly one value card has that card replaced
// in place by its canonical card; each insertable key with no value card has its
// canonical card inserted immediately before END, in the order of edits. END
// follows the last card and the rest of its block is blank cards, so an insertion
// that overflows the END block grows the header by whole blocks. Every other card,
// and the data unit with its trailing padding, is copied verbatim; the data unit is
// never decoded. dst is fsync'd before returning so the durable file precedes any
// rename.
//
// It refuses, before creating dst, when: edits is empty; a key is not on the
// allowlist or appears twice; a value fails its key's rule or its card does not
// fit the value field; a key has more than one value card; a key that may not be
// inserted has no value card; an insertion would overwrite non-blank bytes after
// END; the source has no END within maxHeaderBlocks; or the grown header would
// exceed it. A replace-only edit leaves the post-END padding where it is and does
// not inspect it, so every frame retargeting accepts stays editable.
func EditCards(src, dst string, edits []CardEdit) (EditResult, error) {
	plan, err := planEdits(edits)
	if err != nil {
		return EditResult{}, err
	}

	in, err := os.Open(src)
	if err != nil {
		return EditResult{}, fmt.Errorf("header: open %s: %w", src, err)
	}
	defer in.Close()

	// Buffer the leading header blocks through the one holding END. The header is
	// bounded by maxHeaderBlocks, so buffering it is cheap and lets every refusal
	// happen before dst exists.
	var headerBytes []byte
	endIdx := -1
	buf := make([]byte, blockSize)
	for block := 0; block < maxHeaderBlocks && endIdx < 0; block++ {
		if _, err := io.ReadFull(in, buf); err != nil {
			return EditResult{}, fmt.Errorf("header: %s: end of file before END card: %w", src, err)
		}
		headerBytes = append(headerBytes, buf...)
		for i := 0; i < cardsPerBlock; i++ {
			if model.NormalizeKey(string(buf[i*cardSize:i*cardSize+keyLen])) == "END" {
				endIdx = block*cardsPerBlock + i
				break
			}
		}
	}
	if endIdx < 0 {
		return EditResult{}, fmt.Errorf("header: %s: no END card within %d header blocks", src, maxHeaderBlocks)
	}

	edited := make(map[string]bool, len(plan))
	for _, p := range plan {
		edited[p.key] = true
	}
	// Locate each edited key's value card; a key with none is an insertion.
	replaceAt := make(map[int][cardSize]byte, len(plan))
	var inserts [][cardSize]byte
	for _, p := range plan {
		var found []int
		for i := 0; i < endIdx; i++ {
			c := headerBytes[i*cardSize : (i+1)*cardSize]
			if isValueCard(c) && model.NormalizeKey(string(c[:keyLen])) == p.key {
				found = append(found, i)
			}
		}
		switch {
		case len(found) > 1:
			return EditResult{}, fmt.Errorf("header: %s: %d %s value cards; refusing to guess", src, len(found), p.key)
		case len(found) == 1:
			replaceAt[found[0]] = p.card
		case !EditableKeys[p.key].Insertable:
			return EditResult{}, fmt.Errorf("header: %s: no %s value card before END, and %s is not inserted", src, p.key, p.key)
		default:
			inserts = append(inserts, p.card)
		}
	}

	endCard := headerBytes[endIdx*cardSize : (endIdx+1)*cardSize]
	var outHeader []byte
	outEnd := endIdx
	if len(inserts) == 0 {
		// Replace-only: the header keeps its length and its padding byte for byte.
		outHeader = append([]byte(nil), headerBytes...)
	} else {
		// An insertion overwrites the padding after END, so it must be padding the
		// writer could have created itself (design header-edits §4.1).
		if pad := headerBytes[(endIdx+1)*cardSize:]; len(bytes.Trim(pad, " ")) != 0 {
			return EditResult{}, fmt.Errorf("header: %s: non-blank bytes after END; refusing to overwrite them with an inserted card", src)
		}
		outHeader = append([]byte(nil), headerBytes[:endIdx*cardSize]...)
		for _, c := range inserts {
			outHeader = append(outHeader, c[:]...)
		}
		outEnd = endIdx + len(inserts)
		outHeader = append(outHeader, endCard...)
		for len(outHeader)%blockSize != 0 {
			outHeader = append(outHeader, ' ')
		}
		if len(outHeader) > maxHeaderBlocks*blockSize {
			return EditResult{}, fmt.Errorf("header: %s: inserting %d cards would grow the header past %d blocks", src, len(inserts), maxHeaderBlocks)
		}
	}
	for i, c := range replaceAt {
		copy(outHeader[i*cardSize:], c[:])
	}

	out, err := os.Create(dst)
	if err != nil {
		return EditResult{}, fmt.Errorf("header: create %s: %w", dst, err)
	}
	// On any error past this point the partial dst is the caller's to clean up (it
	// owns the temp path), matching restoreNAS's defer-remove discipline.

	hFull := sha256.New()
	hFull.Write(outHeader)
	if _, err := out.Write(outHeader); err != nil {
		out.Close()
		return EditResult{}, fmt.Errorf("header: write %s: %w", dst, err)
	}

	// Copy the data unit verbatim. in is positioned at the first data byte, the
	// block boundary after END, which is the END-derived offset the data digest
	// starts at; outHeader likewise ends on the block holding its END. The data
	// digests of both streams therefore see the same bytes the copy writes.
	hDataIn, hDataOut := sha256.New(), sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, hFull, hDataIn, hDataOut), in, make([]byte, 32*1024))
	if err != nil {
		out.Close()
		return EditResult{}, fmt.Errorf("header: copy data unit to %s: %w", dst, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return EditResult{}, fmt.Errorf("header: fsync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return EditResult{}, fmt.Errorf("header: close %s: %w", dst, err)
	}

	res := EditResult{OutputSize: int64(len(outHeader)) + n}
	sumInto(&res.OutputSHA256, hFull)
	res.CardComplementIn = cardComplement(headerBytes, endIdx, edited)
	res.CardComplementOut = cardComplement(outHeader, outEnd, edited)
	sumInto(&res.DataIn, hDataIn)
	sumInto(&res.DataOut, hDataOut)
	return res, nil
}

// planEdits validates edits against the allowlist and renders each canonical card,
// so that a bad edit refuses before the source is even opened.
func planEdits(edits []CardEdit) ([]plannedEdit, error) {
	if len(edits) == 0 {
		return nil, errors.New("header: no card edits")
	}
	plan := make([]plannedEdit, 0, len(edits))
	seen := make(map[string]bool, len(edits))
	for _, e := range edits {
		key := model.NormalizeKey(e.Key)
		rule, ok := EditableKeys[key]
		if !ok {
			return nil, fmt.Errorf("header: %q is not an editable key", e.Key)
		}
		if seen[key] {
			return nil, fmt.Errorf("header: %s appears more than once in the edits", key)
		}
		seen[key] = true
		v, err := rule.Normalize(e.Value)
		if err != nil {
			return nil, err
		}
		c, err := CanonicalCard(key, v)
		if err != nil {
			return nil, err
		}
		plan = append(plan, plannedEdit{key: key, card: c})
	}
	return plan, nil
}

// isValueCard reports whether a card carries the "= " value indicator in columns
// 9-10; a commentary card that happens to key an edited keyword is not a value.
func isValueCard(c []byte) bool { return c[keyLen] == '=' && c[keyLen+1] == ' ' }

// cardComplement is the card complement of design header-edits §4.3: a SHA-256 over
// the header's cards before END, in order, skipping every value card whose key is
// in edited. Equal digests across source and edit mean every other card survived,
// unchanged and in its order.
func cardComplement(header []byte, end int, edited map[string]bool) [sha256.Size]byte {
	h := sha256.New()
	for i := 0; i < end; i++ {
		c := header[i*cardSize : (i+1)*cardSize]
		if isValueCard(c) && edited[model.NormalizeKey(string(c[:keyLen]))] {
			continue
		}
		h.Write(c)
	}
	var out [sha256.Size]byte
	sumInto(&out, h)
	return out
}

func sumInto(dst *[sha256.Size]byte, h hash.Hash) { copy(dst[:], h.Sum(nil)) }

// CanonicalCard renders the canonical fixed-format value card for key and value
// (design header-edits §4.1): the key left-justified in columns 1-8, "= " in 9-10,
// the value quoted with embedded single quotes doubled and blank-padded to the
// FITS minimum (closing quote at column >= 20), the card space-padded to 80. Any
// comment is dropped, keeping the edit deterministic. value is written as given —
// the key's rule in EditableKeys is the caller's to apply first. It errors on a key
// that is not a FITS keyword and on a value whose escaped form cannot fit the
// value field.
//
// FROZEN FORMAT — changing these bytes breaks re-apply and restore of every
// previously edited frame. Re-apply (design header-edits §6, routing-and-filing
// §9.4) and restore reproduce the canonical card byte for byte and require the
// result to hash back to the stored edit anchor; any change to the padding,
// quoting or layout here would make an already edited frame fail to re-verify.
// TestBuildObjectCard_GoldenBytes pins the OBJECT output, which must stay the bytes
// retargeting has always written, and TestCanonicalCard_GoldenBytes pins the
// generic layout, so a change fails loudly rather than silently corrupting the
// anchor.
func CanonicalCard(key, value string) ([cardSize]byte, error) {
	if !isFITSKeyword(key) {
		return [cardSize]byte{}, fmt.Errorf("header: %q is not a FITS keyword", key)
	}
	escaped := strings.ReplaceAll(value, "'", "''")
	padded := escaped
	if len(padded) < minValueChars {
		padded += strings.Repeat(" ", minValueChars-len(padded))
	}

	var card [cardSize]byte
	for i := range card {
		card[i] = ' '
	}
	copy(card[0:], key)
	card[keyLen] = '='     // column 9
	card[keyLen+1] = ' '   // column 10
	card[keyLen+2] = '\''  // opening quote, column 11
	valStart := keyLen + 3 // column 12 (0-indexed 11)
	closeIdx := valStart + len(padded)
	if closeIdx > cardSize-1 {
		return [cardSize]byte{}, fmt.Errorf("header: %s value %q does not fit the 70-column value field", key, value)
	}
	copy(card[valStart:], padded)
	card[closeIdx] = '\'' // closing quote
	return card, nil
}

// buildObjectCard is the canonical OBJECT card retargeting writes. It is kept as
// its own name so TestBuildObjectCard_GoldenBytes keeps pinning exactly the bytes
// every retargeted frame on disk carries (design header-edits HE-5).
func buildObjectCard(value string) ([cardSize]byte, error) { return CanonicalCard("OBJECT", value) }

// isFITSKeyword reports whether key is 1-8 characters of the FITS keyword set
// (upper-case letters, digits, hyphen, underscore).
func isFITSKeyword(key string) bool {
	if key == "" || len(key) > keyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// RawHeader is a header as raw 80-byte card images (design header-edits §4.3, "Raw
// cards" and "Padding"). Cards runs from the first card through END inclusive, so
// End is len(Cards)-1; Padding is every byte after the END card up to DataOffset,
// the block boundary where the data unit begins.
type RawHeader struct {
	Cards      [][cardSize]byte
	End        int
	Padding    []byte
	DataOffset int64
}

// ReadRawHeader reads path's header as raw card images. It is the edit proof's
// reader and deliberately shares no code with EditCards: the worker uses it to
// re-prove from the files at rest what the writer's own digests only claim.
// ReadHeader cannot serve, because its parsed map collapses repeated keys, drops
// blank cards and normalises values and comments, so it cannot see a dropped
// COMMENT or a reformatted card. It errors when there is no END within
// maxHeaderBlocks or the file ends inside the block holding END.
func ReadRawHeader(path string) (RawHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return RawHeader{}, fmt.Errorf("header: open %s: %w", path, err)
	}
	defer f.Close()

	var rh RawHeader
	buf := make([]byte, blockSize)
	for block := 0; block < maxHeaderBlocks; block++ {
		if _, err := io.ReadFull(f, buf); err != nil {
			return RawHeader{}, fmt.Errorf("header: %s: end of file before END card: %w", path, err)
		}
		for off := 0; off < blockSize; off += cardSize {
			var c [cardSize]byte
			copy(c[:], buf[off:off+cardSize])
			rh.Cards = append(rh.Cards, c)
			if model.NormalizeKey(string(c[:keyLen])) == "END" {
				rh.End = len(rh.Cards) - 1
				rh.Padding = append([]byte(nil), buf[off+cardSize:]...)
				rh.DataOffset = int64(block+1) * blockSize
				return rh, nil
			}
		}
	}
	return RawHeader{}, fmt.Errorf("header: %s: no END card within %d header blocks", path, maxHeaderBlocks)
}
