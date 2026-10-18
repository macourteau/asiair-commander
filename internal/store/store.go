// Package store is the SQLite operational-state store (design architecture §7).
// It holds the reconstructible operational history — frames, night counters,
// alerts, and log pulls — deliberately kept apart from the precious-and-small
// config so a corrupt state DB can never cost the user their setup.
//
// Write serialization (design architecture §7): a single WRITE handle with
// SetMaxOpenConns(1) under WAL queues every writer onto one connection, so
// SQLITE_BUSY between our own writers is structurally impossible; a separate
// READ pool serves history/stats concurrently. The single-writer property is
// also load-bearing for night-number assignment (see AssignNight).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/panel"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// LogPull is one row of the log_pulls table (design architecture §7). It is
// store-local: device logs grow through a session, so a log is re-pulled only
// when its Size changed since the last pull (GetLogPull feeds that decision) and
// the NAS copy is overwritten.
type LogPull struct {
	RigID      string
	LogType    string
	SourcePath string
	DestPath   string
	Size       int64
	PulledAt   int64 // unix seconds
}

// NightEntry is one (target, ota, observing_date) → night mapping used by
// RebuildNightCounters to restore the counter table from existing NIGHT_n folders
// at startup (design routing-and-filing §6 startup reconciliation).
type NightEntry struct {
	Target        string
	OTA           string
	ObservingDate string
	Night         int
}

// residentStates are the frame states meaning a copy is still on the device, so
// the frame still occupies capacity (design architecture §10 counts).
var residentStates = []model.FrameState{
	model.StateVerified, model.StateUnclassified, model.StateHeld, model.StateFailed,
}

// Store is the operational-state persistence seam consumed by the rig worker, web
// layer, and discovery. The concrete implementation is *sqlStore.
type Store interface {
	// Frames.
	UpsertFrame(ctx context.Context, f *model.Frame) error
	GetFrame(ctx context.Context, rigID, share, path string) (*model.Frame, bool, error)
	GetFrameByID(ctx context.Context, id int64) (*model.Frame, bool, error)
	ListFrames(ctx context.Context, rigID, state, filter string, page, pageSize int) (frames []model.Frame, total int, err error)
	ListTransferHistory(ctx context.Context, rigID string, from, to int64, page, pageSize int) (frames []model.Frame, total int, err error)
	ListNeverDeletable(ctx context.Context, rigID string, page, pageSize int) (frames []model.Frame, total int, err error)
	RecentFrames(ctx context.Context, rigID string, limit int) ([]model.Frame, error)
	DeletionEligible(ctx context.Context, rigID string, graceCutoff int64) ([]model.Frame, error)
	UnclassifiedFrames(ctx context.Context, rigID string) ([]model.Frame, error)
	FailedDueForRetry(ctx context.Context, rigID string, now int64) ([]model.Frame, error)
	UpdateFrame(ctx context.Context, f *model.Frame) error
	SetState(ctx context.Context, id int64, state model.FrameState) error
	// DeferDelete spends one of the pre-delete gate's deferrals on a frame whose
	// source no longer agrees with its row (design frame-lifecycle §11.13).
	DeferDelete(ctx context.Context, id int64) error
	// Astrometry writers/queue (Tier 0 backfill).
	SetFrameAstrometry(ctx context.Context, id int64, a *model.Astrometry) error
	SetFrameAstrometryIfPending(ctx context.Context, id int64, a *model.Astrometry) error
	ListFramesNeedingAstrometry(ctx context.Context, afterID int64, limit int) ([]model.Frame, error)
	// data_sha256 backfill (design frame-lifecycle §11.8). The cursor pages rows
	// that carry no exposure identity and records one only from a copy proven to
	// be the archived bytes; both methods advance data_sha256 and nothing else.
	ListFramesNeedingDataDigest(ctx context.Context, afterID int64, held bool, limit int) ([]model.Frame, error)
	SetFrameDataDigestIfPending(ctx context.Context, id int64, wantSHA256, wantArchiveSHA256, dataSHA256 string) (bool, error)
	// Tier-1 solve queue/claim/reclaim (design features §3.5). The queue is the
	// frame row itself: wcs_status='none' AND frame_type='LIGHT'. Claims and
	// terminal writes are guarded compare-and-sets so a concurrent re-pull can
	// never be clobbered.
	ListFramesNeedingSolve(ctx context.Context, afterID int64, limit int) ([]model.Frame, error)
	ClaimFrameForSolve(ctx context.Context, id int64, a *model.Astrometry) (bool, error)
	SetFrameSolveResult(ctx context.Context, id int64, a *model.Astrometry) (bool, error)
	ReclaimSolvingFrames(ctx context.Context) (int, error)
	ClearFrameAstrometry(ctx context.Context, id int64) error
	RequeueFloorUnsolvable(ctx context.Context, floor float64) (int, error)
	RequeueSolveFailures(ctx context.Context) (int, error)
	// DismissFrame tombstones a frame: state=dismissed, and pulled_at/dest_path
	// cleared so it drops out of the pulled_at-keyed transfer-history and
	// "transferred tonight" queries. size/mtime are retained for decide()'s
	// change-detection.
	DismissFrame(ctx context.Context, id int64) error
	// CountLiveFramesByDest counts frames on a rig that reference dest_path, excluding
	// one id. Used to avoid purging a NAS copy shared by another row. No state filter:
	// dismissed rows have dest_path=NULL and never match, so any row that DOES match
	// owns the NAS file — including a `deleted` row, whose dest_path is its permanent
	// NAS archive (markDeleted removes only the device source). Excluding `deleted`
	// here would let a dismiss purge an archived science frame.
	CountLiveFramesByDest(ctx context.Context, rigID, destPath string, excludeID int64) (int, error)
	CountFramesTonight(ctx context.Context, rigID string, sinceUnix int64) (int, error)

	// Out-of-band deletion reconciliation.
	ScanTargets(ctx context.Context, rigID, root string, allRoots []string, fn func(dir string, frames []ScanFrame) error) error
	FiledSamplesUnderRoot(ctx context.Context, rigID, root string, allRoots []string, limit int) ([]FiledSample, error)
	ListMissingFrames(ctx context.Context, rigID string, page, pageSize int) ([]model.Frame, int, error)
	MissingGroupCounts(ctx context.Context, rigID string) ([]model.MissingGroup, error)
	SetFrameMissing(ctx context.Context, id, at int64) error
	ClearFrameMissing(ctx context.Context, id int64) error
	ClearMissingUnderRoot(ctx context.Context, root string) (int, error)
	// MissingUnderDir returns the rig's missing rows whose filepath.Dir(dest_path)
	// equals dir (byte-exact), with no age cutoff so the acknowledge set matches the
	// unfiltered panel (ListMissingFrames). Backs the group-acknowledge challenge
	// (design library-reconciliation §10.7).
	MissingUnderDir(ctx context.Context, rigID, dir string) ([]model.Frame, error)
	// AcknowledgeMissing waives the settle window for one frame's culled NAS copy.
	// Guarded to verified + marked-missing + no-restore + not-already-acked rows;
	// returns false when the row changed under the read (design
	// library-reconciliation §10.3).
	AcknowledgeMissing(ctx context.Context, rigID string, frameID, at int64) (bool, error)
	// AcknowledgeMissingByIDs applies the same guard to a bound id set, scoped to
	// the rig, and returns the number of rows actually acknowledged (design
	// library-reconciliation §10.8).
	AcknowledgeMissingByIDs(ctx context.Context, rigID string, ids []int64, at int64) (int, error)
	// ResolveCulledFrame retires a row whose device original was deleted because its
	// NAS copy was culled: it nulls dest_path alongside the reconciliation columns so
	// the scan cannot re-derive the mark (design library-reconciliation §10.6).
	ResolveCulledFrame(ctx context.Context, id int64) error
	RequestRestore(ctx context.Context, rigID string, frameID, at int64) (bool, error)
	MarkRestoreInProgress(ctx context.Context, id, at int64) error
	ClearRestoreRequest(ctx context.Context, rigID string, id int64) error
	FramesAwaitingRestore(ctx context.Context, rigID string) ([]model.Frame, error)
	CountOutsideRoots(ctx context.Context, rigID string, roots []string) (int, error)
	BytesPulledSince(ctx context.Context, since int64) (int64, error)
	ResidentCount(ctx context.Context, rigID string) (int, error)
	// ResidentLeads returns, distinct, the lead (first two segments of a deeper
	// path, else the first) of every resident row's source path on share.
	ResidentLeads(ctx context.Context, rigID, share string) ([]string, error)

	// Re-file (design routing-and-filing §8.6). A re-file writes dest_path and
	// nothing else, moving every row that shares the path in one transaction, and
	// announces the move in refile_intents so a crash between rename and commit is
	// recoverable.
	RefileCandidates(ctx context.Context, rigID string) ([]model.Frame, error)
	FramesByDestPath(ctx context.Context, rigID, dest string) ([]model.Frame, error)
	CountFramesByDestAnyRig(ctx context.Context, destPath string) (int, error)
	// CountUndeletedFramesByDestAnyRig is the same count with `deleted` rows left
	// out, for the H13 paths that ask which rows still have a claim on the archive
	// file's CONTENT rather than which rows name it at all. A tombstone has no such
	// claim: its device original is gone and its row is a terminal record. The
	// re-file path keeps the unfiltered count, whose question is the other one.
	CountUndeletedFramesByDestAnyRig(ctx context.Context, destPath string) (int, error)
	MoveDestPath(ctx context.Context, rigID, oldDest, newDest string, policy MoveDestPolicy) (int, error)
	PutRefileIntent(ctx context.Context, in RefileIntent) error
	ClearRefileIntent(ctx context.Context, rigID, fromPath string) error
	ListRefileIntents(ctx context.Context) ([]RefileIntent, error)
	OpenIntentPaths(ctx context.Context, rigID string) (map[string]bool, error)

	// Supersede (design frame-lifecycle §11.9). A supersede replaces the bytes
	// under one archive name through a four-step claim, announcing it in
	// supersede_intents so a crash anywhere in that sequence is recoverable, and
	// commits the anchors the claim carries — the revised row's and, per the
	// sharer invariant, every row sharing the path.
	PutSupersedeIntent(ctx context.Context, in SupersedeIntent) error
	ClearSupersedeIntent(ctx context.Context, rigID, destPath string) error
	ListSupersedeIntents(ctx context.Context) ([]SupersedeIntent, error)
	OpenSupersedePaths(ctx context.Context, rigID string) (map[string]bool, error)
	CommitSupersede(ctx context.Context, in SupersedeIntent) (revised bool, sharers int, err error)
	// CommitRestore is the same two-row write for a completed restore, whose
	// success path wrote no frame column at all (design frame-lifecycle §11.6).
	CommitRestore(ctx context.Context, in ArchiveWrite) (read bool, sharers int, err error)

	// CommitCulledRevision is the targeted write of design frame-lifecycle §11.12:
	// a revision of a frame whose archive copy the operator culled files nothing,
	// so the row's device anchors advance on their own while the three
	// reconciliation marks stand.
	CommitCulledRevision(ctx context.Context, in CulledRevision) (bool, error)

	// Night counters (design routing-and-filing §6), and the destination-folder
	// reads that supply their hints (design routing-and-filing §6.4/§6.2).
	AssignNight(ctx context.Context, req NightRequest) (int, error)
	CanonicalTarget(ctx context.Context, target string) (string, error)
	LookupNight(ctx context.Context, target, ota, observingDate string) (int, bool, error)
	MaxNight(ctx context.Context, target, ota string) (int, error)
	NightHeldByOtherDate(ctx context.Context, target, ota string, night int, observingDate string) (bool, error)
	NightsUnder(ctx context.Context, lo, hi string) ([]int, error)
	NightFolderCounts(ctx context.Context, lo, hi, dateLo, dateHi string) (total, matching int, err error)
	DistinctTargetsUnder(ctx context.Context, lo, hi string) ([]string, error)
	RebuildNightCounters(ctx context.Context, entries []NightEntry) error

	// Alerts (design architecture §10).
	RaiseAlert(ctx context.Context, a model.Alert) (id int64, created bool, err error)
	ResolveAlerts(ctx context.Context, rigID string, category model.Category, messagePrefix string, at int64) (int, error)
	ListAlerts(ctx context.Context, includeAcked bool, limit int) ([]model.Alert, error)
	AckAlert(ctx context.Context, id int64, at int64) error
	UnackedCount(ctx context.Context) (int, error)

	// Log pulls (design architecture §7).
	RecordLogPull(ctx context.Context, l LogPull) error
	GetLogPull(ctx context.Context, rigID, sourcePath string) (*LogPull, bool, error)
	ListLogPulls(ctx context.Context, rigID string) ([]LogPull, error)

	// Retention (design architecture §7) — bounded batches so a big first prune
	// can't hold the write conn in one long transaction.
	PruneDeleted(ctx context.Context, olderThan int64, batch int) (int, error)
	PruneAckedAlerts(ctx context.Context, olderThan int64, batch int) (int, error)

	// Meta is a small key/value store for operational flags that must survive a
	// restart (e.g. the global pause state).
	GetMeta(ctx context.Context, key string) (string, bool, error)
	SetMeta(ctx context.Context, key, value string) error
	EnsureMeta(ctx context.Context, key, candidate string) (string, error)

	// Maintenance.
	Checkpoint(ctx context.Context) error
	StartCheckpointLoop(ctx context.Context, interval time.Duration)
	Close() error
}

// sqlStore holds the two handles on the same db file (design architecture §7
// "Write serialization"): write is SetMaxOpenConns(1) so writers serialize; read
// is a pool for history/stats under WAL.
type sqlStore struct {
	write *sql.DB
	read  *sql.DB
}

var _ Store = (*sqlStore)(nil)

// Open opens (creating if absent) the state DB at path and runs migrations before
// returning. It refuses to start if the stored schema_version is newer than this
// binary understands (design architecture §7). It returns the Store interface
// value (not a pointer-to-interface) so consumers call methods directly.
func Open(path string) (Store, error) {
	// The write handle carries the durability/concurrency pragmas; foreign_keys is
	// ON for normal operation (migrations that rebuild tables toggle it per design
	// architecture §7).
	writeDSN := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	write, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("open write handle: %w", err)
	}
	// database/sql queues writes onto the single connection, so SQLITE_BUSY between
	// our own writers is structurally impossible.
	write.SetMaxOpenConns(1)

	readDSN := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	read, err := sql.Open("sqlite", readDSN)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read handle: %w", err)
	}

	s := &sqlStore{write: write, read: read}
	if err := s.migrate(context.Background()); err != nil {
		write.Close()
		read.Close()
		return nil, err
	}

	return s, nil
}

