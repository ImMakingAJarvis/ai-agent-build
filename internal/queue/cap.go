// Capability queues are one durable queue per capability (cap.<cap>),
// the unit of work for the capability workers (plan §3.4; the worker
// SDK is EVB-19). Like flow queues they are created on demand — every
// capability a model offers gets its own queue — and they carry the
// dead-letter args, so a message a worker rejects, expires, or redelivers
// past the budget lands on the dead queue with a reason instead of
// vanishing. Cap queues also declare x-max-priority 10, the same
// priority range the background/voice/task queues use, so a message can
// carry its envelope priority and the broker orders by it (plan §3.4).
package queue

import (
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// CapQueuePrefix marks a queue as a capability queue.
const CapQueuePrefix = "cap."

// CapMaxPriority is the x-max-priority every cap queue is declared with —
// the plan's value for the capability tier (same range as the task
// queues' 10).
const CapMaxPriority = 10

// CapQueueName returns the queue name for a capability.
func CapQueueName(capability string) string { return CapQueuePrefix + capability }

// ValidCapabilityName reports whether s is acceptable as a capability: a
// dotted path (image.generate) or a single word (echo), lowercase
// alphanumeric segments. The name decides the queue (cap.<cap>) and the
// exchange binding, so it is checked at submit time by every submitter
// (jarvis-mq, the superagent's rabbitmq tool, Hermes's submit_capability)
// rather than in the worker.
func ValidCapabilityName(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	return true
}

// ParseDeadline parses a deadline: an absolute RFC3339 time, or a duration
// taken from now ("2h").
func ParseDeadline(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return time.Now().UTC().Add(d), nil
	}
	return time.Time{}, fmt.Errorf("parse %q as RFC3339 or a duration from now (2h)", s)
}

// IsCapQueue reports whether name is a capability queue.
func IsCapQueue(name string) bool { return strings.HasPrefix(name, CapQueuePrefix) }

// capQueueArgs is the declaration a cap queue gets: the dead-letter
// exchange plus the priority range (classic queue type is the broker
// default and is not spelled out).
func capQueueArgs() amqp.Table {
	args := dlxArgs()
	args["x-max-priority"] = CapMaxPriority
	return args
}

// EnsureCapQueue declares the capability queue (durable,
// x-max-priority 10, with the dead-letter args) and binds it to the tasks
// exchange on <name>.#, so a message published before any worker exists is
// held on the broker until it is consumed or dead-letters. name is the full
// queue name (cap.<cap>). Re-declaring with identical args is a no-op,
// which is what makes this safe to call on every publish; a queue declared
// before the priority arg existed is migrated on the next empty pass.
func (c *Conn) EnsureCapQueue(name string) error {
	if err := c.EnsureDLX(); err != nil {
		return fmt.Errorf("cap %s: %w", name, err)
	}
	if err := c.declareWithDLXArgs(name, capQueueArgs()); err != nil {
		return fmt.Errorf("cap %s: declare: %w", name, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("cap %s: %w", name, err)
	}
	defer ch.Close()
	// Synchronous bind: noWait would return once the frame is written,
	// and a publish on another channel of the same connection is not
	// ordered after it — the broker can route-check the publish before
	// the binding is registered and silently drop it (EVB-19).
	if err := ch.QueueBind(name, name+".#", c.Exchange(), false, nil); err != nil {
		return fmt.Errorf("cap %s: bind: %w", name, err)
	}
	return nil
}
