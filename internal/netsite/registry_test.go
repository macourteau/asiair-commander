package netsite

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingListener counts every TCP connection and UDP datagram that reaches
// 127.0.0.1 on one port, standing in for a device on the LAN.
type recordingListener struct {
	port    int
	tcp     net.Listener
	udp     net.PacketConn
	tcpHits atomic.Int64
	udpHits atomic.Int64
	wg      sync.WaitGroup
}

func newRecordingListener(t *testing.T) *recordingListener {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		tcp.Close()
		t.Fatalf("listen udp: %v", err)
	}
	l := &recordingListener{port: port, tcp: tcp, udp: udp}
	l.wg.Add(2)
	go func() {
		defer l.wg.Done()
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			l.tcpHits.Add(1)
			c.Close()
		}
	}()
	go func() {
		defer l.wg.Done()
		buf := make([]byte, 512)
		for {
			if _, _, err := udp.ReadFrom(buf); err != nil {
				return
			}
			l.udpHits.Add(1)
		}
	}()
	t.Cleanup(func() {
		tcp.Close()
		udp.Close()
		l.wg.Wait()
	})
	return l
}

// TestSitesUnknownSiteNeverDialsLAN (design remote-sites AC2, invariant 4): an
// endpoint naming an unknown site, a removed site, or a site that is not up
// fails its dial with a net.Error and its probe without a send, and nothing
// reaches the LAN — not the recording listener standing in for the device, and
// not the site itself.
func TestSitesUnknownSiteNeverDialsLAN(t *testing.T) {
	lan := newRecordingListener(t)
	sites := NewRegistry(testLogger())

	removed := NewFake("removed")
	if err := sites.Add(removed); err != nil {
		t.Fatalf("add: %v", err)
	}
	sites.Remove("removed")

	down := NewFake("down")
	down.SetState(StateDown)
	if err := sites.Add(down); err != nil {
		t.Fatalf("add: %v", err)
	}

	for _, id := range []ID{"never-added", "removed", "down"} {
		site := sites.Site(id)
		conn, err := site.DialTCP(context.Background(), "127.0.0.1", lan.port, time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("site %q: dial connected, want refused", id)
		}
		var ne net.Error
		if !errors.As(err, &ne) {
			t.Fatalf("site %q: dial error %T %v is not a net.Error", id, err, err)
		}
		var ua *UnavailableError
		if !errors.As(err, &ua) || ua.Site != id {
			t.Fatalf("site %q: dial error %v, want an UnavailableError naming the site", id, err)
		}
		if site.ProbeUDP(context.Background(), "127.0.0.1", lan.port, []byte("probe"), 100*time.Millisecond,
			func([]byte, string) bool { return true }) {
			t.Fatalf("site %q: probe answered, want refused", id)
		}
	}
	if got := removed.Calls(); len(got) != 0 {
		t.Fatalf("removed site carried %v, want nothing", got)
	}
	if got := down.Calls(); len(got) != 0 {
		t.Fatalf("down site carried %v, want nothing", got)
	}

	// The listener does record traffic: the local site reaches it, so the zero
	// counts above mean nothing was sent rather than that nothing could arrive.
	conn, err := sites.Site(Local).DialTCP(context.Background(), "127.0.0.1", lan.port, time.Second)
	if err != nil {
		t.Fatalf("local dial: %v", err)
	}
	conn.Close()
	sites.Site(Local).ProbeUDP(context.Background(), "127.0.0.1", lan.port, []byte("probe"), 200*time.Millisecond,
		func([]byte, string) bool { return false })
	deadline := time.Now().Add(2 * time.Second)
	for (lan.tcpHits.Load() < 1 || lan.udpHits.Load() < 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tcp, udp := lan.tcpHits.Load(), lan.udpHits.Load(); tcp != 1 || udp != 1 {
		t.Fatalf("LAN saw %d connections and %d datagrams, want exactly the local site's one of each", tcp, udp)
	}

	// A down site that comes back up carries traffic again, so the refusal is
	// the state, read at dial time.
	down.SetState(StateUp)
	if _, err := sites.Site("down").DialTCP(context.Background(), "127.0.0.1", lan.port, time.Second); err == nil {
		t.Fatal("the fake site has no dialer, want its own error")
	}
	if got := down.Calls(); len(got) != 1 {
		t.Fatalf("up site carried %v, want the one dial", got)
	}
}

func TestRegistryAddRemove(t *testing.T) {
	sites := NewRegistry(testLogger())
	if err := sites.Add(NewFake(Local)); err == nil {
		t.Fatal("Add replaced the local site, want refused")
	}
	if err := sites.Add(NewFake("bad.id")); err == nil {
		t.Fatal("Add took an id with a dot, want refused")
	}
	if err := sites.Add(NewFake("r1")); err != nil {
		t.Fatalf("Add(r1): %v", err)
	}
	if err := sites.Add(NewFake("r1")); err == nil {
		t.Fatal("Add(r1) twice succeeded, want refused")
	}
	sites.Remove(Local)
	if s := sites.Site(Local); s.State() != StateUp || s.ID() != Local {
		t.Fatalf("local site after Remove(Local): id=%q state=%v, want it still up", s.ID(), s.State())
	}
	if s := sites.Site("r1"); s.ID() != "r1" || s.State() != StateUp {
		t.Fatalf("Site(r1) id=%q state=%v", s.ID(), s.State())
	}
	sites.Remove("r1")
	if s := sites.Site("r1"); s.State() != StateDown {
		t.Fatalf("removed site state %v, want down", s.State())
	}
}

// TestRegistryConcurrentLookup runs lookups and dials against sites being added
// and removed; the race detector is the assertion.
func TestRegistryConcurrentLookup(t *testing.T) {
	sites := NewRegistry(testLogger())
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := sites.Site("r1")
				_, _ = s.DialTCP(context.Background(), "127.0.0.1", 1, time.Millisecond)
				_ = s.State()
				_ = s.Costs()
			}
		}()
	}
	for i := 0; i < 200; i++ {
		_ = sites.Add(NewFake("r1"))
		sites.Remove("r1")
	}
	close(stop)
	wg.Wait()
}

