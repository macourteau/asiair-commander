package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// EnsureMeta must arbitrate a single winner under concurrency: when many rig
// workers auto-link the same destination root at once (each with its own candidate
// library id), every caller must come back with the SAME recorded id, so the
// on-disk marker each then writes can never disagree with the record (design
// library-reconciliation §8.4).
func TestEnsureMetaConvergesUnderConcurrency(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 16
	const key = "libraryid:/nas/lights"

	results := make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once to maximize contention
			got, err := s.EnsureMeta(ctx, key, fmt.Sprintf("candidate-%02d", i))
			if err != nil {
				t.Errorf("EnsureMeta[%d]: %v", i, err)
				return
			}
			results[i] = got
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("EnsureMeta diverged: caller 0 got %q, caller %d got %q", results[0], i, results[i])
		}
	}
	if persisted, _, _ := s.GetMeta(ctx, key); persisted != results[0] {
		t.Fatalf("persisted id %q disagrees with the arbitrated id %q", persisted, results[0])
	}

	// A later EnsureMeta with a fresh candidate must NOT overwrite the winner.
	if got, err := s.EnsureMeta(ctx, key, "late-comer"); err != nil || got != results[0] {
		t.Fatalf("EnsureMeta overwrote a set id: got %q err %v, want %q", got, err, results[0])
	}
}

// newStore opens a fresh file-backed store in a temp dir. A file (not :memory:) is
// required because the write and read handles share one db file.
func newStore(t *testing.T) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// An absent SET-TEMP and a set-point of exactly 0C must read back apart, through
// both write paths: a re-file renders the first as uncooled and the second as 0C.
func TestSetTempAbsenceRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		path   string
		absent bool
	}{{"Autorun/Dark/uncooled.fit", true}, {"Autorun/Dark/zero.fit", false}} {
		f := &model.Frame{RigID: "rig-01", SourceShare: "EMMC Images", SourcePath: tc.path,
			FrameType: "DARK", SetTempAbsent: tc.absent, State: model.StateVerified}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("UpsertFrame: %v", err)
		}
		got, ok, err := s.GetFrame(ctx, "rig-01", "EMMC Images", tc.path)
		if err != nil || !ok {
			t.Fatalf("GetFrame: ok=%v err=%v", ok, err)
		}
		if got.SetTempAbsent != tc.absent || got.SetTemp != 0 {
			t.Fatalf("%s after upsert: absent=%v set_temp=%v, want absent=%v set_temp=0", tc.path, got.SetTempAbsent, got.SetTemp, tc.absent)
		}
		// UpdateFrame writes the column too; flip it and read it back.
		got.SetTempAbsent = !tc.absent
		if err := s.UpdateFrame(ctx, got); err != nil {
			t.Fatalf("UpdateFrame: %v", err)
		}
		again, _, _ := s.GetFrame(ctx, "rig-01", "EMMC Images", tc.path)
		if again.SetTempAbsent != !tc.absent {
			t.Fatalf("%s after update: absent=%v, want %v", tc.path, again.SetTempAbsent, !tc.absent)
		}
	}
}

func TestUpsertFrameRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	in := &model.Frame{
		RigID:       "rig-01",
		SourceShare: "Udisk Images",
		SourcePath:  "Autorun/light_001.fit",
		SHA256:      "abc123",
		Size:        120_000_000,
		Mtime:       1_700_000_000,
		FrameType:   "LIGHT",
		Target:      "C-2025 A6",
		Filter:      "HA",
		Exptime:     300.5,
		SetTemp:     -10,
		Gain:        100,
		Offset:      30,
		Binning:     "1x1",
		Sensor:      "6200MM",
		DateObs:     "2026-07-15T02:00:00",
		Night:       2,
		DestPath:    "C-2025 A6/FRA300/Summer2026/NIGHT_2/light_001.fit",
		Sidecars: []model.Sidecar{
			{Path: "Autorun/light_001_thn.jpg", Size: 4096, Mtime: 1_700_000_001},
		},
		State:       model.StateVerified,
		Attempts:    1,
		NextRetryAt: 0,
		PulledAt:    1_700_000_100,
		DeletedAt:   0,
		Error:       "",
	}
	if err := s.UpsertFrame(ctx, in); err != nil {
		t.Fatalf("UpsertFrame: %v", err)
	}
	if in.ID == 0 {
		t.Fatal("expected ID assigned on insert")
	}

	got, ok, err := s.GetFrame(ctx, "rig-01", "Udisk Images", "Autorun/light_001.fit")
	if err != nil || !ok {
		t.Fatalf("GetFrame: ok=%v err=%v", ok, err)
	}
	if got.SHA256 != "abc123" || got.Night != 2 || got.Target != "C-2025 A6" {
		t.Errorf("scalar mismatch: %+v", got)
	}
	if len(got.Sidecars) != 1 || got.Sidecars[0].Path != "Autorun/light_001_thn.jpg" {
		t.Errorf("sidecars round-trip failed: %+v", got.Sidecars)
	}
	if got.PulledAt != 1_700_000_100 {
		t.Errorf("pulled_at mismatch: %d", got.PulledAt)
	}

	// Update via the unique key: same share/path, changed content.
	in2 := *in
	in2.ID = 0 // upsert must find by unique key regardless of id
	in2.SHA256 = "def456"
	in2.State = model.StateDeleted
	in2.DeletedAt = 1_700_000_200
	in2.Night = 0 // clears to NULL
	if err := s.UpsertFrame(ctx, &in2); err != nil {
		t.Fatalf("UpsertFrame update: %v", err)
	}
	got2, _, err := s.GetFrame(ctx, "rig-01", "Udisk Images", "Autorun/light_001.fit")
	if err != nil {
		t.Fatalf("GetFrame after update: %v", err)
	}
	if got2.SHA256 != "def456" || got2.State != model.StateDeleted {
		t.Errorf("update not applied: %+v", got2)
	}
	if got2.Night != 0 {
		t.Errorf("night should map back from NULL to 0, got %d", got2.Night)
	}
	if got2.DeletedAt != 1_700_000_200 {
		t.Errorf("deleted_at mismatch: %d", got2.DeletedAt)
	}
	if got2.ID != got.ID {
		t.Errorf("upsert should keep same row id: was %d now %d", got.ID, got2.ID)
	}
}

func TestGetFrameMissing(t *testing.T) {
	s := newStore(t)
	_, ok, err := s.GetFrame(context.Background(), "nope", "share", "path")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("expected not found")
	}
}

func TestNullableMappingUnset(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// A frame with all nullable-sentinel fields at 0 must store NULL and read back 0.
	f := &model.Frame{
		RigID: "r", SourceShare: "s", SourcePath: "p",
		Size: 1, Mtime: 2, State: model.StateFailed,
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("UpsertFrame: %v", err)
	}
	got, _, err := s.GetFrameByID(ctx, f.ID)
	if err != nil {
		t.Fatalf("GetFrameByID: %v", err)
	}
	if got.Night != 0 || got.PulledAt != 0 || got.DeletedAt != 0 || got.NextRetryAt != 0 {
		t.Errorf("expected 0 sentinels, got %+v", got)
	}
	if len(got.Sidecars) != 0 {
		t.Errorf("expected no sidecars, got %+v", got.Sidecars)
	}
}

