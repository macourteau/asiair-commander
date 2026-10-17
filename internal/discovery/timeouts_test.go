package discovery

import (
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestTimeoutsScaleWithSiteLatency pins discovery's timeouts: the identity
// probe's budget adds 1 round-trip margin and the site's handshake allowance,
// and the adopt-share listing's adds 3 round-trip margins; on the local site
// both are the base 3 s exactly.
func TestTimeoutsScaleWithSiteLatency(t *testing.T) {
	local := netsite.NewRegistry(nil).Site(netsite.Local)
	if got := probeBudget(local); got != 3*time.Second {
		t.Fatalf("local probe budget = %v, want 3s", got)
	}
	if got := sharesBudget(local); got != 3*time.Second {
		t.Fatalf("local shares budget = %v, want 3s", got)
	}

	f := netsite.NewFake("r1")
	f.SetLatency(100 * time.Millisecond)
	f.SetHandshakeAllowance(5 * time.Second)
	margin := netsite.RoundTrips(f, 1)
	if got, want := probeBudget(f), 3*time.Second+margin+5*time.Second; got != want {
		t.Fatalf("remote probe budget = %v, want %v (3s + 1 round trip + handshake)", got, want)
	}
	if got, want := sharesBudget(f), 3*time.Second+3*margin; got != want {
		t.Fatalf("remote shares budget = %v, want %v (3s + 3 round trips)", got, want)
	}
}
