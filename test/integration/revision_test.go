//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The revision replay's own frame, written into the seeded Autorun/Light/NGC7000/
// directory because the Share seam can create files but not directories. Its OBJECT
// is its own, so it routes to a destination folder no other frame on the share
// shares, and "one file in that folder" means what it says.
const (
	revisionTarget = "H13REV"
	revisionName   = "Light_H13REV_0001.fit"
	revisionW      = 60
	revisionH      = 48
)

// revisionHarness runs a real worker over a real on-disk store, the real OS-backed
// localfs and — unlike the re-file harness beside it — the REAL SMB1 device against
// the live container. That is the whole point: the in-memory device fake hands the
// filing path the bytes a test handed it, so it cannot show that a file rewritten on
// an SMB share re-lists with its new length and re-reads as the revised bytes, which
// is the sequence hardware finding H13 is about.
type revisionHarness struct {
	t     *testing.T
	share device.Share
	store store.Store
	notif *notify.Fake
	w     *rig.Worker
	ctx   context.Context
}

func newRevisionHarness(t *testing.T) *revisionHarness {
	t.Helper()
	dev := newDevice(t)
	ctx := context.Background()

	// A session held for the test's own writes to the share, alongside the per-cycle
	// sessions the worker opens: the device rewriting a frame in place is modelled by
	// this session, not by the worker's.
	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	share, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount(%q): %v", shareName, err)
	}
	t.Cleanup(func() { share.Umount() })

	dir := t.TempDir()
	cfg := config.Defaults()
	// Both quiescence gates off: the frame's age is not what this test is about, and
	// waiting out the shipped five minutes twice would make it useless.
	cfg.Defaults.MinFileAge = model.Duration(0)
	cfg.Defaults.SettleCycles = 0
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	cfg.Rigs = []model.RigProfile{{
		ID: refileRigID, Name: "Test", GUID: "guid-1", CPUID: "cpu-1",
		OTA: refileOTA, Session: refileSession, Managed: true,
		// Scoped to the one seeded directory rather than Autorun: the share also
		// carries the 400-entry BIGNIGHT fragmentation fixture, which this test has no
		// reason to pull.
		Sources: []model.Source{{Share: shareName, Roots: []string{frameDir}}},
	}}

	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	notif := &notify.Fake{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := rig.NewWorker(
		refileRigID, ptr,
		func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev },
		refileResolver{addr: host(), guid: "guid-1"}, netsite.NewRegistry(logger),
		localfs.New(), header.New(), st, hub.New(logger), notif,
		refileAdmission{}, nil, nil, time.UTC, time.Now, metrics.New("integration-revision"), logger,
	)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return &revisionHarness{t: t, share: share, store: st, notif: notif, w: w, ctx: ctx}
}

// writeOnShare puts bytes at the frame's path on the live share, creating the file
// or truncating it in place — the device-side rewrite this test exists to replay.
func (h *revisionHarness) writeOnShare(body []byte) {
	h.t.Helper()
	if err := h.share.WriteMarkerAt(frameDir, revisionName, body); err != nil {
		h.t.Fatalf("write %s on the share: %v", revisionName, err)
	}
}

