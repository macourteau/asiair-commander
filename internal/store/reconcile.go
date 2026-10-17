package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
)

// ScanFrame is one row's minimal identity for the library scan: its id, the
// stored destination path (used verbatim for the confirming Stat that authorizes
// an irreversible deletion), the basename (for the cheap listing diff), and the
// current missing mark (so the scan can compute the sighting-to-clear statistic
// without a per-clear read).
type ScanFrame struct {
	ID            int64
	DestPath      string
	Base          string
	MissingSeenAt int64
}

// scannableStates are the frame states whose rows have a NAS copy worth looking
// for. dismissed rows have dest_path=” (excluded by the predicate) and failed
// rows never landed a copy.
const scannableStates = `('verified','deleted','unclassified','held')`

// UnderRoot reports whether dest lies at or under root. It is the single
// containment implementation shared by the scan's longest-match assignment, the
// relink clear, and the delete pass's "under no configured root" disqualifier —
// separator-anchored and case-folded, because destinations are NTFS and this
// predicate gates a deletion authorizer. It is done in Go, not SQL, because
// SQLite's LIKE folds only ASCII and cannot express separator anchoring cleanly.
func UnderRoot(dest, root string) bool {
	if root == "" {
		return false
	}
	d := strings.ToLower(filepath.Clean(dest))
	r := strings.ToLower(filepath.Clean(root))
	if d == r {
		return true
	}
	return strings.HasPrefix(d, r+string(filepath.Separator))
}

// longestRoot returns the configured root that most-specifically contains dest,
// or "" if none does.
func longestRoot(dest string, roots []string) string {
	best, bestLen := "", -1
	for _, r := range roots {
		// Specificity is measured on the cleaned path, matching UnderRoot's own
		// normalization: a root written with redundant separators ("/astro////")
		// must not out-rank a genuinely more specific nested root ("/astro/cal").
		if n := len(filepath.Clean(r)); UnderRoot(dest, r) && n > bestLen {
			best, bestLen = r, n
		}
	}
	return best
}

