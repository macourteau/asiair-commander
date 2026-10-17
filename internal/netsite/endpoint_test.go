package netsite

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// validHostname is RFC 1123 hostname syntax: dot-separated labels of 1–63
// letters, digits and hyphens, not starting or ending with a hyphen, 253 bytes
// at most. A stored endpoint form that fails it is one no resolver will look up.
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// TestEndpointLegacyAddressIsLocal (design remote-sites AC1, invariant 4):
// every address that predates sites — a lastip: IP, a Host name, a test
// fixture, the demo resolver's guid-as-address — parses as the local site,
// keeps its stored form, and dials exactly as a plain net.Dialer does. A
// non-local endpoint's form round-trips and is something an older binary can
// neither parse as an IP nor resolve.
func TestEndpointLegacyAddressIsLocal(t *testing.T) {
	legacy := map[string]string{
		"lastip IP":       "192.168.1.75",
		"Host name":       "asiair.local",
		"test fixture":    "10.0.0.1",
		"loopback":        "127.0.0.1",
		"demo guid":       "0a1b2c3d",
		"bare short host": "h",
	}
	for name, v := range legacy {
		ep, err := Parse(v)
		if err != nil {
			t.Fatalf("%s: Parse(%q): %v", name, v, err)
		}
		if ep != LocalEndpoint(v) {
			t.Errorf("%s: Parse(%q) = %+v, want the local site at %q", name, v, ep, v)
		}
		if got := ep.String(); got != v {
			t.Errorf("%s: stored form %q, want the bare address %q", name, got, v)
		}
	}

	t.Run("local dials exactly as a net.Dialer", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		local := NewRegistry(testLogger()).Site(LocalEndpoint("127.0.0.1").Site)
		conn, err := local.DialTCP(context.Background(), "127.0.0.1", port, time.Second)
		if err != nil {
			t.Fatalf("local dial to a listener: %v", err)
		}
		conn.Close()

		// A refused port fails with the dialer's own error, byte for byte.
		ln.Close()
		_, siteErr := local.DialTCP(context.Background(), "127.0.0.1", port, time.Second)
		d := net.Dialer{Timeout: time.Second}
		_, plainErr := d.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if siteErr == nil || plainErr == nil || siteErr.Error() != plainErr.Error() {
			t.Fatalf("local dial error %v, want the plain dialer's %v", siteErr, plainErr)
		}
		var ua *UnavailableError
		if errors.As(siteErr, &ua) {
			t.Fatalf("the local site refused a dial (%v); it must always try", siteErr)
		}
		if local.Costs().Latency != 0 || local.State() != StateUp {
			t.Fatalf("local latency=%v state=%v, want 0 and up", local.Costs().Latency, local.State())
		}
	})

	t.Run("non-local form", func(t *testing.T) {
		ep := Endpoint{Site: "r1", IP: "192.168.7.20"}
		form := ep.String()
		back, err := Parse(form)
		if err != nil || back != ep {
			t.Fatalf("Parse(%q) = %+v, %v; want %+v", form, back, err, ep)
		}
		if net.ParseIP(form) != nil {
			t.Fatalf("net.ParseIP(%q) parsed; an older binary would dial it locally", form)
		}
		if validHostname(form) {
			t.Fatalf("%q is a valid hostname; an older binary would look it up", form)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if addrs, err := (&net.Resolver{PreferGo: true}).LookupHost(ctx, form); err == nil {
			t.Fatalf("Go's resolver resolved %q to %v; want a refusal", form, addrs)
		}
	})

	t.Run("malformed forms are errors, never dialled", func(t *testing.T) {
		for _, bad := range []string{"", "..10.0.0.1", "r.1..10.0.0.1", "r1..not-an-ip", "r1..", "r 1..10.0.0.1"} {
			if ep, err := Parse(bad); err == nil {
				t.Errorf("Parse(%q) = %+v, want an error", bad, ep)
			}
		}
	})
}
