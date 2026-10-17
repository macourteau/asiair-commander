package model

// StrandRisk names a directory the prune expects to be unable to remove, and why
// — a `.DS_Store` the Finder wrote, an operator's stacked masters, anything the
// tool does not manage. The preview carries these so a pile of leftover folders is
// explained up front rather than surfacing as skip reports at apply time (design
// routing-and-filing §8.9).
type StrandRisk struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
}

// RefileGroup is one `old dir → new dir` migration the operator can select. A
// group is identified by the PAIR (FromDir, ToDir), not by ToDir alone: two source
// directories can collapse onto one destination.
//
// Count counts distinct dest_paths, NOT rows — a frame present on two device
// shares is two rows sharing one file and one move, so the operator-facing number
// must say one (the same reason CountFramesTonight counts DISTINCT dest_path).
// FrameIDs still carries every row, because every row's dest_path is rewritten.
//
// FromDir and ToDir are ABSOLUTE, matching the stored dest_path
// (internal/rig/file.go) rather than route.Result.Dir, which is root-relative.
type RefileGroup struct {
	FromDir    string       `json:"from_dir"`
	ToDir      string       `json:"to_dir"`
	Count      int          `json:"count"`
	FrameIDs   []int64      `json:"frame_ids"`
	StrandRisk []StrandRisk `json:"strand_risk"`
}

// Refusal is a dest_path excluded from every group, and why: a held or
// missing-marked sharing row, a cross-rig sharer, a NULL or empty dest_path, or a
// frame the current templates would quarantine. A quarantined frame has no ToDir,
// so it cannot be a group member either. Refusals are never group members and so
// can never surface as a RefileResult — this is where "refused and reported"
// lands.
type Refusal struct {
	DestPath string `json:"dest_path"`
	Reason   string `json:"reason"`
}

// RefilePreview is the whole projected migration for one rig, computed from stored
// columns plus one ReadDir per directory the migration would vacate. Scope is
// "mosaic" (frames that route through the mosaic template) or "all". There is no
// truncation: the preview is a database scan and returns every group.
type RefilePreview struct {
	Groups   []RefileGroup `json:"groups"`
	Refusals []Refusal     `json:"refusals"`
	Scope    string        `json:"scope"`
}

// RefileResult is one file's outcome from an apply-refile request, ONE PER FILE
// rather than per row: a frame present on two device shares is two rows sharing
// one dest_path and one move, so its group's FrameIDs carries both rows while
// apply emits a single line. Status is one of "moved" | "skipped" | "failed";
// Reason explains a non-success.
type RefileResult struct {
	FrameID int64  `json:"frame_id"`
	Status  string `json:"status"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// RefileSummary is the final NDJSON line of an apply-refile stream. Prune outcomes
// are directories rather than frames, so they cannot be RefileResults, and the
// skips and their reasons have to be reported.
type RefileSummary struct {
	PrunedDirs  []string     `json:"pruned_dirs"`
	SkippedDirs []StrandRisk `json:"skipped_dirs"`
}