// ScanTargets streams the live rows assigned to one root (by longest match),
// grouped by directory, invoking fn once per directory group. It deliberately
// does NOT filter on missing_seen_at — marked rows must stay in the input so the
// scan can clear them on reappearance. Reads go through the read handle so the
// writer is not blocked during iteration. An fn error aborts the walk.
func (s *sqlStore) ScanTargets(ctx context.Context, rigID, root string, allRoots []string, fn func(dir string, frames []ScanFrame) error) error {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, dest_path, missing_seen_at FROM frames
		 WHERE rig_id=? AND dest_path IS NOT NULL AND dest_path <> ''
		   AND state IN `+scannableStates+`
		 ORDER BY dest_path`, rigID)
	if err != nil {
		return fmt.Errorf("scan targets: %w", err)
	}
	defer rows.Close()

	var curDir string
	var group []ScanFrame
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		g := group
		group = nil
		return fn(curDir, g)
	}
	for rows.Next() {
		var id int64
		var dest string
		var msa sql.NullInt64
		if err := rows.Scan(&id, &dest, &msa); err != nil {
			return fmt.Errorf("scan targets row: %w", err)
		}
		// Assign to the longest configured root; a row under this root only.
		if longestRoot(dest, allRoots) != root {
			continue
		}
		dir := filepath.Dir(dest)
		if dir != curDir {
			if err := flush(); err != nil {
				return err
			}
			curDir = dir
		}
		group = append(group, ScanFrame{ID: id, DestPath: dest, Base: filepath.Base(dest), MissingSeenAt: intFromNull(msa)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan targets iterate: %w", err)
	}
	return flush()
}

// FiledSample is one filed frame's at-rest identity, used to confirm a root still
// holds this installation's own data before auto-linking it.
type FiledSample struct {
	DestPath string
	SHA256   string
	// RetargetSHA256 is the retarget audit record's hash ("" when not retargeted):
	// what the re-label produced. It is NOT what the auto-link verify compares
	// against — that is ArchiveSHA256 below, the live archive-side expectation
	// (design routing-and-filing §9.2, design frame-lifecycle §11.1).
	RetargetSHA256 string
	Size           int64
	// ArchiveSHA256 and ArchiveSize are what the copy at DestPath must hash to and
	// the length to hash it over — the archive-side pair the auto-link verify
	// actually compares against. They are carried explicitly for the same reason
	// the overlay hash above is: this SELECT does not use frameColumns (design
	// frame-lifecycle §11.1).
	ArchiveSHA256 string
	ArchiveSize   int64
}

// FiledSamplesUnderRoot returns up to limit filed frames whose NAS copy should be
// present and verifiable under root (assigned by longest match), preferring the
// states whose recorded hash matches the copy on disk. It lets the scan link a
// root that already holds frames — an existing/drained library that will never see
// a fresh filed frame to trigger the file-path auto-link.
func (s *sqlStore) FiledSamplesUnderRoot(ctx context.Context, rigID, root string, allRoots []string, limit int) ([]FiledSample, error) {
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT dest_path, sha256, retarget_sha256, size, archive_sha256, archive_size FROM frames
		 WHERE rig_id=? AND state IN ('verified','deleted','unclassified')
		   AND dest_path IS NOT NULL AND dest_path <> '' AND sha256 <> '' AND size > 0
		 ORDER BY id DESC`, rigID)
	if err != nil {
		return nil, fmt.Errorf("filed samples: %w", err)
	}
	defer rows.Close()
	var out []FiledSample
	for rows.Next() && len(out) < limit {
		var s FiledSample
		var retarget sql.NullString // nullable: "" when the frame was never retargeted
		var archiveSHA sql.NullString
		var archiveSize sql.NullInt64
		if err := rows.Scan(&s.DestPath, &s.SHA256, &retarget, &s.Size, &archiveSHA, &archiveSize); err != nil {
			return nil, fmt.Errorf("filed sample row: %w", err)
		}
		s.RetargetSHA256 = retarget.String
		s.ArchiveSHA256 = archiveSHA.String
		s.ArchiveSize = archiveSize.Int64
		if longestRoot(s.DestPath, allRoots) == root {
			out = append(out, s)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filed samples iterate: %w", err)
	}
	return out, nil
}

// ListMissingFrames pages every missing row for the UI panel, regardless of age.
// The panel must be a superset of the safe-to-power-off banner, which counts any
// marked-missing row with no time cutoff; a server-side age filter here made the
// banner open an empty panel. The missing_display_period declutter belongs
// client-side (design library-reconciliation §4.5/§4.7), not as a server-side
// row exclusion.
func (s *sqlStore) ListMissingFrames(ctx context.Context, rigID string, page, pageSize int) ([]model.Frame, int, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	if page < 0 {
		page = 0
	}
	var total int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frames WHERE rig_id=? AND missing_seen_at IS NOT NULL`, rigID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count missing frames: %w", err)
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames WHERE rig_id=? AND missing_seen_at IS NOT NULL
		 ORDER BY dest_path LIMIT ? OFFSET ?`, rigID, pageSize, page*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list missing frames: %w", err)
	}
	frames, err := scanFrames(rows)
	if err != nil {
		return nil, 0, err
	}
	return frames, total, nil
}

