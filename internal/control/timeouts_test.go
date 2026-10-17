package control

import (
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestTimeoutsScaleWithSiteLatency pins the control channel's timeouts: the
// fallback dial and each call add 1 round-trip margin of the site's latency,
// so on the local site they are the base 3 s and 5 s exactly.
func TestTimeoutsScaleWithSiteLatency(t *testing.T) {
	local := defaultTimeouts(netsite.NewRegistry(nil).Site(netsite.Local))
	if local.dial != 3*time.Second || local.call != 5*time.Second {
		t.Fatalf("local dial/call = %v/%v, want 3s/5s", local.dial, local.call)
	}
	f := netsite.NewFake("r1")
	f.SetLatency(90 * time.Millisecond)
	margin := netsite.RoundTrips(f, 1)
	remote := defaultTimeouts(f)
	if remote.dial != 3*time.Second+margin || remote.call != 5*time.Second+margin {
		t.Fatalf("remote dial/call = %v/%v, want 3s/5s plus %v", remote.dial, remote.call, margin)
	}
}
