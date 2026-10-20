//go:build !demo

package main

import (
	"context"
	"flag"
	"log/slog"
	"sync/atomic"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/remote/supervisor"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// installDemo is a no-op outside a `-tags demo` build: the real device, resolver,
// and header are used unchanged. The demo variant lives in demo.go.
func installDemo(
	_ context.Context,
	newDevice deviceFactory,
	resolver rig.Resolver,
	hdr header.Reader,
	_ *atomic.Pointer[model.Config],
	_ localfs.LocalFS,
	_ *slog.Logger,
) (deviceFactory, rig.Resolver, header.Reader) {
	return newDevice, resolver, hdr
}

// installDemoRemotes is a no-op outside a `-tags demo` build: every remote is
// dialled through its real tunnel client. The demo variant lives in
// demo_remote.go.
func installDemoRemotes(_ context.Context, _ string, opts supervisor.Options, _ *slog.Logger) supervisor.Options {
	return opts
}

// demoReleasesURL is "" outside a `-tags demo` build, so the update checker
// always asks the real GitHub API. The demo variant lives in
// demo_updatecheck.go.
func demoReleasesURL() string { return "" }

// demoFlags is empty outside a `-tags demo` build: the production binary has no
// demo flag and no lifetime cap. The demo variant lives in demo_lifetime.go.
type demoFlags struct{}

func registerDemoFlags(*flag.FlagSet) *demoFlags { return &demoFlags{} }

// limitLifetime returns ctx unchanged outside a `-tags demo` build.
func (*demoFlags) limitLifetime(ctx context.Context, _ *slog.Logger) (context.Context, context.CancelFunc) {
	return ctx, func() {}
}
