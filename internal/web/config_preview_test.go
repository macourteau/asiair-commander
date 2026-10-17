package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// previewTemplates mirrors the shipped default templates (route_test.go's
// defaultTemplates), so the golden destinations below are anchored to values
// already proven correct end-to-end by the route package's fixture tests.
func previewTemplates() model.Templates {
	return model.Templates{
		Light: "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>",
		Cal: map[model.FrameType]string{
			model.TypeDark: "<SENSOR>/DARK/<GAIN>/<BINNING>/<SET-TEMP>/<EXPOSURE>/<DATE>",
			model.TypeFlat: "<SENSOR>/FLAT/<OTA>/<GAIN>/<BINNING>/<DATE>",
			model.TypeBias: "<SENSOR>/BIAS/<GAIN>/<BINNING>/<SET-TEMP>/<DATE>",
		},
	}
}

// setPreviewRig configures a single rig (id "rig-1") matching route_test.go's
// testRig, so seeded frames resolve to it.
func setPreviewRig(h *harness) {
	h.t.Helper()
	cfg := *h.cfgPtr.Load()
	cfg.Rigs = []model.RigProfile{{ID: "rig-1", OTA: "FRA300", Session: "Summer2026"}}
	h.cfgPtr.Store(&cfg)
}

func card(key string, value any) model.Card { return model.Card{Key: key, Value: value} }

// seedPreviewFrames upserts one verified frame per canonical type into the
// store, each with a dest_path, and injects a header.Fake keyed by those
// dest_paths carrying the same header values as the committed FITS fixtures
// (internal/header/testdata), so the expected destinations match route_test.go's
// golden values exactly.
func seedPreviewFrames(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	type seed struct {
		frame model.Frame
		hdr   model.Header
	}
	seeds := []seed{
		{
			frame: model.Frame{
				RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Light/light_2600mm_ha.fit",
				Size: 100, Mtime: 1, FrameType: string(model.TypeLight), State: model.StateVerified, Night: 2,
				DestPath: "C-2025A6/FRA300/Summer2026/NIGHT_2/light_2600mm_ha.fit",
			},
			hdr: model.NewHeader([]model.Card{
				card("IMAGETYP", "Light"), card("OBJECT", "C-2025 A6"), card("INSTRUME", "ZWO ASI2600MM Pro"),
				card("FILTER", "HA"), card("EXPTIME", 600.0), card("SET-TEMP", -10.0), card("GAIN", int64(100)),
				card("OFFSET", int64(50)), card("XBINNING", int64(1)), card("YBINNING", int64(1)),
				card("DATE-OBS", "2025-10-11T05:25:41.531614"),
			}),
		},
		{
			frame: model.Frame{
				RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Dark/dark_6200mm.fit",
				Size: 100, Mtime: 1, FrameType: string(model.TypeDark), State: model.StateVerified,
				DestPath: "6200MM/DARK/gain100/Bin1/0C/300s/2026-06-18/dark_6200mm.fit",
			},
			hdr: model.NewHeader([]model.Card{
				card("IMAGETYP", "Dark"), card("INSTRUME", "ZWO ASI6200MM Pro"), card("EXPTIME", 300.0),
				card("SET-TEMP", 0.0), card("GAIN", int64(100)), card("OFFSET", int64(50)),
				card("XBINNING", int64(1)), card("YBINNING", int64(1)),
				card("DATE-OBS", "2026-06-19T02:07:50.047338"),
			}),
		},
		{
			frame: model.Frame{
				RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Flat/flat_6200mm_ha.fit",
				Size: 100, Mtime: 1, FrameType: string(model.TypeFlat), State: model.StateVerified,
				DestPath: "6200MM/FLAT/FRA300/gain100/Bin1/2026-07-14/flat_6200mm_ha.fit",
			},
			hdr: model.NewHeader([]model.Card{
				card("IMAGETYP", "Flat"), card("INSTRUME", "ZWO ASI6200MM Pro"), card("EXPTIME", 1.67499804496765),
				card("SET-TEMP", 0.0), card("GAIN", int64(100)), card("OFFSET", int64(50)),
				card("XBINNING", int64(1)), card("YBINNING", int64(1)), card("FILTER", "HA"),
				card("DATE-OBS", "2026-07-15T04:02:25.365452"),
			}),
		},
		{
			frame: model.Frame{
				RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Bias/bias_6200mm.fit",
				Size: 100, Mtime: 1, FrameType: string(model.TypeBias), State: model.StateVerified,
				DestPath: "6200MM/BIAS/gain100/Bin1/-10C/2026-06-18/bias_6200mm.fit",
			},
			hdr: model.NewHeader([]model.Card{
				card("IMAGETYP", "Bias"), card("INSTRUME", "ZWO ASI6200MM Pro"), card("EXPTIME", 0.001),
				card("SET-TEMP", -10.0), card("GAIN", int64(100)), card("OFFSET", int64(50)),
				card("XBINNING", int64(1)), card("YBINNING", int64(1)),
				card("DATE-OBS", "2026-06-19T01:02:22.031444"),
			}),
		},
	}

	fake := header.Fake{Headers: map[string]model.Header{}}
	for _, s := range seeds {
		fr := s.frame
		if err := h.store.UpsertFrame(ctx, &fr); err != nil {
			t.Fatalf("upsert frame: %v", err)
		}
		fake.Headers[s.frame.DestPath] = s.hdr
	}
	h.srv.deps.Header = fake
}

