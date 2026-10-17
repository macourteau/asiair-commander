package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// appFunc adapts a function to the App interface.
type appFunc func(context.Context) error

func (f appFunc) Run(ctx context.Context) error { return f(ctx) }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestProgramLifecycle verifies the SCM contract: Start launches Run and
// returns without blocking; Stop cancels the context and waits for Run to return.
func TestProgramLifecycle(t *testing.T) {
	started := make(chan struct{})
	returned := make(chan struct{})
	app := appFunc(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(returned)
		return ctx.Err()
	})

	prg := NewProgram(app, testLogger())

	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start must not block on Run: the goroutine should reach the started signal
	// while we sit here in the test goroutine.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not launch after Start")
	}

	// Run must still be blocking (not yet returned) before Stop.
	select {
	case <-returned:
		t.Fatal("Run returned before Stop was called")
	default:
	}

	if err := prg.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop, Run has returned (Stop waits for it).
	select {
	case <-returned:
	default:
		t.Fatal("Run did not return after Stop")
	}

	if err := prg.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", err)
	}
}

// TestProgramStopGrace verifies Stop gives up after the grace period when Run
// refuses to unwind, rather than hanging forever.
func TestProgramStopGrace(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	app := appFunc(func(ctx context.Context) error {
		<-block // ignore cancellation, only unwind when the test closes block
		return nil
	})

	prg := NewProgram(app, testLogger())
	prg.grace = 50 * time.Millisecond

	if err := prg.Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = prg.Stop(nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within the grace bound")
	}
}

func TestIsAccessDenied(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"other failure", errors.New("some other failure"), false},
		{"windows text", errors.New("Access is denied."), true},
		{"windows text lowercased", errors.New("SCM: access is denied"), true},
		{"wrapped permission sentinel", fmt.Errorf("control: %w", os.ErrPermission), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAccessDenied(tc.err); got != tc.want {
				t.Fatalf("isAccessDenied(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
