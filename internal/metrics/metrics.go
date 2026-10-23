// Package metrics exposes Prometheus counters, gauges and histograms for
// the worker's job lifecycle: jobs by queue/model, duration, estimated vs.
// actual cost.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const subsystem = "jobs"

// Metrics holds all Prometheus collectors for the worker.
type Metrics struct {
	// jobsTotal: cumulative count of jobs by queue, model, and status.
	jobsTotal *prometheus.CounterVec

	// jobDurationSeconds: histogram of wall-clock duration per queue/model.
	jobDuration *prometheus.HistogramVec

	// estimatedCost: cumulative $ estimated before dispatch.
	estimatedCost prometheus.Gauge

	// actualCost: cumulative $ billed after dispatch.
	actualCost prometheus.Gauge
}

// New creates a Metrics and registers it with the default Prometheus
// registry.
func New() *Metrics {
	m := &Metrics{
		jobsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: subsystem,
				Name:      "total",
				Help:      "Total number of jobs processed, by queue, model, and status",
			},
			[]string{"queue", "model", "status"},
		),
		jobDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Subsystem: subsystem,
				Name:      "duration_seconds",
				Help:      "Wall-clock duration of jobs in seconds",
				Buckets: []float64{
					0.5, 1, 2.5, 5, 10, 25, 50, 75, 120, 300, 600, 900, 1800,
				},
			},
			[]string{"queue", "model"},
		),
		estimatedCost: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "estimated_cost_dollars",
			Help:      "Cumulative estimated cost of all API-dispatched jobs in USD",
		}),
		actualCost: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "actual_cost_dollars",
			Help:      "Cumulative actual cost of all API-dispatched jobs in USD",
		}),
	}
	prometheus.MustRegister(m.jobsTotal)
	prometheus.MustRegister(m.jobDuration)
	prometheus.MustRegister(m.estimatedCost)
	prometheus.MustRegister(m.actualCost)
	return m
}

// ObserveCompleted records a completed job's duration and increments the
// completed counter.
func (m *Metrics) ObserveCompleted(queue, model string, durationSeconds float64) {
	m.jobsTotal.WithLabelValues(queue, model, "completed").Inc()
	m.jobDuration.WithLabelValues(queue, model).Observe(durationSeconds)
}

// ObserveFailed records a failed job.
func (m *Metrics) ObserveFailed(queue, model string) {
	m.jobsTotal.WithLabelValues(queue, model, "failed").Inc()
}

// ObserveDenied records a job denied by the approval gate, along with the
// estimated cost that triggered denial.
func (m *Metrics) ObserveDenied(queue, model string, estimatedCost float64) {
	m.jobsTotal.WithLabelValues(queue, model, "denied").Inc()
	m.estimatedCost.Add(estimatedCost)
}

// ObserveCost records estimated and actual costs for an API-dispatched job.
func (m *Metrics) ObserveCost(estimated, actual float64) {
	m.estimatedCost.Add(estimated)
	m.actualCost.Add(actual)
}
