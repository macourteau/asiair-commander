package model

// Marker file names. MediaMarkerName and IdentityMarkerName are written into
// ASIAIR shares and are the single source of truth: internal/mediaid (which
// reads/writes them) and internal/filter (which excludes them from device-side
// listings) both reference these so the names can never drift. LibraryMarkerName
// is different — it lives on a NAS destination root, not on the device, and is
// deliberately NOT registered in internal/filter (nothing lists a destination
// root through that filter).
const (
	// LibraryMarkerName identifies a linked NAS destination root for out-of-band
	// deletion reconciliation. It carries the library id this installation
	// recorded, and the scan only reconciles a root whose marker id matches.
	LibraryMarkerName = ".asiair-commander-library-id"
	// MediaMarkerName is the per-medium Udisk media-identity marker (guards a
	// swapped/absent removable stick; finding H10).
	MediaMarkerName = ".asiair-commander-media-id"
	// IdentityMarkerName is the name of a legacy per-rig identity marker. Device
	// identity is confirmed by a live scan_air GUID probe (design
	// device-identity-and-media §4), so nothing writes this marker; the constant
	// is retained only so a leftover marker file is excluded from listings
	// (internal/filter) and swept when a share is held.
	IdentityMarkerName = ".asiair-commander-rig-id"
)
