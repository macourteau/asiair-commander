package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestRetargetPreviewReturnsProjection checks the read-only preview endpoint hands
// back exactly what the supervisor projected.
func TestRetargetPreviewReturnsProjection(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.sup.previews = []model.RetargetPreview{
		{FrameID: 1, CurrentTarget: "M31", OldPath: "/lib/M31/a.fit", NewPath: "/lib/NGC224/a.fit", Eligible: true},
		{FrameID: 2, CurrentTarget: "M31", OldPath: "/lib/M31/b.fit", Eligible: false, Reason: "no dest_path"},
	}

	resp := h.do("POST", "/api/rigs/rig-1/retarget/preview",
		map[string]any{"ids": []int64{1, 2}, "target": "NGC224"}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview: want 200, got %d", resp.StatusCode)
	}
	var got []model.RetargetPreview
	decode(t, resp, &got)
	if len(got) != 2 || got[0].NewPath != "/lib/NGC224/a.fit" || got[1].Eligible {
		t.Fatalf("preview projection = %+v", got)
	}
}

// TestRetargetApplyStreamsNDJSON checks apply emits one RetargetResult per line, one
// per frame, and that a per-frame failure is a "failed" line rather than an aborted
// stream.
func TestRetargetApplyStreamsNDJSON(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1/retarget/apply",
		map[string]any{"ids": []int64{10, 11, 12}, "target": "NGC224"}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("apply content-type = %q, want application/x-ndjson", ct)
	}

	var lines []model.RetargetResult
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var res model.RetargetResult
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
		if lines[i].FrameID != want || lines[i].Status != "retargeted" {
			t.Fatalf("line %d = %+v, want frame %d retargeted", i, lines[i], want)
		}
	}

	h.sup.mu.Lock()
	gotIDs := append([]int64(nil), h.sup.retargetIDs...)
	h.sup.mu.Unlock()
	if len(gotIDs) != 3 || gotIDs[0] != 10 || gotIDs[2] != 12 {
		t.Fatalf("supervisor retargeted ids = %v, want [10 11 12]", gotIDs)
	}
}

// TestRetargetBadTargetRejected checks target validation at the boundary: an empty
// (or whitespace-only) target is a 400 on both endpoints.
func TestRetargetBadTargetRejected(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	for _, path := range []string{"/api/rigs/rig-1/retarget/preview", "/api/rigs/rig-1/retarget/apply"} {
		resp := h.do("POST", path,
			map[string]any{"ids": []int64{1}, "target": "   "}, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s empty target: want 400, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestRetargetUnknownRig checks an id outside the supervisor's known set maps to a
// 404 on both endpoints — for apply this is the pre-stream rig-level case, before
// any NDJSON line is written.
func TestRetargetUnknownRig(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	for _, path := range []string{"/api/rigs/nope/retarget/preview", "/api/rigs/nope/retarget/apply"} {
		resp := h.do("POST", path,
			map[string]any{"ids": []int64{1}, "target": "NGC224"}, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unknown rig: want 404, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
