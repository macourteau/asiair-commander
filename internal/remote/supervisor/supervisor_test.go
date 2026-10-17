package supervisor

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// TestRemoteStateMachine walks a remote through Waiting → Connected →
// Reconnecting → Unreachable and back: a remote never heard from waits, its
// first hello and broadcast round connect it, a failing heartbeat makes it
// Reconnecting at once and Unreachable past unreachable_alert_after, measured
// from its last hello, and a restarted remote is reconnected by a rebuild.
func TestRemoteStateMachine(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")

	// The user has not started the container yet.
	h.beat(id)
	st := h.wantState(id, model.RemoteWaiting)
	if st.EverConnected || st.LastHelloAt != 0 {
		t.Fatalf("a never-connected remote reports a past: %+v", st)
	}
	if got := h.sites.Site(netsite.ID(id)).State(); got == netsite.StateUp {
		t.Fatalf("site %v while waiting", got)
	}

	f.start()
	h.connect(id, f)
	st = h.wantState(id, model.RemoteConnected)
	if !st.Direct || st.LatencyMS < 0 || st.Version != "v1.0.0" || st.UptimeS != 60 || !st.EverConnected {
		t.Fatalf("connected status: %+v", st)
	}
	connectedAt := st.LastHelloAt
	if connectedAt != h.clock.now().Unix() {
		t.Fatalf("last hello %d, want %d", connectedAt, h.clock.now().Unix())
	}
	if h.hub.last == nil || h.hub.last[0].State != model.RemoteConnected {
		t.Fatalf("hub not published: %+v", h.hub.last)
	}

	// The remote goes away.
	f.stop()
	h.clock.advance(time.Minute)
	h.beat(id)
	st = h.wantState(id, model.RemoteReconnecting)
	if st.LastHelloAt != connectedAt {
		t.Fatalf("a failed heartbeat moved last hello to %d", st.LastHelloAt)
	}
	if h.sites.Site(netsite.ID(id)).State() != netsite.StateDown {
		t.Fatal("site still up while the heartbeat fails")
	}

	h.clock.advance(15 * time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteUnreachable)

	// It comes back as a new process.
	f.start()
	h.clock.advance(rebuildBackoffMax + time.Second)
	h.connect(id, f)
	st = h.wantState(id, model.RemoteConnected)
	if st.Restarts != 1 {
		t.Fatalf("restarts %d, want 1", st.Restarts)
	}
}

// TestRemoteNeedsUpdate: a digest-algorithm mismatch reads Needs update, naming
// the older side, while dials continue.
func TestRemoteNeedsUpdate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		whole, data int
		want        string
	}{
		{"remote whole-file older", header.WholeDigestVersion - 1, header.DataDigestVersion, model.UpdateRemote},
		{"commander whole-file older", header.WholeDigestVersion + 1, header.DataDigestVersion, model.UpdateCommander},
		{"remote data-unit older", header.WholeDigestVersion, header.DataDigestVersion - 1, model.UpdateRemote},
		{"commander data-unit older", header.WholeDigestVersion, header.DataDigestVersion + 1, model.UpdateCommander},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id, f := h.add("Observatory")
			f.start()
			hd := f.handler()
			hd.mu.Lock()
			hd.whole, hd.data = tc.whole, tc.data
			hd.mu.Unlock()
			h.clock.advance(time.Hour)
			h.beat(id)
			h.beat(id)
			st := h.wantState(id, model.RemoteNeedsUpdate)
			if st.NeedsUpdate != tc.want {
				t.Fatalf("needs update %q, want %q", st.NeedsUpdate, tc.want)
			}
			if h.sites.Site(netsite.ID(id)).State() != netsite.StateUp {
				t.Fatal("a version mismatch stopped dials")
			}
			w, d, ok := h.remote(id).site.PeerDigestVersions()
			if !ok || w != tc.whole || d != tc.data {
				t.Fatalf("peer digest versions %d/%d/%v", w, d, ok)
			}
		})
	}
}

