// Package route implements the pure Router: it turns a classified frame plus its
// rig profile into the destination DIRECTORY (relative to the light or cal root)
// by rendering the applicable path template. It is the correctness-critical
// routing engine — every value it substitutes is specified exactly (design
// routing-and-filing §3 token table) so the running app and its golden tests
// render byte-identical paths. It performs no I/O and only produces the
// directory; the filename and clash-disambiguation happen later at filing
// (design frame-lifecycle §8).
//
// The single decision it owns beyond rendering is quarantine: a frame whose
// applicable template references a routing-critical token that cannot be
// resolved is rejected with a *QuarantineError, which the worker maps to
// state=unclassified + an alert. "Routing-critical" is defined precisely as the
// tokens the applicable template actually references, plus — for a light — the
// night-derivation inputs OBJECT and DATE-OBS, regardless of the template.
package route

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/panel"
	"github.com/macourteau/asiair-commander/internal/sanitize"
)

// RootKind selects which managed root the caller joins the result Dir onto.
type RootKind int

const (
	// RootLight is the light root (roots.Light). LIGHT frames route here.
	RootLight RootKind = iota
	// RootCal is the calibration root (roots.Cal). DARK/FLAT/BIAS route here.
	RootCal
)

// Uncooled is what <SET-TEMP> renders for a frame carrying no SET-TEMP card.
const Uncooled = "uncooled"

// Options carries the rendering inputs the Router does not derive from the
// classification or rig profile itself.
type Options struct {
	Templates model.Templates
	// Night is the derived night ordinal for the <n> token. It is used only for
	// LIGHT frames; calibration is filed by date, never night-bucketed (design
	// routing-and-filing §6).
	Night int
	// ObservingDate is the local observing date (the date of the window's opening
	// local noon) rendered by the <DATE> token. Computed once by the caller via
	// ObservingDate so the same value feeds store.AssignNight and this token.
	ObservingDate time.Time
	// DateFormat is the Go layout for <DATE> (e.g. "2006-01-02").
	DateFormat string
	// StripTargetSpaces removes all internal spaces from <TARGET> when set.
	StripTargetSpaces bool
	// MosaicDetection opts into mosaic panel routing: a LIGHT whose transformed
	// target carries the ASIAIR planner's "_<row>-<col>" panel suffix routes
	// through Templates.Mosaic instead of Templates.Light. While it is off no
	// frame reaches the mosaic template and every routing outcome is what it
	// would be without the feature (INV-M4).
	MosaicDetection bool
}

// routeOpts is Options plus what Route derived once for the frame in hand: the
// parsed panel and whether the mosaic template is the one being rendered. It
// stays unexported so Options keeps its shape — every production caller builds a
// struct literal — while renderSegment and resolveToken still see the panel
// values without re-parsing them, which is also what keeps a mosaic-only token
// from rendering verbatim in a template that is not the mosaic one.
type routeOpts struct {
	Options
	mosaic     bool // the mosaic template is the active one
	panelBase  string
	panelToken string
	panelRow   int
	panelCol   int
}

// Result is the routed destination. Kind selects the root; Dir is the relative
// directory beneath it, forward-slash separated with every segment sanitized.
type Result struct {
	Kind RootKind
	Dir  string
}

// QuarantineError signals that the frame cannot be routed with confidence: a
// routing-critical token referenced by the applicable template is unresolvable.
// The worker maps it to state=unclassified plus an alert. MissingToken names the
// offending header input (best-effort, for the alert message) and may be empty
// when the failure is a whole segment collapsing rather than one token.
type QuarantineError struct {
	Reason       string
	MissingToken string
}

func (e *QuarantineError) Error() string {
	if e.MissingToken != "" {
		return fmt.Sprintf("route: quarantine: %s (missing %s)", e.Reason, e.MissingToken)
	}
	return "route: quarantine: " + e.Reason
}

