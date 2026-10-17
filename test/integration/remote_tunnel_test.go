//go:build integration && remotetest

// Real-tunnel integration tests (design remote-sites AC6,
// AC7, AC19 and AC26): the remote bridge end to end, through a
// real tailcat tunnel and a local relay, against the Samba NT1 container. The
// harness is in remote_harness_test.go.
//
// The restart policy itself is not run here. Running the remote under
// `docker compose` with `restart: unless-stopped` would need the release image
// built in the integration job (the SPA and the ASTAP download included) for
// one assertion that Docker restarts a container exiting non-zero, so it is
// covered instead by deploy/remote/docker-compose.yml and step 11a's manual
// run. What a restart policy relies on is tested here: the remote exits
// non-zero once its relay is gone past its exit window, a fresh run with the
// same pairing reconnects with no human action, and it never exits for want
// of Commander.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/role"
)

// The rig directories each test stages its frames in, pre-created by the
// Samba image.
const (
	remoteCycleDir   = "Autorun/Light/REMOTECYCLE"
	remoteDropDir    = "Autorun/Light/REMOTEDROP"
	remoteRestartDir = "Autorun/Light/REMOTERESTART"
	remoteSpareDir   = "Autorun/Light/REMOTESPARE"
)

// TestRemoteLargeDirectoryListsCompletely is TestLargeDirectoryListsCompletely
// through the tunnel (CLAUDE.md, "Listing completeness is a safety property"):
// a transport change under enumeration needs an exact-count test. The
// directory's TRANS2 FIND reply fragments across several SMB messages, which
// here cross the tunnel as one TCP byte stream; every entry must arrive.
func TestRemoteLargeDirectoryListsCompletely(t *testing.T) {
	h := newRemoteHarness(t, remoteOptions{})
	before := h.lan.smbForwards()
	_, share := h.openRemote(commanderSMBBudget)
	if h.lan.smbForwards() == before {
		t.Fatal("the session did not reach the container through the tunnel")
	}

	entries, err := share.ListDir(bigDir)
	if err != nil {
		t.Fatalf("ListDir(%q) through the tunnel: %v", bigDir, err)
	}
	if len(entries) != bigDirCount {
		t.Fatalf("ListDir(%q) through the tunnel returned %d entries, want exactly %d — a short count with no error is the silent truncation this test exists to catch",
			bigDir, len(entries), bigDirCount)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir {
			t.Errorf("ListDir(%q) returned a directory %q; the seed is files only", bigDir, e.Name)
		}
		if !strings.HasPrefix(e.Name, "Light_NGC7000_") || !strings.HasSuffix(e.Name, ".fit") {
			t.Errorf("unexpected entry name %q — reassembly may have corrupted the listing", e.Name)
		}
		if seen[e.Name] {
			t.Errorf("duplicate entry %q — reassembly overlapped a fragment", e.Name)
		}
		seen[e.Name] = true
	}
	if len(seen) != bigDirCount {
		t.Errorf("got %d distinct names, want %d", len(seen), bigDirCount)
	}
}

// TestRemoteListingDropIsError cuts the tunnel in the middle of the large
// directory's listing, at several points across its fragments, and requires an
// error every time: never a listing, short or otherwise (design
// remote-sites §12 invariant 3). A rig treats any listing error as listingOK
// false, so its verdict reads Unknown rather than "safe to power off".
//
// The cut is placed on the container's reply bytes as they enter the tunnel:
// the read that crosses the cut point is held back while the remote stops,
// taking the tunnel server down with it, and is then discarded. Commander's
// side keeps its session and finds out only from the tunnel.
func TestRemoteListingDropIsError(t *testing.T) {
	// The reply is about 100 KB in messages of about 16.5 KB, so these land in
	// its first message, across a message boundary, and deep in its tail.
	for _, cutAt := range []int64{6 << 10, 40 << 10, 80 << 10} {
		t.Run(fmt.Sprintf("cut_after_%dKiB", cutAt>>10), func(t *testing.T) {
			h := newRemoteHarness(t, remoteOptions{})
			_, share := h.openRemote(commanderSMBBudget)
			cut := h.lan.armCut(cutAt, h.stopRemote)

			entries, err := share.ListDir(bigDir)
			// The cut's channel closes once the remote has stopped, which can
			// trail Commander's own error; a listing that ended before the cut
			// point never closes it.
			select {
			case <-cut:
			case <-time.After(30 * time.Second):
				t.Fatalf("the listing ended (%d entries, error %v) before the cut at %d bytes: the cut point is past the reply", len(entries), err, cutAt)
			}
			if err == nil {
				t.Fatalf("the tunnel was cut mid-listing, yet ListDir returned %d entries and no error: a shorter listing instead of an error", len(entries))
			}
			if len(entries) != 0 {
				t.Fatalf("ListDir returned %d entries along with its error %v", len(entries), err)
			}
			requireNetworkError(t, "ListDir across the cut", err)
			t.Logf("ListDir across the cut: %v", err)
		})
	}
}

