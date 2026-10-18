package model

// Astrometry carries a frame's pointing/orientation, either extracted from the
// WCS the ASIAIR embeds in a light-frame header (Tier 0) or, in Tier 1, produced
// by an actual plate solve. It is stored per frame as a JSON blob (the
// frames.astrometry column), with Status mirrored into frames.wcs_status so the
// work-queues are indexable without json_extract.
//
// Presence is carried structurally, never by a zero sentinel — RA 0°, Dec 0°, and
// rotation 0° are all legitimate values: a nil *Astrometry means "not processed",
// a nil Solution means "processed, no embedded WCS", and each raw hint is a pointer
// so an absent card is distinguishable from a real 0.
type Astrometry struct {
	Status   string       `json:"status"`             // AstroStatus* below
	Solution *WCSSolution `json:"solution,omitempty"` // nil unless a WCS exists
	Hints    *AstroHints  `json:"hints,omitempty"`    // nil unless input cards present

	// Tier-1 solve bookkeeping (all optional, absorbed by the JSON blob so no
	// store migration is needed). Reason explains a failed/unsolvable state;
	// SolvedAt stamps a solve's provenance; Attempts budgets a per-frame poison
	// frame to terminal failure; FOVDeg records the frame's own field height so a
	// below-catalog-floor unsolvable can be re-queued once a deeper catalog lands.
	Reason   string  `json:"reason,omitempty"`
	SolvedAt int64   `json:"solved_at,omitempty"`
	Attempts int     `json:"attempts,omitempty"`
	FOVDeg   float64 `json:"fov_deg,omitempty"`
}

// Astrometry lifecycle values. header/none are produced by Tier 0; solving/solved/
// failed/unsolvable are the Tier-1 solver's transitions out of none. No schema
// migration is needed — the blob absorbs them. wcs_status='none' AND
// frame_type='LIGHT' is the Tier-1 solve queue.
const (
	AstroStatusHeader     = "header"     // embedded WCS extracted (terminal)
	AstroStatusNone       = "none"       // no embedded WCS (calibration, or unsolved-on-mount light)
	AstroStatusSolving    = "solving"    // Tier 1 claimed the row; transient, reclaimed on startup
	AstroStatusSolved     = "solved"     // Tier 1 produced a solution (terminal)
	AstroStatusFailed     = "failed"     // Tier 1 ran and found no match — never retried
	AstroStatusUnsolvable = "unsolvable" // Tier 1 refused: no hint, or FOV below the catalog floor
)

// WCSSolution is the displayable astrometric summary derived from a tangent-plane
// WCS: the image-center sky coordinate, rotation, scale, and field of view. SIP
// distortion is ignored — it does not affect these center/scale/rotation values.
type WCSSolution struct {
	CenterRA      float64 `json:"center_ra"`   // deg, [0,360)
	CenterDec     float64 `json:"center_dec"`  // deg, [-90,90]
	PositionAngle float64 `json:"pos_angle"`   // deg, [0,180) — sensor-axis orientation (180°-folded)
	PixelScale    float64 `json:"pixel_scale"` // arcsec/px
	FOVWidth      float64 `json:"fov_w"`       // deg (0 when NAXIS absent)
	FOVHeight     float64 `json:"fov_h"`       // deg (0 when NAXIS absent)
	Flipped       bool    `json:"flipped"`     // mirror parity: det(CD) > 0
}

// AstroHints are the raw solve seeds a Tier-1 solver needs — object pointing and
// optics — read independently of any solution. Each field is a pointer so an absent
// or non-numeric card is distinguishable from a real 0.
type AstroHints struct {
	RA          *float64 `json:"ra,omitempty"`            // deg (object)
	Dec         *float64 `json:"dec,omitempty"`           // deg (object)
	FocalLenMM  *float64 `json:"focal_len_mm,omitempty"`  // mm
	PixelSizeUm *float64 `json:"pixel_size_um,omitempty"` // µm per binned pixel (XPIXSZ includes binning)
}