// migration is one forward step in the ordered registry. Each runs with its
// version bump in a single transaction (design architecture §7), so a crash
// mid-migration rolls back and re-runs cleanly.
type migration struct {
	version int
	up      func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the ordered forward-migration registry. Adding a version is a
// one-liner: append a {version: N, up: ...} entry and bump
// model.CurrentStoreSchemaVersion.
var migrations = []migration{
	{version: 1, up: migrate1},
	{version: 2, up: migrate2},
	{version: 3, up: migrate3},
	{version: 4, up: migrate4},
	{version: 5, up: migrate5},
	{version: 6, up: migrate6},
	{version: 7, up: migrate7},
	{version: 8, up: migrate8},
	{version: 9, up: migrate9},
	{version: 10, up: migrate10},
	{version: 11, up: migrate11},
}

// migrate1 creates the initial schema exactly as the design architecture §7 DDL
// specifies.
func migrate1(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
CREATE TABLE frames (
  id INTEGER PRIMARY KEY,
  rig_id TEXT NOT NULL, source_share TEXT NOT NULL, source_path TEXT NOT NULL,
  sha256 TEXT, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
  frame_type TEXT, target TEXT, filter TEXT, exptime REAL, set_temp REAL,
  gain INTEGER, "offset" INTEGER, binning TEXT, sensor TEXT, date_obs TEXT, night INTEGER,
  dest_path TEXT, sidecars TEXT,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, next_retry_at INTEGER,
  pulled_at INTEGER, deleted_at INTEGER, error TEXT,
  UNIQUE(rig_id, source_share, source_path)
);
CREATE INDEX idx_frames_rig_state ON frames(rig_id, state);
CREATE TABLE night_counters (
  target TEXT NOT NULL, ota TEXT NOT NULL, observing_date TEXT NOT NULL, night INTEGER NOT NULL,
  UNIQUE(target, ota, observing_date)
);
CREATE TABLE alerts (
  id INTEGER PRIMARY KEY, rig_id TEXT, severity TEXT NOT NULL, category TEXT NOT NULL,
  message TEXT NOT NULL, created_at INTEGER NOT NULL, acked_at INTEGER
);
CREATE TABLE log_pulls (
  id INTEGER PRIMARY KEY, rig_id TEXT NOT NULL, log_type TEXT NOT NULL,
  source_path TEXT NOT NULL, dest_path TEXT, size INTEGER, pulled_at INTEGER
);
`
	// The meta table (design architecture §7 DDL) is created by migrate() before the
	// version read, so it is intentionally omitted here to avoid a double-create.
	_, err := tx.ExecContext(ctx, ddl)
	return err
}

// migrate2 adds the per-frame astrometry columns: wcs_status (the lifecycle
// discriminator both work-queues index on) and astrometry (a JSON blob mirroring
// the sidecars precedent). The partial index keeps the backfill cursor scan cheap.
func migrate2(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
ALTER TABLE frames ADD COLUMN wcs_status TEXT;
ALTER TABLE frames ADD COLUMN astrometry TEXT;
CREATE INDEX idx_frames_need_astro ON frames(id) WHERE wcs_status IS NULL;
`
	_, err := tx.ExecContext(ctx, ddl)
	return err
}

// migrate3 adds the partial index the Tier-1 solve queue scans: the backfill's
// idx_frames_need_astro is WHERE wcs_status IS NULL and does not serve the
// wcs_status='none' predicate ListFramesNeedingSolve walks.
func migrate3(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`CREATE INDEX idx_frames_need_solve ON frames(id) WHERE wcs_status='none';`)
	return err
}

// migrate4 adds the out-of-band-deletion reconciliation columns: missing_seen_at
// (the settle-clock anchor a scan sets when it confirms a NAS copy absent) and
// restore_requested_at (an operator's re-pull request that holds the device
// original). The partial index keeps the missing-panel and scan queries cheap;
// both columns are nullable and default NULL for every existing row.
func migrate4(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
ALTER TABLE frames ADD COLUMN missing_seen_at INTEGER;
ALTER TABLE frames ADD COLUMN restore_requested_at INTEGER;
CREATE INDEX idx_frames_rig_missing ON frames(rig_id) WHERE missing_seen_at IS NOT NULL;
`
	_, err := tx.ExecContext(ctx, ddl)
	return err
}

// migrate5 re-stamps every standing missing mark to now, on the upgrade that arms
// device-original deletion (design library-reconciliation §9.2). A store below
// schema 5 records marks it cannot act on, for as long as it runs; without this
// the first delete pass after the upgrade would find every one of them already
// older than the settle window and authorize the whole backlog at once — under a
// build that CAN delete, with no notice. Re-stamping gives every install a fresh,
// full settle window under that build, which is the point of the window.
func migrate5(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE frames SET missing_seen_at = CAST(strftime('%s','now') AS INTEGER)
		 WHERE missing_seen_at IS NOT NULL`)
	return err
}

// migrate6 adds missing_acked_at: the unix second at which the operator confirmed
// a frame's NAS copy was culled on purpose, waiving the reconcile-settle window for
// that frame and nothing else (design library-reconciliation §10.5/§9.2).
// Nullable, no backfill, no index change — idx_frames_rig_missing still serves
// the candidate queries.
//
// Future-migration note (design library-reconciliation §10.5): migrate5 re-stamps
// missing_seen_at wholesale to grant every install a fresh settle window on the
// release that armed deletion. A migrate5-shaped migration shipped AFTER this
// feature must ALSO null missing_acked_at, or it would re-stamp acknowledged rows
// and leave them acknowledged — silently skipping the very window the migration
// exists to grant, on the highest-risk rows.
func migrate6(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`ALTER TABLE frames ADD COLUMN missing_acked_at INTEGER;`)
	return err
}

// migrate7 adds the retarget overlay columns (design routing-and-filing §9.2):
// retarget_sha256 (the true content hash of a retargeted archive file — non-NULL
// means the frame was retargeted, and archive-side checks use it in place of
// sha256, which stays the immutable device-correlation anchor) and retarget_from
// (the original target string retained for UI/audit). Both nullable, no
// backfill, and deliberately no index — the hot delete path reads them off the
// already-fetched frame row.
func migrate7(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
ALTER TABLE frames ADD COLUMN retarget_sha256 TEXT;
ALTER TABLE frames ADD COLUMN retarget_from TEXT;
`
	_, err := tx.ExecContext(ctx, ddl)
	return err
}

// migrate8 adds the mosaic pane columns, the re-file intent table and the
// dest_path index (design routing-and-filing §7.4).
//
// The pane columns are derived from the raw stored target, so every existing row
// is backfilled by parsing it here — in Go, not in SQL. panel.ParsePanel is a
// regex plus a plausibility bound that SQLite's string functions cannot express
// faithfully, and an approximate SQL parser would be a second parser free to
// diverge from the one routing uses. A target with no pane suffix needs no write
// at all: the columns default to NULL, which scanFrame maps to the same ""/0 the
// filing path binds through nullStr/nullInt, so both paths agree column-for-column.
//
// refile_intents is keyed (rig_id, from_path) rather than on a frame id because
// one move covers every row sharing the vacated dest_path (the H12 two-share
// case). idx_frames_dest_path exists because MoveDestPath and the sharer checks
// both match on dest_path, which had no index of its own.
func migrate8(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
ALTER TABLE frames ADD COLUMN panel_base TEXT;
ALTER TABLE frames ADD COLUMN panel_row INTEGER;
ALTER TABLE frames ADD COLUMN panel_col INTEGER;
CREATE INDEX idx_frames_dest_path ON frames(dest_path);
CREATE TABLE refile_intents (
  rig_id     TEXT NOT NULL,
  from_path  TEXT NOT NULL,
  to_path    TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (rig_id, from_path)
);
`
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}

	type pane struct {
		id       int64
		base     string
		row, col int
	}
	// An empty or NULL target can never carry a pane suffix, so it is filtered out
	// rather than buffered.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, target FROM frames WHERE target IS NOT NULL AND target <> ''`)
	if err != nil {
		return fmt.Errorf("read targets for pane backfill: %w", err)
	}
	defer rows.Close()
	var panes []pane
	for rows.Next() {
		var (
			id     int64
			target string
		)
		if err := rows.Scan(&id, &target); err != nil {
			return fmt.Errorf("scan target for pane backfill: %w", err)
		}
		if base, _, r, c, ok := panel.ParsePanel(target); ok {
			panes = append(panes, pane{id: id, base: base, row: r, col: c})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read targets for pane backfill: %w", err)
	}
	// The cursor MUST be closed before the updates below: the write handle is pinned
	// to a single connection (SetMaxOpenConns(1)) and this transaction owns it, so an
	// open cursor and a write on the same tx would deadlock. Every row is therefore
	// buffered above rather than updated as it is read.
	rows.Close()

	if len(panes) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE frames SET panel_base=?, panel_row=?, panel_col=? WHERE id=?`)
	if err != nil {
		return fmt.Errorf("prepare pane backfill: %w", err)
	}
	defer stmt.Close()
	for _, p := range panes {
		if _, err := stmt.ExecContext(ctx, p.base, p.row, p.col, p.id); err != nil {
			return fmt.Errorf("backfill panes for frame %d: %w", p.id, err)
		}
	}
	return nil
}

// migrate9 splits the columns that describe a copy by WHICH copy they describe
// (design frame-lifecycle §11.1): archive_sha256 and
// archive_size describe the NAS copy, sha256/size/mtime the device copy, and
// data_sha256 the exposure itself, side-independent. delete_defer_count is the
// row-persisted bound of the pre-delete gate's deferral, zero for every existing
// row because no pass has deferred one yet.
//
// The two archive columns are seeded to the answer the code already gives —
// retarget_sha256 where an overlay exists, sha256 otherwise, and size for the
// length — so the seeding is definitionally a no-op while the two copies are still
// byte-for-byte images of one another, and stays one until a writer sets a
// differing value. A row that never recorded a copy (sha256 NULL/”) seeds to the
// same nothing the old fallback returned for it. No index: the archive columns are
// read off an already-fetched frame row, exactly as the overlay pair is.
func migrate9(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
ALTER TABLE frames ADD COLUMN data_sha256 TEXT;
ALTER TABLE frames ADD COLUMN archive_sha256 TEXT;
ALTER TABLE frames ADD COLUMN archive_size INTEGER;
ALTER TABLE frames ADD COLUMN delete_defer_count INTEGER NOT NULL DEFAULT 0;
UPDATE frames SET
  archive_sha256 = CASE WHEN retarget_sha256 IS NOT NULL AND retarget_sha256 <> ''
                        THEN retarget_sha256 ELSE sha256 END,
  archive_size = size;
`
	_, err := tx.ExecContext(ctx, ddl)
	return err
}

// migrate10 creates the supersede intent table: one row per archive copy whose
// four-step name claim has begun and is not yet confirmed complete (design
// frame-lifecycle §11.9).
//
// It is keyed (rig_id, dest_path) for the same reason refile_intents is keyed on
// the vacated path: one swap covers every row sharing the archive file, and the
// delete pass reads the open set by path (OpenSupersedePaths → absentDeferReasons).
//
// It carries the ANCHORS to commit, not just the paths. Recovery runs before any
// worker or device session exists, so it cannot re-read the device — and mtime is
// the DEVICE file's modification time, which no amount of looking at the archive
// copy in hand can recover. An intent naming only paths could not finish the job
// it started.
func migrate10(ctx context.Context, tx *sql.Tx) error {
	const ddl = `
CREATE TABLE supersede_intents (
  rig_id          TEXT NOT NULL,
  dest_path       TEXT NOT NULL,
  aside_path      TEXT NOT NULL,
  frame_id        INTEGER NOT NULL,
  sha256          TEXT NOT NULL,
  size            INTEGER NOT NULL,
  mtime           INTEGER NOT NULL,
  archive_sha256  TEXT NOT NULL,
  archive_size    INTEGER NOT NULL,
  retarget_sha256 TEXT NOT NULL,
  state           TEXT NOT NULL,
  pulled_at       INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  PRIMARY KEY (rig_id, dest_path)
);
`
	_, err := tx.ExecContext(ctx, ddl)
	return err
}

