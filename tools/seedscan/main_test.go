package main

import (
	"net"
	"testing"
)

// A responder that answers any scanner on the LAN is discovered as real hardware
// by every other Commander on it, so a fake device would appear in a live
// instance's rig list. Only this host's own scans get an answer.
func TestOnlyLocalScansAreAnswered(t *testing.T) {
	local := map[string]bool{"192.168.1.100": true, "fe80::1": true}
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"this host's LAN address", "192.168.1.100", true},
		{"loopback", "127.0.0.1", true},
		{"loopback, not in the interface list", "127.0.0.2", true},
		{"this host's IPv6 address", "fe80::1", true},
		{"another machine on the same subnet", "192.168.1.101", false},
		{"a real ASIAIR", "192.168.1.75", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.src)
			if ip == nil {
				t.Fatalf("bad test address %q", tc.src)
			}
			if got := isLocalRequest(ip, local); got != tc.want {
				t.Errorf("isLocalRequest(%s) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// An enumeration failure yields an empty set, and an empty set must answer
// nothing but loopback rather than everything.
func TestEmptyAddressSetAnswersNoRemoteScan(t *testing.T) {
	if isLocalRequest(net.ParseIP("192.168.1.101"), map[string]bool{}) {
		t.Error("a remote scan was answered with no known local addresses")
	}
	if !isLocalRequest(net.ParseIP("127.0.0.1"), map[string]bool{}) {
		t.Error("loopback must still be answered")
	}
}