func decodePreview(t *testing.T, resp *http.Response) []previewSample {
	t.Helper()
	var body struct {
		Samples []previewSample `json:"samples"`
	}
	decode(t, resp, &body)
	return body.Samples
}

// TestConfigPreviewValidTemplate exercises the real pipeline end to end: a
// candidate template set, live alias table + sensor map, real sampled frames
// read back through header.Fake, classified and routed — and asserts each
// canonical type lands exactly where route_test.go's fixture-driven golden
// tests say the same header values should land.
func TestConfigPreviewValidTemplate(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	setPreviewRig(h)
	seedPreviewFrames(t, h)

	req := previewRequest{
		Templates:         previewTemplates(),
		StripTargetSpaces: true,
		DateFormat:        "2006-01-02",
	}
	resp := h.do("POST", "/api/config/preview", req, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("preview: want 200, got %d (%s)", resp.StatusCode, b)
	}
	samples := decodePreview(t, resp)
	if len(samples) != 4 {
		t.Fatalf("samples = %d, want 4", len(samples))
	}

	want := map[string]string{
		"LIGHT": "C-2025A6/FRA300/Summer2026/NIGHT_2/light_2600mm_ha.fit",
		"DARK":  "6200MM/DARK/gain100/Bin1/0C/300s/2026-06-18/dark_6200mm.fit",
		"FLAT":  "6200MM/FLAT/FRA300/gain100/Bin1/2026-07-14/flat_6200mm_ha.fit",
		"BIAS":  "6200MM/BIAS/gain100/Bin1/-10C/2026-06-18/bias_6200mm.fit",
	}
	for _, s := range samples {
		if s.Synthetic {
			t.Errorf("type %s: unexpectedly synthetic", s.Type)
		}
		if s.Error != "" {
			t.Errorf("type %s: unexpected error %q", s.Type, s.Error)
		}
		if got, want := s.Dest, want[s.Type]; got != want {
			t.Errorf("type %s: dest = %q, want %q", s.Type, got, want)
		}
	}
}

