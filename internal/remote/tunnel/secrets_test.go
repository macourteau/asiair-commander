package tunnel

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTunnelLogsNoSecrets captures every log line (the wrapper's, tailcat's
// through the wrapper, and anything on the standard logger) and every error
// text across mint, start, dial, failure and rejection, and checks that none
// carries the address, a private key, the PSK or the env value.
func TestTunnelLogsNoSecrets(t *testing.T) {
	sink := &syncBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(sink)
	defer log.SetOutput(prevOut)
	defer log.SetFlags(prevFlags)
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var texts []string
	note := func(err error) {
		if err != nil {
			texts = append(texts, err.Error())
		}
	}

	h := newRealHarness(t, logger)
	p := h.pairing
	texts = append(texts, fmt.Sprintf("%v %+v %#v %s %q", p, p, p, p.Address, p.ClientKey))
	texts = append(texts, fmt.Sprintf("%v %+v %#v %s", h.env, *h.env, h.env, h.env))

	// Dials that succeed, a refused one, a dead target, a stalled read.
	if c, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanDevice, portSMB); err != nil {
		t.Fatalf("DialTCP: %v", err)
	} else {
		roundTrip(t, c, "x", "x")
		c.Close()
	}
	_, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanNotAllowed, portSMB)
	note(err)
	_, err = h.client.DialTCP(dialCtx(t, 20*time.Second), lanDead, portSMB)
	note(err)
	_, err = h.client.DialTCP(dialCtx(t, 20*time.Second), lanDevice, 22)
	note(err)

	// A rejected client key.
	other := key.NewNode()
	otherText, _ := other.MarshalText()
	intruder, err := newClientForTests(ClientConfig{Address: p.Address, ClientKey: Secret(otherText), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	_, err = intruder.DialProtocol(dialCtx(t, 3*time.Second))
	note(err)
	intruder.Close()
	select {
	case <-h.rejected:
	case <-time.After(10 * time.Second):
		t.Fatal("no rejection event")
	}

	// The server stops: dials fail.
	note(h.server.Close())
	_, err = h.client.DialTCP(dialCtx(t, 2*time.Second), lanDevice, portSMB)
	note(err)
	_, err = h.client.DialProtocol(dialCtx(t, 2*time.Second))
	note(err)
	note(h.client.Close())
	_, err = h.client.DialUDP(dialCtx(t, 2*time.Second), lanDevice, portScan)
	note(err)

	// A client that never starts: its relay is dead.
	deadP, _ := mintEnv(t, deadRegion(t))
	dead, err := newClientForTests(ClientConfig{Address: deadP.Address, ClientKey: deadP.ClientKey, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dead.DialTCP(dialCtx(t, 2*time.Second), lanDevice, portSMB)
	note(err)
	dead.Close()

	// Parse failures of mangled secrets.
	payload := decodeEnv(t, p.EnvValue.Reveal())
	mangled := payload
	mangled.Addr = payload.Addr[:len(payload.Addr)-3]
	_, err = ParseEnv(encodeEnv(t, mangled))
	note(err)
	_, err = ParseEnv(p.EnvValue.Reveal()[:len(p.EnvValue.Reveal())-5])
	note(err)
	// The production parse refuses the test relay this harness runs through.
	if !testRelayAllowed {
		_, err = ParseEnv(p.EnvValue.Reveal())
		note(err)
	}
	_, err = NewClient(ClientConfig{Address: p.Address[:len(p.Address)-4], ClientKey: p.ClientKey})
	note(err)
	_, err = fingerprintForTests(p.Address, p.ClientKey[:20])
	note(err)

	logs := sink.String()
	// The capture must have seen tailcat's own lines, or it proves nothing.
	for _, want := range []string{"ignoring meow from", "connected; connGen=", "tunnel: server started", "tunnel: refused forward"} {
		if !strings.Contains(logs, want) {
			t.Errorf("captured logs lack %q; the capture is not seeing tailcat", want)
		}
	}
	if len(texts) < 10 {
		t.Fatalf("only %d error texts captured", len(texts))
	}

	all := logs + "\n" + strings.Join(texts, "\n")
	for name, secret := range secretForms(t, p, payload, otherText, deadP) {
		if strings.Contains(all, secret) {
			t.Errorf("%s appears in a log line or an error text", name)
		}
	}
}

// secretForms lists every pairing secret in every encoding worth checking.
func secretForms(t *testing.T, p *Pairing, payload envPayload, otherClientKey []byte, deadP *Pairing) map[string]string {
	t.Helper()
	forms := map[string]string{
		"address":               p.Address.Reveal(),
		"client key":            p.ClientKey.Reveal(),
		"env value":             p.EnvValue.Reveal(),
		"server key":            payload.Key,
		"intruder key":          string(otherClientKey),
		"dead-relay address":    deadP.Address.Reveal(),
		"dead-relay client key": deadP.ClientKey.Reveal(),
	}
	var serverKey, clientKey key.NodePrivate
	if err := serverKey.UnmarshalText([]byte(payload.Key)); err != nil {
		t.Fatal(err)
	}
	if err := clientKey.UnmarshalText([]byte(p.ClientKey)); err != nil {
		t.Fatal(err)
	}
	env, err := parseEnvForTests(p.EnvValue.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	raws := map[string][]byte{
		"server key": nodePrivateRaw(serverKey),
		"client key": nodePrivateRaw(clientKey),
		"PSK":        pskRaw(env.ci.PresharedKey),
	}
	for name, raw := range raws {
		if len(raw) != 32 {
			t.Fatalf("%s raw form is %d bytes", name, len(raw))
		}
		forms[name+" hex"] = hex.EncodeToString(raw)
		forms[name+" base64"] = base64.RawStdEncoding.EncodeToString(raw)
		forms[name+" base64url"] = base64.RawURLEncoding.EncodeToString(raw)
	}
	forms["PSK text"] = "psk:" + hex.EncodeToString(raws["PSK"])
	return forms
}

// TestRedactorCoversEncodings checks the redactor on tailcat-style lines.
func TestRedactorCoversEncodings(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab, 0x01}, 16)
	r := newRedactor([]string{"tcSECRETADDR"}, [][]byte{raw})
	for _, line := range []string{
		"addr tcSECRETADDR here",
		"psk:" + hex.EncodeToString(raw),
		"key " + strings.ToUpper(hex.EncodeToString(raw)),
		"b64 " + base64.StdEncoding.EncodeToString(raw),
		"b64url " + base64.RawURLEncoding.EncodeToString(raw),
	} {
		out := r.redact(line)
		if !strings.Contains(out, redactedMark) || strings.Contains(out, "tcSECRETADDR") || strings.Contains(strings.ToLower(out), hex.EncodeToString(raw)) {
			t.Errorf("redact(%q) = %q", line, out)
		}
	}
}
