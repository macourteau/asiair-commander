package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
)

// previewScanPageSize/previewScanMaxPages bound the store scan the preview
// performs to find a sample frame per type: up to previewScanMaxPages pages of
// previewScanPageSize rows (5000 frames) across every rig state, never an
// assignment into night_counters and never a write of any kind.
const (
	previewScanPageSize = 200
	previewScanMaxPages = 25
)

// previewRequest is the candidate config values being edited in the Settings
// template editor — NOT yet saved. Everything else (alias table, sensor map,
// rig profiles) is read from the live config, since only the template-shaped
// fields are what the editor lets the operator preview.
type previewRequest struct {
	Templates         model.Templates `json:"templates"`
	StripTargetSpaces bool            `json:"strip_target_spaces"`
	DateFormat        string          `json:"date_format"`
	RigID             string          `json:"rig_id"`
	// MosaicDetection is the candidate value of the Settings toggle, not the
	// saved one: the preview must show what the operator is about to save. An
	// absent key is off, the same reading a saved config with no such key gets,
	// so a client that does not send it previews with mosaic routing disabled.
	MosaicDetection bool `json:"mosaic_detection"`
}

// previewSample is one canonical type's preview row.
type previewSample struct {
	Type string `json:"type"`
	// Source is the sample frame's basename, real or synthetic.
	Source string `json:"source,omitempty"`
	// Dest is the destination path (root-relative dir + source basename) the
	// candidate templates would route this sample to. Empty when Error is set.
	Dest string `json:"dest,omitempty"`
	// Synthetic reports whether no real recent frame of this type was
	// available (or its header was unreadable), so a built-in sample header
	// stood in.
	Synthetic bool `json:"synthetic"`
	// Error explains why routing failed for this type — most usefully a
	// *route.QuarantineError message naming the token the candidate template
	// can't resolve, the exact signal this endpoint exists to surface.
	Error string `json:"error,omitempty"`
}

// handleConfigPreview is the backend for the Settings template-editor live
// preview (design architecture §9): for each canonical frame type it samples a
// real recent frame (falling back to a built-in synthetic header when none is
// available or its on-disk header can't be read), classifies it with the LIVE
// alias table/sensor map, and routes it with the CANDIDATE templates the
// operator is editing — never the saved ones. It is read-only: no config save,
// no night allocation, no store mutation. This turns a template mistake (a
// token the sample frame can't supply) into a visible quarantine message before
// it ever misfiles a real frame.
func (s *Server) handleConfigPreview(w http.ResponseWriter, r *http.Request) {
	if s.deps.Header == nil {
		writeError(w, http.StatusServiceUnavailable, "preview unavailable: header reader not configured")
		return
	}

	var req previewRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid preview request body")
		return
	}

	cfg := s.currentConfig()
	dateFormat := req.DateFormat
	if dateFormat == "" {
		dateFormat = cfg.DateFormat
	}
	if dateFormat == "" {
		dateFormat = "2006-01-02"
	}

	// The candidate templates are unsaved, so they have been through neither
	// normalization nor validation. Without this a blank mosaic template renders
	// a bare "no template configured" as the operator's only diagnostic, instead
	// of the default the save path would have filled in.
	normalized := model.Config{Templates: req.Templates}
	config.NormalizeTemplates(&normalized)
	req.Templates = normalized.Templates

	samples := make([]previewSample, 0, len(model.CanonicalTypes)+1)
	for _, t := range model.CanonicalTypes {
		samples = append(samples, s.previewSample(r.Context(), cfg, req, dateFormat, t, false))
	}
	// The mosaic row is not a canonical type — a pseudo-member of model.FrameType
	// would have to answer to Route's type switch, Known() and IsCalibration(),
	// none of which has a sensible answer for it. It is a LIGHT row selected with
	// the panel predicate and relabelled, which the SPA keys off by string.
	if req.MosaicDetection {
		samples = append(samples, s.previewSample(r.Context(), cfg, req, dateFormat, model.TypeLight, true))
	}

	writeJSON(w, http.StatusOK, map[string]any{"samples": samples})
}

// mosaicSampleType is the row label the mosaic preview carries in place of a
// frame type, and mosaicSampleObject the OBJECT its synthetic header names.
const (
	mosaicSampleType   = "MOSAIC"
	mosaicSampleObject = "Sadr_1-1"
)

