package netsite

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"
)

// Fake is a scriptable Site for tests: it records every dial and probe it is
// asked to carry, and opens no socket of its own. A test registers it under an
// id of its choosing to stand in for a site other than the local one.
//
// It is safe for concurrent use.
type Fake struct {
	id ID

	mu           sync.Mutex
	state        State
	latency      time.Duration
	minLatency   time.Duration
	matchSource  bool
	calls        []string
	onDial       func(host string, port int) (net.Conn, error)
	reply        []byte
	replySrc     string
	broadcast    []Datagram
	broadcastErr error
	onCall       func(call string)
	onMeasure    func(ctx context.Context, req DigestRequest) (DigestResult, error)
	dropProbes   int
	stallWindow  time.Duration
	handshake    time.Duration
	remote       bool
	remoteName   string

	limiter *ReadLimiter
}

var (
	_ Site        = (*Fake)(nil)
	_ LimitsReads = (*Fake)(nil)
)

// NewFake returns an up Fake with the given id, whose dials fail and whose
// probes and broadcasts go unanswered until scripted. It reports
// RepliesMatchSource true, RemoteStallWindow and RemoteHandshakeAllowance, as a
// site other than the local one does, and has no rate cap. It is not a
// supervised remote until SetRemote.
func NewFake(id ID) *Fake {
	return &Fake{
		id:          id,
		state:       StateUp,
		matchSource: true,
		stallWindow: RemoteStallWindow,
		handshake:   RemoteHandshakeAllowance,
		limiter:     NewReadLimiter(),
	}
}

func (f *Fake) ID() ID { return f.id }

// Costs reports the scripted figures and the Fake's rate cap.
func (f *Fake) Costs() Costs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Costs{
		Latency:            f.latency,
		MinLatency:         f.minLatency,
		StallWindow:        f.stallWindow,
		HandshakeAllowance: f.handshake,
		RepliesMatchSource: f.matchSource,
		RateLimitMBps:      f.limiter.MBps(),
		Remote:             f.remote,
		RemoteName:         f.remoteName,
	}
}

// LimitReads wraps c with the Fake's limiter, as a site other than the local
// one does.
func (f *Fake) LimitReads(c net.Conn) net.Conn { return f.limiter.Wrap(c) }

// SetRateLimit caps the Fake's read rate at mbps megabytes per second; 0 or
// less removes the cap. It applies to connections already open.
func (f *Fake) SetRateLimit(mbps float64) { f.limiter.Set(mbps) }

// SetRemote makes the Fake report itself as a supervised remote named name.
func (f *Fake) SetRemote(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remote, f.remoteName = true, name
}

func (f *Fake) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// DialTCP records "dial host:port" and answers with the scripted dialer, or a
// net.Error when none is set.
func (f *Fake) DialTCP(_ context.Context, host string, port int, _ time.Duration) (net.Conn, error) {
	f.record("dial " + net.JoinHostPort(host, strconv.Itoa(port)))
	f.mu.Lock()
	dial := f.onDial
	f.mu.Unlock()
	if dial == nil {
		return nil, &UnavailableError{Site: f.id, Reason: "fake site has no dialer"}
	}
	return dial(host, port)
}

// ProbeUDP records "probe host:port" and offers the scripted reply, if any, to
// accept. A probe dropped by DropProbes, or one with no reply scripted, returns
// at once rather than waiting out its timeout.
func (f *Fake) ProbeUDP(_ context.Context, host string, port int, _ []byte, _ time.Duration, accept func([]byte, string) bool) bool {
	f.record("probe " + net.JoinHostPort(host, strconv.Itoa(port)))
	f.mu.Lock()
	reply, src := f.reply, f.replySrc
	dropped := f.dropProbes > 0
	if dropped {
		f.dropProbes--
	}
	f.mu.Unlock()
	if reply == nil || dropped {
		return false
	}
	return accept(reply, src)
}

// Broadcast records "broadcast" and answers with the scripted replies and
// error.
func (f *Fake) Broadcast(context.Context) ([]Datagram, error) {
	f.record("broadcast")
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Datagram(nil), f.broadcast...), f.broadcastErr
}

// MeasureDigests answers with the scripted measurement, or, when none is set,
// runs req.Local as a site that streams through itself would. It is not
// recorded in Calls, which hold the dials and probes a session makes.
func (f *Fake) MeasureDigests(ctx context.Context, req DigestRequest) (DigestResult, error) {
	f.mu.Lock()
	measure := f.onMeasure
	f.mu.Unlock()
	if measure == nil {
		return MeasureLocally(req)
	}
	return measure(ctx, req)
}

// OnMeasure scripts MeasureDigests; nil restores the local-read default.
func (f *Fake) OnMeasure(measure func(ctx context.Context, req DigestRequest) (DigestResult, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onMeasure = measure
}

// SetBroadcast scripts every broadcast round's replies and error.
func (f *Fake) SetBroadcast(replies []Datagram, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast, f.broadcastErr = replies, err
}

// DropProbes leaves the next n probes unanswered, as a lost datagram would,
// whatever reply is scripted.
func (f *Fake) DropProbes(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropProbes = n
}

// SetStallWindow sets the stall window the site reports.
func (f *Fake) SetStallWindow(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stallWindow = d
}

// SetHandshakeAllowance sets the handshake allowance the site reports.
func (f *Fake) SetHandshakeAllowance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handshake = d
}

// SetMinLatency sets the minimum recent latency the site reports.
func (f *Fake) SetMinLatency(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minLatency = d
}

// SetRepliesMatchSource sets the RepliesMatchSource Costs reports, so a Fake can
// stand in for the local site too.
func (f *Fake) SetRepliesMatchSource(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.matchSource = v
}

// SetState sets the state the site reports.
func (f *Fake) SetState(s State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

// SetLatency sets the latency the site reports.
func (f *Fake) SetLatency(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency = d
}

// OnDial scripts DialTCP; nil restores the failing default.
func (f *Fake) OnDial(dial func(host string, port int) (net.Conn, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onDial = dial
}

// SetProbeReply makes every probe answer with reply from srcIP; a nil reply
// leaves probes unanswered.
func (f *Fake) SetProbeReply(reply []byte, srcIP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply, f.replySrc = reply, srcIP
}

// OnCall installs a hook seeing each recorded call as it happens, so a test can
// interleave the site's calls with events recorded elsewhere.
func (f *Fake) OnCall(hook func(call string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onCall = hook
}

// Calls returns the dials, probes and broadcasts carried, in order. An empty
// result is the assertion that the site was never used.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *Fake) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	hook := f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
}
