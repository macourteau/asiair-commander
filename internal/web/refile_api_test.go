package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// refileNDJSON reads an apply stream into its raw lines, so a test can assert both
// the per-file results and the trailing summary — two different shapes on one
// stream, told apart by position rather than by a discriminator field.
func refileNDJSON(t *testing.T, resp *http.Response) [][]byte {
	t.Helper()
	defer resp.Body.Close()
	var lines [][]byte
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	return lines
}

// TestRefilePreviewReturnsProjection checks the preview endpoint hands back the
// supervisor's diff verbatim and passes the requested scope through — the scope
// vocabulary belongs to the supervisor, and a handler that mangled it would offer
// the operator a different migration from the one they asked for.
func TestRefilePreviewReturnsProjection(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.sup.refilePreview = model.RefilePreview{
		Scope: "mosaic",
		Groups: []model.RefileGroup{{
			FromDir: "/lib/Sadr_1-1/FRA300/S/NIGHT_1", ToDir: "/lib/Sadr/FRA300/S/Sadr_1-1/NIGHT_1",
			Count: 2, FrameIDs: []int64{7, 8},
			StrandRisk: []model.StrandRisk{{Dir: "/lib/Sadr_1-1/FRA300/S/NIGHT_1", Reason: "will not be empty: 1 entry re-file does not manage remains (.DS_Store)"}},
		}},
		Refusals: []model.Refusal{{DestPath: "/lib/other/x.fit", Reason: "the archived file is marked missing"}},
	}

	resp := h.do("POST", "/api/rigs/rig-1/refile/preview", map[string]any{"scope": "mosaic"}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview: want 200, got %d", resp.StatusCode)
	}
	var got model.RefilePreview
	decode(t, resp, &got)
	if got.Scope != "mosaic" || len(got.Groups) != 1 || len(got.Refusals) != 1 {
		t.Fatalf("preview projection = %+v", got)
	}
	g := got.Groups[0]
	if g.ToDir != "/lib/Sadr/FRA300/S/Sadr_1-1/NIGHT_1" || g.Count != 2 || len(g.FrameIDs) != 2 || len(g.StrandRisk) != 1 {
		t.Fatalf("group = %+v", g)
	}
	h.sup.mu.Lock()
	scope := h.sup.refileScope
	h.sup.mu.Unlock()
	if scope != "mosaic" {
		t.Fatalf("supervisor saw scope %q, want the requested one", scope)
	}
}

// TestRefilePreviewSerializesEmptyDiff checks a library with nothing to migrate
// answers with empty arrays rather than nulls; the SPA maps over both.
func TestRefilePreviewSerializesEmptyDiff(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.sup.refilePreview = model.RefilePreview{Scope: "all"}

	resp := h.do("POST", "/api/rigs/rig-1/refile/preview", map[string]any{"scope": "all"}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview: want 200, got %d", resp.StatusCode)
	}
	var raw struct {
		Groups   []model.RefileGroup `json:"groups"`
		Refusals []model.Refusal     `json:"refusals"`
	}
	body := new(bytes.Buffer)
	defer resp.Body.Close()
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bytes.Contains(body.Bytes(), []byte("null")) {
		t.Fatalf("an empty diff must serialize as [], got %s", body.String())
	}
}

// TestRefileApplyStreamsNDJSON checks apply emits one result line per file, in the
// order the supervisor reported them, followed by the summary as the FINAL line —
// prune outcomes are directories rather than frames, so they cannot be results, and
// the client relies on the summary arriving last.
func TestRefileApplyStreamsNDJSON(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.sup.refileResults = []model.RefileResult{
		{FrameID: 10, Status: "moved", From: "/lib/old/a.fit", To: "/lib/new/a.fit"},
		{FrameID: 11, Status: "skipped", From: "/lib/old/b.fit", Reason: "the archived file is marked missing"},
		{FrameID: 12, Status: "failed", From: "/lib/old/c.fit", Reason: "commit failed; the file was moved back"},
	}
	h.sup.refileSummary = model.RefileSummary{
		PrunedDirs:  []string{"/lib/old"},
		SkippedDirs: []model.StrandRisk{{Dir: "/lib/keep", Reason: "will not be empty"}},
	}

	resp := h.do("POST", "/api/rigs/rig-1/refile/apply",
		map[string]any{"to_dir": "/lib/new", "frame_ids": []int64{10, 11, 12}}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("apply content-type = %q, want application/x-ndjson", ct)
	}

	lines := refileNDJSON(t, resp)
	if len(lines) != 4 {
		t.Fatalf("want 3 result lines plus a summary, got %d: %s", len(lines), lines)
	}
	for i, want := range []struct {
		id     int64
		status string
	}{{10, "moved"}, {11, "skipped"}, {12, "failed"}} {
		var res model.RefileResult
		if err := json.Unmarshal(lines[i], &res); err != nil {
			t.Fatalf("unmarshal line %d %q: %v", i, lines[i], err)
		}
		if res.FrameID != want.id || res.Status != want.status {
			t.Fatalf("line %d = %+v, want frame %d %s", i, res, want.id, want.status)
		}
	}
	var sum model.RefileSummary
	if err := json.Unmarshal(lines[3], &sum); err != nil {
		t.Fatalf("unmarshal summary %q: %v", lines[3], err)
	}
	if len(sum.PrunedDirs) != 1 || sum.PrunedDirs[0] != "/lib/old" || len(sum.SkippedDirs) != 1 {
		t.Fatalf("summary = %+v", sum)
	}

	h.sup.mu.Lock()
	toDir, ids := h.sup.refileArgs.toDir, h.sup.refileArgs.ids
	h.sup.mu.Unlock()
	if toDir != "/lib/new" || len(ids) != 3 || ids[0] != 10 || ids[2] != 12 {
		t.Fatalf("supervisor saw to_dir=%q ids=%v", toDir, ids)
	}
}

