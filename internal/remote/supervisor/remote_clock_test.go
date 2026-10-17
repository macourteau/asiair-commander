package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/role"
)

// remoteLAN is the remote role's own network in TestRemoteClockIsNotTrusted:
// one local site whose broadcast round fails, so the role records a warning.
type remoteLAN struct{ site *netsite.Fake }

func (l remoteLAN) Site(netsite.ID) netsite.Site { return l.site }
func (l remoteLAN) All() []netsite.Site          { return []netsite.Site{l.site} }

// TestRemoteClockIsNotTrusted is design remote-sites §12 invariant 11's
// Commander half: a real remote role whose wall clock is decades wrong is
// supervised exactly as one whose clock is right. The last hello is the time on
// Commander's clock it was answered, and each warning's time is the age the
// remote reported subtracted from Commander's clock on arrival; no time
// Commander publishes about the remote is a reading of the remote's clock.
func TestRemoteClockIsNotTrusted(t *testing.T) {
	h := newHarness(t)
	p, err := h.sup.Add(context.Background(), "Observatory", 0)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	id := p.Remote.ID
	f := h.install(p)

	// The remote's PC thinks it is 1987.
	remoteClock := &clock{t: time.Date(1987, 3, 1, 4, 0, 0, 0, time.UTC)}
	lan := remoteLAN{site: netsite.NewFake(netsite.Local)}
	lan.site.SetBroadcast(nil, errors.New("open broadcast socket: network is unreachable"))
	r, err := role.New(role.Config{
		Build:  proto.Build{Version: "v1.0.0", Date: "2026-10-01T00:00:00Z"},
		Getenv: func(k string) string { return map[string]string{role.EnvVar: p.EnvValue}[k] },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    remoteClock.now,
		Sites:  lan,
	})
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	f.serve = r

	// The first hello brings the site up through a broadcast round, which
	// fails on the remote's LAN and leaves it a warning.
	h.connect(id, f)
	st := h.wantState(id, model.RemoteConnected)
	if st.LastHelloAt != h.clock.now().Unix() {
		t.Fatalf("last hello %d, want Commander's clock %d", st.LastHelloAt, h.clock.now().Unix())
	}

	// The two clocks move by different amounts: the remote's says the warning
	// is ten minutes old, while only three pass on Commander's.
	remoteClock.advance(10 * time.Minute)
	h.clock.advance(3 * time.Minute)
	h.beat(id)
	st = h.wantState(id, model.RemoteConnected)
	now := h.clock.now()
	if st.LastHelloAt != now.Unix() {
		t.Fatalf("last hello %d, want Commander's clock %d", st.LastHelloAt, now.Unix())
	}
	if len(st.Warnings) != 1 || st.Warnings[0].Kind != string(proto.WarnBroadcastFailed) {
		t.Fatalf("warnings %+v, want the remote's broadcast failure", st.Warnings)
	}
	if got, want := st.Warnings[0].At, now.Add(-10*time.Minute).Unix(); got != want {
		t.Fatalf("warning at %d, want Commander's clock less the reported age, %d", got, want)
	}

	// No time published about the remote falls in its clock's year.
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{st.LastHelloAt, st.Warnings[0].At} {
		if year := time.Unix(v, 0).UTC().Year(); year != now.Year() {
			t.Fatalf("a published time falls in %d: %s", year, b)
		}
	}
}