// MissingGroupCounts returns the true per-directory count of missing frames (not
// page-local), so the panel heading is accurate for a large cull. It counts every
// marked-missing row with no age cutoff, matching ListMissingFrames so the group
// tallies match the paged rows. The display-period declutter belongs client-side
// (design library-reconciliation §4.5/§4.7), not as a server-side row exclusion.
func (s *sqlStore) MissingGroupCounts(ctx context.Context, rigID string) ([]model.MissingGroup, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT dest_path FROM frames WHERE rig_id=? AND missing_seen_at IS NOT NULL ORDER BY dest_path`, rigID)
	if err != nil {
		return nil, fmt.Errorf("missing group counts: %w", err)
	}
	defer rows.Close()
	// Aggregate into a map keyed by directory rather than emitting a group on every
	// filepath.Dir change while streaming ORDER BY dest_path: the ordering is by full
	// dest_path, so same-dir rows can be non-adjacent (/a/b/m, /a/b/n/q, /a/b/z), and
	// the streaming approach emitted /a/b twice with partial counts (design
	// library-reconciliation §4.5).
	counts := map[string]int{}
	for rows.Next() {
		var dest string
		if err := rows.Scan(&dest); err != nil {
			return nil, fmt.Errorf("missing group row: %w", err)
		}
		counts[filepath.Dir(dest)]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("missing group iterate: %w", err)
	}
	// Non-nil so an empty result marshals to a JSON [] rather than null: the UI
	// reads groups.length directly, and a null there crashes the missing panel.
	out := make([]model.MissingGroup, 0, len(counts))
	for dir, n := range counts {
		out = append(out, model.MissingGroup{Dir: dir, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// SetFrameMissing marks a frame missing, idempotently: a re-observation never
// restarts a running settle clock.
//
// This is the enforcement point for design library-reconciliation §10.5 invariant
// that an acknowledgement never outlives its missing mark. Nulling missing_acked_at
// on the same statement means a fresh mark can never carry a stale acknowledgement
// from a prior missing episode — a pre-armed waiver that would fire the instant a
// healthy frame went missing again. The WHERE missing_seen_at IS NULL fires only on
// a FRESH mark, so a re-observation of a standing mark leaves a live acknowledgement
// intact, and clear-and-set in one statement leaves no crash window.
func (s *sqlStore) SetFrameMissing(ctx context.Context, id, at int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE frames SET missing_seen_at=?, missing_acked_at=NULL WHERE id=? AND missing_seen_at IS NULL`, at, id)
	if err != nil {
		return fmt.Errorf("set frame missing %d: %w", id, err)
	}
	return nil
}

// ClearFrameMissing clears a frame's missing mark (the file reappeared). It also
// nulls missing_acked_at as defence in depth for the design library-reconciliation
// §10.5 invariant: a returned file must not leave a live acknowledgement behind.
func (s *sqlStore) ClearFrameMissing(ctx context.Context, id int64) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET missing_seen_at=NULL, missing_acked_at=NULL WHERE id=?`, id); err != nil {
		return fmt.Errorf("clear frame missing %d: %w", id, err)
	}
	return nil
}

// ClearMissingUnderRoot clears the missing mark on every row (any rig) whose
// dest_path lies under root. Deliberately not scoped by rig: roots are global and
// the marker baseline is per root, so a relink must reset every rig's clocks under
// that root. Containment uses UnderRoot, matching the scan.
func (s *sqlStore) ClearMissingUnderRoot(ctx context.Context, root string) (int, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, dest_path FROM frames WHERE missing_seen_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("clear missing under root: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var dest string
		if err := rows.Scan(&id, &dest); err != nil {
			rows.Close()
			return 0, fmt.Errorf("clear missing row: %w", err)
		}
		if UnderRoot(dest, root) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("clear missing iterate: %w", err)
	}
	// missing_acked_at is nulled alongside missing_seen_at: this is the relink path
	// (the NAS came back), so a surviving acknowledgement would be a pre-armed waiver
	// on a healthy frame (design library-reconciliation §10.5).
	return s.clearByIDs(ctx, []string{"missing_seen_at", "missing_acked_at"}, ids)
}

// clearByIDs sets the named columns to NULL for a batch of ids (batches of 500 to
// stay under SQLite's bound-variable limit).
func (s *sqlStore) clearByIDs(ctx context.Context, columns []string, ids []int64) (int, error) {
	setClauses := make([]string, len(columns))
	for i, c := range columns {
		setClauses[i] = c + "=NULL"
	}
	setSQL := strings.Join(setClauses, ", ")
	total := 0
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			ph[i] = "?"
			args[i] = id
		}
		res, err := s.write.ExecContext(ctx,
			`UPDATE frames SET `+setSQL+` WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return total, fmt.Errorf("clear %s: %w", setSQL, err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// RequestRestore sets a restore request on an observed-missing row, returning
// whether it applied. deleted+missing rows are included: a both-sides cull's
// original may still exist on a reinserted card, and that is the case Restore is
// for.
func (s *sqlStore) RequestRestore(ctx context.Context, rigID string, frameID, at int64) (bool, error) {
	// Nulling missing_acked_at makes Restore beat Acknowledge: asking for a frame
	// back always clears any acknowledgement in the same statement (design
	// library-reconciliation §10.4).
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET restore_requested_at=?, missing_acked_at=NULL WHERE id=? AND rig_id=? AND missing_seen_at IS NOT NULL`,
		at, frameID, rigID)
	if err != nil {
		return false, fmt.Errorf("request restore %d: %w", frameID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkRestoreInProgress sets a restore request unconditionally (unlike
// RequestRestore, which requires the row to be missing). The truncated-copy
// replace path needs to mark intent on a present-but-short frame so a crash in the
// Remove-then-rename window is recoverable.
func (s *sqlStore) MarkRestoreInProgress(ctx context.Context, id, at int64) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET restore_requested_at=? WHERE id=?`, at, id); err != nil {
		return fmt.Errorf("mark restore in progress %d: %w", id, err)
	}
	return nil
}

