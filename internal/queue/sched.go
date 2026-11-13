// Scheduled cadence queues (docs/scheduled-work.md): one durable queue per
// cadence — sched.hourly, sched.daily, sched.weekly, sched.monthly — each
// bound to the events exchange on <queue>.#. A tick is a sched.<cadence>.tick
// event; the publisher (cmd/sched-tick) fires one at each boundary crossing,
// and any service may subscribe to a cadence queue to run scheduled work on
// the hour, day, week or month. The cadence queues are created on demand
// (EnsureSchedQueue), idempotently, so a publish before the broker reloads
// rabbitmq/definitions.json still routes. Like the cap/flow queues they carry
// the dead-letter args: a tick a subscriber rejects or that expires lands on
// the dead queue with a reason instead of vanishing.
package queue

import (
	"fmt"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/sched"
)

// SchedEvent builds the tick event for one cadence: kind=sched,
// subject=<cadence>, state=tick. The routing key is sched.<cadence>.tick,
// which ParseEventRoutingKey reads back as (sched, <cadence>, tick) with no
// special case.
func SchedEvent(c sched.Cadence) Event {
	return Event{
		Kind:    SchedKind,
		Subject: string(c),
		State:   sched.State,
		At:      time.Now().UTC(),
	}
}

// EnsureSchedQueue declares one cadence queue (durable, dead-letter args, no
// priority — ticks are not prioritized) and binds it to the events exchange
// on <name>.#. Re-declaring with identical args is a no-op, which is what
// makes it safe to call on every start and on every publish. It binds to the
// events exchange specifically — not the Conn's default (tasks) exchange —
// because ticks are scheduled-work triggers, not jobs.
func (c *Conn) EnsureSchedQueue(cadence sched.Cadence) error {
	name := sched.QueueName(cadence)
	if err := c.EnsureDLX(); err != nil {
		return fmt.Errorf("sched %s: %w", name, err)
	}
	if err := c.declareWithDLXArgs(name, dlxArgs()); err != nil {
		return fmt.Errorf("sched %s: declare: %w", name, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("sched %s: %w", name, err)
	}
	defer ch.Close()
	// Synchronous bind (noWait=false): a noWait bind returns before the broker
	// registers it, and a publish on another channel of the same connection is
	// not ordered after it — the broker can route-check the tick before the
	// binding exists and drop it (same hazard EnsureCapQueue documents).
	if err := ch.QueueBind(name, name+".#", EventsExchange, false, nil); err != nil {
		return fmt.Errorf("sched %s: bind: %w", name, err)
	}
	return nil
}

// EnsureAllSchedQueues declares every cadence queue. The publisher calls it
// once at start so the queues exist even if the broker has not reloaded
// definitions.json since the queues were added.
func (c *Conn) EnsureAllSchedQueues() error {
	for _, cadence := range sched.All {
		if err := c.EnsureSchedQueue(cadence); err != nil {
			return err
		}
	}
	return nil
}