// TestRemotePullDeleteCycle runs a rig through the tunnel for a whole pull →
// verify → delete cycle, with the pre-delete digests measured by the remote,
// and mirrors TestSlicedDrainExactCount's end-state checks: every eligible
// original deleted from the device exactly once, and every NAS copy
// re-hashing to its record. The remote measured every deleted frame, and
// Commander read each device file exactly once, for its pull: it never
// re-read one for the pre-delete hash.
func TestRemotePullDeleteCycle(t *testing.T) {
	const frames = 6
	h := newRemoteHarness(t, remoteOptions{})
	var paths []string
	for i := 1; i <= frames; i++ {
		paths = append(paths, h.writeFrame(remoteCycleDir, i, 60, 48))
	}
	r := h.startRig(rigOptions{dir: remoteCycleDir, ep: remoteEP, grace: 3 * time.Second})
	r.waitStates(2*time.Minute, paths, model.StateDeleted)

	entries, err := h.direct.ListDir(remoteCycleDir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", remoteCycleDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("the device still holds %d entries in %s after the cycle: %v", len(entries), remoteCycleDir, names(entries))
	}
	if got := r.counter("asiair_commander_frames_deleted_total",
		map[string]string{"manner": string(metrics.MannerDeviceDeleted)}); got != frames {
		t.Fatalf("frames_deleted_total{manner=device_deleted} = %v, want exactly %d", got, frames)
	}
	r.checkNASCopies(paths)
	for _, p := range paths {
		if n := h.remoteTap.count(p); n < 1 {
			t.Errorf("%s was deleted, but the remote never read it: the pre-delete digest was not measured by the remote", p)
		}
		if n := r.tap.count(p); n != 1 {
			t.Errorf("Commander read %s %d times, want exactly once (its pull): it re-read the device file for the pre-delete hash", p, n)
		}
	}
	r.noTransferFailedAlerts()
}

// TestRemoteMidPullDrop drops the tunnel in the middle of a frame's transfer.
// The frame in flight fails its stream and the next frame opened on the same,
// now dead, session fails to open; both must fail as network errors, so
// neither is recorded failed and both are retried. Once the remote is back,
// every frame is pulled and verified.
func TestRemoteMidPullDrop(t *testing.T) {
	const frames = 3
	h := newRemoteHarness(t, remoteOptions{})
	// Every frame is on the device before the rig's first listing, so the
	// session that the cut kills has more frames to open after the one in
	// flight.
	var paths []string
	for i := 1; i <= frames; i++ {
		paths = append(paths, h.writeFrame(remoteDropDir, i, 1024, 1024))
	}
	// Each frame is 2 MiB and the listing a few KiB, so the cut lands inside
	// the first frame's stream.
	cut := h.lan.armCut(512<<10, h.stopRemote)
	// A grace of an hour keeps the delete pass out of it.
	r := h.startRig(rigOptions{dir: remoteDropDir, ep: remoteEP, grace: time.Hour})
	select {
	case <-cut:
	case <-time.After(time.Minute):
		t.Fatal("no pull reached the cut within a minute")
	}

	waitUntil(t, time.Minute, "the frame in flight to fail its stream and the next one to fail to open", func() bool {
		if n := r.outcomes(metrics.OutcomeOpenSourceFailed); n > 0 {
			t.Fatalf("a frame opened on the dropped session failed with a non-network error (%v times): it was recorded failed", n)
		}
		r.noTransferFailedAlerts()
		return r.outcomes(metrics.OutcomeCopyFailed) >= 1 && r.outcomes(metrics.OutcomeOpenSourceTransient) >= 1
	})
	for _, p := range paths {
		if f, ok := r.frame(p); ok && f.State == model.StateFailed {
			t.Fatalf("%s recorded failed after the drop: %s", p, f.Error)
		}
	}
	t.Logf("after the drop: copy_failed=%v open_source_transient=%v",
		r.outcomes(metrics.OutcomeCopyFailed), r.outcomes(metrics.OutcomeOpenSourceTransient))

	h.startRemote()
	r.waitStates(2*time.Minute, paths, model.StateVerified)
	r.noTransferFailedAlerts()
	if n := r.outcomes(metrics.OutcomeOpenSourceFailed); n != 0 {
		t.Fatalf("transfer_outcomes_total{outcome=open_source_failed} = %v, want 0", n)
	}
	r.checkNASCopies(paths)
}

