package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestEditPreviewReturnsProjection checks the read-only preview endpoint hands
// back exactly what the supervisor projected.
func TestEditPreviewReturnsProjection(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.sup.previews = []model.EditPreview{
		{FrameID: 1, CurrentValue: "M31", CurrentPresent: true, OldPath: "/lib/M31/a.fit", NewPath: "/lib/NGC224/a.fit", Eligible: true},
		{FrameID: 2, CurrentValue: "M31", CurrentPresent: true, OldPath: "/lib/M31/b.fit", Eligible: false, Reason: "no dest_path"},
	}

	resp := h.do("POST", "/api/rigs/rig-1/edit/preview",
		map[string]any{"ids": []int64{1, 2}, "key": "OBJECT", "value": "NGC224"}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview: want 200, got %d", resp.StatusCode)
	}
	var got []model.EditPreview
	decode(t, resp, &got)
	if len(got) != 2 || got[0].NewPath != "/lib/NGC224/a.fit" || got[1].Eligible {
		t.Fatalf("preview projection = %+v", got)
	}
}

// TestEditApplyStreamsNDJSON checks apply emits one EditResult per line, one per
// frame, and passes the normalized key and trimmed value through.
func TestEditApplyStreamsNDJSON(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1/edit/apply",
		map[string]any{"ids": []int64{10, 11, 12}, "key": "filter", "value": "  Ha "}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("apply content-type = %q, want application/x-ndjson", ct)
	}

	var lines []model.EditResult
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var res model.EditResult
		if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal NDJSON line %q: %v", sc.Text(), err)
		}
		lines = append(lines, res)
	}
	resp.Body.Close()
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 NDJSON lines, got %d: %+v", len(lines), lines)
	}
	for i, want := range []int64{10, 11, 12} {
		if lines[i].FrameID != want || lines[i].Status != "edited" {
			t.Fatalf("line %d = %+v, want frame %d edited", i, lines[i], want)
		}
	}

	h.sup.mu.Lock()
	gotIDs := append([]int64(nil), h.sup.editIDs...)
	keys := append([]string(nil), h.sup.editKeys...)
	values := append([]string(nil), h.sup.editValues...)
	h.sup.mu.Unlock()
	if len(gotIDs) != 3 || gotIDs[0] != 10 || gotIDs[2] != 12 {
		t.Fatalf("supervisor edited ids = %v, want [10 11 12]", gotIDs)
	}
	if keys[0] != "FILTER" || values[0] != "Ha" {
		t.Fatalf("supervisor got key %q value %q, want FILTER/Ha", keys[0], values[0])
	}
}

// TestEditAPI_RejectsBadInput (design header-edits HE-11): a key off the allowlist
// and a value outside its key's rule are 400s on both endpoints, before the
// supervisor is asked anything.
func TestEditAPI_RejectsBadInput(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	for _, tc := range []struct {
		name       string
		key, value string
	}{
		{"unlisted key", "EXPTIME", "300"},
		{"structural key", "NAXIS1", "10"},
		{"empty key", "", "L"},
		{"empty value", "FILTER", "   "},
		{"filter outside printable ASCII", "FILTER", "Hα"},
		{"filter too long", "FILTER", strings.Repeat("L", 61)},
		{"target too long", "OBJECT", strings.Repeat("M", 61)},
		{"target empty after sanitize", "OBJECT", "..."},
	} {
		for _, path := range []string{"/api/rigs/rig-1/edit/preview", "/api/rigs/rig-1/edit/apply"} {
			resp := h.do("POST", path,
				map[string]any{"ids": []int64{1}, "key": tc.key, "value": tc.value}, reqOpts{csrf: true})
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s %s: want 400, got %d", tc.name, path, resp.StatusCode)
			}
			resp.Body.Close()
		}
	}
	h.sup.mu.Lock()
	defer h.sup.mu.Unlock()
	if len(h.sup.editIDs) != 0 {
		t.Fatalf("a refused body reached the supervisor for ids %v", h.sup.editIDs)
	}
}

// TestEditUnknownRig checks an id outside the supervisor's known set maps to a 404
// on both endpoints — for apply this is the pre-stream rig-level case, before any
// NDJSON line is written.
func TestEditUnknownRig(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	for _, path := range []string{"/api/rigs/nope/edit/preview", "/api/rigs/nope/edit/apply"} {
		resp := h.do("POST", path,
			map[string]any{"ids": []int64{1}, "key": "OBJECT", "value": "NGC224"}, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unknown rig: want 404, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestRetargetEndpointsRemoved: the /retarget/* endpoints are removed rather than
// aliased (design header-edits §9.1).
func TestRetargetEndpointsRemoved(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	for _, path := range []string{"/api/rigs/rig-1/retarget/preview", "/api/rigs/rig-1/retarget/apply"} {
		resp := h.do("POST", path,
			map[string]any{"ids": []int64{1}, "target": "NGC224"}, reqOpts{csrf: true})
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s still answers 200", path)
		}
		resp.Body.Close()
	}
}
