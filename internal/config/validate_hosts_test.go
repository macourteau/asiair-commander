package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

func TestNormalizeAllowedHosts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty becomes localhost", nil, []string{"localhost"}},
		{"all-blank becomes localhost", []string{"", "   "}, []string{"localhost"}},
		{"case and space folded", []string{"  NAS.Local "}, []string{"nas.local"}},
		{"trailing dot stripped", []string{"nas.local."}, []string{"nas.local"}},
		{"port stripped", []string{"nas.local:8686"}, []string{"nas.local"}},
		{"port and trailing dot", []string{"nas.local.:8686"}, []string{"nas.local"}},
		{"duplicates dropped", []string{"nas.local", "NAS.LOCAL:8686", "nas.local."}, []string{"nas.local"}},
		{"ipv6 unwrapped", []string{"[::1]:8686", "[fd00::1]"}, []string{"::1", "fd00::1"}},
		{"bare ipv6 untouched", []string{"::1"}, []string{"::1"}},
		{"ipv4 with port", []string{"192.168.1.10:8686"}, []string{"192.168.1.10"}},
		// hostOnly would turn this into "http", silently rewriting a stored entry
		// and then persisting the rewrite on the operator's next unrelated save.
		{"scheme survives normalization", []string{"http://nas.local"}, []string{"http://nas.local"}},
		{"scheme with port survives", []string{"http://nas.local:8686"}, []string{"http://nas.local:8686"}},
		{"wildcard survives", []string{"*"}, []string{"*"}},
		{"order preserved", []string{"b.local", "a.local"}, []string{"b.local", "a.local"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &model.Config{}
			cfg.Web.AllowedHosts = tc.in
			NormalizeAllowedHosts(cfg)
			if !reflect.DeepEqual(cfg.Web.AllowedHosts, tc.want) {
				t.Fatalf("got %v, want %v", cfg.Web.AllowedHosts, tc.want)
			}
		})
	}
}

func TestValidateSubmittedHosts(t *testing.T) {
	cases := []struct {
		name      string
		submitted []string
		stored    []string
		wantError bool
		wantWarn  bool
		contains  string
	}{
		{"plain hostname", []string{"nas.local"}, nil, false, false, ""},
		{"hostname with port", []string{"nas.local:8686"}, nil, false, false, ""},
		{"wildcard", []string{"*"}, nil, true, false, "wildcard"},
		{"partial wildcard", []string{"*.local"}, nil, true, false, "wildcard"},
		{"scheme", []string{"https://nas.local"}, nil, true, false, "URL"},
		{"path", []string{"nas.local/ui"}, nil, true, false, "URL"},
		{"userinfo", []string{"user@nas.local"}, nil, true, false, "URL"},
		{"whitespace", []string{"nas local"}, nil, true, false, "whitespace"},
		// Consumer DHCP and NetBIOS names carry underscores, and the allowlist is an
		// exact-match map, so "my_nas" matches exactly what that browser sends.
		// Rejecting it while seedRequestOrigin accepts the same name would be
		// incoherent — both paths feed the one map.
		{"underscore accepted", []string{"my_nas"}, nil, false, false, ""},
		{"underscore in a subdomain accepted", []string{"my_nas.local"}, nil, false, false, ""},
		{"malformed shape still rejected", []string{"nas..local"}, nil, true, false, "valid hostname"},
		{"leading hyphen still rejected", []string{"-nas.local"}, nil, true, false, "valid hostname"},
		{"ip literal warns", []string{"192.168.1.10"}, nil, false, true, "DHCP"},
		{"ipv6 literal warns", []string{"fd00::1"}, nil, false, true, "DHCP"},
		{"public ip warns too", []string{"203.0.113.7"}, nil, false, true, "DHCP"},
		// Grandfathered: the SPA PUTs whole configs and the GET returns the list, so
		// a stored "*" round-trips straight back and must not block an unrelated edit.
		{"stored wildcard is warned not blocked", []string{"*"}, []string{"*"}, false, true, "wildcard"},
		{"stored scheme is warned not blocked", []string{"http://nas.local"}, []string{"http://nas.local"}, false, true, "URL"},
		{"stored entry matched after normalization", []string{"HTTP://NAS.LOCAL"}, []string{"http://nas.local"}, false, true, "URL"},
		{"empty entries ignored", []string{"", "  "}, nil, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := ValidateSubmittedHosts(tc.submitted, tc.stored)
			if HasErrors(problems) != tc.wantError {
				t.Fatalf("HasErrors = %v, want %v (%v)", HasErrors(problems), tc.wantError, problems)
			}
			hasWarn := false
			for _, p := range problems {
				if p.Severity == SeverityWarning {
					hasWarn = true
				}
			}
			if hasWarn != tc.wantWarn {
				t.Errorf("warning = %v, want %v (%v)", hasWarn, tc.wantWarn, problems)
			}
			if tc.contains != "" {
				found := false
				for _, p := range problems {
					if strings.Contains(p.Message, tc.contains) {
						found = true
					}
				}
				if !found {
					t.Errorf("want a problem mentioning %q, got %v", tc.contains, problems)
				}
			}
		})
	}
}