// previewSample builds one row: find-or-synthesize a sample header, classify it
// with the live config, and route it with the candidate templates. mosaic asks
// for the mosaic row — a LIGHT sample restricted to panel frames and relabelled
// — rather than the plain row for t.
func (s *Server) previewSample(ctx context.Context, cfg *model.Config, req previewRequest, dateFormat string, t model.FrameType, mosaic bool) previewSample {
	sample := previewSample{Type: string(t)}
	if mosaic {
		sample.Type = mosaicSampleType
	}

	// Built before the scan because the sample predicate needs it: which frames
	// belong in this row is the same question route.Route asks to pick a
	// template, so it is asked with the same options and the same code. Night and
	// the observing date are filled in below, once a frame has been chosen.
	opts := route.Options{
		Templates:         req.Templates,
		DateFormat:        dateFormat,
		StripTargetSpaces: req.StripTargetSpaces,
		MosaicDetection:   req.MosaicDetection,
	}

	frame, h, found := s.findRecentFrame(ctx, req.RigID, t, opts, mosaic)

	var rigID string
	switch {
	case found:
		rigID = frame.RigID
		sample.Source = filepath.Base(frame.DestPath)
	default:
		// No frame of this type has a readable on-disk header — a brand-new
		// install with no history, or every candidate was culled off the NAS.
		rigID = req.RigID
		object := ""
		if mosaic {
			object = mosaicSampleObject
		}
		h = syntheticHeader(t, object, s.now())
		sample.Synthetic = true
		sample.Source = syntheticSampleName(t)
		s.log.Info("web: config preview: no readable real frame of this type, using synthetic sample", "type", sample.Type)
	}

	rig := s.previewRigProfile(cfg, rigID)
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)

	// Night is never allocated here (that would mutate persistent state); a
	// light reuses its frame's already-assigned night, else falls back to 1.
	night := 1
	if t == model.TypeLight && found && frame.Night > 0 {
		night = int(frame.Night)
	}
	opts.Night = night
	opts.ObservingDate = route.ObservingDate(cls.DateObs, time.Local)

	res, err := route.Route(cls, rig, opts)
	if err != nil {
		// Includes *route.QuarantineError — its Error() already names the
		// missing token, which is exactly the useful signal here.
		sample.Error = err.Error()
		return sample
	}
	// filepath.Join rewrites the router's "/"-separated relative dir to the OS
	// separator, so the previewed destination reads the same way a filed
	// dest_path does ("\" on Windows) instead of mixing separators.
	sample.Dest = filepath.Join(res.Dir, sample.Source)
	return sample
}

// previewRigProfile resolves the rig profile whose OTA/SESSION/ID feed the
// route tokens: the given rig id (from the sample frame, or the request when
// no frame was found) if it names a configured rig, else the first configured
// rig, else a synthetic stand-in so the preview always has something to
// render.
func (s *Server) previewRigProfile(cfg *model.Config, rigID string) model.RigProfile {
	if rigID != "" {
		if rp := cfg.Rig(rigID); rp != nil {
			return *rp
		}
	}
	if len(cfg.Rigs) > 0 {
		return cfg.Rigs[0]
	}
	return model.RigProfile{ID: "rig", OTA: "OTA", Session: "SESSION"}
}

// findRecentFrame looks for a sample frame of type t whose on-disk header still
// reads, preferring rigID when given but falling back to any rig if that rig has
// no readable frame of this type. It returns the frame and its parsed header.
// wantPanel selects which side of the mosaic split the sample must fall on — see
// scanForSample.
func (s *Server) findRecentFrame(ctx context.Context, rigID string, t model.FrameType, opts route.Options, wantPanel bool) (model.Frame, model.Header, bool) {
	if frame, hdr, ok := s.scanForSample(ctx, rigID, t, opts, wantPanel); ok {
		return frame, hdr, true
	}
	if rigID == "" {
		return model.Frame{}, model.Header{}, false
	}
	return s.scanForSample(ctx, "", t, opts, wantPanel)
}