// TestConfigPreviewMissingTokenReturnsError is the load-bearing case (design
// architecture §9): a candidate template references a token the sample frame
// can't supply — here <SENSOR> with an empty live sensor map — and the endpoint
// must report that as a per-type error entry, not fail the whole request or
// 500.
func TestConfigPreviewMissingTokenReturnsError(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	setPreviewRig(h)
	seedPreviewFrames(t, h)

	cfg := *h.cfgPtr.Load()
	cfg.SensorMap = map[string]string{} // <SENSOR> now unresolvable for every INSTRUME
	h.cfgPtr.Store(&cfg)

	req := previewRequest{
		Templates:         previewTemplates(),
		StripTargetSpaces: true,
		DateFormat:        "2006-01-02",
	}
	resp := h.do("POST", "/api/config/preview", req, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("preview with a routing failure: want 200, got %d (%s)", resp.StatusCode, b)
	}
	samples := decodePreview(t, resp)

	byType := map[string]previewSample{}
	for _, s := range samples {
		byType[s.Type] = s
	}
	for _, typ := range []string{"DARK", "FLAT", "BIAS"} { // all reference <SENSOR>
		s, ok := byType[typ]
		if !ok {
			t.Fatalf("missing sample for %s", typ)
		}
		if s.Error == "" {
			t.Errorf("type %s: expected an error (unresolvable SENSOR), got none; dest=%q", typ, s.Dest)
		}
		if s.Dest != "" {
			t.Errorf("type %s: expected no dest alongside an error, got %q", typ, s.Dest)
		}
	}
	if s := byType["LIGHT"]; s.Error != "" {
		t.Errorf("LIGHT: unexpected error %q (its template doesn't reference SENSOR)", s.Error)
	}
}

// TestConfigPreviewSyntheticFallback covers a brand-new install with no frame
// history: every type must still produce a preview row from the built-in
// synthetic sample, marked accordingly.
func TestConfigPreviewSyntheticFallback(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.srv.deps.Header = header.Fake{Headers: map[string]model.Header{}} // present but nothing mapped

	req := previewRequest{
		Templates:         previewTemplates(),
		StripTargetSpaces: true,
		DateFormat:        "2006-01-02",
	}
	resp := h.do("POST", "/api/config/preview", req, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("preview: want 200, got %d (%s)", resp.StatusCode, b)
	}
	samples := decodePreview(t, resp)
	if len(samples) != 4 {
		t.Fatalf("samples = %d, want 4", len(samples))
	}

	wantPrefix := map[string]string{
		"LIGHT": "M31/OTA/SESSION/NIGHT_1/",
		"DARK":  "2600MM/DARK/gain100/Bin1/-10C/300s/",
		"FLAT":  "2600MM/FLAT/OTA/gain100/Bin1/",
		"BIAS":  "2600MM/BIAS/gain100/Bin1/-10C/",
	}
	for _, s := range samples {
		if !s.Synthetic {
			t.Errorf("type %s: expected synthetic (no frame history), got a real sample", s.Type)
		}
		if s.Error != "" {
			t.Errorf("type %s: unexpected error %q", s.Type, s.Error)
		}
		if !strings.HasPrefix(s.Dest, wantPrefix[s.Type]) {
			t.Errorf("type %s: dest = %q, want prefix %q", s.Type, s.Dest, wantPrefix[s.Type])
		}
	}
}

