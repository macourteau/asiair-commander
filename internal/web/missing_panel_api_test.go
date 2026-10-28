package web

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

type missingEnvelope struct {
	Frames []frameDTO `json:"frames"`
	Total  int        `json:"total"`
	Groups []struct {
		Dir     string `json:"dir"`
		Count   int    `json:"count"`
		Ackable int    `json:"ackable"`
	} `json:"groups"`
	Totals struct {
		Actionable int `json:"actionable"`
		Leftover   int `json:"leftover"`
	} `json:"totals"`
}

func (h *harness) missingPanel(t *testing.T, query url.Values) missingEnvelope {
	t.Helper()
	query.Set("missing", "true")
	resp := h.do("GET", "/api/rigs/rig-1/frames?"+query.Encode(), nil, reqOpts{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("missing panel %v: want 200, got %d", query, resp.StatusCode)
	}
	var env missingEnvelope
	decode(t, resp, &env)
	return env
}

// AC-T2 (design library-reconciliation §13.5): over rows seeded through the real
// store, each folder's ackable equals the count a real group-acknowledge challenge
// binds for that folder (409 when zero), with and without q and dir.
func TestMissingPanelAckableMatchesGroupChallenge(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	ctx := context.Background()
	now := time.Now().Unix()
	root := t.TempDir()
	dirA, dirB, dirC := filepath.Join(root, "M31"), filepath.Join(root, "NGC7000"), filepath.Join(root, "IC1396")

	h.seedFrameState(t, "rig-1", dirA, "v1", now, model.StateVerified)
	h.seedFrameState(t, "rig-1", dirA, "v2", now, model.StateVerified)
	h.seedFrameState(t, "rig-1", dirA, "u", now, model.StateUnclassified)
	h.seedFrameState(t, "rig-1", dirA, "h", now, model.StateHeld)
	h.seedFrameState(t, "rig-1", dirA, "d", now, model.StateDeleted)
	rp := h.seedFrameState(t, "rig-1", dirA, "rp", now, model.StateVerified)
	if _, err := h.store.RequestRestore(ctx, "rig-1", rp, now); err != nil {
		t.Fatal(err)
	}
	ack := h.seedFrameState(t, "rig-1", dirB, "k", now, model.StateVerified)
	if ok, err := h.store.AcknowledgeMissing(ctx, "rig-1", ack, now); err != nil || !ok {
		t.Fatalf("ack: ok=%v err=%v", ok, err)
	}
	h.seedFrameState(t, "rig-1", dirB, "v3", now, model.StateVerified)
	h.seedFrameState(t, "rig-1", dirC, "h2", now, model.StateHeld)
	h.seedFrameState(t, "rig-1", dirC, "d2", now, model.StateDeleted)

	for _, q := range []url.Values{
		{},
		{"q": {"m31"}},
		{"dir": {dirB}},
		{"q": {"NGC"}, "dir": {dirA}},
	} {
		env := h.missingPanel(t, q)
		if env.Totals.Actionable != 8 || env.Totals.Leftover != 2 {
			t.Fatalf("%v: totals = %+v, want 8 actionable, 2 leftover", q, env.Totals)
		}
		if len(env.Groups) != 3 {
			t.Fatalf("%v: the folder stack must ignore dir and q; got %+v", q, env.Groups)
		}
		for _, g := range env.Groups {
			resp := h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
				map[string]string{"dir": g.Dir}, reqOpts{csrf: true})
			if g.Ackable == 0 {
				if resp.StatusCode != http.StatusConflict {
					t.Errorf("%v %s: ackable 0 but the challenge answered %d", q, g.Dir, resp.StatusCode)
				}
				resp.Body.Close()
				continue
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%v %s: ackable %d but the challenge answered %d", q, g.Dir, g.Ackable, resp.StatusCode)
			}
			var ch map[string]any
			decode(t, resp, &ch)
			if int(ch["count"].(float64)) != g.Ackable {
				t.Errorf("%v %s: ackable %d, challenge count %v", q, g.Dir, g.Ackable, ch["count"])
			}
		}
	}

	// The leftover section is its own folder stack and total, and filters by dir.
	env := h.missingPanel(t, url.Values{"section": {"leftover"}, "dir": {dirC}})
	if env.Total != 1 || len(env.Frames) != 1 || env.Frames[0].State != "deleted" {
		t.Fatalf("leftover dir filter: total=%d frames=%+v", env.Total, env.Frames)
	}
	if len(env.Groups) != 2 {
		t.Fatalf("leftover groups = %+v, want the two folders holding leftovers", env.Groups)
	}
	for _, g := range env.Groups {
		if g.Ackable != 0 {
			t.Errorf("a leftover folder can never be ackable: %+v", g)
		}
	}
}

func TestMissingPanelRejectsUnknownSection(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	resp := h.do("GET", "/api/rigs/rig-1/frames?missing=true&section=gone", nil, reqOpts{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown section: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// AC-T2 / §13.3: the group-acknowledge refusal explains itself, counting deleted
// rows as gone separately from not_verified.
func TestAcknowledgeGroupNothingToDoNamesSkips(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.addRig("rig-1", false)
	now := time.Now().Unix()
	dir := filepath.Join(t.TempDir(), "M31")
	h.seedFrameState(t, "rig-1", dir, "d1", now, model.StateDeleted)
	h.seedFrameState(t, "rig-1", dir, "d2", now, model.StateDeleted)
	rp := h.seedFrameState(t, "rig-1", dir, "rp", now, model.StateVerified)
	if _, err := h.store.RequestRestore(context.Background(), "rig-1", rp, now); err != nil {
		t.Fatal(err)
	}

	resp := h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var body map[string]any
	decode(t, resp, &body)
	if body["code"] != codeAckNothingToDo {
		t.Fatalf("code = %v", body["code"])
	}
	want := "Nothing to acknowledge in this folder: 2 already removed from the device, 1 restore pending."
	if body["message"] != want {
		t.Fatalf("message = %q, want %q", body["message"], want)
	}

	// A challenge that does mint reports gone separately from not_verified.
	h.seedFrameState(t, "rig-1", dir, "v", now, model.StateVerified)
	h.seedFrameState(t, "rig-1", dir, "h", now, model.StateHeld)
	resp = h.do("POST", "/api/rigs/rig-1/missing-groups/acknowledge",
		map[string]string{"dir": dir}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var ch map[string]any
	decode(t, resp, &ch)
	skipped := ch["skipped"].(map[string]any)
	if skipped["gone"].(float64) != 2 || skipped["not_verified"].(float64) != 1 ||
		skipped["restore_pending"].(float64) != 1 || skipped["already"].(float64) != 0 {
		t.Fatalf("skipped = %+v, want gone 2, not_verified 1, restore_pending 1", skipped)
	}
}
