// Package supervisor is Commander's side of the remote bridge: it runs one
// tailcat client per paired remote, heartbeats it with the remote protocol's
// hello, rebuilds the client when a remote restarts (a client never recovers
// from that by itself), and registers one netsite.Site per remote, up only
// while its tunnel is connected and the broadcast round after each
// (re)connect has filled the remote's forwarding allowlist. It publishes each
// remote's state to the status hub and keeps its connection history in the
// store, and it is the only writer of the config's remotes: adding, re-pairing,
// renaming, capping and removing a remote all go through it.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// ErrUnknownRemote is returned for an id no paired remote has.
var ErrUnknownRemote = errors.New("remote: no such remote")

// ErrUnknownRegion is returned when a pairing names a relay region the relay
// map does not list.
var ErrUnknownRegion = errors.New("remote: no such relay region")

// ErrRelayUnavailable is returned when the relay map or the nearest region
// cannot be fetched or measured, typically because Commander is offline.
var ErrRelayUnavailable = errors.New("remote: relay service unreachable")

const (
	// heartbeatInterval is how often each remote is sent a hello.
	heartbeatInterval = 10 * time.Second
	// nearestRegionTimeout bounds the netcheck that picks a new pairing's
	// relay region.
	nearestRegionTimeout = 15 * time.Second
)

// SiteDropper forgets every discovery sighting made through a site
// (*discovery.Scanner).
type SiteDropper interface {
	DropSite(id netsite.ID)
}

// Publisher receives the remotes' states and Commander's own relay
// reachability (*hub.StatusHub).
type Publisher interface {
	SetRemotes([]model.RemoteStatus)
	SetRelay(*model.RelayStatus)
	SetUnackedAlerts(int)
}

// RegionSource lists the relay regions a pairing can be minted on.
type RegionSource interface {
	// Nearest is the region with the lowest measured latency.
	Nearest(ctx context.Context) (tunnel.Region, error)
	// All lists every region.
	All(ctx context.Context) ([]tunnel.Region, error)
}

// publicRegions is the public DERP map.
type publicRegions struct{}

func (publicRegions) Nearest(ctx context.Context) (tunnel.Region, error) {
	return tunnel.NearestRegion(ctx)
}
func (publicRegions) All(ctx context.Context) ([]tunnel.Region, error) { return tunnel.Regions(ctx) }

// Options wires a Supervisor. Every field without a stated default is
// required.
type Options struct {
	Sites     *netsite.Registry
	Discovery SiteDropper
	Store     MetaStore
	Hub       Publisher
	// Config, Guard and ConfigPath are the live config and its single-writer
	// guard and file, through which every remote edit is saved.
	Config     *atomic.Pointer[model.Config]
	Guard      *config.Guard
	ConfigPath string
	// Build is Commander's own build, for the build-age comparison.
	Build proto.Build
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// Notify raises the remote-level alerts, and Alerts is where they are
	// found again to acknowledge them; with Notify nil none is raised. Rigs is
	// the live fleet, read for the rigs holding a live session through a
	// remote, which hold back its unreachable alert.
	Notify notify.Notifier
	Alerts AlertStore
	Rigs   RigSource
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// ForgetEndpoints clears the last-known endpoint of each of rigIDs that
	// names site, returning how many it cleared; Remove calls it, so the rigs'
	// storage format stays theirs. It is required: a removed remote's endpoints
	// must be cleared, so New refuses a wiring without it.
	ForgetEndpoints func(ctx context.Context, rigIDs []string, site netsite.ID) (int, error)

	// NewClient builds a remote's tunnel client from its stored pairing;
	// nil uses tunnel.NewClient.
	NewClient func(model.RemoteProfile, *slog.Logger) (tunnel.Client, error)
	// Regions defaults to the public DERP map.
	Regions RegionSource
	// Now defaults to time.Now.
	Now func() time.Time
	// HeartbeatInterval defaults to heartbeatInterval; HelloTimeout, when
	// set, replaces the site-scaled bound on one hello.
	HeartbeatInterval time.Duration
	HelloTimeout      time.Duration
}