// TestRemoteHistorySurvivesRestart: a Commander restart neither resets
// "Unreachable since" nor turns a remote that connected before into Waiting.
func TestRemoteHistorySurvivesRestart(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	lastHello := h.status(id).LastHelloAt
	f.stop()
	h.sup.Stop()

	// Commander restarts ten minutes later; the remote is still down.
	h.clock.advance(10 * time.Minute)
	h.sup = h.newSupervisor()
	h.sup.Start(context.Background())
	st := h.wantState(id, model.RemoteReconnecting)
	if !st.EverConnected || st.LastHelloAt != lastHello {
		t.Fatalf("history lost across a restart: %+v", st)
	}
	h.sup.Stop()

	h.clock.advance(10 * time.Minute)
	h.sup = h.newSupervisor()
	h.sup.Start(context.Background())
	st = h.wantState(id, model.RemoteUnreachable)
	if st.LastHelloAt != lastHello {
		t.Fatalf("unreachable since moved to %d, want %d", st.LastHelloAt, lastHello)
	}
}

// TestRepairKeepsHistory: Re-pair mints a fresh pairing on the same id; the
// card waits for the remote to start with the new env, the old env stops
// working at once, and the connection history stays.
func TestRepairKeepsHistory(t *testing.T) {
	h := newHarness(t)
	id, old := h.add("Observatory")
	h.connect(id, old)
	before := h.status(id)
	if before.PairedAt != before.CreatedAt || before.CreatedAt == 0 {
		t.Fatalf("an added remote's pairing dates from its add: %+v", before)
	}

	h.clock.advance(time.Hour)
	p, err := h.sup.Repair(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if p.Remote.ID != id || p.EnvValue == "" || p.Remote.Fingerprint == before.Fingerprint {
		t.Fatalf("repair result: %+v", p.Remote)
	}
	// The card times its "still waiting" hint from the pairing, not the add.
	if p.Remote.PairedAt != h.clock.now().Unix() || p.Remote.CreatedAt != before.CreatedAt {
		t.Fatalf("re-pair time: paired %d created %d, want paired %d created %d",
			p.Remote.PairedAt, p.Remote.CreatedAt, h.clock.now().Unix(), before.CreatedAt)
	}
	if want := tunnel.EnvFile(tunnel.Secret(p.EnvValue)).Reveal(); p.EnvFile != want {
		t.Fatal("re-pair EnvFile is not the .env line for its env value")
	}
	st := h.wantState(id, model.RemoteWaiting)
	if !st.EverConnected || st.LastHelloAt != before.LastHelloAt {
		t.Fatalf("re-pair lost the history: %+v", st)
	}
	if h.sites.Site(netsite.ID(id)).State() != netsite.StateDown {
		t.Fatal("site up on the old pairing after a re-pair")
	}
	if prof := h.cfg.Load().Remote(id); prof.Region != "fake" {
		t.Fatalf("re-pair changed the region: %+v", prof)
	}

	// The old remote keeps running on the old env and is never reached.
	h.clock.advance(time.Hour)
	h.beat(id)
	h.wantState(id, model.RemoteWaiting)

	// The user installs the new env and restarts the container.
	old.stop()
	f := h.install(p)
	h.connect(id, f)
	st = h.wantState(id, model.RemoteConnected)
	if st.Fingerprint != p.Remote.Fingerprint || st.Restarts != before.Restarts {
		t.Fatalf("after re-pair: %+v", st)
	}

	// Neither the env value nor a stored secret reaches the log.
	prof := h.cfg.Load().Remote(id)
	for _, secret := range []string{p.EnvValue, prof.Address, prof.ClientKey} {
		if strings.Contains(h.logs.String(), secret) {
			t.Fatal("a pairing secret reached the log")
		}
	}
}

// TestPairedAtFallsBackToCreatedAt: a config written before the pairing time
// was recorded dates the pairing from the add.
func TestPairedAtFallsBackToCreatedAt(t *testing.T) {
	h := newHarness(t)
	id, _ := h.add("Observatory")
	err := h.sup.commit(func(c *model.Config) error {
		c.Remote(id).PairedAt = 0
		return nil
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if st := h.status(id); st.PairedAt == 0 || st.PairedAt != st.CreatedAt {
		t.Fatalf("paired %d, want created %d", st.PairedAt, st.CreatedAt)
	}
}

// TestPairingConfigChangeRebuilds: a saved edit to any field of the pairing
// config, found by reflection so a field added later is covered too, rebuilds
// the tunnel client as a re-pair; a rename or a cap edit keeps the client.
func TestPairingConfigChangeRebuilds(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	var builds atomic.Int32
	h.mu.Lock()
	h.onBuild = func() { builds.Add(1) }
	h.mu.Unlock()

	edit := func(change func(*model.RemoteProfile)) {
		t.Helper()
		if err := h.sup.commit(func(c *model.Config) error {
			change(c.Remote(id))
			return nil
		}); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	gen := func() int {
		r := h.remote(id)
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.gen
	}

	before, g0 := builds.Load(), gen()
	edit(func(p *model.RemoteProfile) { p.Name, p.RateLimitMBps = "Renamed", 3 })
	if builds.Load() != before || gen() != g0 {
		t.Fatal("a rename or cap edit rebuilt the tunnel client")
	}
	h.wantState(id, model.RemoteConnected)

	fields := reflect.TypeFor[model.RemotePairingConfig]()
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		before, g0 := builds.Load(), gen()
		edit(func(p *model.RemoteProfile) {
			v := reflect.ValueOf(&p.RemotePairingConfig).Elem().Field(i)
			if v.Kind() != reflect.String {
				t.Fatalf("pairing field %s is a %s; teach this test to change it", name, v.Kind())
			}
			v.SetString(v.String() + "-changed")
		})
		if builds.Load() != before+1 || gen() != g0+1 {
			t.Fatalf("changing pairing field %s did not rebuild the client as a re-pair", name)
		}
	}
}

// TestRebuildSparesLiveTransfer is design remote-sites AC7's unit half: a
// failing heartbeat does not rebuild the client while a connection through it
// moves bytes, nor while a remote hash reports progress; once both have been
// quiet past the SMB budget it does.
func TestRebuildSparesLiveTransfer(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	r := h.remote(id)
	budget := netsite.SMBBudget(r.site)
	if budget < netsite.RemoteStallWindow {
		t.Fatalf("budget %v below the remote stall window", budget)
	}

	conn, err := h.sites.Site(netsite.ID(id)).DialTCP(context.Background(), lanDevice, 445, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	device := <-f.lanEnds
	defer device.Close()
	go io.Copy(io.Discard, device)

	c, _ := r.site.current()
	c.(*flakyClient).failProtocol()
	rebuilds := h.clientsBuilt(id)

	// A transfer keeps moving bytes across ten minutes of failed heartbeats.
	for i := 0; i < 10; i++ {
		h.clock.advance(budget / 2)
		if _, err := conn.Write([]byte("frame bytes")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		h.beat(id)
	}
	if got := h.clientsBuilt(id); got != rebuilds {
		t.Fatalf("rebuilt %d times while bytes moved", got-rebuilds)
	}
	if _, err := conn.Write([]byte("still alive")); err != nil {
		t.Fatalf("the transfer was cut: %v", err)
	}

	// A remote hash's progress counts as bytes moving.
	for i := 0; i < 4; i++ {
		h.clock.advance(budget / 2)
		r.site.HashProgress(int64(i) << 20)
		h.beat(id)
	}
	if got := h.clientsBuilt(id); got != rebuilds {
		t.Fatalf("rebuilt %d times while a hash progressed", got-rebuilds)
	}

	// Both quiet: within the budget nothing happens, past it the client is
	// rebuilt and the stale connection ends with it.
	h.clock.advance(budget - time.Second)
	h.beat(id)
	if got := h.clientsBuilt(id); got != rebuilds {
		t.Fatal("rebuilt before the budget elapsed")
	}
	h.clock.advance(2 * time.Second)
	h.beat(id)
	if got := h.clientsBuilt(id); got != rebuilds+1 {
		t.Fatalf("rebuilds %d, want %d", got, rebuilds+1)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Fatal("a connection through the replaced client still writes")
	}
	// The fresh client reaches the remote, which never went away.
	h.beat(id)
	h.wantState(id, model.RemoteConnected)
}

// TestRemoteRestartRecovers: after the remote restarts, the old client stays
// dead (as tailcat's does), and the supervisor rebuilds and reconnects once
// the SMB budget has passed with nothing moving.
func TestRemoteRestartRecovers(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	rebuilds := h.clientsBuilt(id)

	f.start() // the container restarts: a new process, the old client dead
	h.clock.advance(time.Second)
	h.beat(id)
	h.wantState(id, model.RemoteReconnecting)
	if h.clientsBuilt(id) != rebuilds {
		t.Fatal("rebuilt within the budget of the last hello")
	}
	h.clock.advance(time.Second)
	h.beat(id)
	h.wantState(id, model.RemoteReconnecting) // the old client never recovers by itself

	h.clock.advance(netsite.RemoteStallWindow)
	h.beat(id) // fails, then rebuilds
	if h.clientsBuilt(id) != rebuilds+1 {
		t.Fatalf("no rebuild past the budget")
	}
	h.beat(id)
	st := h.wantState(id, model.RemoteConnected)
	if st.Restarts != 1 {
		t.Fatalf("restarts %d, want 1", st.Restarts)
	}
	if f.handler().broadcastCount() != 1 {
		t.Fatalf("the restarted remote got %d broadcast rounds, want 1", f.handler().broadcastCount())
	}
}

// TestReconnectRound: the site is not up until the broadcast round after a
// (re)connect has completed, and no broadcast is sent while the tunnel is down.
func TestReconnectRound(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	f.start()
	hd := f.handler()
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	hd.mu.Lock()
	hd.gate, hd.entered = gate, entered
	hd.mu.Unlock()

	h.clock.advance(time.Hour) // past the rebuild window of the client built at Add
	h.beat(id)                 // the client built before the remote started is dead
	done := make(chan struct{})
	go func() { h.beat(id); close(done) }()
	<-entered
	// The hello succeeded and the round is running: still down.
	site := h.sites.Site(netsite.ID(id))
	if site.State() != netsite.StateDown {
		t.Fatal("site up before its broadcast round completed")
	}
	if _, err := site.DialTCP(context.Background(), lanDevice, 445, time.Second); err == nil {
		t.Fatal("a dial went through before the round")
	}
	close(gate)
	<-done
	if site.State() != netsite.StateUp {
		t.Fatalf("site not up after its round: %+v", h.status(id))
	}
	if n := hd.broadcastCount(); n != 1 {
		t.Fatalf("broadcast rounds %d, want 1", n)
	}

	// A steady heartbeat runs no further round.
	h.beat(id)
	if n := hd.broadcastCount(); n != 1 {
		t.Fatalf("broadcast rounds %d after a steady heartbeat, want 1", n)
	}

	// The tunnel drops: no broadcast is sent while it is down, from the
	// supervisor or through the site.
	c, _ := h.remote(id).site.current()
	c.(*flakyClient).failProtocol()
	h.beat(id)
	h.beat(id)
	if _, err := site.Broadcast(context.Background()); err == nil {
		t.Fatal("a broadcast went through a down site")
	}
	if n := hd.broadcastCount(); n != 1 {
		t.Fatalf("broadcast rounds %d while down, want 1", n)
	}
}

// TestNewRequiresForgetEndpoints: a supervisor wired without the endpoint
// cleanup hook is refused, so no wiring can remove a remote and leave its rigs'
// lastip: keys behind.
func TestNewRequiresForgetEndpoints(t *testing.T) {
	sup, err := New(Options{})
	if err == nil || sup != nil {
		t.Fatalf("New without ForgetEndpoints = %v, %v; want an error", sup, err)
	}
}

// TestRemoveRemoteClearsLastIP is design remote-sites AC11's supervisor half:
// Remove stops dialling at once, drops the site's discovery sightings, asks the
// rigs to forget every last-known endpoint naming the site (the rig side,
// rig.TestForgetLastEndpointsOnSite, clears those and no other), and the id is
// never reused.
func TestRemoveRemoteClearsLastIP(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	other, g := h.add("Cabin")
	h.connect(id, f)
	h.connect(other, g)

	cfg := *h.cfg.Load()
	cfg.Rigs = []model.RigProfile{{ID: "rig-a", Name: "A"}, {ID: "rig-b", Name: "B"}, {ID: "rig-c", Name: "C"}}
	h.cfg.Store(&cfg)
	ctx := context.Background()

	conn, err := h.sites.Site(netsite.ID(id)).DialTCP(ctx, lanDevice, 445, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	<-f.lanEnds

	if err := h.sup.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	_, err = h.sites.Site(netsite.ID(id)).DialTCP(ctx, lanDevice, 445, time.Second)
	var ue *netsite.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("dial after Remove: %v, want an unavailable site", err)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Fatal("an open connection through the removed remote still writes")
	}
	if len(h.disc.dropped) != 1 || h.disc.dropped[0] != netsite.ID(id) {
		t.Fatalf("discovery drops %v, want [%s]", h.disc.dropped, id)
	}
	h.mu.Lock()
	forgets := h.forgets
	h.mu.Unlock()
	if len(forgets) != 1 || forgets[0].site != netsite.ID(id) || strings.Join(forgets[0].rigIDs, ",") != "rig-a,rig-b,rig-c" {
		t.Fatalf("ForgetEndpoints calls %+v, want one naming site %s and every rig", forgets, id)
	}
	if v, _, _ := h.st.GetMeta(ctx, historyKey(id)); v != "" {
		t.Errorf("history left behind: %q", v)
	}
	if h.cfg.Load().Remote(id) != nil {
		t.Fatal("the profile is still in the config")
	}
	if err := h.sup.Remove(id); !errors.Is(err, ErrUnknownRemote) {
		t.Fatalf("second Remove: %v", err)
	}
	for _, s := range h.sup.List() {
		if s.ID == id {
			t.Fatal("removed remote still listed")
		}
	}

	// Remove the newest too: the next remote still gets a fresh id.
	if err := h.sup.Remove(other); err != nil {
		t.Fatal(err)
	}
	next, _ := h.add("Again")
	if next == id || next == other {
		t.Fatalf("id %s reused", next)
	}
}

// TestRemoteSite: the site's own guarantees.
func TestRemoteSite(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	site := h.sites.Site(netsite.ID(id))
	c := site.Costs()
	if !c.RepliesMatchSource {
		t.Fatal("RepliesMatchSource false")
	}
	if c.StallWindow != netsite.RemoteStallWindow || c.HandshakeAllowance != netsite.RemoteHandshakeAllowance {
		t.Fatalf("costs %v/%v", c.StallWindow, c.HandshakeAllowance)
	}
	if !c.Remote || c.RemoteName != "Observatory" {
		t.Fatalf("site presents as remote=%v %q, want a remote named Observatory", c.Remote, c.RemoteName)
	}

	// Down: every dial fails as a network error, through the registry and on
	// the site itself.
	for name, s := range map[string]netsite.Site{"registry": site, "site": h.remote(id).site} {
		_, err := s.DialTCP(context.Background(), lanDevice, 445, time.Second)
		var ne net.Error
		if !errors.As(err, &ne) {
			t.Fatalf("%s dial while down: %v is not a net.Error", name, err)
		}
		if s.ProbeUDP(context.Background(), lanDevice, 4720, []byte("x"), 100*time.Millisecond, func([]byte, string) bool { return true }) {
			t.Fatalf("%s probe while down answered", name)
		}
	}

	h.connect(id, f)
	if c := site.Costs(); c.MinLatency <= 0 || c.Latency <= 0 {
		t.Fatalf("latency %v / min %v once up", c.Latency, c.MinLatency)
	}
	if _, err := site.DialTCP(context.Background(), "not-an-ip", 445, time.Second); err == nil {
		t.Fatal("a hostname was dialled on a remote site")
	}
	got, err := site.Broadcast(context.Background())
	if err != nil || len(got) != 1 || got[0].SrcIP != lanDevice {
		t.Fatalf("broadcast: %v %+v", err, got)
	}

	if err := h.sup.SetRateLimit(id, 2); err != nil {
		t.Fatal(err)
	}
	if got := site.Costs().RateLimitMBps; got != 2 || h.status(id).RateLimitMBps != 2 {
		t.Fatalf("cap not applied: %v", got)
	}
	if err := h.sup.SetRateLimit(id, -1); err == nil {
		t.Fatal("a negative cap was accepted")
	}
	if err := h.sup.Rename(id, "Dome"); err != nil {
		t.Fatal(err)
	}
	if st := h.status(id); st.Name != "Dome" || st.State != model.RemoteConnected {
		t.Fatalf("rename: %+v", st)
	}
	if got := site.Costs().RemoteName; got != "Dome" {
		t.Fatalf("site name after rename = %q, want Dome", got)
	}
	if err := h.sup.Rename("r99", "x"); !errors.Is(err, ErrUnknownRemote) {
		t.Fatalf("rename unknown: %v", err)
	}
}

// TestRemoteWarningsAndInstances: each remote warning is copied to the log at
// Warn once, keyed by instance id and sequence number, and an instance id
// that comes back without a restart flags a shared pairing.
func TestRemoteWarningsAndInstances(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	hd := f.handler()
	set := func(ws ...proto.Warning) {
		hd.mu.Lock()
		hd.warnings = ws
		hd.mu.Unlock()
	}
	w1 := proto.Warning{Seq: 1, AgeMS: 5000, Kind: proto.WarnRefusedForward, Proto: "tcp", Target: "192.168.1.9:445", Reason: "not on the allowlist"}
	w2 := proto.Warning{Seq: 2, AgeMS: 1000, Kind: proto.WarnBroadcastFailed, Reason: "no interface"}
	set(w1, w2)
	h.beat(id)
	h.beat(id)
	const line = "level=WARN msg=\"remote: warning reported by remote\""
	if n := countLines(h.logs.String(), line); n != 2 {
		t.Fatalf("%d warning lines, want 2:\n%s", n, h.logs.String())
	}
	st := h.status(id)
	if len(st.Warnings) != 2 || st.Warnings[0].At != h.clock.now().Add(-5*time.Second).Unix() {
		t.Fatalf("published warnings: %+v", st.Warnings)
	}
	set(w1, w2, proto.Warning{Seq: 3, Kind: proto.WarnRejectedClient, ClientKey: "nodekey:abc"})
	h.beat(id)
	if n := countLines(h.logs.String(), line); n != 3 {
		t.Fatalf("%d warning lines after one more, want 3", n)
	}

	// Another process answers on this pairing (seq restarts at 1), then the
	// first one again without restarting: a shared pairing.
	hd.mu.Lock()
	first := hd.instance
	hd.instance = "inst-other"
	hd.mu.Unlock()
	set(proto.Warning{Seq: 1, Kind: proto.WarnBroadcastFailed})
	h.beat(id)
	if n := countLines(h.logs.String(), line); n != 4 {
		t.Fatalf("a new instance's seq 1 was not copied (%d lines)", n)
	}
	st = h.status(id)
	if st.Restarts != 1 || st.PairingShared {
		t.Fatalf("after a new instance: %+v", st)
	}
	hd.mu.Lock()
	hd.instance = first
	hd.mu.Unlock()
	h.beat(id)
	st = h.status(id)
	if !st.PairingShared || st.Restarts != 1 {
		t.Fatalf("alternation not detected: %+v", st)
	}
}

func TestBuildStaleAndNeedsUpdate(t *testing.T) {
	if !buildStale("2026-10-20T00:00:00Z", "2026-10-01T00:00:00Z") {
		t.Error("19 days behind is not stale")
	}
	if buildStale("2026-10-10T00:00:00Z", "2026-10-01T00:00:00Z") {
		t.Error("9 days behind is stale")
	}
	if buildStale("unknown", "2020-01-01T00:00:00Z") || buildStale("2026-10-20T00:00:00Z", "") {
		t.Error("an unreadable build date is stale")
	}
	if needsUpdate(proto.HelloReply{}) != "" {
		t.Error("needs update before any hello")
	}
}

// TestRepairOvertakesBuildInFlight: a client build that read the old pairing
// before a Re-pair is discarded, not installed over the new pairing's client,
// so the old env never connects again.
func TestRepairOvertakesBuildInFlight(t *testing.T) {
	h := newHarness(t)
	id, old := h.add("Observatory")
	h.connect(id, old)
	r := h.remote(id)

	entered, release := make(chan struct{}), make(chan struct{})
	// Only the first build blocks; the Re-pair's own build must not.
	var builds atomic.Int32
	h.mu.Lock()
	h.onBuild = func() {
		if builds.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { r.rebuild(h.clock.now()); close(done) }() // reads the old pairing, then blocks
	<-entered
	if _, err := h.sup.Repair(context.Background(), id, 0); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	close(release)
	<-done

	// The old remote is still running on the old env; the heartbeat must not
	// reach it.
	h.beat(id)
	h.beat(id)
	h.wantState(id, model.RemoteWaiting)
	if h.sites.Site(netsite.ID(id)).State() == netsite.StateUp {
		t.Fatal("the site came up on the superseded pairing")
	}
}

// TestRemoteStatusShowsSmoothedLatency: the card shows the smoothed round trip,
// while the site's timeouts use it padded by the variation margin.
func TestRemoteStatusShowsSmoothedLatency(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	site := h.remote(id).site
	for _, rtt := range []time.Duration{100, 300, 100, 300} {
		site.lat.Observe(rtt * time.Millisecond)
	}
	st := h.status(id)
	if want := site.lat.Smoothed().Milliseconds(); st.LatencyMS != want {
		t.Fatalf("latency shown %d ms, want the smoothed %d ms", st.LatencyMS, want)
	}
	if st.LatencyMS >= site.Latency().Milliseconds() {
		t.Fatalf("latency shown %d ms is not below the padded %v", st.LatencyMS, site.Latency())
	}
}

// offlineRegions fails every relay lookup, as an offline Commander does.
type offlineRegions struct{}

func (offlineRegions) Nearest(context.Context) (tunnel.Region, error) {
	return tunnel.Region{}, errors.New("offline")
}
func (offlineRegions) All(context.Context) ([]tunnel.Region, error) {
	return nil, errors.New("offline")
}

// TestRegionErrorsAreSentinels: an unknown region id and an unreachable relay
// service are told apart by sentinel, so the API can answer 400 and 502.
func TestRegionErrorsAreSentinels(t *testing.T) {
	h := newHarness(t)
	if _, err := h.sup.Add(context.Background(), "x", 424242); !errors.Is(err, ErrUnknownRegion) {
		t.Fatalf("Add with an unlisted region: %v, want ErrUnknownRegion", err)
	}
	h.sup.regions = offlineRegions{}
	if _, err := h.sup.Add(context.Background(), "x", 0); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("Add on the nearest region, offline: %v, want ErrRelayUnavailable", err)
	}
	if _, err := h.sup.Add(context.Background(), "x", 5); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("Add on a named region, offline: %v, want ErrRelayUnavailable", err)
	}
	if _, err := h.sup.Regions(context.Background()); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("Regions, offline: %v, want ErrRelayUnavailable", err)
	}
}

// blockingCloseClient is a tunnel client whose Close blocks until released,
// as a slow netstack teardown does.
type blockingCloseClient struct {
	tunnel.Client
	entered, release chan struct{}
}

func (c *blockingCloseClient) Close() error {
	close(c.entered)
	<-c.release
	return nil
}

// TestClientCloseDoesNotBlockList: closing a replaced tunnel client happens
// outside the remote's lock, so a slow teardown never stalls GET /api/remotes
// or a publish.
func TestClientCloseDoesNotBlockList(t *testing.T) {
	h := newHarness(t)
	id, _ := h.add("Observatory")
	r := h.remote(id)
	slow := &blockingCloseClient{Client: deadClient{}, entered: make(chan struct{}), release: make(chan struct{})}
	r.site.swap(slow, nil)()

	done := make(chan struct{})
	go func() { r.rebuild(h.clock.now()); close(done) }()
	<-slow.entered
	listed := make(chan struct{})
	go func() { h.sup.List(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("List blocked behind a tunnel client's Close")
	}
	close(slow.release)
	<-done
}
