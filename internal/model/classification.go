package model

import "time"

// Classification is the header-derived, capture-software-independent description
// of a frame produced by the (pure) Classifier. It carries every value the
// Router may substitute into a template, each with a presence flag so the Router
// can decide whether a routing-critical token is resolvable (a missing critical
// token → quarantine, design routing-and-filing §2). It deliberately does NOT
// decide quarantine itself: which tokens are critical depends on the applicable
// template, which only the Router knows. The single classification-level failure
// it does own is an unknown IMAGETYP (Type == TypeUnknown, RawImageType names the
// offending value).
type Classification struct {
	Type FrameType

	// RawImageType is the normalized IMAGETYP string. When Type == TypeUnknown it
	// is the value that had no alias-table entry (used in the alert message).
	RawImageType string

	// Target is the raw OBJECT value (not yet path-sanitized; the Router sanitizes
	// and applies the strip-spaces option). Empty when OBJECT is absent.
	Target        string
	TargetPresent bool

	// Instrument is the raw INSTRUME value; Sensor is its sensor-map short name.
	// SensorMapped is false when INSTRUME is absent or has no map entry (an
	// unresolvable <SENSOR> whenever the template references it).
	Instrument   string
	Sensor       string
	SensorMapped bool

	// Filter is the raw FILTER value. FilterPresent is false on OSC cameras that
	// write no FILTER card — the one token allowed to collapse to empty (design
	// routing-and-filing §3).
	Filter        string
	FilterPresent bool

	// Exptime is EXPTIME in seconds (carries float noise verbatim; the Router
	// rounds to milliseconds for the <EXPOSURE> token).
	Exptime        float64
	ExptimePresent bool

	// SetTempC is the SET-TEMP set-point in degrees C (never CCD-TEMP). Kept as a
	// float for coercion generality; the <SET-TEMP> token renders it as a signed
	// integer.
	SetTempC       float64
	SetTempPresent bool

	Gain        int64
	GainPresent bool

	Offset        int64
	OffsetPresent bool

	// BinX/BinY come from XBINNING/YBINNING. When they differ the <BINNING> token
	// renders <X>x<Y>; otherwise Bin<n>.
	BinX, BinY int64
	BinPresent bool

	// DateObs is DATE-OBS parsed as UTC (the card carries no zone / trailing Z).
	// The Router converts it to the site-local observing date for the <DATE> token
	// and the night-window computation.
	DateObs        time.Time
	DateObsPresent bool
}
