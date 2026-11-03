package model

// EditResult is one frame's outcome from an apply-edit request. Status is one of
// "edited" | "skipped" | "failed"; Reason explains a non-success, and an "edited"
// result carries one too when the edit landed but something about it needs the
// operator's attention. The paths report where the archive was and is (design
// header-edits §9.1).
type EditResult struct {
	FrameID int64  `json:"frame_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	OldPath string `json:"old_path,omitempty"`
	NewPath string `json:"new_path,omitempty"`
}

// EditPreview is one frame's projected header edit, computed without any
// NAS/device I/O (design header-edits §9.1). NewPath is the index-0 canonical path
// (apply may disambiguate a re-filed name), night ordinal included: the preview
// resolves the same reads the filing path does, without ever assigning an ordinal.
//
// CurrentValue is the frame's stored value for the edited key, empty with
// CurrentPresent false when the frame carries no such card. InPlace reports that
// apply will supersede the archive under its own name rather than re-file it
// (design header-edits §8).
//
// NightApprox (OBJECT only) marks an ordinal that something filed between the
// preview and the apply could still shift — every case but a date the ledger has
// already assigned. ExistingSpellings names, for OBJECT, the raw spellings already
// filed in the folder the frame resolves to when they differ from the target as it
// will be written; for FILTER, the rig's filter values that equal the new value
// case-insensitively but not exactly, which stacking software may group apart.
// Eligible/Reason report scope, and Reason is also how an eligible row explains a
// read the preview could not make.
type EditPreview struct {
	FrameID           int64    `json:"frame_id"`
	CurrentValue      string   `json:"current_value"`
	CurrentPresent    bool     `json:"current_present"`
	OldPath           string   `json:"old_path"`
	NewPath           string   `json:"new_path"`
	Eligible          bool     `json:"eligible"`
	Reason            string   `json:"reason,omitempty"`
	InPlace           bool     `json:"in_place"`
	NightApprox       bool     `json:"night_approx"`
	ExistingSpellings []string `json:"existing_spellings,omitempty"`
}