// Route produces the destination directory for a classified frame. It selects
// the template by type (LIGHT → Templates.Light under RootLight; DARK/FLAT/BIAS
// → Templates.Cal[type] under RootCal, and — with mosaic detection on — a LIGHT
// whose target parsed as a panel → Templates.Mosaic, still under RootLight),
// renders each "/"-separated segment by substituting its tokens and sanitizing
// the result, and returns the joined relative path. A non-canonical type or a
// missing template is a plain error (the worker quarantines such frames before
// calling; this is a defensive guard). An unresolvable routing-critical token is
// a *QuarantineError.
func Route(cls model.Classification, rig model.RigProfile, opts Options) (Result, error) {
	tmpl, kind, ropts, err := selectTemplate(cls, opts)
	if err != nil {
		return Result{}, err
	}

	var segments []string
	for _, seg := range splitTemplate(tmpl) {
		rendered, drop, qerr := renderSegment(seg, cls, rig, ropts)
		if qerr != nil {
			return Result{}, qerr
		}
		if drop {
			continue
		}
		segments = append(segments, rendered)
	}

	return Result{Kind: kind, Dir: strings.Join(segments, "/")}, nil
}

// selectTemplate resolves which template a frame routes through, which root the
// rendered directory hangs off, and the per-frame rendering options derived on
// the way — the parsed mosaic panel above all. Route and NightFolder share it so
// the folder a night ordinal is read out of is rendered from the same template
// the frame is filed through; two copies of these conditions are free to
// disagree about that, which is exactly how a frame lands in a folder nobody
// consulted.
func selectTemplate(cls model.Classification, opts Options) (tmpl string, kind RootKind, ropts routeOpts, err error) {
	switch cls.Type {
	case model.TypeLight:
		tmpl = opts.Templates.Light
		kind = RootLight
	case model.TypeDark, model.TypeFlat, model.TypeBias:
		tmpl = opts.Templates.Cal[cls.Type]
		kind = RootCal
	default:
		return "", 0, routeOpts{}, fmt.Errorf("route: non-canonical frame type %q", cls.Type)
	}

	// A mosaic panel routes through its own template under the same light root.
	// The panel is parsed exactly once, here, and carried down to the tokens: a
	// second parse anywhere below could disagree with this one about which
	// template is being rendered. Templates.Mosaic may still be empty — an
	// unnormalized config — which the guard below reports as the missing
	// template it is.
	ropts = routeOpts{Options: opts}
	if base, token, row, col, ok := mosaicPanel(cls.Target, cls.Type, opts); ok {
		tmpl = opts.Templates.Mosaic
		ropts.mosaic = true
		ropts.panelBase, ropts.panelToken, ropts.panelRow, ropts.panelCol = base, token, row, col
	}

	if tmpl == "" {
		return "", 0, routeOpts{}, fmt.Errorf("route: no template configured for type %q", cls.Type)
	}

	// For lights, OBJECT and DATE-OBS are always routing-critical (they feed the
	// night derivation), regardless of which tokens the template names (design
	// routing-and-filing §6).
	if cls.Type == model.TypeLight {
		if !cls.TargetPresent {
			return "", 0, routeOpts{}, &QuarantineError{Reason: "light frame is missing OBJECT", MissingToken: "OBJECT"}
		}
		if !cls.DateObsPresent {
			return "", 0, routeOpts{}, &QuarantineError{Reason: "light frame is missing DATE-OBS", MissingToken: "DATE-OBS"}
		}
	}
	return tmpl, kind, ropts, nil
}

