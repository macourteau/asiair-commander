package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/prometheus/common/expfmt"
	commonmodel "github.com/prometheus/common/model"
)

// metricsServer builds a minimal Server wired with a real metrics surface, a config
// whose MetricsExposeLAN it controls, and a Loopback predicate it controls, so the
// gate can be exercised without depending on the transport's real RemoteAddr.
func metricsServer(t *testing.T, exposeLAN, loopback bool) *httptest.Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Web.MetricsExposeLAN = exposeLAN
	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	srv := New(Deps{
		Config:   ptr,
		DataDir:  t.TempDir(),
		Metrics:  metrics.New("test-version"),
		Loopback: func(string) bool { return loopback },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestMetricsLoopbackGate(t *testing.T) {
	cases := []struct {
		name      string
		exposeLAN bool
		loopback  bool
		want      int
	}{
		{"non-loopback, gated", false, false, http.StatusForbidden},
		{"loopback, gated", false, true, http.StatusOK},
		{"non-loopback, exposed", true, false, http.StatusOK},
		{"loopback, exposed", true, true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := metricsServer(t, tc.exposeLAN, tc.loopback)
			resp, err := http.Get(ts.URL + "/metrics")
			if err != nil {
				t.Fatalf("get /metrics: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestMetricsEndpointServesValidExposition(t *testing.T) {
	ts := metricsServer(t, false, true) // loopback → allowed
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	parser := expfmt.NewTextParser(commonmodel.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatalf("exposition did not parse: %v", err)
	}
	// The go/process/build-info collectors are always present, so the exposition is
	// never empty and must contain the build-info family under our namespace.
	if _, ok := families["asiair_commander_build_info"]; !ok {
		t.Errorf("expected asiair_commander_build_info in exposition; got %d families", len(families))
	}
}
