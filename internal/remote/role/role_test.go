package role

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// TestRemoteRefusesWithoutPin: the role refuses to start with its pairing
// missing or unreadable (and so with no pinned Commander key), and while
// tailcat's address-printing knob is on; no refusal quotes the value.
func TestRemoteRefusesWithoutPin(t *testing.T) {
	good := pairingEnv(t)
	for name, vars := range map[string]map[string]string{
		"missing":          {},
		"empty":            {EnvVar: ""},
		"garbage":          {EnvVar: "acremote:!!!"},
		"bare address":     {EnvVar: "tc" + strings.Repeat("A", 40)},
		"truncated":        {EnvVar: good[:len(good)-6]},
		"debug addr on":    {EnvVar: good, tunnel.DebugAddrVar: "1"},
		"debug addr true":  {EnvVar: good, tunnel.DebugAddrVar: "true"},
		"debug addr bogus": {EnvVar: good, tunnel.DebugAddrVar: "yes please"},
	} {
		r, err := New(Config{Getenv: envOf(vars)})
		if err == nil {
			t.Errorf("%s: New succeeded", name)
			continue
		}
		if r != nil {
			t.Errorf("%s: New returned a remote with its error", name)
		}
		if v := vars[EnvVar]; len(v) > 8 && strings.Contains(err.Error(), v) {
			t.Errorf("%s: error quotes the env value", name)
		}
	}
	for name, vars := range map[string]map[string]string{
		"well-formed":    {EnvVar: good},
		"debug addr off": {EnvVar: good, tunnel.DebugAddrVar: "0"},
	} {
		if _, err := New(Config{Getenv: envOf(vars)}); err != nil {
			t.Errorf("%s: New: %v", name, err)
		}
	}
}

// TestForwardPolicy (role half): nothing is forwarded before the first
// broadcast round; afterwards only TCP 445, TCP 4700 and UDP 4720, and only to
// allowlisted addresses, through the policy and through the tunnel.
func TestForwardPolicy(t *testing.T) {
	device := listen(t)
	rr := startRemote(t, startOpts{cfg: Config{
		DialLAN: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, device)
		},
	}})
	asiair := netip.MustParseAddr("192.0.2.10")
	other := netip.MustParseAddr("192.0.2.66")

	for _, c := range []struct {
		p    tunnel.Proto
		port uint16
	}{{tunnel.TCP, 445}, {tunnel.TCP, 4700}, {tunnel.UDP, 4720}} {
		if rr.r.policy(c.p, asiair, c.port) {
			t.Errorf("before any broadcast, policy allows %s %v:%d", c.p, asiair, c.port)
		}
	}
	if c, err := rr.tc.DialTCP(ctxFor(t, 5*time.Second), asiair, 445); err == nil {
		c.Close()
		t.Fatal("a flow was forwarded before the first broadcast round")
	}

	rr.lan.site.SetBroadcast([]netsite.Datagram{{Data: scanReply("g1", "c1", "192.0.2.10"), SrcIP: "192.0.2.10"}}, nil)
	if _, err := rr.client.Broadcast(ctxFor(t, 5*time.Second), "b-1"); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	for _, c := range []struct {
		p    tunnel.Proto
		ip   netip.Addr
		port uint16
		want bool
	}{
		{tunnel.TCP, asiair, 445, true},
		{tunnel.TCP, asiair, 4700, true},
		{tunnel.UDP, asiair, 4720, true},
		{tunnel.TCP, asiair, 4720, false},
		{tunnel.UDP, asiair, 445, false},
		{tunnel.TCP, asiair, 22, false},
		{tunnel.TCP, asiair, 80, false},
		{tunnel.UDP, asiair, 53, false},
		{tunnel.TCP, other, 445, false},
		{tunnel.UDP, other, 4720, false},
	} {
		if got := rr.r.policy(c.p, c.ip, c.port); got != c.want {
			t.Errorf("policy(%s %v:%d) = %v, want %v", c.p, c.ip, c.port, got, c.want)
		}
	}

	c, err := rr.tc.DialTCP(ctxFor(t, 5*time.Second), asiair, 445)
	if err != nil {
		t.Fatalf("an allowlisted flow was refused: %v", err)
	}
	c.Close()
	if c, err := rr.tc.DialTCP(ctxFor(t, 5*time.Second), other, 445); err == nil {
		c.Close()
		t.Error("a flow to an address off the allowlist was forwarded")
	}
	if c, err := rr.tc.DialTCP(ctxFor(t, 5*time.Second), asiair, 22); err == nil {
		c.Close()
		t.Error("a flow to a port never forwarded was forwarded")
	}
}