func TestListFramesPaginationAndFilter(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		state := model.StateVerified
		if i%5 == 0 {
			state = model.StateFailed
		}
		f := &model.Frame{
			RigID: "rig-01", SourceShare: "Udisk Images",
			SourcePath: fmt.Sprintf("Autorun/f_%03d.fit", i),
			Size:       1, Mtime: 1, State: state,
			DestPath: fmt.Sprintf("d/%03d.fit", i),
		}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// A frame from another rig must not leak into rig-01 results.
	if err := s.UpsertFrame(ctx, &model.Frame{RigID: "rig-02", SourceShare: "x", SourcePath: "y", Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "z"}); err != nil {
		t.Fatal(err)
	}

	page0, total, err := s.ListFrames(ctx, "rig-01", "", "", 0, 10)
	if err != nil {
		t.Fatalf("ListFrames: %v", err)
	}
	if total != 25 {
		t.Errorf("total = %d, want 25", total)
	}
	if len(page0) != 10 {
		t.Errorf("page0 len = %d, want 10", len(page0))
	}
	page2, _, _ := s.ListFrames(ctx, "rig-01", "", "", 2, 10)
	if len(page2) != 5 {
		t.Errorf("page2 len = %d, want 5", len(page2))
	}

	// State filter: 5 failed (i = 0,5,10,15,20).
	failed, ftotal, err := s.ListFrames(ctx, "rig-01", string(model.StateFailed), "", 0, 100)
	if err != nil {
		t.Fatalf("ListFrames failed: %v", err)
	}
	if ftotal != 5 || len(failed) != 5 {
		t.Errorf("failed filter: total=%d len=%d, want 5/5", ftotal, len(failed))
	}
	for _, f := range failed {
		if f.State != model.StateFailed {
			t.Errorf("state filter leaked %s", f.State)
		}
	}
}

