package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// --- helpers -------------------------------------------------------------------

// addRig appends a rig to the live config so the acknowledge handlers' step-1
// "rig still configured" check passes. Frame rows can outlive a rig's config
// removal, which is exactly why the check exists.
func (h *harness) addRig(id string, keepOriginals bool) {
	cfg := h.cfgPtr.Load()
	nc := *cfg
	nc.Rigs = append(append([]model.RigProfile(nil), cfg.Rigs...),
		model.RigProfile{ID: id, KeepOriginals: keepOriginals})
	h.cfgPtr.Store(&nc)
}

// seedMissingFrame upserts a verified frame whose dest_path is under dir and marks
// it missing at seenAt, returning its id.
func (h *harness) seedMissingFrame(t *testing.T, rigID, dir, name string, seenAt int64) int64 {
	t.Helper()
	return h.seedFrameState(t, rigID, dir, name, seenAt, model.StateVerified)
}

func (h *harness) seedFrameState(t *testing.T, rigID, dir, name string, seenAt int64, state model.FrameState) int64 {
	t.Helper()
	f := &model.Frame{
		RigID: rigID, SourceShare: "EMMC Images",
		SourcePath: "Autorun/Light/" + rigID + "_" + name + ".fit",
		Size:       100, Mtime: 1, State: state,
		DestPath: filepath.Join(dir, name+".fit"),
	}
	if err := h.store.UpsertFrame(context.Background(), f); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if seenAt != 0 {
		if err := h.store.SetFrameMissing(context.Background(), f.ID, seenAt); err != nil {
			t.Fatalf("set missing: %v", err)
		}
	}
	return f.ID
}

func (h *harness) supCalls() []string {
	h.sup.mu.Lock()
	defer h.sup.mu.Unlock()
	return append([]string(nil), h.sup.calls...)
}

func (h *harness) frameAckPath(rigID string, frameID int64) string {
	return fmt.Sprintf("/api/rigs/%s/frames/%d/acknowledge", rigID, frameID)
}

// --- frame: challenge + confirm ------------------------------------------------

func TestAcknowledgeFrameChallengeThenConfirm(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	// Challenge: an empty body returns the server's description and a token.
	resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("challenge: want 200, got %d", resp.StatusCode)
	}
	var ch map[string]any
	decode(t, resp, &ch)
	token, _ := ch["token"].(string)
	if token == "" {
		t.Fatalf("challenge returned no token: %+v", ch)
	}
	if ch["dest_path"] != filepath.Join("/astro/M31", "a.fit") {
		t.Errorf("dest_path = %v", ch["dest_path"])
	}
	if ch["state"] != "verified" {
		t.Errorf("state = %v", ch["state"])
	}

	// Confirm: 204 and exactly one supervisor call.
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("confirm: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if calls := h.supCalls(); len(calls) != 1 || calls[0] != fmt.Sprintf("ack:rig-1:%d", id) {
		t.Fatalf("supervisor calls = %v, want one ack", calls)
	}
}

func TestAcknowledgeFrameEmptyBodyVariantsAreChallenges(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	// nil (no body), {} both decode to an empty token → challenge, not 400.
	for _, body := range []any{nil, map[string]string{}} {
		resp := h.do("POST", h.frameAckPath("rig-1", id), body, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("challenge body %v: want 200, got %d", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- frame: confirmation rejections --------------------------------------------

func TestAcknowledgeFrameRejectsBadTokens(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	h.addRig("rig-2", false)
	h.sup.known["rig-2"] = true
	idA := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())
	idB := h.seedMissingFrame(t, "rig-1", "/astro/M31", "b", time.Now().Unix())
	idC := h.seedMissingFrame(t, "rig-2", "/astro/M31", "c", time.Now().Unix())

	mint := func(rigID string, frameID int64) string {
		resp := h.do("POST", h.frameAckPath(rigID, frameID), nil, reqOpts{csrf: true})
		var ch map[string]any
		decode(t, resp, &ch)
		return ch["token"].(string)
	}
	tokA := mint("rig-1", idA)

	reject := func(name, path, token string) {
		resp := h.do("POST", path, map[string]string{"token": token}, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d", name, resp.StatusCode)
		}
		if code := errorCode(t, resp); code != codeAckConfirmationInvalid {
			t.Fatalf("%s: want code %q, got %q", name, codeAckConfirmationInvalid, code)
		}
	}
	reject("wrong token", h.frameAckPath("rig-1", idA), "deadbeef")
	reject("token for another frame", h.frameAckPath("rig-1", idB), tokA)
	reject("token for another rig", h.frameAckPath("rig-2", idC), tokA)

	if calls := h.supCalls(); len(calls) != 0 {
		t.Fatalf("supervisor must not be called on a rejected confirm; calls=%v", calls)
	}
}

func TestAcknowledgeFrameExpiredToken(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	// Freeze the clock so the token's 60s TTL can be crossed without sleeping.
	base := time.Now()
	h.srv.now = func() time.Time { return base }

	resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: true})
	var ch map[string]any
	decode(t, resp, &ch)
	token := ch["token"].(string)

	// Advance past the TTL; the confirm's lookup now sees an expired entry.
	h.srv.now = func() time.Time { return base.Add(2 * time.Minute) }
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired confirm: want 403, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != codeAckConfirmationInvalid {
		t.Fatalf("expired confirm: want code %q, got %q", codeAckConfirmationInvalid, code)
	}
	if calls := h.supCalls(); len(calls) != 0 {
		t.Fatalf("expired confirm must not call supervisor; calls=%v", calls)
	}
}

func TestAcknowledgeFrameReplayRejected(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: true})
	var ch map[string]any
	decode(t, resp, &ch)
	token := ch["token"].(string)

	// First confirm consumes the token.
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first confirm: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Replaying it is rejected.
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("replay: want 403, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != codeAckConfirmationInvalid {
		t.Fatalf("replay: want code %q, got %q", codeAckConfirmationInvalid, code)
	}
	if calls := h.supCalls(); len(calls) != 1 {
		t.Fatalf("replay must not re-call supervisor; calls=%v", calls)
	}
}

