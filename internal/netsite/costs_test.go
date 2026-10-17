package netsite

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// streamServer accepts TCP connections on 127.0.0.1 and writes zeros to each
// until it is closed, standing in for a device streaming a frame.
func streamServer(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		buf := make([]byte, 64*1024)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// readFor reads n bytes from c and returns how long it took.
func readFor(t *testing.T, c net.Conn, n int64) time.Duration {
	t.Helper()
	start := time.Now()
	got, err := io.CopyN(io.Discard, c, n)
	if err != nil {
		t.Fatalf("read %d bytes: got %d, %v", n, got, err)
	}
	return time.Since(start)
}

// TestSiteRateLimit pins the per-site read cap: with a cap set, reads on a
// connection dialled through the site average no more than the cap beyond one
// bucket of burst, whether the cap was set before or after the dial; a cap of
// 0 is off; and the local site is never throttled, whatever it is asked.
func TestSiteRateLimit(t *testing.T) {
	host, port := streamServer(t)
	reg := NewRegistry(nil)
	fake := NewFake("r1")
	fake.OnDial(func(h string, p int) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort(h, strconv.Itoa(p)))
	})
	if err := reg.Add(fake); err != nil {
		t.Fatal(err)
	}
	dial := func(id ID) net.Conn {
		t.Helper()
		c, err := reg.Site(id).DialTCP(context.Background(), host, port, time.Second)
		if err != nil {
			t.Fatalf("dial via %q: %v", id, err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}

	t.Run("a cap bounds the average read rate", func(t *testing.T) {
		c := dial("r1")
		// Set after the dial: the cap reaches connections already open.
		const capMBps = 2.0
		fake.SetRateLimit(capMBps)
		t.Cleanup(func() { fake.SetRateLimit(0) })
		if got := reg.Site("r1").Costs().RateLimitMBps; got != capMBps {
			t.Fatalf("RateLimitMBps = %v, want %v", got, capMBps)
		}

		const n = 2_500_000
		elapsed := readFor(t, c, n)
		// The bucket starts full, so the first readBurst bytes are free; every
		// byte past it is paced at the cap. 5% tolerance for timer slack.
		floor := time.Duration(float64(n-readBurst) / (capMBps * bytesPerMB) * float64(time.Second))
		if elapsed < floor*95/100 {
			t.Fatalf("read %d bytes in %v at a %v MB/s cap; want at least %v", n, elapsed, capMBps, floor)
		}
	})

	t.Run("a cap of 0 is off", func(t *testing.T) {
		c := dial("r1")
		fake.SetRateLimit(0.1)
		fake.SetRateLimit(0)
		if got := fake.Costs().RateLimitMBps; got != 0 {
			t.Fatalf("RateLimitMBps after 0 = %v, want 0", got)
		}
		// 20 MB at 0.1 MB/s would take minutes.
		if elapsed := readFor(t, c, 20_000_000); elapsed > 10*time.Second {
			t.Fatalf("uncapped read took %v", elapsed)
		}
	})

	t.Run("the local site is never throttled", func(t *testing.T) {
		// The local site has no limiter for anything to set.
		if _, ok := Site(localSite{}).(LimitsReads); ok {
			t.Fatal("the local site has a read limiter")
		}
		if got := reg.Site(Local).Costs().RateLimitMBps; got != 0 {
			t.Fatalf("local RateLimitMBps = %v, want 0 (no cap)", got)
		}
		c := dial(Local)
		if elapsed := readFor(t, c, 20_000_000); elapsed > 10*time.Second {
			t.Fatalf("local read took %v", elapsed)
		}
	})
}

// TestLimitedConnCloseEndsWait pins that closing a connection releases a read
// waiting on the limiter, so a cancelled transfer is not held for the cap.
func TestLimitedConnCloseEndsWait(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := NewReadLimiter()
	l.Set(0.001) // 1 kB/s: a full bucket's overdraft waits minutes
	c := l.Wrap(a)
	go func() {
		buf := make([]byte, 2*readBurst)
		for {
			if _, err := b.Write(buf); err != nil {
				return
			}
		}
	}()
	// Drain the bucket, then the next read owes a long wait.
	if _, err := io.ReadFull(c, make([]byte, readBurst)); err != nil {
		t.Fatalf("first read: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = c.Read(make([]byte, readBurst))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read waiting on the limiter outlived its connection's Close")
	}
}

// TestSMBBudget pins the budget the rig, the supervisor and a remote digest
// share: the local site's is LocalStallWindow exactly; another site's is its
// stall window plus smbRoundTrips margins of its latency.
func TestSMBBudget(t *testing.T) {
	reg := NewRegistry(nil)
	if got := SMBBudget(reg.Site(Local)); got != LocalStallWindow {
		t.Fatalf("local SMBBudget = %v, want %v", got, LocalStallWindow)
	}
	f := NewFake("r1")
	f.SetLatency(150 * time.Millisecond)
	want := RemoteStallWindow + smbRoundTrips*marginPerRoundTrip*150*time.Millisecond
	if got := SMBBudget(f); got != want {
		t.Fatalf("SMBBudget at 150ms = %v, want %v", got, want)
	}
	if got := CappedReadAllowance(f, 1_000_000); got != 0 {
		t.Fatalf("uncapped allowance = %v, want 0", got)
	}
	f.SetRateLimit(0.5)
	if got := CappedReadAllowance(f, 1_000_000); got != 2*time.Second {
		t.Fatalf("1 MB at 0.5 MB/s = %v, want 2s", got)
	}
}

// TestBuiltInSiteCosts pins the costs the registry's own sites report: the
// local site's are the plain-LAN ones and it is no remote; an unknown site's
// are the local site's, except that its replies must match their source.
func TestBuiltInSiteCosts(t *testing.T) {
	reg := NewRegistry(nil)
	if got, want := reg.Site(Local).Costs(), (Costs{StallWindow: LocalStallWindow}); got != want {
		t.Fatalf("local costs %+v, want %+v", got, want)
	}
	if got, want := reg.Site("gone").Costs(), (Costs{StallWindow: LocalStallWindow, RepliesMatchSource: true}); got != want {
		t.Fatalf("unknown site costs %+v, want %+v", got, want)
	}
	f := NewFake("r1")
	if f.Costs().Remote {
		t.Fatal("a Fake is a remote before SetRemote")
	}
	f.SetRemote("Backyard")
	if c := f.Costs(); !c.Remote || c.RemoteName != "Backyard" {
		t.Fatalf("Fake after SetRemote: remote=%v %q", c.Remote, c.RemoteName)
	}
}