func TestRecentFrames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		f := &model.Frame{
			RigID: "rig-01", SourceShare: "Udisk Images",
			SourcePath: fmt.Sprintf("Autorun/f_%03d.fit", i),
			Size:       1, Mtime: 1, State: model.StateVerified,
			DestPath: fmt.Sprintf("d/%03d.fit", i),
		}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// Another rig's frames must not leak in.
	if err := s.UpsertFrame(ctx, &model.Frame{RigID: "rig-02", SourceShare: "x", SourcePath: "y", Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "z"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentFrames(ctx, "rig-01", 3)
	if err != nil {
		t.Fatalf("RecentFrames: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (limit honored)", len(got))
	}
	// Newest first: the last-inserted frame (f_009) leads.
	if got[0].SourcePath != "Autorun/f_009.fit" || got[2].SourcePath != "Autorun/f_007.fit" {
		t.Errorf("expected newest-first, got %s then … %s", got[0].SourcePath, got[2].SourcePath)
	}
	for _, f := range got {
		if f.RigID != "rig-01" {
			t.Errorf("rig leaked: %s", f.RigID)
		}
	}
}

func TestListTransferHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// rig-01: three pulled frames at increasing times, plus one never pulled.
	seed := []*model.Frame{
		{RigID: "rig-01", SourceShare: "s", SourcePath: "a", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 1000, DestPath: "a"},
		{RigID: "rig-01", SourceShare: "s", SourcePath: "b", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: "b"},
		{RigID: "rig-01", SourceShare: "s", SourcePath: "c", Size: 1, Mtime: 1, State: model.StateDeleted, PulledAt: 3000, DestPath: "c"},
		// Never pulled (failed transfer) → excluded from history.
		{RigID: "rig-01", SourceShare: "s", SourcePath: "never", Size: 1, Mtime: 1, State: model.StateFailed, PulledAt: 0, DestPath: ""},
		// A second rig, pulled — spans the fleet when rigID is "".
		{RigID: "rig-02", SourceShare: "s", SourcePath: "z", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 2500, DestPath: "z"},
	}
	for _, f := range seed {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	// All rigs, no window: 4 pulled frames (never-pulled excluded), newest id first.
	all, total, err := s.ListTransferHistory(ctx, "", 0, 0, 0, 50)
	if err != nil {
		t.Fatalf("ListTransferHistory: %v", err)
	}
	if total != 4 || len(all) != 4 {
		t.Fatalf("all history: total=%d len=%d, want 4/4", total, len(all))
	}
	// Newest-first is id DESC; rig-02/z was inserted last.
	if all[0].SourcePath != "z" {
		t.Errorf("expected newest (id DESC) first, got %s", all[0].SourcePath)
	}
	for _, f := range all {
		if f.SourcePath == "never" {
			t.Error("never-pulled frame leaked into history")
		}
	}

	// Rig filter: only rig-01's three pulled frames.
	r1, r1total, err := s.ListTransferHistory(ctx, "rig-01", 0, 0, 0, 50)
	if err != nil {
		t.Fatalf("ListTransferHistory rig: %v", err)
	}
	if r1total != 3 || len(r1) != 3 {
		t.Errorf("rig-01 history: total=%d len=%d, want 3/3", r1total, len(r1))
	}
	for _, f := range r1 {
		if f.RigID != "rig-01" {
			t.Errorf("rig filter leaked %s", f.RigID)
		}
	}

	// Date window is inclusive on both bounds: [2000, 2500] → b (2000) and z (2500).
	win, wtotal, err := s.ListTransferHistory(ctx, "", 2000, 2500, 0, 50)
	if err != nil {
		t.Fatalf("ListTransferHistory window: %v", err)
	}
	if wtotal != 2 || len(win) != 2 {
		t.Errorf("window history: total=%d len=%d, want 2/2", wtotal, len(win))
	}

	// Open-ended lower bound: from=3000 → only c (3000), inclusive.
	from3, f3total, _ := s.ListTransferHistory(ctx, "", 3000, 0, 0, 50)
	if f3total != 1 || len(from3) != 1 || from3[0].SourcePath != "c" {
		t.Errorf("from=3000 history: total=%d rows=%v, want just c", f3total, from3)
	}

	// Pagination: page size 2 over 4 rows → page 0 has 2, page 1 has 2, total stays 4.
	p0, ptotal, _ := s.ListTransferHistory(ctx, "", 0, 0, 0, 2)
	p1, _, _ := s.ListTransferHistory(ctx, "", 0, 0, 1, 2)
	if ptotal != 4 || len(p0) != 2 || len(p1) != 2 {
		t.Errorf("pagination: total=%d p0=%d p1=%d, want 4/2/2", ptotal, len(p0), len(p1))
	}
	if p0[0].ID <= p1[0].ID {
		t.Errorf("expected page 0 to hold newer ids than page 1 (DESC), got p0=%d p1=%d", p0[0].ID, p1[0].ID)
	}
}

func TestDeletionEligible(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const cutoff = 1000

	seed := []*model.Frame{
		// eligible: verified, sha present, pulled_at <= cutoff
		{RigID: "r", SourceShare: "s", SourcePath: "eligible", Size: 1, Mtime: 1, State: model.StateVerified, SHA256: "h", PulledAt: 500, DestPath: "a"},
		// not eligible: grace not elapsed (pulled_at > cutoff)
		{RigID: "r", SourceShare: "s", SourcePath: "young", Size: 1, Mtime: 1, State: model.StateVerified, SHA256: "h", PulledAt: 1500, DestPath: "b"},
		// not eligible: no sha256
		{RigID: "r", SourceShare: "s", SourcePath: "nosha", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 500, DestPath: "c"},
		// not eligible: wrong state
		{RigID: "r", SourceShare: "s", SourcePath: "held", Size: 1, Mtime: 1, State: model.StateHeld, SHA256: "h", PulledAt: 500, DestPath: "d"},
		// eligible, older (should sort first)
		{RigID: "r", SourceShare: "s", SourcePath: "oldest", Size: 1, Mtime: 1, State: model.StateVerified, SHA256: "h", PulledAt: 100, DestPath: "e"},
	}
	for _, f := range seed {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.DeletionEligible(ctx, "r", cutoff)
	if err != nil {
		t.Fatalf("DeletionEligible: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 eligible, got %d: %+v", len(got), got)
	}
	if got[0].SourcePath != "oldest" || got[1].SourcePath != "eligible" {
		t.Errorf("expected oldest-first ordering, got %s then %s", got[0].SourcePath, got[1].SourcePath)
	}
}

// design library-reconciliation I37 (AC1b): DeletionEligible ANDs rig_id, grace, and
// sha256 across BOTH state branches, so no cross-rig or pre-grace unclassified frame
// is ever returned — the SQL-precedence hazard the wrapped state disjunction guards
// against. Only a same-rig, past-grace, culled unclassified frame (plus verified
// frames) is eligible.
func TestDeletionEligibleScopesUnclassifiedByRigAndGrace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const cutoff = 1000

	mk := func(rig, path string, state model.FrameState, pulled int64, missing bool) int64 {
		f := &model.Frame{RigID: rig, SourceShare: "s", SourcePath: path, Size: 1, Mtime: 1,
			State: state, SHA256: "h", PulledAt: pulled, DestPath: path}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
		if missing {
			if err := s.SetFrameMissing(ctx, f.ID, 500); err != nil {
				t.Fatalf("set missing %s: %v", path, err)
			}
		}
		return f.ID
	}

	wantID := mk("r", "unc_culled", model.StateUnclassified, 500, true) // eligible
	mk("r", "unc_notculled", model.StateUnclassified, 500, false)       // no mark → excluded
	mk("r", "unc_young", model.StateUnclassified, 1500, true)           // inside grace → excluded
	mk("other", "unc_otherrig", model.StateUnclassified, 500, true)     // different rig → excluded
	verifiedID := mk("r", "verified", model.StateVerified, 500, false)  // verified still eligible

	got, err := s.DeletionEligible(ctx, "r", cutoff)
	if err != nil {
		t.Fatalf("DeletionEligible: %v", err)
	}
	gotIDs := map[int64]string{}
	for _, f := range got {
		gotIDs[f.ID] = f.SourcePath
	}
	if _, ok := gotIDs[wantID]; !ok {
		t.Fatalf("the in-rig, past-grace, culled unclassified frame must be eligible; got %+v", gotIDs)
	}
	if _, ok := gotIDs[verifiedID]; !ok {
		t.Fatalf("a verified frame must remain eligible; got %+v", gotIDs)
	}
	if len(got) != 2 {
		t.Fatalf("only the culled-unclassified and the verified frame are eligible; got %d: %+v", len(got), gotIDs)
	}
}

func TestUnclassifiedAndFailedRetry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed := []*model.Frame{
		{RigID: "r", SourceShare: "s", SourcePath: "u1", Size: 1, Mtime: 1, State: model.StateUnclassified},
		{RigID: "r", SourceShare: "s", SourcePath: "f_due_null", Size: 1, Mtime: 1, State: model.StateFailed, NextRetryAt: 0},
		{RigID: "r", SourceShare: "s", SourcePath: "f_due_past", Size: 1, Mtime: 1, State: model.StateFailed, NextRetryAt: 500},
		{RigID: "r", SourceShare: "s", SourcePath: "f_future", Size: 1, Mtime: 1, State: model.StateFailed, NextRetryAt: 5000},
	}
	for _, f := range seed {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	unc, err := s.UnclassifiedFrames(ctx, "r")
	if err != nil || len(unc) != 1 {
		t.Fatalf("UnclassifiedFrames: len=%d err=%v", len(unc), err)
	}
	due, err := s.FailedDueForRetry(ctx, "r", 1000)
	if err != nil {
		t.Fatalf("FailedDueForRetry: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("expected 2 due, got %d: %+v", len(due), due)
	}
}

func TestSetStateAndUpdateFrame(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	f := &model.Frame{RigID: "r", SourceShare: "s", SourcePath: "p", Size: 1, Mtime: 1, State: model.StateFailed, Attempts: 1}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(ctx, f.ID, model.StateVerified); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, _, _ := s.GetFrameByID(ctx, f.ID)
	if got.State != model.StateVerified {
		t.Errorf("SetState failed: %s", got.State)
	}

	f.State = model.StateHeld
	f.Attempts = 3
	f.Error = "mismatch"
	if err := s.UpdateFrame(ctx, f); err != nil {
		t.Fatalf("UpdateFrame: %v", err)
	}
	got2, _, _ := s.GetFrameByID(ctx, f.ID)
	if got2.State != model.StateHeld || got2.Attempts != 3 || got2.Error != "mismatch" {
		t.Errorf("UpdateFrame not applied: %+v", got2)
	}
}

func TestCountsDedupeByDestPath(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// Same dest_path on two shares (H12: USB + eMMC) → counts once.
	shared := "C-2025 A6/NIGHT_1/light_007.fit"
	seed := []*model.Frame{
		{RigID: "r", SourceShare: "Udisk Images", SourcePath: "Autorun/light_007.fit", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: shared},
		{RigID: "r", SourceShare: "EMMC Images", SourcePath: "Autorun/light_007.fit", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: shared},
		{RigID: "r", SourceShare: "Udisk Images", SourcePath: "Autorun/light_008.fit", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: "other.fit"},
		// deleted → not resident, but was pulled tonight
		{RigID: "r", SourceShare: "Udisk Images", SourcePath: "Autorun/old.fit", Size: 1, Mtime: 1, State: model.StateDeleted, PulledAt: 2000, DestPath: "old.fit"},
	}
	for _, f := range seed {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	tonight, err := s.CountFramesTonight(ctx, "r", 1000)
	if err != nil {
		t.Fatalf("CountFramesTonight: %v", err)
	}
	// distinct dest_paths pulled >= 1000: shared, other.fit, old.fit = 3
	if tonight != 3 {
		t.Errorf("CountFramesTonight = %d, want 3 (deduped)", tonight)
	}
	resident, err := s.ResidentCount(ctx, "r")
	if err != nil {
		t.Fatalf("ResidentCount: %v", err)
	}
	// resident distinct dest_paths: shared (once), other.fit = 2 (deleted excluded)
	if resident != 2 {
		t.Errorf("ResidentCount = %d, want 2 (deduped, deleted excluded)", resident)
	}
}

func TestResidentLeads(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const share = "EMMC Images"
	seed := []struct {
		rig, share, path string
		state            model.FrameState
	}{
		{"r", share, "Autorun/Light/M31/a.fit", model.StateVerified},
		{"r", share, "Autorun/Light/M42/b.fit", model.StateUnclassified}, // same lead, once
		{"r", "emmc images", "Autorun/Light/M42/c.fit", model.StateHeld}, // share in another case
		{"r", share, "ASIAIR/Autorun/d.fit", model.StateFailed},
		{"r", share, "Plan/e.fit", model.StateVerified},
		{"r", share, "top.fit", model.StateVerified},
		{"r", share, "Gone/Light/f.fit", model.StateDeleted},
		{"r", share, "Dismissed/Light/g.fit", model.StateDismissed},
		{"r", "Udisk Images", "Other/h.fit", model.StateVerified},
		{"r2", share, "Elsewhere/i.fit", model.StateVerified},
	}
	for _, sd := range seed {
		f := &model.Frame{RigID: sd.rig, SourceShare: sd.share, SourcePath: sd.path, Size: 1, Mtime: 1, State: sd.state}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ResidentLeads(ctx, "r", share)
	if err != nil {
		t.Fatalf("ResidentLeads: %v", err)
	}
	want := []string{"ASIAIR/Autorun", "Autorun/Light", "Plan", "top.fit"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ResidentLeads = %q, want %q", got, want)
	}
	if got, err := s.ResidentLeads(ctx, "r", "Nowhere"); err != nil || len(got) != 0 {
		t.Errorf("ResidentLeads on a share with no rows = %q, %v; want none", got, err)
	}
}

func TestBytesPulledSince(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed := []*model.Frame{
		// Same dest_path on two shares (H12: USB + eMMC) → its bytes count once.
		{RigID: "r", SourceShare: "Udisk Images", SourcePath: "Autorun/a.fit", Size: 1000, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: "night/a.fit"},
		{RigID: "r", SourceShare: "EMMC Images", SourcePath: "Autorun/a.fit", Size: 1000, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: "night/a.fit"},
		// A second pulled frame, on a different rig — the total is fleet-wide.
		{RigID: "r2", SourceShare: "Udisk Images", SourcePath: "Autorun/b.fit", Size: 500, Mtime: 1, State: model.StateUnclassified, PulledAt: 2500, DestPath: "night/b.fit"},
		// Pulled before the window → excluded.
		{RigID: "r", SourceShare: "Udisk Images", SourcePath: "Autorun/c.fit", Size: 9999, Mtime: 1, State: model.StateVerified, PulledAt: 500, DestPath: "night/c.fit"},
		// Failed transfer with no NAS copy (pulled_at unset) → excluded.
		{RigID: "r", SourceShare: "Udisk Images", SourcePath: "Autorun/d.fit", Size: 7777, Mtime: 1, State: model.StateFailed, PulledAt: 0, DestPath: ""},
	}
	for _, f := range seed {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.BytesPulledSince(ctx, 1000)
	if err != nil {
		t.Fatalf("BytesPulledSince: %v", err)
	}
	// deduped a.fit (1000) + b.fit (500); c.fit before window, d.fit never pulled.
	if want := int64(1500); got != want {
		t.Errorf("BytesPulledSince = %d, want %d", got, want)
	}

	// Empty window → 0, never an error.
	if got, err := s.BytesPulledSince(ctx, 1_000_000); err != nil || got != 0 {
		t.Errorf("BytesPulledSince(future) = %d, %v; want 0, nil", got, err)
	}
}

func TestAssignNightDeterminismAndStability(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Same date → same ordinal, repeatedly.
	for i := 0; i < 5; i++ {
		n, err := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15"})
		if err != nil {
			t.Fatalf("AssignNight: %v", err)
		}
		if n != 1 {
			t.Errorf("call %d: night = %d, want 1", i, n)
		}
	}
	// New date → next ordinal.
	n2, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-16"})
	if n2 != 2 {
		t.Errorf("second date night = %d, want 2", n2)
	}
	// Different OTA is an independent scope → restarts at 1.
	nOther, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "RASA8", ObservingDate: "2026-07-16"})
	if nOther != 1 {
		t.Errorf("different OTA night = %d, want 1", nOther)
	}
	// Different target likewise.
	nTarget, _ := s.AssignNight(ctx, NightRequest{Target: "M42", OTA: "FRA300", ObservingDate: "2026-07-16"})
	if nTarget != 1 {
		t.Errorf("different target night = %d, want 1", nTarget)
	}

	// An earlier date arriving late takes the NEXT free ordinal, never renumbering.
	nLate, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-14"})
	if nLate != 3 {
		t.Errorf("late-arriving earlier date = %d, want 3 (never renumber)", nLate)
	}
	// The original assignments are unchanged.
	nRecheck, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15"})
	if nRecheck != 1 {
		t.Errorf("original date renumbered to %d, want stable 1", nRecheck)
	}

	// Consistency under a tight loop of repeated calls across scopes.
	for i := 0; i < 50; i++ {
		if n, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15"}); n != 1 {
			t.Fatalf("loop iter %d: night drifted to %d", i, n)
		}
	}
}

func TestRebuildNightCounters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	entries := []NightEntry{
		{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15", Night: 1},
		{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-16", Night: 2},
		{Target: "M42", OTA: "FRA300", ObservingDate: "2026-07-16", Night: 1},
	}
	if err := s.RebuildNightCounters(ctx, entries); err != nil {
		t.Fatalf("RebuildNightCounters: %v", err)
	}
	// Existing ordinals are preserved (never renumbered), and a subsequent assign for
	// an existing date returns the rebuilt value.
	if n, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-16"}); n != 2 {
		t.Errorf("rebuilt ordinal not preserved: %d, want 2", n)
	}
	// A new date after rebuild continues the sequence from max+1.
	if n, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-17"}); n != 3 {
		t.Errorf("post-rebuild new date = %d, want 3", n)
	}
	// Rebuild is idempotent — re-running preserves ordinals.
	if err := s.RebuildNightCounters(ctx, entries); err != nil {
		t.Fatalf("RebuildNightCounters (2nd): %v", err)
	}
	if n, _ := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15"}); n != 1 {
		t.Errorf("idempotent rebuild changed ordinal: %d, want 1", n)
	}
}