// migrate11 adds alerts.resolved_at, the time a standing condition's owner saw it
// clear (design architecture §10). It is separate from acked_at because an
// operator's acknowledgement silences a condition while it persists, and only a
// resolve lets the same condition raise anew. The old schema recorded both as
// acked_at, so every row acknowledged before this version is read as resolved: a
// condition the operator acknowledged and that still holds raises once more after
// the upgrade, which is louder than needed but never silences a returning one.
func migrate11(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`ALTER TABLE alerts ADD COLUMN resolved_at INTEGER`,
		`UPDATE alerts SET resolved_at = acked_at WHERE acked_at IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// migrate reads (or initializes) meta.schema_version and runs ordered forward
// migrations. It refuses to run if the stored version is newer than this binary
// understands.
func (s *sqlStore) migrate(ctx context.Context) error {
	// The meta table may not exist yet on a brand-new DB; create it first so the
	// version read is always well-defined. This is idempotent.
	if _, err := s.write.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		return fmt.Errorf("ensure meta table: %w", err)
	}

	current, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if current > model.CurrentStoreSchemaVersion {
		return fmt.Errorf("state DB schema_version %d is newer than this binary understands (%d): DB written by a newer version; refusing to start",
			current, model.CurrentStoreSchemaVersion)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.runMigration(ctx, m); err != nil {
			return fmt.Errorf("migration to v%d: %w", m.version, err)
		}
	}
	return nil
}

// runMigration applies one migration and its version bump in a single
// transaction on the write handle.
func (s *sqlStore) runMigration(ctx context.Context, m migration) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := m.up(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		fmt.Sprint(m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// schemaVersion reads meta.schema_version, returning 0 for a fresh DB.
func (s *sqlStore) schemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.read.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	return v, nil
}

// Close closes both handles.
func (s *sqlStore) Close() error {
	return errors.Join(s.write.Close(), s.read.Close())
}

// Checkpoint runs wal_checkpoint(TRUNCATE) to keep the -wal file bounded
// (design architecture §7).
func (s *sqlStore) Checkpoint(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		return fmt.Errorf("wal_checkpoint: %w", err)
	}
	return nil
}

// StartCheckpointLoop periodically checkpoints until ctx is cancelled. It is an
// optional helper; the caller decides the cadence.
func (s *sqlStore) StartCheckpointLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A failed checkpoint is non-fatal — WAL simply grows until the next tick.
			_ = s.Checkpoint(ctx)
		}
	}
}

// --- nullable-column mapping ---------------------------------------------------
//
// The Frame struct uses a 0 sentinel for the nullable columns (Night, PulledAt,
// DeletedAt, NextRetryAt); SQL stores them as NULL. These helpers convert both
// directions so a not-set value round-trips as NULL rather than a literal 0.

// setTempArg binds set_temp: NULL for a frame that carried no SET-TEMP card, so
// a re-file can tell an uncooled frame from one cooled to exactly 0C.
func setTempArg(f *model.Frame) any {
	if f.SetTempAbsent {
		return nil
	}
	return f.SetTemp
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func intFromNull(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}

// --- frames --------------------------------------------------------------------

// frameColumns is the shared SELECT column list, kept in one place so scanFrame
// stays in lockstep with every query.
const frameColumns = `id, rig_id, source_share, source_path, sha256, size, mtime,
	frame_type, target, filter, exptime, set_temp, gain, "offset", binning, sensor,
	date_obs, night, dest_path, sidecars, state, attempts, next_retry_at, pulled_at,
	deleted_at, error, wcs_status, astrometry, missing_seen_at, restore_requested_at,
	missing_acked_at, retarget_sha256, retarget_from, panel_base, panel_row, panel_col,
	data_sha256, archive_sha256, archive_size, delete_defer_count`

// scanFrame reads one frames row (in frameColumns order) into a model.Frame,
// unmarshalling sidecars and mapping NULLs back to the 0 sentinels.
func scanFrame(sc interface{ Scan(...any) error }) (model.Frame, error) {
	var f model.Frame
	var (
		sha256, frameType, target, filter, binning, sensor, dateObs, destPath, sidecars, errStr sql.NullString
		wcsStatus, astrometry                                                                   sql.NullString
		retargetSHA256, retargetFrom, panelBase                                                 sql.NullString
		dataSHA256, archiveSHA256                                                               sql.NullString
		exptime, setTemp                                                                        sql.NullFloat64
		gain, offset, night, nextRetryAt, pulledAt, deletedAt                                   sql.NullInt64
		missingSeenAt, restoreRequestedAt, missingAckedAt                                       sql.NullInt64
		panelRow, panelCol, archiveSize, deleteDeferCount                                       sql.NullInt64
		state                                                                                   string
	)
	// deleteDeferCount is scanned LAST, matching frameColumns' trailing column. The
	// column list and these Scan args are positional with nothing enforcing the
	// match, so both must stay appended in lockstep (design library-reconciliation
	// §4.1, design routing-and-filing §9.2).
	if err := sc.Scan(&f.ID, &f.RigID, &f.SourceShare, &f.SourcePath, &sha256, &f.Size, &f.Mtime,
		&frameType, &target, &filter, &exptime, &setTemp, &gain, &offset, &binning, &sensor,
		&dateObs, &night, &destPath, &sidecars, &state, &f.Attempts, &nextRetryAt, &pulledAt,
		&deletedAt, &errStr, &wcsStatus, &astrometry, &missingSeenAt, &restoreRequestedAt,
		&missingAckedAt, &retargetSHA256, &retargetFrom, &panelBase, &panelRow, &panelCol,
		&dataSHA256, &archiveSHA256, &archiveSize, &deleteDeferCount); err != nil {
		return model.Frame{}, err
	}
	// The copy-keyed columns (design frame-lifecycle §11.1): NULL maps to the same
	// ""/0 "not recorded" sentinel the filing path binds through nullStr/nullInt, so
	// a migrated row and a freshly-written one read back identically.
	f.DataSHA256 = dataSHA256.String
	f.ArchiveSHA256 = archiveSHA256.String
	f.ArchiveSize = intFromNull(archiveSize)
	f.DeleteDeferCount = int(intFromNull(deleteDeferCount))
	// NULL is the "no pane suffix" encoding, mapping to the same ""/0 the filing
	// path binds for a target that does not parse (design routing-and-filing §7.4).
	f.PanelBase = panelBase.String
	f.PanelRow = int(intFromNull(panelRow))
	f.PanelCol = int(intFromNull(panelCol))
	f.MissingSeenAt = intFromNull(missingSeenAt)
	f.RestoreRequestedAt = intFromNull(restoreRequestedAt)
	f.MissingAckedAt = intFromNull(missingAckedAt)
	// NULL and '' both map to "" — the sentinel archiveHash and the re-apply rule test.
	f.RetargetSHA256 = retargetSHA256.String
	f.RetargetFrom = retargetFrom.String
	f.SHA256 = sha256.String
	f.FrameType = frameType.String
	f.Target = target.String
	f.Filter = filter.String
	f.Exptime = exptime.Float64
	f.SetTemp = setTemp.Float64
	f.SetTempAbsent = !setTemp.Valid
	f.Gain = gain.Int64
	f.Offset = offset.Int64
	f.Binning = binning.String
	f.Sensor = sensor.String
	f.DateObs = dateObs.String
	f.Night = intFromNull(night)
	f.DestPath = destPath.String
	f.State = model.FrameState(state)
	f.NextRetryAt = intFromNull(nextRetryAt)
	f.PulledAt = intFromNull(pulledAt)
	f.DeletedAt = intFromNull(deletedAt)
	f.Error = errStr.String
	if sidecars.Valid && sidecars.String != "" {
		if err := json.Unmarshal([]byte(sidecars.String), &f.Sidecars); err != nil {
			return model.Frame{}, fmt.Errorf("unmarshal sidecars for frame %d: %w", f.ID, err)
		}
	}
	// wcs_status is redundant with Astrometry.Status (scanned only to keep column
	// order), so the blob is the source of truth for the Frame value.
	if astrometry.Valid && astrometry.String != "" {
		var a model.Astrometry
		if err := json.Unmarshal([]byte(astrometry.String), &a); err != nil {
			return model.Frame{}, fmt.Errorf("unmarshal astrometry for frame %d: %w", f.ID, err)
		}
		f.Astrometry = &a
	}
	return f, nil
}

// astrometryCols renders a frame's Astrometry into its (wcs_status, astrometry)
// column bindings. A nil Astrometry — every non-primary write (a failed re-pull, a
// reclassify move, the delete lifecycle) — binds both to NULL so the COALESCE
// conflict-update preserves any existing value rather than erasing it.
func astrometryCols(a *model.Astrometry) (status any, blob any, err error) {
	if a == nil {
		return nil, nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal astrometry: %w", err)
	}
	return a.Status, string(b), nil
}

// scanFrames drains rows into a slice.
func scanFrames(rows *sql.Rows) ([]model.Frame, error) {
	defer rows.Close()
	var out []model.Frame
	for rows.Next() {
		f, err := scanFrame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertFrame inserts or updates a frame by UNIQUE(rig_id, source_share,
// source_path). Sidecars marshal to JSON; the 0-sentinel columns store as NULL.
func (s *sqlStore) UpsertFrame(ctx context.Context, f *model.Frame) error {
	sidecars, err := json.Marshal(f.Sidecars)
	if err != nil {
		return fmt.Errorf("marshal sidecars: %w", err)
	}
	wcsStatus, astro, err := astrometryCols(f.Astrometry)
	if err != nil {
		return err
	}
	// The two astrometry CASEs make the Tier-1 statuses (solving/solved/failed/
	// unsolvable) STICKY against a Tier-0 re-extraction: a re-file/reclassify/differ
	// re-pull carries a non-nil `none` astrometry (recordFiled sets f.Astrometry from
	// FromHeader), and a plain COALESCE would let that `none` overwrite a solved WCS
	// and re-queue a frame the design promised never to re-queue. The ELSE branch is
	// the original Tier-0 COALESCE verbatim, preserved for NULL/none/header rows. A
	// genuine content change (differ) must clear the row explicitly via
	// ClearFrameAstrometry — a decision, not an accident.
	const q = `
INSERT INTO frames (rig_id, source_share, source_path, sha256, size, mtime,
	frame_type, target, filter, exptime, set_temp, gain, "offset", binning, sensor,
	date_obs, night, dest_path, sidecars, state, attempts, next_retry_at, pulled_at,
	deleted_at, error, wcs_status, astrometry, retarget_sha256, retarget_from,
	panel_base, panel_row, panel_col, data_sha256, archive_sha256, archive_size)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(rig_id, source_share, source_path) DO UPDATE SET
	sha256=excluded.sha256, size=excluded.size, mtime=excluded.mtime,
	frame_type=excluded.frame_type, target=excluded.target, filter=excluded.filter,
	exptime=excluded.exptime, set_temp=excluded.set_temp, gain=excluded.gain,
	"offset"=excluded."offset", binning=excluded.binning, sensor=excluded.sensor,
	date_obs=excluded.date_obs, night=excluded.night, dest_path=excluded.dest_path,
	sidecars=excluded.sidecars, state=excluded.state, attempts=excluded.attempts,
	next_retry_at=excluded.next_retry_at, pulled_at=excluded.pulled_at,
	deleted_at=excluded.deleted_at, error=excluded.error,
	wcs_status = CASE WHEN frames.wcs_status IN ('solving','solved','failed','unsolvable')
	                  THEN frames.wcs_status
	                  ELSE COALESCE(excluded.wcs_status, frames.wcs_status) END,
	astrometry = CASE WHEN frames.wcs_status IN ('solving','solved','failed','unsolvable')
	                  THEN frames.astrometry
	                  ELSE COALESCE(excluded.astrometry, frames.astrometry) END,
	-- A re-file describes a file that is once again present and verified at rest,
	-- so any out-of-band-deletion mark, restore request, or acknowledgement is stale
	-- and cleared (missing_acked_at per design library-reconciliation §10.5).
	missing_seen_at = NULL, restore_requested_at = NULL, missing_acked_at = NULL,
	-- The retarget overlay follows the plain excluded= pattern (NOT the sticky
	-- astrometry CASE, NOT the always-NULL missing_* pattern): a genuinely-new re-file
	-- binds "" → NULL and RESETS the overlay, while a retarget/re-apply binds the real
	-- values and SETS it — the reset-vs-set behavior comes for free (design
	-- routing-and-filing §9.2).
	retarget_sha256 = excluded.retarget_sha256, retarget_from = excluded.retarget_from,
	-- The pane columns are derived from target, so they follow it on every write:
	-- a retarget that renames a frame INTO or OUT OF a pane must not leave the old
	-- coordinates behind (design routing-and-filing §7.4).
	panel_base = excluded.panel_base, panel_row = excluded.panel_row,
	panel_col = excluded.panel_col,
	-- The exposure identity is fixed for the exposure's life, so a re-file that
	-- arrives without one PRESERVES the recorded value rather than nulling it (the
	-- astrometry COALESCE precedent, not the overlay's excluded= reset). A genuinely
	-- different exposure at the same path carries its own non-NULL digest and
	-- overwrites it (design frame-lifecycle §11.3).
	data_sha256 = COALESCE(excluded.data_sha256, frames.data_sha256),
	-- The archive-side anchors follow the plain excluded= pattern, exactly as the
	-- overlay does: every filing site writes the file it just verified, so a sticky
	-- CASE or a COALESCE here would let a stale value outlive the bytes it names.
	-- A missed write site therefore empties the column loudly instead of degrading
	-- silently (design frame-lifecycle §11.14).
	archive_sha256 = excluded.archive_sha256, archive_size = excluded.archive_size,
	-- Every filing write is the frame's source coming to agree with its row again,
	-- which is the deferral counter's reset condition, so the bound is per episode of
	-- disagreement rather than per row lifetime. It is absent from the INSERT list
	-- for the same reason: a fresh row has nothing deferred, and the column's
	-- DEFAULT 0 says so (design frame-lifecycle §11.2).
	delete_defer_count = 0
RETURNING id, wcs_status, astrometry`
	// RETURNING gives the EFFECTIVE post-conflict state so the caller's emit never
	// paints a stale value (a re-file of a solved row reports solved, not the
	// incoming none). It MUST include id: switching from Exec+LastInsertId to
	// QueryRow otherwise leaves f.ID==0 on a fresh insert, breaking the solve nudge.
	var (
		id                  int64
		effStatus, effAstro sql.NullString
	)
	if err := s.write.QueryRowContext(ctx, q,
		f.RigID, f.SourceShare, f.SourcePath, f.SHA256, f.Size, f.Mtime,
		f.FrameType, f.Target, f.Filter, f.Exptime, setTempArg(f), f.Gain, f.Offset, f.Binning, f.Sensor,
		f.DateObs, nullInt(f.Night), f.DestPath, string(sidecars), string(f.State), f.Attempts,
		nullInt(f.NextRetryAt), nullInt(f.PulledAt), nullInt(f.DeletedAt), f.Error, wcsStatus, astro,
		nullStr(f.RetargetSHA256), nullStr(f.RetargetFrom),
		nullStr(f.PanelBase), nullInt(int64(f.PanelRow)), nullInt(int64(f.PanelCol)),
		nullStr(f.DataSHA256), nullStr(f.ArchiveSHA256), nullInt(f.ArchiveSize)).
		Scan(&id, &effStatus, &effAstro); err != nil {
		return fmt.Errorf("upsert frame %s/%s/%s: %w", f.RigID, f.SourceShare, f.SourcePath, err)
	}
	f.ID = id
	// Reflect the persisted astrometry back onto the struct (nil on NULL), exactly as
	// scanFrame does, so the caller's frame.done emit and the solve-enqueue predicate
	// read the effective status.
	if effAstro.Valid && effAstro.String != "" {
		var a model.Astrometry
		if err := json.Unmarshal([]byte(effAstro.String), &a); err != nil {
			return fmt.Errorf("unmarshal effective astrometry for frame %d: %w", f.ID, err)
		}
		f.Astrometry = &a
	} else {
		f.Astrometry = nil
	}
	return nil
}

// UpdateFrame updates a frame by id (full row overwrite of the mutable columns).
//
// It writes a SUBSET: every column added after migrate2 — wcs_status, astrometry,
// the three missing_*, both retarget_*, the three panel_*, data_sha256, both
// archive_* and delete_defer_count — is deliberately absent. Its callers
// (holdFrame, markDeleted) pass a by-value copy of a model.Frame read earlier, so
// listing a column here is what would let a stale copy clobber a value another
// writer set in the meantime. Adding the pane columns would CREATE that hazard,
// not avoid it: they change only when target does, which this path never touches,
// and the copy-keyed columns get targeted writers for the same reason.
func (s *sqlStore) UpdateFrame(ctx context.Context, f *model.Frame) error {
	sidecars, err := json.Marshal(f.Sidecars)
	if err != nil {
		return fmt.Errorf("marshal sidecars: %w", err)
	}
	const q = `
UPDATE frames SET
	rig_id=?, source_share=?, source_path=?, sha256=?, size=?, mtime=?,
	frame_type=?, target=?, filter=?, exptime=?, set_temp=?, gain=?, "offset"=?,
	binning=?, sensor=?, date_obs=?, night=?, dest_path=?, sidecars=?, state=?,
	attempts=?, next_retry_at=?, pulled_at=?, deleted_at=?, error=?
WHERE id=?`
	_, err = s.write.ExecContext(ctx, q,
		f.RigID, f.SourceShare, f.SourcePath, f.SHA256, f.Size, f.Mtime,
		f.FrameType, f.Target, f.Filter, f.Exptime, setTempArg(f), f.Gain, f.Offset,
		f.Binning, f.Sensor, f.DateObs, nullInt(f.Night), f.DestPath, string(sidecars), string(f.State),
		f.Attempts, nullInt(f.NextRetryAt), nullInt(f.PulledAt), nullInt(f.DeletedAt), f.Error, f.ID)
	if err != nil {
		return fmt.Errorf("update frame %d: %w", f.ID, err)
	}
	return nil
}

// SetFrameAstrometry overwrites a frame's astrometry unconditionally (by id). This
// is the writer a future Tier-1 solver uses to promote none→solved/failed; the
// Tier-0 backfill uses the pending-guarded variant instead.
func (s *sqlStore) SetFrameAstrometry(ctx context.Context, id int64, a *model.Astrometry) error {
	status, blob, err := astrometryCols(a)
	if err != nil {
		return err
	}
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status=?, astrometry=? WHERE id=?`, status, blob, id); err != nil {
		return fmt.Errorf("set astrometry for frame %d: %w", id, err)
	}
	return nil
}

