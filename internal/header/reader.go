// Package header reads FITS primary-HDU headers. It parses only the leading
// 2880-byte header blocks — the 80-column card images up to the END card — and
// never decodes the data unit; the digests of datadigest.go hash those bytes as
// they pass but never interpret them. This is a deliberate deviation from the seam
// table's astrogo/fitsio real impl: fitsio.Open eagerly decodes the full
// ~120 MB data unit into memory (there is no lazy header read), so the router's
// classify step, which needs only a handful of cards, would pay a 120 MB
// allocation per frame. Reading the block-aligned header directly is both
// correct and cheap, and lets the test fixtures be tiny header-only files.
package header

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
)

const (
	blockSize = 2880 // a FITS header is a whole number of these blocks
	cardSize  = 80   // each card image is exactly 80 columns
	keyLen    = 8    // the keyword occupies columns 1-8

	// maxHeaderBlocks bounds how far we scan for the END card. A conforming
	// header ends well before this; the cap only exists so a malformed file
	// with no END errors out instead of us walking into (and reading) a huge
	// data unit. 100 blocks is 288 KB — orders of magnitude below any data unit.
	maxHeaderBlocks = 100
)

// Reader is the HeaderReader seam: it turns a local file path into a parsed
// model.Header. The real implementation reads header blocks; tests use Fake.
type Reader interface {
	ReadHeader(path string) (model.Header, error)
}

// New returns the real header-block Reader.
func New() Reader { return blockReader{} }

// blockReader parses FITS header blocks off local disk.
type blockReader struct{}

// ReadHeader reads 2880-byte blocks, parses their 80-column cards, and stops at
// the END card without ever reading the data unit. A truncated or non-FITS file
// (no END within the block budget) returns an error so the caller can file the
// frame as unclassified; it never panics on malformed input.
func (blockReader) ReadHeader(path string) (model.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.Header{}, fmt.Errorf("header: open %s: %w", path, err)
	}
	defer f.Close()

	var cards []model.Card
	buf := make([]byte, blockSize)
	for block := 0; block < maxHeaderBlocks; block++ {
		if _, err := io.ReadFull(f, buf); err != nil {
			return model.Header{}, fmt.Errorf("header: %s: end of file before END card: %w", path, err)
		}
		for i := 0; i+cardSize <= blockSize; i += cardSize {
			card := string(buf[i : i+cardSize])
			key := model.NormalizeKey(card[:keyLen])
			if key == "END" {
				return model.NewHeader(cards), nil
			}
			if c, ok := parseCard(card, key); ok {
				cards = append(cards, c)
			}
		}
	}
	return model.Header{}, fmt.Errorf("header: %s: no END card within %d header blocks", path, maxHeaderBlocks)
}

// parseCard turns one 80-byte card image into a model.Card. key is the already
// normalized columns 1-8. It returns ok=false for blank padding cards.
func parseCard(card, key string) (model.Card, bool) {
	if key == "" {
		return model.Card{}, false // blank/padding card
	}
	// A value card carries the "= " value indicator in columns 9-10; anything
	// else (COMMENT, HISTORY, ...) is commentary and has no typed value.
	if card[8] == '=' && card[9] == ' ' {
		value, comment := parseValue(card[10:])
		return model.Card{Key: key, Value: value, Comment: comment}, true
	}
	return model.Card{Key: key, Value: strings.TrimSpace(card[8:])}, true
}

// parseValue parses the value field (columns 11-80) of a value card, returning
// the typed value and the trailing comment. It follows FITS value conventions:
// quoted strings with the doubled-single-quote escape and trailing-pad trimming,
// logical T/F, integers, and floats (including "300.", exponent forms, and the
// Fortran "D" exponent).
func parseValue(field string) (value any, comment string) {
	i := 0
	for i < len(field) && field[i] == ' ' {
		i++
	}
	if i >= len(field) {
		return "", "" // blank value field
	}

	if field[i] == '\'' {
		return parseString(field, i)
	}

	// Unquoted value: everything up to an unquoted '/' is the value token, the
	// rest is the comment.
	rest := field[i:]
	token := rest
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		token = rest[:slash]
		comment = strings.TrimSpace(rest[slash+1:])
	}
	return parseScalar(strings.TrimSpace(token)), comment
}

// parseString reads a FITS quoted string starting at the opening quote index,
// unescaping doubled single quotes and right-trimming FITS pad spaces, then
// extracts any trailing comment.
func parseString(field string, i int) (value any, comment string) {
	var sb strings.Builder
	i++ // skip opening quote
	for i < len(field) {
		if field[i] == '\'' {
			if i+1 < len(field) && field[i+1] == '\'' {
				sb.WriteByte('\'') // doubled quote escapes one quote
				i += 2
				continue
			}
			i++ // closing quote
			break
		}
		sb.WriteByte(field[i])
		i++
	}
	// FITS pads the string with trailing spaces inside the quotes; those are not
	// content. Internal spaces (e.g. "ZWO ASI6200MM Pro") are preserved.
	value = strings.TrimRight(sb.String(), " ")
	if rest := field[i:]; strings.ContainsRune(rest, '/') {
		comment = strings.TrimSpace(rest[strings.IndexByte(rest, '/')+1:])
	}
	return value, comment
}

// parseScalar types an unquoted value token as bool, int64, or float64, falling
// back to the raw string when it matches none of those.
func parseScalar(tok string) any {
	if tok == "" {
		return ""
	}
	if tok == "T" {
		return true
	}
	if tok == "F" {
		return false
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		return f
	}
	// FITS permits a Fortran "D" exponent (e.g. 1.23D-4); Go's parser wants "E".
	if strings.ContainsAny(tok, "Dd") {
		swapped := strings.Map(func(r rune) rune {
			if r == 'D' || r == 'd' {
				return 'E'
			}
			return r
		}, tok)
		if f, err := strconv.ParseFloat(swapped, 64); err == nil {
			return f
		}
	}
	return tok
}

// PIIDenylist is the design architecture §14.2 privacy guard: header keywords
// that would leak the observing site or observer identity and must never appear
// in committed fixtures. It is a package var so callers (and
// TestFixturesCarryNoPII) can extend it. Matching is case-insensitive, and any
// key starting "GPS" is also treated as PII regardless of this list.
var PIIDenylist = []string{
	"SITELAT", "SITELONG", "SITEELEV",
	"ALT-OBS", "LAT-OBS", "LONG-OBS",
	"OBSERVER", "SITENAME", "OBSERVAT",
	"LATITUDE", "LONGITUDE",
}
