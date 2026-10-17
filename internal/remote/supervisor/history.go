package supervisor

import (
	"context"
	"encoding/json"
	"time"
)

// MetaStore is the slice of Commander's store the supervisor needs: the meta
// key/value table in state.db, where each remote's connection history and the
// rigs' last-known endpoints live.
type MetaStore interface {
	GetMeta(ctx context.Context, key string) (string, bool, error)
	SetMeta(ctx context.Context, key, value string) error
}

// historyKey is the meta key of a remote's connection history. It is keyed by
// remote id, so a Re-pair, which keeps the id, keeps the history.
func historyKey(id string) string { return "remote:history:" + id }

// history is what survives a Commander restart about a remote's connection:
// when it last answered a hello, under which pairing, and the remote process
// that answered. It holds no secret. Without it a restart would turn a remote
// that has been down for hours into "Waiting for first connection", and reset
// "Unreachable since".
type history struct {
	// LastHelloNS is the last successful hello under any pairing, on
	// Commander's clock (unix nanoseconds); 0 if none.
	LastHelloNS int64 `json:"last_hello_ns,omitempty"`
	// Fingerprint is the pairing that last answered; a hello under the
	// current pairing has happened when it matches.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Instance is the remote process that last answered, and Restarts how many
	// times a new one has.
	Instance string `json:"instance,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
}

func (h history) lastHello() time.Time {
	if h.LastHelloNS == 0 {
		return time.Time{}
	}
	return time.Unix(0, h.LastHelloNS)
}

// loadHistory reads a remote's history; a missing or unreadable record reads
// as none, which only costs the card its past (it shows Waiting until the next
// hello).
func (s *Supervisor) loadHistory(ctx context.Context, id string) history {
	raw, ok, err := s.store.GetMeta(ctx, historyKey(id))
	if err != nil {
		s.log.Warn("remote: could not read connection history", "remote", id, "error", err)
		return history{}
	}
	if !ok || raw == "" {
		return history{}
	}
	var h history
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		s.log.Warn("remote: connection history unreadable; starting without it", "remote", id, "error", err)
		return history{}
	}
	return h
}

func (s *Supervisor) saveHistory(ctx context.Context, id string, h history) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return s.store.SetMeta(ctx, historyKey(id), string(b))
}
