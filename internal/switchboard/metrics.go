package switchboard

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the switchboard's Prometheus collectors, on a private registry
// (the token-ledger pattern): the service exposes /metrics from its own
// registry, so a switchboard restart never disturbs another service's
// series. Counters are per-process (they reset on a restart) — the switchboard
// keeps no state in the metrics, its durability lives in the listener and job
// stores.
type Metrics struct {
	reg *prometheus.Registry

	ListenersActive prometheus.Gauge
	WakesTotal      prometheus.Counter
	ForwardsTotal   prometheus.Counter
	NotifiesTotal   prometheus.Counter
	TimeoutsTotal   prometheus.Counter
	Resubscribes    prometheus.Counter
}

// NewMetrics builds and registers the switchboard collectors on a fresh
// registry. The returned registry is the one /metrics serves.
func NewMetrics() (*Metrics, *prometheus.Registry) {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		ListenersActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "switchboard_listeners_active",
			Help: "Number of listeners the switchboard currently holds (one file each, durable across restarts).",
		}),
		WakesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_wakes_total",
			Help: "Waiting jobs woken back to ready because a result landed on a listened queue.",
		}),
		ForwardsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_forwards_total",
			Help: "Messages republished to a target queue by a forward listener.",
		}),
		NotifiesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_notifies_total",
			Help: "Discord notifications sent by a notify listener (message-landed and timeout).",
		}),
		TimeoutsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_timeouts_total",
			Help: "Wake listeners whose deadline passed with the correlation unarrived; the job was woken with status=timeout.",
		}),
		Resubscribes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_resubscribes_total",
			Help: "Times the switchboard re-detected a dead broker connection and reconnected, resuming its listener queues.",
		}),
	}
	m.reg.MustRegister(
		m.ListenersActive, m.WakesTotal, m.ForwardsTotal,
		m.NotifiesTotal, m.TimeoutsTotal, m.Resubscribes,
	)
	return m, m.reg
}
