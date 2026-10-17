package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

// The SMB login every request in TestRemoteHashContract carries; neither may
// appear in a log line or an error.
const (
	hashUser = "smb-user-7f3a"
	hashPass = "smb-pass-c41d"
)

// remoteWhole and remoteData are well-formed digests a remote reports.
var (
	remoteWhole = strings.Repeat("a1", 32)
	remoteData  = strings.Repeat("d4", 32)
)

// testStall is the short stall window of the test site, so the stall rule
// runs in milliseconds.
const testStall = 300 * time.Millisecond

// hashSite connects a remote whose hello reports the given digest-algorithm
// versions, with a short stall window, and returns the site as the registry
// hands it to a rig: gated.
func hashSite(t *testing.T, whole, data int) (*harness, *fakeRemote, netsite.Site) {
	t.Helper()
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.remote(id).site.stall = testStall
	f.start()
	f.handler().setDigestVersions(whole, data)
	site := h.sites.Site(netsite.ID(id))
	// Up, not Connected: a remote on another digest algorithm reads Needs
	// update while it carries traffic.
	for i := 0; i < 3 && site.State() != netsite.StateUp; i++ {
		h.beat(id)
	}
	if site.State() != netsite.StateUp {
		t.Fatalf("remote %s did not come up: %+v", id, h.status(id))
	}
	return h, f, site
}

// localCounter is a request's local measurement: Commander's own read, which
// the test counts.
type localCounter struct{ calls atomic.Int32 }

func (l *localCounter) measure() (netsite.Digests, error) {
	l.calls.Add(1)
	return netsite.Digests{Whole: "local-whole", Data: "local-data"}, nil
}

func digestRequest(local *localCounter, stop func() bool) netsite.DigestRequest {
	if stop == nil {
		stop = func() bool { return false }
	}
	return netsite.DigestRequest{
		IP: lanDevice, Share: "EMMC Images", Path: "Autorun/Light/Light_0001.fit",
		User: hashUser, Password: hashPass, Stop: stop, Local: local.measure,
	}
}

// noSecrets fails if the harness logs or any of errs carry the SMB login.
func noSecrets(t *testing.T, h *harness, errs ...error) {
	t.Helper()
	texts := []string{h.logs.String()}
	for _, err := range errs {
		if err != nil {
			texts = append(texts, err.Error(), fmt.Sprintf("%+v", err))
		}
	}
	for _, s := range texts {
		if strings.Contains(s, hashUser) || strings.Contains(s, hashPass) {
			t.Fatalf("the SMB login leaked: %q", s)
		}
	}
}

