// Package filter holds the pure predicates the rig worker's discovery step
// (design frame-lifecycle §3) applies to a device directory listing: what to prune
// before it is ever looked at (junk directories), what to drop by name
// (junk files), what extension policy governs transfer versus preview
// exclusion, and how a preview sidecar is paired to the frame it belongs to.
// Everything here is a name-only decision — no I/O beyond the shared marker-name
// constants in internal/model — so it can be exercised with plain strings and
// reused by both the SMB discovery path and its tests without pulling in smb1client.
//
// Matching throughout is case-insensitive: the ASIAIR's storage is FAT,
// which commonly reports names like FSCK0000.REC or .FIT/.JPG in uppercase,
// and a case-sensitive compare would either leak a preview onto the NAS or
// silently drop a science frame (design frame-lifecycle §3).
package filter

import (
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
)

// MediaIDMarkerName and RigIDMarkerName are this tool's own identity markers
// (design device-identity-and-media §4.5 "Guarding against binding a profile to
// the wrong device", and the H10 media marker). They are junk-class: never
// transferred, never treated as frame data, never deleted by cleanup. The
// literals live in internal/model as the single source of truth, shared with
// internal/mediaid (which reads/writes them), so the two can never drift.
const (
	MediaIDMarkerName = model.MediaMarkerName
	RigIDMarkerName   = model.IdentityMarkerName
)

// The junk-file filter's lists — exhaustive: this is the specification and
// the test fixture. Matching is case-insensitive against the file name
// only, never the path. Adapted from an earlier project's junk filter;
// ASIAIR-specific additions are noted inline.
var (
	junkFileNames = map[string]bool{
		".ds_store":                           true,
		"thumbs.db":                           true,
		"ehthumbs.db":                         true,
		"desktop.ini":                         true,
		".apdisk":                             true,
		".com.apple.timemachine.donotpresent": true,
		// This tool's own identity markers.
		strings.ToLower(MediaIDMarkerName): true,
		strings.ToLower(RigIDMarkerName):   true,
	}
	junkDirNames = map[string]bool{
		"#recycle":                  true, // Synology share-level recycle bin
		"@recycle":                  true, // QNAP recycle bin
		".@__thumb":                 true, // QNAP thumbnails
		".stfolder":                 true, // Syncthing marker
		".stversions":               true, // Syncthing versioning
		"system volume information": true,
		"$recycle.bin":              true,
		".spotlight-v100":           true,
		".fseventsd":                true,
		".trashes":                  true,
		".temporaryitems":           true,
		"lost+found":                true,
		"@eadir":                    true,
		// ASIAIR live-stack scratch directory (design architecture §7); never a source
		// of frame data, and its contents churn during a live session.
		"batch_stack_tmp": true,
	}
)

// IsJunkFile reports whether a file name is junk — dropped from discovery
// before it is ever classified.
func IsJunkFile(name string) bool {
	l := strings.ToLower(name)
	if junkFileNames[l] {
		return true
	}
	if strings.HasPrefix(l, "._") || strings.HasPrefix(l, "~$") {
		return true // AppleDouble; Office lock files
	}
	if strings.HasPrefix(l, "fsck") && strings.HasSuffix(l, ".rec") {
		return true // filesystem-recovery artifacts (FSCK*.REC)
	}
	if strings.HasSuffix(l, ".tmp") || strings.HasSuffix(l, "~") {
		return true
	}
	return false
}

// IsJunkDir reports whether a directory name is junk — pruned from the walk
// entirely rather than filtered entry by entry, so nothing under it (a
// recycle bin's discarded files, ASIAIR's live-stack scratch) is ever
// treated as source data.
func IsJunkDir(name string) bool {
	return junkDirNames[strings.ToLower(name)]
}

// Ext returns the lowercased extension under the fixed semantics: the
// substring after the final dot of the file name — "foo.tar.gz" has
// extension "gz" — and a dotfile (leading dot, no other dot, e.g.
// ".DS_Store") has none. Empty means no extension.
func Ext(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 { // no dot, or a dotfile's leading dot
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// ExtMatches reports whether name's extension (per Ext) appears in exts.
// exts entries are matched case-insensitively and may be given with or
// without a leading dot ("jpg" and ".jpg" are equivalent). A name with no
// extension never matches, regardless of exts.
func ExtMatches(name string, exts []string) bool {
	ext := Ext(name)
	if ext == "" {
		return false
	}
	for _, e := range exts {
		if strings.EqualFold(strings.TrimPrefix(e, "."), ext) {
			return true
		}
	}
	return false
}

// IsExcluded reports whether name's extension is in excludeExts — the
// transfer-exclusion test (design architecture §7 exclude_extensions, default
// jpg/jpeg): an excluded file is not streamed to the NAS, though it may
// still be tracked as a preview sidecar (IsSidecarOf). It is exactly
// ExtMatches; the separate name documents the decision it makes at the
// discovery call site.
func IsExcluded(name string, excludeExts []string) bool {
	return ExtMatches(name, excludeExts)
}

// previewSuffix is the ASIAIR's own preview-naming convention: a preview of
// frame "<stem>.fit" is written as "<stem>_thn.jpg" — never a bare
// "<stem>.jpg" (design frame-lifecycle §3).
const previewSuffix = "_thn"

// PreviewStem extracts the frame stem a preview name should pair with. It
// implements the ASIAIR convention exactly: previewName's extension must be
// one of previewExts (case-insensitive, e.g. jpg/jpeg), and the name
// (extension stripped) must end in "_thn" (case-insensitive); that suffix
// is stripped and the remainder returned as stem with ok=true. Anything
// else — wrong extension, or a base that does not end in "_thn" — returns
// ok=false.
//
// This is deliberately strict rather than falling back to a bare
// same-stem match: design frame-lifecycle §3 explicitly calls out that
// association "strips a trailing _thn before matching the frame's stem, not a
// naïve same-stem compare," because a same-named-but-different-purpose file
// (e.g. an operator's own unrelated same-stem JPEG dropped into the tree) must
// not be silently paired with — and later deleted alongside — a science frame
// it has nothing to do with. IsSidecarOf relies on this strict behavior; there
// is no separate lenient variant.
func PreviewStem(previewName string, previewExts []string) (stem string, ok bool) {
	if !ExtMatches(previewName, previewExts) {
		return "", false
	}
	base := previewName
	if i := strings.LastIndexByte(previewName, '.'); i > 0 {
		base = previewName[:i]
	}
	if len(base) < len(previewSuffix) || !strings.EqualFold(base[len(base)-len(previewSuffix):], previewSuffix) {
		return "", false
	}
	return base[:len(base)-len(previewSuffix)], true
}

// IsSidecarOf reports whether previewName is a preview (extension in
// previewExts, name ending "_thn" per PreviewStem) whose stem matches
// frameName's stem, case-insensitively. This is the association predicate
// the discovery path uses to pair an ASIAIR "_thn.jpg" preview to its
// parent frame (design frame-lifecycle §3) — both for exclude-but-track
// handling at pull time and for the orphan-preview sweep's parent lookup.
func IsSidecarOf(previewName, frameName string, previewExts []string) bool {
	stem, ok := PreviewStem(previewName, previewExts)
	if !ok {
		return false
	}
	frameStem := frameName
	if i := strings.LastIndexByte(frameName, '.'); i > 0 {
		frameStem = frameName[:i]
	}
	return strings.EqualFold(stem, frameStem)
}
