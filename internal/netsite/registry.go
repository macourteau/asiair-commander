package netsite

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"
)

// Registry maps site ids to sites. It always holds the local site; other sites
// are added and removed at runtime, and lookups are safe from any goroutine.
// It is distinct from discovery's device registry, which is keyed by guid.
type Registry struct {
	logger *slog.Logger

	mu    sync.RWMutex
	sites map[ID]Site
}

var _ Sites = (*Registry)(nil)

// NewRegistry returns a registry holding only the local site. A nil logger
// uses slog.Default.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		logger: logger,
		sites:  map[ID]Site{Local: localSite{logger: logger}},
	}
}

// Add registers s under its id. The local site is fixed, and an id already
// registered is refused rather than replaced, so a dial in flight never
// silently changes network.
func (r *Registry) Add(s Site) error {
	id := s.ID()
	if !ValidID(id) {
		return fmt.Errorf("netsite: invalid site id %q", string(id))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sites[id]; ok {
		return fmt.Errorf("netsite: site %q already registered", string(id))
	}
	r.sites[id] = s
	r.logger.Info("netsite: site registered", "site", string(id))
	return nil
}

// Remove unregisters a site, so every later dial naming it fails. The local
// site cannot be removed.
func (r *Registry) Remove(id ID) {
	if id == Local {
		r.logger.Warn("netsite: refusing to remove the local site")
		return
	}
	r.mu.Lock()
	_, ok := r.sites[id]
	delete(r.sites, id)
	r.mu.Unlock()
	if ok {
		r.logger.Info("netsite: site removed", "site", string(id))
	}
}

// Site returns the site registered under id, gated so that a dial or probe
// made while it is not up fails closed. An unknown id yields a site that fails
// every dial and probe. Either way no socket is opened on any other network.
func (r *Registry) Site(id ID) Site {
	r.mu.RLock()
	s, ok := r.sites[id]
	r.mu.RUnlock()
	if !ok {
		return unknownSite{id: id, logger: r.logger}
	}
	return gatedSite{Site: s, logger: r.logger}
}

// All returns every registered site, the local one first and the rest in id
// order, each gated as Site gates it. Discovery fans its broadcast round out
// over them.
func (r *Registry) All() []Site {
	r.mu.RLock()
	out := make([]Site, 0, len(r.sites))
	for _, s := range r.sites {
		out = append(out, gatedSite{Site: s, logger: r.logger})
	}
	r.mu.RUnlock()
	// Local's id is "", so it sorts first.
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// gatedSite refuses traffic while its site is not up. The state is read at
// each dial, not at lookup, so a site that goes down mid-cycle stops carrying
// the cycle's later connections.
type gatedSite struct {
	Site
	logger *slog.Logger
}

func (g gatedSite) DialTCP(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	if st := g.State(); st != StateUp {
		g.logger.Debug("netsite: dial refused, site not up", "site", string(g.ID()), "state", st.String())
		return nil, &UnavailableError{Site: g.ID(), Reason: "site is " + st.String()}
	}
	c, err := g.Site.DialTCP(ctx, host, port, timeout)
	if err != nil {
		return nil, err
	}
	// Applied here rather than by each site, so no connection a site dials
	// escapes its rate limit.
	if l, ok := g.Site.(LimitsReads); ok {
		return l.LimitReads(c), nil
	}
	return c, nil
}

func (g gatedSite) ProbeUDP(ctx context.Context, host string, port int, req []byte, timeout time.Duration, accept func([]byte, string) bool) bool {
	if st := g.State(); st != StateUp {
		g.logger.Debug("netsite: probe refused, site not up", "site", string(g.ID()), "state", st.String())
		return false
	}
	return g.Site.ProbeUDP(ctx, host, port, req, timeout, accept)
}

func (g gatedSite) Broadcast(ctx context.Context) ([]Datagram, error) {
	if st := g.State(); st != StateUp {
		g.logger.Debug("netsite: broadcast refused, site not up", "site", string(g.ID()), "state", st.String())
		return nil, &UnavailableError{Site: g.ID(), Reason: "site is " + st.String()}
	}
	return g.Site.Broadcast(ctx)
}

// MeasureDigests is refused while the site is not up, as a dial is: a
// measurement is SMB traffic to the device.
func (g gatedSite) MeasureDigests(ctx context.Context, req DigestRequest) (DigestResult, error) {
	if st := g.State(); st != StateUp {
		g.logger.Debug("netsite: digest measurement refused, site not up", "site", string(g.ID()), "state", st.String())
		return DigestResult{}, &UnavailableError{Site: g.ID(), Reason: "site is " + st.String()}
	}
	return g.Site.MeasureDigests(ctx, req)
}

// unknownSite stands in for an id with no registered site: one never added,
// or one removed. It reports itself down and carries nothing.
type unknownSite struct {
	id     ID
	logger *slog.Logger
}

func (u unknownSite) ID() ID     { return u.id }
func (unknownSite) State() State { return StateDown }

// An unknown site carries nothing, so its costs are the local site's, except
// that RepliesMatchSource is true so that, were a reply ever attributed to an
// unknown site, it would get the stricter treatment. It is not a remote: one
// never paired or since removed is not one whose state stands in for a rig's.
func (unknownSite) Costs() Costs {
	c := localCosts
	c.RepliesMatchSource = true
	return c
}

func (u unknownSite) Broadcast(context.Context) ([]Datagram, error) {
	u.logger.Warn("netsite: broadcast refused, unknown site", "site", string(u.id))
	return nil, &UnavailableError{Site: u.id, Reason: "unknown site"}
}

func (u unknownSite) DialTCP(context.Context, string, int, time.Duration) (net.Conn, error) {
	u.logger.Warn("netsite: dial refused, unknown site", "site", string(u.id))
	return nil, &UnavailableError{Site: u.id, Reason: "unknown site"}
}

func (u unknownSite) ProbeUDP(context.Context, string, int, []byte, time.Duration, func([]byte, string) bool) bool {
	u.logger.Warn("netsite: probe refused, unknown site", "site", string(u.id))
	return false
}

func (u unknownSite) MeasureDigests(context.Context, DigestRequest) (DigestResult, error) {
	u.logger.Warn("netsite: digest measurement refused, unknown site", "site", string(u.id))
	return DigestResult{}, &UnavailableError{Site: u.id, Reason: "unknown site"}
}
