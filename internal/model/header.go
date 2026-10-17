package model

import (
	"strconv"
	"strings"
)

// Card is one parsed FITS header record. Value is one of: string, int64,
// float64, or bool — never the raw interface{} soup fitsio would hand back
// untyped. Comment is the text after the '/' on the card (may be empty).
type Card struct {
	Key     string
	Value   any
	Comment string
}

// Header is a parsed FITS primary-HDU header: the leading 2880-byte blocks only,
// never the data unit. Keys are normalized to upper-case, trimmed. It preserves
// card order for diagnostics and round-tripping.
type Header struct {
	cards map[string]Card
	order []string
}

// NewHeader builds a Header from parsed cards. Later cards with a duplicate key
// win (FITS allows repeats; the last is the effective value), but order records
// first appearance.
func NewHeader(cards []Card) Header {
	h := Header{cards: make(map[string]Card, len(cards))}
	for _, c := range cards {
		k := NormalizeKey(c.Key)
		if k == "" {
			continue
		}
		if _, seen := h.cards[k]; !seen {
			h.order = append(h.order, k)
		}
		c.Key = k
		h.cards[k] = c
	}
	return h
}

// NormalizeKey upper-cases and trims a FITS keyword.
func NormalizeKey(k string) string {
	return strings.ToUpper(strings.TrimSpace(k))
}

// Card returns the raw card for a key (case-insensitive).
func (h Header) Card(key string) (Card, bool) {
	c, ok := h.cards[NormalizeKey(key)]
	return c, ok
}

// Has reports whether the key is present.
func (h Header) Has(key string) bool {
	_, ok := h.cards[NormalizeKey(key)]
	return ok
}

// Keys returns the keys in first-appearance order.
func (h Header) Keys() []string {
	out := make([]string, len(h.order))
	copy(out, h.order)
	return out
}

// Str returns a string value, trimmed. Non-string values are rendered in their
// canonical form. Returns false if the key is absent.
func (h Header) Str(key string) (string, bool) {
	c, ok := h.cards[NormalizeKey(key)]
	if !ok {
		return "", false
	}
	switch v := c.Value.(type) {
	case string:
		return strings.TrimSpace(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), true
	case bool:
		if v {
			return "T", true
		}
		return "F", true
	default:
		return "", false
	}
}

// Int returns an integer value, coercing from int64, float64 (must be integral),
// or a numeric string. This is the asInt of the design: fitsio may hand back
// EXPTIME/GAIN/SET-TEMP as either int64 or float64 depending on how the capture
// software wrote the card, and a bare type-assert would panic. Returns false if
// the key is absent or the value is not coercible to an integer.
func (h Header) Int(key string) (int64, bool) {
	c, ok := h.cards[NormalizeKey(key)]
	if !ok {
		return 0, false
	}
	return AsInt(c.Value)
}

// Float returns a floating value, coercing from float64, int64, or a numeric
// string (the asFloat of the design). Returns false if absent or non-numeric.
func (h Header) Float(key string) (float64, bool) {
	c, ok := h.cards[NormalizeKey(key)]
	if !ok {
		return 0, false
	}
	return AsFloat(c.Value)
}

// AsInt coerces a header value to int64. Accepts int64, an integral float64, or
// a numeric string. A non-integral float or non-numeric value returns false.
func AsInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		i := int64(x)
		if float64(i) == x {
			return i, true
		}
		return 0, false
	case string:
		s := strings.TrimSpace(x)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			i := int64(f)
			if float64(i) == f {
				return i, true
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

// AsFloat coerces a header value to float64. Accepts float64, int64, or a
// numeric string. A non-numeric value returns false.
func AsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f, true
		}
		return 0, false
	default:
		return 0, false
	}
}
