package kshadow

import (
	"context"
	"sort"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
)

// Row is one source queue's parity: the AMQP side (broker counters), the
// shadow side (Kafka records with header source=amqp) and kshadow's own
// accounting, so a gap between any two columns localizes the leak
// (published−mirrored: tap or attribution; mirrored−written: Kafka writes;
// written−consumed: live consumers).
type Row struct {
	Queue     string    `json:"queue"`
	Exchange  string    `json:"exchange"`
	Published int64     `json:"published"` // mgmt publish − baseline (tap Seq when mgmt is down)
	Mirrored  int64     `json:"mirrored"`  // source=amqp records on the Kafka topic (independent scan)
	Written   int64     `json:"written"`   // kshadow's successful Kafka writes
	Consumed  int64     `json:"consumed"`  // mgmt deliver − baseline
	Ready     int64     `json:"ready"`
	Unacked   int64     `json:"unacked"`
	Dropped   int64     `json:"dropped"`   // Published − Written
	Mismatch  int64     `json:"mismatch"`  // sampled read-backs whose body differed
	LastID    string    `json:"last_id,omitempty"`
	LastAt    time.Time `json:"last_at,omitempty"`
	Lag       string    `json:"lag,omitempty"` // now − LastAt, e.g. "3s"
}

// Report is the whole parity snapshot (`jarvis-kq parity` and the
// dashboard tile both render it).
type Report struct {
	At        time.Time `json:"at"`
	StartedAt time.Time `json:"started_at"` // kshadow first-seen time
	Kshadow   string    `json:"kshadow"`    // "up" or "down"
	Rows      []Row     `json:"rows"`
}

// Parity builds a Report from kshadow's persisted state, the RabbitMQ
// management API and an independent scan of the shadow topics.
type Parity struct {
	State  *State
	Admin  *Admin
	Kafka  *kq.Client
}

// Report builds the parity report for every queue kshadow has seen.
func (p *Parity) Report(ctx context.Context) (*Report, error) {
	rep := &Report{At: time.Now().UTC(), StartedAt: p.State.StartedAt, Kshadow: "up"}
	now := time.Now()
	for name, q := range p.State.Queues {
		row := Row{
			Queue:     name,
			Exchange:  q.Exchange,
			Written:   q.Written,
			Mismatch:  q.Mismatch,
			LastID:    q.LastID,
			LastAt:    q.LastAt,
			Published: q.Seq, // fallback: the tap count
			Consumed:  0,
		}
		if st, err := p.Admin.QueueStats(ctx, name); err == nil {
			if q.BaselinePub > 0 {
				row.Published = st.Published - q.BaselinePub
			}
			if q.BaselineDeliver > 0 {
				row.Consumed = st.Delivered - q.BaselineDeliver
			}
			row.Ready = st.Ready
			row.Unacked = st.Unacked
		} else {
			row.Consumed = -1 // mgmt unreachable: unknown, not zero
		}
		row.Dropped = row.Published - row.Written
		if q.LastAt.IsZero() {
			// No traffic on this queue since kshadow started: no lag to
			// report (an idle queue is not a lagging queue).
			row.Lag = ""
		} else {
			row.Lag = now.Sub(q.LastAt).Round(time.Second).String()
		}
		// Independent shadow-side count: scan the Kafka topic for
		// source=amqp records. Tasks queues mirror to their own topic
		// (== queue name); events mirror to the single events topic.
		topic := name
		if q.Exchange == "events" {
			topic = EventsQueue
		}
		if n, err := p.Kafka.CountSource(ctx, topic, "source", "amqp"); err == nil {
			row.Mirrored = n
		} else {
			row.Mirrored = -1 // scan failed: unknown, not zero
		}
		rep.Rows = append(rep.Rows, row)
	}
	sort.Slice(rep.Rows, func(i, j int) bool { return rep.Rows[i].Queue < rep.Rows[j].Queue })
	return rep, nil
}

// KshadowDown builds the report for the case where kshadow's state file
// cannot be read (the container is down or was never started): parity
// cannot be computed, and that is the answer (exit code 3).
func KshadowDown() *Report {
	return &Report{At: time.Now().UTC(), Kshadow: "down"}
}
