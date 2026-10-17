package config

import "sync"

// Guard serializes every config write so the app has exactly one writer at a
// time. It is deliberately just a mutex wrapper: each writer (web saveConfig,
// logout's rotateSessionSecret, setup completion, and the migration applier)
// runs its existing Load → … → Save → ApplyConfig → Store inside Do, so no
// caller's behavior changes — only mutual exclusion is added, which also closes
// the pre-existing TOCTOU in saveConfig's unlocked read-modify-write.
//
// The zero value is ready to use.
type Guard struct {
	mu sync.Mutex
}

// Do runs fn while holding the write lock and returns fn's error unchanged.
func (g *Guard) Do(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}
