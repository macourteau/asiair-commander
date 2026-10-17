// Package tunnel wraps tailcat, the userspace WireGuard tunnel that joins
// Commander to a remote site. It is the only package in the repository that
// imports github.com/tailscale/tailcat or tailscale.com, so a tailcat API break
// lands here and nowhere else.
//
// It provides the pairing mint (a remote's server identity plus Commander's
// client key for it), the server role the remote runs (pinned to one client
// key, forwarding only what the forwarding policy allows), the client role
// Commander runs, and an in-memory fake with the same interfaces for hermetic
// tests.
//
// Pairing secrets (the address, both private keys, the pre-shared key and the
// env value) never appear in a log line or in the Error() text of an error
// returned from this package.
package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// Secret is a pairing secret: the tailcat address, a private key or the env
// value. Its String and GoString redact it, so formatting a struct that holds
// one cannot leak it into a log line; Reveal returns the value for storage.
type Secret string

// String implements fmt.Stringer and redacts the secret.
func (Secret) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer and redacts the secret.
func (Secret) GoString() string { return "[redacted]" }

// Reveal returns the secret's value, for storing it or showing it once.
func (s Secret) Reveal() string { return string(s) }

// Region is the DERP relay region a pairing rendezvouses through. It is fixed
// at pairing because a region re-picked at every start would change the
// address, and its definition is embedded in the address so neither side
// fetches the relay map or runs a netcheck to use it. Regions come from
// NearestRegion or Regions.
type Region struct {
	// ID is the region's ID in the relay map.
	ID int
	// Code and Name describe the region for display.
	Code string
	Name string

	def *tailcfg.DERPRegion
}

// NearestRegion runs a netcheck over the public DERP map and returns the region
// with the lowest latency. ctx bounds the whole measurement; it fails cleanly
// when the map cannot be fetched or no region answers, as when offline.
func NearestRegion(ctx context.Context) (Region, error) {
	return nearestRegion(ctx, tailcat.DefaultDERPMapURL)
}

// Regions lists the public DERP map's regions, for choosing one other than the
// nearest.
func Regions(ctx context.Context) ([]Region, error) {
	dm, err := tailcat.FetchDERPMap(ctx, tailcat.DERPMapURL(tailcat.DefaultDERPMapURL))
	if err != nil {
		return nil, fmt.Errorf("tunnel: fetch relay map: %w", err)
	}
	out := make([]Region, 0, len(dm.Regions))
	for _, id := range slices.Sorted(maps.Keys(dm.Regions)) {
		out = append(out, regionOf(dm.Regions[id]))
	}
	return out, nil
}

