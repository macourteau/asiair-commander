package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/demofiles"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
)

// TestHeldFrameStaysUnarchived pins the staging the device file browser's
// delete dialog relies on: the held row seeddemo writes is the harvester's row
// for the device file the demo build stages (same key, size and mtime), it
// survives the store's write path as held, the tool counts it as unarchived,
// and the delete pass never selects it.
func TestHeldFrameStaysUnarchived(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	profile := &model.RigProfile{
		ID: "rig-fra", GUID: demofiles.RigFRA, Managed: true,
		Sources: []model.Source{{Share: demofiles.Share, Roots: []string{"Autorun"}}},
	}
	hf := demofiles.HeldFrame
	if !rig.HarvestCandidate(profile, nil, demofiles.Share, hf.Path) {
		t.Fatalf("%s is not a harvest candidate, so no held row could cover it", hf.Path)
	}
	key, ok := rig.HarvestStoreKey(profile, demofiles.Share, hf.Path)
	if !ok {
		t.Fatalf("%s has no store key", hf.Path)
	}

	if err := st.UpsertFrame(ctx, heldFrame(dir, profile.ID)); err != nil {
		t.Fatalf("upsert held frame: %v", err)
	}
	row, found, err := st.GetFrame(ctx, profile.ID, demofiles.Share, key)
	if err != nil || !found {
		t.Fatalf("held row not found under the harvester's key %q: found=%v err=%v", key, found, err)
	}
	if row.State != model.StateHeld {
		t.Fatalf("state = %q, want held", row.State)
	}
	if row.Size != int64(hf.Size) || row.Mtime != hf.Mtime.Unix() {
		t.Fatalf("row size/mtime = %d/%d, want the staged file's %d/%d", row.Size, row.Mtime, hf.Size, hf.Mtime.Unix())
	}
	if rig.ArchivedCopy(row, int64(hf.Size), hf.Mtime) {
		t.Fatal("ArchivedCopy accepted the held row; the delete dialog would count 0 unarchived")
	}
	eligible, err := st.DeletionEligible(ctx, profile.ID, time.Now().Add(24*time.Hour).Unix())
	if err != nil {
		t.Fatalf("deletion eligible: %v", err)
	}
	if len(eligible) != 0 {
		t.Fatalf("delete pass would select %d held frame(s)", len(eligible))
	}
}