// --- frame: supervisor error mapping -------------------------------------------

func TestAcknowledgeSupervisorErrorKeepsToken(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: true})
	var ch map[string]any
	decode(t, resp, &ch)
	token := ch["token"].(string)

	// A genuine failure → 500, and the token is NOT consumed.
	h.sup.ackErr = errors.New("worker busy")
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("error confirm: want 500, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The same token works once the transient error clears.
	h.sup.ackErr = nil
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("retry confirm: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if calls := h.supCalls(); len(calls) != 1 {
		t.Fatalf("supervisor calls = %v, want one successful ack", calls)
	}
}

func TestAcknowledgeQueuedOnDeadline(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: true})
	var ch map[string]any
	decode(t, resp, &ch)
	token := ch["token"].(string)

	// A busy worker times out the reply; the acknowledgement may already have
	// applied, so it is a 202 ACK_QUEUED success body — never a failure.
	h.sup.ackErr = context.DeadlineExceeded
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deadline confirm: want 202, got %d", resp.StatusCode)
	}
	var body map[string]any
	decode(t, resp, &body)
	if body["code"] != codeAckQueued {
		t.Fatalf("202 body code = %v, want %q", body["code"], codeAckQueued)
	}
	// The token was NOT consumed (the request may not have applied): a retry works.
	h.sup.ackErr = nil
	resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("retry after 202: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAcknowledgeConfirmErrorCodes(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not acknowledgeable", rig.ErrNotAcknowledgeable, http.StatusConflict, codeAckNotVerified},
		{"not missing", ErrFrameNotMissing, http.StatusConflict, codeAckNotMissing},
		{"restore pending", rig.ErrRestorePending, http.StatusConflict, codeAckRestorePending},
		{"frame not found", rig.ErrFrameNotFound, http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		// Fresh token each time (a failed confirm does not consume it, but the
		// mapping is what matters here).
		resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: true})
		var ch map[string]any
		decode(t, resp, &ch)
		token := ch["token"].(string)

		h.sup.ackErr = tc.err
		resp = h.do("POST", h.frameAckPath("rig-1", id), map[string]string{"token": token}, reqOpts{csrf: true})
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: want %d, got %d", tc.name, tc.status, resp.StatusCode)
		}
		if tc.code != "" {
			if code := errorCode(t, resp); code != tc.code {
				t.Fatalf("%s: want code %q, got %q", tc.name, tc.code, code)
			}
		} else {
			resp.Body.Close()
		}
		h.sup.ackErr = nil
	}
}

// --- frame: mint refusals ------------------------------------------------------

