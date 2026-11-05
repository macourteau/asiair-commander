//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
	"github.com/macourteau/asiair-commander/internal/web"
)

// TestRunStackJoinsBackfillBeforeClosingStore holds the real astrometry backfill
// inside a header read by filing a frame whose archive path is a FIFO: the read
// blocks until the test lets it go, as one against a slow volume would, and no
// shutdown signal reaches it. runStack must still be waiting for it when the test
// looks, because returning would close the store underneath the running backfill.
func TestRunStackJoinsBackfillBeforeClosingStore(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Defaults()
	cfg.WebPort = freePortPair(t)
	if err := config.Save(filepath.Join(dataDir, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(dataDir, "held.fit")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	seedFiledFrame(t, dataDir, fifo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() {
		done <- runStack(ctx, dataDir, logger, web.NewLogBuffer(100), nil)
	}()

	writer := awaitFIFOReader(t, fifo, done)
	defer writer.Close()

	cancel()
	select {
	case err := <-done:
		t.Fatalf("runStack returned (%v) while the backfill was still running", err)
	case <-time.After(500 * time.Millisecond):
	}

	writer.Close() // the header read ends at EOF and the backfill unwinds
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runStack returned %v after the backfill finished", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runStack did not return after the backfill finished")
	}
}

// seedFiledFrame records a verified frame at destPath with no astrometry, which is
// the row the backfill exists to visit, then closes the store for runStack to open.
func seedFiledFrame(t *testing.T, dataDir, destPath string) {
	t.Helper()
	st, err := store.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.UpsertFrame(context.Background(), &model.Frame{
		RigID:       "rig-01",
		SourceShare: "EMMC Images",
		SourcePath:  "Autorun/Light/M31/held.fit",
		DestPath:    destPath,
		State:       model.StateVerified,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// awaitFIFOReader returns a write end of the FIFO once a reader has it open, which
// is the moment the backfill is inside its header read. O_NONBLOCK makes the open
// fail with ENXIO until then, instead of blocking the test.
func awaitFIFOReader(t *testing.T, fifo string, done <-chan error) *os.File {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return w
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("runStack returned (%v) before the backfill reached the frame", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the backfill did not reach the frame within 20s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