// TestRemoteProtocolPolicy: hash refuses an address off the allowlist without
// touching the LAN; broadcast sends only the remote's own request, whatever
// the request frame carries; the allowlist admits a reply whose result.ip is
// empty or equal to its source and no other; membership never expires; and a
// new remote starts with an empty list.
func TestRemoteProtocolPolicy(t *testing.T) {
	clock := newFakeClock()
	rr := startRemote(t, startOpts{cfg: Config{Now: clock.Now}})

	// hash before any broadcast: refused, and nothing on the LAN is touched.
	_, err := rr.client.Hash(ctxFor(t, 5*time.Second), "h-1", proto.HashRequest{IP: "192.0.2.10", Share: "s", Path: "p"}, proto.HashOptions{Stall: 5 * time.Second})
	if proto.CodeOf(err) != proto.CodeRefused {
		t.Fatalf("hash before any broadcast = %v, want refused", err)
	}
	if ev := rr.lan.Events(); len(ev) != 0 {
		t.Fatalf("a refused hash touched the LAN: %v", ev)
	}

	rr.lan.site.SetBroadcast([]netsite.Datagram{
		{Data: scanReply("g-match", "c1", "192.0.2.10"), SrcIP: "192.0.2.10"},
		{Data: scanReply("g-noip", "c2", ""), SrcIP: "192.0.2.11"},
		{Data: scanReply("g-mismatch", "c3", "192.0.2.50"), SrcIP: "192.0.2.12"},
		{Data: []byte(`{"method":"scan_air","result":{"guid":""},"code":0}`), SrcIP: "192.0.2.13"},
		{Data: []byte(`not a scan_air reply`), SrcIP: "192.0.2.14"},
		{Data: scanReply("g-v6", "c4", ""), SrcIP: "2001:db8::15"},
		{Data: scanReply("g-mapped", "c5", "::ffff:192.0.2.16"), SrcIP: "192.0.2.16"},
	}, nil)

	// A request frame carrying bytes of its own: the remote ignores them and
	// runs its own round.
	conn, err := rr.tc.DialProtocol(ctxFor(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte(`{"v":1,"call_id":"b-raw","op":"broadcast","request":"eyJtZXRob2QiOiJyZWJvb3QifQ==","data":"x"}` + "\n"))
	buf := make([]byte, 64<<10)
	n, err := conn.Read(buf)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Kind      string               `json:"kind"`
		Broadcast proto.BroadcastReply `json:"broadcast"`
	}
	if err := json.Unmarshal(buf[:n], &rep); err != nil || rep.Kind != "result" || len(rep.Broadcast.Replies) != 7 {
		t.Fatalf("broadcast reply %s (%v), want all 7 raw replies", buf[:n], err)
	}
	if ev := rr.lan.Events(); len(ev) != 1 || ev[0] != "broadcast" {
		t.Fatalf("LAN calls = %v, want one broadcast round", ev)
	}

	allowed := map[string]bool{"192.0.2.10": true, "192.0.2.11": true, "192.0.2.16": true}
	for _, ip := range []string{"192.0.2.10", "192.0.2.11", "192.0.2.12", "192.0.2.50", "192.0.2.13", "192.0.2.14", "2001:db8::15", "192.0.2.16"} {
		if got := rr.r.policy(tunnel.TCP, netip.MustParseAddr(ip), 445); got != allowed[ip] {
			t.Errorf("after the round, %s allowlisted = %v, want %v", ip, got, allowed[ip])
		}
	}

	// No expiry, and a failed round removes nothing.
	clock.Advance(30 * 24 * time.Hour)
	rr.lan.site.SetBroadcast(nil, errors.New("network is unreachable"))
	if _, err := rr.client.Broadcast(ctxFor(t, 5*time.Second), "b-2"); proto.CodeOf(err) != proto.CodeNetwork {
		t.Fatalf("failed broadcast = %v, want a network error", err)
	}
	if !rr.r.policy(tunnel.TCP, netip.MustParseAddr("192.0.2.10"), 445) {
		t.Error("an allowlisted address expired")
	}
	_, err = rr.client.Hash(ctxFor(t, 5*time.Second), "h-2", proto.HashRequest{IP: "192.0.2.12", Share: "s", Path: "p"}, proto.HashOptions{Stall: 5 * time.Second})
	if proto.CodeOf(err) != proto.CodeRefused {
		t.Errorf("hash to a mismatching reply's source = %v, want refused", err)
	}

	// A restart is a new remote: its list starts empty.
	rr.stop(t)
	fresh := startRemote(t, startOpts{})
	if fresh.r.policy(tunnel.TCP, netip.MustParseAddr("192.0.2.10"), 445) {
		t.Error("a new remote instance starts with a non-empty allowlist")
	}
}

