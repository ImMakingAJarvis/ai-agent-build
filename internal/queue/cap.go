// Capability queues are one durable queue per capability (cap.<cap>),
// the unit of work for the capability workers (plan §3.4; the worker
// SDK is EVB-19). Like flow queues they are created on demand — every
// capability a model offers gets its own queue — and they carry the
// dead-letter args, so a message a worker rejects, expires, or redelivers
// past the budget lands on the dead queue with a reason instead of
// vanishing.
package queue

import (
	"fmt"
	"strings"
)

// CapQueuePrefix marks a queue as a capability queue.
const CapQueuePrefix = "cap."

// CapQueueName returns the queue name for a capability.
func CapQueueName(capability string) string { return CapQueuePrefix + capability }

// IsCapQueue reports whether name is a capability queue.
func IsCapQueue(name string) bool { return strings.HasPrefix(name, CapQueuePrefix) }

// EnsureCapQueue declares the capability queue (durable, with the
// dead-letter args) and binds it to the tasks exchange on <name>.#, so a
// message published before any worker exists is held on the broker until
// it is consumed or dead-letters. name is the full queue name (cap.<cap>).
// Re-declaring with identical args is a no-op, which is what makes this
// safe to call on every publish.
func (c *Conn) EnsureCapQueue(name string) error {
	if err := c.EnsureDLX(); err != nil {
		return fmt.Errorf("cap %s: %w", name, err)
	}
	if err := c.declareWithDLX(name); err != nil {
		return fmt.Errorf("cap %s: declare: %w", name, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("cap %s: %w", name, err)
	}
	defer ch.Close()
	if err := ch.QueueBind(name, name+".#", c.Exchange(), true, nil); err != nil {
		return fmt.Errorf("cap %s: bind: %w", name, err)
	}
	return nil
}
