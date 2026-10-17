package devfiles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// errNoProbeReply is an identity probe that got no answer: the session is not
// trusted, and withRetry retries it like a network failure.
var errNoProbeReply = errors.New("devfiles: no reply to the identity probe")

// rigPool is one rig's sessions: a semaphore of slotsPerRig checkouts and at
// most one parked idle session. A parked session holds no slot, but a checkout
// always takes it before opening another, so the rig never sees more than
// slotsPerRig tool sessions.
type rigPool struct {
	slots chan struct{}
	idle  *session
	timer *time.Timer
}

// session is one checked-out SMB session. It is used by one request on one
// goroutine at a time, so its fields need no lock.
type session struct {
	rigID   string
	name    string
	op      string
	cfg     *model.Config
	profile *model.RigProfile
	ep      netsite.Endpoint
	site    netsite.Site
	sess    device.Session
	mounts  map[string]device.Share
	// writable is the identity probe's verdict on this session together with a
	// non-empty profile GUID: a host-bound rig is read-only.
	writable bool
	// ctx is the current checkout's request context; a session checked in after
	// it ended is closed, never parked.
	ctx context.Context
	// broken marks a session an engine saw fail at the network level outside the
	// error it hands to release (a best-effort Chtimes, say).
	broken bool
}

// share mounts name once per session: smbSession.Mount adds a tree connect on
// every call, so a parked session must not remount per request.
func (c *session) share(name string) (device.Share, error) {
	if sh, ok := c.mounts[name]; ok {
		return sh, nil
	}
	sh, err := c.sess.Mount(name)
	if err != nil {
		if device.IsNotFound(err) {
			// A share is not a path that can go away under the pane: a hand-typed
			// or stale name is simply not one of the rig's shares.
			return nil, &Error{Kind: ErrNotFound, Rig: c.rigID, Err: err,
				Msg: fmt.Sprintf("%s isn't a share on %s", name, c.name)}
		}
		return nil, classify(err, c.rigID, c.name, "Share "+name)
	}
	c.mounts[name] = sh
	return sh, nil
}

// markBroken makes release close this session even if the error it is handed
// is not a network one. It is a no-op for a non-network err.
func (c *session) markBroken(err error) {
	if device.IsNetwork(err) {
		c.broken = true
	}
}

// poolLocked returns rigID's pool, creating it. Caller holds s.mu.
func (s *Service) poolLocked(rigID string) *rigPool {
	p := s.pools[rigID]
	if p == nil {
		p = &rigPool{slots: make(chan struct{}, slotsPerRig)}
		s.pools[rigID] = p
	}
	return p
}

// endpoint is where the rig's worker last found its device, from the hub
// snapshot: the tool resolves nothing itself. ok is false when the worker has
// not published an address yet.
func (s *Service) endpoint(rigID string) (netsite.Endpoint, bool) {
	for _, st := range s.status.Snapshot().Rigs {
		if st.ID == rigID {
			if st.Address == "" {
				return netsite.Endpoint{}, false
			}
			return netsite.Endpoint{Site: netsite.ID(st.Site), IP: st.Address}, true
		}
	}
	return netsite.Endpoint{}, false
}

// managedProfile returns rigID's profile from cfg, or ErrUnknownRig when it is
// not configured or not managed.
func managedProfile(cfg *model.Config, rigID string) (*model.RigProfile, error) {
	profile := cfg.Rig(rigID)
	if profile == nil || !profile.Managed {
		return nil, newErr(ErrUnknownRig, rigID, "unknown rig %q", rigID)
	}
	return profile, nil
}

// acquire checks out a session on rigID for op: it takes one of the rig's
// slots (waiting on ctx), then the parked session if it was opened under the
// same endpoint and the same config pointer, or else opens and identity-probes
// a fresh one. Every config save swaps the pointer, so a credential edit or a
// GUID bind never reuses a session proved against the old identity. Each
// successful acquire is paired with exactly one release.
func (s *Service) acquire(ctx context.Context, rigID, op string) (*session, error) {
	cfg := s.cfg.Load()
	profile, err := managedProfile(cfg, rigID)
	if err != nil {
		return nil, err
	}
	name := rigName(profile, rigID)
	ep, ok := s.endpoint(rigID)
	if !ok {
		return nil, newErr(ErrUnreachable, rigID, "Commander doesn't know where %s is yet", name)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, newErr(ErrClosed, rigID, "Commander is shutting down")
	}
	pool := s.poolLocked(rigID)
	s.mu.Unlock()

	select {
	case pool.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-pool.slots
		return nil, newErr(ErrClosed, rigID, "Commander is shutting down")
	}
	idle := pool.idle
	pool.idle = nil
	if pool.timer != nil {
		pool.timer.Stop()
		pool.timer = nil
	}
	s.mu.Unlock()

	if idle != nil {
		if idle.ep == ep && idle.cfg == cfg {
			idle.ctx, idle.op, idle.broken = ctx, op, false
			// Debug, not Info: a walk checks the parked session out once per listing.
			s.log.Debug("devfiles: session checked out", "rig", rigID, "op", op,
				"site", string(ep.Site), "ip", ep.IP, "reused", true)
			return idle, nil
		}
		s.closeSession(idle, "stale")
	}

	c, err := s.open(ctx, rigID, name, op, cfg, profile, ep)
	if err != nil {
		<-pool.slots
		return nil, err
	}
	return c, nil
}