// TestRemoteHello: hello reports uptime and warning ages from the remote's
// clock, never a timestamp; the instance id is stable within a process and
// differs between processes; warnings carry increasing sequence numbers, the
// call id of the call that raised them, and the detail the card shows.
func TestRemoteHello(t *testing.T) {
	clock := newFakeClock()
	build := proto.Build{Version: "v9.9.9", Commit: "abc", Date: "2026-10-01T00:00:00Z"}
	// Every LAN target refuses, as a powered-off device does.
	refuse := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	rr := startRemote(t, startOpts{cfg: Config{Now: clock.Now, Build: build, DialLAN: refuse}})

	h1, rtt, err := rr.client.Hello(ctxFor(t, 5*time.Second), "hello-1")
	if err != nil {
		t.Fatal(err)
	}
	if rtt <= 0 {
		t.Errorf("rtt = %v", rtt)
	}
	if h1.InstanceID == "" || len(h1.Versions) == 0 || h1.Versions[0] != proto.V1 || h1.Build != build {
		t.Errorf("hello = %+v", h1)
	}
	if h1.WholeDigestVersion == 0 || h1.DataDigestVersion == 0 {
		t.Errorf("hello reports no digest versions: %+v", h1)
	}
	if !h1.Path.Connected || !h1.Path.Direct {
		t.Errorf("hello path = %+v, want the fake's direct path", h1.Path)
	}
	if h1.Uptime() != 0 {
		t.Errorf("uptime = %v at start", h1.Uptime())
	}

	// A broadcast failure, a refused forward and a foreign key, each a minute
	// apart.
	rr.lan.site.SetBroadcast(nil, errors.New("open broadcast socket: network is unreachable"))
	rr.client.Broadcast(ctxFor(t, 5*time.Second), "bcast-7")
	clock.Advance(time.Minute)
	if c, err := rr.tc.DialTCP(ctxFor(t, 5*time.Second), netip.MustParseAddr("192.0.2.77"), 445); err == nil {
		c.Close()
		t.Fatal("forwarded before any broadcast")
	}
	clock.Advance(time.Minute)
	rr.fake.Reject("nodekey:" + strings.Repeat("1", 64))
	deadline := time.Now().Add(5 * time.Second)
	for len(rr.r.warnings.snapshot(clock.Now())) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	clock.Advance(time.Minute)

	h2, _, err := rr.client.Hello(ctxFor(t, 5*time.Second), "hello-2")
	if err != nil {
		t.Fatal(err)
	}
	if h2.InstanceID != h1.InstanceID {
		t.Error("instance id changed within one process")
	}
	if h2.Uptime() != 3*time.Minute {
		t.Errorf("uptime = %v, want 3m", h2.Uptime())
	}
	if len(h2.Warnings) != 3 {
		t.Fatalf("warnings = %+v, want 3", h2.Warnings)
	}
	want := []struct {
		kind proto.WarningKind
		age  time.Duration
	}{
		{proto.WarnBroadcastFailed, 3 * time.Minute},
		{proto.WarnRefusedForward, 2 * time.Minute},
		{proto.WarnRejectedClient, time.Minute},
	}
	for i, w := range h2.Warnings {
		if w.Seq != uint64(i+1) || w.Kind != want[i].kind || w.Age() != want[i].age {
			t.Errorf("warning %d = %+v, want seq %d, %s, age %v", i, w, i+1, want[i].kind, want[i].age)
		}
	}
	if w := h2.Warnings[0]; w.CallID != "bcast-7" || !strings.Contains(w.Reason, "unreachable") {
		t.Errorf("broadcast warning = %+v, want call id bcast-7 and its reason", w)
	}
	if w := h2.Warnings[1]; w.Proto != "tcp" || w.Target != "192.0.2.77:445" || !strings.Contains(w.Reason, "no broadcast round") {
		t.Errorf("refused-forward warning = %+v", w)
	}
	if w := h2.Warnings[2]; !strings.HasPrefix(w.ClientKey, "nodekey:1111") {
		t.Errorf("rejected-client warning = %+v", w)
	}

	// No reply carries a timestamp of the remote's clock: the only date is the
	// build's.
	b, _ := json.Marshal(h2)
	if strings.Contains(strings.ReplaceAll(string(b), build.Date, ""), "2026-") {
		t.Errorf("hello carries a timestamp: %s", b)
	}

	// A powered-off but allowlisted device is not a warning.
	rr.lan.site.SetBroadcast([]netsite.Datagram{{Data: scanReply("g1", "c1", ""), SrcIP: "192.0.2.10"}}, nil)
	if _, err := rr.client.Broadcast(ctxFor(t, 5*time.Second), "bcast-8"); err != nil {
		t.Fatal(err)
	}
	if c, err := rr.tc.DialTCP(ctxFor(t, 10*time.Second), netip.MustParseAddr("192.0.2.10"), 445); err == nil {
		c.Close()
	}
	if n := len(rr.r.warnings.snapshot(clock.Now())); n != 3 {
		t.Errorf("a dial to an allowlisted device raised a warning (%d warnings)", n)
	}

	other := startRemote(t, startOpts{})
	h3, _, err := other.client.Hello(ctxFor(t, 5*time.Second), "hello-3")
	if err != nil {
		t.Fatal(err)
	}
	if h3.InstanceID == h1.InstanceID {
		t.Error("two remote instances share an instance id")
	}
}

