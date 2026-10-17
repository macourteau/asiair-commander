package model

// RetargetResult is one frame's outcome from an apply-retarget request. Status is
// one of "retargeted" | "skipped" | "failed"; Reason explains a non-success, and
// the paths report where the archive moved (design routing-and-filing §9.6).
type RetargetResult struct {
	FrameID int64  `json:"frame_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	OldPath string `json:"old_path,omitempty"`
	NewPath string `json:"new_path,omitempty"`
}

// RetargetPreview is one frame's projected retarget destination, computed without
// any NAS/device I/O. NewPath is the index-0 canonical path (apply may
// disambiguate), night ordinal included: the preview resolves the same reads the
// filing path does, without ever assigning an ordinal.
//
// NightApprox marks an ordinal that something filed between the preview and the
// apply could still shift — every case but a date the ledger has already assigned.
// ExistingSpellings names the raw OBJECT spellings already filed in the folder the
// frame resolves to when they differ from the target as it will be written: the
// night is unified, the archive's spellings are not, and anything grouping by
// OBJECT still sees two targets. Eligible/Reason report scope, and Reason is also
// how an eligible row explains a read the preview could not make.
type RetargetPreview struct {
	FrameID           int64    `json:"frame_id"`
	CurrentTarget     string   `json:"current_target"`
	OldPath           string   `json:"old_path"`
	NewPath           string   `json:"new_path"`
	NightApprox       bool     `json:"night_approx"`
	ExistingSpellings []string `json:"existing_spellings,omitempty"`
	Eligible          bool     `json:"eligible"`
	Reason            string   `json:"reason,omitempty"`
}
