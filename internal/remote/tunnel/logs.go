package tunnel

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// redactor replaces every known encoding of a pairing's secrets with a marker.
// It guards tailcat's own log lines and error texts, which this package does
// not control, on top of the wrapper never formatting a secret itself.
type redactor struct {
	r *strings.Replacer
}

const redactedMark = "[redacted]"

// newRedactor builds a redactor for the given secret strings (an address, an
// env value) and secret byte strings (a private key, a pre-shared key), each of
// the latter in every encoding a log line or error is likely to use.
func newRedactor(texts []string, raws [][]byte) *redactor {
	var pairs []string
	add := func(s string) {
		if s != "" {
			pairs = append(pairs, s, redactedMark)
		}
	}
	for _, s := range texts {
		add(s)
	}
	for _, b := range raws {
		if len(b) == 0 {
			continue
		}
		add(hex.EncodeToString(b))
		add(strings.ToUpper(hex.EncodeToString(b)))
		add(base64.StdEncoding.EncodeToString(b))
		add(base64.RawStdEncoding.EncodeToString(b))
		add(base64.URLEncoding.EncodeToString(b))
		add(base64.RawURLEncoding.EncodeToString(b))
	}
	return &redactor{r: strings.NewReplacer(pairs...)}
}

func (r *redactor) redact(s string) string {
	if r == nil {
		return s
	}
	return r.r.Replace(s)
}

// nodePrivateRaw returns a node private key's raw bytes, through its text form
// since the key type does not expose them.
func nodePrivateRaw(k key.NodePrivate) []byte {
	text, err := k.MarshalText()
	if err != nil {
		return nil
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(string(text), "privkey:"))
	if err != nil {
		return nil
	}
	return raw
}

// pskRaw returns a pre-shared key's raw bytes.
func pskRaw(p tailcat.PresharedKey) []byte {
	b, _ := p.MarshalBinary()
	return b
}

// tailcat reports a client it does not admit only as this debug line
// (locoBackend.onMeow in v0.7.0), so a rejected key is recovered from it. A
// behaviour test pins the form, so a tailcat bump that changes it fails the
// gate rather than silently losing the rejection alert.
var rejectedMeowLine = regexp.MustCompile(`ignoring meow from (nodekey:[0-9a-f]{64}): not in allowedClients`)

// magicsock reports its relay connection state only in log lines on v0.7.0
// (the health tracker that records it is unexported). The connected line is
// logged once per (re)connection, the receive-error and closing lines when a
// connection is lost. A behaviour test pins them like the rejected-key line.
var (
	derpConnectedLine = regexp.MustCompile(`derp-(\d+) connected; connGen=`)
	derpLostLine      = regexp.MustCompile(`derp\.Recv\(derp-(\d+)\): |closing connection to derp-(\d+)`)
)

// tailcatLogf adapts tailcat's printf-style logger: each line is redacted and
// emitted at debug level, since tailcat and its engine are chatty, and handed
// to observe (when set) for the state this package recovers from log lines.
func tailcatLogf(log *slog.Logger, role string, red *redactor, observe func(line string)) logger.Logf {
	return func(format string, args ...any) {
		line := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
		if observe != nil {
			observe(line)
		}
		log.Debug("tunnel: tailcat", "role", role, "line", red.redact(line))
	}
}

// parseRegionID returns the first non-empty numeric submatch.
func parseRegionID(m []string) (int, bool) {
	for _, s := range m[1:] {
		if s == "" {
			continue
		}
		id, err := strconv.Atoi(s)
		return id, err == nil
	}
	return 0, false
}