// open dials a fresh session within the open budget and re-proves the device's
// identity with a live probe while the session is open. A foreign answer is
// ErrIdentity (never retried); no answer is errNoProbeReply (retried); an
// answer for a rig with no GUID yet is accepted read-only. The tool never
// records a GUID bind: binding stays the worker's job.
func (s *Service) open(ctx context.Context, rigID, name, op string, cfg *model.Config,
	profile *model.RigProfile, ep netsite.Endpoint) (*session, error) {
	site := s.sites.Site(ep.Site)
	dev := s.newDevice(ep, profile.User, profile.Password, rig.SMBTimeout(site))
	budget := s.openBase + netsite.RoundTrips(site, 3)
	openCtx, cancel := context.WithTimeout(ctx, budget)
	sess, err := dev.Open(openCtx)
	budgetSpent := openCtx.Err() != nil && ctx.Err() == nil
	cancel()
	if err != nil {
		if budgetSpent && !device.IsNetwork(err) {
			// The budget, not the device, ended the attempt: a timeout is a network
			// failure (context.DeadlineExceeded is a net.Error).
			err = fmt.Errorf("%w (%v)", context.DeadlineExceeded, err)
		}
		s.log.Info("devfiles: session open failed", "rig", rigID, "op", op,
			"site", string(ep.Site), "ip", ep.IP, "budget", budget, "error", err)
		return nil, err
	}

	c := &session{
		rigID: rigID, name: name, op: op, cfg: cfg, profile: profile,
		ep: ep, site: site, sess: sess, mounts: map[string]device.Share{}, ctx: ctx,
	}
	guid, cpuID, probed := s.prober.ProbeGUID(ctx, ep)
	switch {
	case !probed:
		s.log.Warn("devfiles: identity unconfirmed: no probe reply", "rig", rigID, "op", op, "ip", ep.IP)
		s.closeSession(c, "error")
		return nil, fmt.Errorf("%s did not answer the identity probe at %s: %w", name, ep.IP, errNoProbeReply)
	case profile.GUID == "":
		s.log.Info("devfiles: identity not bound yet; session is read-only", "rig", rigID, "op", op, "ip", ep.IP)
	case !rig.IdentityMatches(profile, guid, cpuID):
		s.log.Warn("devfiles: identity foreign: refusing the device", "rig", rigID, "op", op, "ip", ep.IP)
		s.closeSession(c, "error")
		return nil, newErr(ErrIdentity, rigID, "The device at %s is not %s. Commander will not touch it.", ep.IP, name)
	default:
		c.writable = true
		s.log.Info("devfiles: identity confirmed", "rig", rigID, "op", op, "ip", ep.IP)
	}
	s.log.Info("devfiles: session checked out", "rig", rigID, "op", op,
		"site", string(ep.Site), "ip", ep.IP, "reused", false)
	return c, nil
}

// release checks c back in. A session that failed at the network level, whose
// request was cancelled, or that outlives the service is closed; any other is
// parked as the rig's idle session (closing the one it replaces) for idleTTL.
// The slot is freed last, after any close, so the rig never sees more than
// slotsPerRig sessions. Every stream on c must be closed before release, which
// clears its deadline: a parked session never carries an armed deadline.
func (s *Service) release(c *session, err error) {
	reason := ""
	switch {
	case c.ctx.Err() != nil:
		reason = "cancelled"
	case c.broken || device.IsNetwork(err):
		reason = "error"
	}

	s.mu.Lock()
	pool := s.poolLocked(c.rigID)
	if reason == "" && s.closed {
		reason = "shutdown"
	}
	var replaced *session
	if reason == "" {
		replaced = pool.idle
		pool.idle = c
		if pool.timer != nil {
			pool.timer.Stop()
		}
		pool.timer = time.AfterFunc(s.idleTTL, func() { s.expire(c) })
	}
	s.mu.Unlock()

	if replaced != nil {
		s.closeSession(replaced, "replaced")
	}
	if reason != "" {
		s.closeSession(c, reason)
	}
	<-pool.slots
}

// expire closes c when its idle window ends, unless it was checked out again.
func (s *Service) expire(c *session) {
	s.mu.Lock()
	pool := s.pools[c.rigID]
	if pool == nil || pool.idle != c {
		s.mu.Unlock()
		return
	}
	pool.idle = nil
	pool.timer = nil
	s.mu.Unlock()
	s.closeSession(c, "idle")
}

// closeSession logs off c. On a silent device it blocks until the device
// layer's per-request timeout ends each step of the teardown (an umount per
// mounted share, then the logoff), so it is never called under s.mu.
func (s *Service) closeSession(c *session, reason string) {
	if err := c.sess.Close(); err != nil {
		s.log.Debug("devfiles: session close error", "rig", c.rigID, "op", c.op, "error", err)
	}
	s.log.Info("devfiles: session closed", "rig", c.rigID, "op", c.op, "reason", reason)
}

// requireWritable refuses a write on a session whose rig has no GUID yet.
func (s *Service) requireWritable(c *session, share string) error {
	if c.writable {
		return nil
	}
	s.log.Warn("devfiles: write refused on a host-bound rig", "rig", c.rigID, "share", share, "op", c.op)
	return newErr(ErrHostBound, c.rigID,
		"Commander is still identifying %s; writes are disabled until it has", c.name)
}
