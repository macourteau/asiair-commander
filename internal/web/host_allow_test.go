package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsPrivateHostIP(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// Loopback.
		{"127.0.0.1", true},
		{"127.0.0.1:8686", true},
		{"::1", true},
		{"[::1]:8686", true},
		// RFC1918 private.
		{"10.0.0.4", true},
		{"10.0.0.4:8686", true},
		{"172.16.5.9", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false}, // just outside 172.16/12
		{"172.15.0.1", false}, // just below 172.16/12
		{"192.168.1.20", true},
		{"192.168.1.20:8686", true},
		// CGNAT / shared address space (100.64.0.0/10).
		{"100.64.0.1", true},
		{"100.127.255.254:8686", true},
		{"100.128.0.1", false}, // just outside 100.64/10
		// Link-local.
		{"169.254.10.20", true},
		{"fe80::1", true},
		{"[fe80::1]:8686", true},
		// IPv6 ULA.
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// IPv4-mapped IPv6 of a private address.
		{"::ffff:192.168.1.1", true},
		// Public IP literals — must NOT be auto-permitted.
		{"203.0.113.4", false},
		{"203.0.113.4:8686", false},
		{"8.8.8.8", false},
		{"2606:4700:4700::1111", false},
		// Hostnames — must NOT be auto-permitted.
		{"evil.com", false},
		{"evil.com:8686", false},
		{"localhost", false}, // a name, not an IP literal (allowlist handles it)
		{"", false},
	}
	for _, c := range cases {
		if got := isPrivateHostIP(c.host); got != c.want {
			t.Errorf("isPrivateHostIP(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestCheckHostPrivateIPBypass proves a private-IP Host passes checkHost without
// an allowlist entry, while a public-IP or unknown-hostname Host is rejected with
// 403 and a body that names the host and points at web.allowed_hosts.
func TestCheckHostPrivateIPBypass(t *testing.T) {
	h := newHarness(t, true)

	cases := []struct {
		name    string
		host    string
		allowed bool
	}{
		{"private LAN IP", "192.168.1.50:8686", true},
		{"loopback IP", "127.0.0.1:8686", true},
		{"CGNAT IP", "100.64.0.1", true},
		{"IPv6 ULA", "[fd00::1]:8686", true},
		{"configured hostname localhost", "localhost:8686", true}, // via the allowlist
		{"public IP", "203.0.113.4:8686", false},
		{"unknown hostname", "evil.example", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/config", nil)
			req.Host = c.host
			rec := httptest.NewRecorder()

			ok := h.srv.checkHost(rec, req)
			if ok != c.allowed {
				t.Fatalf("checkHost(%q) = %v, want %v", c.host, ok, c.allowed)
			}
			if c.allowed {
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("host %q: want 403, got %d", c.host, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, c.host) || !strings.Contains(body, "web.allowed_hosts") {
				t.Fatalf("403 body %q should name the host and web.allowed_hosts", body)
			}
		})
	}
}
