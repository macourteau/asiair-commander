package updatecheck

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeGitHub serves the latest-release endpoint from a swappable response, and
// counts the requests it got.
type fakeGitHub struct {
	mu        sync.Mutex
	status    int
	body      string
	requests  atomic.Int64
	userAgent atomic.Value
}

func (f *fakeGitHub) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	f := &fakeGitHub{status: http.StatusOK, body: `{"tag_name":"v1.0.0"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != latestReleasePath {
			http.NotFound(w, r)
			return
		}
		f.requests.Add(1)
		f.userAgent.Store(r.Header.Get("User-Agent"))
		f.mu.Lock()
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func newChecker(version, baseURL string, enabled *atomic.Bool, onChange func()) *Checker {
	return New(Options{
		Version:  version,
		Enabled:  enabled.Load,
		OnChange: onChange,
		Logger:   quietLogger(),
		BaseURL:  baseURL,
	})
}

func on() *atomic.Bool {
	var b atomic.Bool
	b.Store(true)
	return &b
}

func TestReleaseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", true},
		{"1.2.3", "v1.2.3", true},
		{"v10.0.0", "v10.0.0", true},
		{"dev", "", false},
		{"", "", false},
		{"abc1234", "", false},
		{"v1.2.3-4-gabcdef0", "", false}, // git describe between tags
		{"v1.2.3-rc.1", "", false},
		{"v1.2.3+meta", "", false},
		{"v1.2", "", false},
		{"astap-cli-2024.01.01", "", false},
	}
	for _, c := range cases {
		got, ok := releaseVersion(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("releaseVersion(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestCheckReportsOnlyNewer drives checkOnce against each kind of latest tag and
// asserts that only a semver-greater one is reported.
func TestCheckReportsOnlyNewer(t *testing.T) {
	cases := []struct {
		tag  string
		want string
	}{
		{"v1.4.0", "v1.4.0"},
		{"v1.3.10", "v1.3.10"}, // numeric, not lexical
		{"v2.0.0", "v2.0.0"},
		{"v1.3.2", ""}, // same
		{"v1.3.1", ""}, // older (a deleted release falls back to it)
		{"v1.3.9", "v1.3.9"},
		{"v1.4.0-rc.1", ""}, // not a release
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			f, srv := newFakeGitHub(t)
			f.set(http.StatusOK, `{"tag_name":"`+c.tag+`","name":"x","assets":[]}`)
			ch := newChecker("v1.3.2", srv.URL, on(), nil)
			ch.checkOnce(context.Background())
			if got := ch.Latest(); got != c.want {
				t.Errorf("Latest() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRequestCarriesUserAgent(t *testing.T) {
	f, srv := newFakeGitHub(t)
	ch := newChecker("v1.3.2", srv.URL, on(), nil)
	ch.checkOnce(context.Background())
	if got := f.userAgent.Load(); got != "asiair-commander/v1.3.2" {
		t.Errorf("User-Agent = %v, want asiair-commander/v1.3.2", got)
	}
}

// TestFailedCheckKeepsLastValue asserts fail-open: after a good check, every
// kind of failure leaves the reported release unchanged and fires no OnChange.
func TestFailedCheckKeepsLastValue(t *testing.T) {
	f, srv := newFakeGitHub(t)
	var changes atomic.Int64
	ch := newChecker("v1.3.2", srv.URL, on(), func() { changes.Add(1) })

	f.set(http.StatusOK, `{"tag_name":"v1.5.0"}`)
	ch.checkOnce(context.Background())
	if got := ch.Latest(); got != "v1.5.0" {
		t.Fatalf("after a good check Latest() = %q, want v1.5.0", got)
	}
	if changes.Load() != 1 {
		t.Fatalf("OnChange calls = %d, want 1", changes.Load())
	}

	failures := []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
		{"too many requests", http.StatusTooManyRequests, ``},
		{"server error", http.StatusInternalServerError, `oops`},
		{"not found", http.StatusNotFound, `{"message":"Not Found"}`},
		{"malformed json", http.StatusOK, `{"tag_name":`},
		{"not json", http.StatusOK, `<html>`},
		{"no tag", http.StatusOK, `{"name":"v9.9.9"}`},
		{"garbage tag", http.StatusOK, `{"tag_name":"latest"}`},
	}
	for _, fl := range failures {
		f.set(fl.status, fl.body)
		ch.checkOnce(context.Background())
		if got := ch.Latest(); got != "v1.5.0" {
			t.Errorf("%s: Latest() = %q, want the kept v1.5.0", fl.name, got)
		}
	}

	// A network failure: the server is gone.
	srv.Close()
	ch.checkOnce(context.Background())
	if got := ch.Latest(); got != "v1.5.0" {
		t.Errorf("network failure: Latest() = %q, want the kept v1.5.0", got)
	}
	if changes.Load() != 1 {
		t.Errorf("OnChange calls after failures = %d, want still 1", changes.Load())
	}
}

// TestOlderLatestClearsIndicator: a successful answer that is no longer newer
// (the newer release was deleted, so releases/latest fell back) hides it.
func TestOlderLatestClearsIndicator(t *testing.T) {
	f, srv := newFakeGitHub(t)
	var changes atomic.Int64
	ch := newChecker("v1.3.2", srv.URL, on(), func() { changes.Add(1) })
	f.set(http.StatusOK, `{"tag_name":"v1.4.0"}`)
	ch.checkOnce(context.Background())
	f.set(http.StatusOK, `{"tag_name":"v1.3.2"}`)
	ch.checkOnce(context.Background())
	if got := ch.Latest(); got != "" {
		t.Errorf("Latest() = %q, want empty", got)
	}
	if changes.Load() != 2 {
		t.Errorf("OnChange calls = %d, want 2", changes.Load())
	}
}

func TestOversizedBodyFailsOpen(t *testing.T) {
	f, srv := newFakeGitHub(t)
	ch := newChecker("v1.3.2", srv.URL, on(), nil)
	f.set(http.StatusOK, `{"tag_name":"v1.4.0"}`)
	ch.checkOnce(context.Background())
	pad := make([]byte, maxBodyBytes)
	for i := range pad {
		pad[i] = ' '
	}
	f.set(http.StatusOK, `{"body":"`+string(pad)+`","tag_name":"v9.0.0"}`)
	ch.checkOnce(context.Background())
	if got := ch.Latest(); got != "v1.4.0" {
		t.Errorf("Latest() = %q, want the kept v1.4.0", got)
	}
}

// runFor runs the checker's schedule with an instant timer until it has waited
// n times, and returns the delays it asked for.
func runFor(t *testing.T, ch *Checker, n int) []time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var delays []time.Duration
	ch.opts.After = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		delays = append(delays, d)
		if len(delays) >= n {
			cancel()
			return make(chan time.Time) // never fires; ctx ends the loop
		}
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	done := make(chan struct{})
	go func() { ch.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
	mu.Lock()
	defer mu.Unlock()
	return delays
}

// TestScheduleJitterBounds pins the schedule: the first wait is in [1m, 10m],
// every later one in [55m, 65m], including at both extremes of the random source.
func TestScheduleJitterBounds(t *testing.T) {
	check := func(t *testing.T, delays []time.Duration) {
		t.Helper()
		if len(delays) == 0 {
			t.Fatal("no delays recorded")
		}
		if d := delays[0]; d < time.Minute || d > 10*time.Minute {
			t.Errorf("first delay %v outside [1m, 10m]", d)
		}
		for i, d := range delays[1:] {
			if d < 55*time.Minute || d > 65*time.Minute {
				t.Errorf("delay %d = %v outside [55m, 65m]", i+1, d)
			}
		}
	}
	_, srv := newFakeGitHub(t)

	t.Run("random", func(t *testing.T) {
		ch := newChecker("v1.0.0", srv.URL, on(), nil)
		check(t, runFor(t, ch, 200))
	})
	t.Run("low extreme", func(t *testing.T) {
		ch := newChecker("v1.0.0", srv.URL, on(), nil)
		ch.opts.Int64N = func(int64) int64 { return 0 }
		d := runFor(t, ch, 3)
		check(t, d)
		if d[0] != time.Minute || d[1] != 55*time.Minute {
			t.Errorf("low extreme delays = %v, want 1m then 55m", d)
		}
	})
	t.Run("high extreme", func(t *testing.T) {
		ch := newChecker("v1.0.0", srv.URL, on(), nil)
		ch.opts.Int64N = func(n int64) int64 { return n - 1 }
		d := runFor(t, ch, 3)
		check(t, d)
		if d[0] != 10*time.Minute || d[1] != 65*time.Minute {
			t.Errorf("high extreme delays = %v, want 10m then 65m", d)
		}
	})
}

// TestRunChecksOnSchedule: each tick of an enabled checker is one request.
func TestRunChecksOnSchedule(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.set(http.StatusOK, `{"tag_name":"v1.1.0"}`)
	ch := newChecker("v1.0.0", srv.URL, on(), nil)
	runFor(t, ch, 4) // three ticks fire, the fourth wait is cancelled
	if got := f.requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
	if got := ch.Latest(); got != "v1.1.0" {
		t.Errorf("Latest() = %q, want v1.1.0", got)
	}
}

// TestDevBuildNeverChecks: a build that is not a clean release returns from Run
// at once, never schedules, never requests.
func TestDevBuildNeverChecks(t *testing.T) {
	for _, v := range []string{"dev", "", "abc1234", "v1.2.3-4-gabcdef0"} {
		t.Run(v, func(t *testing.T) {
			f, srv := newFakeGitHub(t)
			f.set(http.StatusOK, `{"tag_name":"v99.0.0"}`)
			ch := newChecker(v, srv.URL, on(), nil)
			delays := runFor(t, ch, 3)
			if len(delays) != 0 {
				t.Errorf("a %q build scheduled %d waits, want none", v, len(delays))
			}
			if got := f.requests.Load(); got != 0 {
				t.Errorf("a %q build made %d requests, want none", v, got)
			}
			if got := ch.Latest(); got != "" {
				t.Errorf("Latest() = %q, want empty", got)
			}
		})
	}
}

// TestDisabledMakesNoRequestsAndReportsNothing: switched off, the schedule
// still ticks but nothing is fetched, and a value learned while on is hidden
// until the switch is turned back on.
func TestDisabledMakesNoRequestsAndReportsNothing(t *testing.T) {
	f, srv := newFakeGitHub(t)
	f.set(http.StatusOK, `{"tag_name":"v2.0.0"}`)
	enabled := on()
	ch := newChecker("v1.0.0", srv.URL, enabled, nil)

	ch.checkOnce(context.Background())
	if ch.Latest() != "v2.0.0" {
		t.Fatalf("Latest() = %q before disabling, want v2.0.0", ch.Latest())
	}
	before := f.requests.Load()

	enabled.Store(false)
	if got := ch.Latest(); got != "" {
		t.Errorf("disabled Latest() = %q, want empty", got)
	}
	runFor(t, ch, 5)
	if got := f.requests.Load() - before; got != 0 {
		t.Errorf("disabled checker made %d requests, want none", got)
	}

	enabled.Store(true)
	if got := ch.Latest(); got != "v2.0.0" {
		t.Errorf("re-enabled Latest() = %q, want v2.0.0", got)
	}
}
