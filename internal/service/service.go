// Package service is the thin kardianos/service wrapper that drives the
// application's lifecycle under a service manager (the Windows SCM on the NAS,
// systemd/launchd in dev). It knows nothing about the app it runs: the
// composition root hands it an App, and this package only starts, stops, and
// installs/uninstalls the service around it (design architecture §2, §19).
//
// The one non-obvious invariant lives in Program.Start: it launches the app in
// a goroutine and returns immediately. kardianos reports Running to the service
// manager only after Start returns, so blocking there is exactly what makes the
// Windows SCM declare a start-hang and time out. Stop cancels the app's context
// and waits (bounded) for it to unwind, because the service manager does not
// deliver a signal the app could observe on its own.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"
)

// stopGrace bounds how long Stop waits for the app to unwind after cancellation
// before it gives up and lets the process exit anyway. The SCM's own stop
// timeout is longer, so this only guards against a wedged shutdown.
const stopGrace = 30 * time.Second

// App is the lifecycle the service drives. Run must block until ctx is
// cancelled and return nil (or context.Canceled) on a clean shutdown; any other
// error is logged and surfaced as a failed service start.
type App interface {
	Run(ctx context.Context) error
}

// Program adapts an App to kardianos/service.Interface.
type Program struct {
	app    App
	logger *slog.Logger
	grace  time.Duration

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// NewProgram builds a Program around app. A nil logger falls back to the slog
// default.
func NewProgram(app App, logger *slog.Logger) *Program {
	if logger == nil {
		logger = slog.Default()
	}
	return &Program{app: app, logger: logger, grace: stopGrace}
}

// Start launches the app's Run in a goroutine and returns immediately so the
// service manager sees the service reach Running. It never blocks.
func (p *Program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		p.err = p.app.Run(ctx)
		if p.err != nil && !errors.Is(p.err, context.Canceled) {
			p.logger.Error("service: app run returned error", "err", p.err)
		}
	}()
	return nil
}

// Stop cancels the app's context and waits, bounded by the stop grace, for Run
// to return. A wedged shutdown logs and is abandoned rather than hanging the SCM.
func (p *Program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(p.grace):
			p.logger.Error("service: app did not stop within grace; abandoning wait", "grace", p.grace)
		}
	}
	return nil
}

// Err reports the error the app's Run returned, if it has returned. It is read
// after svc.Run() unwinds on the managed (non-interactive) path.
func (p *Program) Err() error { return p.err }

// Config builds the kardianos service definition. args are the arguments the
// service manager relaunches the binary with (the composition root passes
// ["run"] so the installed service re-enters the run verb).
//
// UserName pins the Windows logon account to the least-privilege virtual service
// account NT SERVICE\<name> (empty off Windows). kardianos hands this straight to
// the SCM as ServiceStartName with an empty password, which the Windows API reads
// as "auto-provision the per-service virtual account" — so the install verb alone
// registers a hardened service, no LocalSystem.
func Config(name, displayName, description string, args []string) *service.Config {
	return &service.Config{
		Name:        name,
		DisplayName: displayName,
		Description: description,
		UserName:    serviceUserName(name),
		Arguments:   args,
	}
}

// Run drives app under the service manager. Interactively (an operator running
// the binary in a terminal) it runs app directly under a SIGINT/SIGTERM-cancelled
// context, so a startup failure exits immediately with its error. Under the
// service manager it dispatches through kardianos so the manager observes
// Running (via Program.Start) and can stop cleanly (via Program.Stop).
func Run(name, displayName, description string, app App, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	prg := NewProgram(app, logger)
	svc, err := service.New(prg, Config(name, displayName, description, []string{"run"}))
	if err != nil {
		return fmt.Errorf("service: build: %w", err)
	}

	if service.Interactive() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		logger.Info("service: running interactively", "name", name)
		return app.Run(ctx)
	}

	if err := svc.Run(); err != nil {
		return fmt.Errorf("service: run: %w", err)
	}
	return prg.Err()
}

// Control performs install/uninstall/start/stop/restart against the service
// manager, wrapping an access-denied refusal with an actionable elevation hint.
func Control(name, displayName, description, action string) error {
	svc, err := service.New(NewProgram(noopApp{}, nil), Config(name, displayName, description, []string{"run"}))
	if err != nil {
		return fmt.Errorf("service: build: %w", err)
	}
	if err := service.Control(svc, action); err != nil {
		return elevationHint(action, err)
	}
	return nil
}

// noopApp is the App the control path installs behind: control operations never
// invoke Start/Stop, but service.New still requires an Interface.
type noopApp struct{}

func (noopApp) Run(ctx context.Context) error { return nil }

// elevationHint annotates the service manager's access-denied refusal with the
// next step; every other control error passes through unchanged.
func elevationHint(action string, err error) error {
	if isAccessDenied(err) {
		return fmt.Errorf(`%s requires an elevated (Administrator) prompt — right-click Command Prompt or PowerShell, choose "Run as administrator", then re-run: %w`, action, err)
	}
	return err
}

// isAccessDenied reports whether err is a permission/access-denied refusal from
// the service manager. It matches both the POSIX permission sentinel and the
// Windows SCM's "Access is denied" text so no OS-specific build tag is needed.
func isAccessDenied(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "access is denied")
}
