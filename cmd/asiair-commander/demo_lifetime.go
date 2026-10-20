//go:build demo

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"strings"
	"time"
)

// defaultDemoMaxLifetime bounds a demo server that nobody stopped. A demo run
// exists for a UI or screenshot pass, which takes minutes; one left behind keeps
// its generator and in-memory devices running, so it shuts itself down well
// before it can matter. A pass that really needs longer passes the flag.
const defaultDemoMaxLifetime = 16 * time.Hour

// demoFlags holds the demo build's own command-line flags.
type demoFlags struct {
	maxLifetime time.Duration
}

// registerDemoFlags adds -demo-max-lifetime to fs. Only a `-tags demo` build
// has it; see demo_off.go.
func registerDemoFlags(fs *flag.FlagSet) *demoFlags {
	d := &demoFlags{maxLifetime: defaultDemoMaxLifetime}
	fs.Func("demo-max-lifetime",
		"shut the demo server down cleanly after this long (default 16h; 0 = unlimited)",
		func(v string) error {
			dur, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			if dur < 0 {
				return errors.New("must not be negative (0 = unlimited)")
			}
			d.maxLifetime = dur
			return nil
		})
	return d
}

// limitLifetime returns a context cancelled once the maximum lifetime has
// elapsed, so the app unwinds through the same graceful shutdown a SIGTERM
// drives. The returned cancel stops the timer.
func (d *demoFlags) limitLifetime(ctx context.Context, logger *slog.Logger) (context.Context, context.CancelFunc) {
	if d.maxLifetime == 0 {
		logger.Warn("DEMO: no lifetime cap (-demo-max-lifetime=0); this server runs until it is stopped")
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	limit := shortDuration(d.maxLifetime)
	deadline := time.Now().Add(d.maxLifetime)
	logger.Info("DEMO: lifetime capped; the server shuts itself down at the deadline",
		"max_lifetime", limit, "deadline", deadline.Format(time.RFC3339))
	t := time.AfterFunc(d.maxLifetime, func() {
		logger.Warn("demo lifetime of " + limit + " reached; shutting down; restart it if still needed")
		cancel()
	})
	return ctx, func() {
		t.Stop()
		cancel()
	}
}

// shortDuration renders d without zero trailing units: 16h, not 16h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
