package web

import (
	"bytes"
	"net/http"
	"runtime/pprof"
	"time"
)

// metricsHandler serves /metrics, loopback-bound unless metrics_expose_lan is set
// (design architecture §8: per-rig labels are richer than bare counts, so they are
// not served to the whole LAN by default). It is unauthenticated (a scraper needs
// to reach it). The gauges are read at scrape time by the metrics state collector,
// so there is no snapshot refresh here — the endpoint just gates and delegates to
// the promhttp handler built once at construction. A nil metrics handle (tests
// that omit it) serves an empty 200 rather than 503, so a scraper never errors on
// a stub server.
func (s *Server) metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.currentConfig()
		if !cfg.Web.MetricsExposeLAN && !s.loopback(r.RemoteAddr) {
			writeError(w, http.StatusForbidden,
				"metrics are loopback-only; set web.metrics_expose_lan to expose them to the LAN")
			return
		}
		if s.promHandler == nil {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.promHandler.ServeHTTP(w, r)
	})
}

// handleHealthz is the unauthenticated liveness/readiness probe the installer
// polls (design architecture §8/§19): 200 when the store answers a trivial
// query, 503 otherwise. It is not Host-restricted so the installer's localhost
// poll and external monitoring can both reach it.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store != nil {
		if _, err := s.deps.Store.UnackedCount(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePprof captures a CPU profile for the requested seconds (server-capped at
// 60) and returns it as an octet-stream (design architecture §8: profiling is an authenticated action,
// not an open pprof endpoint). It is guarded like any mutation.
func (s *Server) handlePprof(w http.ResponseWriter, r *http.Request) {
	seconds := atoiDefault(r.URL.Query().Get("seconds"), 30)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}

	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		writeError(w, http.StatusConflict, "a profile is already running")
		return
	}
	select {
	case <-time.After(time.Duration(seconds) * time.Second):
	case <-r.Context().Done():
	}
	pprof.StopCPUProfile()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="cpu.pprof"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
