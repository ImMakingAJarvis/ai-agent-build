// Package metrics exposes Prometheus counters, gauges and histograms for
// the worker's job lifecycle: jobs by queue/model, duration, estimated vs.
// actual cost, and model run info (current model, VRAM, token throughput).
package metrics

import (
	"time"

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

	// --- Model run info ---

	// modelLoaded: 1 if a model is loaded in Ollama, 0 otherwise.
	modelLoaded prometheus.Gauge

	// modelName: current model name (label "model").
	modelName *prometheus.GaugeVec

	// modelVRAMBytes: VRAM used by the currently loaded model.
	modelVRAMBytes prometheus.Gauge

	// modelSizeBytes: total size of the currently loaded model.
	modelSizeBytes prometheus.Gauge

	// modelFamily: model architecture family (label "family").
	modelFamily *prometheus.GaugeVec

	// modelParams: parameter count of the current model in billions.
	modelParams prometheus.Gauge

	// modelQuantization: quantization level (label "quantization").
	modelQuantization *prometheus.GaugeVec

	// modelContextLength: context length of the current model.
	modelContextLength prometheus.Gauge

	// promptTokensTotal: cumulative prompt tokens evaluated.
	promptTokensTotal prometheus.Counter

	// generationTokensTotal: cumulative generated tokens.
	generationTokensTotal prometheus.Counter

	// promptEvalSecondsTotal: cumulative time spent evaluating prompts.
	promptEvalSecondsTotal prometheus.Counter

	// generationEvalSecondsTotal: cumulative time spent generating tokens.
	generationEvalSecondsTotal prometheus.Counter

	// requestsTotal: total Ollama requests made.
	requestsTotal prometheus.Counter
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

		// Model run info
		modelLoaded: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: "model",
			Name:      "loaded",
			Help:      "1 if a model is loaded in Ollama, 0 otherwise",
		}),
		modelName: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Subsystem: "model",
				Name:      "current_name",
				Help:      "Name of the currently loaded model",
			},
			[]string{"model"},
		),
		modelVRAMBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: "model",
			Name:      "vram_bytes",
			Help:      "VRAM used by the currently loaded model in bytes",
		}),
		modelSizeBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: "model",
			Name:      "size_bytes",
			Help:      "Total size of the currently loaded model in bytes",
		}),
		modelFamily: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Subsystem: "model",
				Name:      "current_family",
				Help:      "Architecture family of the currently loaded model",
			},
			[]string{"family"},
		),
		modelParams: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: "model",
			Name:      "params_billions",
			Help:      "Parameter count of the current model in billions",
		}),
		modelQuantization: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Subsystem: "model",
				Name:      "current_quantization",
				Help:      "Quantization level of the currently loaded model",
			},
			[]string{"quantization"},
		),
		modelContextLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: "model",
			Name:      "context_length",
			Help:      "Context length of the currently loaded model",
		}),
		promptTokensTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: "model",
			Name:      "prompt_tokens_total",
			Help:      "Cumulative number of prompt tokens evaluated",
		}),
		generationTokensTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: "model",
			Name:      "generation_tokens_total",
			Help:      "Cumulative number of generated (completion) tokens",
		}),
		promptEvalSecondsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: "model",
			Name:      "prompt_eval_seconds_total",
			Help:      "Cumulative time spent evaluating prompts (in seconds)",
		}),
		generationEvalSecondsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: "model",
			Name:      "generation_eval_seconds_total",
			Help:      "Cumulative time spent generating tokens (in seconds)",
		}),
		requestsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: "model",
			Name:      "requests_total",
			Help:      "Total number of Ollama requests made",
		}),
	}
	prometheus.MustRegister(m.jobsTotal)
	prometheus.MustRegister(m.jobDuration)
	prometheus.MustRegister(m.estimatedCost)
	prometheus.MustRegister(m.actualCost)
	prometheus.MustRegister(m.modelLoaded)
	prometheus.MustRegister(m.modelName)
	prometheus.MustRegister(m.modelVRAMBytes)
	prometheus.MustRegister(m.modelSizeBytes)
	prometheus.MustRegister(m.modelFamily)
	prometheus.MustRegister(m.modelParams)
	prometheus.MustRegister(m.modelQuantization)
	prometheus.MustRegister(m.modelContextLength)
	prometheus.MustRegister(m.promptTokensTotal)
	prometheus.MustRegister(m.generationTokensTotal)
	prometheus.MustRegister(m.promptEvalSecondsTotal)
	prometheus.MustRegister(m.generationEvalSecondsTotal)
	prometheus.MustRegister(m.requestsTotal)
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

// ObserveModelInfo updates model info gauges. Call periodically to reflect
// the currently loaded model. Pass loaded=false to clear all gauges.
func (m *Metrics) ObserveModelInfo(name string, vramBytes, sizeBytes int64,
	family, quantization string, paramsBillions float64, contextLength int, loaded bool) {
	if loaded {
		m.modelLoaded.Set(1)
		m.modelName.WithLabelValues(name).Set(1)
		m.modelVRAMBytes.Set(float64(vramBytes))
		m.modelSizeBytes.Set(float64(sizeBytes))
		m.modelFamily.WithLabelValues(family).Set(1)
		m.modelParams.Set(paramsBillions)
		m.modelQuantization.WithLabelValues(quantization).Set(1)
		m.modelContextLength.Set(float64(contextLength))
	} else {
		m.modelLoaded.Set(0)
		m.modelVRAMBytes.Set(0)
		m.modelSizeBytes.Set(0)
		m.modelParams.Set(0)
		m.modelContextLength.Set(0)
	}
}

// ObserveTokens records prompt and generation token counts along with their
// evaluation durations.
func (m *Metrics) ObserveTokens(promptTokens, generationTokens int,
	promptDuration, generationDuration time.Duration) {
	m.promptTokensTotal.Add(float64(promptTokens))
	m.generationTokensTotal.Add(float64(generationTokens))
	m.promptEvalSecondsTotal.Add(promptDuration.Seconds())
	m.generationEvalSecondsTotal.Add(generationDuration.Seconds())
}

// ObserveRequest increments the total request counter.
func (m *Metrics) ObserveRequest() {
	m.requestsTotal.Inc()
}