// ClearRestoreRequest clears a restore request (cancel, or a completed restore).
func (s *sqlStore) ClearRestoreRequest(ctx context.Context, rigID string, id int64) error {
	// Scoped by rig_id (like RequestRestore) so a cancel routed through one rig's
	// endpoint cannot clear another rig's frame.
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET restore_requested_at=NULL WHERE id=? AND rig_id=?`, id, rigID); err != nil {
		return fmt.Errorf("clear restore request %d: %w", id, err)
	}
	return nil
}

// FramesAwaitingRestore returns the rows with a pending restore request for a rig.
func (s *sqlStore) FramesAwaitingRestore(ctx context.Context, rigID string) ([]model.Frame, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames WHERE rig_id=? AND restore_requested_at IS NOT NULL ORDER BY id`, rigID)
	if err != nil {
		return nil, fmt.Errorf("frames awaiting restore: %w", err)
	}
	return scanFrames(rows)
}

// CountOutsideRoots counts a rig's scannable rows whose dest_path lies under none
// of the configured roots — the invisible orphaned state a repointed root leaves.
func (s *sqlStore) CountOutsideRoots(ctx context.Context, rigID string, roots []string) (int, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT dest_path FROM frames WHERE rig_id=? AND dest_path IS NOT NULL AND dest_path <> ''
		 AND state IN `+scannableStates, rigID)
	if err != nil {
		return 0, fmt.Errorf("count outside roots: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var dest string
		if err := rows.Scan(&dest); err != nil {
			return 0, fmt.Errorf("count outside roots row: %w", err)
		}
		if longestRoot(dest, roots) == "" {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("count outside roots iterate: %w", err)
	}
	return n, nil
}

// MissingUnderDir returns the rig's missing rows whose filepath.Dir(dest_path)
// equals dir, with no age cutoff so the acknowledge set matches the unfiltered
// panel (ListMissingFrames). It backs the group-acknowledge challenge: the count,
// the skipped breakdown, and the id set bound to the token.
//
// dir is matched BYTE-EXACTLY against filepath.Dir(dest_path), the same way
// MissingGroupCounts forms its groups — never case-folded. Folding is correct for
// UnderRoot (a containment predicate on assumed-NTFS destinations) but wrong for a
// selection predicate on a delete-sooner control: on a case-sensitive destination it
// would silently merge /astro/M31 and /astro/m31 — two distinct panel chips — into
// one acknowledge target (design library-reconciliation §10.7).
func (s *sqlStore) MissingUnderDir(ctx context.Context, rigID, dir string) ([]model.Frame, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE rig_id=? AND missing_seen_at IS NOT NULL
		 ORDER BY dest_path`, rigID)
	if err != nil {
		return nil, fmt.Errorf("missing under dir: %w", err)
	}
	frames, err := scanFrames(rows)
	if err != nil {
		return nil, err
	}
	out := make([]model.Frame, 0, len(frames))
	for _, f := range frames {
		if filepath.Dir(f.DestPath) == dir {
			out = append(out, f)
		}
	}
	return out, nil
}

