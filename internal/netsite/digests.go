package netsite

import (
	"errors"
	"log/slog"
	"time"
)

// Credential is an SMB user name or password carried in a DigestRequest. It
// formats as redacted, so a request logged or wrapped into an error by mistake
// never shows it.
//
// proto.HashRequest carries it on the remote protocol's wire as a plain JSON
// string, which is a frozen wire contract: it must never gain a MarshalJSON or
// MarshalText that redacts or reshapes it. TestNetsiteWireForm pins it.
type Credential string

// String implements fmt.Stringer and redacts the credential.
func (Credential) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer and redacts the credential.
func (Credential) GoString() string { return "[redacted]" }

// LogValue implements slog.LogValuer and redacts the credential.
func (Credential) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Reveal returns the credential's value, for the SMB login.
func (c Credential) Reveal() string { return string(c) }

// DigestRequest names one device file whose pre-delete digests a site
// measures. It carries no expected digest or identity: a site only measures,
// and the caller compares.
type DigestRequest struct {
	IP, Share, Path string
	User, Password  Credential
	// Stop reports that the measurement must be abandoned (a Pause, or the
	// cycle ending). A site that measures elsewhere polls it and cancels. It
	// is optional: nil means the measurement is never stopped.
	Stop func() bool
	// Local measures the file in Commander's own SMB session: the streaming
	// read a site falls back to when it cannot measure the file itself.
	Local func() (Digests, error)
}

// String implements fmt.Stringer. A request carries credentials, so none of
// its fields is ever formatted.
func (DigestRequest) String() string { return "netsite.DigestRequest{[redacted]}" }

// GoString implements fmt.GoStringer like String.
func (r DigestRequest) GoString() string { return r.String() }

// LogValue implements slog.LogValuer like String.
func (r DigestRequest) LogValue() slog.Value { return slog.StringValue(r.String()) }

// Digests are the whole-file and data-unit digests of one fresh full read.
type Digests struct {
	Whole, Data string
}

// DigestResult is what a site measured for a DigestRequest.
type DigestResult struct {
	Digests
	// Stated reports that the measuring side re-stated the file before its
	// read; Size and Mtime are that stat, the mtime in the device's clock.
	Stated bool
	Size   int64
	Mtime  time.Time
	// Probed reports that the measuring side took its own identity probe in
	// the session it read through; GUID and CPUID are the device's answer, both
	// empty when it did not answer. A measurement in Commander's session is not
	// probed: that session's identity is confirmed before any delete.
	Probed      bool
	GUID, CPUID string
	// DataRecordable reports that the data-unit digest was computed by this
	// build's algorithm, so it may be recorded as a frame's exposure identity.
	DataRecordable bool
}

// ErrStopped is a measurement abandoned because its request's Stop fired.
var ErrStopped = errors.New("netsite: digest measurement stopped")

// MeasureLocally runs req's local measurement and returns its digests
// unchanged: unprobed, since Commander's own session confirmed identity, and
// recordable, since this build computed them.
func MeasureLocally(req DigestRequest) (DigestResult, error) {
	d, err := req.Local()
	if err != nil {
		return DigestResult{}, err
	}
	return DigestResult{Digests: d, DataRecordable: true}, nil
}