func TestAcknowledgeChallengeRefusals(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	h.addRig("keep-rig", true)
	h.sup.known["keep-rig"] = true

	now := time.Now().Unix()
	notMissing := h.seedFrameState(t, "rig-1", "/astro/M31", "nm", 0, model.StateVerified)
	held := h.seedFrameState(t, "rig-1", "/astro/M31", "held", now, model.StateHeld)
	restorePending := h.seedMissingFrame(t, "rig-1", "/astro/M31", "rp", now)
	if _, err := h.store.RequestRestore(context.Background(), "rig-1", restorePending, now); err != nil {
		t.Fatalf("request restore: %v", err)
	}
	acked := h.seedMissingFrame(t, "rig-1", "/astro/M31", "ak", now)
	if _, err := h.store.AcknowledgeMissing(context.Background(), "rig-1", acked, now); err != nil {
		t.Fatalf("ack: %v", err)
	}
	keepFrame := h.seedMissingFrame(t, "keep-rig", "/astro/M31", "k", now)

	// Unknown rig: the frame's rig is not in config → 404, and NO panic (step-1
	// ordering reads Rig(id) before .KeepOriginals).
	unknown := h.seedMissingFrame(t, "gone-rig", "/astro/M31", "g", now)
	resp := h.do("POST", h.frameAckPath("gone-rig", unknown), nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	type refusal struct {
		name    string
		rig     string
		frameID int64
		status  int
		code    string
		already bool
	}
	for _, tc := range []refusal{
		{"keep_originals", "keep-rig", keepFrame, http.StatusConflict, codeAckKeepOriginals, false},
		{"not missing", "rig-1", notMissing, http.StatusConflict, codeAckNotMissing, false},
		{"not verified", "rig-1", held, http.StatusConflict, codeAckNotVerified, false},
		{"restore pending", "rig-1", restorePending, http.StatusConflict, codeAckRestorePending, false},
		{"already acked", "rig-1", acked, http.StatusOK, "", true},
	} {
		resp := h.do("POST", h.frameAckPath(tc.rig, tc.frameID), nil, reqOpts{csrf: true})
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: want %d, got %d", tc.name, tc.status, resp.StatusCode)
		}
		var body map[string]any
		decode(t, resp, &body)
		if tc.already {
			if body["already"] != true {
				t.Fatalf("%s: want {already:true}, got %+v", tc.name, body)
			}
		} else if body["code"] != tc.code {
			t.Fatalf("%s: want code %q, got %v", tc.name, tc.code, body["code"])
		}
		if _, hasToken := body["token"]; hasToken {
			t.Fatalf("%s: a refusal must not mint a token; got %+v", tc.name, body)
		}
	}
	if calls := h.supCalls(); len(calls) != 0 {
		t.Fatalf("no confirm was issued; supervisor calls=%v", calls)
	}
}

// design library-reconciliation I35 (AC6, mint): the per-frame challenge accepts a
// culled unclassified frame and echoes its state, while the GROUP challenge buckets
// it as not_verified (group ack stays verified-only). Held stays refused with
// ACK_NOT_VERIFIED.
func TestAcknowledgeFrameChallengeAcceptsUnclassified(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	now := time.Now().Unix()

	// Per-frame challenge mints for a culled unclassified frame and echoes its state.
	unc := h.seedFrameState(t, "rig-1", "/astro/unc", "u", now, model.StateUnclassified)
	resp := h.do("POST", h.frameAckPath("rig-1", unc), nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("per-frame challenge for unclassified: want 200, got %d", resp.StatusCode)
	}
	var ch map[string]any
	decode(t, resp, &ch)
	if tok, _ := ch["token"].(string); tok == "" {
		t.Fatalf("per-frame challenge must mint a token for a culled unclassified frame: %+v", ch)
	}
	if ch["state"] != "unclassified" {
		t.Errorf("challenge state = %v, want unclassified", ch["state"])
	}

	// Held stays refused.
	held := h.seedFrameState(t, "rig-1", "/astro/unc", "h", now, model.StateHeld)
	resp = h.do("POST", h.frameAckPath("rig-1", held), nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("held per-frame challenge: want 409, got %d", resp.StatusCode)
	}
	var hb map[string]any
	decode(t, resp, &hb)
	if hb["code"] != codeAckNotVerified {
		t.Errorf("held refusal code = %v, want %s", hb["code"], codeAckNotVerified)
	}

	// Group challenge on a dir holding one acknowledgeable verified frame AND one culled
	// unclassified frame: only the verified one is counted; the unclassified frame lands
	// in the not_verified skip bucket, so a group-ack drains no quarantine frame.
	dir := filepath.Join(t.TempDir(), "mixed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.seedMissingFrame(t, "rig-1", dir, "v", now)                         // verified, acknowledgeable
	h.seedFrameState(t, "rig-1", dir, "gu", now, model.StateUnclassified) // skipped
	resp = h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("group challenge: want 200, got %d", resp.StatusCode)
	}
	var gch map[string]any
	decode(t, resp, &gch)
	if gch["count"].(float64) != 1 {
		t.Fatalf("group challenge count = %v, want 1 (only the verified frame)", gch["count"])
	}
	skipped := gch["skipped"].(map[string]any)
	if skipped["not_verified"].(float64) != 1 {
		t.Errorf("group skipped = %+v, want not_verified 1 (the culled unclassified frame)", skipped)
	}
}