func nearestRegion(ctx context.Context, mapURL string) (Region, error) {
	dm, err := tailcat.FetchDERPMap(ctx, tailcat.DERPMapURL(mapURL))
	if err != nil {
		return Region{}, fmt.Errorf("tunnel: fetch relay map: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Region{}, fmt.Errorf("tunnel: pick relay region: %w", err)
	}
	id, err := tailcat.PickBestRegion(ctx, dm)
	if err != nil {
		return Region{}, fmt.Errorf("tunnel: pick relay region: %w", err)
	}
	r, ok := dm.Regions[id]
	if id == 0 || !ok {
		return Region{}, errors.New("tunnel: pick relay region: no relay region answered")
	}
	return regionOf(r), nil
}

// regionOf copies a relay map region, keeping at most two relay nodes (as
// tailcat's own Addr.Resolve does) so the address stays short.
func regionOf(r *tailcfg.DERPRegion) Region {
	def := r.Clone()
	def.Nodes = def.Nodes[:min(2, len(def.Nodes))]
	return Region{ID: int(r.RegionID), Code: r.RegionCode, Name: r.RegionName, def: def}
}

// Pairing is what minting a remote produces. Commander keeps Address and
// ClientKey; EnvValue is shown to the user once and then discarded.
type Pairing struct {
	// Address is the remote's tailcat address. It carries the pre-shared key,
	// so it is a secret bearer capability.
	Address Secret
	// ClientKey is Commander's private key for this remote.
	ClientKey Secret
	// EnvValue is the one opaque string the user puts in the remote's .env:
	// the remote's server identity plus Commander's client public key.
	EnvValue Secret
	// Fingerprint identifies the pairing by its two public keys only.
	Fingerprint string
}

// envFormatVersion is the env value's format. A later field (such as a
// self-hosted relay override) bumps it without changing the identities, so an
// existing pairing keeps working without a Re-pair.
//
// Every version from 1 up to this one stays readable: a remote's .env is never
// rewritten by an update, so a build that refused an older version would break
// every pairing minted before it. A bump only adds optional fields to
// envPayload; a field an older version lacks reads as its zero value.
const envFormatVersion = 1

// EnvVar names the environment variable holding the pairing's env value on the
// remote PC. The compose file passes it from the remote's .env; the name is part
// of that file's forward-compatible contract and never changes.
const EnvVar = "ASIAIR_REMOTE_PAIRING"

// EnvFile is the remote's .env text carrying an env value, ready to save. It is
// as secret as the env value it holds.
func EnvFile(value Secret) Secret {
	return Secret(EnvVar + "=" + value.Reveal() + "\n")
}

// envPrefix marks an env value so a pasted address or key is rejected with a
// clear error rather than misparsed.
const envPrefix = "acremote:"

// envPayload is the env value's wire form, base64url-encoded JSON behind
// envPrefix.
type envPayload struct {
	Version int    `json:"v"`
	Key     string `json:"k"` // the remote's node private key, "privkey:…"
	Addr    string `json:"a"` // the remote's tailcat address (public keys, PSK, region)
	Client  string `json:"c"` // Commander's client public key, "nodekey:…"
	// RegionCode and RegionName restore the region's display names, which
	// tailcat strips from an address, so the remote reports its relay as
	// "fra" rather than "1".
	RegionCode string `json:"rc,omitempty"`
	RegionName string `json:"rn,omitempty"`
}

// Mint generates a remote's server identity (node key, pre-shared key and the
// fixed region) and Commander's client key for that remote.
func Mint(region Region) (*Pairing, error) {
	if err := CheckDebugAddr(nil); err != nil {
		return nil, err
	}
	if region.def == nil || len(region.def.Nodes) == 0 {
		return nil, fmt.Errorf("tunnel: mint: relay region %d has no definition; pick it from NearestRegion or Regions", region.ID)
	}
	server := tailcat.NewPrivateKey()
	server.Public.Region = []*tailcfg.DERPRegion{region.def}
	addr := server.Public.Addr()
	client := key.NewNode()

	serverKeyText, err := server.Private.MarshalText()
	if err != nil {
		return nil, errors.New("tunnel: mint: encode server key")
	}
	clientKeyText, err := client.MarshalText()
	if err != nil {
		return nil, errors.New("tunnel: mint: encode client key")
	}
	clientPubText, err := client.Public().MarshalText()
	if err != nil {
		return nil, errors.New("tunnel: mint: encode client public key")
	}
	payload, err := json.Marshal(envPayload{
		Version:    envFormatVersion,
		Key:        string(serverKeyText),
		Addr:       string(addr),
		Client:     string(clientPubText),
		RegionCode: region.def.RegionCode,
		RegionName: region.def.RegionName,
	})
	if err != nil {
		return nil, errors.New("tunnel: mint: encode env value")
	}
	return &Pairing{
		Address:     Secret(addr),
		ClientKey:   Secret(clientKeyText),
		EnvValue:    Secret(envPrefix + base64.RawURLEncoding.EncodeToString(payload)),
		Fingerprint: fingerprint(server.Private.Public(), client.Public()),
	}, nil
}

// Env is a parsed env value: what the remote role needs to start its server.
type Env struct {
	value     string
	serverKey key.NodePrivate
	addr      tailcat.Addr
	ci        tailcat.ConnInfo
	clientPub key.NodePublic
}

// String implements fmt.Stringer without exposing any secret.
func (e Env) String() string { return "tunnel.Env{" + e.Fingerprint() + "}" }

// GoString implements fmt.GoStringer without exposing any secret.
func (e Env) GoString() string { return e.String() }

// Fingerprint identifies the pairing by its two public keys only.
func (e Env) Fingerprint() string {
	return fingerprint(e.serverKey.Public(), e.clientPub)
}

// ParseEnv parses an env value. It fails when the value is not one this build
// can read, including when Commander's client key (the pin) is missing or
// malformed, so a remote never starts without its pin, and when its relay
// region is a test relay, whose TLS certificate is never verified (except in a
// build made with the remotetest tag; see testRelayAllowed). No error
// text carries any part of the value.
func ParseEnv(value string) (*Env, error) {
	return parseEnv(value, testRelayAllowed)
}

// parseEnv is ParseEnv; allowTestRelay admits a region marked
// InsecureForTests, which only this package's tests, running a local relay,
// ever need.
func parseEnv(value string, allowTestRelay bool) (*Env, error) {
	value = strings.TrimSpace(value)
	rest, ok := strings.CutPrefix(value, envPrefix)
	if !ok {
		return nil, errors.New("tunnel: env value is not a remote pairing")
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return nil, errors.New("tunnel: env value is malformed (encoding)")
	}
	var p envPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("tunnel: env value is malformed (payload)")
	}
	// Older versions stay readable (envFormatVersion); only a newer one, from
	// a newer Commander, is refused.
	if p.Version < 1 || p.Version > envFormatVersion {
		return nil, fmt.Errorf("tunnel: env value has format version %d, this build reads versions 1 to %d; update the remote or make a new pairing", p.Version, envFormatVersion)
	}
	e := &Env{value: value, addr: tailcat.Addr(p.Addr)}
	if err := e.serverKey.UnmarshalText([]byte(p.Key)); err != nil || e.serverKey.IsZero() {
		return nil, errors.New("tunnel: env value has a malformed server key")
	}
	if p.Client == "" {
		return nil, errors.New("tunnel: env value has no pinned client key")
	}
	if err := e.clientPub.UnmarshalText([]byte(p.Client)); err != nil || e.clientPub.IsZero() {
		return nil, errors.New("tunnel: env value has a malformed pinned client key")
	}
	ci, err := parseAddr(e.addr)
	if err != nil {
		return nil, err
	}
	// The address must describe this very server, or clients holding it would
	// never reach the identity started from the key.
	if ci.ServerPublic.NodePublic != e.serverKey.Public() || ci.ServerDiscoPublic != tailcat.DiscoPublicForNode(e.serverKey) {
		return nil, errors.New("tunnel: env value's address does not match its server key")
	}
	if !allowTestRelay && isTestRelay(ci) {
		return nil, errors.New("tunnel: env value's relay region is a test relay; refusing to start")
	}
	r := ci.Region[0]
	if p.RegionCode != "" {
		r.RegionCode = p.RegionCode
	}
	if p.RegionName != "" {
		r.RegionName = p.RegionName
	}
	e.ci = ci
	return e, nil
}

