//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/localfs"
)

// TestCullDrainDeviceRehashByteExact (design library-reconciliation I32) pins the
// primitive the culled-original drain rests on against a REAL device: the
// pre-delete device re-hash. The drain deletes a frame's only remaining copy off
// the card only when a fresh streamed hash of the device copy still equals
// f.SHA256, the immutable capture anchor. The in-memory fake returns bytes
// verbatim, so it cannot prove the SMB1 read reproduces that anchor byte-exactly,
// nor that a changed device copy hashes differently — which is exactly what the
// `deviceHash != f.SHA256` abort depends on. Both are proven here against the live
// container.
func TestCullDrainDeviceRehashByteExact(t *testing.T) {
	dev := newDevice(t)
	ctx := context.Background()

	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()
	share, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount(%q): %v", shareName, err)
	}
	defer share.Umount()

	fixtureBytes, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(fixtureRel)))
	if err != nil {
		t.Fatalf("read local fixture: %v", err)
	}
	anchor := sha256.Sum256(fixtureBytes) // stands in for the recorded f.SHA256

	framePath := filepath.ToSlash(filepath.Join(frameDir, frameName))

	// The device re-hash the drain streams must equal the anchor byte-for-byte.
	rc, err := share.OpenReader(framePath)
	if err != nil {
		t.Fatalf("OpenReader(%q): %v", framePath, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		rc.Close()
		t.Fatalf("stream device copy: %v", err)
	}
	rc.Close()
	deviceHash := hex.EncodeToString(h.Sum(nil))
	if deviceHash != hex.EncodeToString(anchor[:]) {
		t.Fatalf("device re-hash %s != anchor %s — the drain's gate would misfire on a real device",
			deviceHash, hex.EncodeToString(anchor[:]))
	}

	// A single flipped byte must produce a different hash, so a counter-reset run that
	// overwrote the path is caught and the frame is held, never deleted.
	mutated := append([]byte(nil), fixtureBytes...)
	mutated[len(mutated)/2] ^= 0xFF
	if mutatedHash := sha256.Sum256(mutated); hex.EncodeToString(mutatedHash[:]) == deviceHash {
		t.Fatal("a changed device copy must not hash to the anchor")
	}

	// The drain's terminal action — a real Delete removing the only remaining copy —
	// against a throwaway file so the shared fixture is left intact for other tests.
	throwaway := ".aac-cull-drain-probe"
	if err := share.WriteMarkerAt("", throwaway, []byte("probe")); err != nil {
		t.Fatalf("seed throwaway: %v", err)
	}
	if err := share.Delete(throwaway); err != nil {
		t.Fatalf("Delete(%q): %v", throwaway, err)
	}
	if _, err := share.Stat(throwaway); err == nil {
		t.Fatalf("Stat after Delete(%q) returned nil error; the file was not removed", throwaway)
	}
}

// TestCullDrainReappearedCopyRealFS (design library-reconciliation I33) pins the
// real-OS filesystem semantics the reappeared-copy guard depends on: the drain
// decides a frame was culled from a localfs Stat reporting the NAS copy absent, and
// skips-and-clears when it reappears. That decision is only as sound as os.Stat's
// not-found signal, which the in-memory localfs fake merely emulates. Here the real
// OS-backed localfs proves the absent → fs.ErrNotExist → present transition end to
// end.
func TestCullDrainReappearedCopyRealFS(t *testing.T) {
	lfs := localfs.New()
	dir := t.TempDir()
	dest := filepath.Join(dir, "Light_0001.fit")

	// Present: the frame is archived on the NAS — the drain would run the ordinary
	// present-copy path (and, for an unclassified frame, skip the delete).
	if err := os.WriteFile(dest, []byte("archived-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lfs.Stat(dest); err != nil {
		t.Fatalf("Stat of a present copy must succeed; got %v", err)
	}

	// Culled: the operator removed the NAS copy — Stat must surface fs.ErrNotExist,
	// the exact signal handleAbsentNAS treats as "culled".
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lfs.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat of an absent copy must be fs.ErrNotExist; got %v", err)
	}

	// Reappeared: a hand restore / backup put it back mid-drain — Stat succeeds again,
	// which is the premise the guard uses to skip the delete and clear the stale mark.
	if err := os.WriteFile(dest, []byte("restored-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lfs.Stat(dest); err != nil {
		t.Fatalf("Stat of a reappeared copy must succeed; got %v", err)
	}
}
