package devfiles

import (
	"path"
	"strings"
	"unicode"

	"github.com/macourteau/asiair-commander/internal/model"
)

// maxNameBytes is FAT's and SMB's longest single name component.
const maxNameBytes = 255

// Paths on the wire: share is a share name; dir and every item path are
// share-relative, "/"-separated, with no leading slash, and "" is the share
// root (the device seam's convention). Every request's paths, echoed run items
// included, pass through these checks before any device call.

// checkShare validates a share name: non-empty, one component.
func checkShare(share string) error {
	if share == "" || strings.ContainsAny(share, "/\\\x00") {
		return newErr(ErrBadPath, "", "invalid share name %q", share)
	}
	return nil
}

// checkDir validates a share-relative folder; "" is the share root.
func checkDir(dir string) error {
	if dir == "" {
		return nil
	}
	return checkRel(dir)
}

// checkRel validates a non-empty share-relative path: no leading slash, no
// empty, "." or ".." component, no backslash, no NUL, and no component naming
// one of Commander's marker files (the tool never touches them).
func checkRel(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return newErr(ErrBadPath, "", "invalid path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return newErr(ErrBadPath, "", "invalid path %q", p)
		}
		if isMarker(seg) {
			return newErr(ErrBadPath, "", "%q is Commander's device-identity file; the tool never touches it", p)
		}
	}
	return nil
}

// checkName validates a single name component (a selected entry).
func checkName(name string) error {
	if strings.Contains(name, "/") {
		return newErr(ErrBadPath, "", "invalid name %q", name)
	}
	return checkRel(name)
}

// nameKind chooses only the wording of validateName's refusal.
type nameKind int

const (
	nameFolder nameKind = iota // New folder: "Invalid folder name"
	nameFile                   // an upload: "Invalid file name"
	nameAny                    // a rename, file or folder: "Invalid name"
)

// String is the refusal's lead-in for the kind.
func (k nameKind) String() string {
	switch k {
	case nameFolder:
		return "Invalid folder name"
	case nameFile:
		return "Invalid file name"
	default:
		return "Invalid name"
	}
}

// nameForbidden are the characters FAT refuses in a name.
const nameForbidden = "/\\:*?\"<>|"

// validateName applies the name rules New folder, rename and upload share,
// mirroring the SPA's validateName: not empty, ".", or "..", none of
// / \ : * ? " < > |, no control or format character (Unicode Cc and Cf:
// zero-width spaces and joiners, bidi overrides and isolates, the BOM, which
// make a name that reads like another), no leading space, no trailing space or
// dot (FAT drops them), at most 255 bytes, and not a marker name.
func validateName(name string, kind nameKind) error {
	bad := func(why string) error {
		return newErr(ErrBadPath, "", "%s %q: %s", kind, name, why)
	}
	switch {
	case name == "":
		return bad("it is empty")
	case name == "." || name == "..":
		return bad("it is reserved")
	case len(name) > maxNameBytes:
		return bad("it is longer than 255 bytes")
	case strings.HasPrefix(name, " "):
		return bad("it starts with a space")
	case strings.HasSuffix(name, " ") || strings.HasSuffix(name, "."):
		return bad("it ends with a space or a dot")
	case strings.ContainsAny(name, nameForbidden):
		return bad(`it contains one of / \ : * ? " < > |`)
	case isMarker(name):
		return bad("it is Commander's device-identity file name")
	}
	for _, r := range name {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return bad("it contains a control or invisible formatting character")
		}
	}
	return nil
}

// checkStrictSegments refuses, for the requests that move or write a named
// entry (rename and upload), a share-relative path any of whose segments holds
// a character FAT never stores or ends in a dot or space. The listing's
// checkRel accepts them, but ":" is SMB stream syntax, SMB1's rename treats its
// source as a pattern, and the Linux vfat and exFAT drivers strip trailing dots
// on lookup: "Autorun." could reach the watched Autorun while every predicate
// compares it as different text. p has passed checkRel or checkDir.
func checkStrictSegments(p string) error {
	if p == "" {
		return nil
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.ContainsAny(seg, nameForbidden) ||
			strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return newErr(ErrBadPath, "", "invalid path %q", p)
		}
	}
	return nil
}

// isMarker reports whether name is one of Commander's device marker files, in
// any case: FAT lists them however they were written.
func isMarker(name string) bool {
	return strings.EqualFold(name, model.MediaMarkerName) ||
		strings.EqualFold(name, model.IdentityMarkerName)
}

// joinRel joins share-relative segments, keeping "" as the share root.
func joinRel(parts ...string) string {
	return strings.TrimPrefix(path.Join(parts...), "/")
}
