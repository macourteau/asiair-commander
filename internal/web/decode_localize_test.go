package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/macourteau/asiair-commander/internal/config"
)

// TestUnitlessDurationNamesField (C1) checks that a duration typed without
// a unit — which fails inside json.Decode before Validate runs — comes back as a
// problems[] entry naming the offending field with a unit hint, on both the config
// PUT and the rig endpoints, rather than the generic "invalid body" error.
func TestUnitlessDurationNamesField(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	t.Run("config PUT", func(t *testing.T) {
		cfg := h.getConfig()
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal config: %v", err)
		}
		// A unit-less string in "Show missing frames for": rejected by UnmarshalJSON.
		m["missing_display_period"] = json.RawMessage(`"60"`)

		resp := h.do("PUT", "/api/config", m, reqOpts{csrf: true})
		fields := problemFields(t, resp, "config PUT")
		if fields["missing_display_period"] == "" {
			t.Fatalf("config PUT: want a problem naming missing_display_period, got %v", fields)
		}
	})

	t.Run("rig create", func(t *testing.T) {
		// A bare-number override on a rig create: id comes from the body.
		body := map[string]any{
			"id":            "rig-new",
			"name":          "New",
			"poll_interval": 60, // JSON number → not a duration string
		}
		resp := h.do("POST", "/api/rigs", body, reqOpts{csrf: true})
		fields := problemFields(t, resp, "rig create")
		if fields["rigs[rig-new].poll_interval"] == "" {
			t.Fatalf("rig create: want a problem naming rigs[rig-new].poll_interval, got %v", fields)
		}
	})

	t.Run("rig update", func(t *testing.T) {
		// Seed a valid rig, then update it with a unit-less duration: id from the URL.
		create := map[string]any{
			"id":      "rig-up",
			"name":    "Up",
			"managed": false,
		}
		resp := h.do("POST", "/api/rigs", create, reqOpts{csrf: true})
		if resp.StatusCode != 201 {
			t.Fatalf("seed rig: want 201, got %d", resp.StatusCode)
		}
		resp.Body.Close()

		body := map[string]any{
			"id":    "rig-up",
			"name":  "Up",
			"grace": "5", // unit-less string
		}
		resp = h.do("PUT", "/api/rigs/rig-up", body, reqOpts{csrf: true})
		fields := problemFields(t, resp, "rig update")
		if fields["rigs[rig-up].grace"] == "" {
			t.Fatalf("rig update: want a problem naming rigs[rig-up].grace, got %v", fields)
		}
	})
}

// problemFields decodes a 400 problems body and returns Field→Message. It fails if
// the status is not 400 or the body carries no problems (the generic-banner path).
func problemFields(t *testing.T, resp *http.Response, label string) map[string]string {
	t.Helper()
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("%s: want 400, got %d", label, resp.StatusCode)
	}
	var body struct {
		Problems []config.Problem `json:"problems"`
	}
	decode(t, resp, &body)
	if len(body.Problems) == 0 {
		t.Fatalf("%s: 400 had no problems[] (fell to the generic banner)", label)
	}
	out := map[string]string{}
	for _, p := range body.Problems {
		out[p.Field] = p.Message
	}
	return out
}
