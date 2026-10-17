package web

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/metrics"
)

// LogBuffer is a fixed-capacity, in-memory ring of the application's own slog
// records (design architecture §18). It is the backing store for the Logs
// surface's app-log view (GET /api/app-logs): a headless service's log is
// otherwise only reachable by RDP-ing into the NAS, so the most recent lines
// are kept queryable in process.
//
// LogBuffer is the store; Handler returns an slog.Handler that writes into it.
// Wire it at the composition root — typically fanned out alongside the rotating
// file handler — so every log line the app emits is also visible in the UI.
type LogBuffer struct {
	mu    sync.Mutex
	buf   []LogEntry
	cap   int
	start int // index of the oldest entry
	n     int // number of live entries (<= cap)

	// Live subscribers for the SSE log stream. Sends are non-blocking (a full
	// channel drops the entry) so logging can never stall on a slow client.
	subs   map[int64]chan LogEntry
	nextID int64

	// metrics counts dropped entries. Held in an atomic.Pointer because the
	// composition root wires it (SetMetrics) while startup logs are already flowing
	// through append on other goroutines. nil-safe.
	metrics atomic.Pointer[metrics.Metrics]
}

// SetMetrics wires the metrics surface for the drop counter. Safe to call while
// logging is in flight.
func (b *LogBuffer) SetMetrics(m *metrics.Metrics) { b.metrics.Store(m) }

// LogEntry is one captured record, shaped for JSON delivery to the UI.
type LogEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// NewLogBuffer returns a ring holding the most recent capacity records. A
// capacity <= 0 falls back to a sane default so a mis-wired caller still works.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &LogBuffer{buf: make([]LogEntry, capacity), cap: capacity}
}

// Handler returns an slog.Handler that records into this buffer. Each derived
// handler (WithAttrs/WithGroup) shares the same ring but carries its own
// accumulated attributes, exactly as slog expects.
func (b *LogBuffer) Handler() slog.Handler { return &logHandler{store: b} }

// append pushes one entry, overwriting the oldest when full, and fans it out to
// any live SSE subscribers (non-blocking, so a slow client never stalls logging).
func (b *LogBuffer) append(e LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n < b.cap {
		b.buf[(b.start+b.n)%b.cap] = e
		b.n++
	} else {
		// Full: overwrite the oldest and advance the window.
		b.buf[b.start] = e
		b.start = (b.start + 1) % b.cap
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Slow subscriber; drop this entry rather than block logging. NO log line
			// here: append holds b.mu and runs from the slog handler, so a log call
			// would re-enter append → b.mu and deadlock every logging goroutine (P1).
			// The counter is safe because it takes no lock.
			b.metrics.Load().RecordLogEntryDropped()
		}
	}
}

// Subscribe registers a live stream of newly-appended entries. The returned
// cancel unsubscribes and closes the channel; it is idempotent-safe to defer.
func (b *LogBuffer) Subscribe() (<-chan LogEntry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = make(map[int64]chan LogEntry)
	}
	b.nextID++
	id := b.nextID
	ch := make(chan LogEntry, 512)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// Entries returns the buffered records newest-first, keeping only those at or
// above minLevel and at most limit of them (limit <= 0 means no cap).
func (b *LogBuffer) Entries(minLevel slog.Level, limit int) []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]LogEntry, 0, b.n)
	// Walk newest-first.
	for i := b.n - 1; i >= 0; i-- {
		e := b.buf[(b.start+i)%b.cap]
		if levelValue(e.Level) < minLevel {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// levelValue maps a stored level string back to an slog.Level for filtering.
func levelValue(s string) slog.Level {
	switch s {
	case slog.LevelDebug.String():
		return slog.LevelDebug
	case slog.LevelInfo.String():
		return slog.LevelInfo
	case slog.LevelWarn.String():
		return slog.LevelWarn
	case slog.LevelError.String():
		return slog.LevelError
	default:
		var l slog.Level
		if err := l.UnmarshalText([]byte(s)); err == nil {
			return l
		}
		return slog.LevelInfo
	}
}

// logHandler is an slog.Handler view over a LogBuffer. Derived handlers share
// the ring but hold their own attribute/group context.
type logHandler struct {
	store  *LogBuffer
	attrs  []slog.Attr
	groups []string
}

func (h *logHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs()+len(h.attrs))
	prefix := ""
	if len(h.groups) > 0 {
		prefix = joinGroups(h.groups) + "."
	}
	for _, a := range h.attrs {
		attrs[a.Key] = attrValue(a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[prefix+a.Key] = attrValue(a.Value)
		return true
	})
	if len(attrs) == 0 {
		attrs = nil
	}
	h.store.append(LogEntry{
		Time:    r.Time,
		Level:   r.Level.String(),
		Message: r.Message,
		Attrs:   attrs,
	})
	return nil
}

// attrValue resolves one attribute to a JSON-friendly value. An error is stored
// as its message: json.Marshal emits only exported fields, so a bare error value
// (e.g. *os.PathError, whose inner err is unexported) would serialise to {} and
// the UI log stream would show an empty object instead of the failure text.
func attrValue(v slog.Value) any {
	a := v.Resolve().Any()
	if e, ok := a.(error); ok {
		return e.Error()
	}
	return a
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	prefix := ""
	if len(h.groups) > 0 {
		prefix = joinGroups(h.groups) + "."
	}
	next := make([]slog.Attr, 0, len(h.attrs)+len(as))
	next = append(next, h.attrs...)
	for _, a := range as {
		next = append(next, slog.Attr{Key: prefix + a.Key, Value: a.Value})
	}
	return &logHandler{store: h.store, attrs: next, groups: h.groups}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, len(h.groups)+1)
	copy(groups, h.groups)
	groups[len(h.groups)] = name
	return &logHandler{store: h.store, attrs: h.attrs, groups: groups}
}

func joinGroups(groups []string) string {
	out := ""
	for i, g := range groups {
		if i > 0 {
			out += "."
		}
		out += g
	}
	return out
}
