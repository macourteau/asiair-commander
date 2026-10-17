package devfiles

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// Shared test fixture: two managed rigs on device.Fake devices, reached at
// the addresses a fake hub snapshot publishes, with a scripted identity probe.
// Session opens, closes and mounts are counted through a wrapping device.

const (
	emmc  = "EMMC Images"
	udisk = "Udisk Images"
	ip1   = "10.0.0.1"
	ip2   = "10.0.0.2"
)

var t0 = time.Date(2026, 10, 7, 21, 0, 0, 0, time.UTC)

type probeAnswer struct {
	guid, cpuID string
	ok          bool
}

// fakeProber answers ProbeGUID per IP from a script; the last answer repeats.
// With no script, it answers the GUID registered for the IP.
type fakeProber struct {
	mu      sync.Mutex
	guids   map[string]string
	scripts map[string][]probeAnswer
	calls   int
}

func (p *fakeProber) ProbeGUID(_ context.Context, ep netsite.Endpoint) (string, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if s := p.scripts[ep.IP]; len(s) > 0 {
		a := s[0]
		if len(s) > 1 {
			p.scripts[ep.IP] = s[1:]
		}
		return a.guid, a.cpuID, a.ok
	}
	g, ok := p.guids[ep.IP]
	return g, "", ok
}

func (p *fakeProber) script(ip string, answers ...probeAnswer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[ip] = answers
}

func (p *fakeProber) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeStatus struct {
	snap atomic.Pointer[model.Snapshot]
}

func (f *fakeStatus) Snapshot() model.Snapshot { return *f.snap.Load() }

// fakeStore is the fixture's default store: no frame rows and no meta, and
// resident leads per share as a test sets them, or leadsErr for every share.
// withStore replaces it with a real one.
type fakeStore struct {
	mu         sync.Mutex
	leads      map[string][]string
	leadsErr   error
	leadsCalls int
}

func (s *fakeStore) GetFrame(context.Context, string, string, string) (*model.Frame, bool, error) {
	return nil, false, nil
}

func (s *fakeStore) GetMeta(context.Context, string) (string, bool, error) { return "", false, nil }

func (s *fakeStore) ResidentLeads(_ context.Context, _, share string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leadsCalls++
	if s.leadsErr != nil {
		return nil, s.leadsErr
	}
	return append([]string(nil), s.leads[share]...), nil
}

// setLeads makes ResidentLeads answer leads on share.
func (s *fakeStore) setLeads(share string, leads ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leads[share] = leads
}

func (s *fakeStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leadsCalls
}

// fixture is a Service over fake rigs r1 ("Rig One", GUID g1, at ip1) and r2
// ("Rig Two", GUID g2, at ip2), each watching Autorun on both shares.
type fixture struct {
	t       *testing.T
	svc     *Service
	cfg     atomic.Pointer[model.Config]
	status  *fakeStatus
	store   *fakeStore
	prober  *fakeProber
	devs    map[string]*device.Fake
	blocked map[string]bool // IPs whose Open waits for its context to end

	opens, closes, mounts atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{
		t:      t,
		status: &fakeStatus{},
		store:  &fakeStore{leads: map[string][]string{}},
		prober: &fakeProber{
			guids:   map[string]string{ip1: "g1", ip2: "g2"},
			scripts: map[string][]probeAnswer{},
		},
		devs:    map[string]*device.Fake{ip1: device.NewFake(), ip2: device.NewFake()},
		blocked: map[string]bool{},
	}
	for _, d := range fx.devs {
		d.SetServerTime(clock.ServerTime{Time: t0, ReceivedAt: t0})
		d.AddShare(emmc)
		d.AddShare(udisk)
	}
	sources := []model.Source{
		{Share: emmc, Roots: []string{"Autorun"}},
		{Share: udisk, Roots: []string{"Autorun"}},
	}
	fx.cfg.Store(&model.Config{Rigs: []model.RigProfile{
		{ID: "r1", Name: "Rig One", GUID: "g1", Managed: true, Sources: sources},
		{ID: "r2", Name: "Rig Two", GUID: "g2", Managed: true, Sources: sources},
		{ID: "r3", Name: "Unmanaged", GUID: "g3", Sources: sources},
	}})
	fx.setAddress("r1", ip1)
	fx.setAddress("r2", ip2)

	fx.svc = New(Deps{
		Config:    &fx.cfg,
		Store:     fx.store,
		Status:    fx.status,
		Prober:    fx.prober,
		Sites:     netsite.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil))),
		NewDevice: fx.newDevice,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	fx.svc.openBase = 200 * time.Millisecond
	fx.svc.backoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(fx.svc.Close)
	return fx
}

// setAddress publishes rigID's address in the hub snapshot ("" for unknown).
func (fx *fixture) setAddress(rigID, ip string) {
	var rigs []model.RigStatus
	if cur := fx.status.snap.Load(); cur != nil {
		for _, r := range cur.Rigs {
			if r.ID != rigID {
				rigs = append(rigs, r)
			}
		}
	}
	rigs = append(rigs, model.RigStatus{ID: rigID, Address: ip, Online: ip != ""})
	fx.status.snap.Store(&model.Snapshot{Rigs: rigs})
}

// editRig swaps the config pointer with rigID's profile changed by fn, as a
// config save does.
func (fx *fixture) editRig(rigID string, fn func(*model.RigProfile)) {
	cur := fx.cfg.Load()
	next := *cur
	next.Rigs = append([]model.RigProfile(nil), cur.Rigs...)
	fn(next.Rig(rigID))
	fx.cfg.Store(&next)
}

func (fx *fixture) dev(ip string) *device.Fake { return fx.devs[ip] }

func (fx *fixture) newDevice(ep netsite.Endpoint, _, _ string, _ time.Duration) device.Device {
	return &countingDevice{fx: fx, ip: ep.IP}
}

// countingDevice counts opens, closes and mounts, and can block Open until
// its context ends (a dead address).
type countingDevice struct {
	fx *fixture
	ip string
}

func (d *countingDevice) Probe(context.Context) bool { return true }

func (d *countingDevice) Open(ctx context.Context) (device.Session, error) {
	d.fx.opens.Add(1)
	if d.fx.blocked[d.ip] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f := d.fx.devs[d.ip]
	if f == nil {
		return nil, device.NetworkError("dial " + d.ip)
	}
	sess, err := f.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &countingSession{Session: sess, fx: d.fx}, nil
}

type countingSession struct {
	device.Session
	fx *fixture
}

func (s *countingSession) Mount(share string) (device.Share, error) {
	s.fx.mounts.Add(1)
	return s.Session.Mount(share)
}

func (s *countingSession) Close() error {
	s.fx.closes.Add(1)
	return s.Session.Close()
}

// waitFor polls cond until it holds or a second passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// wantKind asserts err is a classified *Error of kind.
func wantKind(t *testing.T, err, kind error) *Error {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("error = %v, want %v", err, kind)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an *Error", err)
	}
	return e
}

func errNetworkForTest() error { return device.NetworkError("connection reset") }
