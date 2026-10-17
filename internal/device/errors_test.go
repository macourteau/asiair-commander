package device

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"

	smb1 "github.com/macourteau/smb1client"
)

// The error values below are built the way smb1client builds them (session.go:
// permissionError, mapRemoveDirError, mapSMBErrorToLinkError), from its exported
// types. The integration suite checks the same predicates against errors a real
// Samba server provokes.
var (
	libSharingViolation = fmt.Errorf("%w: %w", os.ErrPermission, &smb1.ResponseError{Code: 0xC0000043})
	libDeleteInUse      = &os.PathError{Op: "remove", Path: "a.fit", Err: libSharingViolation}
	libRenameInUse      = &os.LinkError{Op: "rename", Old: "a", New: "b", Err: libSharingViolation}
	libDirNotEmpty      = &os.PathError{Op: "remove", Path: "Night1", Err: &smb1.ResponseError{Code: 0xC0000101}}
	libCollision        = &os.PathError{Op: "open", Path: "a.fit", Err: &smb1.ResponseError{Code: 0xC0000035}}
	libRenameCollision  = &os.LinkError{Op: "rename", Old: "a", New: "b", Err: &smb1.ResponseError{Code: 0xC0000035}}
	libDenied           = &os.PathError{Op: "remove", Path: "a.fit", Err: os.ErrPermission}
	libRenameDenied     = &os.LinkError{Op: "rename", Old: "a", New: "b", Err: os.ErrPermission}
	libNotFound         = &os.PathError{Op: "remove", Path: "a.fit", Err: os.ErrNotExist}
	libExistSentinel    = &os.PathError{Op: "mkdir", Path: "d", Err: os.ErrExist}
	plainPermission     = os.ErrPermission
)

type predicateCase struct {
	name string
	err  error
	want bool
}

func runPredicate(t *testing.T, pred func(error) bool, cases []predicateCase) {
	t.Helper()
	for _, c := range cases {
		if got := pred(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v (err %v)", c.name, got, c.want, c.err)
		}
	}
}

func TestIsExist(t *testing.T) {
	runPredicate(t, IsExist, []predicateCase{
		{"create collision", libCollision, true},
		{"rename collision", libRenameCollision, true},
		{"os.ErrExist", libExistSentinel, true},
		{"fake collision", existError("open", "a.fit"), true},
		{"not found", libNotFound, false},
		{"denied", libDenied, false},
		{"in use", libDeleteInUse, false},
		{"dir not empty", libDirNotEmpty, false},
		{"nil", nil, false},
	})
}

func TestIsInUse(t *testing.T) {
	runPredicate(t, IsInUse, []predicateCase{
		{"delete in use", libDeleteInUse, true},
		{"rename in use", libRenameInUse, true},
		{"fake delete in use", SharingViolationError("remove", "a.fit"), true},
		{"fake rename in use", SharingViolationLinkError("rename", "a", "b"), true},
		{"plain os.ErrPermission", plainPermission, false},
		{"denied", libDenied, false},
		{"rename denied", libRenameDenied, false},
		{"not found", libNotFound, false},
		{"dir not empty", libDirNotEmpty, false},
		{"nil", nil, false},
	})
	// The reason IsInUse goes first: the permission view still matches.
	if !errors.Is(libDeleteInUse, os.ErrPermission) || !IsPermission(libDeleteInUse) {
		t.Fatal("a sharing violation should still read as a permission error")
	}
}

func TestIsDirNotEmpty(t *testing.T) {
	runPredicate(t, IsDirNotEmpty, []predicateCase{
		{"library shape", libDirNotEmpty, true},
		{"fake shape", DirNotEmptyError("Night1"), true},
		{"through adaptErr", adaptErr(libDirNotEmpty), true},
		{"wrapped further", fmt.Errorf("delete: %w", libDirNotEmpty), true},
		{"not found", libNotFound, false},
		{"fs.ErrNotExist", fs.ErrNotExist, false},
		{"denied", libDenied, false},
		{"in use", libDeleteInUse, false},
		{"collision", libCollision, false},
		{"nil", nil, false},
	})
}

func TestIsPermission(t *testing.T) {
	runPredicate(t, IsPermission, []predicateCase{
		{"denied", libDenied, true},
		{"rename denied", libRenameDenied, true},
		{"plain os.ErrPermission", plainPermission, true},
		{"in use", libDeleteInUse, true},
		{"rename in use", libRenameInUse, true},
		{"not found", libNotFound, false},
		{"collision", libCollision, false},
		{"dir not empty", libDirNotEmpty, false},
		{"nil", nil, false},
	})
}
