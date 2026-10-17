package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

func decodeEnv(t *testing.T, value string) envPayload {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, envPrefix))
	if err != nil {
		t.Fatal(err)
	}
	var p envPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func encodeEnv(t *testing.T, p envPayload) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return envPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// TestPairingRoundTrip: mint → env value → parse keeps both identities; the
// fingerprint is the same from either side, stable, and excludes the PSK; the
// format version is checked.
func TestPairingRoundTrip(t *testing.T) {
	relay := startLocalRelay(t).region
	for _, region := range []Region{testRegion, relay} {
		p, err := Mint(region)
		if err != nil {
			t.Fatalf("Mint(%d): %v", region.ID, err)
		}
		// The local relay is a test relay, which only the test-only parse
		// admits (TestParseEnvRefusesTestRelay).
		parse := ParseEnv
		if region.Code == relay.Code {
			parse = parseEnvForTests
		}
		env, err := parse(p.EnvValue.Reveal())
		if err != nil {
			t.Fatalf("ParseEnv: %v", err)
		}
		// Surrounding whitespace from a pasted .env line is tolerated.
		if _, err := parse("  " + p.EnvValue.Reveal() + "\n"); err != nil {
			t.Fatalf("ParseEnv with whitespace: %v", err)
		}

		// Commander's address is the one the remote serves.
		if string(env.addr) != p.Address.Reveal() {
			t.Fatal("env value's address differs from the stored address")
		}
		ci, err := tailcat.ParseAddr(tailcat.Addr(p.Address.Reveal()))
		if err != nil {
			t.Fatal(err)
		}
		// The fixed region is embedded, so neither side fetches the relay map.
		if len(ci.Region) != 1 || ci.RegionID != 0 || ci.Region[0].Nodes[0].HostName != region.def.Nodes[0].HostName {
			t.Fatalf("address does not embed the fixed region: %d regions, region ID %d", len(ci.Region), ci.RegionID)
		}
		if env.ci.Region[0].RegionCode != region.Code {
			t.Fatalf("env region code %q, want %q", env.ci.Region[0].RegionCode, region.Code)
		}

		// The pin is Commander's client public key.
		fromStore, err := fingerprintForTests(p.Address, p.ClientKey)
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}
		if fromStore != p.Fingerprint || env.Fingerprint() != p.Fingerprint {
			t.Fatalf("fingerprints differ: mint %s, store %s, env %s", p.Fingerprint, fromStore, env.Fingerprint())
		}
		if again, _ := fingerprintForTests(p.Address, p.ClientKey); again != fromStore {
			t.Fatal("fingerprint is not stable")
		}

		// The fingerprint covers the public keys only: another PSK leaves it
		// unchanged.
		ci.PresharedKey = tailcat.NewPresharedKey()
		otherPSK, err := fingerprintForTests(Secret(ci.Addr()), p.ClientKey)
		if err != nil {
			t.Fatal(err)
		}
		if otherPSK != p.Fingerprint {
			t.Fatal("fingerprint depends on the pre-shared key")
		}

		// Two mints never share a fingerprint or a secret.
		p2, err := Mint(region)
		if err != nil {
			t.Fatal(err)
		}
		if p2.Fingerprint == p.Fingerprint || p2.Address == p.Address || p2.ClientKey == p.ClientKey {
			t.Fatal("two mints share an identity")
		}
	}

	p, _ := mintEnv(t, testRegion)
	payload := decodeEnv(t, p.EnvValue.Reveal())
	for name, value := range map[string]string{
		"future version": encodeEnv(t, func() envPayload { q := payload; q.Version = envFormatVersion + 1; return q }()),
		"no version":     encodeEnv(t, func() envPayload { q := payload; q.Version = 0; return q }()),
		"no prefix":      strings.TrimPrefix(p.EnvValue.Reveal(), envPrefix),
		"bare address":   p.Address.Reveal(),
		"bad encoding":   envPrefix + "!!!",
		"bad payload":    envPrefix + base64.RawURLEncoding.EncodeToString([]byte("{")),
		"bad server key": encodeEnv(t, func() envPayload { q := payload; q.Key = "privkey:00"; return q }()),
		"foreign addr": encodeEnv(t, func() envPayload {
			q := payload
			other, _ := Mint(testRegion)
			q.Addr = other.Address.Reveal()
			return q
		}()),
	} {
		_, err := ParseEnv(value)
		if err == nil {
			t.Errorf("ParseEnv(%s) succeeded", name)
			continue
		}
		if name == "future version" && !strings.Contains(err.Error(), fmt.Sprintf("version %d", envFormatVersion+1)) {
			t.Errorf("version error %q does not name the version", err)
		}
	}

	if _, err := Mint(Region{}); err == nil {
		t.Error("Mint without a region succeeded")
	}
	if _, err := Mint(Region{ID: 3, Code: "fra"}); err == nil {
		t.Error("Mint with a bare region ID succeeded; the region must be embedded")
	}
	// An address naming its region only by ID (or "auto") is refused: it
	// would make both sides fetch the relay map.
	byID, err := tailcat.ParseAddr(tailcat.Addr(p.Address.Reveal()))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []tailcfg.DERPRegionID{3, -1} {
		byID.Region, byID.RegionID = nil, id
		if _, err := NewClient(ClientConfig{Address: Secret(byID.Addr()), ClientKey: p.ClientKey}); err == nil {
			t.Errorf("NewClient with region ID %d and no embedded region succeeded", id)
		}
	}
	if _, err := NewClient(ClientConfig{Address: "tcnotanaddress", ClientKey: p.ClientKey}); err == nil {
		t.Error("NewClient with a malformed address succeeded")
	}
	if _, err := NewClient(ClientConfig{Address: p.Address, ClientKey: "privkey:00"}); err == nil {
		t.Error("NewClient with a malformed key succeeded")
	}
}

