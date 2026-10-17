package web

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestLogBufferStringifiesErrorAttr guards the UI log stream against empty {}
// error objects: an error attr (e.g. *os.PathError, whose inner err is
// unexported) marshals to {} if stored raw, so the buffer stores its message
// instead. The stored value must be the error string and survive JSON delivery.
func TestLogBufferStringifiesErrorAttr(t *testing.T) {
	buf := NewLogBuffer(10)
	logger := slog.New(buf.Handler())

	err := &os.PathError{Op: "readdir", Path: "Autorun", Err: fs.ErrNotExist}
	logger.Error("rig: list failed", "error", err)

	entries := buf.Entries(slog.LevelDebug, 0)
	if len(entries) != 1 {
		t.Fatalf("want 1 buffered entry, got %d", len(entries))
	}
	got, ok := entries[0].Attrs["error"]
	if !ok {
		t.Fatalf("error attr missing; attrs=%v", entries[0].Attrs)
	}
	s, ok := got.(string)
	if !ok {
		t.Fatalf("error attr stored as %T, want string", got)
	}
	if want := err.Error(); s != want {
		t.Fatalf("error attr = %q, want %q", s, want)
	}

	// The whole point: it must not serialise to an empty object.
	raw, jerr := json.Marshal(entries[0])
	if jerr != nil {
		t.Fatalf("marshal entry: %v", jerr)
	}
	if !strings.Contains(string(raw), "file does not exist") {
		t.Fatalf("serialised entry lost the error message: %s", raw)
	}
	if strings.Contains(string(raw), `"error":{}`) {
		t.Fatalf("error attr serialised to an empty object: %s", raw)
	}
}
