package store

import "github.com/macourteau/asiair-commander/internal/metrics"

// RegisterPoolMetrics wires the read and write connection-pool stats of a store
// opened by Open into the metrics registry (go_sql_* series, one set per handle).
// It is a no-op for a nil metrics handle or a store that is not the concrete
// sqlStore (a test fake), so callers need no type checks.
//
// This lives here rather than changing Open's signature so the 17 test call sites of
// Open stay untouched; it also keeps the concrete *sql.DB handles unexported.
func RegisterPoolMetrics(s Store, m *metrics.Metrics) {
	ss, ok := s.(*sqlStore)
	if !ok {
		return
	}
	m.RegisterDBStats(ss.read, "read")
	m.RegisterDBStats(ss.write, "write")
}
