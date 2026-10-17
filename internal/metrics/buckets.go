package metrics

import "github.com/prometheus/client_golang/prometheus"

// Bucket sets. prometheus.DefBuckets is deliberately never used — it tops out at
// 10s, while cycle durations scale with the poll interval (minutes). Each set is
// explicit so the classic buckets survive alongside the native ones (P11): setting
// NativeHistogramBucketFactor with a nil Buckets silently emits no classic buckets.
var (
	// BucketsNetwork covers per-frame transfers and admission waits: 10ms – 60s.
	BucketsNetwork = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

	// BucketsCycle covers a whole rig cycle, which scales with the poll interval:
	// 100ms – 1h.
	BucketsCycle = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800, 3600}

	// nativeHistogramBucketFactor gives ~8 native buckets per power of two on a
	// Prometheus 2.40+ scraper; older scrapers ignore it and read the classic
	// buckets above. Native histograms are experimental in client_golang, so this
	// one knob is confined to this file (design architecture §17.5).
	nativeHistogramBucketFactor = 1.1
)

// newHistogramVec builds a dual-emitting HistogramVec: explicit classic buckets and
// a native factor. It panics if buckets is empty — the one thing P11 forbids — so a
// nil-bucket histogram cannot be introduced by accident.
func newHistogramVec(name, help string, buckets []float64, labels []string) *prometheus.HistogramVec {
	if len(buckets) == 0 {
		panic("metrics: histogram " + name + " must declare explicit classic buckets (P11)")
	}
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:                        name,
		Help:                        help,
		Buckets:                     buckets,
		NativeHistogramBucketFactor: nativeHistogramBucketFactor,
	}, labels)
}
