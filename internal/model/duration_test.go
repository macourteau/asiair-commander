package model

import (
	"encoding/json"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestDurationUnitJSONRejectsYAMLLenient pins the deliberate asymmetry (C2):
// the JSON/API path a form uses requires an explicit unit and rejects a bare
// number, while UnmarshalYAML stays lenient and accepts bare seconds so an
// existing hand-edited config.yaml still loads.
func TestDurationUnitJSONRejectsYAMLLenient(t *testing.T) {
	// JSON: a bare number is rejected (the form footgun the API reports per field).
	var dJSON Duration
	if err := json.Unmarshal([]byte(`60`), &dJSON); err == nil {
		t.Errorf("JSON bare number 60: want error, got %v", dJSON.D())
	}
	// JSON: an unquoted-unit string is also rejected (missing unit).
	if err := json.Unmarshal([]byte(`"60"`), &dJSON); err == nil {
		t.Errorf("JSON unitless string \"60\": want error, got %v", dJSON.D())
	}
	// JSON: an explicit-unit string is accepted.
	if err := json.Unmarshal([]byte(`"5m"`), &dJSON); err != nil {
		t.Fatalf("JSON \"5m\": unexpected error %v", err)
	}
	if dJSON.D() != 5*time.Minute {
		t.Errorf("JSON \"5m\" = %v, want 5m", dJSON.D())
	}

	// YAML: a bare number is accepted as seconds (compat with hand-edited configs).
	var dYAML Duration
	if err := yaml.Unmarshal([]byte(`60`), &dYAML); err != nil {
		t.Fatalf("YAML bare number 60: unexpected error %v", err)
	}
	if dYAML.D() != 60*time.Second {
		t.Errorf("YAML bare 60 = %v, want 60s", dYAML.D())
	}
	// YAML: an explicit-unit string still works.
	if err := yaml.Unmarshal([]byte(`"5m"`), &dYAML); err != nil {
		t.Fatalf("YAML \"5m\": unexpected error %v", err)
	}
	if dYAML.D() != 5*time.Minute {
		t.Errorf("YAML \"5m\" = %v, want 5m", dYAML.D())
	}
}