// SetFrameAstrometryIfPending writes a frame's astrometry only while wcs_status is
// still NULL. The startup backfill uses it so a live re-pull that populated the row
// between the queue snapshot and this write is never clobbered — it only ever
// promotes NULL → a terminal status.
func (s *sqlStore) SetFrameAstrometryIfPending(ctx context.Context, id int64, a *model.Astrometry) error {
	status, blob, err := astrometryCols(a)
	if err != nil {
		return err
	}
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status=?, astrometry=? WHERE id=? AND wcs_status IS NULL`,
		status, blob, id); err != nil {
		return fmt.Errorf("set astrometry (if pending) for frame %d: %w", id, err)
	}
	return nil
}

// ListFramesNeedingAstrometry returns the next page of frames the Tier-0 backfill
// has not processed (wcs_status IS NULL), by an advancing id cursor. It filters to
// frames whose bytes are retained on the NAS — the same on-disk-copy states
// eligibleForImage (internal/web/image_api.go) uses. `deleted` is included
// deliberately: its dest_path is the permanent NAS archive, still readable, so do
// not "fix" this by excluding it.
func (s *sqlStore) ListFramesNeedingAstrometry(ctx context.Context, afterID int64, limit int) ([]model.Frame, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE id > ? AND wcs_status IS NULL AND dest_path <> ''
		   AND state IN ('verified','deleted','unclassified','held')
		 ORDER BY id LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list frames needing astrometry: %w", err)
	}
	return scanFrames(rows)
}

// ListFramesNeedingDataDigest returns the next page of rows carrying no exposure
// identity, by an advancing id cursor, for the backfill of design frame-lifecycle
// §11.8. A row with no recorded data digest gives the discriminator's second leg
// nothing to establish, so a revision of its frame cannot be recognised until the
// backfill records one.
//
// held selects which of the two passes to page through, and the order is the
// design's, not an optimisation. The first pass takes rows already `held` by this
// bug: those are the ones whose next listing would otherwise raise a critical
// frame_lost, and the archive copy that arm verifies is the very proof the cursor
// needs, so they are backfilled ahead of rows that merely lack the value. The
// second pass takes every other row with an archive copy — and then nothing else
// proactively, a row with no archive_sha256 having nothing to prove a digest
// against.
//
// It filters to the same on-disk-copy states ListFramesNeedingAstrometry uses,
// `deleted` included for the same reason: its dest_path is the permanent NAS
// archive, still readable.
func (s *sqlStore) ListFramesNeedingDataDigest(ctx context.Context, afterID int64, held bool, limit int) ([]model.Frame, error) {
	if limit <= 0 {
		limit = 256
	}
	states := `state IN ('verified','deleted','unclassified')`
	if held {
		states = `state = 'held'`
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE id > ? AND COALESCE(data_sha256,'') = '' AND dest_path <> ''
		   AND COALESCE(archive_sha256,'') <> '' AND `+states+`
		 ORDER BY id LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list frames needing a data digest: %w", err)
	}
	return scanFrames(rows)
}

// SetFrameDataDigestIfPending records an exposure identity on a row that carries
// none. It is the backfill's only write, and it advances data_sha256 and nothing
// else (design frame-lifecycle §11.2).
//
// It is a compare-and-set on the anchors the caller proved against rather than a
// blind UPDATE, because the digest is only as good as the copy it was read from
// and the row that copy answers for is named by those anchors: a re-file landing
// between the read and this write leaves them describing a different file, and
// writing the digest anyway would record one exposure's identity on another
// exposure's row — the single failure the proof exists to prevent. The
// already-recorded guard is the same rule facing the other way: a value a real
// proof path has since written is never clobbered by a cursor holding older bytes.
// A refused write simply leaves the row for the next pass.
func (s *sqlStore) SetFrameDataDigestIfPending(ctx context.Context, id int64, wantSHA256, wantArchiveSHA256, dataSHA256 string) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET data_sha256=?
		 WHERE id=? AND COALESCE(data_sha256,'') = ''
		   AND COALESCE(sha256,'') = ? AND COALESCE(archive_sha256,'') = ?`,
		nullStr(dataSHA256), id, wantSHA256, wantArchiveSHA256)
	if err != nil {
		return false, fmt.Errorf("set data digest for frame %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set data digest for frame %d: %w", id, err)
	}
	return n > 0, nil
}

// ListFramesNeedingSolve returns the next page of Tier-1-solvable frames — light
// frames the ASIAIR left without an embedded WCS (wcs_status='none') — by an
// advancing id cursor. It mirrors ListFramesNeedingAstrometry's eligible-state
// and advancing-cursor contract; frame_type='LIGHT' (upper-case, per
// model.TypeLight) excludes calibration frames, which have no sky to solve.
func (s *sqlStore) ListFramesNeedingSolve(ctx context.Context, afterID int64, limit int) ([]model.Frame, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE id > ? AND wcs_status = 'none' AND frame_type = 'LIGHT' AND dest_path <> ''
		   AND state IN ('verified','deleted','unclassified','held')
		 ORDER BY id LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list frames needing solve: %w", err)
	}
	return scanFrames(rows)
}

// ClaimFrameForSolve moves a frame none→solving with a guarded compare-and-set,
// writing the blob with Status="solving" (Hints preserved) so the column and the
// blob move together (the wcs_status == astrometry.status invariant holds). It
// returns rowsAffected==1; a false claim means the row was re-pulled, solved, or
// already claimed — skip it.
func (s *sqlStore) ClaimFrameForSolve(ctx context.Context, id int64, a *model.Astrometry) (bool, error) {
	status, blob, err := astrometryCols(a)
	if err != nil {
		return false, err
	}
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status=?, astrometry=? WHERE id=? AND wcs_status='none'`,
		status, blob, id)
	if err != nil {
		return false, fmt.Errorf("claim frame %d for solve: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetFrameSolveResult is the guarded terminal writer (design features §3.5.3): it
// writes the result only WHILE the row is still 'solving', so a mid-solve differ
// re-pull that reset the row cannot be clobbered by a WCS computed from bytes
// that no longer exist. It writes solved/failed/unsolvable (and is reused for the
// per-frame panic reclaim, writing 'none'). A false return means the row left
// 'solving' under the solve → the caller discards the result and emits nothing.
func (s *sqlStore) SetFrameSolveResult(ctx context.Context, id int64, a *model.Astrometry) (bool, error) {
	status, blob, err := astrometryCols(a)
	if err != nil {
		return false, err
	}
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status=?, astrometry=? WHERE id=? AND wcs_status='solving'`,
		status, blob, id)
	if err != nil {
		return false, fmt.Errorf("set solve result for frame %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReclaimSolvingFrames resets every 'solving' row back to 'none' in one bulk
// statement, moving column and blob together (json_set on the status key) so the
// invariant holds. Called once at startup so a crash mid-solve self-heals; at most
// a handful of rows.
func (s *sqlStore) ReclaimSolvingFrames(ctx context.Context) (int, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status='none', astrometry=json_set(astrometry,'$.status','none')
		 WHERE wcs_status='solving'`)
	if err != nil {
		return 0, fmt.Errorf("reclaim solving frames: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ClearFrameAstrometry resets a frame to NULL/NULL (unprocessed) — the deliberate
// reset the content-change (differ) path calls so new content re-extracts (Tier 0)
// and re-solves (Tier 1). NULL, not 'none', because a re-file follows and
// re-extracts; this is the store method design features §3.5.1's sticky guard
// depends on for a genuine content change to override a stale solution.
func (s *sqlStore) ClearFrameAstrometry(ctx context.Context, id int64) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status=NULL, astrometry=NULL WHERE id=?`, id); err != nil {
		return fmt.Errorf("clear astrometry for frame %d: %w", id, err)
	}
	return nil
}

// RequeueFloorUnsolvable re-queues the below-catalog-floor unsolvable frames whose
// own field height (fov_deg) now fits a catalog with the given floor: it flips them
// 'unsolvable' → 'none' (NOT NULL — 'none' is the Tier-1 solve queue the re-sweep,
// the nudge, and ClaimFrameForSolve all target; NULL is the Tier-0 backfill queue
// that runs only once at startup). The worker re-reads the header for hints/NAXIS,
// so the stale blob body doesn't matter. The primary trigger is a catalog download
// completing; the startup call is a safety net. Returns the number re-queued.
func (s *sqlStore) RequeueFloorUnsolvable(ctx context.Context, floor float64) (int, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status='none', astrometry=json_set(astrometry,'$.status','none')
		 WHERE wcs_status='unsolvable'
		   AND CAST(json_extract(astrometry,'$.fov_deg') AS REAL) >= ?`,
		floor)
	if err != nil {
		return 0, fmt.Errorf("requeue below-floor unsolvable frames: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// RequeueSolveFailures re-queues every `failed` frame back to `none`, resetting the
// per-frame attempt budget, so the solve worker retries them — the manual "re-solve"
// action after a deeper star catalog is installed. `failed` is where a shallow
// catalog's "no star match" lands; below-floor `unsolvable` frames are re-queued
// automatically on catalog download, and no-hint `unsolvable`/`solved` frames are
// deliberately left alone (re-solving them can't help / has no value). Returns the
// number re-queued.
func (s *sqlStore) RequeueSolveFailures(ctx context.Context) (int, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET wcs_status='none',
		   astrometry=json_set(astrometry,'$.status','none','$.attempts',0)
		 WHERE wcs_status='failed'`)
	if err != nil {
		return 0, fmt.Errorf("requeue solve failures: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// SetState updates just the state column of a frame by id.
func (s *sqlStore) SetState(ctx context.Context, id int64, state model.FrameState) error {
	_, err := s.write.ExecContext(ctx, `UPDATE frames SET state=? WHERE id=?`, string(state), id)
	if err != nil {
		return fmt.Errorf("set state of frame %d: %w", id, err)
	}
	return nil
}

// DeferDelete advances the pre-delete gate's deferral counter by one, the
// row-persisted bound of design frame-lifecycle §11.13.
//
// It is a read-modify-write in SQL rather than a value the caller computes, so
// the column can only ever move forward: every path that RESETS it writes a
// literal zero, and a caller writing back a count it read earlier is how a reset
// committed in between would be undone. Nothing else on the row is touched —
// the deferral proves nothing about either copy, it only records that this pass
// declined to conclude anything.
func (s *sqlStore) DeferDelete(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE frames SET delete_defer_count = delete_defer_count + 1 WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("defer delete of frame %d: %w", id, err)
	}
	return nil
}

// DismissFrame tombstones a frame in one UPDATE: state=dismissed with pulled_at
// and dest_path cleared, so the row leaves the pulled_at-keyed transfer-history /
// "transferred tonight" queries while its size/mtime are retained for decide()'s
// change-detection (the tombstone's stickiness).
func (s *sqlStore) DismissFrame(ctx context.Context, id int64) error {
	// Also clear the out-of-band-deletion columns: dismissal nulls dest_path, so a
	// surviving restore_requested_at would keep the row in FramesAwaitingRestore and
	// send restoreNAS at an empty destination, and a surviving missing_seen_at could
	// never be cleared by the scan (which skips dest_path='').
	_, err := s.write.ExecContext(ctx,
		`UPDATE frames SET state='dismissed', pulled_at=NULL, dest_path=NULL,
			missing_seen_at=NULL, restore_requested_at=NULL, missing_acked_at=NULL WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("dismiss frame %d: %w", id, err)
	}
	return nil
}

// CountLiveFramesByDest counts a rig's frames referencing destPath, excluding one
// id, so a dismiss can tell whether another row still owns the shared NAS copy.
func (s *sqlStore) CountLiveFramesByDest(ctx context.Context, rigID, destPath string, excludeID int64) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frames WHERE rig_id=? AND dest_path=? AND id!=?`,
		rigID, destPath, excludeID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count live frames by dest: %w", err)
	}
	return n, nil
}

// GetFrame is the skip-predicate lookup by (rig_id, source_share, source_path).
func (s *sqlStore) GetFrame(ctx context.Context, rigID, share, path string) (*model.Frame, bool, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+frameColumns+` FROM frames WHERE rig_id=? AND source_share=? AND source_path=?`,
		rigID, share, path)
	return oneFrame(row)
}

// GetFrameByID looks up a frame by primary key.
func (s *sqlStore) GetFrameByID(ctx context.Context, id int64) (*model.Frame, bool, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+frameColumns+` FROM frames WHERE id=?`, id)
	return oneFrame(row)
}

