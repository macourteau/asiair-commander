//go:build live

package device

// Live-device smoke tests, gated behind a build tag plus environment variables
// naming a real ASIAIR. They exercise the device seam against real hardware and are
// excluded from CI's hermetic suite.
//
//	ASIAIR_COMMANDER_LIVE_HOST=192.168.1.50 go test -tags live -run TestLive ./internal/device/

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

func liveDevice(t *testing.T) Device {
	t.Helper()
	host := os.Getenv("ASIAIR_COMMANDER_LIVE_HOST")
	if host == "" {
		t.Skip("ASIAIR_COMMANDER_LIVE_HOST not set")
	}
	user := os.Getenv("ASIAIR_COMMANDER_LIVE_USER")
	if user == "" {
		user = "guest"
	}
	pass := os.Getenv("ASIAIR_COMMANDER_LIVE_PASS")
	if pass == "" {
		pass = "12345678"
	}
	return New(netsite.NewRegistry(nil), netsite.LocalEndpoint(host), user, pass, 5*time.Second)
}

// TestLiveProbeOpenSharesMount probes reachability, opens a session, reads the
// server clock (finding H1), lists shares (H2), mounts "EMMC Images", and
// lists its root as "" (H2).
func TestLiveProbeOpenSharesMount(t *testing.T) {
	d := liveDevice(t)
	ctx := context.Background()

	if !d.Probe(ctx) {
		t.Fatal("device unreachable on TCP/445")
	}

	sess, err := d.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	st := sess.ServerTime()
	if st.Time.IsZero() {
		t.Error("negotiate response carried no server clock (finding H1 regression)")
	}
	t.Logf("device clock offset: %v", clock.ForSession(st).Offset())

	shares, err := sess.ListShares()
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	t.Logf("storage shares: %v", shares)

	sh, err := sess.Mount("EMMC Images")
	if err != nil {
		t.Fatalf("Mount EMMC Images: %v", err)
	}
	entries, err := sh.ListDir("")
	if err != nil {
		t.Fatalf("ListDir root: %v", err)
	}
	t.Logf("EMMC Images root: %d entries", len(entries))

	free, total, err := sh.Statfs()
	if err != nil {
		t.Fatalf("Statfs: %v", err)
	}
	t.Logf("EMMC Images capacity: %d free / %d total", free, total)
}

// TestLiveReadFirstFile opens and drains the first regular file found under the
// EMMC Images root, exercising the streaming read path against real hardware.
func TestLiveReadFirstFile(t *testing.T) {
	d := liveDevice(t)
	ctx := context.Background()
	sess, err := d.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	sh, err := sess.Mount("EMMC Images")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sh.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		rc, err := sh.OpenReader(e.Name)
		if err != nil {
			t.Fatalf("OpenReader %q: %v", e.Name, err)
		}
		n, err := io.Copy(io.Discard, rc)
		rc.Close()
		t.Logf("read %q: %d bytes, err=%v", e.Name, n, err)
		return
	}
	t.Log("no regular files under EMMC Images root")
}
