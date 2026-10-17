// Package model holds the shared domain value types that every other package
// speaks in terms of: parsed FITS headers, classifications, configuration, frame
// rows, alerts, and the live status snapshot. It performs no I/O and imports no
// other internal package, so it can be the common vocabulary without creating
// import cycles. The seam interfaces (Device, LocalFS, Store, ...) live in their
// own packages alongside their implementations; this package holds only the data
// that flows across those seams.
package model

import "strings"

// FrameType is the canonical, capture-software-independent kind of a frame,
// derived from the FITS IMAGETYP card through the alias table.
type FrameType string

const (
	// TypeUnknown means IMAGETYP had no alias-table entry. Such a frame is filed
	// to the unclassified area and never deleted.
	TypeUnknown FrameType = ""
	TypeLight   FrameType = "LIGHT"
	TypeDark    FrameType = "DARK"
	TypeFlat    FrameType = "FLAT"
	TypeBias    FrameType = "BIAS"
)

// CanonicalTypes are the four types the router knows how to route. Calibration
// covers DARK/FLAT/BIAS; LIGHT is the light path.
var CanonicalTypes = []FrameType{TypeLight, TypeDark, TypeFlat, TypeBias}

// IsCalibration reports whether the type is a calibration frame (filed by date,
// never night-bucketed).
func (t FrameType) IsCalibration() bool {
	return t == TypeDark || t == TypeFlat || t == TypeBias
}

// Known reports whether the type is one of the four canonical routed types.
func (t FrameType) Known() bool {
	return t == TypeLight || t == TypeDark || t == TypeFlat || t == TypeBias
}

// CanonicalizeType maps an arbitrary alias-table right-hand-side value to a
// canonical FrameType. It case-folds and trims so the alias table can be written
// in any case. An unrecognized value yields TypeUnknown.
func CanonicalizeType(s string) FrameType {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "LIGHT":
		return TypeLight
	case "DARK":
		return TypeDark
	case "FLAT":
		return TypeFlat
	case "BIAS":
		return TypeBias
	default:
		return TypeUnknown
	}
}
