package tunnel

import (
	"strings"
	"testing"
)

// TestDebugAddrRefused: while tailcat's address-printing knob is on, or set to
// something that is not a boolean, Commander's side mints no pairing and builds
// no client, and the refusal carries no secret; off or unset, both work.
func TestDebugAddrRefused(t *testing.T) {
	p, err := Mint(testRegion)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	cfg := ClientConfig{Address: p.Address, ClientKey: p.ClientKey}

	for _, v := range []string{"1", "true", "yes please"} {
		t.Run("refused "+v, func(t *testing.T) {
			t.Setenv(DebugAddrVar, v)
			if CheckDebugAddr(nil) == nil {
				t.Fatal("CheckDebugAddr accepted the knob")
			}
			if got, err := Mint(testRegion); err == nil || got != nil {
				t.Fatal("Mint succeeded with the knob on")
			}
			c, err := NewClient(cfg)
			if err == nil {
				c.Close()
				t.Fatal("NewClient succeeded with the knob on")
			}
			if !strings.Contains(err.Error(), DebugAddrVar) {
				t.Fatalf("error %q does not name %s", err, DebugAddrVar)
			}
			if strings.Contains(err.Error(), p.Address.Reveal()) || strings.Contains(err.Error(), p.ClientKey.Reveal()) {
				t.Fatal("the refusal carries a secret")
			}
		})
	}
	for _, v := range []string{"", "0", "false"} {
		t.Run("allowed "+v, func(t *testing.T) {
			t.Setenv(DebugAddrVar, v)
			if _, err := Mint(testRegion); err != nil {
				t.Fatalf("Mint: %v", err)
			}
			c, err := NewClient(cfg)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			c.Close()
		})
	}
}

// TestCheckDebugAddrReadsGetenv: the check reads the getenv it is given, so
// the remote role's injected environment decides, not the process's.
func TestCheckDebugAddrReadsGetenv(t *testing.T) {
	t.Setenv(DebugAddrVar, "")
	on := func(k string) string {
		if k == DebugAddrVar {
			return "1"
		}
		return ""
	}
	if CheckDebugAddr(on) == nil {
		t.Fatal("CheckDebugAddr ignored the getenv it was given")
	}
}
