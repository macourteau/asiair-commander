package role

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

const (
	hashShare = "EMMC Images"
	hashPath  = "Autorun/Light/M31/Light_M31_001.fit"
	hashIP    = "192.0.2.10"
	hashUser  = "operator-x9"
	hashPass  = "pw-Zq7-secret"
)

// hashRemote is a remote with one ASIAIR on its allowlist and one FITS frame
// on its share.
func hashRemote(t *testing.T, cfg Config, probeAnswers bool) (*running, []byte, time.Time) {
	t.Helper()
	rr := startRemote(t, startOpts{cfg: cfg})
	rr.lan.site.SetBroadcast([]netsite.Datagram{{Data: scanReply("guid-1", "cpu-1", hashIP), SrcIP: hashIP}}, nil)
	if _, err := rr.client.Broadcast(ctxFor(t, 5*time.Second), "b-setup"); err != nil {
		t.Fatal(err)
	}
	if probeAnswers {
		rr.lan.site.SetProbeReply(scanReply("guid-1", "cpu-1", hashIP), hashIP)
	}
	frame := fitstest.Build(fitstest.LightCards(400, 300, "M31"), fitstest.SyntheticImage(400, 300))
	mtime := time.Date(2026, 10, 5, 23, 14, 7, 123456789, time.UTC)
	rr.lan.dev.AddFile(hashShare, hashPath, frame, mtime)
	return rr, frame, mtime
}

func hashReq() proto.HashRequest {
	return proto.HashRequest{IP: hashIP, Share: hashShare, Path: hashPath, User: hashUser, Password: hashPass}
}

// localDigests is what Commander's local pre-delete hash (rig's hashDevice)
// computes for the same bytes: one pass through header.DigestTee.
func localDigests(t *testing.T, b []byte) (whole, data string) {
	t.Helper()
	tee := header.NewDigestTee()
	if _, err := io.Copy(tee, strings.NewReader(string(b))); err != nil {
		t.Fatal(err)
	}
	return tee.Sums()
}

