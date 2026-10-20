//go:build !demo

package main

import "testing"

// The lifetime cap is demo-only: a production build must not know the flag.
func TestNonDemoBuildHasNoDemoLifetimeFlag(t *testing.T) {
	if code := realMain([]string{"run", "-data-dir", t.TempDir(), "-demo-max-lifetime", "1s"}); code != 2 {
		t.Fatalf("a non-demo build accepted -demo-max-lifetime (exit %d, want the usage error 2)", code)
	}
}
