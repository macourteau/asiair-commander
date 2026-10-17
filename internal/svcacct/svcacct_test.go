//go:build !windows

package svcacct

import (
	"os/user"
	"testing"
)

// TestServiceAccountNameReturnsCurrentUser verifies the non-Windows path resolves
// to the running user's name (never empty), matching os/user.Current.
func TestServiceAccountNameReturnsCurrentUser(t *testing.T) {
	got := ServiceAccountName()
	if got == "" {
		t.Fatal("ServiceAccountName returned empty; the UI always needs a name to show")
	}
	if u, err := user.Current(); err == nil && u.Username != "" && got != u.Username {
		t.Errorf("ServiceAccountName = %q, want current user %q", got, u.Username)
	}
}