// TestRemoteHashContract (remote half): the remote measures the digests
// Commander's own streaming read computes for the same file, returns the
// identity probed inside its own session (none when unanswered, without
// failing), streams progress during a slow read, stops reading when the call
// is cancelled, and keeps the call's credentials and arguments out of every log
// line and error.
func TestRemoteHashContract(t *testing.T) {
	t.Run("digests and identity", func(t *testing.T) {
		rr, frame, mtime := hashRemote(t, Config{}, true)
		hr, err := rr.client.Hash(ctxFor(t, 10*time.Second), "x-1", hashReq(), proto.HashOptions{Stall: 5 * time.Second})
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		whole, data := localDigests(t, frame)
		if hr.Whole != whole || hr.Data != data {
			t.Errorf("remote digests (%s, %s), want the local read's (%s, %s)", hr.Whole, hr.Data, whole, data)
		}
		// An independent check of both digests, not through DigestTee: the
		// whole file, and the bytes from the data unit's offset on.
		sum := sha256.Sum256(frame)
		if hr.Whole != hex.EncodeToString(sum[:]) {
			t.Errorf("whole digest %s is not the file's SHA-256", hr.Whole)
		}
		f := filepath.Join(t.TempDir(), "frame.fit")
		if err := os.WriteFile(f, frame, 0o600); err != nil {
			t.Fatal(err)
		}
		off, err := header.DataOffset(f)
		if err != nil {
			t.Fatal(err)
		}
		dsum := sha256.Sum256(frame[off:])
		if !header.IsUsableDataDigest(hr.Data) || hr.Data != hex.EncodeToString(dsum[:]) {
			t.Errorf("data digest %s is not the data unit's SHA-256", hr.Data)
		}
		if hr.Size != int64(len(frame)) || !hr.Mtime().Equal(mtime) {
			t.Errorf("size %d mtime %v, want %d %v", hr.Size, hr.Mtime(), len(frame), mtime)
		}
		if hr.GUID != "guid-1" || hr.CPUID != "cpu-1" || !hr.Identified() {
			t.Errorf("identity (%q, %q), want the probed one", hr.GUID, hr.CPUID)
		}

		// The session is the remote's own, on its local network, and the
		// probe went out only once it was open, to the same device.
		ev := rr.lan.Events()
		open, probe := -1, -1
		for i, e := range ev {
			switch e {
			case "open /" + hashIP:
				open = i
			case "probe " + hashIP + ":4720":
				probe = i
			}
		}
		if open < 0 || probe < 0 || probe < open {
			t.Errorf("LAN events %v: want the session opened, then the probe", ev)
		}
		// A fresh read: the file is opened once per call, never served from
		// anything earlier.
		before := rr.lan.dev.ContentReads()
		if _, err := rr.client.Hash(ctxFor(t, 10*time.Second), "x-2", hashReq(), proto.HashOptions{Stall: 5 * time.Second}); err != nil {
			t.Fatal(err)
		}
		if rr.lan.dev.ContentReads() != before+1 {
			t.Errorf("a second hash read the file %d times", rr.lan.dev.ContentReads()-before)
		}
	})

	t.Run("no identity is not an error", func(t *testing.T) {
		rr, frame, _ := hashRemote(t, Config{}, false)
		hr, err := rr.client.Hash(ctxFor(t, 15*time.Second), "x-3", hashReq(), proto.HashOptions{Stall: 10 * time.Second})
		if err != nil {
			t.Fatalf("Hash with an unanswered probe: %v", err)
		}
		whole, _ := localDigests(t, frame)
		if hr.Identified() || hr.GUID != "" || hr.CPUID != "" || hr.Whole != whole {
			t.Errorf("hash = %+v, want the digests and no identity", hr)
		}
	})

	t.Run("progress during a slow read", func(t *testing.T) {
		rr, _, _ := hashRemote(t, Config{ProgressInterval: 20 * time.Millisecond}, true)
		rr.lan.setSlow(30 * time.Millisecond)
		var last atomic.Int64
		var reports atomic.Int32
		hr, err := rr.client.Hash(ctxFor(t, 30*time.Second), "x-4", hashReq(), proto.HashOptions{
			Stall:      500 * time.Millisecond,
			OnProgress: func(n int64) { reports.Add(1); last.Store(n) },
		})
		if err != nil {
			t.Fatalf("slow Hash: %v", err)
		}
		if reports.Load() < 2 || last.Load() <= 0 || last.Load() > hr.Size {
			t.Errorf("%d progress reports, last %d bytes of %d", reports.Load(), last.Load(), hr.Size)
		}
	})

	t.Run("cancel stops the read", func(t *testing.T) {
		rr, _, _ := hashRemote(t, Config{ProgressInterval: 10 * time.Millisecond}, true)
		rr.lan.setSlow(20 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			_, err := rr.client.Hash(ctx, "x-5", hashReq(), proto.HashOptions{
				Stall: 5 * time.Second,
				OnProgress: func(int64) {
					select {
					case started <- struct{}{}:
					default:
					}
				},
			})
			done <- err
		}()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Hash = %v", err)
		}
		// The read stops within a buffer or two of the cancel.
		time.Sleep(100 * time.Millisecond)
		n := rr.lan.Reads()
		time.Sleep(200 * time.Millisecond)
		if rr.lan.Reads() != n {
			t.Errorf("the remote kept reading after the call was cancelled (%d -> %d reads)", n, rr.lan.Reads())
		}
		if strings.Contains(rr.logs.String(), "hash measured") {
			t.Error("a cancelled hash ran to completion")
		}
	})

	t.Run("no credentials or arguments in logs or errors", func(t *testing.T) {
		rr, _, _ := hashRemote(t, Config{}, true)
		var errs []string
		note := func(err error) {
			if err != nil {
				errs = append(errs, err.Error(), fmt.Sprintf("%v %+v %#v", err, err, err))
			}
		}
		_, err := rr.client.Hash(ctxFor(t, 10*time.Second), "x-6", hashReq(), proto.HashOptions{Stall: 5 * time.Second})
		note(err)
		// A device whose errors quote everything it was given.
		rr.lan.dev.FailStat(hashShare, fmt.Errorf("stat %s\\%s on %s as %s/%s: %w", hashShare,
			strings.ReplaceAll(hashPath, "/", `\`), hashIP, hashUser, hashPass, device.NotFoundError("no such file")))
		_, err = rr.client.Hash(ctxFor(t, 10*time.Second), "x-7", hashReq(), proto.HashOptions{Stall: 5 * time.Second})
		if proto.CodeOf(err) != proto.CodeNotFound {
			t.Errorf("hash of a missing file = %v, want not_found", err)
		}
		note(err)
		rr.lan.dev.FailSessionOpen(fmt.Errorf("SMB authentication against %s for %s with %s: %w", hashIP, hashUser, hashPass, device.NetworkError("reset")))
		_, err = rr.client.Hash(ctxFor(t, 10*time.Second), "x-8", hashReq(), proto.HashOptions{Stall: 5 * time.Second})
		if proto.CodeOf(err) != proto.CodeNetwork {
			t.Errorf("hash with a failing session = %v, want network", err)
		}
		note(err)
		off := hashReq()
		off.IP = "192.0.2.99"
		_, err = rr.client.Hash(ctxFor(t, 10*time.Second), "x-9", off, proto.HashOptions{Stall: 5 * time.Second})
		note(err)

		logs := rr.logs.String()
		var hashLines []string
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "call_id=x-") {
				hashLines = append(hashLines, line)
			}
		}
		if len(hashLines) < 4 {
			t.Fatalf("expected the hash calls to be logged by call id, got %q", hashLines)
		}
		for _, secret := range []string{hashUser, hashPass} {
			if strings.Contains(logs, secret) {
				t.Errorf("a log line carries the credential %q", secret)
			}
		}
		texts := append(errs, hashLines...)
		for _, arg := range []string{hashUser, hashPass, hashIP, "192.0.2.99", hashShare, hashPath, strings.ReplaceAll(hashPath, "/", `\`)} {
			for _, s := range texts {
				if strings.Contains(s, arg) {
					t.Errorf("%q appears in %q", arg, s)
				}
			}
		}
	})
}