// TestWarningsBounded: the list keeps the most recent entries, numbered on.
func TestWarningsBounded(t *testing.T) {
	ws := warnings{max: 3}
	now := time.Now()
	for range 5 {
		ws.add(now, proto.Warning{Kind: proto.WarnBroadcastFailed})
	}
	got := ws.snapshot(now)
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Errorf("snapshot = %+v, want seqs 3..5", got)
	}
}

// homeServer overrides a server's relay status.
type homeServer struct {
	tunnel.Server
	home func() bool
}

func (h homeServer) HomeConnected() bool { return h.home() }

// TestRemoteExitRule: the remote exits after ExitAfter without its relay home
// region, and never for want of a hello.
func TestRemoteExitRule(t *testing.T) {
	t.Run("rule", func(t *testing.T) {
		start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
		rule := exitRule{after: 10 * time.Minute, lastHome: start}
		if rule.expired(start.Add(9*time.Minute), false) {
			t.Error("expired before the window")
		}
		if rule.expired(start.Add(9*time.Minute+30*time.Second), true) {
			t.Error("expired while connected")
		}
		// The window restarts at the last connected reading.
		if rule.expired(start.Add(19*time.Minute), false) {
			t.Error("expired before a full window since the last connection")
		}
		if !rule.expired(start.Add(19*time.Minute+30*time.Second), false) {
			t.Error("did not expire after a full window without the relay")
		}
	})

	t.Run("exits without relay", func(t *testing.T) {
		clock := newFakeClock()
		rr := startRemote(t, startOpts{
			cfg:  Config{Now: clock.Now, ExitAfter: 10 * time.Minute, ExitCheckInterval: 5 * time.Millisecond},
			wrap: func(s tunnel.Server) tunnel.Server { return homeServer{s, func() bool { return false }} },
		})
		clock.Advance(9 * time.Minute)
		select {
		case err := <-rr.done:
			t.Fatalf("exited before the window: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		clock.Advance(time.Minute)
		select {
		case err := <-rr.done:
			if !errors.Is(err, ErrRelayUnreachable) {
				t.Fatalf("Run = %v, want ErrRelayUnreachable", err)
			}
			rr.cancel = nil
		case <-time.After(5 * time.Second):
			t.Fatal("did not exit after the window without the relay")
		}
	})

	t.Run("no hello never exits", func(t *testing.T) {
		clock := newFakeClock()
		rr := startRemote(t, startOpts{cfg: Config{Now: clock.Now, ExitAfter: time.Minute, ExitCheckInterval: 5 * time.Millisecond}})
		// A day without a single hello, relay connected.
		for range 24 * 60 {
			clock.Advance(time.Minute)
		}
		select {
		case err := <-rr.done:
			t.Fatalf("exited with the relay up and no hello: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if err := rr.stop(t); err != nil {
			t.Fatalf("Run after cancel = %v", err)
		}
	})
}

// TestRemoteHealthcheck: healthy only while the tunnel server runs with its
// pin and the relay home region is connected; never depends on Commander.
func TestRemoteHealthcheck(t *testing.T) {
	var home atomic.Bool
	home.Store(true)
	rr := startRemote(t, startOpts{wrap: func(s tunnel.Server) tunnel.Server { return homeServer{s, home.Load} }})
	var addr string
	for addr == "" {
		rr.r.mu.RLock()
		addr = rr.r.healthAddr
		rr.r.mu.RUnlock()
		time.Sleep(time.Millisecond)
	}
	// No hello has ever been answered.
	if code := Healthcheck(addr); code != 0 {
		t.Errorf("healthcheck = %d with the server up and the relay connected", code)
	}
	home.Store(false)
	if code := Healthcheck(addr); code == 0 {
		t.Error("healthy without the relay home region")
	}
	home.Store(true)
	rr.stop(t)
	if code := Healthcheck(addr); code == 0 {
		t.Error("healthy after the remote stopped")
	}
}

// TestRemoteRoleIsStateless: the role's package imports neither the store nor
// the rig logic, and a remote that serves every call writes no file.
func TestRemoteRoleIsStateless(t *testing.T) {
	t.Run("imports", func(t *testing.T) {
		out, err := exec.Command("go", "list", "-deps", ".").Output()
		if err != nil {
			t.Fatalf("go list: %v", err)
		}
		deps := strings.Fields(string(out))
		if len(deps) < 10 {
			t.Fatalf("go list -deps returned %d packages", len(deps))
		}
		for _, d := range deps {
			for _, banned := range []string{"/internal/store", "/internal/rig", "/internal/web", "/internal/config"} {
				if strings.HasSuffix(d, banned) || strings.Contains(d, banned+"/") {
					t.Errorf("the remote role depends on %s", d)
				}
			}
		}
	})

	t.Run("writes no file", func(t *testing.T) {
		home := t.TempDir()
		work := t.TempDir()
		for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "TMPDIR", "ProgramData", "AppData", "LocalAppData"} {
			t.Setenv(k, home)
		}
		t.Chdir(work)

		rr := startRemote(t, startOpts{cfg: Config{ProgressInterval: 5 * time.Millisecond}})
		rr.lan.site.SetBroadcast([]netsite.Datagram{{Data: scanReply("g1", "c1", "192.0.2.10"), SrcIP: "192.0.2.10"}}, nil)
		rr.lan.site.SetProbeReply(scanReply("g1", "c1", "192.0.2.10"), "192.0.2.10")
		rr.lan.dev.AddFile("EMMC Images", "Light/a.fit", []byte(strings.Repeat("x", 100000)), time.Now())
		if _, _, err := rr.client.Hello(ctxFor(t, 5*time.Second), "h-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := rr.client.Broadcast(ctxFor(t, 5*time.Second), "b-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := rr.client.Hash(ctxFor(t, 5*time.Second), "x-1", proto.HashRequest{IP: "192.0.2.10", Share: "EMMC Images", Path: "Light/a.fit", User: "guest", Password: "pw"}, proto.HashOptions{Stall: 5 * time.Second}); err != nil {
			t.Fatal(err)
		}
		rr.fake.Reject("nodekey:" + strings.Repeat("2", 64))
		rr.stop(t)

		for _, dir := range []string{home, work} {
			filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if path != dir {
					t.Errorf("the remote wrote %s", path)
				}
				return nil
			})
		}
	})
}
