package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/macourteau/asiair-commander/internal/web"
)

// logCapacity is how many recent records the in-memory ring keeps for the UI.
const logCapacity = 2000

// appLogFilename is the current (unrotated) app-log file inside <data-dir>/logs.
// lumberjack appends here and renames it aside when rotating, so this path always
// names the live file — the one the UI's log-download endpoint serves.
const appLogFilename = "asiair-commander.log"

// appLogPath is the single source of truth for the current app-log file location,
// shared by setupLogging (which writes it) and the web layer (which serves it).
func appLogPath(dataDir string) string {
	return filepath.Join(dataDir, "logs", appLogFilename)
}

// multiHandler fans one slog record out to several handlers, gated by a shared
// leveler so the rotating file, stdout, and the in-memory ring all honour the
// same threshold. Sub-handlers are invoked directly (their own Enabled is not
// consulted), so the level check must live here.
type multiHandler struct {
	level    slog.Leveler
	handlers []slog.Handler
}

func newMultiHandler(level slog.Leveler, handlers ...slog.Handler) *multiHandler {
	return &multiHandler{level: level, handlers: handlers}
}

func (m *multiHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= m.level.Level()
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		// Clone so a handler that mutates the record (e.g. buffering attrs) does
		// not corrupt it for the next.
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{level: m.level, handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return m
	}
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{level: m.level, handlers: next}
}

// tolerantWriter fans a write out to every underlying writer, ignoring
// individual errors and always reporting success. It exists so a dead stdout
// (Windows service with no console) cannot suppress the rotating file log.
type tolerantWriter []io.Writer

func (t tolerantWriter) Write(p []byte) (int, error) {
	for _, w := range t {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// setupLogging builds the application logger fanning out to a rotating file in
// logs/, stdout, and an in-memory ring surfaced in the UI (design architecture
// §18). It returns the logger, the ring the web layer reads, and a close func
// that flushes/closes the rotating file. Never logs secrets — the callers
// control what they pass.
func setupLogging(dataDir string, debug bool) (*slog.Logger, *web.LogBuffer, func() error, error) {
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return nil, nil, nil, err
	}

	rotator := &lumberjack.Logger{
		Filename:   appLogPath(dataDir),
		MaxSize:    20, // MiB
		MaxBackups: 5,
		MaxAge:     30, // days
		Compress:   true,
	}

	level := new(slog.LevelVar)
	if debug {
		level.Set(slog.LevelDebug)
	} else {
		level.Set(slog.LevelInfo)
	}

	// File + stdout share one text handler. A plain io.MultiWriter stops at the
	// first writer that errors, so under the Windows service manager — where the
	// process has no valid stdout and every os.Stdout write fails — the rotating
	// FILE would never be written, hiding startup failures. tolerantWriter writes
	// to every destination regardless and never reports an error, so the file log
	// survives a dead stdout.
	textOut := tolerantWriter{os.Stdout, rotator}
	textHandler := slog.NewTextHandler(textOut, &slog.HandlerOptions{Level: level})

	appLog := web.NewLogBuffer(logCapacity)
	handler := newMultiHandler(level, textHandler, appLog.Handler())

	logger := slog.New(handler)
	return logger, appLog, rotator.Close, nil
}
