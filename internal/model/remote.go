package model

// RemoteState is a remote's connection state as its card shows it.
type RemoteState string

const (
	// RemoteWaiting: no successful hello since the remote was added or
	// re-paired.
	RemoteWaiting RemoteState = "waiting"
	// RemoteConnected: the latest hello succeeded and the site is up.
	RemoteConnected RemoteState = "connected"
	// RemoteReconnecting: the remote connected under this pairing before, and
	// its site is not up, for less than the unreachable threshold.
	RemoteReconnecting RemoteState = "reconnecting"
	// RemoteUnreachable: as Reconnecting, past the unreachable threshold.
	RemoteUnreachable RemoteState = "unreachable"
	// RemoteNeedsUpdate: connected, but a digest algorithm differs between the
	// remote and Commander; NeedsUpdate names the side to update.
	RemoteNeedsUpdate RemoteState = "needs_update"
)

// Which side of a pairing runs the older digest algorithm.
const (
	UpdateRemote    = "remote"
	UpdateCommander = "commander"
)

// RemoteStatus is one remote's published state. It never carries the
// remote's address, a key, or an endpoint's stored form. Times are on
// Commander's clock: the remote's own clock is untrusted, so what it reports
// as an age is turned into a time here, when its hello arrives.
type RemoteStatus struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	State RemoteState `json:"state"`
	// Direct is the path the remote last reported: true direct, false relayed.
	Direct bool `json:"direct"`
	// RelayRegion is the relay region the remote last reported reaching
	// Commander through; Region is the one fixed at pairing.
	RelayRegion string `json:"relay_region,omitempty"`
	Region      string `json:"region"`
	// LatencyMS is the smoothed round trip, without the variation margin the
	// site's timeouts add; 0 before any hello.
	LatencyMS int64 `json:"latency_ms"`
	// Version and BuildDate identify the remote's binary, from its last hello.
	Version   string `json:"version,omitempty"`
	BuildDate string `json:"build_date,omitempty"`
	// UptimeS is the remote process's uptime as of its last hello (seconds).
	UptimeS int64 `json:"uptime_s"`
	// Restarts counts the remote restarts seen: a new instance id.
	Restarts int `json:"restarts"`
	// Fingerprint identifies the pairing by its two public keys.
	Fingerprint string `json:"fingerprint"`
	// LastHelloAt is the last successful hello under any pairing (unix
	// seconds), 0 if none; the card shows its age.
	LastHelloAt int64 `json:"last_hello_at,omitempty"`
	// EverConnected is whether a hello ever succeeded, under any pairing.
	EverConnected bool `json:"ever_connected"`
	// Warnings is the remote's recent warnings, oldest first.
	Warnings []RemoteWarning `json:"warnings,omitempty"`
	// NeedsUpdate is UpdateRemote or UpdateCommander while a digest algorithm
	// differs, "" otherwise.
	NeedsUpdate string `json:"needs_update,omitempty"`
	// BuildStale is set while the remote's build trails Commander's by more
	// than about two weeks.
	BuildStale bool `json:"build_stale,omitempty"`
	// PairingShared is set once an instance id seen before came back without a
	// restart: two remotes are using this pairing.
	PairingShared bool `json:"pairing_shared,omitempty"`
	// RateLimitMBps is the bandwidth cap, 0 when off.
	RateLimitMBps float64 `json:"rate_limit_mbps,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	// PairedAt is when the current pairing was minted (unix seconds): the add
	// or the last re-pair. The card times its "still waiting" hint from it.
	PairedAt int64 `json:"paired_at"`
}

// RemoteWarning is one of a remote's recent warnings.
type RemoteWarning struct {
	// At is when the warning was raised, on Commander's clock (unix seconds),
	// derived from the age the remote reported.
	At   int64  `json:"at"`
	Kind string `json:"kind"`
	// Proto and Target name a refused flow; ClientKey a rejected client's
	// public key.
	Proto     string `json:"proto,omitempty"`
	Target    string `json:"target,omitempty"`
	ClientKey string `json:"client_key,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// RemoteWarningRefusedForward is the Kind of a refused-forward warning
// (proto.WarnRefusedForward), whose Target is the refused flow's ip:port.
const RemoteWarningRefusedForward = "refused_forward"

// RemoteRegion is a relay region a remote can be paired on.
type RemoteRegion struct {
	ID   int    `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	// Nearest marks the region a netcheck measured as nearest, the default.
	Nearest bool `json:"nearest,omitempty"`
}

// RemotePairing is what adding or re-pairing a remote returns, once. EnvValue
// is the secret the user puts in the remote's .env; nothing returns it again.
// EnvFile is that .env, ready to save, so no caller formats the line itself.
//
// It is never marshalled directly, so it carries no JSON tags: the web layer
// builds its own response from it, sent with no-store caching headers, which a
// plain JSON write of this type would skip.
type RemotePairing struct {
	Remote   RemoteStatus
	EnvValue string
	EnvFile  string
}

// RelayState is Commander's own reachability of the relay regions its remotes
// rendezvous through.
type RelayState string

const (
	// RelayUp: at least one remote's tunnel client holds its connection to
	// its relay region.
	RelayUp RelayState = "up"
	// RelayDown: no tunnel client has held a relay connection for longer than
	// a fresh client needs to make one, so Commander's own internet, or its
	// path to the relays, is down; a remote that is down then says nothing
	// about the remote.
	RelayDown RelayState = "down"
	// RelayUnknown: no client has had the time to connect yet.
	RelayUnknown RelayState = "unknown"
)

// RelayStatus is Commander's own reachability of the relay regions, published
// apart from any remote's state.
type RelayStatus struct {
	State RelayState `json:"state"`
	// Since is when State last changed (unix seconds).
	Since int64 `json:"since"`
}
