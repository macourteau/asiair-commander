package sanitize

import (
	"strings"
	"testing"
)

func TestComponent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Empty and degenerate inputs.
		{"empty string", "", ""},
		{"single dot", ".", ""},
		{"double dot", "..", ""},
		{"many dots", "......", ""},
		{"spaces only", "   ", ""},
		{"dots and spaces only", " . . . ", ""},

		// Happy-path values straight from the real fixtures design routing-and-filing
		// §4 sanitises.
		{"object with space", "NGC 7000", "NGC 7000"},
		{"object with hyphen", "C-2025 A6", "C-2025 A6"},
		{"filter code", "HA", "HA"},
		{"sensor name", "ZWO ASI6200MM Pro", "ZWO ASI6200MM Pro"},

		// Windows reserved device names, case-insensitive, with and
		// without an extension.
		{"reserved upper", "CON", "_CON"},
		{"reserved lower", "con", "_con"},
		{"reserved mixed case", "CoM3", "_CoM3"},
		{"reserved with extension", "CON.txt", "_CON.txt"},
		{"reserved nul", "NUL", "_NUL"},
		{"reserved prn", "PRN", "_PRN"},
		{"reserved aux", "AUX", "_AUX"},
		{"reserved lpt9", "LPT9", "_LPT9"},
		{"reserved com1", "COM1", "_COM1"},
		{"not reserved com0", "COM0", "COM0"},
		{"not reserved com10", "COM10", "COM10"},
		{"not reserved lpt0", "LPT0", "LPT0"},
		{"reserved substring is not reserved", "CONSTELLATION", "CONSTELLATION"},

		// Path separators embedded in a single value must never be
		// interpreted as structure.
		{"embedded forward slash", "M31/M110", "M31_M110"},
		{"embedded backslash", "Ha\\O3", "Ha_O3"},
		{"traversal with backslashes", `..\..\Windows`, `.._.._Windows`},
		{"traversal with forward slashes", "../../Windows", ".._.._Windows"},
		{"drive letter attempt", "C:", "C_"},
		{"drive letter with path", `C:\Windows\System32`, "C__Windows_System32"},

		// Illegal characters and control characters.
		{"illegal chars", `a<b>c:d"e/f\g|h?i*j`, "a_b_c_d_e_f_g_h_i_j"},
		{"embedded NUL", "abc\x00def", "abc_def"},
		{"embedded tab", "abc\tdef", "abc_def"},
		{"embedded DEL", "abc\x7Fdef", "abc_def"},

		// Trailing dots/spaces (Windows strips them); leading spaces
		// are left alone since Windows accepts them.
		{"trailing dot", "file.", "file"},
		{"trailing dots", "file...", "file"},
		{"trailing space", "file ", "file"},
		{"trailing dot and space", "file. . ", "file"},
		{"leading space preserved", " file", " file"},

		// Unicode passes through unchanged (NTFS permits it); case is
		// never altered anywhere in the pipeline.
		{"unicode letters", "café", "café"},
		{"unicode symbol", "M31 ☾ Ha", "M31 ☾ Ha"},
		{"mixed case preserved", "MixedCase Name", "MixedCase Name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Component(tt.input); got != tt.want {
				t.Errorf("Component(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestComponent_LengthCap(t *testing.T) {
	t.Run("truncates to MaxComponentLen runes", func(t *testing.T) {
		input := strings.Repeat("a", MaxComponentLen+50)
		got := Component(input)
		if n := len([]rune(got)); n != MaxComponentLen {
			t.Fatalf("len(Component(...)) = %d, want %d", n, MaxComponentLen)
		}
		if got != strings.Repeat("a", MaxComponentLen) {
			t.Fatalf("Component(...) did not simply truncate: got %q", got)
		}
	})

	t.Run("re-trims a trailing dot exposed by truncation", func(t *testing.T) {
		// The 200th rune lands on '.'; truncation must re-trim it
		// rather than hand back a name Windows would itself strip.
		input := strings.Repeat("a", MaxComponentLen-1) + "." + strings.Repeat("b", 50)
		want := strings.Repeat("a", MaxComponentLen-1)
		if got := Component(input); got != want {
			t.Fatalf("Component(...) = %q, want %q", got, want)
		}
	})

	t.Run("truncation that leaves only dots collapses to empty", func(t *testing.T) {
		input := strings.Repeat(".", MaxComponentLen) + "CON"
		if got := Component(input); got != "" {
			t.Fatalf("Component(...) = %q, want empty", got)
		}
	})

	t.Run("unicode runes are counted, not bytes", func(t *testing.T) {
		// Each 'é' is 2 bytes in UTF-8 but one rune; the cap must
		// apply per-rune so multi-byte content isn't truncated mid-
		// codepoint or capped too aggressively.
		input := strings.Repeat("é", MaxComponentLen+10)
		got := Component(input)
		if n := len([]rune(got)); n != MaxComponentLen {
			t.Fatalf("len(Component(...)) = %d, want %d", n, MaxComponentLen)
		}
	})
}

// TestComponent_EmptySignalsQuarantine documents the contract a Router
// caller relies on: there is no separate predicate, the empty return value
// itself is the quarantine signal for a routing-critical segment. Only
// values that are entirely dots/spaces collapse to empty — a value made
// entirely of illegal characters (e.g. "///") sanitizes to a non-empty run
// of '_' instead, since underscore substitutes are never trimmed.
func TestComponent_EmptySignalsQuarantine(t *testing.T) {
	degenerate := []string{"", ".", "..", "...", "   ", " . . "}
	for _, in := range degenerate {
		if got := Component(in); got != "" {
			t.Errorf("Component(%q) = %q, want empty (quarantine signal)", in, got)
		}
	}

	nonDegenerate := []string{"///", `\\\`, ":::"}
	for _, in := range nonDegenerate {
		if got := Component(in); got == "" {
			t.Errorf("Component(%q) = %q, want non-empty (all-illegal-char values sanitize to underscores, not empty)", in, got)
		}
	}
}
