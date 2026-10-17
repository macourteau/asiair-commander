package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// transport is the UDP seam: the real implementation talks to sites, a fake
// drives tests, mirroring the device/localfs philosophy so discovery runs
// hermetically without real broadcast traffic.
type transport interface {
	// Broadcast runs one scan_air round on every up site and returns the
	// replies, each tagged with the site it arrived through and deduped by guid
	// within that site. A site whose round fails contributes nothing; its error
	// is returned alongside the other sites' replies.
	Broadcast(ctx context.Context) ([]scannedDevice, error)
	// Probe sends a unicast scan_air to ep through ep's site and returns the
	// first well-formed reply within a short timeout (ok=false on
	// timeout/garbled, or when the site carries no traffic).
	Probe(ctx context.Context, ep netsite.Endpoint, req []byte) (scannedDevice, bool)
	// Site returns the site registered under id, for its costs.
	Site(id netsite.ID) netsite.Site
}

// siteSet is what the transport needs of the site registry: a lookup for the
// probe, and every site for the broadcast fan-out. *netsite.Registry
// implements it.
type siteSet interface {
	netsite.Sites
	All() []netsite.Site
}

const (
	// probeTimeout is the base of a unicast Probe's whole budget, retries
	// included; probeBudget adds the site's costs.
	probeTimeout = 3 * time.Second
	// probeFirstWait is how long the first probe datagram waits for its reply
	// before it is re-sent; each later wait doubles, and the last takes whatever
	// budget remains. Long enough that a reply slower than a LAN's is not cut
	// short by its own retransmit, short enough that a lost datagram costs a
	// third of the local budget, not all of it.
	probeFirstWait = time.Second
)

// probeBudget is the identity probe's deadline on site, every re-send
// included: probeTimeout, plus 1 round-trip margin (the datagram and its
// reply), plus the site's handshake allowance, since the probe may be the first
// traffic a freshly built path carries. On the local site it is probeTimeout
// exactly.
func probeBudget(site netsite.Site) time.Duration {
	c := site.Costs()
	return probeTimeout + c.RoundTrips(1) + c.HandshakeAllowance
}

// udpTransport is the real site-backed transport. IPv4-only: scan_air is used
// over IPv4. Broadcast fans out to every site; Probe goes through the site its
// endpoint names.
type udpTransport struct {
	sites  siteSet
	logger *slog.Logger
}

func newUDPTransport(sites siteSet, logger *slog.Logger) udpTransport {
	if logger == nil {
		logger = slog.Default()
	}
	return udpTransport{sites: sites, logger: logger}
}

// Site returns the site registered under id from the transport's registry.
func (t udpTransport) Site(id netsite.ID) netsite.Site { return t.sites.Site(id) }

// Broadcast runs a broadcast round on every up site concurrently and parses
// what each returns. A site that is not up is skipped, so its devices age out
// like powered-off ones.
func (t udpTransport) Broadcast(ctx context.Context) ([]scannedDevice, error) {
	var up []netsite.Site
	for _, site := range t.sites.All() {
		if st := site.State(); st != netsite.StateUp {
			t.logger.Debug("discovery: site not up, skipping its broadcast round",
				"site", string(site.ID()), "state", st.String())
			continue
		}
		up = append(up, site)
	}

	results := make([][]scannedDevice, len(up))
	errs := make([]error, len(up))
	var wg sync.WaitGroup
	for i, site := range up {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = t.round(ctx, site)
		}()
	}
	wg.Wait()

	var out []scannedDevice
	for _, r := range results {
		out = append(out, r...)
	}
	return out, errors.Join(errs...)
}

// round runs one site's broadcast round and parses its replies, tagging each
// with the site and the site's minimum recent latency.
func (t udpTransport) round(ctx context.Context, site netsite.Site) ([]scannedDevice, error) {
	id := site.ID()
	costs := site.Costs()
	latency := costs.MinLatency
	datagrams, err := site.Broadcast(ctx)
	if err != nil {
		return nil, fmt.Errorf("site %q: %w", string(id), err)
	}
	matchSource := costs.RepliesMatchSource

	seen := map[string]bool{}
	var out []scannedDevice
	for _, dg := range datagrams {
		dev, ok := parseReply(dg.Data, dg.SrcIP)
		if !ok || seen[dev.GUID] {
			continue
		}
		// Such a site forwards only to addresses that answered from themselves,
		// so recording an address that differs from the reply's source would
		// point the rig at a path the site refuses to carry.
		if matchSource && !sameIP(dev.Address, dg.SrcIP) {
			t.logger.Debug("discovery: dropping reply whose address differs from its source",
				"site", string(id), "guid", dev.GUID, "address", dev.Address, "source", dg.SrcIP)
			continue
		}
		seen[dev.GUID] = true
		dev.Site = id
		dev.PathLatency = latency
		out = append(out, dev)
	}
	return out, nil
}

// sameIP reports whether a and b are the same IP address; anything that does
// not parse as one matches nothing.
func sameIP(a, b string) bool {
	ia, ib := net.ParseIP(a), net.ParseIP(b)
	return ia != nil && ib != nil && ia.Equal(ib)
}

// Probe sends a unicast scan_air to ep's scanPort through ep's site and returns
// the first well-formed reply within probeBudget. Confirmed against real
// hardware: the device answers a directed unicast with the same guid as its
// broadcast reply, so this ties a guid to the exact device at an address with
// no dependence on the possibly-stale registry.
//
// A datagram, or its reply, can be lost, so an unanswered probe is re-sent with
// a doubling wait until the budget runs out; scan_air is idempotent. Nothing
// outlives the budget: an unanswered probe leaves the caller's session
// unconfirmed, and the next cycle probes afresh.
func (t udpTransport) Probe(ctx context.Context, ep netsite.Endpoint, req []byte) (scannedDevice, bool) {
	site := t.sites.Site(ep.Site)
	var dev scannedDevice
	accept := func(reply []byte, srcIP string) bool {
		d, parsed := parseReply(reply, srcIP)
		if parsed {
			dev = d
		}
		return parsed
	}
	deadline := time.Now().Add(probeBudget(site))
	wait := probeFirstWait + netsite.RoundTrips(site, 1)
	sends := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// The last wait takes the rest of the budget rather than a doubled share
		// that would leave too little for another send.
		if 2*wait > remaining {
			wait = remaining
		}
		start := time.Now()
		sends++
		if site.ProbeUDP(ctx, ep.IP, scanPort, req, wait, accept) {
			if sends > 1 {
				t.logger.Debug("discovery: identity probe answered after re-send",
					"site", string(ep.Site), "address", ep.IP, "sends", sends)
			}
			return dev, true
		}
		// A site that is not up refuses every send at once; re-sending cannot
		// change that within the budget.
		if ctx.Err() != nil || site.State() != netsite.StateUp {
			break
		}
		// A send that failed early must not turn the retries into a spin.
		if !sleepCtx(ctx, time.Until(start.Add(wait))) {
			break
		}
		wait *= 2
	}
	t.logger.Debug("discovery: identity probe unanswered within its budget",
		"site", string(ep.Site), "address", ep.IP, "sends", sends)
	return scannedDevice{}, false
}

// sleepCtx waits d, or until ctx is done; it reports whether the full wait
// elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}