// TestRemoteRestartRecovers restarts the remote between two batches of frames.
// The supervisor notices on its own, rebuilds its tunnel client (a client
// never recovers from a server restart by itself), runs the reconnect round
// that refills the new process's empty allowlist, and the second batch goes
// through the whole cycle, its pre-delete digests measured by the restarted
// remote. The rebuild waits out the remote site's SMB budget of tunnel silence;
// the supervisor's sped-up clock (clockRate) is the test seam that makes that
// two seconds.
func TestRemoteRestartRecovers(t *testing.T) {
	h := newRemoteHarness(t, remoteOptions{})
	var first, second []string
	for i := 1; i <= 3; i++ {
		first = append(first, h.writeFrame(remoteRestartDir, i, 60, 48))
	}
	r := h.startRig(rigOptions{dir: remoteRestartDir, ep: remoteEP, grace: 3 * time.Second})
	r.waitStates(2*time.Minute, first, model.StateDeleted)

	builds, rebuilds := h.builds.Load(), h.rebuilds()
	h.restartRemote()
	waitUntil(t, time.Minute, "the supervisor to reconnect to the restarted remote", func() bool {
		return h.status().Restarts == 1 && h.site().State() == netsite.StateUp
	})
	if h.rebuilds() <= rebuilds || h.builds.Load() <= builds {
		t.Fatalf("the remote restarted and the site came back without a client rebuild (rebuilds %v → %v, builds %d → %d)",
			rebuilds, h.rebuilds(), builds, h.builds.Load())
	}

	for i := 4; i <= 6; i++ {
		second = append(second, h.writeFrame(r.dir, i, 60, 48))
	}
	r.waitStates(2*time.Minute, second, model.StateDeleted)

	all := append(first, second...)
	if got := r.counter("asiair_commander_frames_deleted_total",
		map[string]string{"manner": string(metrics.MannerDeviceDeleted)}); got != float64(len(all)) {
		t.Fatalf("frames_deleted_total{manner=device_deleted} = %v, want exactly %d", got, len(all))
	}
	r.checkNASCopies(all)
	for _, p := range all {
		if n := h.remoteTap.count(p); n < 1 {
			t.Errorf("%s was deleted, but the remote never read it", p)
		}
		if n := r.tap.count(p); n != 1 {
			t.Errorf("Commander read %s %d times, want exactly once (its pull)", p, n)
		}
	}
	r.noTransferFailedAlerts()
}

