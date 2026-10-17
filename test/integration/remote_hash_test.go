//go:build integration

package integration

import (
	"context"
	"io"
	"net"
	"path"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/role"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// scanSite is the remote's real local site, with its scan_air broadcast and
// probe answered as the container's ASIAIR would: the Samba container speaks
// SMB only. Its SMB dials are the real local site's.
type scanSite struct {
	netsite.Site
	reply []byte
	src   string
}

func (s scanSite) Broadcast(context.Context) ([]netsite.Datagram, error) {
	return []netsite.Datagram{{Data: s.reply, SrcIP: s.src}}, nil
}

func (s scanSite) ProbeUDP(_ context.Context, _ string, _ int, _ []byte, _ time.Duration, accept func([]byte, string) bool) bool {
	return accept(s.reply, s.src)
}

type scanSites struct{ site netsite.Site }

func (s scanSites) Site(netsite.ID) netsite.Site { return s.site }
func (s scanSites) All() []netsite.Site          { return []netsite.Site{s.site} }

// TestRemoteHashMatchesLocalRead: against the real Samba NT1 share, the remote
// role's hash, measured in its own SMB session, returns the same digests as
// Commander's own streaming read of the same file in its session (rig's
// hashDevice: OpenReader through header.DigestTee), with the probed identity.
func TestRemoteHashMatchesLocalRead(t *testing.T) {
	dev := newDevice(t) // skips without the container
	h := host()
	ip := net.ParseIP(h)
	if ip == nil || ip.To4() == nil {
		t.Skipf("the remote allowlists IPv4 literals only; SMB host %q is not one", h)
	}
	file := path.Join(frameDir, frameName)

	// Commander's local read.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	share, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	fi, err := share.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	rc, err := share.OpenReader(file)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	tee := header.NewDigestTee()
	if _, err := io.CopyBuffer(tee, rc, make([]byte, 512*1024)); err != nil {
		t.Fatalf("local read: %v", err)
	}
	rc.Close()
	sess.Close()
	whole, data := tee.Sums()

	// The remote, on the fake tunnel, with the container as its one ASIAIR.
	p, err := tunnel.Mint(tunnel.FakeRegion())
	if err != nil {
		t.Fatal(err)
	}
	reply := []byte(`{"method":"scan_air","result":{"guid":"int-guid","cpuId":"int-cpu"},"code":0}`)
	local := netsite.NewRegistry(nil).Site(netsite.Local)
	var fake *tunnel.Fake
	ready := make(chan struct{})
	r, err := role.New(role.Config{
		Getenv:     func(k string) string { return map[string]string{role.EnvVar: p.EnvValue.Reveal()}[k] },
		Sites:      scanSites{scanSite{Site: local, reply: reply, src: h}},
		HealthAddr: "127.0.0.1:0",
		StartServer: func(cfg tunnel.ServerConfig) (tunnel.Server, error) {
			f, err := tunnel.NewFake(cfg)
			fake = f
			close(ready)
			return f, err
		},
	})
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	go r.Run(rctx)
	<-ready
	client := proto.NewClient(fake.NewClient().DialProtocol, nil)
	defer client.Close()

	if _, err := client.Broadcast(ctx, "int-b-1"); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	hr, err := client.Hash(ctx, "int-h-1", proto.HashRequest{IP: h, Share: shareName, Path: file, User: "guest", Password: "12345678"},
		proto.HashOptions{Stall: 30 * time.Second})
	if err != nil {
		t.Fatalf("remote Hash: %v", err)
	}
	// The seeded fixture is header-only, so its data digest is the empty
	// data unit's; the unit test covers a frame with pixels.
	if hr.Whole != whole || hr.Data != data {
		t.Errorf("remote digests (%s, %s), want Commander's read's (%s, %s)", hr.Whole, hr.Data, whole, data)
	}
	if hr.Size != fi.Size || !hr.Mtime().Equal(fi.Mtime) {
		t.Errorf("remote stat (%d, %v), want (%d, %v)", hr.Size, hr.Mtime(), fi.Size, fi.Mtime)
	}
	if hr.GUID != "int-guid" || hr.CPUID != "int-cpu" {
		t.Errorf("remote identity (%q, %q), want the probed one", hr.GUID, hr.CPUID)
	}
}