// --- group ---------------------------------------------------------------------

func TestAcknowledgeGroupChallengeAndConfirm(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)

	dir := filepath.Join(t.TempDir(), "M31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().Unix()
	var want []int64
	for _, n := range []string{"a", "b", "c"} {
		want = append(want, h.seedMissingFrame(t, "rig-1", dir, n, now))
	}
	// One held + one restore-pending row under the same dir populate the skipped
	// breakdown and must NOT be acknowledgeable.
	h.seedFrameState(t, "rig-1", dir, "held", now, model.StateHeld)
	rp := h.seedMissingFrame(t, "rig-1", dir, "rp", now)
	if _, err := h.store.RequestRestore(context.Background(), "rig-1", rp, now); err != nil {
		t.Fatalf("request restore: %v", err)
	}

	resp := h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("group challenge: want 200, got %d", resp.StatusCode)
	}
	var ch map[string]any
	decode(t, resp, &ch)
	token := ch["token"].(string)
	if token == "" || ch["count"].(float64) != 3 {
		t.Fatalf("group challenge = %+v, want token + count 3", ch)
	}
	if ch["dir_gone"] != false {
		t.Errorf("dir_gone = %v, want false (the directory exists)", ch["dir_gone"])
	}
	skipped := ch["skipped"].(map[string]any)
	if skipped["not_verified"].(float64) != 1 || skipped["restore_pending"].(float64) != 1 {
		t.Errorf("skipped = %+v, want not_verified 1 restore_pending 1", skipped)
	}

	// Grow the directory AFTER the challenge: the two new rows must not be swept
	// into the acknowledgement, because the token carries the bound ids.
	h.seedMissingFrame(t, "rig-1", dir, "grow1", now)
	h.seedMissingFrame(t, "rig-1", dir, "grow2", now)

	resp = h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir, "token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("group confirm: want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	decode(t, resp, &out)
	if out["acknowledged"].(float64) != 3 {
		t.Fatalf("acknowledged = %v, want 3", out["acknowledged"])
	}
	h.sup.mu.Lock()
	gotIDs := append([]int64(nil), h.sup.ackIDs...)
	h.sup.mu.Unlock()
	if len(gotIDs) != 3 {
		t.Fatalf("supervisor received %d ids, want the 3 bound ones (not the grown set)", len(gotIDs))
	}
	for i, wantID := range want {
		if gotIDs[i] != wantID {
			t.Fatalf("bound ids = %v, want %v", gotIDs, want)
		}
	}
}

func TestAcknowledgeGroupNothingToDo(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)

	// A dir with only a held (non-acknowledgeable) missing frame → count 0.
	dir := filepath.Join(t.TempDir(), "M31")
	h.seedFrameState(t, "rig-1", dir, "held", time.Now().Unix(), model.StateHeld)

	resp := h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("nothing-to-do: want 409, got %d", resp.StatusCode)
	}
	var body map[string]any
	decode(t, resp, &body)
	if body["code"] != codeAckNothingToDo {
		t.Fatalf("code = %v, want %q", body["code"], codeAckNothingToDo)
	}
	if _, hasToken := body["token"]; hasToken {
		t.Fatalf("nothing-to-do must not mint a token; got %+v", body)
	}
}

func TestAcknowledgeGroupDirGone(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)

	// A directory that does not exist on the filesystem is the move/rename signature.
	dir := filepath.Join(t.TempDir(), "moved-away")
	h.seedMissingFrame(t, "rig-1", dir, "a", time.Now().Unix())

	resp := h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("group challenge: want 200, got %d", resp.StatusCode)
	}
	var ch map[string]any
	decode(t, resp, &ch)
	if ch["dir_gone"] != true {
		t.Errorf("dir_gone = %v, want true for a vanished directory", ch["dir_gone"])
	}
}

func TestAcknowledgeGroupEmptyDir(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)

	resp := h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": ""}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty dir: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- CSRF ----------------------------------------------------------------------

// The acknowledge routes are behind guardWrite: a missing CSRF token is a bare 403
// from the guard, distinct from the ACK_CONFIRMATION_INVALID a bad confirmation
// carries — the SPA relies on that distinction.
func TestAcknowledgeRequiresCSRF(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	id := h.seedMissingFrame(t, "rig-1", "/astro/M31", "a", time.Now().Unix())

	resp := h.do("POST", h.frameAckPath("rig-1", id), nil, reqOpts{csrf: false})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF: want 403, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code == codeAckConfirmationInvalid {
		t.Fatalf("a CSRF failure must be a bare 403, not %q", codeAckConfirmationInvalid)
	}
}
