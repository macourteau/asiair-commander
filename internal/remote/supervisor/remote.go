package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

const (
	// rebuildBackoffMax caps the interval between rebuilds while a remote is
	// Unreachable.
	rebuildBackoffMax = 10 * time.Minute
	// historyPersistEvery is how often a remote that keeps answering refreshes
	// its durable last-hello time; a Commander restart can misdate "last seen"
	// by at most this much.
	historyPersistEvery = time.Minute
	// maxSeenInstances bounds the instance ids kept for spotting two remotes on
	// one pairing.
	maxSeenInstances = 16
	// buildStaleAfter is how far a remote's build may trail Commander's before
	// its card asks the user to check its updater: watchtower updates at least
	// weekly, so two weeks means at least one missed update.
	buildStaleAfter = 14 * 24 * time.Hour
)

// remote supervises one paired remote: its tunnel client, heartbeat, state and
// site. Its heartbeat runs on one goroutine; the lifecycle calls (rename, cap,
// re-pair) come from others, so every field below mu is read and written under
// it, and no network call is made while holding it.
type remote struct {
	sup  *Supervisor
	site *site
	log  *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	prof model.RemoteProfile
	// fingerprint is the current pairing's; gen counts pairings, so a
	// heartbeat that straddles a Re-pair discards its result.
	fingerprint string
	gen         int
	// buildErr is set while the stored pairing cannot build a client.
	buildErr error

	// hist is the durable record, and persistedAt when it was last written.
	hist        history
	persistedAt time.Time

	// roundDone is set once the broadcast round after the latest (re)connect
	// completed; failingSince is when the current run of failed heartbeats
	// began, zero while they succeed.
	roundDone    bool
	failingSince time.Time
	failures     int // consecutive failed heartbeats
	attempts     int // heartbeats since the supervisor started this remote
	lastRebuild  time.Time
	rebuildWait  time.Duration // the backoff interval while Unreachable
	rebuilds     int

	hello      proto.HelloReply
	helloAt    time.Time
	seen       []string          // instance ids seen under this pairing
	warnSeq    map[string]uint64 // per instance id, the highest warning copied
	warnings   []model.RemoteWarning
	sharedSeen bool // an instance id came back without a restart
}

// newRemote builds a remote's supervision from its profile and durable
// history. It does not start the heartbeat.
func newRemote(s *Supervisor, prof model.RemoteProfile, hist history) *remote {
	r := &remote{
		sup:     s,
		site:    newSite(netsite.ID(prof.ID), s.now, s.log.With("remote", prof.ID)),
		log:     s.log.With("remote", prof.ID),
		prof:    prof,
		hist:    hist,
		warnSeq: map[string]uint64{},
	}
	r.site.SetRateLimit(prof.RateLimitMBps)
	r.site.setName(prof.Name)
	r.fingerprint = pairingFingerprint(prof)
	if hist.Instance != "" {
		r.seen = []string{hist.Instance}
	}
	r.buildClient()
	return r
}

// pairingFingerprint identifies a stored pairing; "" when it cannot be read,
// which buildClient reports.
func pairingFingerprint(p model.RemoteProfile) string {
	fp, err := tunnel.Fingerprint(tunnel.Secret(p.Address), tunnel.Secret(p.ClientKey))
	if err != nil {
		return ""
	}
	return fp
}

// buildClient installs a fresh tunnel client for the current pairing, closing
// the previous one. The site goes down until a hello and a broadcast round
// bring it up again. A build that a Re-pair overtook is discarded rather than
// installed, so a client for a superseded pairing is never dialled again.
func (r *remote) buildClient() {
	r.mu.Lock()
	prof, gen := r.prof, r.gen
	r.mu.Unlock()
	c, err := r.sup.newClient(prof)

	r.mu.Lock()
	if gen != r.gen {
		r.mu.Unlock()
		if c != nil {
			c.Close()
		}
		return
	}
	// The install happens under r.mu, so a Re-pair's own build, which bumps
	// gen first, always lands after this one. The replaced client is closed
	// only after unlocking: its teardown is a network call.
	first := false
	var closeOld func()
	if err != nil {
		closeOld = r.site.swap(nil, nil)
		first = r.buildErr == nil
		r.buildErr = err
	} else {
		closeOld = r.site.swap(c, proto.NewClient(c.DialProtocol, r.log))
		r.buildErr = nil
	}
	r.roundDone = false
	r.mu.Unlock()
	closeOld()
	if err != nil && first {
		r.log.Error("remote: stored pairing cannot build a tunnel client; re-pair this remote", "error", err)
	}
}