// TestNearestRegion picks the region a netcheck measures, and fails cleanly
// (within its context) when the relay map cannot be fetched.
func TestNearestRegion(t *testing.T) {
	relay := startLocalRelay(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(relay.dm)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := nearestRegion(ctx, srv.URL)
	if err != nil {
		t.Fatalf("nearestRegion: %v", err)
	}
	if r.ID != 1 || r.Code != "test" {
		t.Fatalf("nearestRegion = %+v, want region 1 (test)", r)
	}
	// The picked region carries its definition, so it can be minted.
	if _, err := Mint(r); err != nil {
		t.Fatalf("Mint(nearest region): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	offline := "http://" + ln.Addr().String() + "/derpmap.json"
	ln.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := nearestRegion(ctx, offline); err == nil {
		t.Fatal("nearestRegion offline succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("nearestRegion offline took %v, beyond its context", d)
	}
}

// TestParseEnvRefusesTestRelay: the production parse refuses a pairing whose
// relay region is marked InsecureForTests, whose TLS certificate tailcat never
// verifies; only the test-only parse admits one.
func TestParseEnvRefusesTestRelay(t *testing.T) {
	if testRelayAllowed {
		t.Skip("a remotetest build admits a test relay by design")
	}
	p, err := Mint(startLocalRelay(t).region)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := ParseEnv(p.EnvValue.Reveal()); err == nil {
		t.Fatal("ParseEnv accepted a test relay region")
	} else if strings.Contains(err.Error(), p.EnvValue.Reveal()) {
		t.Fatal("ParseEnv's error carries the env value")
	}
	if _, err := parseEnvForTests(p.EnvValue.Reveal()); err != nil {
		t.Fatalf("parseEnvForTests: %v", err)
	}

	// A public-style region still parses, and so does the fake's, which other
	// packages mint test pairings on.
	if fp, err := Mint(FakeRegion()); err != nil {
		t.Fatalf("Mint(FakeRegion): %v", err)
	} else if _, err := ParseEnv(fp.EnvValue.Reveal()); err != nil {
		t.Fatalf("ParseEnv(FakeRegion): %v", err)
	}
	q, err := Mint(testRegion)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := ParseEnv(q.EnvValue.Reveal()); err != nil {
		t.Fatalf("ParseEnv(public region): %v", err)
	}
}

// TestClientRefusesTestRelay: Commander's side refuses a stored address whose
// relay region is a test relay, as the remote's ParseEnv does, so a pairing
// minted on one never reaches tailcat in production; the test-only forms admit
// it. A remotetest build admits it in NewClient and Fingerprint by design
// (TestRemoteTestBuildAdmitsTestRelay); AddressRegion refuses it in every build.
func TestClientRefusesTestRelay(t *testing.T) {
	p, err := Mint(startLocalRelay(t).region)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !testRelayAllowed {
		if c, err := NewClient(ClientConfig{Address: p.Address, ClientKey: p.ClientKey}); err == nil {
			c.Close()
			t.Fatal("NewClient accepted a test relay region")
		} else if strings.Contains(err.Error(), p.Address.Reveal()) || strings.Contains(err.Error(), p.ClientKey.Reveal()) {
			t.Fatal("NewClient's error carries a secret")
		}
		if _, err := Fingerprint(p.Address, p.ClientKey); err == nil {
			t.Fatal("Fingerprint accepted a test relay region")
		}
	}
	if _, err := AddressRegion(p.Address, "test", "Test"); err == nil {
		t.Fatal("AddressRegion accepted a test relay region")
	}
	c, err := newClientForTests(ClientConfig{Address: p.Address, ClientKey: p.ClientKey})
	if err != nil {
		t.Fatalf("newClientForTests: %v", err)
	}
	c.Close()
	if _, err := fingerprintForTests(p.Address, p.ClientKey); err != nil {
		t.Fatalf("fingerprintForTests: %v", err)
	}

	// A public-style region is accepted by every production form, and its
	// embedded region mints a fresh pairing on the same relay.
	q, err := Mint(FakeRegion())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if c, err := NewClient(ClientConfig{Address: q.Address, ClientKey: q.ClientKey}); err != nil {
		t.Fatalf("NewClient(FakeRegion): %v", err)
	} else {
		c.Close()
	}
	if fp, err := Fingerprint(q.Address, q.ClientKey); err != nil || fp != q.Fingerprint {
		t.Fatalf("Fingerprint(FakeRegion) = %q, %v; want %q", fp, err, q.Fingerprint)
	}
	r, err := AddressRegion(q.Address, "fake", "Fake")
	if err != nil {
		t.Fatalf("AddressRegion: %v", err)
	}
	if r.ID != 1 || r.Code != "fake" || r.Name != "Fake" {
		t.Fatalf("AddressRegion = %+v", r)
	}
	again, err := Mint(r)
	if err != nil {
		t.Fatalf("Mint on the address's region: %v", err)
	}
	if again.Address == q.Address {
		t.Fatal("re-minting on the same region reused the address")
	}
	env, err := ParseEnv(again.EnvValue.Reveal())
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	if got := env.ci.Region[0].RegionCode; got != "fake" {
		t.Fatalf("re-minted env region code %q, want fake", got)
	}
}

// TestEnvFileFormat pins the remote's .env line: the variable name is part of
// the compose file's forward-compatible contract and never changes, and the
// line carries the env value verbatim.
func TestEnvFileFormat(t *testing.T) {
	if EnvVar != "ASIAIR_REMOTE_PAIRING" {
		t.Fatalf("EnvVar = %q; the name is a fixed contract with every deployed compose file", EnvVar)
	}
	if got, want := EnvFile("acremote:VALUE").Reveal(), "ASIAIR_REMOTE_PAIRING=acremote:VALUE\n"; got != want {
		t.Fatalf("EnvFile = %q, want %q", got, want)
	}
}

// TestParseEnvReadsEveryOlderVersion: an update never breaks an existing .env,
// so every recorded env value (testdata/env, one per format version from 1 up
// to the current one) parses to its recorded fingerprint and region. The values
// were minted by the build that introduced each format, so a later format that
// renames, retypes or requires a field fails here.
func TestParseEnvReadsEveryOlderVersion(t *testing.T) {
	for v := 1; v <= envFormatVersion; v++ {
		name := fmt.Sprintf("v%d.env", v)
		t.Run(name, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join("testdata", "env", name))
			if err != nil {
				t.Fatalf("format version %d has no recorded env value: %v", v, err)
			}
			var value, wantFP string
			for line := range strings.Lines(string(text)) {
				line = strings.TrimSpace(line)
				if fp, ok := strings.CutPrefix(line, "# fingerprint: "); ok {
					wantFP = fp
				}
				if val, ok := strings.CutPrefix(line, EnvVar+"="); ok {
					value = val
				}
			}
			if value == "" || wantFP == "" {
				t.Fatalf("%s lacks the %s line or the fingerprint comment", name, EnvVar)
			}
			if got := decodeEnv(t, value).Version; got != v {
				t.Fatalf("%s carries format version %d", name, got)
			}
			// The recorded values sit on a test relay, which only the
			// test-only parse admits.
			env, err := parseEnvForTests(value)
			if err != nil {
				t.Fatalf("ParseEnv: %v", err)
			}
			if env.Fingerprint() != wantFP {
				t.Fatalf("fingerprint %s, want %s", env.Fingerprint(), wantFP)
			}
			if got := env.ci.Region[0].RegionCode; got != "golden" {
				t.Fatalf("region code %q, want golden", got)
			}
		})
	}
}