// TestRemoteHashContract (Commander half, the site): a remote site measures
// the pre-delete digests through its remote's hash when both sides run the
// same whole-file algorithm, reporting the identity the remote probed and
// whether the data unit may be recorded; otherwise Commander streams the file
// itself. The stall window, not a whole-call deadline, bounds the call; the
// request's stop cancels it, and the remote's read with it; the SMB login
// reaches the remote and nothing else.
func TestRemoteHashContract(t *testing.T) {
	mtime := time.Date(2026, 10, 1, 22, 0, 0, 0, time.UTC)
	reply := proto.HashReply{GUID: "dev-guid", CPUID: "cpu-1", Whole: remoteWhole, Data: remoteData,
		Size: 42, MtimeNS: mtime.UnixNano()}

	t.Run("matching algorithms: the remote measures in its own session", func(t *testing.T) {
		h, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
		var got proto.HashRequest
		f.handler().setHash(func(_ context.Context, req proto.HashRequest, _ func(int64)) (proto.HashReply, error) {
			got = req
			return reply, nil
		})
		var local localCounter
		res, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
		if err != nil {
			t.Fatalf("MeasureDigests: %v", err)
		}
		want := netsite.DigestResult{
			Digests: netsite.Digests{Whole: remoteWhole, Data: remoteData},
			Stated:  true, Size: 42, Mtime: time.Unix(0, mtime.UnixNano()),
			Probed: true, GUID: "dev-guid", CPUID: "cpu-1", DataRecordable: true,
		}
		if res != want {
			t.Fatalf("result %+v, want %+v", res, want)
		}
		if local.calls.Load() != 0 {
			t.Fatal("Commander streamed the file although the remote measured it")
		}
		if got.IP != lanDevice || got.Share != "EMMC Images" || got.Path != "Autorun/Light/Light_0001.fit" ||
			got.User.Reveal() != hashUser || got.Password.Reveal() != hashPass {
			t.Fatalf("the remote was asked for ip=%q share=%q path=%q with another login", got.IP, got.Share, got.Path)
		}
		noSecrets(t, h)
	})

	t.Run("data-unit algorithm differs: the whole-file digest stands, the data unit is not recordable", func(t *testing.T) {
		h, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion+1)
		f.handler().setHash(func(context.Context, proto.HashRequest, func(int64)) (proto.HashReply, error) {
			return reply, nil
		})
		var local localCounter
		res, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
		if err != nil {
			t.Fatalf("MeasureDigests: %v", err)
		}
		if res.Whole != remoteWhole || res.DataRecordable || !res.Probed || local.calls.Load() != 0 {
			t.Fatalf("result %+v (local reads %d), want the remote's whole digest with the data unit not recordable",
				res, local.calls.Load())
		}
		noSecrets(t, h)
	})

	t.Run("whole-file algorithm differs: Commander streams the file through the tunnel", func(t *testing.T) {
		h, f, site := hashSite(t, header.WholeDigestVersion+1, header.DataDigestVersion)
		var remoteCalls atomic.Int32
		f.handler().setHash(func(context.Context, proto.HashRequest, func(int64)) (proto.HashReply, error) {
			remoteCalls.Add(1)
			return reply, nil
		})
		var local localCounter
		res, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
		if err != nil {
			t.Fatalf("MeasureDigests: %v", err)
		}
		want := netsite.DigestResult{Digests: netsite.Digests{Whole: "local-whole", Data: "local-data"}, DataRecordable: true}
		if res != want || local.calls.Load() != 1 || remoteCalls.Load() != 0 {
			t.Fatalf("result %+v, local reads %d, remote hashes %d; want Commander's own read, once",
				res, local.calls.Load(), remoteCalls.Load())
		}
		noSecrets(t, h)
	})

	t.Run("a remote failure is returned as an error, not a stop", func(t *testing.T) {
		h, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
		f.handler().setHash(func(context.Context, proto.HashRequest, func(int64)) (proto.HashReply, error) {
			return proto.HashReply{}, &proto.Error{Code: proto.CodeFailed, Message: "smb: logon failure"}
		})
		var local localCounter
		_, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
		if err == nil || errors.Is(err, netsite.ErrStopped) || local.calls.Load() != 0 {
			t.Fatalf("err = %v (local reads %d), want the remote's failure", err, local.calls.Load())
		}
		noSecrets(t, h, err)
	})

	t.Run("a malformed digest in a reply is a failed measurement", func(t *testing.T) {
		for name, bad := range map[string]proto.HashReply{
			"empty whole":     {GUID: "dev-guid", Data: remoteData},
			"short whole":     {GUID: "dev-guid", Whole: remoteWhole[:63], Data: remoteData},
			"uppercase whole": {GUID: "dev-guid", Whole: strings.ToUpper(remoteWhole)},
			"non-hex data":    {GUID: "dev-guid", Whole: remoteWhole, Data: strings.Repeat("zz", 32)},
		} {
			t.Run(name, func(t *testing.T) {
				h, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
				f.handler().setHash(func(context.Context, proto.HashRequest, func(int64)) (proto.HashReply, error) {
					return bad, nil
				})
				var local localCounter
				res, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
				if err == nil || errors.Is(err, netsite.ErrStopped) || res != (netsite.DigestResult{}) {
					t.Fatalf("result %+v, err %v; want a failed measurement", res, err)
				}
				if !strings.Contains(h.logs.String(), "malformed digest") {
					t.Fatal("the malformed reply was not logged")
				}
				noSecrets(t, h, err)
			})
		}
	})

	t.Run("no progress within the stall window gives up; progress keeps it waiting", func(t *testing.T) {
		h, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
		f.handler().setHash(func(ctx context.Context, _ proto.HashRequest, progress func(int64)) (proto.HashReply, error) {
			progress(1)
			<-ctx.Done()
			return proto.HashReply{}, ctx.Err()
		})
		var local localCounter
		start := time.Now()
		_, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
		if !errors.Is(err, proto.ErrStalled) || errors.Is(err, netsite.ErrStopped) {
			t.Fatalf("a stalled hash returned %v, want a stall", err)
		}
		if d := time.Since(start); d < testStall || d > 10*testStall {
			t.Fatalf("a stalled hash gave up after %v, want about %v", d, testStall)
		}
		noSecrets(t, h, err)

		f.handler().setHash(func(_ context.Context, _ proto.HashRequest, progress func(int64)) (proto.HashReply, error) {
			for i := int64(1); i <= 20; i++ {
				progress(i << 20)
				time.Sleep(testStall / 6)
			}
			return reply, nil
		})
		start = time.Now()
		res, err := site.MeasureDigests(context.Background(), digestRequest(&local, nil))
		if err != nil {
			t.Fatalf("a progressing hash failed after %v: %v", time.Since(start), err)
		}
		if d := time.Since(start); d < 3*testStall || res.Whole != remoteWhole {
			t.Fatalf("a progressing hash returned %+v after %v, want it to outlast the stall window", res, d)
		}
	})

	stopCases := []struct {
		name string
		// arm returns the context the call runs under, the request's stop, and
		// what fires the stop.
		arm func() (context.Context, func() bool, func())
	}{
		{"a Pause", func() (context.Context, func() bool, func()) {
			var paused atomic.Bool
			return context.Background(), paused.Load, func() { paused.Store(true) }
		}},
		{"ctx ending", func() (context.Context, func() bool, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, func() bool { return ctx.Err() != nil }, cancel
		}},
	}
	for _, c := range stopCases {
		t.Run(c.name+" stops the remote read", func(t *testing.T) {
			h, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
			reading, stopped := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.handler().setHash(func(ctx context.Context, _ proto.HashRequest, progress func(int64)) (proto.HashReply, error) {
				once.Do(func() { close(reading) })
				// Progress keeps the stall rule out of it: only the stop ends the call.
				for i := int64(1); ; i++ {
					progress(i)
					select {
					case <-ctx.Done():
						close(stopped)
						return proto.HashReply{}, ctx.Err()
					case <-time.After(testStall / 6):
					}
				}
			})
			ctx, stop, fire := c.arm()
			var local localCounter
			done := make(chan error, 1)
			go func() {
				_, err := site.MeasureDigests(ctx, digestRequest(&local, stop))
				done <- err
			}()
			<-reading
			fire()
			select {
			case err := <-done:
				if !errors.Is(err, netsite.ErrStopped) {
					t.Fatalf("a stopped measurement returned %v, want ErrStopped", err)
				}
				noSecrets(t, h, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the measurement outlived its stop")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("the remote's read kept running after the stop")
			}
		})
	}

	t.Run("a site that is not up measures nothing", func(t *testing.T) {
		h := newHarness(t)
		id, _ := h.add("Observatory")
		var local localCounter
		_, err := h.sites.Site(netsite.ID(id)).MeasureDigests(context.Background(), digestRequest(&local, nil))
		var ue *netsite.UnavailableError
		if !errors.As(err, &ue) || local.calls.Load() != 0 {
			t.Fatalf("err = %v (local reads %d), want an unavailable site and no read", err, local.calls.Load())
		}
		noSecrets(t, h, err)
	})

	t.Run("a request never formats its login", func(t *testing.T) {
		var local localCounter
		req := digestRequest(&local, nil)
		var buf strings.Builder
		slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "req", req, "user", req.User, "password", req.Password)
		for _, s := range []string{fmt.Sprint(req), fmt.Sprintf("%+v", req), fmt.Sprintf("%#v", req), buf.String()} {
			if strings.Contains(s, hashUser) || strings.Contains(s, hashPass) {
				t.Fatalf("a request formatted its login: %q", s)
			}
		}
	})
}

