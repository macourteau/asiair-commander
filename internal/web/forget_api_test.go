package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

const leftoversForgetPath = "/api/rigs/rig-1/missing-leftovers/forget"

func TestForgetFrameRoute(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	path := func(id string) string { return fmt.Sprintf("/api/rigs/rig-1/frames/%s/forget", id) }

	resp := h.do("POST", path("42"), nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("forget: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if calls := h.supCalls(); len(calls) != 1 || calls[0] != "forget:rig-1:42" {
		t.Fatalf("supervisor calls = %v, want [forget:rig-1:42]", calls)
	}

	resp = h.do("POST", path("nope"), nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-integer frame id: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("POST", "/api/rigs/ghost/frames/42/forget", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{rig.ErrNotLeftover, http.StatusConflict, codeForgetNotLeftover},
		{rig.ErrRestorePending, http.StatusConflict, codeForgetRestorePending},
		{rig.ErrFrameNotFound, http.StatusNotFound, ""},
		{context.DeadlineExceeded, http.StatusAccepted, codeForgetQueued},
		{fmt.Errorf("disk on fire"), http.StatusInternalServerError, ""},
	} {
		h.sup.forgetErr = c.err
		resp := h.do("POST", path("42"), nil, reqOpts{csrf: true})
		if resp.StatusCode != c.status {
			t.Errorf("%v: want %d, got %d", c.err, c.status, resp.StatusCode)
		}
		if got := errorCode(t, resp); got != c.code {
			t.Errorf("%v: code = %q, want %q", c.err, got, c.code)
		}
	}
	h.sup.forgetErr = nil

	resp = h.do("POST", path("42"), nil, reqOpts{csrf: false})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// forgetChallenge posts a tokenless bulk Forget and returns the decoded body.
func (h *harness) forgetChallenge(t *testing.T, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	resp := h.do("POST", leftoversForgetPath, body, reqOpts{csrf: true})
	if resp.StatusCode != wantStatus {
		t.Fatalf("forget challenge %v: want %d, got %d", body, wantStatus, resp.StatusCode)
	}
	var out map[string]any
	decode(t, resp, &out)
	return out
}

// design library-reconciliation I40 / AC-T3: a bulk Forget's token binds the ids
// its challenge selected — rows that appear afterwards are not forgotten, a row
// that gains a restore request is refused by the guard while every other bound row
// is forgotten, and the confirm reports the count actually changed. The token is
// also bound to dir and older_than_days.
func TestForgetLeftoversTokenBindsIDs(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	ctx := context.Background()
	// Apply the confirm through the real store so the SQL guard is exercised.
	h.sup.forgetApply = func(rigID string, ids []int64) (int, error) {
		return h.store.ForgetLeftovers(ctx, rigID, ids)
	}
	// Freeze the clock (at the real time, so the login session stays valid) so the
	// age cutoff is exact.
	base := time.Now()
	h.srv.now = func() time.Time { return base }
	day := int64(24 * 60 * 60)

	dir := filepath.Join(t.TempDir(), "M31")
	old1 := h.seedFrameState(t, "rig-1", dir, "old1", base.Unix()-10*day, model.StateDeleted)
	old2 := h.seedFrameState(t, "rig-1", dir, "old2", base.Unix()-9*day, model.StateDeleted)
	old3 := h.seedFrameState(t, "rig-1", dir, "old3", base.Unix()-8*day, model.StateDeleted)
	recent := h.seedFrameState(t, "rig-1", dir, "recent", base.Unix()-1*day, model.StateDeleted)
	rp := h.seedFrameState(t, "rig-1", dir, "rp", base.Unix()-10*day, model.StateDeleted)
	if _, err := h.store.RequestRestore(ctx, "rig-1", rp, base.Unix()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(dir), "NGC7000")
	elsewhere := h.seedFrameState(t, "rig-1", other, "e", base.Unix()-10*day, model.StateDeleted)
	h.seedFrameState(t, "rig-1", dir, "v", base.Unix()-10*day, model.StateVerified)

	ch := h.forgetChallenge(t, map[string]any{"dir": dir, "older_than_days": 7}, http.StatusOK)
	token, _ := ch["token"].(string)
	if token == "" || ch["count"].(float64) != 3 {
		t.Fatalf("challenge = %+v, want a token and the 3 old leftovers", ch)
	}
	if sk := ch["skipped"].(map[string]any); sk["restore_pending"].(float64) != 1 {
		t.Fatalf("skipped = %+v, want restore_pending 1", sk)
	}

	// The token is scoped to its dir and age: any other scope is a 403.
	for _, body := range []map[string]any{
		{"dir": dir, "older_than_days": 28, "token": token},
		{"dir": dir, "token": token},
		{"dir": other, "older_than_days": 7, "token": token},
		{"older_than_days": 7, "token": token},
		{"dir": dir, "older_than_days": 7, "token": "nope"},
	} {
		resp := h.do("POST", leftoversForgetPath, body, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%v: want 403, got %d", body, resp.StatusCode)
		}
		if code := errorCode(t, resp); code != codeForgetConfirmationInvalid {
			t.Errorf("%v: code = %q", body, code)
		}
	}

	// Between dialog and confirm: a new old leftover appears (not bound), and a
	// bound row gains a restore request (refused by the guard).
	late := h.seedFrameState(t, "rig-1", dir, "late", base.Unix()-20*day, model.StateDeleted)
	if _, err := h.store.RequestRestore(ctx, "rig-1", old2, base.Unix()); err != nil {
		t.Fatal(err)
	}

	resp := h.do("POST", leftoversForgetPath,
		map[string]any{"dir": dir, "older_than_days": 7, "token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	decode(t, resp, &out)
	if out["forgotten"].(float64) != 2 {
		t.Fatalf("forgotten = %v, want 2 (the bound rows minus the one now restore-pending)", out["forgotten"])
	}
	h.sup.mu.Lock()
	bound := append([]int64(nil), h.sup.forgetIDs...)
	h.sup.mu.Unlock()
	if len(bound) != 3 {
		t.Fatalf("supervisor received %v, want exactly the 3 bound ids", bound)
	}
	for id, wantForgotten := range map[int64]bool{
		old1: true, old3: true, old2: false, recent: false, rp: false, late: false, elsewhere: false,
	} {
		f, _, _ := h.store.GetFrameByID(ctx, id)
		if forgotten := f.MissingSeenAt == 0 && f.DestPath == ""; forgotten != wantForgotten {
			t.Errorf("frame %d: forgotten=%v, want %v", id, forgotten, wantForgotten)
		}
	}

	// The token is spent on success.
	resp = h.do("POST", leftoversForgetPath,
		map[string]any{"dir": dir, "older_than_days": 7, "token": token}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("replay: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The whole section (no dir, no age) binds every remaining forgettable leftover.
	ch = h.forgetChallenge(t, map[string]any{}, http.StatusOK)
	if ch["count"].(float64) != 3 { // recent, late, elsewhere
		t.Fatalf("whole-section challenge = %+v, want count 3", ch)
	}
}

func TestForgetLeftoversValidation(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)

	for _, raw := range []string{`{"older_than_days": 0}`, `{"older_than_days": -1}`,
		`{"older_than_days": 1.5}`, `{"older_than_days": "7"}`, `{"dir": 3}`, `{"bogus": 1}`} {
		resp := h.do("POST", leftoversForgetPath, json.RawMessage(raw), reqOpts{csrf: true})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", raw, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Nothing forgettable: a 409 with no token, naming the skip when there is one.
	ch := h.forgetChallenge(t, map[string]any{}, http.StatusConflict)
	if ch["code"] != codeForgetNothingToDo || ch["token"] != nil {
		t.Fatalf("empty section = %+v, want FORGET_NOTHING_TO_DO and no token", ch)
	}

	resp := h.do("POST", "/api/rigs/ghost/missing-leftovers/forget", map[string]any{}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