// scanForSample walks a bounded window of a rig's (or every rig's, when rigID
// is empty) frame history and returns the most recent successfully-filed frame
// of type t (state verified or deleted, with a dest_path) whose on-disk header
// still reads, together with that header. It probes candidates newest-first and
// stops at the first readable one: the recorded most-recent frame may have been
// culled off the NAS, in which case an older sibling of the same type is
// usually still there and makes a more representative sample than the built-in
// synthetic header. It never filters on the store's "filter" query param
// (that's the FITS FILTER header value, not frame type), so it filters
// frame_type and state client-side over pages of ListFrames.
//
// wantPanel splits the LIGHT population in two while mosaic detection is on: the
// LIGHT row must not preview a panel and the MOSAIC row must preview only one.
// Without it, after a mosaic night the newest LIGHT is a panel, and the row
// labelled LIGHT would render the MOSAIC template — leaving the operator editing
// the light template with nothing changing in its own row. The split is computed
// with route.IsMosaicPanel over the stored target rather than read from the
// persisted panel columns, because with detection off panels legitimately route
// through the light template and excluding them would make that row
// unrepresentative; IsMosaicPanel answers false for every frame in that case, so
// wantPanel=false is the whole population and no third state is needed.
func (s *Server) scanForSample(ctx context.Context, rigID string, t model.FrameType, opts route.Options, wantPanel bool) (model.Frame, model.Header, bool) {
	var candidates []model.Frame
	for page := 0; page < previewScanMaxPages; page++ {
		rows, total, err := s.deps.Store.ListFrames(ctx, rigID, "", "", page, previewScanPageSize)
		if err != nil {
			s.log.Warn("web: config preview: list frames failed", "rig", rigID, "type", t, "err", err)
			return model.Frame{}, model.Header{}, false
		}
		for _, f := range rows {
			if model.FrameType(f.FrameType) != t || f.DestPath == "" {
				continue
			}
			if f.State != model.StateVerified && f.State != model.StateDeleted {
				continue
			}
			if route.IsMosaicPanel(f.Target, t, opts) != wantPanel {
				continue
			}
			candidates = append(candidates, f)
		}
		if len(rows) == 0 || (page+1)*previewScanPageSize >= total {
			break
		}
	}
	// ListFrames returns ascending id, so walk candidates newest-first and take
	// the first whose header still reads. The probe count is bounded by the same
	// page window that bounds the scan, so this stays a fixed amount of disk work.
	for i := len(candidates) - 1; i >= 0; i-- {
		f := candidates[i]
		hdr, err := s.deps.Header.ReadHeader(f.DestPath)
		if err != nil {
			s.log.Debug("web: config preview: candidate header unreadable, trying an older frame",
				"type", t, "dest", f.DestPath, "err", err)
			continue
		}
		return f, hdr, true
	}
	return model.Frame{}, model.Header{}, false
}

// syntheticSampleName is the placeholder source name shown when no real frame
// backs a preview row.
func syntheticSampleName(t model.FrameType) string {
	return strings.ToLower(string(t)) + "_sample.fit"
}

// syntheticHeader builds a representative built-in header for a canonical
// type so the preview always shows something, even on a brand-new install
// with no frame history yet. Cards mirror a real ASIAIR mono-camera header
// (design routing-and-filing §3 token table): IMAGETYP, INSTRUME, EXPTIME,
// SET-TEMP, GAIN, OFFSET, XBINNING/YBINNING, FILTER, DATE-OBS, plus OBJECT for
// a light.
//
// object overrides the OBJECT a light carries, so the mosaic row can name a
// panel without this function having to know what a mosaic is; "" keeps the
// ordinary target.
func syntheticHeader(t model.FrameType, object string, now time.Time) model.Header {
	cards := []model.Card{
		{Key: "IMAGETYP", Value: syntheticImageType(t)},
		{Key: "INSTRUME", Value: "ZWO ASI2600MM Pro"},
		{Key: "EXPTIME", Value: syntheticExptime(t)},
		{Key: "SET-TEMP", Value: float64(-10)},
		{Key: "GAIN", Value: int64(100)},
		{Key: "OFFSET", Value: int64(50)},
		{Key: "XBINNING", Value: int64(1)},
		{Key: "YBINNING", Value: int64(1)},
		{Key: "FILTER", Value: "HA"},
		{Key: "DATE-OBS", Value: now.UTC().Format("2006-01-02T15:04:05.000000")},
	}
	if t == model.TypeLight {
		if object == "" {
			object = "M31"
		}
		cards = append(cards, model.Card{Key: "OBJECT", Value: object})
	}
	return model.NewHeader(cards)
}

// syntheticImageType renders the raw IMAGETYP value a real ASIAIR header
// carries for each canonical type (verified against committed fixtures), so
// the sample classifies correctly against a default alias table.
func syntheticImageType(t model.FrameType) string {
	switch t {
	case model.TypeLight:
		return "Light"
	case model.TypeDark:
		return "Dark"
	case model.TypeFlat:
		return "Flat"
	case model.TypeBias:
		return "Bias"
	default:
		return ""
	}
}

// syntheticExptime picks a representative exposure per type.
func syntheticExptime(t model.FrameType) float64 {
	switch t {
	case model.TypeLight, model.TypeDark:
		return 300
	case model.TypeFlat:
		return 1.5
	case model.TypeBias:
		return 0.001
	default:
		return 0
	}
}