// TestRefileApplyEmptyGroupStillSummarizes checks an empty selection is a valid
// empty 200 that still carries the summary line, so the client's stream reader
// terminates the same way it does for real work.
func TestRefileApplyEmptyGroupStillSummarizes(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1/refile/apply",
		map[string]any{"to_dir": "/lib/new", "frame_ids": []int64{}}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: want 200, got %d", resp.StatusCode)
	}
	if lines := refileNDJSON(t, resp); len(lines) != 1 {
		t.Fatalf("want just the summary line, got %d: %s", len(lines), lines)
	}
}

// TestRefileApplyRequiresDestination checks the destination the operator saw in the
// diff is mandatory: apply refuses any frame routing elsewhere (INV-M10), so an
// omitted to_dir would refuse the whole group in a way that reads like a bug.
func TestRefileApplyRequiresDestination(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1/refile/apply",
		map[string]any{"frame_ids": []int64{1}}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing to_dir: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestRefileUnknownRig checks an id outside the supervisor's known set is a 404 on
// both endpoints — for apply this is the pre-stream case, before any line is
// written, which is why the handler begins the stream lazily rather than deferring.
func TestRefileUnknownRig(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cases := []struct {
		path string
		body map[string]any
	}{
		{"/api/rigs/nope/refile/preview", map[string]any{"scope": "all"}},
		{"/api/rigs/nope/refile/apply", map[string]any{"to_dir": "/lib/new", "frame_ids": []int64{1}}},
	}
	for _, c := range cases {
		resp := h.do("POST", c.path, c.body, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unknown rig: want 404, got %d", c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestRefileEndpointsAreGuarded checks both routes take guardWrite — including the
// read-only preview, since it is a POST: unauthenticated is a 401 and a missing
// CSRF header a 403.
func TestRefileEndpointsAreGuarded(t *testing.T) {
	h := newHarness(t, true)

	body := map[string]any{"scope": "all", "to_dir": "/lib/new", "frame_ids": []int64{1}}
	for _, path := range []string{"/api/rigs/rig-1/refile/preview", "/api/rigs/rig-1/refile/apply"} {
		resp := h.do("POST", path, body, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: want 401, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	h.login()
	for _, path := range []string{"/api/rigs/rig-1/refile/preview", "/api/rigs/rig-1/refile/apply"} {
		resp := h.do("POST", path, body, reqOpts{csrf: false})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s without CSRF: want 403, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestRefileApplyHonoursRequestCancellation (design routing-and-filing AC38, the
// server half) proves the handler hands the supervisor the REQUEST's context: an
// operator who cancels the apply must stop the migration, and every other
// supervisor call takes a detached context that no cancellation could ever reach.
func TestRefileApplyHonoursRequestCancellation(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	entered := make(chan struct{})
	h.sup.mu.Lock()
	h.sup.refileBlock = make(chan struct{}) // never closed: the group is "in flight"
	h.sup.refileEntered = entered
	h.sup.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	body, err := json.Marshal(map[string]any{"to_dir": "/lib/new", "frame_ids": []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/api/rigs/rig-1/refile/apply", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", h.baseURL)
	req.Header.Set(csrfHeader, h.csrf())

	done := make(chan error, 1)
	go func() {
		resp, err := h.client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()

	<-entered // the supervisor is inside the group
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled request must not complete normally")
	}

	// The server notices the dropped connection a moment after the client does, so
	// poll rather than read once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.sup.mu.Lock()
		ctxErr := h.sup.refileCtxErr
		h.sup.mu.Unlock()
		if ctxErr != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervisor's context must be cancelled with the request, not detached from it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