// design architecture §8.2 invariant 8: no newly-introduced wildcard entry can
// be stored, while one already present round-trips unchanged.
func TestAllowedHostsRejectsNewWildcard(t *testing.T) {
	if !HasErrors(ValidateSubmittedHosts([]string{"nas.local", "*"}, []string{"nas.local"})) {
		t.Fatal("a newly introduced wildcard must be an error")
	}
	if HasErrors(ValidateSubmittedHosts([]string{"*"}, []string{"*"})) {
		t.Fatal("a wildcard already stored must not block the write")
	}

	// Normalization never removes one either, so the round-trip is exact.
	cfg := &model.Config{}
	cfg.Web.AllowedHosts = []string{"*"}
	NormalizeAllowedHosts(cfg)
	if !reflect.DeepEqual(cfg.Web.AllowedHosts, []string{"*"}) {
		t.Fatalf("normalization changed a stored wildcard: %v", cfg.Web.AllowedHosts)
	}
}

func TestAcceptableSeededHost(t *testing.T) {
	// Waived: the hostname-shape check, because the value demonstrably reached the
	// server from the operator's own browser and refusing it would 403 the next call.
	for _, ok := range []string{"nas.local", "my_nas", "nas..local", "192.168.1.10", "fd00::1"} {
		if !AcceptableSeededHost(ok) {
			t.Errorf("AcceptableSeededHost(%q) = false, want true", ok)
		}
	}
	// Still enforced: Origin is free text a non-browser client sets to anything.
	for _, bad := range []string{"", "*", "http://nas.local", "nas.local/ui", "a b", "user@nas.local"} {
		if AcceptableSeededHost(bad) {
			t.Errorf("AcceptableSeededHost(%q) = true, want false", bad)
		}
	}
}

// design architecture §8.2 invariant 13: a stored config whose allowlist is
// absent, empty, or holds values the submitted-input rules would reject still
// loads, still passes Validate, and is therefore still writable — the regression
// this split exists to avoid, since putting these rules in Validate would 400
// every config PUT and every rig save on a field the operator never touched.
func TestLegacyAllowedHostsNeverBlockStoredConfig(t *testing.T) {
	cases := []struct {
		name   string
		stored []string
		want   []string
	}{
		{"key absent", nil, []string{"localhost"}},
		{"key empty", []string{}, []string{"localhost"}},
		{"blank entries", []string{"", "  "}, []string{"localhost"}},
		{"wildcard", []string{"*"}, []string{"*"}},
		{"scheme", []string{"http://nas.local"}, []string{"http://nas.local"}},
		{"underscore", []string{"my_nas"}, []string{"my_nas"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			// Save does not normalize, so this reproduces a hand-edited file on disk.
			stored := Defaults()
			stored.Web.AllowedHosts = tc.stored
			if err := Save(path, stored); err != nil {
				t.Fatalf("seed config: %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg.Web.AllowedHosts, tc.want) {
				t.Fatalf("loaded allowlist = %v, want %v", cfg.Web.AllowedHosts, tc.want)
			}
			if problems := Validate(cfg, nil); HasErrors(problems) {
				t.Fatalf("a stored config must still validate, got %v", problems)
			}
			// And a write that merely round-trips the stored value is not blocked.
			if HasErrors(ValidateSubmittedHosts(cfg.Web.AllowedHosts, cfg.Web.AllowedHosts)) {
				t.Fatalf("round-tripping the stored allowlist must not error")
			}
		})
	}
}