// oneFrame scans a single-row query, returning (nil, false, nil) when absent.
func oneFrame(row *sql.Row) (*model.Frame, bool, error) {
	f, err := scanFrame(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &f, true, nil
}

// ListFrames returns a page of frames for a rig, optionally filtered by state and
// filter (empty = all), plus the unpaginated total. It uses idx_frames_rig_state.
func (s *sqlStore) ListFrames(ctx context.Context, rigID, state, filter string, page, pageSize int) ([]model.Frame, int, error) {
	var where []string
	var args []any
	if rigID != "" {
		where = append(where, "rig_id=?")
		args = append(args, rigID)
	}
	if state != "" {
		where = append(where, "state=?")
		args = append(args, state)
	}
	if filter != "" {
		where = append(where, "filter=?")
		args = append(args, filter)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM frames`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count frames: %w", err)
	}

	if pageSize <= 0 {
		pageSize = 50
	}
	if page < 0 {
		page = 0
	}
	q := `SELECT ` + frameColumns + ` FROM frames` + clause + ` ORDER BY id LIMIT ? OFFSET ?`
	rows, err := s.read.QueryContext(ctx, q, append(args, pageSize, page*pageSize)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list frames: %w", err)
	}
	frames, err := scanFrames(rows)
	if err != nil {
		return nil, 0, err
	}
	return frames, total, nil
}

// ListTransferHistory returns a page of the combined transfer history: frames
// that were actually pulled to the NAS (pulled_at set), newest first, optionally
// scoped to one rig and/or a pulled_at time window. rigID "" spans every rig;
// from/to are inclusive unix-second bounds on pulled_at (0 = unbounded on that
// side). It returns the page plus the unpaginated total. Unlike ListFrames (id
// ASC), this orders by id DESC so the latest transfer leads the history view.
func (s *sqlStore) ListTransferHistory(ctx context.Context, rigID string, from, to int64, page, pageSize int) ([]model.Frame, int, error) {
	// pulled_at IS NOT NULL is the "actually copied to the NAS" predicate: the 0
	// sentinel stores as NULL, so this excludes never-pulled (failed/held) frames.
	where := []string{"pulled_at IS NOT NULL"}
	var args []any
	if from > 0 {
		where = append(where, "pulled_at >= ?")
		args = append(args, from)
	}
	if to > 0 {
		where = append(where, "pulled_at <= ?")
		args = append(args, to)
	}
	return s.historyPage(ctx, rigID, where, args, page, pageSize)
}

// neverDeletableWhere selects the frames that are never removed from the device:
// held and failed always, unclassified only while not culled, since a culled one
// is leaving the device through the cull drain (design library-reconciliation
// §11.5, I36). It is the backlog alert's condition (design architecture §10).
const neverDeletableWhere = `(state IN ('held','failed') OR (state='unclassified' AND missing_seen_at IS NULL))`

// ListNeverDeletable returns a page of a rig's never-deletable frames (rigID ""
// spans every rig), newest first, pulled or not, with the unpaginated total: the
// frames behind the backlog alert, which links to exactly this list.
func (s *sqlStore) ListNeverDeletable(ctx context.Context, rigID string, page, pageSize int) ([]model.Frame, int, error) {
	return s.historyPage(ctx, rigID, []string{neverDeletableWhere}, nil, page, pageSize)
}

// historyPage runs one newest-first page of a history query: where/args are the
// view's predicates, to which a non-empty rigID adds the rig scope.
func (s *sqlStore) historyPage(ctx context.Context, rigID string, where []string, args []any, page, pageSize int) ([]model.Frame, int, error) {
	if rigID != "" {
		where = append(where, "rig_id=?")
		args = append(args, rigID)
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM frames`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count transfer history: %w", err)
	}

	if pageSize <= 0 {
		pageSize = 50
	}
	if page < 0 {
		page = 0
	}
	q := `SELECT ` + frameColumns + ` FROM frames` + clause + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.read.QueryContext(ctx, q, append(args, pageSize, page*pageSize)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list transfer history: %w", err)
	}
	frames, err := scanFrames(rows)
	if err != nil {
		return nil, 0, err
	}
	return frames, total, nil
}

// RecentFrames returns a rig's most recently seen frames, newest first, across
// every state (the "recent transfers" view). It orders by id DESC — the frame's
// insertion order — so the latest activity leads. limit is bounded by the caller.
func (s *sqlStore) RecentFrames(ctx context.Context, rigID string, limit int) ([]model.Frame, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames WHERE rig_id=? ORDER BY id DESC LIMIT ?`,
		rigID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent frames: %w", err)
	}
	return scanFrames(rows)
}

// DeletionEligible returns the rig's proven-archived frames whose grace has
// elapsed: sha256 present AND pulled_at <= graceCutoff AND either state=verified
// (deletable once grace passes, NAS copy present or culled) or state=unclassified
// AND culled (missing_seen_at set — a non-culled unclassified frame keeps its
// device original as the authoritative copy). Oldest first (the delete pass,
// design frame-lifecycle §9).
//
// The state disjunction is wrapped in its own parentheses so rig_id, grace, and
// the sha256 scoping AND across BOTH branches: mis-parenthesizing would drop
// rig_id/grace from the unclassified branch and authorize deleting other rigs'
// and pre-grace originals (design library-reconciliation I37).
func (s *sqlStore) DeletionEligible(ctx context.Context, rigID string, graceCutoff int64) ([]model.Frame, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE rig_id = ?
		   AND sha256 IS NOT NULL AND sha256 <> ''
		   AND pulled_at IS NOT NULL AND pulled_at <= ?
		   AND ( state = 'verified'
		         OR (state = 'unclassified' AND missing_seen_at IS NOT NULL) )
		 ORDER BY pulled_at ASC, id ASC`,
		rigID, graceCutoff)
	if err != nil {
		return nil, fmt.Errorf("deletion-eligible frames: %w", err)
	}
	return scanFrames(rows)
}

// UnclassifiedFrames returns the rig's frames in the unclassified state, oldest
// first (the reclassify command).
func (s *sqlStore) UnclassifiedFrames(ctx context.Context, rigID string) ([]model.Frame, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames WHERE rig_id=? AND state=? ORDER BY id ASC`,
		rigID, string(model.StateUnclassified))
	if err != nil {
		return nil, fmt.Errorf("unclassified frames: %w", err)
	}
	return scanFrames(rows)
}

// FailedDueForRetry returns failed frames whose backoff has elapsed:
// state=failed AND (next_retry_at IS NULL OR next_retry_at <= now).
func (s *sqlStore) FailedDueForRetry(ctx context.Context, rigID string, now int64) ([]model.Frame, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE rig_id=? AND state=? AND (next_retry_at IS NULL OR next_retry_at <= ?)
		 ORDER BY id ASC`,
		rigID, string(model.StateFailed), now)
	if err != nil {
		return nil, fmt.Errorf("failed-due-for-retry frames: %w", err)
	}
	return scanFrames(rows)
}

// CountFramesTonight counts distinct dest_path frames pulled since sinceUnix for a
// rig. It dedupes by dest_path so an H12 frame present on both the USB and eMMC
// shares (two rows, one dest_path) counts once (design architecture §7).
func (s *sqlStore) CountFramesTonight(ctx context.Context, rigID string, sinceUnix int64) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT dest_path) FROM frames
		 WHERE rig_id=? AND pulled_at IS NOT NULL AND pulled_at >= ?
		   AND dest_path IS NOT NULL AND dest_path != ''`,
		rigID, sinceUnix).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count frames tonight: %w", err)
	}
	return n, nil
}

// BytesPulledSince returns the total bytes copied to the NAS across the whole
// fleet since the given unix-seconds instant. pulled_at is set only when a frame
// was byte-verified onto the NAS (verified and unclassified frames both have a
// NAS copy; a failed/held transfer never records pulled_at), so pulled_at IS NOT
// NULL is the correct "actually copied" predicate. It dedupes by dest_path like
// CountFramesTonight so a frame present on both the USB and eMMC shares (two rows,
// one dest_path) contributes its bytes once.
func (s *sqlStore) BytesPulledSince(ctx context.Context, since int64) (int64, error) {
	var n int64
	err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size), 0) FROM (
			SELECT MAX(size) AS size FROM frames
			WHERE pulled_at IS NOT NULL AND pulled_at >= ?
			  AND dest_path IS NOT NULL AND dest_path != ''
			GROUP BY dest_path
		 )`,
		since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("bytes pulled since: %w", err)
	}
	return n, nil
}

// ResidentCount counts distinct dest_path frames still on the device (state IN
// verified, unclassified, held, failed), deduped by dest_path (design
// architecture §7).
func (s *sqlStore) ResidentCount(ctx context.Context, rigID string) (int, error) {
	placeholders, args := stateInClause(rigID, residentStates)
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT dest_path) FROM frames
		 WHERE rig_id=? AND dest_path IS NOT NULL AND dest_path != '' AND state IN (`+placeholders+`)`,
		args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("resident count: %w", err)
	}
	return n, nil
}

// ResidentLeads returns, distinct and sorted, the lead of the source path of
// every frame of rigID on share (matched ignoring case, as Samba resolves it)
// still on the device (residentStates): the path cut to its first two segments
// when it has three or more, else to its first. The device file browser derives
// from them the roots the store still keeps in play, which rename refuses to
// move. DISTINCT runs in SQL so a rig keeping years of originals answers a
// handful of strings; the scan uses idx_frames_rig_state.
func (s *sqlStore) ResidentLeads(ctx context.Context, rigID, share string) ([]string, error) {
	placeholders, args := stateInClause(rigID, residentStates)
	args = append(args, share)
	// i1 is the first slash; i2 the second, counted from just after the first.
	rows, err := s.read.QueryContext(ctx,
		`SELECT DISTINCT CASE
			WHEN i1 = 0 THEN p
			WHEN i2 = 0 THEN substr(p, 1, i1 - 1)
			ELSE substr(p, 1, i1 + i2 - 1)
		 END AS lead
		 FROM (SELECT source_path AS p, instr(source_path, '/') AS i1,
		              instr(substr(source_path, instr(source_path, '/') + 1), '/') AS i2
		       FROM frames
		       WHERE rig_id=? AND state IN (`+placeholders+`) AND source_share=? COLLATE NOCASE)
		 ORDER BY lead`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("resident leads: %w", err)
	}
	defer rows.Close()
	var leads []string
	for rows.Next() {
		var lead string
		if err := rows.Scan(&lead); err != nil {
			return nil, fmt.Errorf("resident leads: %w", err)
		}
		leads = append(leads, lead)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resident leads: %w", err)
	}
	return leads, nil
}

// stateInClause builds a "?,?,..." placeholder string and the matching args
// (rigID first, then each state) for a state IN (...) filter.
func stateInClause(rigID string, states []model.FrameState) (string, []any) {
	ph := make([]string, len(states))
	args := make([]any, 0, len(states)+1)
	args = append(args, rigID)
	for i, st := range states {
		ph[i] = "?"
		args = append(args, string(st))
	}
	return strings.Join(ph, ","), args
}

// --- re-file -------------------------------------------------------------------

// ErrEmptyDestPath refuses an operation keyed on an empty dest_path. An empty
// match key is never a legitimate one: markFailed's upsert stores an empty string
// and ResolveCulledFrame/DismissFrame store NULL, so a rig can hold thousands of
// such rows and matching on the empty string would sweep every one of them into one move
// (design routing-and-filing §8.7).
var ErrEmptyDestPath = errors.New("store: empty dest_path is not a valid match key")

// ErrNoRowsMoved reports that a MoveDestPath matched no row. On the live apply
// path that is a failure the caller must report: PruneDeleted runs hourly and
// deletes one of re-file's two eligible states, so a row can vanish between the
// pre-move re-read and the commit — leaving the file moved with nothing
// referencing it. Only crash recovery, which re-runs a commit that may already
// have happened, passes TolerateNoRows (design routing-and-filing §8.6).
var ErrNoRowsMoved = errors.New("store: no rows matched the move")

// MoveDestPolicy says how MoveDestPath treats a zero-row update. It is a named
// type rather than a bool so the tolerance is legible at the call site.
type MoveDestPolicy int

const (
	// RequireRows fails a move that matched nothing. Every live caller uses it.
	RequireRows MoveDestPolicy = iota
	// TolerateNoRows accepts a move that matched nothing, for crash recovery
	// re-running a commit that may already have landed.
	TolerateNoRows
)

// RefileIntent is one row of refile_intents: a re-file move announced before its
// rename and not yet confirmed committed. It is keyed on the vacated dest_path
// rather than a frame id because one move covers every row sharing that path.
// CreatedAt is diagnostic only — intents have no expiry, because an intent that
// outlives its move is exactly what recovery exists to resolve.
type RefileIntent struct {
	RigID     string
	FromPath  string
	ToPath    string
	CreatedAt int64 // unix seconds
}

// RefileCandidates returns the rig's re-fileable frames, oldest first: an
// archived copy exists (verified or deleted), the row names it (a non-empty
// dest_path), and neither a missing mark nor a standing restore request puts its
// whereabouts in question. Eligibility is then decided per dest_path by the
// caller, since a sharing row can be ineligible on its own.
func (s *sqlStore) RefileCandidates(ctx context.Context, rigID string) ([]model.Frame, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames
		 WHERE rig_id=? AND state IN (?,?)
		   AND dest_path IS NOT NULL AND dest_path <> ''
		   AND missing_seen_at IS NULL AND restore_requested_at IS NULL
		 ORDER BY id ASC`,
		rigID, string(model.StateVerified), string(model.StateDeleted))
	if err != nil {
		return nil, fmt.Errorf("refile candidates: %w", err)
	}
	return scanFrames(rows)
}

