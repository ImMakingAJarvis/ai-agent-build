// metrics.go is the loop's Prometheus collectors (ELR-1, M3.5, card
// ELR-33): the numbers that show whether the runtime does its job, above
// all how long a slot sat idle while a job was ready. Like the
// switchboard (internal/switchboard/metrics.go), the loop keeps them on
// a private registry: loopd serves /metrics from its own registry, so a
// loopd restart never disturbs another service's series. The collectors
// are per-process (they reset on a restart) — the durable state lives in
// the job store and the slot snapshots.
package loop

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The outcome labels of loopd_ticks_total.
const (
	tickOutcomeFinal     = "final"      // no tool call: the job is done
	tickOutcomeToolCalls = "tool_calls" // the job waits on the calls
	tickOutcomeError     = "error"      // the model round did not finish
)

// Metrics is the loop's Prometheus collectors, on a private registry.
// Every collector here is touched by the loop's own code path; a loop
// built with a nil *Metrics records nothing.
type Metrics struct {
	reg *prometheus.Registry

	// IdleWhileReadySeconds is the total time jobs sat in the ready
	// list while no tick was running — the number the whole project
	// exists to keep small.
	IdleWhileReadySeconds prometheus.Counter
	// TicksTotal is one model round per tick, labelled by outcome
	// (final, tool_calls, error).
	TicksTotal *prometheus.CounterVec
	// TickSeconds is the wall time of one tick: slot acquisition, the
	// model round, and the checkpoint.
	TickSeconds prometheus.Histogram
	// PromptTokens and OutputTokens are the tokens the model rounds
	// consumed, prompt and output separately.
	PromptTokens prometheus.Counter
	OutputTokens prometheus.Counter
	// SnapshotSaves counts the KV caches the slot manager saved to a
	// snapshot file (through slot.Config.OnSave); SnapshotRestores
	// counts the slots refilled from one.
	SnapshotSaves    prometheus.Counter
	SnapshotRestores prometheus.Counter
	// WarmHits counts the acquisitions that found the job's KV cache
	// still warm in the slot (no save, no restore, no engine calls).
	WarmHits prometheus.Counter
	// OpsRunning is the number of tool calls running right now in the
	// Runner's goroutines, started by this process.
	OpsRunning prometheus.Gauge
	// ReadyJobs is the number of jobs waiting in the ready list.
	ReadyJobs prometheus.Gauge
}

// NewMetrics builds and registers the loop's collectors on a fresh
// registry. The returned registry is the one /metrics serves.
func NewMetrics() (*Metrics, *prometheus.Registry) {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		IdleWhileReadySeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopd_idle_while_ready_seconds_total",
			Help: "Total time jobs sat in the ready list while no tick was running.",
		}),
		TicksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loopd_ticks_total",
			Help: "Model rounds run by the loop, by outcome: final, tool_calls, error.",
		}, []string{"outcome"}),
		TickSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "loopd_tick_seconds",
			Help:    "Wall time of one tick: slot acquisition, model round, checkpoint.",
			Buckets: prometheus.DefBuckets,
		}),
		PromptTokens: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopd_prompt_tokens_total",
			Help: "Prompt tokens the model rounds consumed.",
		}),
		OutputTokens: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopd_output_tokens_total",
			Help: "Output tokens the model rounds produced.",
		}),
		SnapshotSaves: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopd_snapshot_saves_total",
			Help: "KV caches the slot manager saved to a snapshot file.",
		}),
		SnapshotRestores: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopd_snapshot_restores_total",
			Help: "Slots refilled from a saved snapshot.",
		}),
		WarmHits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopd_warm_hits_total",
			Help: "Acquisitions that found the job's KV cache still warm in the slot.",
		}),
		OpsRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "loopd_ops_running",
			Help: "Tool calls running in the runner's goroutines right now.",
		}),
		ReadyJobs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "loopd_ready_jobs",
			Help: "Jobs waiting in the ready list.",
		}),
	}
	m.reg.MustRegister(
		m.IdleWhileReadySeconds, m.TicksTotal, m.TickSeconds,
		m.PromptTokens, m.OutputTokens, m.SnapshotSaves, m.SnapshotRestores,
		m.WarmHits, m.OpsRunning, m.ReadyJobs,
	)
	return m, m.reg
}

// recordTick records one tick: its outcome (final, tool_calls or error),
// its wall time, and the tokens the model round consumed; warm and
// restored mark the lease the tick ran under. A nil receiver records
// nothing, so it is safe to call through a nil Deps.Metrics.
func (m *Metrics) recordTick(outcome string, d time.Duration, promptTokens, outputTokens int, warm, restored bool) {
	if m == nil {
		return
	}
	m.TicksTotal.WithLabelValues(outcome).Inc()
	m.TickSeconds.Observe(d.Seconds())
	m.PromptTokens.Add(float64(promptTokens))
	m.OutputTokens.Add(float64(outputTokens))
	if warm {
		m.WarmHits.Inc()
	}
	if restored {
		m.SnapshotRestores.Inc()
	}
}

// addIdle folds d of ready-list idle time into the idle counter. A nil
// receiver records nothing.
func (m *Metrics) addIdle(d time.Duration) {
	if m == nil {
		return
	}
	m.IdleWhileReadySeconds.Add(d.Seconds())
}

// setReadyJobs sets the ready-list gauge to n. A nil receiver records
// nothing.
func (m *Metrics) setReadyJobs(n int) {
	if m == nil {
		return
	}
	m.ReadyJobs.Set(float64(n))
}

// countSnapshotSave records one saved KV-cache snapshot. A nil receiver
// records nothing, so the method is safe to wire to slot.Config.OnSave
// even when the loop has no metrics.
func (m *Metrics) countSnapshotSave() {
	if m == nil {
		return
	}
	m.SnapshotSaves.Inc()
}
