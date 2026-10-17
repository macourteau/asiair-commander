//go:build remotetest

package tunnel

import "tailscale.com/tailcfg"

// testRelayAllowed lets ParseEnv, NewClient and Fingerprint admit a test
// relay, so the cross-version test's binaries and the integration tests can run
// through a local relay. Only builds made with the remotetest tag carry it; no
// release is built with that tag.
const testRelayAllowed = true

// TestRelayRegion wraps a local test relay's region (relaytest, through
// tunneltest) as a Region that Mint accepts. It exists only in builds made with
// the remotetest tag.
func TestRelayRegion(def *tailcfg.DERPRegion) Region {
	return Region{ID: int(def.RegionID), Code: def.RegionCode, Name: def.RegionName, def: def}
}
