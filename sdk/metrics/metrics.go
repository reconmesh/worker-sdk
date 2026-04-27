// Package metrics — Prometheus exposition for worker-sdk Tools.
//
// Counters / histograms exported by every worker:
//
//   reconmesh_worker_jobs_total{tool, phase, outcome}
//                                 counter — Run() invocations.
//                                 outcome ∈ ok, error.
//   reconmesh_worker_job_duration_seconds{tool, phase}
//                                 histogram — Run() wall-clock.
//   reconmesh_worker_assets_emitted_total{tool, phase, kind}
//                                 counter — NewAssets count.
//   reconmesh_worker_findings_emitted_total{tool, phase, severity}
//                                 counter — Findings count.
//
// Mounted at /metrics on the existing admin port (default :9090) by
// the runtime; the Tool author doesn't have to wire anything.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	JobsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "reconmesh_worker_jobs_total",
			Help: "Worker Run() invocations by (tool, phase, outcome).",
		},
		[]string{"tool", "phase", "outcome"},
	)
	JobDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "reconmesh_worker_job_duration_seconds",
			Help:    "Worker Run() wall-clock by (tool, phase).",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 600},
		},
		[]string{"tool", "phase"},
	)
	AssetsEmitted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "reconmesh_worker_assets_emitted_total",
			Help: "NewAssets returned by Run() by (tool, phase, kind).",
		},
		[]string{"tool", "phase", "kind"},
	)
	FindingsEmitted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "reconmesh_worker_findings_emitted_total",
			Help: "Findings returned by Run() by (tool, phase, severity).",
		},
		[]string{"tool", "phase", "severity"},
	)
)

func init() {
	prometheus.MustRegister(JobsTotal, JobDuration, AssetsEmitted, FindingsEmitted)
}

// Handler returns the Prometheus exposition handler. The runtime
// mounts this at /metrics on the admin server. Each Tool inherits
// the endpoint for free.
func Handler() http.Handler {
	return promhttp.Handler()
}
