package model

import (
	"fmt"
	"time"
)

// Duration is a time.Duration that marshals to/from a human-legible Go duration
// string ("45s", "5m", "1h") in YAML and JSON, so the config file stays readable
// (design architecture §7: "durations are Go strings so the file is
// human-legible").
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalYAML renders the duration as its Go string form.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// UnmarshalYAML accepts a Go duration string ("120s") or a bare number of
// seconds (for forgiving hand-edits, though the UI always writes strings). The
// bare-seconds fallback stays lenient on purpose: the deployed host runs off a
// hand-editable config.yaml, so an existing `poll_interval: 30` must still load
// even though the JSON/API path a form uses requires an explicit unit. yaml.v3
// coerces a scalar int into a string ("30"), so ParseDuration is tried first and
// a unit-less value falls through to the seconds fallback rather than erroring.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		if parsed, perr := time.ParseDuration(s); perr == nil {
			*d = Duration(parsed)
			return nil
		}
	}
	var secs float64
	if err := unmarshal(&secs); err != nil {
		return fmt.Errorf("duration must be a Go duration string like \"120s\"")
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalJSON / UnmarshalJSON mirror the YAML form for the REST API.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}