// run heartbeats until ctx ends.
func (r *remote) run(ctx context.Context) {
	defer close(r.done)
	t := time.NewTicker(r.sup.interval)
	defer t.Stop()
	for {
		r.heartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// heartbeat sends one hello and acts on its outcome: on success it updates the
// remote's state and, after a (re)connect, runs the broadcast round that brings
// the site up; on failure it takes the site down and applies the rebuild rule.
func (r *remote) heartbeat(ctx context.Context) {
	r.mu.Lock()
	gen := r.gen
	r.attempts++
	r.mu.Unlock()

	_, pc := r.site.current()
	if pc == nil {
		r.buildClient()
		_, pc = r.site.current()
	}
	if pc == nil {
		r.failed(ctx, gen, errNoClient)
		r.sup.metrics.SetRemoteUp(r.id(), false)
		r.sup.publish()
		return
	}
	hctx, cancel := context.WithTimeout(ctx, r.sup.helloTimeout(r.site))
	hr, rtt, err := pc.Hello(hctx, newCallID())
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		r.failed(ctx, gen, err)
	} else {
		r.answered(ctx, gen, hr, rtt)
	}
	// The gauge follows the state that gates dials, whichever step changed it.
	r.sup.metrics.SetRemoteUp(r.id(), r.site.State() == netsite.StateUp)
	r.sup.publish()
}

var errNoClient = errors.New("no tunnel client")

// answered handles a successful hello.
func (r *remote) answered(ctx context.Context, gen int, hr proto.HelloReply, rtt time.Duration) {
	now := r.sup.now()
	r.mu.Lock()
	if gen != r.gen {
		r.mu.Unlock()
		return
	}
	r.site.lat.Observe(rtt)
	r.site.touch()
	r.site.setPeerDigests(hr.WholeDigestVersion, hr.DataDigestVersion)
	if !r.failingSince.IsZero() {
		r.log.Debug("remote: heartbeat answered after failures", "failures", r.failures, "since", r.failingSince)
	}
	r.failingSince, r.failures, r.rebuildWait = time.Time{}, 0, 0

	// A new remote process starts with an empty forwarding allowlist, so it
	// needs the broadcast round too, even if no heartbeat failed in between.
	if hr.InstanceID != r.hist.Instance {
		r.newInstanceLocked(hr.InstanceID)
		r.roundDone = false
		r.site.setUp(false)
	}
	fresh := r.copyWarningsLocked(hr, now)
	r.hello, r.helloAt = hr, now

	persist := r.hist.Fingerprint != r.fingerprint || now.Sub(r.persistedAt) >= historyPersistEvery || r.hist.Instance != hr.InstanceID
	r.hist.LastHelloNS = now.UnixNano()
	r.hist.Fingerprint = r.fingerprint
	r.hist.Instance = hr.InstanceID
	needRound := !r.roundDone
	hist := r.hist
	r.mu.Unlock()

	r.sup.metrics.RecordRemoteHeartbeat(r.id(), metrics.RemoteCallOK)
	r.sup.metrics.SetRemoteLatency(r.id(), r.site.Latency().Seconds())
	for range fresh {
		r.sup.metrics.RecordRemoteWarning(r.id())
	}
	for _, w := range fresh {
		r.log.Warn("remote: warning reported by remote", "kind", string(w.Kind), "proto", w.Proto,
			"target", w.Target, "client_key", w.ClientKey, "reason", w.Reason, "call_id", w.CallID,
			"age", w.Age(), "instance", hr.InstanceID, "seq", w.Seq)
	}
	if persist {
		if err := r.sup.saveHistory(ctx, r.id(), hist); err != nil {
			r.log.Warn("remote: could not record connection history", "error", err)
		} else {
			r.mu.Lock()
			r.persistedAt = now
			r.mu.Unlock()
		}
	}
	if needRound {
		r.reconnectRound(ctx, gen)
	}
}

// newInstanceLocked records a remote process the last hello did not come from:
// a restart, or, when the id was seen before, a second remote answering on this
// pairing.
func (r *remote) newInstanceLocked(id string) {
	prev := r.hist.Instance
	switch {
	case prev == "":
	case slices.Contains(r.seen, id):
		if !r.sharedSeen {
			r.log.Warn("remote: an earlier remote process answered again without a restart; another remote is using this pairing",
				"instance", id, "previous", prev)
		}
		r.sharedSeen = true
	default:
		r.hist.Restarts++
		r.log.Info("remote: remote restarted", "instance", id, "previous", prev, "restarts", r.hist.Restarts)
	}
	if !slices.Contains(r.seen, id) {
		r.seen = append(r.seen, id)
		if len(r.seen) > maxSeenInstances {
			r.seen = r.seen[len(r.seen)-maxSeenInstances:]
		}
	}
}

// copyWarningsLocked takes the hello's warnings as the remote's recent list
// and returns the ones not seen before, by instance id and sequence number.
// The remote's clock is untrusted, so each warning's time is its reported age
// before now, on Commander's clock.
func (r *remote) copyWarningsLocked(hr proto.HelloReply, now time.Time) []proto.Warning {
	last := r.warnSeq[hr.InstanceID]
	var fresh []proto.Warning
	list := make([]model.RemoteWarning, 0, len(hr.Warnings))
	for _, w := range hr.Warnings {
		if w.Seq > last {
			fresh = append(fresh, w)
			r.warnSeq[hr.InstanceID] = max(r.warnSeq[hr.InstanceID], w.Seq)
		}
		list = append(list, model.RemoteWarning{
			At: now.Add(-w.Age()).Unix(), Kind: string(w.Kind), Proto: w.Proto,
			Target: w.Target, ClientKey: w.ClientKey, Reason: w.Reason,
		})
	}
	r.warnings = list
	return fresh
}

// reconnectRound runs one broadcast round on the remote's LAN, which fills its
// forwarding allowlist, and only then brings the site up, so the rigs behind a
// remote that just (re)started never meet an empty allowlist. A remote's own
// error reply still completes the round (the remote ran it, and lists the
// failure among its warnings); a transport failure leaves the site down for the
// next heartbeat to retry.
func (r *remote) reconnectRound(ctx context.Context, gen int) {
	_, pc := r.site.current()
	if pc == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, proto.BroadcastTimeout+netsite.RoundTrips(r.site, 1))
	replies, err := pc.Broadcast(rctx, newCallID())
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil && proto.CodeOf(err) == "" {
		r.sup.metrics.RecordRemoteReconnectRound(r.id(), metrics.RemoteCallFailed)
		r.log.Debug("remote: reconnect broadcast round failed; retrying at the next heartbeat", "error", err)
		return
	}
	r.sup.metrics.RecordRemoteReconnectRound(r.id(), metrics.RemoteCallOK)
	if err != nil {
		r.log.Debug("remote: reconnect broadcast round ran with an error on the remote", "error", err)
	}
	r.mu.Lock()
	if gen != r.gen {
		r.mu.Unlock()
		return
	}
	r.roundDone = true
	r.site.setUp(true)
	r.mu.Unlock()
	r.log.Debug("remote: reconnect broadcast round done; site up", "replies", len(replies))
}