// Fingerprint computes a pairing's fingerprint from what Commander stores: the
// address and its client key. Like ParseEnv, it refuses an address whose relay
// region is a test relay, except in a build made with the remotetest tag.
func Fingerprint(address, clientKey Secret) (string, error) {
	return fingerprintOf(address, clientKey, testRelayAllowed)
}

// fingerprintOf is Fingerprint; allowTestRelay admits a test relay, as
// parseEnv's does.
func fingerprintOf(address, clientKey Secret, allowTestRelay bool) (string, error) {
	ci, err := parseAddr(tailcat.Addr(address))
	if err != nil {
		return "", err
	}
	if !allowTestRelay && isTestRelay(ci) {
		return "", errTestRelay
	}
	var k key.NodePrivate
	if err := k.UnmarshalText([]byte(clientKey)); err != nil || k.IsZero() {
		return "", errors.New("tunnel: malformed client key")
	}
	return fingerprint(ci.ServerPublic.NodePublic, k.Public()), nil
}

// AddressRegion returns the relay region embedded in a stored address, so a
// Re-pair can mint fresh keys on the region the remote already uses. code and
// name restore the region's display names, which the address does not carry.
// Like NewClient, it refuses a test relay.
func AddressRegion(address Secret, code, name string) (Region, error) {
	ci, err := parseAddr(tailcat.Addr(address))
	if err != nil {
		return Region{}, err
	}
	if isTestRelay(ci) {
		return Region{}, errTestRelay
	}
	def := ci.Region[0].Clone()
	def.RegionCode, def.RegionName = code, name
	return Region{ID: int(def.RegionID), Code: code, Name: name, def: def}, nil
}

// errTestRelay refuses a stored address whose relay region is a test relay.
var errTestRelay = errors.New("tunnel: remote address's relay region is a test relay; refusing to use it")

// isTestRelay reports whether an address's relay region is a test relay, one
// marked InsecureForTests, whose TLS certificate tailcat never verifies. Only
// this package's tests, running a local relay, ever mint one.
func isTestRelay(ci tailcat.ConnInfo) bool {
	for _, r := range ci.Region {
		for _, n := range r.Nodes {
			if n.InsecureForTests {
				return true
			}
		}
	}
	return false
}

// parseAddr parses and validates a stored address. tailcat's own parse errors
// are not passed through, since a pre-shared key parse error may quote it.
func parseAddr(addr tailcat.Addr) (tailcat.ConnInfo, error) {
	ci, err := tailcat.ParseAddr(addr)
	if err != nil {
		return tailcat.ConnInfo{}, errors.New("tunnel: malformed remote address")
	}
	switch {
	case ci.ServerPublic.IsZero():
		return tailcat.ConnInfo{}, errors.New("tunnel: remote address has no server key")
	case ci.ServerDiscoPublic.IsZero():
		return tailcat.ConnInfo{}, errors.New("tunnel: remote address has no path-discovery key")
	case ci.PresharedKey.IsZero():
		return tailcat.ConnInfo{}, errors.New("tunnel: remote address has no pre-shared key")
	case len(ci.Region) != 1:
		// A pairing embeds exactly one fixed region. A region ID alone would
		// make both sides fetch the relay map, and an "auto" one (-1) is
		// re-picked by netcheck at every start and changes the address.
		return tailcat.ConnInfo{}, errors.New("tunnel: remote address does not embed one fixed relay region")
	}
	return ci, nil
}

// fingerprint hashes the server's and the client's public keys. It never covers
// the pre-shared key, so it is safe to display and to log.
func fingerprint(server, client key.NodePublic) string {
	h := sha256.New()
	s := server.Raw32()
	c := client.Raw32()
	h.Write(s[:])
	h.Write(c[:])
	sum := hex.EncodeToString(h.Sum(nil)[:8])
	return sum[0:4] + "-" + sum[4:8] + "-" + sum[8:12] + "-" + sum[12:16]
}