// FramesByDestPath returns the rig's rows referencing dest, in id order. Crash
// recovery uses it to tell "the row is still at from_path" from "no row matches
// from_path at all", which are different outcomes with different actions.
func (s *sqlStore) FramesByDestPath(ctx context.Context, rigID, dest string) ([]model.Frame, error) {
	if dest == "" {
		return nil, ErrEmptyDestPath
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+frameColumns+` FROM frames WHERE rig_id=? AND dest_path=? ORDER BY id ASC`,
		rigID, dest)
	if err != nil {
		return nil, fmt.Errorf("frames by dest path: %w", err)
	}
	return scanFrames(rows)
}

// CountFramesByDestAnyRig counts every row referencing destPath across the whole
// fleet. It is deliberately NOT rig-scoped, unlike CountLiveFramesByDest: it is
// how a cross-rig sharer is detected, and such a path is refused rather than
// moved, since one goroutine owns a rig's frame mutations.
func (s *sqlStore) CountFramesByDestAnyRig(ctx context.Context, destPath string) (int, error) {
	if destPath == "" {
		return 0, ErrEmptyDestPath
	}
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frames WHERE dest_path=?`, destPath).Scan(&n); err != nil {
		return 0, fmt.Errorf("count frames by dest (any rig): %w", err)
	}
	return n, nil
}

// CountUndeletedFramesByDestAnyRig counts the rows referencing destPath across the
// fleet that still have a claim on the file's CONTENT, which excludes a `deleted`
// row: its device original is gone and its row is a terminal record, so no write of
// that file can strand it — DeletionEligible never admits it back into the delete
// pass, which is where a stranded anchor would otherwise become a critical
// delete_mismatch.
//
// Not rig-scoped, for the same reason its unfiltered sibling is not.
func (s *sqlStore) CountUndeletedFramesByDestAnyRig(ctx context.Context, destPath string) (int, error) {
	if destPath == "" {
		return 0, ErrEmptyDestPath
	}
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frames WHERE dest_path=? AND state<>?`,
		destPath, string(model.StateDeleted)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count undeleted frames by dest (any rig): %w", err)
	}
	return n, nil
}

