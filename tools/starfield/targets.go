package starfield

import "math"

// Camera is a mono astronomy camera as the ASIAIR describes it in a frame header.
type Camera struct {
	Model     string  // INSTRUME, e.g. "ZWO ASI533MM Pro"
	Short     string  // the sensor segment of an ASIAIR frame name, e.g. "533MM"
	Width     int     // unbinned sensor width, pixels
	Height    int     // unbinned sensor height, pixels
	PixelUm   float64 // unbinned pixel pitch, microns
	Gain      int     // GAIN the frames are shot at
	Offset    int     // OFFSET the frames are shot at
	EGain     float64 // electrons per 16-bit ADU at Gain (EGAIN)
	ReadNoise float64 // read noise at Gain, electrons
}

// Optics is a telescope and the camera behind it, binned as the frames are shot.
type Optics struct {
	FocalLengthMm float64
	ApertureMm    float64
	Spikes        bool // a Newtonian's secondary spider draws diffraction spikes
	Camera        Camera
	Bin           int
}

// Width and Height are the binned frame dimensions in pixels.
func (o Optics) Width() int  { return o.Camera.Width / o.Bin }
func (o Optics) Height() int { return o.Camera.Height / o.Bin }

// PixelUm is the binned pixel pitch, the XPIXSZ an ASIAIR writes.
func (o Optics) PixelUm() float64 { return o.Camera.PixelUm * float64(o.Bin) }

// ScaleDeg is the plate scale in degrees per binned pixel.
func (o Optics) ScaleDeg() float64 {
	return math.Atan(o.PixelUm()*1e-3/o.FocalLengthMm) * 180 / math.Pi
}

// FOV is the field of view, width and height, in degrees.
func (o Optics) FOV() (w, h float64) {
	s := o.ScaleDeg()
	return s * float64(o.Width()), s * float64(o.Height())
}

// Target is one field: what is pointed at, how it is framed and exposed, and how
// deep the catalog cone behind it goes.
type Target struct {
	Name        string  // OBJECT and the frame-name target segment
	RA, Dec     float64 // field centre, ICRS degrees
	RotationDeg float64 // camera angle; 0 renders north up, east left
	Optics      Optics
	Exposure    float64 // seconds
	Filter      string
	MagLimit    float64 // faintest G magnitude fetched and rendered
}

// ConeRadius is the catalog cone that covers the whole frame: half its diagonal
// plus a margin, so a star just outside a corner still lands its halo inside.
func (t Target) ConeRadius() float64 {
	w, h := t.Optics.FOV()
	return math.Hypot(w, h)/2 + 0.05
}

// The ZWO ASI533MM Pro: a 3008x3008 3.76 µm 14-bit sensor, whose unity gain is
// 100, and the smallest cooled mono format in common use (a frame is about 18 MB).
var ASI533MM = Camera{
	Model: "ZWO ASI533MM Pro", Short: "533MM",
	Width: 3008, Height: 3008, PixelUm: 3.76,
	Gain: 100, Offset: 50, EGain: 0.2471, ReadNoise: 1.5,
}

// The two rigs the demo runs: an Askar FRA300 Pro (300 mm f/5 refractor) for the
// wide clusters, and a 200 mm f/4 Newtonian for a globular.
var (
	FRA300 = Optics{FocalLengthMm: 300, ApertureMm: 60, Camera: ASI533MM, Bin: 1}
	N200   = Optics{FocalLengthMm: 800, ApertureMm: 200, Spikes: true, Camera: ASI533MM, Bin: 1}
)

// Targets are the fields the demo shoots. Each is recognisable from its stars
// alone (the renderer draws no nebulosity) at thumbnail size: the Pleiades'
// dipper, the Double Cluster's pair, the Coathanger's bar and hook, the
// Beehive's loose swarm, and the Hercules globular's resolved ball.
var Targets = []Target{
	{Name: "M45", RA: 56.75, Dec: 24.12, RotationDeg: 4, Optics: FRA300, Exposure: 60, Filter: "L", MagLimit: 14.5},
	{Name: "Double Cluster", RA: 35.15, Dec: 57.13, RotationDeg: 354, Optics: FRA300, Exposure: 60, Filter: "L", MagLimit: 14.5},
	// Rotated about half a turn so the hook sits above the bar, the way the
	// asterism gets its name; north-up it hangs upside down. No framing is exactly
	// square to the sky, so every target carries a few degrees of camera angle.
	{Name: "Coathanger", RA: 291.55, Dec: 20.12, RotationDeg: 182, Optics: FRA300, Exposure: 60, Filter: "L", MagLimit: 14.5},
	{Name: "M44", RA: 130.10, Dec: 19.67, RotationDeg: 8, Optics: FRA300, Exposure: 60, Filter: "L", MagLimit: 14.5},
	{Name: "M13", RA: 250.4235, Dec: 36.4613, RotationDeg: 357, Optics: N200, Exposure: 30, Filter: "L", MagLimit: 18},
}

// TargetByName returns the preset with this name.
func TargetByName(name string) (Target, bool) {
	for _, t := range Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}
