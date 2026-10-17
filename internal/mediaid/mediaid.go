// Package mediaid implements the MEDIA marker that stands between this tool
// and misdirected deletes on removable storage.
//
// The MEDIA marker (Media, MediaMarkerName) guards a single removable medium —
// the ASIAIR's Udisk share reports a synthetic, constant volume serial with or
// without a USB stick inserted (finding H10), so a missing stick mounts
// successfully with an empty root, indistinguishable from "everything already
// transferred" by directory listing alone. The marker breaks that tie: written
// on first managed cleanup, it must read back unchanged on every later pass, or
// the pass treats the medium as absent/swapped rather than empty-by-transfer.
//
// Whole-device identity — is this the right rig? — is a separate question
// answered by a live scan_air GUID probe in the rig worker (design
// device-identity-and-media §4), not by a marker.
//
// The mechanism is transport-free: it reads and writes through the small Origin
// seam rather than any concrete filesystem or SMB type, so it is hermetically
// testable against an in-memory fake. A device.Share is adapted to an Origin by
// PrefixedOrigin, which roots every marker access at a fixed directory prefix —
// "ASIAIR" for the relocated media marker on removable media, "" for the legacy
// share-root fallback and for fixed media.
package mediaid

import (
	"errors"
	"io/fs"
	"path"
)

// Origin is the read/write seam a marker's caller supplies — Share.Open/
// Share.Create-backed on the real device, an in-memory map in tests. name
// selects the marker (MediaMarkerName) being read or written. ReadMarker
// returns an error matching fs.ErrNotExist when no marker of that name is
// present — absence is a distinct, expected outcome (first contact), never
// folded into "unreadable".
type Origin interface {
	ReadMarker(name string) ([]byte, error)
	WriteMarker(name string, data []byte) error
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// MarkerStore is the path-capable marker backing PrefixedOrigin adapts to an
// Origin. A device.Share satisfies it structurally, keeping mediaid transport-
// free. ReadMarker takes an already-joined subpath; WriteMarkerAt joins dir and
// name itself so a caller can pin the write directory.
type MarkerStore interface {
	ReadMarker(name string) ([]byte, error)
	WriteMarkerAt(dir, name string, data []byte) error
}

// prefixedOrigin adapts a MarkerStore to an Origin rooted at a fixed directory
// prefix: reads join the prefix onto the marker name, writes pin the store's
// write directory to the prefix. A prefix of "" is the store's own root.
type prefixedOrigin struct {
	store  MarkerStore
	prefix string
}

// PrefixedOrigin returns an Origin that reads and writes markers under prefix on
// store (prefix "" is the root).
func PrefixedOrigin(store MarkerStore, prefix string) Origin {
	return prefixedOrigin{store: store, prefix: prefix}
}

func (p prefixedOrigin) ReadMarker(name string) ([]byte, error) {
	return p.store.ReadMarker(path.Join(p.prefix, name))
}

func (p prefixedOrigin) WriteMarker(name string, data []byte) error {
	return p.store.WriteMarkerAt(p.prefix, name, data)
}
