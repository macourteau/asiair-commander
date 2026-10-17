package role

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

// Main runs the `remote` verb and returns the process exit code. With the
// argument "healthcheck" it asks a running remote whether it is healthy
// instead. It logs to stdout only; ASIAIR_DEBUG enables debug logging.
func Main(args []string, build proto.Build) int {
	if len(args) > 0 {
		switch args[0] {
		case "healthcheck":
			return Healthcheck("")
		default:
			fmt.Fprintf(os.Stderr, "unknown remote argument %q; usage: remote [healthcheck]\n", args[0])
			return 2
		}
	}

	level := slog.LevelInfo
	if os.Getenv("ASIAIR_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	r, err := New(Config{Build: build, Logger: log})
	if err != nil {
		log.Error("remote: refusing to start", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := r.Run(ctx); err != nil {
		if !errors.Is(err, ErrRelayUnreachable) {
			log.Error("remote: stopped", "error", err)
		}
		return 1
	}
	return 0
}