// TestRegistryAllGatesBroadcast: All lists the local site first and every
// registered site after it, and a listed site that is not up refuses its
// broadcast round with a net.Error rather than running it.
func TestRegistryAllGatesBroadcast(t *testing.T) {
	r := NewRegistry(nil)
	b, a := NewFake("b"), NewFake("a")
	a.SetBroadcast([]Datagram{{Data: []byte("x"), SrcIP: "192.168.7.1"}}, nil)
	for _, s := range []Site{b, a} {
		if err := r.Add(s); err != nil {
			t.Fatalf("add %q: %v", s.ID(), err)
		}
	}

	all := r.All()
	var ids []ID
	for _, s := range all {
		ids = append(ids, s.ID())
	}
	if len(ids) != 3 || ids[0] != Local || ids[1] != "a" || ids[2] != "b" {
		t.Fatalf("All ids = %q, want [\"\" a b]", ids)
	}
	if got := all[0].Costs().MinLatency; got != 0 {
		t.Fatalf("local MinLatency = %v, want 0", got)
	}
	if all[0].Costs().RepliesMatchSource {
		t.Fatal("local RepliesMatchSource = true, want false")
	}

	if got, err := all[1].Broadcast(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("up site Broadcast = %v, %v; want its one scripted reply", got, err)
	}
	a.SetState(StateDown)
	got, err := all[1].Broadcast(context.Background())
	var ne net.Error
	if err == nil || !errors.As(err, &ne) || got != nil {
		t.Fatalf("down site Broadcast = %v, %v; want a net.Error and no replies", got, err)
	}
	if calls := a.Calls(); len(calls) != 1 {
		t.Fatalf("down site carried the round: calls = %v", calls)
	}
}