// AcknowledgeMissing waives the settle window for one frame's culled NAS copy by
// stamping missing_acked_at. The WHERE is the durable, authoritative guard (design
// library-reconciliation §10.3): only a proven-archived (verified or unclassified),
// marked-missing, no-restore, not-already-acked row qualifies — held/deleted are
// excluded. This is the PER-FRAME path; the group AcknowledgeMissingByIDs below
// stays verified-only so a single group-ack on the flat quarantine directory cannot
// drain the whole unclassified backlog (design library-reconciliation §11.3).
// missing_acked_at IS NULL keeps the column meaning "when the operator FIRST
// vouched"; a re-ack is handled as an idempotent no-op by the caller, not an error.
//
// Returns RowsAffected() > 0. false means the row changed between the caller's read
// and this write, so the acknowledgement did NOT apply — the caller must surface a
// refusal, never success.
func (s *sqlStore) AcknowledgeMissing(ctx context.Context, rigID string, frameID, at int64) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET missing_acked_at = ?
		 WHERE id = ? AND rig_id = ? AND (state = 'verified' OR state = 'unclassified')
		   AND missing_seen_at IS NOT NULL
		   AND restore_requested_at IS NULL
		   AND missing_acked_at IS NULL`,
		at, frameID, rigID)
	if err != nil {
		return false, fmt.Errorf("acknowledge missing %d: %w", frameID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AcknowledgeMissingByIDs is the GROUP acknowledge and stays state = 'verified' ONLY
// — deliberately narrower than the per-frame AcknowledgeMissing above. Every
// unclassified frame is filed to one flat quarantine directory, so a group-ack that
// accepted unclassified would drain the entire backlog across all targets and nights
// in one click; keeping this verified-only is what confines the quarantine reclaim to
// the one-at-a-time per-frame path (do NOT widen this to unclassified). It is scoped to
// the rig, in batches of 500 (mirroring clearByIDs), and returns the number of rows
// actually acknowledged — which is what the group confirm reports, so the count stays
// truthful even if some rows were restored or acknowledged in the meantime (design
// library-reconciliation §10.8).
func (s *sqlStore) AcknowledgeMissingByIDs(ctx context.Context, rigID string, ids []int64, at int64) (int, error) {
	total := 0
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+2)
		args = append(args, at, rigID)
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		res, err := s.write.ExecContext(ctx,
			`UPDATE frames SET missing_acked_at = ?
			 WHERE rig_id = ? AND state = 'verified'
			   AND missing_seen_at IS NOT NULL
			   AND restore_requested_at IS NULL
			   AND missing_acked_at IS NULL
			   AND id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return total, fmt.Errorf("acknowledge missing by ids: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// ResolveCulledFrame retires a row whose device original was deleted because its NAS
// copy was culled on purpose (design library-reconciliation §10.6). It nulls
// dest_path alongside the reconciliation columns: an empty dest_path takes the row
// out of ScanTargets' `dest_path <> ”` predicate, so the next library scan cannot
// re-derive the missing mark from the (now permanently absent) filesystem copy and
// raise a false library_reconciled alert. This is the same mechanism DismissFrame
// uses; do NOT "simplify" it back to ClearFrameMissing, which would leave a valid
// dest_path the scan re-marks.
func (s *sqlStore) ResolveCulledFrame(ctx context.Context, id int64) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET missing_seen_at=NULL, missing_acked_at=NULL,
		                   restore_requested_at=NULL, dest_path=NULL
		 WHERE id=?`, id); err != nil {
		return fmt.Errorf("resolve culled frame %d: %w", id, err)
	}
	return nil
}
