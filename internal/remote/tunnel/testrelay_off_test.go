//go:build !remotetest

package tunnel

import "testing"

// TestProductionBuildRefusesTestRelay: a build without the remotetest tag, which
// is every shipped build, never admits a test relay through ParseEnv.
func TestProductionBuildRefusesTestRelay(t *testing.T) {
	if testRelayAllowed {
		t.Fatal("testRelayAllowed is true without the remotetest tag")
	}
	p, err := Mint(startLocalRelay(t).region)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := ParseEnv(p.EnvValue.Reveal()); err == nil {
		t.Fatal("ParseEnv accepted a test relay region in a production build")
	}
}
