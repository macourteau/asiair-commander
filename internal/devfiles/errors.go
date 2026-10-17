package devfiles

import (
	"errors"
	"fmt"

	"github.com/macourteau/asiair-commander/internal/device"
)

// Sentinels the web handler maps to the API's error codes. Every error the
// service returns for one of them is an *Error whose Kind is the sentinel, so
// errors.Is matches and Error() is the message to show. Any other error is an
// unexpected failure (500).
var (
	// ErrUnknownRig: the rig is not configured, or not managed (404).
	ErrUnknownRig = errors.New("devfiles: unknown rig")
	// ErrBadPath: a share, folder, name or item path fails validation (400 bad_path).
	ErrBadPath = errors.New("devfiles: bad path")
	// ErrTooMany: a selection exceeds the file or run-body cap (400 too_many).
	ErrTooMany = errors.New("devfiles: too many files")
	// ErrIntoItself: a copy destination lies inside a selected folder (400 into_itself).
	ErrIntoItself = errors.New("devfiles: copy into itself")
	// ErrSameFolder: a copy's source and destination are one folder (400 same_folder).
	ErrSameFolder = errors.New("devfiles: same folder")
	// ErrTypeConflict: a file stands where a folder must go, or the reverse (409 type_conflict).
	ErrTypeConflict = errors.New("devfiles: type conflict")
	// ErrNotFound: the path or share is gone (404 not_found).
	ErrNotFound = errors.New("devfiles: not found")
	// ErrExists: the name is taken (409 exists).
	ErrExists = errors.New("devfiles: already exists")
	// ErrNoShare: the other rig has no share by that name (409 no_share).
	ErrNoShare = errors.New("devfiles: no such share")
	// ErrLayout: creating a folder would hide the share's ASIAIR/-nested root
	// from the harvester (the layout guard; 409 layout).
	ErrLayout = errors.New("devfiles: would hide a watched root")
	// ErrBusy: another copy, upload, rename, delete, download or plan holds the operation lock (409 busy).
	ErrBusy = errors.New("devfiles: busy")
	// ErrUnarchived: the unarchived count grew since the user confirmed (409 unarchived).
	ErrUnarchived = errors.New("devfiles: unarchived count grew")
	// ErrIdentity: a different device answered at the rig's address (409 identity).
	ErrIdentity = errors.New("devfiles: wrong device")
	// ErrHostBound: the rig has no GUID yet, so writes are refused (409 host_bound).
	ErrHostBound = errors.New("devfiles: rig not yet identified")
	// ErrDenied: the device refused the operation (403 denied).
	ErrDenied = errors.New("devfiles: access denied")
	// ErrInUse: the device reports the file in use, a sharing violation (409 in_use).
	ErrInUse = errors.New("devfiles: in use")
	// ErrUnreachable: the rig could not be reached, retries included (503 unreachable).
	ErrUnreachable = errors.New("devfiles: unreachable")
	// ErrCancelled: the request's context ended; the handler writes nothing more.
	ErrCancelled = errors.New("devfiles: cancelled")
	// ErrClosed: the service is shutting down.
	ErrClosed = errors.New("devfiles: service closed")
)

// Sentinels of rename (design device-file-browser §6).
var (
	// ErrWatched: the request would move or create a path the harvester keys by
	// name, a watched root or a reserved path (409 watched). A copy or upload
	// answers it for a file that would stand where such a path must go.
	ErrWatched = errors.New("devfiles: watched or reserved path")
)

// Sentinels of upload staging (design device-file-browser §6).
var (
	// ErrLengthRequired: a staging request did not declare its length (411
	// length_required).
	ErrLengthRequired = errors.New("devfiles: length required")
	// ErrStagingGone: the staging id is unknown, still receiving, expired,
	// evicted, discarded or another session's (410 staging_gone).
	ErrStagingGone = errors.New("devfiles: staged file gone")
	// ErrStagingFull: the staging cap is reached with every staged file in use,
	// or Commander's disk would keep less than its reserve (507 staging_full).
	ErrStagingFull = errors.New("devfiles: staging full")
	// ErrStageBody: a staging request's body failed, ended short of its
	// declared length or ran past it. Nobody reads an answer to a broken body,
	// so, like ErrMidStream, it has no code: the web handler aborts the request.
	ErrStageBody = errors.New("devfiles: staging body failed")
)

// AllErrors lists every sentinel an *Error's Kind can be: the blocks above and
// ErrMidStream. A sentinel added to the package belongs here too, so the web
// handler's test can prove each one answers with a code rather than a 500.
func AllErrors() []error {
	return []error{
		ErrUnknownRig, ErrBadPath, ErrTooMany, ErrIntoItself, ErrSameFolder,
		ErrTypeConflict, ErrNotFound, ErrExists, ErrNoShare, ErrLayout, ErrBusy,
		ErrUnarchived, ErrIdentity, ErrHostBound, ErrDenied, ErrInUse,
		ErrUnreachable, ErrCancelled, ErrClosed, ErrMidStream,
		ErrWatched,
		ErrLengthRequired, ErrStagingGone, ErrStagingFull, ErrStageBody,
	}
}

// Error is a classified failure: Kind is one of the sentinels above, Rig the
// rig it concerns ("" when none), Msg the user-facing message and Err the
// underlying cause, if any.
type Error struct {
	Kind error
	Rig  string
	Msg  string
	Err  error
}

func (e *Error) Error() string { return e.Msg }

// Unwrap exposes both the sentinel and the cause, so errors.Is matches the
// sentinel and device.IsNetwork still sees a network cause.
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Err}
}

// newErr builds a classified error for rig with a formatted message.
func newErr(kind error, rig string, format string, args ...any) *Error {
	return &Error{Kind: kind, Rig: rig, Msg: fmt.Sprintf(format, args...)}
}

// classify maps a device error onto a sentinel, naming what failed (what) on
// rigName. Network errors and anything unclassified are returned unchanged, so
// withRetry still sees a network failure as one. IsInUse is tested before
// IsPermission because a sharing violation is a permission error too.
func classify(err error, rigID, rigName, what string) error {
	var e *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &e):
		return err
	case device.IsNetwork(err):
		return err
	case device.IsNotFound(err):
		return &Error{Kind: ErrNotFound, Rig: rigID, Msg: fmt.Sprintf("%s no longer exists on %s", what, rigName), Err: err}
	case device.IsExist(err):
		return &Error{Kind: ErrExists, Rig: rigID, Msg: fmt.Sprintf("%s already exists on %s", what, rigName), Err: err}
	case device.IsInUse(err):
		return &Error{Kind: ErrInUse, Rig: rigID, Msg: fmt.Sprintf("%s is in use on %s", what, rigName), Err: err}
	case device.IsPermission(err):
		return &Error{Kind: ErrDenied, Rig: rigID, Msg: fmt.Sprintf("%s refused: access denied (%s)", rigName, what), Err: err}
	}
	return err
}
