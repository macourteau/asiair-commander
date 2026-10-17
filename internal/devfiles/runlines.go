package devfiles

import "errors"

// The lines of a copy or delete run's NDJSON stream (design device-file-browser
// §4.3). The web handler encodes each value Emit receives as one line; the
// stream begins lazily, so an error returned before the first Emit is an
// ordinary JSON error.

// Emit hands one run line (a ProgressLine, ItemLine or SummaryLine) to the
// stream. A non-nil return, such as a write to a disconnected client, ends
// the run: the engine stops and returns that error, writing nothing more.
type Emit func(line any) error

// ProgressLine is emitted as each item starts; Current names that item.
type ProgressLine struct {
	Type       string `json:"type"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	Current    string `json:"current"`
}

// ItemLine reports one skipped or failed item, or a done item carrying a note.
// DstSize and DstMtime are set only on a copy's appeared or changed row.
type ItemLine struct {
	Type     string `json:"type"`
	Rel      string `json:"rel"`
	Outcome  string `json:"outcome"`
	Code     string `json:"code"`
	DstSize  *int64 `json:"dst_size,omitempty"`
	DstMtime *int64 `json:"dst_mtime,omitempty"`
	Message  string `json:"message,omitempty"`
}

// SummaryLine ends a run that was not cancelled.
type SummaryLine struct {
	Type    string `json:"type"`
	Done    int    `json:"done"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Aborted string `json:"aborted"`
}

// Line types.
const (
	lineProgress = "progress"
	lineItem     = "item"
	lineSummary  = "summary"
)

// Item outcomes, one per FilesItemLine.outcome in web/src/api/types.ts.
const (
	outcomeCopied = "copied"
	// outcomeDeleted is never sent: a deleted item has no line. It mirrors the
	// SPA's wire enum.
	outcomeDeleted = "deleted"
	outcomeSkipped = "skipped"
	outcomeFailed  = "failed"
)

// Reason codes, one per FilesReasonCode in web/src/api/types.ts.
const (
	// codeChoseSkip and codeStopped are never sent: the SPA makes those rows
	// itself, from the plan and from a stopped stream. They mirror its enum.
	codeChoseSkip         = "chose_skip"
	codeAppeared          = "appeared"
	codeTypeConflict      = "type_conflict"
	codeSourceGone        = "source_gone"
	codeChanged           = "changed"
	codeAlreadyGone       = "already_gone"
	codeNotEmpty          = "not_empty"
	codeKeptMarker        = "kept_marker"
	codeDenied            = "denied"
	codeInUse             = "in_use"
	codeUnreachable       = "unreachable"
	codeIdentity          = "identity"
	codeMtimeNotPreserved = "mtime_not_preserved"
	codeStopped           = "stopped"
	codeNotAttempted      = "not_attempted"
	codeError             = "error"
	// codeWatched fails a copied file that would stand at a reserved path,
	// found at write time after a configuration change since the plan.
	codeWatched = "watched"
)

// SummaryLine.Aborted values, one per FilesSummaryLine.aborted in
// web/src/api/types.ts: a run ended by an unreachable rig, or by a device that
// is not the rig or a rig not yet identified.
const (
	abortedUnreachable = "unreachable"
	abortedIdentity    = "identity"
)

// runAbort is the SummaryLine.Aborted value of an item failure that ends a
// multi-item run, or "" for one that only fails its item. An unreachable rig
// has used up its retries; a wrong device or a rig that lost its identity
// would be re-dialled and re-probed for every remaining item to the same end.
func runAbort(err error) string {
	switch {
	case errors.Is(err, ErrUnreachable):
		return abortedUnreachable
	case errors.Is(err, ErrIdentity), errors.Is(err, ErrHostBound):
		return abortedIdentity
	}
	return ""
}