// failed handles a failed heartbeat: the site goes down at once, so no new
// dial or broadcast is sent while the tunnel is down, and the client is
// rebuilt under the rebuild rule.
func (r *remote) failed(ctx context.Context, gen int, err error) {
	now := r.sup.now()
	r.mu.Lock()
	if gen != r.gen {
		r.mu.Unlock()
		return
	}
	if r.failingSince.IsZero() {
		r.failingSince = now
	}
	r.failures++
	r.roundDone = false
	r.site.setUp(false)
	failures, attempts := r.failures, r.attempts
	rebuild := r.shouldRebuildLocked(now)
	r.mu.Unlock()

	r.sup.metrics.RecordRemoteHeartbeat(r.id(), metrics.RemoteCallFailed)
	r.log.Debug("remote: heartbeat failed", "error", err, "failures", failures, "attempts", attempts)
	if rebuild {
		r.rebuild(now)
	}
}

// shouldRebuildLocked is the rebuild rule. Rebuilding tears down every
// connection through the client, so it happens only once the heartbeat has
// failed and nothing has moved through the tunnel (a transfer's bytes, a remote
// hash's progress, a hello) for longer than the site's SMB budget; a transfer
// still making progress is never killed. A rebuild restarts that quiet window,
// and while the remote is Unreachable the window doubles per rebuild up to
// rebuildBackoffMax.
func (r *remote) shouldRebuildLocked(now time.Time) bool {
	quietSince := r.site.lastActivity()
	if r.lastRebuild.After(quietSince) {
		quietSince = r.lastRebuild
	}
	wait := netsite.SMBBudget(r.site)
	if r.stateLocked(now) == model.RemoteUnreachable {
		wait = max(wait, r.rebuildWait)
	}
	return now.Sub(quietSince) > wait
}

func (r *remote) rebuild(now time.Time) {
	r.mu.Lock()
	r.lastRebuild = now
	r.rebuilds++
	if r.stateLocked(now) == model.RemoteUnreachable {
		r.rebuildWait = min(2*max(r.rebuildWait, netsite.SMBBudget(r.site)), rebuildBackoffMax)
	}
	rebuilds, wait := r.rebuilds, r.rebuildWait
	r.mu.Unlock()
	r.sup.metrics.RecordRemoteRebuild(r.id())
	r.log.Debug("remote: rebuilding tunnel client", "rebuilds", rebuilds, "next_wait", wait)
	r.buildClient()
}

