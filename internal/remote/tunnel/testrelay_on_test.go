//go:build remotetest

package tunnel

import "testing"

// TestRemoteTestBuildAdmitsTestRelay: the remotetest tag is what lets the
// cross-version test's binaries and the integration tests run through a local
// relay, on both sides of the tunnel.
func TestRemoteTestBuildAdmitsTestRelay(t *testing.T) {
	p, err := Mint(startLocalRelay(t).region)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := ParseEnv(p.EnvValue.Reveal()); err != nil {
		t.Fatalf("ParseEnv refused a test relay in a remotetest build: %v", err)
	}
	c, err := NewClient(ClientConfig{Address: p.Address, ClientKey: p.ClientKey})
	if err != nil {
		t.Fatalf("NewClient refused a test relay in a remotetest build: %v", err)
	}
	c.Close()
	if _, err := Fingerprint(p.Address, p.ClientKey); err != nil {
		t.Fatalf("Fingerprint refused a test relay in a remotetest build: %v", err)
	}
}
