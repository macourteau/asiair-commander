// Package sanitize makes device- and user-controlled values safe to use as a
// single Windows filesystem path component. OBJECT, INSTRUME, and FILTER FITS
// header cards, and free-text template segments, are device- or
// user-controlled and land directly in destination paths (design
// routing-and-filing §4); this package is the one place that rule set lives, so
// Router and anything else that builds a path from such a value goes through it
// rather than a parallel copy.
//
// Unlike the sanitizer from an earlier project that this package adapts,
// Component never appends a collision-disambiguation suffix. That sanitizer
// needs one because it derives durable, colliding-by-design directory names
// (per-instance spool state) straight from the sanitized value. Router's
// consumers are template path segments; filename-level collisions there are
// disambiguated at filing time with the frame's own content (design
// routing-and-filing §4, "Filename uniqueness is guaranteed on write"), so a
// hash suffix on every segment would just be noise baked into paths that are
// otherwise meant to be human-readable.
package sanitize

import "strings"

// MaxComponentLen caps a sanitized component at 200 runes. NTFS allows up to
// 255 UTF-16 code units per path component, and extended-length paths lift
// the legacy 260-character MAX_PATH ceiling on the full path (handled where
// paths are assembled, not here). 200 leaves headroom under the per-component
// limit for whatever a caller appends downstream, while still turning a
// pathological header value into a bounded name instead of an unbounded one.
const MaxComponentLen = 200

// reservedWindowsNames are device names Windows refuses as a file or
// directory name in any location, with or without an extension, regardless
// of case.
var reservedWindowsNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// windowsIllegal are the characters NTFS refuses in a path component, beyond
// the C0 control range (handled separately so the switch in Component stays
// a simple two-way branch).
const windowsIllegal = `<>:"/\|?*`

// Component sanitizes name into a single Windows-safe path segment:
//
//   - Path separators ('/', '\'), the other Windows-illegal characters
//     (<>:"|?*), and control characters are replaced with '_'. An embedded
//     separator is neutralized rather than treated as a boundary, so it can
//     never be mistaken for one by whatever code later joins segments into
//     a path — Component's caller is responsible for path structure, not
//     the value being sanitized.
//   - Trailing dots and spaces are trimmed: Windows silently strips them
//     from any name it accepts, so a value differing only by them would
//     collide anyway.
//   - A value of "." or ".." (or anything that is entirely dots and spaces)
//     collapses to "" under the trim rule above — there is no separate
//     traversal check because embedded separators are already neutralized,
//     so ".." can only ever appear as a full, harmless value or reduce to
//     empty here.
//   - A reserved device name (CON, PRN, NUL, AUX, COM1-9, LPT1-9), matched
//     case-insensitively against the portion before the first dot, is
//     prefixed with '_'. Windows refuses the bare name in any directory
//     regardless of extension, so this is the transform that lets a
//     legitimately occurring value (a FILTER literally named "Aux", say)
//     still file instead of forcing every such frame to quarantine.
//   - The result is capped at MaxComponentLen runes, then re-trimmed in
//     case truncation exposed a new trailing dot or space.
//
// Case is never changed. Case-only collisions on a case-insensitive
// filesystem are a filing-time concern (disambiguation), not sanitize's.
//
// Component returns "" when name is empty, or when it sanitizes to nothing
// (an all-separator, all-illegal-character, all-dots-and-spaces, or
// truncated-to-nothing value). That is the only signal: a routing-critical
// segment resolving to "" is the caller's (Router's) cue to quarantine the
// frame per design routing-and-filing §4. Check the return value directly —
// there is no separate IsSafe/IsEmpty predicate to keep in sync with it.
func Component(name string) string {
	if name == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7F:
			b.WriteRune('_')
		case strings.ContainsRune(windowsIllegal, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}

	s := strings.TrimRight(b.String(), ". ")
	if s == "" {
		return ""
	}

	base := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		base = s[:i]
	}
	if reservedWindowsNames[strings.ToLower(base)] {
		s = "_" + s
	}

	if runes := []rune(s); len(runes) > MaxComponentLen {
		s = strings.TrimRight(string(runes[:MaxComponentLen]), ". ")
	}

	return s
}