// Supervisor supervises every paired remote.
type Supervisor struct {
	sites     *netsite.Registry
	discovery SiteDropper
	store     MetaStore
	hub       Publisher
	cfg       *atomic.Pointer[model.Config]
	guard     *config.Guard
	path      string
	build     proto.Build
	metrics   *metrics.Metrics
	notify    notify.Notifier
	alerts    AlertStore
	rigs      RigSource
	log       *slog.Logger
	regions   RegionSource
	now       func() time.Time
	interval  time.Duration
	helloTO   time.Duration
	mkClient  func(model.RemoteProfile, *slog.Logger) (tunnel.Client, error)
	// forgetEndpoints is Options.ForgetEndpoints.
	forgetEndpoints func(ctx context.Context, rigIDs []string, site netsite.ID) (int, error)

	// pubMu serialises publishing, so a list computed earlier is never stored
	// over a newer one, and guards the alert latches and the relay state that
	// publishing keeps.
	pubMu           sync.Mutex
	latches         map[string]map[model.Category]latch
	lastState       map[string]model.RemoteState
	waker           RigWaker
	relay           model.RelayStatus
	relayQuietSince time.Time

	mu      sync.Mutex
	remotes map[string]*remote
	ctx     context.Context // set by Start; nil before and after
	stop    context.CancelFunc
}

