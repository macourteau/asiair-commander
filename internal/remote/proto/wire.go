// Package proto is the remote protocol: the three calls Commander makes to a
// remote over the tunnel (hello, broadcast and hash), shared by both roles. The
// remote role serves it (Serve); Commander's supervisor calls it (Client).
//
// The wire format is newline-delimited JSON over one tunnel connection. A
// client writes one request frame and the server answers with zero or more
// progress frames and then exactly one result or error frame, all echoing the
// request's call id; a connection may carry several calls in turn. Every frame
// names the protocol version it is written in. Decoders ignore unknown fields,
// so a later version adds fields without breaking an older peer; a field is
// never removed or retyped. The recorded transcripts under testdata hold every
// version to that (TestRemoteProtocolTranscripts).
//
// Versioning: both sides speak every version since the first, and a side uses a
// newer feature only once the peer's hello reports a version that carries it.
//
// The remote never compares: hash returns what it measured and Commander
// decides (design remote-sites §12 invariant 5). Its timings are ages and
// uptime, never timestamps of its own wall clock (design remote-sites §12
// invariant 11).
package proto

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// V1 is the first protocol version.
const V1 = 1

// Versions lists every protocol version this build speaks, oldest first.
var Versions = []int{V1}

// Op names a call.
type Op string

// The three calls.
const (
	OpHello     Op = "hello"
	OpBroadcast Op = "broadcast"
	OpHash      Op = "hash"
)

// request is a client's frame.
type request struct {
	V      int          `json:"v"`
	CallID string       `json:"call_id"`
	Op     Op           `json:"op"`
	Hash   *HashRequest `json:"hash,omitempty"`
}

// Reply frame kinds.
const (
	kindResult   = "result"
	kindProgress = "progress"
	kindError    = "error"
)