// splitTemplate splits a path template into the segments Route renders one by
// one. A template may be authored with either separator (an operator on Windows
// types "\"); collapse "\" to "/" first so a "\"-authored template splits into
// the same segments — and routes identically — as its "/"-authored twin. Saved
// configs are already canonicalized to "/", but the preview path renders
// candidate, not-yet-saved templates, so accept both here too. Empty segments
// are kept so a caller slicing the result stays aligned index-for-index with
// Route's own rendering pass.
func splitTemplate(tmpl string) []string {
	return strings.Split(strings.ReplaceAll(tmpl, `\`, "/"), "/")
}

// SplitAtNight returns a template's segments before its first <n>-naming
// segment, that segment itself, and false when no segment names <n>. It splits
// with Route's semantics so the prefix aligns index-for-index with the segment
// list Route renders.
//
// It is exported because internal/config's mosaic validation asks the same
// question of a template, and two implementations of "the segments above the
// night folder" that could disagree is the class of bug this whole area exists
// to eliminate.
func SplitAtNight(tmpl string) (before []string, nightSeg string, ok bool) {
	segs := splitTemplate(tmpl)
	for i, seg := range segs {
		if strings.Contains(seg, "<n>") {
			return segs[:i], seg, true
		}
	}
	return nil, "", false
}

// NightFolder renders the destination directory ABOVE a frame's night folder,
// plus nightName, which renders the night folder's own segment for a candidate
// ordinal. Together they let a caller ask the library which ordinal a folder
// already holds for an observing date, instead of deriving it from the night
// ledger's target key alone — the two can disagree, because the folder comes
// from the template and the ledger key comes from the raw OBJECT card.
//
// The caller cannot render the night segment itself: renderSegment, resolveToken
// and routeOpts are unexported, and the segment may name other tokens
// (<DATE>_NIGHT_<n>, <FILTER>_<n>). Options.Night is irrelevant to the prefix by
// construction — no segment above the first <n> can name it — so the prefix a
// caller gets here is exactly the prefix Route renders moments later.
//
// ok is false when the template names no <n> at all, when the prefix renders
// empty (a template whose first segment names <n>: the range would then span the
// whole library), or when the frame has no applicable template. In every such
// case the consultation is simply disabled and filing behaves as it does without
// it. A routing-critical token that cannot be resolved is returned as the same
// *QuarantineError Route derives from the same segments.
func NightFolder(cls model.Classification, rig model.RigProfile, opts Options) (
	prefix string, nightName func(n int) (string, error), ok bool, err error) {
	tmpl, _, ropts, serr := selectTemplate(cls, opts)
	if serr != nil {
		var qe *QuarantineError
		if errors.As(serr, &qe) {
			return "", nil, false, qe
		}
		// A non-canonical type or an unconfigured template is not a question about
		// folders: Route re-derives it moments later and the caller quarantines the
		// frame there, as it does with no consultation. Disabling the consultation
		// keeps that outcome instead of turning it into a deferral.
		return "", nil, false, nil
	}

	before, nightSeg, has := SplitAtNight(tmpl)
	if !has {
		return "", nil, false, nil
	}

	var segments []string
	for _, seg := range before {
		rendered, drop, qerr := renderSegment(seg, cls, rig, ropts)
		if qerr != nil {
			return "", nil, false, qerr
		}
		if drop {
			continue
		}
		segments = append(segments, rendered)
	}
	if len(segments) == 0 {
		return "", nil, false, nil
	}

	name := func(n int) (string, error) {
		nopts := ropts
		nopts.Night = n
		rendered, drop, qerr := renderSegment(nightSeg, cls, rig, nopts)
		if qerr != nil {
			return "", qerr
		}
		if drop {
			return "", &QuarantineError{Reason: "night segment " + strconv.Quote(nightSeg) + " rendered empty"}
		}
		return rendered, nil
	}
	return strings.Join(segments, "/"), name, true, nil
}

// IsMosaicPanel reports whether a frame carrying this OBJECT value and type
// would route through the mosaic template under opts. It takes the RAW target
// and applies the transforms itself, so it is callable from the places that
// hold a model.Frame and no model.Classification — one does not exist until a
// header is read, and two of its three consumers never read one.
//
// Those consumers are re-file's default scope, the Settings preview's LIGHT and
// MOSAIC row predicates, and previewRetarget's NightApprox template arm; all
// three need the answer route.Result cannot give, since it carries only the root
// and the rendered directory. It exists so none of them re-derives
// "detection && LIGHT && parses", and no routing or predicate decision anywhere
// may call panel.ParsePanel directly — the two deliberate raw-target callers are
// recordFiled and the store backfill, which persist the columns and route
// nothing.
func IsMosaicPanel(target string, t model.FrameType, opts Options) bool {
	_, _, _, _, ok := mosaicPanel(target, t, opts)
	return ok
}

// mosaicPanel is the single gate both Route and IsMosaicPanel ask, so the
// template a frame is filed through and every prediction of it agree by
// construction rather than by two copies of the same three conditions.
func mosaicPanel(target string, t model.FrameType, opts Options) (base, panelToken string, row, col int, ok bool) {
	// Detection is checked first and calibration second: a cal frame can carry a
	// stale OBJECT from the light it followed, and the mosaic template routes
	// under the LIGHT root, so a suffix match on one would file it in the wrong
	// tree entirely.
	if !opts.MosaicDetection || t != model.TypeLight {
		return "", "", 0, 0, false
	}
	return panel.ParsePanel(transformTarget(target, opts.StripTargetSpaces))
}

// transformTarget applies the two target transforms — the optional space strip,
// then the trim — in that order, and is the only place they live. <TARGET>
// renders its result and the panel suffix is split off that same result, which
// is what makes "<TARGET_BASE>_<PANEL>" and "<TARGET>" render byte-identical
// directories: reassembling the parts reproduces the transformed string exactly,
// and the segment is sanitized as a whole either way. Splitting the raw value
// and transforming the pieces separately would diverge on "NGC 7000 _1-1" and
// would miss "Sadr_1 -1", a panel only after the strip.
func transformTarget(target string, stripSpaces bool) string {
	if stripSpaces {
		target = strings.ReplaceAll(target, " ", "")
	}
	return strings.TrimSpace(target)
}

// renderSegment substitutes every <TOKEN> in one template segment and sanitizes
// the fully-substituted string into a single path component. A required token
// that cannot be resolved returns a *QuarantineError. A segment that renders
// empty is a quarantine UNLESS it collapsed solely because the optional <FILTER>
// token was absent, in which case it is dropped (drop=true) so no "//" appears
// in the path. When an absent optional token collapses inside a segment that has
// other content, renderSegment also absorbs one adjacent separator so the result
// carries no dangling, leading, or doubled punctuation.
func renderSegment(seg string, cls model.Classification, rig model.RigProfile, opts routeOpts) (rendered string, drop bool, qerr *QuarantineError) {
	var buf []byte
	sawAbsentOptional := false

	for i := 0; i < len(seg); {
		if seg[i] == '<' {
			if end := strings.IndexByte(seg[i:], '>'); end >= 0 {
				token := seg[i : i+end+1]
				val, optionalAbsent, qe := resolveToken(token, cls, rig, opts)
				if qe != nil {
					return "", false, qe
				}
				i += end + 1
				if optionalAbsent {
					sawAbsentOptional = true
					// The optional token (only <FILTER>, absent on OSC) rendered
					// empty. Absorb one adjacent separator so it leaves no dangling
					// ("NGC7000_"), leading (".NGC7000"), or doubled ("2600MM__1.7s")
					// punctuation. Prefer eating the following separator; otherwise drop
					// the preceding one.
					if i < len(seg) && isPathSep(seg[i]) {
						i++
					} else if n := len(buf); n > 0 && isPathSep(buf[n-1]) {
						buf = buf[:n-1]
					}
					continue
				}
				buf = append(buf, val...)
				continue
			}
		}
		buf = append(buf, seg[i])
		i++
	}

	sanitized := sanitize.Component(string(buf))
	if sanitized == "" {
		// The one legal collapse: a segment emptied solely by an absent optional
		// token (just <FILTER> on OSC) is dropped, not quarantined (design
		// routing-and-filing §3).
		if sawAbsentOptional {
			return "", true, nil
		}
		return "", false, &QuarantineError{Reason: "template segment " + strconv.Quote(seg) + " rendered empty"}
	}
	return sanitized, false, nil
}

// isPathSep reports whether b is one of the punctuation characters operators use
// to join tokens within a template segment. When an optional token (<FILTER>)
// collapses on an OSC frame, renderSegment absorbs one adjacent separator so the
// rendered segment carries no dangling, leading, or doubled punctuation. Digits
// and letters are deliberately excluded — they can be literal template content.
func isPathSep(b byte) bool {
	switch b {
	case '_', '-', '.', ',', ' ':
		return true
	default:
		return false
	}
}

// resolveToken renders one <TOKEN> to its substitution value. It returns
// optionalAbsent=true for the sole optional token (<FILTER>) when its card is
// absent, so the caller can drop an otherwise-empty segment. A required token
// that cannot be resolved returns a *QuarantineError. An unrecognized token is
// treated as literal text (template validation lives at config-save time, not
// here); its angle brackets are neutralized by the segment sanitize step.
func resolveToken(token string, cls model.Classification, rig model.RigProfile, opts routeOpts) (value string, optionalAbsent bool, qerr *QuarantineError) {
	// The four panel tokens mean nothing outside the mosaic template. Validation
	// rejects them there at save, but Load does not validate, so a hand-edited
	// config.yaml reaches routing unchecked — and the default branch below would
	// then file frames into a directory literally named "_PANEL_". Quarantine
	// instead: loud, recoverable, and the treatment every other unresolvable
	// routing-critical token already gets.
	if isPanelToken(token) && !opts.mosaic {
		return "", false, &QuarantineError{Reason: "mosaic-only token " + token + " used outside the mosaic template"}
	}

	switch token {
	case "<TARGET>":
		target := transformTarget(cls.Target, opts.StripTargetSpaces)
		// <TARGET> is routing-critical: a value that sanitizes to nothing (empty,
		// all-illegal, or a pure-dots traversal like "..") is unresolvable even if
		// surrounding literals would otherwise keep the segment non-empty.
		if sanitize.Component(target) == "" {
			return "", false, &QuarantineError{Reason: "OBJECT sanitizes to empty", MissingToken: "OBJECT"}
		}
		return target, false, nil

	// The panel tokens below are resolvable by construction: the mosaic template
	// is selected only when the parse succeeded, so no optional-token collapse
	// arises. <TARGET_BASE> is the one exception, and only for a nameless base.
	case "<TARGET_BASE>":
		// Routing-critical with its own check, exactly as <TARGET> is: an OBJECT
		// of "_1-1" leaves the base empty, and a frame whose name is unknowable
		// has no correct home. Leaning on the whole-segment collapse instead
		// would file it wherever a surrounding literal kept the segment non-empty
		// — <OTA>/<SESSION>/<RIG> are allowed that, this is not.
		if sanitize.Component(opts.panelBase) == "" {
			return "", false, &QuarantineError{Reason: "OBJECT has no name before its panel suffix", MissingToken: "OBJECT"}
		}
		return opts.panelBase, false, nil

	case "<PANEL>":
		// Verbatim, not re-rendered from row and col: a zero-padded producer name
		// (Sadr_01-02) must reassemble into the frame's own OBJECT value.
		return opts.panelToken, false, nil
	case "<PANEL_ROW>":
		return strconv.Itoa(opts.panelRow), false, nil
	case "<PANEL_COL>":
		return strconv.Itoa(opts.panelCol), false, nil

	case "<OTA>":
		return rig.OTA, false, nil
	case "<SESSION>":
		return rig.Session, false, nil
	case "<RIG>":
		return rig.ID, false, nil

	case "<n>":
		return strconv.Itoa(opts.Night), false, nil

	case "<SENSOR>":
		if !cls.SensorMapped {
			return "", false, &QuarantineError{Reason: "INSTRUME has no sensor-map entry", MissingToken: "SENSOR"}
		}
		return cls.Sensor, false, nil

	case "<DATE>":
		if !cls.DateObsPresent {
			return "", false, &QuarantineError{Reason: "DATE-OBS is absent", MissingToken: "DATE-OBS"}
		}
		return opts.ObservingDate.Format(opts.DateFormat), false, nil

	// <YEAR>/<MONTH>/<DAY> break the observing date into date-based folders. They
	// read the same ObservingDate instant as <DATE> (design routing-and-filing
	// §6), so a frame's <YEAR>/<MONTH>/<DAY> tree and its <DATE> stamp always
	// agree, and they share <DATE>'s missing-value handling: an absent DATE-OBS
	// quarantines.
	case "<YEAR>":
		if !cls.DateObsPresent {
			return "", false, &QuarantineError{Reason: "DATE-OBS is absent", MissingToken: "DATE-OBS"}
		}
		return opts.ObservingDate.Format("2006"), false, nil

	case "<MONTH>":
		if !cls.DateObsPresent {
			return "", false, &QuarantineError{Reason: "DATE-OBS is absent", MissingToken: "DATE-OBS"}
		}
		return opts.ObservingDate.Format("01"), false, nil

	case "<DAY>":
		if !cls.DateObsPresent {
			return "", false, &QuarantineError{Reason: "DATE-OBS is absent", MissingToken: "DATE-OBS"}
		}
		return opts.ObservingDate.Format("02"), false, nil

	case "<SET-TEMP>":
		// The ASIAIR writes SET-TEMP only while the cooler is on, so an absent card
		// means an uncooled frame, not an unreadable one. It gets its own bucket so
		// uncooled darks never mix with cooled ones; CCD-TEMP is no substitute — it
		// is a reading, not a set-point, and would scatter the library.
		if !cls.SetTempPresent {
			return Uncooled, false, nil
		}
		return strconv.FormatInt(int64(math.Round(cls.SetTempC)), 10) + "C", false, nil

	case "<EXPOSURE>":
		if !cls.ExptimePresent {
			return "", false, &QuarantineError{Reason: "EXPTIME is absent", MissingToken: "EXPTIME"}
		}
		return renderExposure(cls.Exptime), false, nil

	case "<FILTER>":
		if !cls.FilterPresent {
			return "", true, nil // the one token allowed to collapse (OSC has no FILTER)
		}
		return cls.Filter, false, nil

	case "<GAIN>":
		if !cls.GainPresent {
			return "", false, &QuarantineError{Reason: "GAIN is absent", MissingToken: "GAIN"}
		}
		return "gain" + strconv.FormatInt(cls.Gain, 10), false, nil

	case "<OFFSET>":
		if !cls.OffsetPresent {
			return "", false, &QuarantineError{Reason: "OFFSET is absent", MissingToken: "OFFSET"}
		}
		return "offset" + strconv.FormatInt(cls.Offset, 10), false, nil

	case "<BINNING>":
		if !cls.BinPresent {
			return "", false, &QuarantineError{Reason: "XBINNING is absent", MissingToken: "BINNING"}
		}
		return renderBinning(cls.BinX, cls.BinY), false, nil

	default:
		return token, false, nil // unrecognized: literal passthrough
	}
}

// isPanelToken reports whether a token is one of the four the mosaic template
// alone may name. It gates them at the top of resolveToken so none of them can
// take the literal-passthrough path in a template where there is no panel to
// render.
func isPanelToken(token string) bool {
	switch token {
	case "<TARGET_BASE>", "<PANEL>", "<PANEL_ROW>", "<PANEL_COL>":
		return true
	default:
		return false
	}
}

// renderExposure renders <EXPOSURE>: round EXPTIME to the nearest millisecond
// first (killing float noise such as 0.00100000004749745 → 0.001), then emit the
// shortest-form decimal plus "s" (300.0 → "300s", 0.001 → "0.001s").
func renderExposure(exptime float64) string {
	secs := math.Round(exptime*1000) / 1000
	return strconv.FormatFloat(secs, 'f', -1, 64) + "s"
}

// renderBinning renders <BINNING>: "Bin<X>" when the axes match, "Bin<X>x<Y>"
// when they differ.
func renderBinning(x, y int64) string {
	if x == y {
		return "Bin" + strconv.FormatInt(x, 10)
	}
	return "Bin" + strconv.FormatInt(x, 10) + "x" + strconv.FormatInt(y, 10)
}

// ObservingDate returns the local observing date for a frame: the date of the
// noon-to-noon window's opening local noon. It converts dateObs into loc,
// subtracts 12h, and takes that calendar date at local midnight — so a frame
// captured at 02:00 local carries the previous day's date, the intuitive "which
// night was this" answer (design routing-and-filing §6). It is pure and exported
// so the worker computes it once and feeds both store.AssignNight and
// Options.ObservingDate, keeping the night bucket and the <DATE> token
// consistent.
func ObservingDate(dateObs time.Time, loc *time.Location) time.Time {
	local := dateObs.In(loc).Add(-12 * time.Hour)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// ObservingDateKeyLayout is the fixed layout for the night-counter database key.
// It is deliberately NOT the operator-editable Options.DateFormat (the <DATE>
// token's display layout): night_counters must key on a stable string so a later
// change to the display format can never re-key an existing night and break the
// never-renumber guarantee (design routing-and-filing §6).
const ObservingDateKeyLayout = "2006-01-02"

// ObservingDateKey renders an observing date (from ObservingDate) as the stable
// string used to key night_counters in the store. Both the live filing path and
// the startup NIGHT_n rebuild must use this, so they agree independently.
func ObservingDateKey(observingDate time.Time) string {
	return observingDate.Format(ObservingDateKeyLayout)
}