// New builds a supervisor. It touches nothing until Start. It fails when
// Options.ForgetEndpoints is nil.
func New(o Options) (*Supervisor, error) {
	if o.ForgetEndpoints == nil {
		return nil, errors.New("remote: supervisor needs a ForgetEndpoints hook to clear a removed remote's rig endpoints")
	}
	s := &Supervisor{
		sites: o.Sites, discovery: o.Discovery, store: o.Store, hub: o.Hub,
		cfg: o.Config, guard: o.Guard, path: o.ConfigPath, build: o.Build,
		metrics: o.Metrics, notify: o.Notify, alerts: o.Alerts, rigs: o.Rigs,
		log: o.Logger, regions: o.Regions, now: o.Now,
		interval: o.HeartbeatInterval, helloTO: o.HelloTimeout, mkClient: o.NewClient,
		forgetEndpoints: o.ForgetEndpoints,
		remotes:         map[string]*remote{}, latches: map[string]map[model.Category]latch{},
		lastState: map[string]model.RemoteState{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.regions == nil {
		s.regions = publicRegions{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.interval <= 0 {
		s.interval = heartbeatInterval
	}
	if s.guard == nil {
		s.guard = &config.Guard{}
	}
	if s.mkClient == nil {
		s.mkClient = func(p model.RemoteProfile, log *slog.Logger) (tunnel.Client, error) {
			return tunnel.NewClient(tunnel.ClientConfig{Address: tunnel.Secret(p.Address), ClientKey: tunnel.Secret(p.ClientKey), Logger: log})
		}
	}
	return s, nil
}

func (s *Supervisor) newClient(p model.RemoteProfile) (tunnel.Client, error) {
	return s.mkClient(p, s.log.With("remote", p.ID))
}

// helloTimeout bounds one heartbeat: a fresh client's first handshake and one
// round trip on top of the base.
func (s *Supervisor) helloTimeout(st netsite.Site) time.Duration {
	if s.helloTO > 0 {
		return s.helloTO
	}
	return proto.HelloTimeout + st.Costs().HandshakeAllowance + netsite.RoundTrips(st, 1)
}

// unreachableAfter is the configured threshold past which a remote whose
// heartbeat keeps failing reads Unreachable, as a LAN rig does.
func (s *Supervisor) unreachableAfter() time.Duration {
	if c := s.cfg.Load(); c != nil && c.Defaults.UnreachableAlertAfter.D() > 0 {
		return c.Defaults.UnreachableAlertAfter.D()
	}
	// A config without the setting falls back to its shipped default.
	return config.Defaults().Defaults.UnreachableAlertAfter.D()
}

// Start registers a site for every remote in the config and starts their
// heartbeats; they run until Stop or until ctx ends.
func (s *Supervisor) Start(ctx context.Context) {
	_ = s.guard.Do(func() error {
		s.apply(s.cfg.Load())
		return nil
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx, s.stop = context.WithCancel(ctx)
	for _, r := range s.remotes {
		s.startLocked(r)
	}
}

// Stop ends every heartbeat and closes every tunnel client. The sites stay
// registered, down, so a dial during shutdown fails closed.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
	}
	s.ctx, s.stop = nil, nil
	rs := make([]*remote, 0, len(s.remotes))
	for _, r := range s.remotes {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	for _, r := range rs {
		r.halt()
		r.site.swap(nil, nil)()
	}
}

// startLocked begins r's heartbeat when the supervisor is running. Caller
// holds s.mu.
func (s *Supervisor) startLocked(r *remote) {
	if s.ctx == nil || r.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	r.cancel, r.done = cancel, make(chan struct{})
	go r.run(ctx)
}

// halt stops r's heartbeat and waits for it to return; a hello in flight is
// cancelled.
func (r *remote) halt() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

// apply brings the supervised remotes in line with cfg: a new profile gets a
// site and a heartbeat, a changed pairing config (model.RemotePairingConfig) is
// re-paired, any other edit (a rename, the cap) is applied in place, and a
// profile that is gone is removed. The caller holds the config guard, so
// applies happen in the order their configs were saved.
func (s *Supervisor) apply(cfg *model.Config) {
	if cfg == nil {
		return
	}
	want := make(map[string]model.RemoteProfile, len(cfg.Remotes))
	for _, p := range cfg.Remotes {
		want[p.ID] = p
	}

	s.mu.Lock()
	var gone []*remote
	for id, r := range s.remotes {
		if _, ok := want[id]; !ok {
			gone = append(gone, r)
			delete(s.remotes, id)
		}
	}
	type change struct {
		r      *remote
		p      model.RemoteProfile
		repair bool
	}
	var changes []change
	var added []model.RemoteProfile
	for _, p := range cfg.Remotes {
		r, ok := s.remotes[p.ID]
		if !ok {
			added = append(added, p)
			continue
		}
		r.mu.Lock()
		old := r.prof
		r.mu.Unlock()
		// The pairing config is compared whole, so a field added to it
		// rebuilds the client without this switch learning its name.
		switch {
		case old.RemotePairingConfig != p.RemotePairingConfig:
			changes = append(changes, change{r, p, true})
		case old != p:
			changes = append(changes, change{r, p, false})
		}
	}
	s.mu.Unlock()

	// A removed remote stops dialling before anything else happens.
	for _, r := range gone {
		s.teardown(r, cfg)
	}
	for _, c := range changes {
		if c.repair {
			c.r.repair(c.p)
		} else {
			c.r.update(c.p)
		}
	}
	for _, p := range added {
		s.add(p)
	}
	s.publish()
}

// add begins supervising a profile: it registers the remote's site (down until
// its first hello and broadcast round) and starts its heartbeat.
func (s *Supervisor) add(p model.RemoteProfile) {
	r := newRemote(s, p, s.loadHistory(context.Background(), p.ID))
	if err := s.sites.Add(r.site); err != nil {
		s.log.Error("remote: could not register the remote's site; it stays unsupervised", "remote", p.ID, "error", err)
		r.site.swap(nil, nil)()
		return
	}
	s.metrics.SetRemoteUp(p.ID, false)
	// The initial state is logged here, so publish's transition tracking starts
	// from it rather than treating the first publish as a first sighting.
	initial := r.status(s.now()).State
	s.pubMu.Lock()
	s.lastState[p.ID] = initial
	s.pubMu.Unlock()
	s.mu.Lock()
	s.remotes[p.ID] = r
	s.startLocked(r)
	s.mu.Unlock()
	r.log.Info("remote: supervising", "name", p.Name, "state", string(initial), "fingerprint", r.fingerprint)
}

// teardown removes a remote from service. The site is unregistered first, so
// every later dial naming it fails closed at once; then its client closes,
// which fails the operations in flight through it as network errors; then
// discovery forgets what was seen through it and every rig's last-known
// endpoint naming it is cleared, as rig deletion clears its own. Site ids are
// never reused, so a cycle that records an endpoint on the removed site after
// this names an unknown site and fails closed.
func (s *Supervisor) teardown(r *remote, cfg *model.Config) {
	id := r.site.id
	s.sites.Remove(id)
	r.halt()
	r.site.swap(nil, nil)()
	if s.discovery != nil {
		s.discovery.DropSite(id)
	}
	ctx := context.Background()
	rigIDs := make([]string, 0, len(cfg.Rigs))
	for _, rp := range cfg.Rigs {
		rigIDs = append(rigIDs, rp.ID)
	}
	cleared, err := s.forgetEndpoints(ctx, rigIDs, id)
	if err != nil {
		s.log.Warn("remote: could not clear every rig's last-known endpoint", "remote", string(id), "error", err)
	}
	if err := s.store.SetMeta(ctx, historyKey(string(id)), ""); err != nil {
		s.log.Warn("remote: could not clear connection history", "remote", string(id), "error", err)
	}
	s.metrics.ForgetRemote(string(id))
	s.forgetAlerts(string(id))
	r.log.Info("remote: removed; no longer dialled", "cleared_endpoints", cleared)
}

// publish sends every remote's state and Commander's relay reachability to
// the hub, then acts on what changed. It is the one place a remote's state
// transitions are acted on: logged and the rigs reached through it woken
// (onTransitions), and its alerts raised or retired (alertRemotes). List and
// status are side-effect-free getters, so GET /api/remotes never logs a
// transition.
func (s *Supervisor) publish() {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	list := s.List()
	relay := s.relayStatus()
	if s.hub != nil {
		s.hub.SetRemotes(list)
		s.hub.SetRelay(relay)
	}
	s.onTransitions(list)
	s.alertRemotes(list)
}

// List returns every paired remote's state, in id order.
func (s *Supervisor) List() []model.RemoteStatus {
	s.mu.Lock()
	rs := make([]*remote, 0, len(s.remotes))
	for _, r := range s.remotes {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	now := s.now()
	out := make([]model.RemoteStatus, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.status(now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Supervisor) statusOf(id string) model.RemoteStatus {
	s.mu.Lock()
	r := s.remotes[id]
	s.mu.Unlock()
	if r == nil {
		return model.RemoteStatus{ID: id}
	}
	return r.status(s.now())
}

// Regions lists the relay regions a pairing can use, the nearest first and
// flagged. When the nearest cannot be measured the list is still returned,
// with none flagged.
func (s *Supervisor) Regions(ctx context.Context) ([]model.RemoteRegion, error) {
	all, err := s.regions.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelayUnavailable, err)
	}
	nctx, cancel := context.WithTimeout(ctx, nearestRegionTimeout)
	near, nerr := s.regions.Nearest(nctx)
	cancel()
	if nerr != nil {
		s.log.Warn("remote: could not measure the nearest relay region", "error", nerr)
	}
	out := make([]model.RemoteRegion, 0, len(all))
	for _, r := range all {
		m := model.RemoteRegion{ID: r.ID, Code: r.Code, Name: r.Name, Nearest: nerr == nil && r.ID == near.ID}
		if m.Nearest {
			out = append([]model.RemoteRegion{m}, out...)
		} else {
			out = append(out, m)
		}
	}
	return out, nil
}

// region resolves a region choice: 0 is the nearest.
func (s *Supervisor) region(ctx context.Context, id int) (tunnel.Region, error) {
	if id == 0 {
		nctx, cancel := context.WithTimeout(ctx, nearestRegionTimeout)
		defer cancel()
		r, err := s.regions.Nearest(nctx)
		if err != nil {
			return tunnel.Region{}, fmt.Errorf("%w: pick the nearest region: %w", ErrRelayUnavailable, err)
		}
		return r, nil
	}
	all, err := s.regions.All(ctx)
	if err != nil {
		return tunnel.Region{}, fmt.Errorf("%w: %w", ErrRelayUnavailable, err)
	}
	for _, r := range all {
		if r.ID == id {
			return r, nil
		}
	}
	return tunnel.Region{}, fmt.Errorf("%w: %d", ErrUnknownRegion, id)
}

// commit saves an edit of the remotes: under the config guard it edits a copy
// of the live config, validates and saves it, stores it, and applies it. The
// remotes reach config.yaml only through here.
func (s *Supervisor) commit(edit func(*model.Config) error) error {
	return s.guard.Do(func() error {
		next := config.Clone(s.cfg.Load())
		if next == nil {
			return errors.New("remote: no config loaded")
		}
		if err := edit(next); err != nil {
			return err
		}
		if problems := config.Validate(next, nil); config.HasErrors(problems) {
			return fmt.Errorf("remote: config invalid: %v", problems)
		}
		if err := config.Save(s.path, next); err != nil {
			return fmt.Errorf("remote: save config: %w", err)
		}
		s.cfg.Store(next)
		s.apply(next)
		return nil
	})
}

// nextRemoteID hands out the next remote id, "r<n>". It counts past both the
// stored sequence and every id in use, so an id is never handed out twice even
// if the sequence was lost from a hand-edited config.
func nextRemoteID(c *model.Config) string {
	n := c.RemoteSeq
	for _, r := range c.Remotes {
		if v, err := strconv.Atoi(strings.TrimPrefix(r.ID, "r")); err == nil && v > n {
			n = v
		}
	}
	c.RemoteSeq = n + 1
	return "r" + strconv.Itoa(c.RemoteSeq)
}

// Add pairs a new remote on a relay region (0 for the nearest) and returns its
// state and, once, the env value the user puts in the remote's .env.
// Commander keeps only the address and its own client key.
func (s *Supervisor) Add(ctx context.Context, name string, regionID int) (model.RemotePairing, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.RemotePairing{}, errors.New("remote: a remote needs a name")
	}
	region, err := s.region(ctx, regionID)
	if err != nil {
		return model.RemotePairing{}, err
	}
	p, err := tunnel.Mint(region)
	if err != nil {
		return model.RemotePairing{}, err
	}
	var id string
	now := s.now().Unix()
	err = s.commit(func(c *model.Config) error {
		id = nextRemoteID(c)
		c.Remotes = append(c.Remotes, model.RemoteProfile{
			ID: id, Name: name,
			Address: p.Address.Reveal(), ClientKey: p.ClientKey.Reveal(),
			Region: region.Code, RegionName: region.Name,
			CreatedAt: now, PairedAt: now,
		})
		return nil
	})
	if err != nil {
		return model.RemotePairing{}, err
	}
	s.log.Info("remote: added", "remote", id, "name", name, "region", region.Code, "fingerprint", p.Fingerprint)
	return pairingOf(s.statusOf(id), p), nil
}

// pairingOf is the once-only answer to an Add or a Repair.
func pairingOf(st model.RemoteStatus, p *tunnel.Pairing) model.RemotePairing {
	return model.RemotePairing{Remote: st, EnvValue: p.EnvValue.Reveal(), EnvFile: tunnel.EnvFile(p.EnvValue).Reveal()}
}

// Repair mints a fresh pairing for a remote, on a relay region (0 keeps the
// current one), and returns, once, the new env value. The old env stops
// working as soon as this returns; the remote keeps its id and its connection
// history.
func (s *Supervisor) Repair(ctx context.Context, id string, regionID int) (model.RemotePairing, error) {
	cur := s.cfg.Load().Remote(id)
	if cur == nil {
		return model.RemotePairing{}, ErrUnknownRemote
	}
	var region tunnel.Region
	var err error
	if regionID == 0 {
		region, err = tunnel.AddressRegion(tunnel.Secret(cur.Address), cur.Region, cur.RegionName)
	} else {
		region, err = s.region(ctx, regionID)
	}
	if err != nil {
		return model.RemotePairing{}, err
	}
	p, err := tunnel.Mint(region)
	if err != nil {
		return model.RemotePairing{}, err
	}
	err = s.commit(func(c *model.Config) error {
		r := c.Remote(id)
		if r == nil {
			return ErrUnknownRemote
		}
		r.Address, r.ClientKey = p.Address.Reveal(), p.ClientKey.Reveal()
		r.Region, r.RegionName = region.Code, region.Name
		r.PairedAt = s.now().Unix()
		return nil
	})
	if err != nil {
		return model.RemotePairing{}, err
	}
	s.repairedAlerts(id)
	s.log.Info("remote: re-paired", "remote", id, "region", region.Code, "fingerprint", p.Fingerprint)
	return pairingOf(s.statusOf(id), p), nil
}

// Rename changes a remote's display name only.
func (s *Supervisor) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("remote: a remote needs a name")
	}
	return s.commit(func(c *model.Config) error {
		r := c.Remote(id)
		if r == nil {
			return ErrUnknownRemote
		}
		r.Name = name
		return nil
	})
}

// SetRateLimit sets a remote's bandwidth cap in megabytes per second; 0 removes
// it. It applies to the remote's open connections too.
func (s *Supervisor) SetRateLimit(id string, mbps float64) error {
	if mbps < 0 {
		return errors.New("remote: a bandwidth cap cannot be negative")
	}
	return s.commit(func(c *model.Config) error {
		r := c.Remote(id)
		if r == nil {
			return ErrUnknownRemote
		}
		r.RateLimitMBps = mbps
		return nil
	})
}

// Remove unpairs a remote: it is never dialled again, its rigs' last-known
// endpoints through it and its discovery sightings are cleared, and its id is
// never reused.
func (s *Supervisor) Remove(id string) error {
	return s.commit(func(c *model.Config) error {
		for i, r := range c.Remotes {
			if r.ID == id {
				c.Remotes = append(c.Remotes[:i:i], c.Remotes[i+1:]...)
				return nil
			}
		}
		return ErrUnknownRemote
	})
}