// MoveDestPath repoints every row of the rig sharing oldDest at newDest, in one
// transaction and writing no other column. A frame present on both the USB and
// eMMC shares is two rows and one file, and four consumers key on that identity
// (CountLiveFramesByDest, CountFramesTonight, bytes-tonight, ResidentCount), so
// the rows must move together or those counts double. It returns the number of
// rows updated.
func (s *sqlStore) MoveDestPath(ctx context.Context, rigID, oldDest, newDest string, policy MoveDestPolicy) (int, error) {
	if oldDest == "" {
		return 0, ErrEmptyDestPath
	}
	if newDest == "" {
		return 0, fmt.Errorf("move dest path: empty destination")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("move dest path: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE frames SET dest_path=? WHERE rig_id=? AND dest_path=?`, newDest, rigID, oldDest)
	if err != nil {
		return 0, fmt.Errorf("move dest path %s: %w", oldDest, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("move dest path %s: %w", oldDest, err)
	}
	if n == 0 && policy != TolerateNoRows {
		return 0, fmt.Errorf("move dest path %s: %w", oldDest, ErrNoRowsMoved)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("move dest path %s: %w", oldDest, err)
	}
	return int(n), nil
}

// PutRefileIntent records (or rewrites) the intent for one move. It upserts on
// (rig_id, from_path) because the EEXIST retry ladder picks a different candidate
// name for the same source, and the intent must always name the path the rename is
// about to attempt.
func (s *sqlStore) PutRefileIntent(ctx context.Context, in RefileIntent) error {
	if in.FromPath == "" || in.ToPath == "" {
		return ErrEmptyDestPath
	}
	if _, err := s.write.ExecContext(ctx,
		`INSERT INTO refile_intents(rig_id, from_path, to_path, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(rig_id, from_path) DO UPDATE SET
			to_path = excluded.to_path, created_at = excluded.created_at`,
		in.RigID, in.FromPath, in.ToPath, in.CreatedAt); err != nil {
		return fmt.Errorf("put refile intent %s: %w", in.FromPath, err)
	}
	return nil
}

// ClearRefileIntent drops one intent. Clearing an absent intent is not an error:
// every failure path clears unconditionally.
func (s *sqlStore) ClearRefileIntent(ctx context.Context, rigID, fromPath string) error {
	if _, err := s.write.ExecContext(ctx,
		`DELETE FROM refile_intents WHERE rig_id=? AND from_path=?`, rigID, fromPath); err != nil {
		return fmt.Errorf("clear refile intent %s: %w", fromPath, err)
	}
	return nil
}

// ListRefileIntents returns every open intent across the fleet, oldest first. It
// is fleet-wide because startup recovery runs before any worker exists and must
// resolve every rig's intents before workers start.
func (s *sqlStore) ListRefileIntents(ctx context.Context) ([]RefileIntent, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT rig_id, from_path, to_path, created_at FROM refile_intents
		 ORDER BY created_at ASC, rig_id ASC, from_path ASC`)
	if err != nil {
		return nil, fmt.Errorf("list refile intents: %w", err)
	}
	defer rows.Close()
	var out []RefileIntent
	for rows.Next() {
		var in RefileIntent
		if err := rows.Scan(&in.RigID, &in.FromPath, &in.ToPath, &in.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan refile intent: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// OpenIntentPaths returns the rig's open intents keyed by the path being vacated,
// for absentDeferReasons: a frame whose dest_path is mid-move is never eligible
// for device-original deletion, however the move failed.
func (s *sqlStore) OpenIntentPaths(ctx context.Context, rigID string) (map[string]bool, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT from_path FROM refile_intents WHERE rig_id=?`, rigID)
	if err != nil {
		return nil, fmt.Errorf("open intent paths: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan open intent path: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}

// SupersedeIntent is one row of supersede_intents: an archive copy whose four-step
// name claim has begun and is not yet confirmed complete, together with everything
// its commit writes.
//
// It is keyed on the rig and the canonical dest_path — the path the row describes,
// never the candidate name a fresh frame's filing path would claim — because one
// swap covers every row sharing that archive file.
//
// The anchors travel WITH the intent rather than being re-derived at recovery
// time: recovery runs before any worker or device session exists, so the device
// copy is out of reach, and Mtime is the DEVICE file's modification time, which
// cannot be derived from the archive copy left on disk. RetargetSHA256 is the
// audit record the commit advances — the overlay describes the archive file, and
// the swap rewrote it — carried for the same reason (design frame-lifecycle
// §11.9).
type SupersedeIntent struct {
	RigID    string
	DestPath string
	// AsidePath is the reserved sibling the existing archive copy is renamed to in
	// step 1, derived from that copy's own content hash. Recovery reads the swap's
	// progress off its presence.
	AsidePath string
	// FrameID is the revised row: the one row of the sharing set whose DEVICE copy
	// was read, and so the only one whose device anchors the commit advances.
	FrameID int64
	// The revised row's device-side anchors, as proven off the device copy.
	SHA256 string
	Size   int64
	Mtime  int64
	// The archive-side anchors of the bytes the claim installs at DestPath, written
	// to every row sharing it.
	ArchiveSHA256 string
	ArchiveSize   int64
	// RetargetSHA256 is what the overlay's audit record becomes on a row that
	// carries one. "" leaves every row's overlay untouched.
	RetargetSHA256 string
	// State and PulledAt are the revised row's lifecycle columns after the commit:
	// the state a completed supersede has proven, and the grace anchor restarting
	// because the device copy has just been read afresh.
	State    model.FrameState
	PulledAt int64
	// CreatedAt is diagnostic only — intents have no expiry, because an intent that
	// outlives its swap is exactly what recovery exists to resolve.
	CreatedAt int64
}

// PutSupersedeIntent records (or rewrites) the intent for one swap. It upserts on
// (rig_id, dest_path): a supersede re-attempted for the same archive copy must
// always name the anchors the commit about to be attempted would write.
func (s *sqlStore) PutSupersedeIntent(ctx context.Context, in SupersedeIntent) error {
	if in.DestPath == "" {
		return ErrEmptyDestPath
	}
	if in.AsidePath == "" {
		return fmt.Errorf("put supersede intent %s: empty aside path", in.DestPath)
	}
	if _, err := s.write.ExecContext(ctx,
		`INSERT INTO supersede_intents(rig_id, dest_path, aside_path, frame_id, sha256, size,
			mtime, archive_sha256, archive_size, retarget_sha256, state, pulled_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(rig_id, dest_path) DO UPDATE SET
			aside_path = excluded.aside_path, frame_id = excluded.frame_id,
			sha256 = excluded.sha256, size = excluded.size, mtime = excluded.mtime,
			archive_sha256 = excluded.archive_sha256, archive_size = excluded.archive_size,
			retarget_sha256 = excluded.retarget_sha256, state = excluded.state,
			pulled_at = excluded.pulled_at, created_at = excluded.created_at`,
		in.RigID, in.DestPath, in.AsidePath, in.FrameID, in.SHA256, in.Size, in.Mtime,
		in.ArchiveSHA256, in.ArchiveSize, in.RetargetSHA256, string(in.State), in.PulledAt,
		in.CreatedAt); err != nil {
		return fmt.Errorf("put supersede intent %s: %w", in.DestPath, err)
	}
	return nil
}

// ClearSupersedeIntent drops one intent. Clearing an absent intent is not an
// error: every failure path clears unconditionally.
func (s *sqlStore) ClearSupersedeIntent(ctx context.Context, rigID, destPath string) error {
	if _, err := s.write.ExecContext(ctx,
		`DELETE FROM supersede_intents WHERE rig_id=? AND dest_path=?`, rigID, destPath); err != nil {
		return fmt.Errorf("clear supersede intent %s: %w", destPath, err)
	}
	return nil
}

// ListSupersedeIntents returns every open intent across the fleet, oldest first.
// It is fleet-wide because startup recovery runs before any worker exists and must
// resolve every rig's intents before workers start.
func (s *sqlStore) ListSupersedeIntents(ctx context.Context) ([]SupersedeIntent, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT rig_id, dest_path, aside_path, frame_id, sha256, size, mtime,
			archive_sha256, archive_size, retarget_sha256, state, pulled_at, created_at
		 FROM supersede_intents ORDER BY created_at ASC, rig_id ASC, dest_path ASC`)
	if err != nil {
		return nil, fmt.Errorf("list supersede intents: %w", err)
	}
	defer rows.Close()
	var out []SupersedeIntent
	for rows.Next() {
		var (
			in    SupersedeIntent
			state string
		)
		if err := rows.Scan(&in.RigID, &in.DestPath, &in.AsidePath, &in.FrameID, &in.SHA256,
			&in.Size, &in.Mtime, &in.ArchiveSHA256, &in.ArchiveSize, &in.RetargetSHA256,
			&state, &in.PulledAt, &in.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan supersede intent: %w", err)
		}
		in.State = model.FrameState(state)
		out = append(out, in)
	}
	return out, rows.Err()
}

// OpenSupersedePaths returns the rig's open swaps keyed by the archive path being
// claimed, for absentDeferReasons: a frame whose archive copy is mid-swap is never
// eligible for device-original deletion, however the swap failed. That is what
// makes the crash window closed by construction rather than alerted about
// afterwards (design frame-lifecycle §11.9).
func (s *sqlStore) OpenSupersedePaths(ctx context.Context, rigID string) (map[string]bool, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT dest_path FROM supersede_intents WHERE rig_id=?`, rigID)
	if err != nil {
		return nil, fmt.Errorf("open supersede paths: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan open supersede path: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}

// CommitSupersede writes a swap's anchors in one transaction: the revised row's
// and every sharer's. It is the whole of step 3, and recovery replays it verbatim
// rather than deciding anything of its own, so the two can never drift. The
// transaction itself is commitArchiveWrite, which the restore commit shares.
//
// The two row sets differ because the columns are keyed on the copy they describe
// (design frame-lifecycle §11.1 and §11.2):
//
//   - the REVISED row's device anchors advance, because its device copy is the one
//     that was read; so do the archive anchors, its state and its pulled_at (the
//     grace clock restarting on a device read that genuinely happened), and the
//     deferral counter resets, the frame's source and its row agreeing again.
//   - a SHARER's device anchors, state and pulled_at stay untouched: the swap never
//     read its device copy, did not cause any hold it carries and cannot discharge
//     one. Only the archive side moves, because the archive side moves with the
//     file.
//
// data_sha256 is absent from both statements, which is the preserve: the exposure
// identity is side-independent and fixed for the exposure's life, and the header
// edit a supersede files is exactly what it survives. retarget_from is untouched
// for the converse reason — what the frame was labelled before does not change
// when its bytes do.
//
// The overlay advances only on a row that already carries one: retarget_sha256 is
// an audit record whose non-empty value MEANS the frame was retargeted, so writing
// it onto a row that never was would assert a re-label nobody made. An empty
// RetargetSHA256 leaves every row's overlay alone.
//
// The revised row is matched on its dest_path as well as its id, so the commit
// applies only to a row still naming the path the intent was written against. A row
// re-filed elsewhere in the meantime commits nothing rather than acquiring archive
// anchors describing a file at a path it no longer names — a bad anchor, and a
// critical delete_mismatch on its next pass, rather than merely an orphan. It is a
// predicate rather than an argument about reachability for the reason the re-file
// recovery reads its rows by path: a predicate stays true by itself.
//
// It reports whether the revised row was found and how many sharers moved. A
// vanished revised row is not an error here: PruneDeleted can legitimately remove
// a row mid-swap, a re-file can move one out from under an open intent, and
// recovery re-runs a commit that may already have landed.
func (s *sqlStore) CommitSupersede(ctx context.Context, in SupersedeIntent) (bool, int, error) {
	return s.commitArchiveWrite(ctx, ArchiveWrite{
		RigID:          in.RigID,
		DestPath:       in.DestPath,
		FrameID:        in.FrameID,
		SHA256:         in.SHA256,
		Size:           in.Size,
		Mtime:          in.Mtime,
		ArchiveSHA256:  in.ArchiveSHA256,
		ArchiveSize:    in.ArchiveSize,
		RetargetSHA256: in.RetargetSHA256,
		State:          in.State,
		PulledAt:       in.PulledAt,
	})
}

// ArchiveWrite is one write of a shared archive file, as the rows that name it
// record it: the row whose DEVICE copy the write read, and — per the sharer
// invariant of "Any path that writes the shared archive file commits every row
// sharing that dest_path" — every other row naming the same file.
//
// It is the argument of commitArchiveWrite, which both the supersede and the
// restore commit through. The two paths differ in what they prove, not in what the
// proof makes of the rows: design frame-lifecycle §11.2 gives them the same two
// rows of the matrix, so they share one implementation rather than re-deriving the
// column split twice and drifting.
type ArchiveWrite struct {
	RigID string
	// DestPath is the archive file that was written, which is the predicate the
	// sharer set is taken by.
	DestPath string
	// FrameID is the row whose device copy was read, and so the only one whose
	// device-side anchors and lifecycle columns this write advances.
	FrameID int64
	// The device-side anchors of that row's copy, as just proven off the device.
	SHA256 string
	Size   int64
	Mtime  int64
	// The archive-side anchors of the bytes now at DestPath, written to every row
	// sharing it.
	ArchiveSHA256 string
	ArchiveSize   int64
	// RetargetSHA256 is what the overlay's audit record becomes on a row that
	// carries one. "" leaves every row's overlay untouched.
	RetargetSHA256 string
	// State and PulledAt are the read row's lifecycle columns after the write.
	State    model.FrameState
	PulledAt int64
}

// commitArchiveWrite is the transaction both CommitSupersede and CommitRestore are.
func (s *sqlStore) commitArchiveWrite(ctx context.Context, in ArchiveWrite) (bool, int, error) {
	if in.DestPath == "" {
		return false, 0, ErrEmptyDestPath
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("commit archive write: %w", err)
	}
	defer tx.Rollback()

	const overlay = `retarget_sha256 = CASE
		WHEN ? <> '' AND retarget_sha256 IS NOT NULL AND retarget_sha256 <> ''
		THEN ? ELSE retarget_sha256 END`

	sharers, err := tx.ExecContext(ctx,
		`UPDATE frames SET archive_sha256=?, archive_size=?, `+overlay+`
		 WHERE rig_id=? AND dest_path=? AND id<>?`,
		in.ArchiveSHA256, in.ArchiveSize, in.RetargetSHA256, in.RetargetSHA256,
		in.RigID, in.DestPath, in.FrameID)
	if err != nil {
		return false, 0, fmt.Errorf("commit archive write sharers of %s: %w", in.DestPath, err)
	}
	sn, err := sharers.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("commit archive write sharers of %s: %w", in.DestPath, err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE frames SET sha256=?, size=?, mtime=?, archive_sha256=?, archive_size=?,
			state=?, pulled_at=?, delete_defer_count=0, `+overlay+`
		 WHERE rig_id=? AND id=? AND dest_path=?`,
		in.SHA256, in.Size, in.Mtime, in.ArchiveSHA256, in.ArchiveSize,
		string(in.State), in.PulledAt, in.RetargetSHA256, in.RetargetSHA256,
		in.RigID, in.FrameID, in.DestPath)
	if err != nil {
		return false, 0, fmt.Errorf("commit archive write %s: %w", in.DestPath, err)
	}
	rn, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("commit archive write %s: %w", in.DestPath, err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit archive write %s: %w", in.DestPath, err)
	}
	return rn > 0, int(sn), nil
}

// CommitRestore writes what a completed restore has proven, through the same
// transaction a supersede commits: a restore rewrites the archive file from a
// device copy read once, so design frame-lifecycle §11.2 gives its two rows —
// the row whose device copy was read, and every dest_path sharer — the same column
// split the swap's rows have.
//
// FrameID is the row whose device copy was STREAMED, which after a supersede need
// not be the row whose restore the operator requested: a restore puts back the
// bytes the archive expectation names, so the copy it reads is whichever sharing
// row's answers for that expectation (design frame-lifecycle §11.6, the
// restoreNAS row). The requested row is then one of the sharers, and its own
// device anchors stay untouched because its copy was never read.
//
// Committing at all is what a restore's success path did not do: both copies then
// hold the restored bytes, and a half-commit would leave the row two header blocks
// short of both files, with every later VerifyRead hashing a truncated prefix that
// can never match.
func (s *sqlStore) CommitRestore(ctx context.Context, in ArchiveWrite) (bool, int, error) {
	return s.commitArchiveWrite(ctx, in)
}

// CulledRevision is the targeted write of design frame-lifecycle §11.12: the
// device rewrote the header of a frame whose archive copy the operator deleted
// on purpose, so the row's device anchors advance while nothing is filed. The
// operator deleted that exposure, and the device rewriting its header does not
// un-delete it.
//
// It carries the device side and the exposure identity alone. The archive side is
// deliberately absent: no archive copy changed, so the archive-side expectation
// must not move — that is what keeps a file the operator restores by hand later
// from raising a critical delete_mismatch against a perfectly good copy, and what
// stops culled-and-retargeted being a permanent pin.
type CulledRevision struct {
	RigID   string
	FrameID int64
	// The device-side anchors, as proven off the device copy just pulled.
	SHA256 string
	Size   int64
	Mtime  int64
	// DataSHA256 is the exposure identity this path proved off the bytes in hand.
	// It is the one write path that ADVANCES it rather than preserving it: the
	// discriminator establishes the incoming file's data unit against the row's
	// before this write is reached, so writing what was proven is value-identical
	// to leaving it alone, and saying so explicitly is what keeps the column
	// keyed on a proof rather than on a preserve (design frame-lifecycle §11.2).
	DataSHA256 string
}

// CommitCulledRevision advances one culled row's device anchors without filing
// anything, following SetFrameAstrometry's precedent: a targeted write whose
// fields UpsertFrame must never clobber. UpsertFrame clears missing_seen_at,
// restore_requested_at and missing_acked_at unconditionally on every re-file, so
// an ordinary filing write cannot leave the cull mark standing — and leaving the
// row alone is no better, decide's skip gate being keyed on size, so an untouched
// row never matches the revised device file again and the frame is pulled in full
// every poll interval forever.
//
// Every other column is untouched. The astrometry columns in particular are not
// reset: that is ClearFrameAstrometry's job on the supersede path that
// re-extracts, and this path files nothing for a re-extraction to describe.
//
// The write is guarded on the cull mark still standing, and reports whether it
// landed. That guard is the method's contract rather than a formality: advancing
// sha256 on a row that has stopped being culled would leave the row's device
// anchor matching bytes no archive copy holds, and the next pull of that frame
// would read as a content clash instead of a revision — a rot_clash alert and a
// token-suffixed duplicate. A refused write leaves the caller on the ordinary
// path, where the row is re-filed as the frame it has come to look like.
func (s *sqlStore) CommitCulledRevision(ctx context.Context, in CulledRevision) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE frames SET sha256=?, size=?, mtime=?, data_sha256=?, delete_defer_count=0
		 WHERE rig_id=? AND id=? AND missing_seen_at IS NOT NULL`,
		in.SHA256, in.Size, in.Mtime, nullStr(in.DataSHA256), in.RigID, in.FrameID)
	if err != nil {
		return false, fmt.Errorf("commit culled revision of frame %d: %w", in.FrameID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("commit culled revision of frame %d: %w", in.FrameID, err)
	}
	return n > 0, nil
}

// --- night counters ------------------------------------------------------------

// NightRequest is one night-ordinal assignment: the (target, ota, observing_date)
// the ledger is keyed on, plus what the frame's destination folder was observed
// to hold. The hints are read outside this write transaction, so the transaction
// re-validates them before using either.
type NightRequest struct {
	Target, OTA, ObservingDate string
	// Preferred is the ordinal the destination folder already holds for this
	// observing date, or 0 when it holds none.
	Preferred int
	// FolderHighWater is the highest ordinal that folder holds at all, or 0.
	FolderHighWater int
}

// AssignNight returns the stable, never-renumbered night ordinal for a
// (target, ota, observing_date), assigning an ordinal for that (target, ota)
// scope the first time the date is seen (design routing-and-filing §6).
//
// Two mechanisms are jointly load-bearing here and neither is sufficient alone:
//
//  1. The UNIQUE(target, ota, observing_date) constraint plus INSERT ... ON
//     CONFLICT DO NOTHING guarantees each *date* maps to exactly one ordinal — two
//     rigs sharing a (target, ota) that both try to insert the same date can't
//     create two rows.
//  2. The single-writer store (SetMaxOpenConns(1), design architecture §7) serializes the max(n)+1
//     computation — two *different* new dates computing max+1 at the same instant
//     would otherwise both read the same max and collide on the ordinal. The
//     constraint can't catch that (the dates differ), so the single writer must.
//
// Three rules govern the hints, and the transaction applies them itself because
// the hints were read from another pool and may be stale:
//
//  1. A date that already has a row keeps its ordinal — the read-back returns it
//     and both hints are ignored. An assigned night is never renumbered.
//  2. A Preferred that some OTHER observing date in this scope already holds is
//     discarded. One ordinal serves at most one date per scope, and a stale
//     hint must not be what breaks that.
//  3. With no usable hint the ordinal is max(MAX(night), FolderHighWater) + 1,
//     not MAX(night)+1: minting on top of an ordinal the destination folder
//     already occupies is the failure the folder consultation exists to prevent.
//
// The whole thing runs in one write transaction so the read-back reflects this
// assignment.
func (s *sqlStore) AssignNight(ctx context.Context, req NightRequest) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	target, err := canonicalTarget(ctx, tx, req.Target)
	if err != nil {
		return 0, err
	}

	// Rule 1, asked before anything is computed: a date that already has a row
	// keeps its ordinal. The read folds case so a row written under another
	// spelling of this target answers it, which is what stops a second spelling
	// from opening a second night for a date that already has one.
	var night int
	err = tx.QueryRowContext(ctx,
		`SELECT night FROM night_counters
		  WHERE target=? COLLATE NOCASE AND ota=? AND observing_date=?
		  ORDER BY rowid LIMIT 1`,
		target, req.OTA, req.ObservingDate).Scan(&night)
	if err == nil {
		return night, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read assigned night: %w", err)
	}

	var scopeMax int
	// This read is safe under a single writer because no concurrent write can slip
	// a new row in between it and the INSERT below.
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(night), 0) FROM night_counters WHERE target=? COLLATE NOCASE AND ota=?`,
		target, req.OTA).Scan(&scopeMax); err != nil {
		return 0, fmt.Errorf("compute next night: %w", err)
	}

	next := scopeMax + 1
	if req.FolderHighWater > scopeMax {
		next = req.FolderHighWater + 1
	}
	if req.Preferred >= 1 {
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM night_counters
			  WHERE target=? COLLATE NOCASE AND ota=? AND night=? AND observing_date<>?`,
			target, req.OTA, req.Preferred, req.ObservingDate).Scan(&taken); err != nil {
			return 0, fmt.Errorf("validate preferred night: %w", err)
		}
		if taken == 0 {
			next = req.Preferred
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO night_counters(target, ota, observing_date, night) VALUES (?,?,?,?)
		 ON CONFLICT(target, ota, observing_date) DO NOTHING`,
		target, req.OTA, req.ObservingDate, next); err != nil {
		return 0, fmt.Errorf("insert night counter: %w", err)
	}
	// Read back the ordinal actually stored for this exact date — on a conflict the
	// existing (never-renumbered) value wins, not our computed `next`.
	if err := tx.QueryRowContext(ctx,
		`SELECT night FROM night_counters
		  WHERE target=? COLLATE NOCASE AND ota=? AND observing_date=?
		  ORDER BY rowid LIMIT 1`,
		target, req.OTA, req.ObservingDate).Scan(&night); err != nil {
		return 0, fmt.Errorf("read back night: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return night, nil
}

// CanonicalTarget returns the spelling this target was first filed under, when a
// row differing from it only in case already exists, and the target unchanged
// otherwise.
//
// A target's identity has to be case-insensitive because the archive's is: on
// Windows and on an SMB share "IC5146" and "ic5146" name one directory, so two
// spellings of one OBJECT card are two ledger scopes writing into a single tree
// — the second scope mints from an empty ledger and drops a fresh night into a
// NIGHT_n folder that is already somebody else's. Folding at the store's reads
// keeps the ledger one scope; adopting the canonical spelling for the frame keeps
// the path, the frames.target column and every grouping that reads it single too.
//
// The first spelling wins because it is the one the destination folder was
// created under, and night_counters is where it survives: the frames table is a
// rolling window, and the ledger is not.
func (s *sqlStore) CanonicalTarget(ctx context.Context, target string) (string, error) {
	return canonicalTarget(ctx, s.read, target)
}

// rowQuerier is the single-row read *sql.DB and *sql.Tx both provide, so a query
// an assignment must make against its own uncommitted snapshot can be written
// once.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// canonicalTarget is CanonicalTarget over either pool. Inside a write
// transaction it resolves the spelling against the same snapshot the assignment
// inserts into, so two case-variant rows cannot be minted in one batch.
func canonicalTarget(ctx context.Context, q rowQuerier, target string) (string, error) {
	var got string
	err := q.QueryRowContext(ctx,
		`SELECT target FROM night_counters WHERE target=? COLLATE NOCASE ORDER BY rowid LIMIT 1`,
		target).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return target, nil
	}
	if err != nil {
		return "", fmt.Errorf("canonical target: %w", err)
	}
	return got, nil
}

// LookupNight returns the ordinal already assigned to an observing date in a
// scope, if any. It lets the filing path skip both the destination-folder reads
// and the write transaction on the common path where the date is already
// assigned: only the first frame of a night needs either.
func (s *sqlStore) LookupNight(ctx context.Context, target, ota, observingDate string) (int, bool, error) {
	var night int
	err := s.read.QueryRowContext(ctx,
		`SELECT night FROM night_counters
		  WHERE target=? COLLATE NOCASE AND ota=? AND observing_date=?
		  ORDER BY rowid LIMIT 1`,
		target, ota, observingDate).Scan(&night)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup night: %w", err)
	}
	return night, true, nil
}

// MaxNight returns the highest ordinal a scope has assigned, or 0 for a scope
// with no rows. It is the scope half of the mint formula, which a projection of
// what AssignNight would do has to model exactly.
func (s *sqlStore) MaxNight(ctx context.Context, target, ota string) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(night), 0) FROM night_counters WHERE target=? COLLATE NOCASE AND ota=?`,
		target, ota).Scan(&n); err != nil {
		return 0, fmt.Errorf("max night: %w", err)
	}
	return n, nil
}

// NightHeldByOtherDate reports whether an ordinal is already held by a DIFFERENT
// observing date in this scope — AssignNight's rule 2, exposed as a read so a
// projection of an assignment can model the same discard the transaction makes.
func (s *sqlStore) NightHeldByOtherDate(ctx context.Context, target, ota string, night int, observingDate string) (bool, error) {
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM night_counters
		  WHERE target=? COLLATE NOCASE AND ota=? AND night=? AND observing_date<>?`,
		target, ota, night, observingDate).Scan(&n); err != nil {
		return false, fmt.Errorf("night held by other date: %w", err)
	}
	return n > 0, nil
}

// NightsUnder returns the distinct night ordinals of every filed frame whose
// dest_path falls in the half-open range [lo, hi) — step 1 of reading a
// destination folder. The range is byte-exact rather than a GLOB or LIKE match:
// LIKE is case-insensitive for ASCII (a Linux install would match across "M31/"
// and "m31/"), and GLOB has no ESCAPE clause while "[" and "]" survive path
// sanitization. The caller builds the bounds, because the separator is a
// filepath concern.
func (s *sqlStore) NightsUnder(ctx context.Context, lo, hi string) ([]int, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT DISTINCT night FROM frames WHERE dest_path >= ? AND dest_path < ? AND night >= 1`,
		lo, hi)
	if err != nil {
		return nil, fmt.Errorf("nights under path: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan night under path: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NightFolderCounts counts the filed frames in one exact night folder and, of
// those, how many were captured inside a half-open date_obs window — step 2 of
// reading a destination folder. Both counts come from the same index-ranged
// scan, so the date and the folder provably belong to the same rows; two
// independent aggregates over a wider subtree could sample different folders.
//
// COALESCE is load-bearing rather than cosmetic: SUM over zero rows is NULL,
// which fails to scan into an int, and zero rows is the normal case — a pruned
// folder, a fresh folder, and every candidate ordinal that came from a sibling
// subtree.
func (s *sqlStore) NightFolderCounts(ctx context.Context, lo, hi, dateLo, dateHi string) (total, matching int, err error) {
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN date_obs >= ?3 AND date_obs < ?4 THEN 1 ELSE 0 END), 0)
		   FROM frames
		  WHERE dest_path >= ?1 AND dest_path < ?2 AND night >= 1`,
		lo, hi, dateLo, dateHi).Scan(&total, &matching); err != nil {
		return 0, 0, fmt.Errorf("night folder counts: %w", err)
	}
	return total, matching, nil
}

// DistinctTargetsUnder returns the distinct RAW OBJECT spellings filed under a
// dest_path range. Raw is the point: classification stores the OBJECT card
// untouched, so this is what an operator's own grouping by target sees, and two
// spellings of one target sharing a folder is exactly what the caller reports.
func (s *sqlStore) DistinctTargetsUnder(ctx context.Context, lo, hi string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT DISTINCT target FROM frames
		  WHERE dest_path >= ? AND dest_path < ? AND target <> ''`,
		lo, hi)
	if err != nil {
		return nil, fmt.Errorf("distinct targets under path: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan target under path: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RebuildNightCounters upserts the given entries (from existing NIGHT_n folders at
// startup), preserving any existing ordinal — it never renumbers (design
// routing-and-filing §6). It runs in one transaction.
func (s *sqlStore) RebuildNightCounters(ctx context.Context, entries []NightEntry) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, e := range entries {
		// The representative header a NIGHT_n folder is read through carries
		// whichever OBJECT spelling that one frame was written with, so the walk can
		// hand back two case-variant spellings of a target whose folders are one
		// tree. Folding here keeps the rebuilt ledger a single scope.
		target, err := canonicalTarget(ctx, tx, e.Target)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO night_counters(target, ota, observing_date, night) VALUES (?,?,?,?)
			 ON CONFLICT(target, ota, observing_date) DO NOTHING`,
			target, e.OTA, e.ObservingDate, e.Night); err != nil {
			return fmt.Errorf("rebuild night counter (%s,%s,%s): %w", target, e.OTA, e.ObservingDate, err)
		}
	}
	return tx.Commit()
}

// --- alerts --------------------------------------------------------------------

// RaiseAlert inserts an alert row and returns its id, with created reporting
// whether a row was inserted; only a created row is news the notifier announces.
// An identical (rig, category, message) alert that still stands absorbs the raise
// and its id is returned instead. What "still stands" means depends on the raise
// (design architecture §10): for an event it is a row not yet acknowledged, so the
// same problem happening after an acknowledgement is raised again; for a standing
// condition (a.Standing) it is a row not yet resolved, acknowledged or not, so an
// acknowledgement holds until the condition's owner resolves it.
func (s *sqlStore) RaiseAlert(ctx context.Context, a model.Alert) (int64, bool, error) {
	open := `acked_at IS NULL`
	if a.Standing {
		open = `resolved_at IS NULL`
	}
	var existing int64
	err := s.write.QueryRowContext(ctx,
		`SELECT id FROM alerts
		 WHERE rig_id IS ? AND category=? AND message=? AND `+open+`
		 ORDER BY id DESC LIMIT 1`,
		nullStr(a.RigID), string(a.Category), a.Message).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("dedup-check alert: %w", err)
	}

	res, err := s.write.ExecContext(ctx,
		`INSERT INTO alerts(rig_id, severity, category, message, created_at, acked_at)
		 VALUES (?,?,?,?,?,?)`,
		nullStr(a.RigID), string(a.Severity), string(a.Category), a.Message, a.CreatedAt, nullInt(a.AckedAt))
	if err != nil {
		return 0, false, fmt.Errorf("raise alert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ResolveAlerts records that a condition cleared: every unresolved alert of the
// rig (rigID "" for a fleet-wide one) and category whose message starts with
// messagePrefix ("" for any) is marked resolved at `at`, and acknowledged at `at`
// unless the operator already acknowledged it. A resolved row no longer absorbs a
// standing raise, so the condition raises anew if it returns. It returns the
// number of rows resolved.
func (s *sqlStore) ResolveAlerts(ctx context.Context, rigID string, category model.Category, messagePrefix string, at int64) (int, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE alerts SET resolved_at=?, acked_at=COALESCE(acked_at, ?)
		 WHERE rig_id IS ? AND category=? AND resolved_at IS NULL AND substr(message, 1, ?)=?`,
		at, at, nullStr(rigID), string(category), len([]rune(messagePrefix)), messagePrefix)
	if err != nil {
		return 0, fmt.Errorf("resolve %s alerts: %w", category, err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// nullStr maps an empty rig id (a fleet-wide alert) to SQL NULL.
func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ListAlerts returns alerts newest first, up to limit. Acked alerts are excluded
// unless includeAcked is set.
func (s *sqlStore) ListAlerts(ctx context.Context, includeAcked bool, limit int) ([]model.Alert, error) {
	q := `SELECT id, rig_id, severity, category, message, created_at, acked_at FROM alerts`
	if !includeAcked {
		q += ` WHERE acked_at IS NULL`
	}
	q += ` ORDER BY id DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()

	var out []model.Alert
	for rows.Next() {
		var a model.Alert
		var rigID sql.NullString
		var acked sql.NullInt64
		var severity string
		var category string
		if err := rows.Scan(&a.ID, &rigID, &severity, &category, &a.Message, &a.CreatedAt, &acked); err != nil {
			return nil, err
		}
		a.RigID = rigID.String
		a.Severity = model.Severity(severity)
		a.Category = model.Category(category)
		a.AckedAt = intFromNull(acked)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AckAlert marks an alert acknowledged at the given time.
func (s *sqlStore) AckAlert(ctx context.Context, id int64, at int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE alerts SET acked_at=? WHERE id=?`, at, id)
	if err != nil {
		return fmt.Errorf("ack alert %d: %w", id, err)
	}
	return nil
}

// UnackedCount returns the number of unacknowledged alerts (the UI badge).
// GetMeta reads a meta key; ok is false when the key is absent.
func (s *sqlStore) GetMeta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetMeta upserts a meta key/value.
func (s *sqlStore) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// EnsureMeta records candidate as key's value only if key is currently unset or
// empty, and returns the value now in effect (candidate if this call set it, the
// existing value otherwise). It runs in one write transaction against the
// single-writer handle, so concurrent auto-linking of the same destination root by
// multiple rig workers converges on a single library id instead of racing to a
// marker/recorded-id mismatch (design library-reconciliation §8.4). It never
// overwrites a non-empty value.
func (s *sqlStore) EnsureMeta(ctx context.Context, key, candidate string) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("ensure meta begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("ensure meta read: %w", err)
	}
	if existing != "" {
		return existing, nil // already set (by us earlier or a racing worker)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, candidate); err != nil {
		return "", fmt.Errorf("ensure meta write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("ensure meta commit: %w", err)
	}
	return candidate, nil
}

func (s *sqlStore) UnackedCount(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE acked_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("unacked count: %w", err)
	}
	return n, nil
}

// --- log pulls -----------------------------------------------------------------

// RecordLogPull upserts a log-pull record, keyed by (rig_id, source_path) so a
// re-pull overwrites the prior size/dest/pulled_at.
func (s *sqlStore) RecordLogPull(ctx context.Context, l LogPull) error {
	// log_pulls has no UNIQUE constraint in the design architecture §7 DDL, so upsert
	// by hand: update the existing row for this (rig_id, source_path) or insert a
	// fresh one.
	res, err := s.write.ExecContext(ctx,
		`UPDATE log_pulls SET log_type=?, dest_path=?, size=?, pulled_at=?
		 WHERE rig_id=? AND source_path=?`,
		l.LogType, l.DestPath, l.Size, l.PulledAt, l.RigID, l.SourcePath)
	if err != nil {
		return fmt.Errorf("record log pull (update): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = s.write.ExecContext(ctx,
		`INSERT INTO log_pulls(rig_id, log_type, source_path, dest_path, size, pulled_at)
		 VALUES (?,?,?,?,?,?)`,
		l.RigID, l.LogType, l.SourcePath, l.DestPath, l.Size, l.PulledAt)
	if err != nil {
		return fmt.Errorf("record log pull (insert): %w", err)
	}
	return nil
}

// GetLogPull returns the last recorded pull for (rig_id, source_path), feeding the
// size-changed re-pull decision.
func (s *sqlStore) GetLogPull(ctx context.Context, rigID, sourcePath string) (*LogPull, bool, error) {
	var l LogPull
	var dest sql.NullString
	var size, pulledAt sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT rig_id, log_type, source_path, dest_path, size, pulled_at
		 FROM log_pulls WHERE rig_id=? AND source_path=? ORDER BY id DESC LIMIT 1`,
		rigID, sourcePath).Scan(&l.RigID, &l.LogType, &l.SourcePath, &dest, &size, &pulledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get log pull: %w", err)
	}
	l.DestPath = dest.String
	l.Size = intFromNull(size)
	l.PulledAt = intFromNull(pulledAt)
	return &l, true, nil
}

// ListLogPulls returns all recorded log pulls for a rig, newest first.
func (s *sqlStore) ListLogPulls(ctx context.Context, rigID string) ([]LogPull, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT rig_id, log_type, source_path, dest_path, size, pulled_at
		 FROM log_pulls WHERE rig_id=? ORDER BY id DESC`, rigID)
	if err != nil {
		return nil, fmt.Errorf("list log pulls: %w", err)
	}
	defer rows.Close()

	var out []LogPull
	for rows.Next() {
		var l LogPull
		var dest sql.NullString
		var size, pulledAt sql.NullInt64
		if err := rows.Scan(&l.RigID, &l.LogType, &l.SourcePath, &dest, &size, &pulledAt); err != nil {
			return nil, err
		}
		l.DestPath = dest.String
		l.Size = intFromNull(size)
		l.PulledAt = intFromNull(pulledAt)
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- retention -----------------------------------------------------------------

// PruneDeleted removes deleted frames whose deleted_at is older than olderThan, in
// bounded batches so a big first prune can't hold the write conn in one long
// transaction (design architecture §7). Returns the total rows deleted.
func (s *sqlStore) PruneDeleted(ctx context.Context, olderThan int64, batch int) (int, error) {
	// A `deleted` row that still carries a missing mark is a both-sides cull whose
	// device original may reappear on a reinserted card, so it must not be pruned —
	// pruning it would let decide() re-pull the frame. An authorized deletion clears
	// the mark, so it ages out normally.
	return s.pruneBatched(ctx, batch,
		`DELETE FROM frames WHERE id IN (
			SELECT id FROM frames WHERE state=? AND deleted_at IS NOT NULL AND deleted_at < ?
			  AND missing_seen_at IS NULL LIMIT ?
		 )`,
		string(model.StateDeleted), olderThan)
}

// PruneAckedAlerts removes acknowledged alerts acked before olderThan, batched.
func (s *sqlStore) PruneAckedAlerts(ctx context.Context, olderThan int64, batch int) (int, error) {
	return s.pruneBatched(ctx, batch,
		`DELETE FROM alerts WHERE id IN (
			SELECT id FROM alerts WHERE acked_at IS NOT NULL AND acked_at < ? LIMIT ?
		 )`,
		olderThan)
}

// pruneBatched runs a bounded DELETE (whose final `?` is the LIMIT) in a loop until
// no rows are affected, so each transaction is short. leadingArgs are the query's
// args before the trailing LIMIT placeholder.
func (s *sqlStore) pruneBatched(ctx context.Context, batch int, query string, leadingArgs ...any) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	args := append(append([]any{}, leadingArgs...), batch)
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.write.ExecContext(ctx, query, args...)
		if err != nil {
			return total, fmt.Errorf("prune batch: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(n)
		if int(n) < batch {
			return total, nil
		}
	}
}