// reply is a server's frame. Exactly one payload is set, matching Kind and, for
// a result, the request's op.
type reply struct {
	V         int             `json:"v"`
	CallID    string          `json:"call_id"`
	Kind      string          `json:"kind"`
	Hello     *HelloReply     `json:"hello,omitempty"`
	Broadcast *BroadcastReply `json:"broadcast,omitempty"`
	Hash      *HashReply      `json:"hash,omitempty"`
	Progress  *Progress       `json:"progress,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}

// HelloReply is what a remote says about itself.
type HelloReply struct {
	// Versions lists the protocol versions the remote speaks.
	Versions []int `json:"versions"`
	// WholeDigestVersion and DataDigestVersion are the remote's digest
	// algorithms (header.WholeDigestVersion, header.DataDigestVersion).
	WholeDigestVersion int `json:"whole_digest_version"`
	DataDigestVersion  int `json:"data_digest_version"`
	// Build identifies the remote's binary.
	Build Build `json:"build"`
	// UptimeMS is how long the remote process has run.
	UptimeMS int64 `json:"uptime_ms"`
	// InstanceID is random per remote process: two different ids under one
	// pairing without a restart between them mean two remotes share it.
	InstanceID string `json:"instance_id"`
	// Path is how the remote reaches Commander, from its tunnel status.
	Path Path `json:"path"`
	// Warnings is the remote's recent warnings, oldest first.
	Warnings []Warning `json:"warnings"`
}

// Uptime is UptimeMS as a duration.
func (h HelloReply) Uptime() time.Duration { return time.Duration(h.UptimeMS) * time.Millisecond }

// Build identifies a binary.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// Date is the build time the binary was stamped with at build (RFC 3339
	// when built by the Makefile). It is a property of the binary, not a
	// reading of the remote's clock.
	Date string `json:"date"`
}

// Path is the tunnel path between the remote and Commander, as the remote's
// server reports it.
type Path struct {
	// Connected is false when the server lists no peer, which only happens
	// between a reconnect and its first handshake.
	Connected bool `json:"connected"`
	// Direct is true on a direct UDP path, false when relayed.
	Direct bool `json:"direct"`
	// RelayRegion is the relay region the peer is reached through.
	RelayRegion string `json:"relay_region,omitempty"`
}

// WarningKind classifies a remote warning.
type WarningKind string

// The warnings a remote keeps.
const (
	// WarnBroadcastFailed: a broadcast round could not run on the remote's LAN.
	WarnBroadcastFailed WarningKind = "broadcast_failed"
	// WarnRefusedForward: the remote refused to forward a flow; Proto, Target
	// and Reason say which and why.
	WarnRefusedForward WarningKind = "refused_forward"
	// WarnRejectedClient: a client key other than the pinned one tried to
	// connect; ClientKey names it.
	WarnRejectedClient WarningKind = "rejected_client"
)

// Warning is one entry of a remote's recent warnings.
type Warning struct {
	// Seq increases by one per warning within a remote process, so Commander
	// can tell the ones it has not seen.
	Seq uint64 `json:"seq"`
	// AgeMS is how long ago the warning was raised, when hello answered.
	AgeMS int64       `json:"age_ms"`
	Kind  WarningKind `json:"kind"`
	// CallID is the call that produced the warning, if one did.
	CallID string `json:"call_id,omitempty"`
	// Proto and Target are a refused flow's transport and ip:port.
	Proto  string `json:"proto,omitempty"`
	Target string `json:"target,omitempty"`
	// ClientKey is a rejected client's public key.
	ClientKey string `json:"client_key,omitempty"`
	// Reason says why, in words.
	Reason string `json:"reason,omitempty"`
}

// Age is AgeMS as a duration.
func (w Warning) Age() time.Duration { return time.Duration(w.AgeMS) * time.Millisecond }

// BroadcastReply is one broadcast round's raw replies.
type BroadcastReply struct {
	Replies []Datagram `json:"replies"`
}

// Datagram is one raw scan_air reply and its UDP source address, as a site's
// broadcast round returns it. It is netsite's type, so a reply crosses from
// the remote's LAN to Commander's discovery without a conversion; its JSON
// tags are the wire form.
type Datagram = netsite.Datagram

// HashRequest names one file for the remote to measure. It carries no expected
// digest or identity: the remote measures and never compares.
type HashRequest struct {
	IP    string `json:"ip"`
	Share string `json:"share"`
	Path  string `json:"path"`
	// User and Password are netsite.Credential, which formats as redacted, so
	// a struct holding one cannot leak it into a log line; each is a plain
	// string on the wire, where the tunnel encrypts it.
	User     netsite.Credential `json:"user"`
	Password netsite.Credential `json:"password"`
}

// String implements fmt.Stringer. No argument of a hash call is ever logged.
func (HashRequest) String() string { return "proto.HashRequest{[redacted]}" }

// GoString implements fmt.GoStringer like String.
func (r HashRequest) GoString() string { return r.String() }

// LogValue implements slog.LogValuer like String.
func (r HashRequest) LogValue() slog.Value { return slog.StringValue(r.String()) }

// HashReply is what the remote measured, in its own SMB session.
type HashReply struct {
	// GUID and CPUID are what the device answered to the unicast scan_air
	// probe sent inside the session; both are empty when it did not answer.
	GUID  string `json:"guid"`
	CPUID string `json:"cpu_id"`
	// Whole and Data are the hex whole-file and data-unit digests of one fresh
	// read (header.DigestTee); Data is empty when the header did not parse.
	Whole string `json:"whole"`
	Data  string `json:"data"`
	// Size and MtimeNS are the file's re-stat before the read; the mtime is in
	// the device's clock domain, as SMB reports it, in Unix nanoseconds.
	Size    int64 `json:"size"`
	MtimeNS int64 `json:"mtime_ns"`
}

// Identified reports whether the probe inside the session was answered.
func (h HashReply) Identified() bool { return h.GUID != "" }

// Mtime is MtimeNS as a time.
func (h HashReply) Mtime() time.Time { return time.Unix(0, h.MtimeNS) }

// Progress is how many bytes a hash has read so far.
type Progress struct {
	Bytes int64 `json:"bytes"`
}

// ErrorCode classifies a failed call.
type ErrorCode string

// The error codes. A client treats a code it does not know as CodeFailed.
const (
	// CodeBadRequest: the request was malformed.
	CodeBadRequest ErrorCode = "bad_request"
	// CodeUnsupported: the op or version is one the remote does not speak.
	CodeUnsupported ErrorCode = "unsupported"
	// CodeRefused: the remote's policy refused the call (hash to an address
	// off its forwarding allowlist).
	CodeRefused ErrorCode = "refused"
	// CodeNotFound: the share or file does not exist on the device.
	CodeNotFound ErrorCode = "not_found"
	// CodeNetwork: the remote could not reach the device.
	CodeNetwork ErrorCode = "network"
	// CodeFailed: anything else.
	CodeFailed ErrorCode = "failed"
)

// Error is a call's failure as the remote reports it. A handler returns one to
// choose the code; the client returns one for every error frame.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("remote: %s: %s", e.Code, e.Message)
}

// CodeOf returns the remote error code err carries, or "" when err is not a
// remote's error reply (a transport failure, a stall, a cancellation).
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Frame size limits. A request is a few hundred bytes. A broadcast reply holds
// at most 256 datagrams of at most 4 KiB, base64-encoded.
const (
	maxRequestFrame = 64 << 10
	maxReplyFrame   = 8 << 20
)

// errFrameTooLarge is a framing violation; the connection is dropped.
var errFrameTooLarge = errors.New("remote protocol: frame too large")

// readFrame reads one newline-terminated frame of at most max bytes.
func readFrame(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, errFrameTooLarge
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// validCallID reports whether id can be a call id: 1–64 characters of
// letters, digits, '-', '_', '.' and ':'. The remote echoes it into its log
// lines, so nothing else is accepted.
func validCallID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// speaks reports whether v is a version this build speaks.
func speaks(v int) bool {
	for _, have := range Versions {
		if have == v {
			return true
		}
	}
	return false
}
