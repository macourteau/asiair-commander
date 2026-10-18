package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
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

// TestLiveSeedArchiveCopiesExist pins that every row the live seed writes has its
// NAS copy on disk with the recorded size and hash. The library scan marks a row
// whose dest_path is absent as missing, so a row without its file raises "Library
// reconciled" on the first linked scan and flips the rig's verdict.
func TestLiveSeedArchiveCopiesExist(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := seedLive(ctx, st, dir); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	seen := 0
	for _, rigID := range liveRigs {
		rows, _, err := st.ListFrames(ctx, rigID, "", "", 0, 100)
		if err != nil {
			t.Fatalf("list frames %s: %v", rigID, err)
		}
		for _, row := range rows {
			seen++
			data, err := os.ReadFile(row.DestPath)
			if err != nil {
				t.Fatalf("%s frame %s: archive copy unreadable: %v", rigID, row.SourcePath, err)
			}
			if int64(len(data)) != row.ArchiveSize {
				t.Fatalf("%s: archive copy is %d bytes, row records %d", row.DestPath, len(data), row.ArchiveSize)
			}
			sum := fmt.Sprintf("%x", sha256.Sum256(data))
			if sum != row.ArchiveSHA256 || sum != row.SHA256 {
				t.Fatalf("%s: archive hash %s, row records archive %s and device %s", row.DestPath, sum, row.ArchiveSHA256, row.SHA256)
			}
			// The demo build stages the device file from the same contract, so the
			// device, archive and recorded copies agree.
			if dev := fmt.Sprintf("%x", sha256.Sum256(demofiles.HeldFrame.Content())); dev != sum {
				t.Fatalf("staged device file hashes to %s, archive to %s", dev, sum)
			}
			if row.State != model.StateHeld {
				t.Fatalf("%s: state %q, want held", row.DestPath, row.State)
			}
		}
	}
	if seen == 0 {
		t.Fatal("live seed wrote no rows")
	}
}