func TestAlertsRaiseAckUnacked(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id1, _, err := s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityWarn, Category: model.AlertBacklog, Message: "backlog growing", CreatedAt: 100})
	if err != nil {
		t.Fatalf("RaiseAlert: %v", err)
	}
	// Identical unacked alert is de-duplicated → same id, no new row.
	id2, _, err := s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityWarn, Category: model.AlertBacklog, Message: "backlog growing", CreatedAt: 200})
	if err != nil {
		t.Fatalf("RaiseAlert dup: %v", err)
	}
	if id1 != id2 {
		t.Errorf("expected dedup to reuse id %d, got %d", id1, id2)
	}
	// A different message is a distinct alert.
	if _, _, err := s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityCritical, Category: model.AlertNASDiskLow, Message: "disk low", CreatedAt: 300}); err != nil {
		t.Fatal(err)
	}
	// A fleet-wide alert (empty rig id → NULL).
	if _, _, err := s.RaiseAlert(ctx, model.Alert{Severity: model.SeverityInfo, Category: model.AlertConfig, Message: "cfg", CreatedAt: 400}); err != nil {
		t.Fatal(err)
	}

	n, err := s.UnackedCount(ctx)
	if err != nil {
		t.Fatalf("UnackedCount: %v", err)
	}
	if n != 3 {
		t.Errorf("UnackedCount = %d, want 3", n)
	}

	unacked, err := s.ListAlerts(ctx, false, 0)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(unacked) != 3 {
		t.Errorf("ListAlerts(unacked) len = %d, want 3", len(unacked))
	}
	// Newest first.
	if unacked[0].Message != "cfg" {
		t.Errorf("expected newest first, got %q", unacked[0].Message)
	}

	if err := s.AckAlert(ctx, id1, 500); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if n, _ := s.UnackedCount(ctx); n != 2 {
		t.Errorf("after ack UnackedCount = %d, want 2", n)
	}
	all, _ := s.ListAlerts(ctx, true, 0)
	if len(all) != 3 {
		t.Errorf("ListAlerts(includeAcked) len = %d, want 3", len(all))
	}
	// After acking, the same alert can be raised anew (dedup only skips unacked).
	id3, _, _ := s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityWarn, Category: model.AlertBacklog, Message: "backlog growing", CreatedAt: 600})
	if id3 == id1 {
		t.Error("expected a new alert row after prior was acked")
	}
}

