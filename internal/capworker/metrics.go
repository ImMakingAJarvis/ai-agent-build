package capworker

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is one worker's Prometheus metrics, on its own registry: a
// worker is its own process, but tests run several in one process, where
// the default registry would reject the second registration.
type Metrics struct {
	reg *prometheus.Registry

	deliveries *prometheus.CounterVec // capworker_deliveries_total{outcome}
	duration   *prometheus.HistogramVec
	inflight   prometheus.Gauge
	loaded     prometheus.Gauge
	lastWork   prometheus.Gauge
}

// NewMetrics builds the metric vectors and pre-creates the outcome
// series so they show up in a scrape from the first request on.
func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.deliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "capworker",
		Name:      "deliveries_total",
		Help:      "capability deliveries by terminal outcome (ok, timeout, error)",
	}, []string{"outcome"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "capworker",
		Name:      "handler_duration_seconds",
		Help:      "time the worker spent on one request, by outcome",
		Buckets:   []float64{0.5, 1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"outcome"})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "capworker",
		Name:      "inflight",
		Help:      "requests currently being handled (0 or 1 — prefetch is 1)",
	})
	m.loaded = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "capworker",
		Name:      "loaded",
		Help:      "1 = the worker's resources are loaded, 0 = idle-unloaded",
	})
	m.lastWork = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "capworker",
		Name:      "last_work_timestamp_seconds",
		Help:      "unix time the worker last picked up a request",
	})
	m.reg.MustRegister(
		m.deliveries, m.duration, m.inflight, m.loaded, m.lastWork,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, outcome := range []string{"ok", "timeout", "error"} {
		m.deliveries.WithLabelValues(outcome).Add(0)
		m.duration.WithLabelValues(outcome).Observe(0)
	}
	m.loaded.Set(1)
	m.lastWork.SetToCurrentTime()
	return m
}

// Handler is the /metrics handler for this worker's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Observe records one delivery's outcome and how long the worker spent
// on it (the handler's run for a real delivery; near-zero for a
// redelivery or a deadline timeout, where no work happened).
func (m *Metrics) Observe(outcome string, d time.Duration) {
	m.deliveries.WithLabelValues(outcome).Inc()
	m.duration.WithLabelValues(outcome).Observe(d.Seconds())
}

// SetInflight records the number of deliveries being handled.
func (m *Metrics) SetInflight(n float64) { m.inflight.Set(n) }

// SetLoaded records whether the worker's resources are loaded.
func (m *Metrics) SetLoaded(loaded bool) {
	if loaded {
		m.loaded.Set(1)
	} else {
		m.loaded.Set(0)
	}
}

// SetLastWork records the time the worker last picked up a request.
func (m *Metrics) SetLastWork(t time.Time) { m.lastWork.Set(float64(t.Unix())) }
