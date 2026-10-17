package devfiles

import (
	"errors"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

func TestPathValidation(t *testing.T) {
	good := []string{"a", "a/b", "EMMC Images/x", "Autorun/Light/M42/frame 1.fit", "._junk"}
	for _, p := range good {
		if err := checkRel(p); err != nil {
			t.Errorf("checkRel(%q) = %v, want ok", p, err)
		}
	}
	bad := []string{
		"", "/a", "a/", "a//b", "..", "a/../b", ".", "a/./b", `a\b`, "a\x00b",
		model.MediaMarkerName, "ASIAIR/" + model.IdentityMarkerName,
		"x/" + strings.ToUpper(model.MediaMarkerName),
	}
	for _, p := range bad {
		if err := checkRel(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("checkRel(%q) = %v, want bad_path", p, err)
		}
	}

	if err := checkDir(""); err != nil {
		t.Errorf("checkDir(\"\") = %v, want the share root accepted", err)
	}
	for _, s := range []string{"", "a/b", `a\b`, "a\x00"} {
		if err := checkShare(s); !errors.Is(err, ErrBadPath) {
			t.Errorf("checkShare(%q) = %v, want bad_path", s, err)
		}
	}
	if err := checkName("a/b"); !errors.Is(err, ErrBadPath) {
		t.Errorf("checkName(a/b) = %v, want bad_path", err)
	}

	if got := joinRel("", "a"); got != "a" {
		t.Errorf("joinRel = %q", got)
	}
	if got := joinRel("", ""); got != "" {
		t.Errorf("joinRel of the root = %q", got)
	}
}

func TestValidateName(t *testing.T) {
	good := []string{"M42", "Reference frames", "a.b", ".hidden", "Ünïcødé", strings.Repeat("a", 255)}
	bad := []string{
		"", ".", "..", "a/b", `a\b`, "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b",
		"tab\there", "nul\x00", "del\x7f", "trailing ", "trailing.", " leading",
		"c1\u0085ctl", "zero\u200bwidth", "zw\u200cnj", "zw\u200dj", "bidi\u202eover",
		"iso\u2066late", "\ufeffbom", "soft\u00adhyphen",
		strings.Repeat("a", 256), strings.Repeat("é", 128), // 256 bytes
		model.MediaMarkerName, strings.ToUpper(model.IdentityMarkerName),
	}
	// The rules are one set; the kind chooses only the refusal's wording, and
	// New folder's stays as it was.
	kinds := []struct {
		kind nameKind
		lead string
	}{
		{nameFolder, "Invalid folder name"},
		{nameFile, "Invalid file name"},
		{nameAny, "Invalid name"},
	}
	for _, k := range kinds {
		for _, n := range good {
			if err := validateName(n, k.kind); err != nil {
				t.Errorf("validateName(%q, %v) = %v, want ok", n, k.kind, err)
			}
		}
		for _, n := range bad {
			err := validateName(n, k.kind)
			if !errors.Is(err, ErrBadPath) {
				t.Errorf("validateName(%q, %v) = %v, want bad_path", n, k.kind, err)
				continue
			}
			if !strings.HasPrefix(err.Error(), k.lead+" ") {
				t.Errorf("validateName(%q) message %q, want it to lead with %q", n, err, k.lead)
			}
		}
	}
	err := validateName("a:b", nameFolder)
	if want := `Invalid folder name "a:b": it contains one of / \ : * ? " < > |`; err == nil || err.Error() != want {
		t.Errorf("New folder's wording = %v, want %q", err, want)
	}
}

func TestCheckStrictSegments(t *testing.T) {
	good := []string{"", "Preview", "Preview/a.fit", "a.b/c d", ".hidden/x", "Ünïcødé/._junk"}
	for _, p := range good {
		if err := checkStrictSegments(p); err != nil {
			t.Errorf("checkStrictSegments(%q) = %v, want ok", p, err)
		}
	}
	bad := []string{
		"Autorun.", "Autorun ", "Autorun./x", "Preview/a.fit.", "Preview/a.fit ",
		"Autorun:stream", "Preview/a:b", "Auto*", "Auto?un", `A"b`, "a<b", "a>b", "a|b",
		"Preview/sub./x.fit", `a\b`,
	}
	for _, p := range bad {
		if err := checkStrictSegments(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("checkStrictSegments(%q) = %v, want bad_path", p, err)
		}
	}
}