func TestLogPullsRoundTripAndSizeChange(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	l := LogPull{RigID: "r", LogType: "autorun", SourcePath: "log/autorun.log", DestPath: "D:/logs/autorun.log", Size: 1024, PulledAt: 100}
	if err := s.RecordLogPull(ctx, l); err != nil {
		t.Fatalf("RecordLogPull: %v", err)
	}
	got, ok, err := s.GetLogPull(ctx, "r", "log/autorun.log")
	if err != nil || !ok {
		t.Fatalf("GetLogPull: ok=%v err=%v", ok, err)
	}
	if got.Size != 1024 || got.DestPath != "D:/logs/autorun.log" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Re-pull with a larger size overwrites, not duplicates.
	l.Size = 2048
	l.PulledAt = 200
	if err := s.RecordLogPull(ctx, l); err != nil {
		t.Fatalf("RecordLogPull re-pull: %v", err)
	}
	got2, _, _ := s.GetLogPull(ctx, "r", "log/autorun.log")
	if got2.Size != 2048 || got2.PulledAt != 200 {
		t.Errorf("size-changed re-pull not applied: %+v", got2)
	}
	list, err := s.ListLogPulls(ctx, "r")
	if err != nil {
		t.Fatalf("ListLogPulls: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 log pull row after re-pull, got %d", len(list))
	}

	_, ok, _ = s.GetLogPull(ctx, "r", "log/missing.log")
	if ok {
		t.Error("expected missing log pull to report not found")
	}
}

func TestPruneDeletedBatching(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// 17 old deleted frames + a few that must survive.
	for i := 0; i < 17; i++ {
		f := &model.Frame{RigID: "r", SourceShare: "s", SourcePath: fmt.Sprintf("del_%d", i), Size: 1, Mtime: 1, State: model.StateDeleted, DeletedAt: 100, DestPath: fmt.Sprintf("d%d", i)}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	// Survivors: too recent, or not deleted.
	if err := s.UpsertFrame(ctx, &model.Frame{RigID: "r", SourceShare: "s", SourcePath: "recent", Size: 1, Mtime: 1, State: model.StateDeleted, DeletedAt: 9999, DestPath: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFrame(ctx, &model.Frame{RigID: "r", SourceShare: "s", SourcePath: "verified", Size: 1, Mtime: 1, State: model.StateVerified, DeletedAt: 0, DestPath: "r2"}); err != nil {
		t.Fatal(err)
	}

	// Batch of 5 forces the loop to run several times (17 → 5+5+5+2).
	deleted, err := s.PruneDeleted(ctx, 1000, 5)
	if err != nil {
		t.Fatalf("PruneDeleted: %v", err)
	}
	if deleted != 17 {
		t.Errorf("PruneDeleted = %d, want 17", deleted)
	}
	_, total, _ := s.ListFrames(ctx, "r", "", "", 0, 100)
	if total != 2 {
		t.Errorf("survivors = %d, want 2", total)
	}
	// Second prune finds nothing.
	again, _ := s.PruneDeleted(ctx, 1000, 5)
	if again != 0 {
		t.Errorf("second PruneDeleted = %d, want 0", again)
	}
}

func TestPruneAckedAlerts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		id, _, err := s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityInfo, Category: model.AlertConfig, Message: fmt.Sprintf("m%d", i), CreatedAt: 100})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AckAlert(ctx, id, 200); err != nil {
			t.Fatal(err)
		}
	}
	// One recent acked (survives) and one unacked (survives).
	id, _, _ := s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityInfo, Category: model.AlertConfig, Message: "recent", CreatedAt: 100})
	s.AckAlert(ctx, id, 99999)
	s.RaiseAlert(ctx, model.Alert{RigID: "r", Severity: model.SeverityInfo, Category: model.AlertConfig, Message: "live", CreatedAt: 100})

	pruned, err := s.PruneAckedAlerts(ctx, 1000, 3)
	if err != nil {
		t.Fatalf("PruneAckedAlerts: %v", err)
	}
	if pruned != 7 {
		t.Errorf("PruneAckedAlerts = %d, want 7", pruned)
	}
	all, _ := s.ListAlerts(ctx, true, 0)
	if len(all) != 2 {
		t.Errorf("survivors = %d, want 2", len(all))
	}
}

func TestMigrationIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	ctx := context.Background()
	if err := s1.UpsertFrame(ctx, &model.Frame{RigID: "r", SourceShare: "s", SourcePath: "p", Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}

	// Re-open the same file: migrations must be a no-op and data must survive.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2 (re-migrate): %v", err)
	}
	defer s2.Close()
	_, total, err := s2.ListFrames(ctx, "r", "", "", 0, 10)
	if err != nil {
		t.Fatalf("ListFrames after reopen: %v", err)
	}
	if total != 1 {
		t.Errorf("data lost across reopen: total = %d, want 1", total)
	}
}

func TestRefuseNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force a schema_version above what this binary understands.
	cs := s.(*sqlStore)
	if _, err := cs.write.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, model.CurrentStoreSchemaVersion+1); err != nil {
		t.Fatalf("bump schema_version: %v", err)
	}
	s.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("expected Open to refuse a newer schema_version")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error should mention a newer version, got: %v", err)
	}
}