// TestConfigPreviewPrefersReadableFrame covers a culled NAS: the most recent
// frame of a type is gone from disk (its header no longer reads), but an older
// sibling is still there. The preview must skip the missing one and sample the
// older readable frame rather than falling back to the synthetic header.
func TestConfigPreviewPrefersReadableFrame(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	setPreviewRig(h)
	ctx := context.Background()

	// Insert the readable frame first (lower id), the culled one second (higher
	// id = more recent). Only the readable frame's dest_path is mapped in the
	// fake, so the culled frame's header read fails like a deleted file.
	readable := model.Frame{
		RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Dark/dark_older.fit",
		Size: 100, Mtime: 1, FrameType: string(model.TypeDark), State: model.StateVerified,
		DestPath: "6200MM/DARK/gain100/Bin1/0C/300s/2026-06-10/dark_older.fit",
	}
	culled := model.Frame{
		RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Dark/dark_newer.fit",
		Size: 100, Mtime: 2, FrameType: string(model.TypeDark), State: model.StateVerified,
		DestPath: "6200MM/DARK/gain100/Bin1/0C/300s/2026-06-18/dark_newer.fit",
	}
	for _, f := range []model.Frame{readable, culled} {
		fr := f
		if err := h.store.UpsertFrame(ctx, &fr); err != nil {
			t.Fatalf("upsert frame: %v", err)
		}
	}
	h.srv.deps.Header = header.Fake{Headers: map[string]model.Header{
		readable.DestPath: model.NewHeader([]model.Card{
			card("IMAGETYP", "Dark"), card("INSTRUME", "ZWO ASI6200MM Pro"), card("EXPTIME", 300.0),
			card("SET-TEMP", 0.0), card("GAIN", int64(100)), card("OFFSET", int64(50)),
			card("XBINNING", int64(1)), card("YBINNING", int64(1)),
			card("DATE-OBS", "2026-06-11T02:07:50.047338"),
		}),
	}}

	req := previewRequest{Templates: previewTemplates(), StripTargetSpaces: true, DateFormat: "2006-01-02"}
	resp := h.do("POST", "/api/config/preview", req, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("preview: want 200, got %d (%s)", resp.StatusCode, b)
	}
	byType := map[string]previewSample{}
	for _, s := range decodePreview(t, resp) {
		byType[s.Type] = s
	}
	dark := byType["DARK"]
	if dark.Synthetic {
		t.Fatalf("DARK: fell back to synthetic despite a readable older frame on disk")
	}
	if dark.Source != "dark_older.fit" {
		t.Errorf("DARK: source = %q, want the readable older frame dark_older.fit", dark.Source)
	}
	// The other types have no frames at all, so they must synthesize.
	if !byType["LIGHT"].Synthetic {
		t.Errorf("LIGHT: expected synthetic (no frames), got a real sample")
	}
}

// TestConfigPreviewAllCulledSynthesizes covers the case where every candidate
// frame of a type is gone from disk: with no readable real frame, the preview
// must fall back to the synthetic sample rather than routing a header it could
// not read.
func TestConfigPreviewAllCulledSynthesizes(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	setPreviewRig(h)
	ctx := context.Background()

	fr := model.Frame{
		RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Dark/dark_gone.fit",
		Size: 100, Mtime: 1, FrameType: string(model.TypeDark), State: model.StateVerified,
		DestPath: "6200MM/DARK/gain100/Bin1/0C/300s/2026-06-18/dark_gone.fit",
	}
	if err := h.store.UpsertFrame(ctx, &fr); err != nil {
		t.Fatalf("upsert frame: %v", err)
	}
	// Header present but nothing mapped: every dest_path read fails.
	h.srv.deps.Header = header.Fake{Headers: map[string]model.Header{}}

	req := previewRequest{Templates: previewTemplates(), StripTargetSpaces: true, DateFormat: "2006-01-02"}
	resp := h.do("POST", "/api/config/preview", req, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("preview: want 200, got %d (%s)", resp.StatusCode, b)
	}
	byType := map[string]previewSample{}
	for _, s := range decodePreview(t, resp) {
		byType[s.Type] = s
	}
	dark := byType["DARK"]
	if !dark.Synthetic {
		t.Errorf("DARK: expected synthetic fallback (culled frame unreadable), got source %q", dark.Source)
	}
}

// TestConfigPreviewNilHeaderReturns503 asserts the endpoint fails gracefully
// (not a panic) when the composition root hasn't wired a header.Reader —
// newHarness leaves Deps.Header nil by default.
func TestConfigPreviewNilHeaderReturns503(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/config/preview", previewRequest{Templates: previewTemplates()}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("nil header reader: want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestConfigPreviewRequiresAuthAndCSRF confirms the endpoint sits behind the
// same guardWrite defence as every other mutation.
func TestConfigPreviewRequiresAuthAndCSRF(t *testing.T) {
	h := newHarness(t, true)
	h.srv.deps.Header = header.Fake{Headers: map[string]model.Header{}}

	req := previewRequest{Templates: previewTemplates()}

	resp := h.do("POST", "/api/config/preview", req, reqOpts{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.login()
	resp = h.do("POST", "/api/config/preview", req, reqOpts{csrf: false})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