// TestRebuildSparesLiveTransfer makes every heartbeat fail while a transfer
// is moving bytes, and then while the remote's hash is reporting progress, and
// requires that the supervisor rebuild neither client: rebuilding would cut a
// transfer that is making progress. Then, with the heartbeat still failing and
// nothing moving, the rebuild must come, which proves the first two phases
// gave the rule its chance to fire.
//
// The remote drops each hello as it arrives (helloGate) and keeps serving
// everything else. The supervisor's clock runs clockRate times fast, so its
// SMB budget of silence is two real seconds while bytes move every 50 ms.
func TestRebuildSparesLiveTransfer(t *testing.T) {
	h := newRemoteHarness(t, remoteOptions{})
	data := fitstest.Build(fitstest.LightCards(1024, 1024, "REMOTESPARE"), fitstest.SyntheticImage(1024, 1024))
	p := h.writeFile(remoteSpareDir, "Light_REMOTESPARE_0001.fit", data)
	want := sha256Hex(data)

	// requireNoRebuild runs phase while every hello fails, and checks that the
	// heartbeat failed throughout it and that no client was rebuilt.
	requireNoRebuild := func(what string, phase func()) {
		t.Helper()
		builds, rebuilds, failed := h.builds.Load(), h.rebuilds(), h.heartbeatsFailed()
		h.failHellos.Store(true)
		phase()
		h.failHellos.Store(false)
		if n := h.heartbeatsFailed() - failed; n < 2 {
			t.Fatalf("%s: the heartbeat failed %v times, want at least 2: the rebuild rule was never asked", what, n)
		}
		if h.rebuilds() != rebuilds || h.builds.Load() != builds {
			t.Fatalf("%s: the supervisor rebuilt the tunnel client (rebuilds %v → %v, builds %d → %d) while it was moving bytes",
				what, rebuilds, h.rebuilds(), builds, h.builds.Load())
		}
	}

	// A transfer: Commander streams the file through the tunnel slowly.
	_, share := h.openRemote(commanderSMBBudget)
	rc, err := share.OpenReader(p)
	if err != nil {
		t.Fatalf("OpenReader through the tunnel: %v", err)
	}
	requireNoRebuild("a live transfer", func() {
		sum := sha256.New()
		buf := make([]byte, slowChunk)
		for {
			n, err := rc.Read(buf)
			sum.Write(buf[:n])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("the transfer failed while the heartbeat was failing: %v", err)
			}
			time.Sleep(slowPause)
		}
		if got := hex.EncodeToString(sum.Sum(nil)); got != want {
			t.Fatalf("the transfer read %s, want %s", got, want)
		}
	})
	rc.Close()

	// A remote hash: the remote reads the file slowly in its own session and
	// reports progress; nothing else crosses the tunnel.
	h.waitSiteUp("the site to come back once hellos answer")
	h.remoteTap.slow.Store(true)
	before := h.remoteTap.count(p)
	type measured struct {
		res netsite.DigestResult
		err error
	}
	done := make(chan measured, 1)
	go func() {
		res, err := h.site().MeasureDigests(context.Background(), netsite.DigestRequest{
			IP: remoteDevIP, Share: shareName, Path: p,
			User: netsite.Credential(smbUser), Password: netsite.Credential(smbPassword),
			Stop: func() bool { return false },
			Local: func() (netsite.Digests, error) {
				return netsite.Digests{}, errors.New("Commander streamed the file itself: the remote did not measure it")
			},
		})
		done <- measured{res, err}
	}()
	waitUntil(t, 30*time.Second, "the remote to start reading for the hash", func() bool { return h.remoteTap.count(p) > before })
	requireNoRebuild("a remote hash reporting progress", func() {
		select {
		case m := <-done:
			if m.err != nil {
				t.Fatalf("the remote hash failed while the heartbeat was failing: %v", m.err)
			}
			if m.res.Whole != want || !m.res.Probed || m.res.GUID != remoteGUID {
				t.Fatalf("the remote hash measured %s (probed %v, guid %q), want %s from %q", m.res.Whole, m.res.Probed, m.res.GUID, want, remoteGUID)
			}
		case <-time.After(time.Minute):
			t.Fatal("the remote hash did not finish within a minute")
		}
	})
	h.remoteTap.slow.Store(false)

	// The control: the heartbeat fails and nothing moves, so the client is
	// rebuilt.
	rebuilds := h.rebuilds()
	h.failHellos.Store(true)
	waitUntil(t, 30*time.Second, "a rebuild once nothing moves", func() bool { return h.rebuilds() > rebuilds })
	h.failHellos.Store(false)
	h.waitSiteUp("the site to come back after the rebuild")
}