func TestCheckpoint(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.UpsertFrame(ctx, &model.Frame{RigID: "r", SourceShare: "s", SourcePath: "p", Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
}

func TestStartCheckpointLoopStopsOnContext(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.StartCheckpointLoop(ctx, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartCheckpointLoop did not return on context cancel")
	}
}

// --- astrometry ----------------------------------------------------------------

func astroFrame(path string, a *model.Astrometry) *model.Frame {
	return &model.Frame{
		RigID: "rig-astro", SourceShare: "Udisk", SourcePath: path,
		Size: 1, Mtime: 1, FrameType: "LIGHT", State: model.StateVerified,
		DestPath: "/nas/" + path, Astrometry: a,
	}
}

func TestAstrometryRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	a := &model.Astrometry{
		Status: model.AstroStatusHeader,
		Solution: &model.WCSSolution{
			CenterRA: 180.25, CenterDec: -12.5, PositionAngle: 91.3,
			PixelScale: 1.23, FOVWidth: 1.5, FOVHeight: 1.0, Flipped: true,
		},
	}
	in := astroFrame("light_001.fit", a)
	if err := s.UpsertFrame(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetFrameByID(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID: ok=%v err=%v", ok, err)
	}
	if got.Astrometry == nil || got.Astrometry.Solution == nil {
		t.Fatalf("astrometry lost across round-trip: %+v", got.Astrometry)
	}
	if *got.Astrometry.Solution != *a.Solution || got.Astrometry.Status != a.Status {
		t.Errorf("astrometry mismatch:\n got %+v\nwant %+v", got.Astrometry, a)
	}
}

func TestAstrometryNilStoresNull(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	in := astroFrame("legacy_001.fit", nil)
	if err := s.UpsertFrame(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.GetFrameByID(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Astrometry != nil {
		t.Errorf("expected nil Astrometry for a never-processed frame, got %+v", got.Astrometry)
	}
	// A NULL-status eligible frame is exactly the backfill queue.
	need, err := s.ListFramesNeedingAstrometry(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 || need[0].ID != in.ID {
		t.Fatalf("ListFramesNeedingAstrometry = %d rows, want the one NULL frame", len(need))
	}
}

func TestListFramesNeedingAstrometryExcludesProcessed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	pending := astroFrame("pending.fit", nil)
	header := astroFrame("header.fit", &model.Astrometry{Status: model.AstroStatusHeader,
		Solution: &model.WCSSolution{CenterRA: 10}})
	none := astroFrame("none.fit", &model.Astrometry{Status: model.AstroStatusNone})
	for _, f := range []*model.Frame{pending, header, none} {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	need, err := s.ListFramesNeedingAstrometry(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 || need[0].ID != pending.ID {
		t.Fatalf("queue = %+v, want only the pending frame", ids(need))
	}
}

func TestSetFrameAstrometry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	in := astroFrame("solve_me.fit", nil)
	if err := s.UpsertFrame(ctx, in); err != nil {
		t.Fatal(err)
	}
	a := &model.Astrometry{Status: model.AstroStatusHeader,
		Solution: &model.WCSSolution{CenterRA: 42, CenterDec: 7}}
	if err := s.SetFrameAstrometry(ctx, in.ID, a); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetFrameByID(ctx, in.ID)
	if got.Astrometry == nil || got.Astrometry.Solution.CenterRA != 42 {
		t.Fatalf("SetFrameAstrometry not persisted: %+v", got.Astrometry)
	}
}

func TestSetFrameAstrometryIfPendingGuards(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// A frame that already has a header solution must not be overwritten by the
	// pending-guarded writer (simulating a live re-pull winning the race).
	solved := astroFrame("already.fit", &model.Astrometry{Status: model.AstroStatusHeader,
		Solution: &model.WCSSolution{CenterRA: 100}})
	if err := s.UpsertFrame(ctx, solved); err != nil {
		t.Fatal(err)
	}
	other := &model.Astrometry{Status: model.AstroStatusNone}
	if err := s.SetFrameAstrometryIfPending(ctx, solved.ID, other); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetFrameByID(ctx, solved.ID)
	if got.Astrometry == nil || got.Astrometry.Solution == nil || got.Astrometry.Solution.CenterRA != 100 {
		t.Errorf("pending guard failed: solution was overwritten: %+v", got.Astrometry)
	}

	// A NULL frame is promoted.
	pending := astroFrame("pending.fit", nil)
	if err := s.UpsertFrame(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFrameAstrometryIfPending(ctx, pending.ID, other); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetFrameByID(ctx, pending.ID)
	if got.Astrometry == nil || got.Astrometry.Status != model.AstroStatusNone {
		t.Errorf("pending frame not promoted: %+v", got.Astrometry)
	}
}

func TestUpsertNilAstrometryPreservesSolution(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// A solved frame, then a re-pull that carries no astrometry (markFailed /
	// reclassify build frames without it). COALESCE must keep the solution.
	solved := astroFrame("science.fit", &model.Astrometry{Status: model.AstroStatusHeader,
		Solution: &model.WCSSolution{CenterRA: 200, CenterDec: -30}})
	if err := s.UpsertFrame(ctx, solved); err != nil {
		t.Fatal(err)
	}
	repull := astroFrame("science.fit", nil) // same UNIQUE key, nil astrometry
	repull.State = model.StateFailed
	if err := s.UpsertFrame(ctx, repull); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetFrameByID(ctx, solved.ID)
	if got.Astrometry == nil || got.Astrometry.Solution == nil || got.Astrometry.Solution.CenterRA != 200 {
		t.Errorf("nil-astrometry re-pull erased the solution: %+v", got.Astrometry)
	}
}

func ids(frames []model.Frame) []int64 {
	out := make([]int64, len(frames))
	for i, f := range frames {
		out[i] = f.ID
	}
	return out
}

// TestUpgradeV1ToV2 pins the real upgrade path for existing installs: a v1 DB (no
// astrometry columns) opens, migrate2 runs, and astrometry round-trips afterward
// with the pre-existing row preserved.
func TestUpgradeV1ToV2(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	// Build a v1 DB by hand: run migrate1's DDL and stamp schema_version=1.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate1(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version','1')`); err != nil {
		t.Fatal(err)
	}
	// A legacy frame with no astrometry columns available yet.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO frames(rig_id,source_share,source_path,size,mtime,state,dest_path)
		 VALUES('rig','Udisk','old.fit',1,1,'verified','/nas/old.fit')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Open through the store: migrate2 must add the columns and index.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer s.Close()

	// The legacy row is now in the backfill queue (wcs_status NULL).
	need, err := s.ListFramesNeedingAstrometry(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 || need[0].SourcePath != "old.fit" {
		t.Fatalf("expected the legacy frame in the queue, got %+v", need)
	}
	// And astrometry can be written to it.
	a := &model.Astrometry{Status: model.AstroStatusHeader, Solution: &model.WCSSolution{CenterRA: 12}}
	if err := s.SetFrameAstrometryIfPending(ctx, need[0].ID, a); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetFrameByID(ctx, need[0].ID)
	if got.Astrometry == nil || got.Astrometry.Solution.CenterRA != 12 {
		t.Errorf("astrometry not persisted after upgrade: %+v", got.Astrometry)
	}
}

// TestUpgradeV4ToV5RestampsMissingMarks pins the release-two upgrade guarantee
// (design library-reconciliation §9.2): a v4 DB carrying missing marks that are
// already far older than any settle window must have every mark re-stamped to now
// when migrate5 runs, so the first delete pass under the version that CAN delete
// does not authorize the whole backlog at once. Rows with no mark are untouched.
func TestUpgradeV4ToV5RestampsMissingMarks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []func(context.Context, *sql.Tx) error{migrate1, migrate2, migrate3, migrate4} {
		if err := m(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version','4')`); err != nil {
		t.Fatal(err)
	}
	const ancient = 1_000_000 // long before any settle window
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO frames(rig_id,source_share,source_path,size,mtime,state,dest_path,sha256,missing_seen_at)
		 VALUES('rig','Udisk','culled.fit',1,1,'verified','/nas/culled.fit','h',?)`, ancient); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO frames(rig_id,source_share,source_path,size,mtime,state,dest_path,sha256)
		 VALUES('rig','Udisk','present.fit',1,1,'verified','/nas/present.fit','h')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	before := time.Now().Unix()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer s.Close()

	var marked, unmarked sql.NullInt64
	if err := s.(*sqlStore).read.QueryRowContext(ctx,
		`SELECT missing_seen_at FROM frames WHERE source_path='culled.fit'`).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if err := s.(*sqlStore).read.QueryRowContext(ctx,
		`SELECT missing_seen_at FROM frames WHERE source_path='present.fit'`).Scan(&unmarked); err != nil {
		t.Fatal(err)
	}
	if !marked.Valid || marked.Int64 == ancient {
		t.Fatalf("the standing mark must be re-stamped, got %v", marked)
	}
	if marked.Int64 < before {
		t.Fatalf("re-stamped mark %d predates the upgrade (%d)", marked.Int64, before)
	}
	if unmarked.Valid {
		t.Fatalf("a row with no mark must stay unmarked, got %v", unmarked)
	}
}

// --- retarget overlay ----------------------------------------------------------

// TestFreshDBIsSchema11AndHasOverlayColumns pins that a fresh Open lands at schema
// version 11 and that the retarget overlay, mosaic pane and copy-keyed columns exist
// on the frames table. Column PRESENCE is all this proves; the positional coupling
// between frameColumns, scanFrame and UpsertFrame is pinned by
// TestFrameColumnsRoundTripEveryColumn.
func TestFreshDBIsSchema11AndHasOverlayColumns(t *testing.T) {
	ctx := context.Background()
	s := newStore(t).(*sqlStore)

	if model.CurrentStoreSchemaVersion != 11 {
		t.Fatalf("CurrentStoreSchemaVersion = %d, want 11", model.CurrentStoreSchemaVersion)
	}
	v, err := s.schemaVersion(ctx)
	if err != nil {
		t.Fatalf("schemaVersion: %v", err)
	}
	if v != 11 {
		t.Fatalf("fresh DB schema_version = %d, want 11", v)
	}

	cols := map[string]bool{}
	rows, err := s.read.QueryContext(ctx, `SELECT name FROM pragma_table_info('frames')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !cols["retarget_sha256"] || !cols["retarget_from"] {
		t.Fatalf("frames table missing retarget columns; have %v", cols)
	}
	if !cols["panel_base"] || !cols["panel_row"] || !cols["panel_col"] {
		t.Fatalf("frames table missing mosaic pane columns; have %v", cols)
	}
	if !cols["data_sha256"] || !cols["archive_sha256"] || !cols["archive_size"] || !cols["delete_defer_count"] {
		t.Fatalf("frames table missing the copy-keyed columns; have %v", cols)
	}
}

// TestRetargetOverlaySetResetSet exercises the UpsertFrame overlay contract (design
// routing-and-filing §9.2): the overlay round-trips when set; a re-file on the SAME
// natural key with the fields empty RESETS the columns to "" (the differ-repull
// reset); and a later re-file with them set again SETS them (a retarget). The
// behavior must come purely from the excluded= bind pattern, not any CASE.
func TestRetargetOverlaySetResetSet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	base := &model.Frame{
		RigID: "rig-rt", SourceShare: "Udisk", SourcePath: "Autorun/light_007.fit",
		SHA256: "orighash", Size: 100, Mtime: 1, FrameType: "LIGHT", Target: "M31",
		State: model.StateVerified, DestPath: "/lib/M31/light_007.fit",
		RetargetSHA256: "archhash", RetargetFrom: "Moon",
	}
	if err := s.UpsertFrame(ctx, base); err != nil {
		t.Fatalf("upsert (set): %v", err)
	}
	got, ok, err := s.GetFrameByID(ctx, base.ID)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID: ok=%v err=%v", ok, err)
	}
	if got.RetargetSHA256 != "archhash" || got.RetargetFrom != "Moon" {
		t.Fatalf("overlay not round-tripped: sha=%q from=%q", got.RetargetSHA256, got.RetargetFrom)
	}
	if got.SHA256 != "orighash" {
		t.Fatalf("sha256 must stay the original anchor, got %q", got.SHA256)
	}

	// A genuinely-new re-file of the same natural key leaves the overlay fields empty:
	// the columns must RESET to "" (NULL), exactly as a differ re-pull needs.
	reset := *base
	reset.ID = 0
	reset.RetargetSHA256 = ""
	reset.RetargetFrom = ""
	if err := s.UpsertFrame(ctx, &reset); err != nil {
		t.Fatalf("upsert (reset): %v", err)
	}
	got, _, err = s.GetFrameByID(ctx, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetargetSHA256 != "" || got.RetargetFrom != "" {
		t.Fatalf("overlay must reset to empty on a re-file, got sha=%q from=%q", got.RetargetSHA256, got.RetargetFrom)
	}
	if got.ID != base.ID {
		t.Fatalf("re-file must keep the same row id: was %d now %d", base.ID, got.ID)
	}

	// A retarget re-files with the overlay set again: it must SET the columns.
	set := *base
	set.ID = 0
	set.RetargetSHA256 = "arch2"
	set.RetargetFrom = "Jupiter"
	if err := s.UpsertFrame(ctx, &set); err != nil {
		t.Fatalf("upsert (set again): %v", err)
	}
	got, _, err = s.GetFrameByID(ctx, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetargetSHA256 != "arch2" || got.RetargetFrom != "Jupiter" {
		t.Fatalf("overlay must set on a retarget, got sha=%q from=%q", got.RetargetSHA256, got.RetargetFrom)
	}
}

// TestAssignedNightIgnoresFolder (INV-N4) is never-renumber under the folder
// hints: a date that already has a row keeps its ordinal whatever the destination
// folder was observed to hold. The operator has written NIGHT_3 into their notes
// and their stacking scripts.
func TestAssignedNightIgnoresFolder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first, err := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15"})
	if err != nil || first != 1 {
		t.Fatalf("first assign = %d, %v; want 1", first, err)
	}
	again, err := s.AssignNight(ctx, NightRequest{
		Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15",
		Preferred: 7, FolderHighWater: 9,
	})
	if err != nil || again != first {
		t.Fatalf("assigned date renumbered to %d (err %v); want a stable %d", again, err, first)
	}
	if n, _ := s.MaxNight(ctx, "M31", "FRA300"); n != 1 {
		t.Fatalf("MaxNight = %d, want 1 — the discarded hints must not have written a row", n)
	}
}

// TestAssignNightAppliesHintRules drives the two rules the write transaction owns,
// because neither is reproducible through the filing layer: the hints are read on
// another pool and may be stale by the time they arrive.
func TestAssignNightAppliesHintRules(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	assign := func(date string, preferred, high int) int {
		t.Helper()
		n, err := s.AssignNight(ctx, NightRequest{
			Target: "M31", OTA: "FRA300", ObservingDate: date,
			Preferred: preferred, FolderHighWater: high,
		})
		if err != nil {
			t.Fatalf("AssignNight(%s): %v", date, err)
		}
		return n
	}

	// A folder holding this date at 3 is adopted outright, even in a fresh scope.
	if n := assign("2026-07-15", 3, 3); n != 3 {
		t.Fatalf("adopted night = %d, want 3", n)
	}
	// No hint at all: the next free ordinal in the scope.
	if n := assign("2026-07-16", 0, 0); n != 4 {
		t.Fatalf("unhinted night = %d, want 4", n)
	}
	// A high-water mark ABOVE the scope's own maximum lifts the mint: filing on
	// top of an ordinal the destination folder already occupies is the failure the
	// consultation exists to prevent.
	if n := assign("2026-07-17", 0, 9); n != 10 {
		t.Fatalf("night above folder high-water = %d, want 10", n)
	}
	// A Preferred another date in this scope already holds is discarded, and the
	// fallback is max(MAX(night), FolderHighWater)+1 — one ordinal serves at most
	// one date per scope, and a stale hint must not be what breaks that.
	if n := assign("2026-07-18", 3, 12); n != 13 {
		t.Fatalf("stale-hint night = %d, want 13", n)
	}
	if n, _, _ := s.LookupNight(ctx, "M31", "FRA300", "2026-07-15"); n != 3 {
		t.Fatalf("the date holding 3 was renumbered to %d", n)
	}
}

func TestLookupNightAndMaxNight(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if n, ok, err := s.LookupNight(ctx, "M31", "FRA300", "2026-07-15"); err != nil || ok || n != 0 {
		t.Fatalf("miss = (%d,%v,%v); want (0,false,nil)", n, ok, err)
	}
	if n, err := s.MaxNight(ctx, "M31", "FRA300"); err != nil || n != 0 {
		t.Fatalf("MaxNight of an empty scope = (%d,%v); want (0,nil)", n, err)
	}

	if _, err := s.AssignNight(ctx, NightRequest{Target: "M31", OTA: "FRA300", ObservingDate: "2026-07-15"}); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.LookupNight(ctx, "M31", "FRA300", "2026-07-15"); err != nil || !ok || n != 1 {
		t.Fatalf("hit = (%d,%v,%v); want (1,true,nil)", n, ok, err)
	}
	// The scope is (target, ota): another OTA is a different ledger.
	if _, ok, _ := s.LookupNight(ctx, "M31", "RASA8", "2026-07-15"); ok {
		t.Fatal("LookupNight leaked across OTAs")
	}
	if n, _ := s.MaxNight(ctx, "M31", "FRA300"); n != 1 {
		t.Fatalf("MaxNight = %d, want 1", n)
	}

	held, err := s.NightHeldByOtherDate(ctx, "M31", "FRA300", 1, "2026-07-16")
	if err != nil || !held {
		t.Fatalf("NightHeldByOtherDate(other date) = (%v,%v); want true", held, err)
	}
	held, err = s.NightHeldByOtherDate(ctx, "M31", "FRA300", 1, "2026-07-15")
	if err != nil || held {
		t.Fatalf("NightHeldByOtherDate(its own date) = (%v,%v); want false", held, err)
	}
}

// TestNightFolderRangeQueries covers the two index-ranged reads a folder
// consultation is made of, and the bounds they depend on: a sibling directory
// whose name merely starts with the same characters is outside the range, a
// deeper descendant is inside it, and the date window brackets both the
// fractional and the whole-second spellings date_obs is written in.
func TestNightFolderRangeQueries(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	seed := func(id int, dest, target, dateObs string, night int64) {
		t.Helper()
		f := &model.Frame{
			RigID: "rig-01", SourceShare: "emmc", SourcePath: fmt.Sprintf("src/%d.fit", id),
			Size: 1, Mtime: 1, State: model.StateVerified,
			FrameType: string(model.TypeLight), Target: target, DateObs: dateObs,
			Night: night, DestPath: dest,
		}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("seed %d: %v", id, err)
		}
	}
	// One night folder holding a whole-second and a fractional timestamp of the
	// same observing date, one holding the night before, one nested a level deeper,
	// and two decoys: a sibling target sorting just above the range, and a frame
	// with no ordinal at all.
	seed(1, "/nas/M31/NIGHT_1/a.fit", "M31", "2026-07-15T04:00:00Z", 1)
	seed(2, "/nas/M31/NIGHT_1/b.fit", "M 31", "2026-07-15T04:00:00.031444Z", 1)
	seed(3, "/nas/M31/NIGHT_2/c.fit", "M31", "2026-07-16T04:00:00Z", 2)
	seed(4, "/nas/M31/NIGHT_2/HA/d.fit", "M31", "2026-07-16T05:00:00Z", 2)
	seed(5, "/nas/M31_old/NIGHT_9/e.fit", "M31_old", "2026-07-15T04:00:00Z", 9)
	seed(6, "/nas/M31/NIGHT_1/f.fit", "M31", "2026-07-15T04:00:00Z", 0)

	nights, err := s.NightsUnder(ctx, "/nas/M31/", "/nas/M310")
	if err != nil {
		t.Fatalf("NightsUnder: %v", err)
	}
	got := map[int]bool{}
	for _, n := range nights {
		got[n] = true
	}
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("NightsUnder = %v; want exactly {1,2} — 9 is a sibling target and 0 is unassigned", nights)
	}

	// The window of observing date 2026-07-14 (noon to noon, UTC).
	const dateLo, dateHi = "2026-07-15T00:00:00", "2026-07-16T00:00:00"
	total, matching, err := s.NightFolderCounts(ctx, "/nas/M31/NIGHT_1/", "/nas/M31/NIGHT_10", dateLo, dateHi)
	if err != nil || total != 2 || matching != 2 {
		t.Fatalf("NIGHT_1 counts = (%d,%d,%v); want (2,2,nil) — both spellings of the timestamp match", total, matching, err)
	}
	total, matching, err = s.NightFolderCounts(ctx, "/nas/M31/NIGHT_2/", "/nas/M31/NIGHT_20", dateLo, dateHi)
	if err != nil || total != 2 || matching != 0 {
		t.Fatalf("NIGHT_2 counts = (%d,%d,%v); want (2,0,nil) — the nested row counts, the date does not match",
			total, matching, err)
	}
	// Zero rows is the normal case for a pruned or fresh folder: SUM over none is
	// NULL, which must not fail the scan.
	total, matching, err = s.NightFolderCounts(ctx, "/nas/M31/NIGHT_7/", "/nas/M31/NIGHT_70", dateLo, dateHi)
	if err != nil || total != 0 || matching != 0 {
		t.Fatalf("empty folder counts = (%d,%d,%v); want (0,0,nil)", total, matching, err)
	}

	targets, err := s.DistinctTargetsUnder(ctx, "/nas/M31/NIGHT_1/", "/nas/M31/NIGHT_10")
	if err != nil {
		t.Fatalf("DistinctTargetsUnder: %v", err)
	}
	seen := map[string]bool{}
	for _, tg := range targets {
		seen[tg] = true
	}
	if len(seen) != 2 || !seen["M31"] || !seen["M 31"] {
		t.Fatalf("DistinctTargetsUnder = %v; want the two raw spellings filed in that folder", targets)
	}
}

// TestNightLedgerFoldsTargetCase pins the scope key against the case of the
// OBJECT card. An ASIAIR that writes "ic 5146" for a target already filed as
// "IC 5146" has not renamed anything: <TARGET> renders "ic5146", which on the
// case-insensitive filesystems Commander archives to is the very folder
// "IC5146" already occupies. A separate scope there mints MAX(night)+1 over an
// empty ledger and drops a fresh night back into an existing NIGHT_n folder.
//
// The first spelling seen is the one kept, because it is the spelling the
// destination folder was created under.
func TestNightLedgerFoldsTargetCase(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if n, err := s.AssignNight(ctx, NightRequest{Target: "IC 5146", OTA: "131F", ObservingDate: "2026-08-06"}); err != nil || n != 1 {
		t.Fatalf("first night = (%d,%v); want (1,nil)", n, err)
	}
	// A case variant is the same target: the next date continues the same ledger.
	if n, err := s.AssignNight(ctx, NightRequest{Target: "ic 5146", OTA: "131F", ObservingDate: "2026-08-24"}); err != nil || n != 2 {
		t.Fatalf("case-variant night = (%d,%v); want (2,nil) — a case difference forked the scope", n, err)
	}
	// An assigned date keeps its ordinal whichever spelling asks for it.
	if n, err := s.AssignNight(ctx, NightRequest{Target: "IC 5146", OTA: "131F", ObservingDate: "2026-08-24"}); err != nil || n != 2 {
		t.Fatalf("re-ask = (%d,%v); want (2,nil)", n, err)
	}
	if n, ok, err := s.LookupNight(ctx, "iC 5146", "131F", "2026-08-06"); err != nil || !ok || n != 1 {
		t.Fatalf("LookupNight(case variant) = (%d,%v,%v); want (1,true,nil)", n, ok, err)
	}
	if n, err := s.MaxNight(ctx, "IC 5146", "131F"); err != nil || n != 2 {
		t.Fatalf("MaxNight = (%d,%v); want (2,nil)", n, err)
	}
	held, err := s.NightHeldByOtherDate(ctx, "ic 5146", "131F", 1, "2026-08-24")
	if err != nil || !held {
		t.Fatalf("NightHeldByOtherDate(case variant) = (%v,%v); want true", held, err)
	}
	// The ledger keeps one spelling, and it is the first one filed.
	got, err := s.CanonicalTarget(ctx, "IC 5146")
	if err != nil || got != "IC 5146" {
		t.Fatalf("CanonicalTarget = (%q,%v); want (\"IC 5146\",nil)", got, err)
	}
	if got, err := s.CanonicalTarget(ctx, "ic 5146"); err != nil || got != "IC 5146" {
		t.Fatalf("CanonicalTarget(variant) = (%q,%v); want (\"IC 5146\",nil)", got, err)
	}
	// An unknown target is its own canonical spelling — nothing to adopt.
	if got, err := s.CanonicalTarget(ctx, "NGC 7000"); err != nil || got != "NGC 7000" {
		t.Fatalf("CanonicalTarget(unknown) = (%q,%v); want (\"NGC 7000\",nil)", got, err)
	}
}
