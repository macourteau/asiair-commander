package device

import (
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestTimeoutsScaleWithSiteLatency pins the device's own timeout: with no
// timeout passed, the dial and each operation get defaultTimeout plus 1
// round-trip margin of the endpoint's site, so defaultTimeout exactly on the
// local site; a timeout the caller passes is used as given, its margins
// already added.
func TestTimeoutsScaleWithSiteLatency(t *testing.T) {
	sites := netsite.NewRegistry(nil)
	f := netsite.NewFake("r1")
	f.SetLatency(80 * time.Millisecond)
	if err := sites.Add(f); err != nil {
		t.Fatal(err)
	}
	margin := netsite.RoundTrips(f, 1)

	local := New(sites, netsite.LocalEndpoint("10.0.0.1"), "u", "p", 0).(*SMBDevice)
	if local.dialTimeout() != defaultTimeout || local.opTimeout() != defaultTimeout {
		t.Fatalf("local fallback dial/op = %v/%v, want %v", local.dialTimeout(), local.opTimeout(), defaultTimeout)
	}
	remote := New(sites, netsite.Endpoint{Site: "r1", IP: "10.0.0.1"}, "u", "p", 0).(*SMBDevice)
	if want := defaultTimeout + margin; remote.dialTimeout() != want || remote.opTimeout() != want {
		t.Fatalf("remote fallback dial/op = %v/%v, want %v", remote.dialTimeout(), remote.opTimeout(), want)
	}
	given := New(sites, netsite.Endpoint{Site: "r1", IP: "10.0.0.1"}, "u", "p", 7*time.Second).(*SMBDevice)
	if given.dialTimeout() != 7*time.Second || given.opTimeout() != 7*time.Second {
		t.Fatalf("given timeout dial/op = %v/%v, want 7s", given.dialTimeout(), given.opTimeout())
	}
}