// repair installs a fresh pairing: the old client stops being dialled at
// once, and everything learnt about the old pairing's remote process is
// dropped. The last-hello time is the remote's, not the pairing's, so it stays.
func (r *remote) repair(prof model.RemoteProfile) {
	r.site.setName(prof.Name)
	r.mu.Lock()
	r.prof = prof
	r.fingerprint = pairingFingerprint(prof)
	r.gen++
	r.roundDone, r.failingSince, r.failures = false, time.Time{}, 0
	r.lastRebuild, r.rebuildWait = time.Time{}, 0
	r.hello, r.helloAt = proto.HelloReply{}, time.Time{}
	r.seen, r.warnSeq, r.warnings, r.sharedSeen = nil, map[string]uint64{}, nil, false
	r.hist.Instance = ""
	r.mu.Unlock()
	r.log.Info("remote: re-paired; waiting for the remote to start with its new pairing", "fingerprint", r.fingerprint)
	r.buildClient()
}

// update applies a profile edit that keeps the pairing config: the name and the
// cap; the display region names are only stored.
func (r *remote) update(prof model.RemoteProfile) {
	r.site.setName(prof.Name)
	r.mu.Lock()
	r.prof = prof
	r.mu.Unlock()
	if r.site.RateLimit() != prof.RateLimitMBps {
		r.log.Info("remote: bandwidth cap set", "mbps", prof.RateLimitMBps)
	}
	r.site.SetRateLimit(prof.RateLimitMBps)
}

func (r *remote) id() string { return string(r.site.id) }

// stateLocked derives the card state. Up means connected; otherwise a remote
// that never answered under this pairing is Waiting, and one that did is
// Reconnecting until its last hello is older than the unreachable threshold.
func (r *remote) stateLocked(now time.Time) model.RemoteState {
	if r.site.State() == netsite.StateUp {
		if needsUpdate(r.hello) != "" {
			return model.RemoteNeedsUpdate
		}
		return model.RemoteConnected
	}
	if r.hist.Fingerprint == "" || r.hist.Fingerprint != r.fingerprint || r.hist.LastHelloNS == 0 {
		return model.RemoteWaiting
	}
	if now.Sub(r.hist.lastHello()) >= r.sup.unreachableAfter() {
		return model.RemoteUnreachable
	}
	return model.RemoteReconnecting
}

// status is the remote's state as of now. It has no side effects: transitions
// are acted on in one place, the supervisor's publish (onTransitions).
func (r *remote) status(now time.Time) model.RemoteStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stateLocked(now)
	out := model.RemoteStatus{
		ID: r.prof.ID, Name: r.prof.Name, State: st,
		Region: r.prof.Region, Fingerprint: r.fingerprint,
		Restarts: r.hist.Restarts, EverConnected: r.hist.LastHelloNS != 0,
		RateLimitMBps: r.prof.RateLimitMBps, CreatedAt: r.prof.CreatedAt,
		PairedAt: r.prof.PairedAt, PairingShared: r.sharedSeen,
	}
	if out.PairedAt == 0 {
		out.PairedAt = r.prof.CreatedAt
	}
	if r.hist.LastHelloNS != 0 {
		out.LastHelloAt = r.hist.lastHello().Unix()
	}
	if !r.helloAt.IsZero() {
		out.Direct = r.hello.Path.Direct
		out.RelayRegion = r.hello.Path.RelayRegion
		// The user sees the link's round trip; the padded Latency is for
		// timeouts only.
		out.LatencyMS = r.site.lat.Smoothed().Milliseconds()
		out.Version = r.hello.Build.Version
		out.BuildDate = r.hello.Build.Date
		out.UptimeS = int64(r.hello.Uptime().Seconds())
		out.Warnings = slices.Clone(r.warnings)
		out.NeedsUpdate = needsUpdate(r.hello)
		out.BuildStale = buildStale(r.sup.build.Date, r.hello.Build.Date)
	}
	return out
}

// needsUpdate names the side running the older digest algorithm, "" when both
// match or before any hello. The whole-file algorithm decides; the data-unit
// one only when the whole-file ones match.
func needsUpdate(h proto.HelloReply) string {
	if h.WholeDigestVersion == 0 && h.DataDigestVersion == 0 {
		return ""
	}
	rw, rd := h.WholeDigestVersion, h.DataDigestVersion
	switch {
	case rw == header.WholeDigestVersion && rd == header.DataDigestVersion:
		return ""
	case rw < header.WholeDigestVersion, rw == header.WholeDigestVersion && rd < header.DataDigestVersion:
		return model.UpdateRemote
	default:
		return model.UpdateCommander
	}
}

// buildStale reports whether the remote's build date trails Commander's by more
// than buildStaleAfter. Either date unreadable (a development build) is not
// stale: there is nothing to compare.
func buildStale(commander, remote string) bool {
	c, err := time.Parse(time.RFC3339, commander)
	if err != nil {
		return false
	}
	r, err := time.Parse(time.RFC3339, remote)
	if err != nil {
		return false
	}
	return c.Sub(r) > buildStaleAfter
}