// TestMeasureDigestsNilStop: a request with no Stop is never stopped, as on
// the local site. A hash that outlives a stop poll, and one that fails, both
// complete without consulting it.
func TestMeasureDigestsNilStop(t *testing.T) {
	reply := proto.HashReply{GUID: "dev-guid", Whole: remoteWhole, Data: remoteData}
	request := func(local *localCounter) netsite.DigestRequest {
		req := digestRequest(local, nil)
		req.Stop = nil
		return req
	}

	t.Run("a hash outliving a stop poll completes", func(t *testing.T) {
		_, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
		f.handler().setHash(func(ctx context.Context, _ proto.HashRequest, progress func(int64)) (proto.HashReply, error) {
			// Progress inside the stall window keeps the call alive across
			// several polls.
			for i := range 4 {
				select {
				case <-ctx.Done():
					return proto.HashReply{}, ctx.Err()
				case <-time.After(stopPoll / 2):
				}
				progress(int64(i + 1))
			}
			return reply, nil
		})
		var local localCounter
		res, err := site.MeasureDigests(context.Background(), request(&local))
		if err != nil || res.Whole != remoteWhole {
			t.Fatalf("result %+v, err %v; want the remote's digests", res, err)
		}
	})

	t.Run("a failed hash is an error, not a stop", func(t *testing.T) {
		_, f, site := hashSite(t, header.WholeDigestVersion, header.DataDigestVersion)
		f.handler().setHash(func(context.Context, proto.HashRequest, func(int64)) (proto.HashReply, error) {
			return proto.HashReply{}, &proto.Error{Code: proto.CodeFailed, Message: "smb: logon failure"}
		})
		var local localCounter
		_, err := site.MeasureDigests(context.Background(), request(&local))
		if err == nil || errors.Is(err, netsite.ErrStopped) {
			t.Fatalf("err = %v, want the remote's failure", err)
		}
	})
}