// awaitRow polls the ledger until the frame's row satisfies want, forcing a cycle
// each time round: the worker's own poll interval is the shipped 45s, and the
// device's revision is not something it can be told about.
func (h *revisionHarness) awaitRow(what string, want func(model.Frame) bool) model.Frame {
	h.t.Helper()
	src := path.Join(frameDir, revisionName)
	deadline := time.Now().Add(90 * time.Second)
	for {
		f, ok, err := h.store.GetFrame(h.ctx, refileRigID, shareName, src)
		if err != nil {
			h.t.Fatalf("GetFrame: %v", err)
		}
		if ok && want(*f) {
			return *f
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: the row never reached that state within the deadline (present=%v)", what, ok)
		}
		h.w.Send(rig.RunNow)
		time.Sleep(250 * time.Millisecond)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fileSum(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return sha256Hex(b)
}

// TestDeviceRevisionOverRealSMB (design frame-lifecycle AC1) replays hardware
// finding H13's observed sequence against the live container: a frame filed, then
// the same frame rewritten ON THE SHARE with its header grown by one 2880-byte
// block and its data unit untouched. One file survives at the canonical name
// holding the revised bytes, no rot_clash is raised, and both sides of the row
// describe the copy they name.
//
// The in-process test in internal/rig is necessary and is not replaced: it reaches
// crash windows and ledger shapes no container can stage. It is not sufficient on
// its own, because the device fake returns the bytes a test handed it — so what it
// proves about a rewritten file is a property of the fake. Here the rewrite is a
// real SMB1 Create-and-truncate, the re-listing is a real TRANS2 FIND, and the
// re-read the discriminator's first leg is teed off is a real SMB1 read.
func TestDeviceRevisionOverRealSMB(t *testing.T) {
	h := newRevisionHarness(t)
	cards := fitstest.LightCards(revisionW, revisionH, revisionTarget)
	original, revised := fitstest.Revision(cards, fitstest.SyntheticImage(revisionW, revisionH))
	if len(revised)-len(original) != fitstest.BlockSize {
		t.Fatalf("setup: the revision must grow the file by one header block, got %d", len(revised)-len(original))
	}

	// Leave the share as this test found it: the fixture tree is shared with every
	// other test in the package.
	t.Cleanup(func() {
		if err := h.share.Delete(path.Join(frameDir, revisionName)); err != nil {
			t.Logf("cleanup: delete %s: %v", revisionName, err)
		}
	})

	h.writeOnShare(original)
	filed := h.awaitRow("the first pull", func(f model.Frame) bool { return f.State == model.StateVerified })
	if filed.SHA256 != sha256Hex(original) {
		t.Fatalf("the filed row must anchor to the bytes the device held, got %q", filed.SHA256)
	}
	if filed.DataSHA256 == "" {
		t.Fatal("the filing path must record the exposure identity")
	}
	dest := filed.DestPath
	if got := fileSum(t, dest); got != sha256Hex(original) {
		t.Fatalf("the archive copy does not hold the pulled bytes")
	}

	// The device rewrites the finished frame in place, injecting the WCS it has since
	// solved. Nothing else about the frame changes.
	h.writeOnShare(revised)
	got := h.awaitRow("the revision", func(f model.Frame) bool { return f.SHA256 == sha256Hex(revised) })

	ents, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("read dest dir: %v", err)
	}
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("a revision must leave ONE file at the canonical name, got %v", names)
	}
	if got.DestPath != dest {
		t.Fatalf("the frame must stay at its own name: %q, want %q", got.DestPath, dest)
	}
	if sum := fileSum(t, dest); sum != sha256Hex(revised) {
		t.Fatalf("the canonical name must hold the revised bytes, got %s", sum)
	}
	for _, a := range h.notif.Raised() {
		if a.Category == model.AlertRotClash {
			t.Fatalf("a proven revision is not a content clash; rot_clash raised: %s", a.Message)
		}
	}

	// The anchors, keyed on the copy they describe: the device side is what the share
	// now holds, the archive side what the NAS now holds, and the exposure identity is
	// what survives the header edit.
	if got.Size != int64(len(revised)) {
		t.Fatalf("size = %d, want the revised device file's %d", got.Size, len(revised))
	}
	if got.ArchiveSHA256 != sha256Hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must describe the bytes on the NAS: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DataSHA256 != filed.DataSHA256 {
		t.Fatalf("the exposure identity is what survives the revision: %q became %q", filed.DataSHA256, got.DataSHA256)
	}
	if got.State != model.StateVerified {
		t.Fatalf("state = %s, want verified so the delete pass reaches the frame at all", got.State)
	}
	// The listed mtime the row records is the device's, read back over the same SMB
	// seam; the ±2s tolerance is the one decide() compares with, for FAT granularity
	// and clock-offset jitter.
	fi, err := h.share.Stat(path.Join(frameDir, revisionName))
	if err != nil {
		t.Fatalf("Stat the revised frame: %v", err)
	}
	if d := got.Mtime - fi.Mtime.Unix(); d < -2 || d > 2 {
		t.Fatalf("mtime = %d, want the revised file's %d", got.Mtime, fi.Mtime.Unix())
	}
	if got.PulledAt < filed.PulledAt {
		t.Fatalf("a supersede restarts the grace clock: pulled_at went from %d to %d", filed.PulledAt, got.PulledAt)
	}
}
