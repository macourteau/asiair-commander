package model

// HeaderEdit is one edited header card of a frame's archive copy (design
// header-edits §5.1).
type HeaderEdit struct {
	// Key is the FITS keyword, as the edit allowlist spells it (upper case).
	Key string `json:"key"`
	// Value is the folded, sanitised value the edit wrote, and the one a re-apply
	// writes again.
	Value string `json:"value"`
	// From is the device copy's original value, nil when the device copy carries
	// no such card. It records the FIRST edit's original and never moves after.
	From *string `json:"from"`
}

// HeaderEdits is a frame's edit overlay, in the order each key was first edited.
// The order is load-bearing: a re-apply inserts absent cards in list order, which
// is what makes a one-pass re-apply byte-identical to an archive edited in several
// steps (design header-edits §5.1).
type HeaderEdits []HeaderEdit

// Lookup returns the entry for key, matched exactly.
func (e HeaderEdits) Lookup(key string) (HeaderEdit, bool) {
	for _, h := range e {
		if h.Key == key {
			return h, true
		}
	}
	return HeaderEdit{}, false
}

// Set returns the overlay with key's value set. A key already edited keeps its
// position and its From, so from is used only when key is new, which is appended.
// The receiver is never modified: Frame values are copied freely, and a shared
// backing array would let one copy's edit leak into another.
func (e HeaderEdits) Set(key, value string, from *string) HeaderEdits {
	out := make(HeaderEdits, len(e), len(e)+1)
	copy(out, e)
	for i := range out {
		if out[i].Key == key {
			out[i].Value = value
			return out
		}
	}
	return append(out, HeaderEdit{Key: key, Value: value, From: from})
}