// TestForwardRefusesDeadTarget: an ASIAIR that answered the remote's broadcast
// and was then powered off sits on the remote's allowlist, and a dial to it
// through the live remote fails instead of connecting. Probe reads it
// unreachable and the rig takes the ordinary offline path and unreachable alert,
// while the remote stays up and lists no warning for it: a powered-off rig is
// the rig's own offline state, not a remote problem.
func TestForwardRefusesDeadTarget(t *testing.T) {
	h := newRemoteHarness(t, remoteOptions{})
	h.dead.on.Store(false)
	deadEP := netsite.Endpoint{Site: remoteID, IP: remoteDeadIP}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := h.site().DialTCP(ctx, remoteDeadIP, 445, commanderSMBBudget)
	if err == nil {
		conn.Close()
		t.Fatal("a dial through the remote to a powered-off device connected")
	}
	requireNetworkError(t, "dial to a powered-off device", err)
	if device.New(h.sites, deadEP, smbUser, smbPassword, commanderSMBBudget).Probe(ctx) {
		t.Fatal("Probe read a powered-off device behind the remote as reachable")
	}
	if h.site().State() != netsite.StateUp {
		t.Fatal("a powered-off device took its remote's site down")
	}

	r := h.startRig(rigOptions{dir: remoteCycleDir, ep: deadEP, grace: time.Hour, unreachableAfter: 2 * time.Second})
	waitUntil(t, time.Minute, "the rig to read offline and raise its unreachable alert", func() bool {
		if r.rigState() != model.RigStateOffline {
			return false
		}
		for _, a := range r.notify.Raised() {
			if a.Category == model.AlertUnreachable {
				return true
			}
		}
		return false
	})
	if h.site().State() != netsite.StateUp {
		t.Fatal("the remote's site went down while its rig was offline")
	}
	for _, w := range h.status().Warnings {
		if w.Kind == string(proto.WarnRefusedForward) && strings.Contains(w.Target, remoteDeadIP) {
			t.Fatalf("the remote reported the powered-off device as a refused forward: %+v", w)
		}
	}
}

// TestRemoteRecoversFromUplinkDrop: with its relay link down past its exit
// window, the remote exits non-zero (Run's ErrRelayUnreachable, which the
// remote verb returns as exit code 1); re-run with the same pairing, as its
// restart policy would once the link is back, it reconnects to Commander with
// no human action.
func TestRemoteRecoversFromUplinkDrop(t *testing.T) {
	const exitAfter = 4 * time.Second
	h := newRemoteHarness(t, remoteOptions{exitAfter: exitAfter})
	run := h.remote()
	waitUntil(t, 30*time.Second, "the remote's relay home region", run.homeConnected)

	cutAt := time.Now()
	h.relay.Cut()
	select {
	case <-run.exited:
	case <-time.After(exitAfter + 30*time.Second):
		t.Fatalf("the remote was still running %v after its relay went down, past its %v exit window", time.Since(cutAt), exitAfter)
	}
	if !errors.Is(run.err, role.ErrRelayUnreachable) {
		t.Fatalf("the remote stopped with %v, want %v", run.err, role.ErrRelayUnreachable)
	}
	// The window runs from the last check that saw the relay connected, at
	// most one check interval before the cut.
	if d := time.Since(cutAt); d < exitAfter-exitCheckInterval {
		t.Fatalf("the remote exited %v after its relay went down, inside its %v exit window", d, exitAfter)
	}

	h.relay.Restore()
	run = h.startRemote()
	waitUntil(t, 90*time.Second, "Commander to reconnect to the re-run remote", func() bool {
		return h.status().Restarts == 1 && h.site().State() == netsite.StateUp
	})
	waitUntil(t, 30*time.Second, "the re-run remote's relay home region", run.homeConnected)
}

// TestRemoteOutlivesAbsentCommander: with Commander stopped and the relay up,
// the remote keeps running well past its exit window. A missing hello never
// makes it exit, so a stopped, removed or re-paired Commander does not send it
// into a restart loop.
func TestRemoteOutlivesAbsentCommander(t *testing.T) {
	const exitAfter = 3 * time.Second
	h := newRemoteHarness(t, remoteOptions{exitAfter: exitAfter})
	run := h.remote()
	h.sup.Stop()

	select {
	case <-run.exited:
		t.Fatalf("the remote exited with Commander stopped and its relay up: %v", run.err)
	case <-time.After(3 * exitAfter):
	}
	if !run.homeConnected() {
		t.Fatal("the remote lost its relay home region with the relay up")
	}
}
