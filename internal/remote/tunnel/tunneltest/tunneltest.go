//go:build remotetest

// Package tunneltest lets tests outside the tunnel package run a real tailcat
// tunnel through a local DERP and STUN relay, never public DERP. It exists only
// under the remotetest build tag, the only builds whose ParseEnv, NewClient and
// Fingerprint admit a test relay; no release is built with that tag, so a
// shipped build can neither start this relay nor accept a pairing minted on it.
package tunneltest

import (
	"testing"

	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel/internal/relaytest"
)

// Hermetic keeps the test binary's tunnels off the real network: no portmapper
// probes and no captive-portal requests. Call it from TestMain.
func Hermetic() { relaytest.Hermetic() }

// Relay is a running local relay.
type Relay struct {
	r      *relaytest.Relay
	region tunnel.Region
}

// StartRelay runs a relay on loopback until the test ends.
func StartRelay(t testing.TB) *Relay {
	t.Helper()
	r := relaytest.Start(t)
	return &Relay{r: r, region: tunnel.TestRelayRegion(r.Region)}
}

// Region is the relay's region, for tunnel.Mint.
func (r *Relay) Region() tunnel.Region { return r.region }

// Cut drops every connection to the relay and refuses new ones until Restore,
// as an uplink outage would; a tunnel's DERP home connection is lost.
func (r *Relay) Cut() { r.r.Cut() }

// Restore lets connections reach the relay again, at the same address.
func (r *Relay) Restore() { r.r.Restore() }
