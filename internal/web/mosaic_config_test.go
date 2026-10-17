package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

const customMosaicTemplate = "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>"

// storeMosaicConfig turns detection on with a customised template, as an
// operator who opted in would have.
func storeMosaicConfig(h *harness) {
	h.t.Helper()
	cfg := *h.cfgPtr.Load()
	cfg.MosaicDetection = true
	cfg.Templates.Mosaic = customMosaicTemplate
	h.cfgPtr.Store(&cfg)
}

// putConfigWithout PUTs the stored config as the SPA would, minus the named
// top-level keys and minus the named templates sub-keys — the body an older SPA
// against a newer server produces.
func putConfigWithout(h *harness, topLevel []string, templateKeys []string) *http.Response {
	h.t.Helper()
	raw, err := json.Marshal(config.Redact(h.cfgPtr.Load()))
	if err != nil {
		h.t.Fatalf("marshal config: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		h.t.Fatalf("unmarshal config: %v", err)
	}
	// Each key must have been there to begin with, or the test proves nothing.
	for _, k := range topLevel {
		if _, ok := body[k]; !ok {
			h.t.Fatalf("config JSON carries no %q key to omit", k)
		}
		delete(body, k)
	}
	if tmpls, ok := body["templates"].(map[string]any); ok {
		for _, k := range templateKeys {
			if _, ok := tmpls[k]; !ok {
				h.t.Fatalf("templates JSON carries no %q key to omit", k)
			}
			delete(tmpls, k)
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal body: %v", err)
	}
	return h.doRaw("PUT", "/api/config", out)
}

// TestPutConfigOmittingMosaicKeysPreservesThem is design routing-and-filing
// AC18's real invariant. An old SPA against a new server omits both added keys;
// an omitted bool decodes as false, which would silently reverse the operator's
// opt-in and start filing panels flat again — by a path no UI warning can see.
// It must instead succeed and change neither field.
func TestPutConfigOmittingMosaicKeysPreservesThem(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	storeMosaicConfig(h)

	resp := putConfigWithout(h, []string{"mosaic_detection"}, []string{"mosaic"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("omitting PUT: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()

	got := h.cfgPtr.Load()
	if !got.MosaicDetection {
		t.Error("mosaic_detection was reversed by a PUT that never mentioned it")
	}
	if got.Templates.Mosaic != customMosaicTemplate {
		t.Errorf("templates.mosaic = %q, want the stored %q", got.Templates.Mosaic, customMosaicTemplate)
	}
}

// TestPutConfigOmittingTemplatesPreservesAll pins the other half of the shadow
// struct: because it captures the whole templates object, the embedded config's
// copy decodes zero-valued, so light and cal have to be projected back too.
// Getting that wrong is a hard 400, since decodeJSON disallows unknown fields.
func TestPutConfigOmittingTemplatesPreservesAll(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	storeMosaicConfig(h)
	before := *h.cfgPtr.Load()

	resp := putConfigWithout(h, []string{"mosaic_detection", "templates"}, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("templates-less PUT: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()

	got := h.cfgPtr.Load()
	if got.Templates.Light != before.Templates.Light {
		t.Errorf("light template = %q, want %q", got.Templates.Light, before.Templates.Light)
	}
	if got.Templates.Mosaic != customMosaicTemplate {
		t.Errorf("templates.mosaic = %q, want %q", got.Templates.Mosaic, customMosaicTemplate)
	}
	for _, ft := range []model.FrameType{model.TypeDark, model.TypeFlat, model.TypeBias} {
		if got.Templates.Cal[ft] != before.Templates.Cal[ft] {
			t.Errorf("cal[%s] = %q, want %q", ft, got.Templates.Cal[ft], before.Templates.Cal[ft])
		}
	}
}

// TestPutConfigSavesMosaicFields proves preservation is not a one-way trap: a
// payload that DOES carry the keys still sets them, including turning detection
// back off, and <PANEL_ROW>/<PANEL_COL> save (design routing-and-filing AC16).
func TestPutConfigSavesMosaicFields(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig()
	cfg.MosaicDetection = true
	cfg.Templates.Mosaic = customMosaicTemplate
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enabling PUT: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()
	if got := h.cfgPtr.Load(); !got.MosaicDetection || got.Templates.Mosaic != customMosaicTemplate {
		t.Fatalf("mosaic fields not applied: detection=%v template=%q", got.MosaicDetection, got.Templates.Mosaic)
	}

	off := h.getConfig()
	off.MosaicDetection = false
	resp = h.do("PUT", "/api/config", off, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("disabling PUT: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()
	if h.cfgPtr.Load().MosaicDetection {
		t.Error("an explicit mosaic_detection:false must turn detection off")
	}
}

// TestPutConfigRejectsBadMosaicTemplate is the save-side of design
// routing-and-filing AC13: with detection on, a mosaic template that cannot tell
// panels apart is a field-level 400 rather than a silently accepted value.
func TestPutConfigRejectsBadMosaicTemplate(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig()
	cfg.MosaicDetection = true
	cfg.Templates.Mosaic = "<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>"
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("flat mosaic template: want 400, got %d (%s)", resp.StatusCode, b)
	}
	var body struct {
		Problems []config.Problem `json:"problems"`
	}
	decode(t, resp, &body)
	found := false
	for _, p := range body.Problems {
		if p.Field == "templates.mosaic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a templates.mosaic field error, got %+v", body.Problems)
	}
}

// TestPutConfigEmptyMosaicTemplateTakesDefault is design routing-and-filing
// AC13's last clause on the save path: a cleared field normalises to the default
// instead of being rejected.
func TestPutConfigEmptyMosaicTemplateTakesDefault(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig()
	cfg.MosaicDetection = true
	cfg.Templates.Mosaic = ""
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("empty mosaic template: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()
	if got := h.cfgPtr.Load().Templates.Mosaic; got != config.Defaults().Templates.Mosaic {
		t.Fatalf("cleared mosaic template = %q, want the shipped default", got)
	}
}

// seedMosaicPreviewFrames stores a non-panel light and a NEWER panel light, both
// readable. The order matters: the panel is the most recent LIGHT, which is
// exactly the situation in which a predicate-free LIGHT row would preview the
// mosaic template.
func seedMosaicPreviewFrames(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	lightCards := func(object string) model.Header {
		return model.NewHeader([]model.Card{
			card("IMAGETYP", "Light"), card("OBJECT", object), card("INSTRUME", "ZWO ASI2600MM Pro"),
			card("FILTER", "HA"), card("EXPTIME", 600.0), card("SET-TEMP", -10.0), card("GAIN", int64(100)),
			card("OFFSET", int64(50)), card("XBINNING", int64(1)), card("YBINNING", int64(1)),
			card("DATE-OBS", "2026-07-11T05:25:41.531614"),
		})
	}

	fake := header.Fake{Headers: map[string]model.Header{}}
	for _, s := range []struct {
		target string
		dest   string
	}{
		{"NGC6888", "NGC6888/FRA300/Summer2026/NIGHT_1/ngc6888.fit"},
		{"Sadr_1-1", "Sadr_1-1/FRA300/Summer2026/NIGHT_3/sadr_1-1.fit"},
	} {
		fr := model.Frame{
			RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Light/" + filepath.Base(s.dest),
			Size: 100, Mtime: 1, FrameType: string(model.TypeLight), State: model.StateVerified,
			Night: 3, Target: s.target, DestPath: s.dest,
		}
		if err := h.store.UpsertFrame(ctx, &fr); err != nil {
			t.Fatalf("upsert frame: %v", err)
		}
		fake.Headers[s.dest] = lightCards(s.target)
	}
	h.srv.deps.Header = fake
}

// TestConfigPreviewMosaicRow is design routing-and-filing AC17's testable core.
// With detection on the preview gains a MOSAIC row rendered through the mosaic
// template from a real panel frame, and — the part that is easy to get wrong —
// the LIGHT row keeps previewing a NON-panel frame even though the newest light
// is a panel.
func TestConfigPreviewMosaicRow(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	setPreviewRig(h)
	seedMosaicPreviewFrames(t, h)

	tmpls := previewTemplates()
	tmpls.Mosaic = "<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>"

	req := previewRequest{
		Templates:         tmpls,
		StripTargetSpaces: true,
		DateFormat:        "2006-01-02",
		MosaicDetection:   true,
	}
	resp := h.do("POST", "/api/config/preview", req, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("preview: want 200, got %d (%s)", resp.StatusCode, b)
	}
	byType := map[string]previewSample{}
	for _, s := range decodePreview(t, resp) {
		byType[s.Type] = s
	}

	mosaic, ok := byType[mosaicSampleType]
	if !ok {
		t.Fatalf("no MOSAIC row with detection on: %+v", byType)
	}
	if mosaic.Synthetic {
		t.Error("MOSAIC row fell back to a synthetic header despite a real panel frame")
	}
	if want := filepath.Join("Sadr", "FRA300", "Summer2026", "Sadr_1-1", "NIGHT_3", "sadr_1-1.fit"); mosaic.Dest != want {
		t.Errorf("MOSAIC dest = %q, want %q", mosaic.Dest, want)
	}

	light := byType["LIGHT"]
	if want := filepath.Join("NGC6888", "FRA300", "Summer2026", "NIGHT_3", "ngc6888.fit"); light.Dest != want {
		t.Errorf("LIGHT dest = %q, want %q — the LIGHT row must preview a non-panel frame", light.Dest, want)
	}
}

// TestConfigPreviewNoMosaicRowWhileOff pins the other side: no MOSAIC row while
// detection is off, and the LIGHT row is then free to sample a panel frame,
// because with the toggle off panels legitimately route through the light
// template.
func TestConfigPreviewNoMosaicRowWhileOff(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	setPreviewRig(h)
	seedMosaicPreviewFrames(t, h)

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
	if len(samples) != len(model.CanonicalTypes) {
		t.Fatalf("samples = %d, want %d (no MOSAIC row while detection is off)", len(samples), len(model.CanonicalTypes))
	}
	for _, s := range samples {
		if s.Type == mosaicSampleType {
			t.Fatalf("MOSAIC row emitted while detection is off: %+v", s)
		}
		if s.Type == "LIGHT" {
			want := filepath.Join("Sadr_1-1", "FRA300", "Summer2026", "NIGHT_3", "sadr_1-1.fit")
			if s.Dest != want {
				t.Errorf("LIGHT dest = %q, want %q — with detection off a panel is an ordinary light", s.Dest, want)
			}
		}
	}
}
