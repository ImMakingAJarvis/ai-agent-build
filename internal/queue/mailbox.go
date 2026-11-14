// Mailboxes are the machine channel between agents (plan §3.8): one durable
// queue per agent identity, agent.<name>.inbox. A brain round publishes a
// question to the recipient's inbox with jarvis-mq ask; the recipient's
// round answers with jarvis-mq answer, which lands on the questioner's inbox
// carrying the question's correlation id — the switchboard's wake listener
// (created by ask --wake) sees it and wakes the parked job. fyi/handoff/
// review_request messages have no wake and sit on the inbox until the
// recipient's round reads them. Like flow and cap queues, mailbox queues are
// created on demand and carry the dead-letter args, so a message nobody
// claims lands on the dead queue with a reason instead of vanishing.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// MailboxQueuePrefix marks a queue as an agent mailbox.
const MailboxQueuePrefix = "agent."

// MailboxQueueSuffix is the queue-suffix every mailbox carries.
const MailboxQueueSuffix = ".inbox"

// MailboxQueueName returns the queue name for an agent identity.
func MailboxQueueName(agent string) string {
	return MailboxQueuePrefix + agent + MailboxQueueSuffix
}

// MailboxAgentFromQueue returns the agent identity behind a mailbox queue
// name, or "" if name is not a mailbox queue.
func MailboxAgentFromQueue(name string) string {
	if !IsMailboxQueue(name) {
		return ""
	}
	return name[len(MailboxQueuePrefix) : len(name)-len(MailboxQueueSuffix)]
}

// IsMailboxQueue reports whether name is an agent mailbox queue.
func IsMailboxQueue(name string) bool {
	if len(name) <= len(MailboxQueuePrefix)+len(MailboxQueueSuffix) {
		return false
	}
	return strings.HasPrefix(name, MailboxQueuePrefix) &&
		strings.HasSuffix(name, MailboxQueueSuffix)
}

var mailboxAgentRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidMailboxAgent reports whether s is acceptable as an agent identity:
// lowercase alphanumeric segments separated by hyphens (brain, igpu-helper,
// sonnet, planner — and a remote agent's name when it lands). A single
// segment (no dots) keeps the queue name a clean topic-binding leaf, and the
// same rule is what the CLI validates --from/--to against.
func ValidMailboxAgent(s string) bool { return mailboxAgentRe.MatchString(s) }

// Mailbox message kinds (plan §3.8). question and answer form a wake pair
// (a question with a wake_job is a listener-wake; the answer carries the
// question's corr and lands on the questioner's inbox); the rest are fire
// and forget.
const (
	MailKindQuestion      = "question"
	MailKindAnswer        = "answer"
	MailKindFYI           = "fyi"
	MailKindHandoff       = "handoff"
	MailKindReviewRequest = "review_request"
)

// ValidMailKind reports whether k is a mailbox message kind.
func ValidMailKind(k string) bool {
	switch k {
	case MailKindQuestion, MailKindAnswer, MailKindFYI,
		MailKindHandoff, MailKindReviewRequest:
		return true
	}
	return false
}

// MailboxMessage is the mailbox envelope (plan §3.8): kind, from, to,
// subject, body, an optional corr (an answer copies its question's), an
// optional wake_job (informational — the wake listener is the mechanism,
// created by ask --wake), and an optional board ref tying the message to a
// Plane work item (mirrored as a comment on the item).
type MailboxMessage struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // question|answer|fyi|handoff|review_request
	From      string    `json:"from"` // sender identity
	To        string    `json:"to"`   // recipient identity (decides the queue)
	Subject   string    `json:"subject"`
	Body      string    `json:"body,omitempty"`
	Corr      string    `json:"corr,omitempty"` // correlation id; an answer copies its question's
	WakeJob   string    `json:"wake_job,omitempty"`
	BoardRef  string    `json:"board_ref,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks the envelope at publish time.
func (m MailboxMessage) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("mailbox: missing id")
	}
	if !ValidMailKind(m.Kind) {
		return fmt.Errorf("mailbox: kind %q is not one of question, answer, fyi, handoff, review_request", m.Kind)
	}
	if !ValidMailboxAgent(m.From) {
		return fmt.Errorf("mailbox: sender %q is not a valid agent identity", m.From)
	}
	if !ValidMailboxAgent(m.To) {
		return fmt.Errorf("mailbox: recipient %q is not a valid agent identity", m.To)
	}
	if m.Subject == "" {
		return fmt.Errorf("mailbox: missing subject")
	}
	return nil
}

// EncodeMailbox renders the envelope as JSON.
func EncodeMailbox(m MailboxMessage) ([]byte, error) {
	return json.Marshal(m)
}

// DecodeMailbox parses a mailbox envelope. Any JSON object decodes
// (missing fields stay zero), so callers discriminate on To/Kind — see
// IsMailboxBody.
func DecodeMailbox(b []byte) (MailboxMessage, error) {
	var m MailboxMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// IsMailboxBody reports whether body looks like a mailbox envelope: a valid
// kind AND a non-empty recipient. That pair is what distinguishes it from a
// generic Message (whose Kind is "" or "agent", and which has no to field)
// and from a Result (no kind/to at all), in the switchboard's decode order.
func IsMailboxBody(b []byte) bool {
	m, err := DecodeMailbox(b)
	return err == nil && m.To != "" && ValidMailKind(m.Kind)
}

// EnsureMailboxQueue declares the agent's mailbox (durable, with the
// dead-letter args — like flow and cap) and binds it to the tasks exchange
// on agent.<agent>.inbox.#, so a message published before the recipient's
// round exists is held on the broker. The bind and routing-key conventions
// match flow/cap, which is what keeps RequeueDead working for mailbox
// messages (it republishes with the original routing key).
func (c *Conn) EnsureMailboxQueue(agent string) error {
	if !ValidMailboxAgent(agent) {
		return fmt.Errorf("mailbox: %q is not a valid agent identity", agent)
	}
	q := MailboxQueueName(agent)
	if err := c.EnsureDLX(); err != nil {
		return fmt.Errorf("mailbox %s: %w", q, err)
	}
	if err := c.declareWithDLX(q); err != nil {
		return fmt.Errorf("mailbox %s: declare: %w", q, err)
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("mailbox %s: %w", q, err)
	}
	defer ch.Close()
	// Synchronous bind — see EnsureFlowQueue: a noWait bind can be outrun by
	// a publish on another channel of the same connection.
	if err := ch.QueueBind(q, q+".#", c.Exchange(), false, nil); err != nil {
		return fmt.Errorf("mailbox %s: bind: %w", q, err)
	}
	return nil
}

// PublishMailbox publishes the mailbox message to the recipient's inbox,
// creating and binding the queue first, and blocks until the broker
// confirms. The corr is mirrored onto the native AMQP correlation_id so the
// broker's own logs and any Get-based peeker see it too; the message is
// persistent (durable queue, delivery_mode 2) — that is what survives a
// broker restart.
func (c *Conn) PublishMailbox(ctx context.Context, m MailboxMessage) error {
	if err := m.Validate(); err != nil {
		return err
	}
	q := MailboxQueueName(m.To)
	if err := c.EnsureMailboxQueue(m.To); err != nil {
		return err
	}
	ch, err := c.NewChannel()
	if err != nil {
		return fmt.Errorf("mailbox %s: %w", q, err)
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("mailbox %s: confirm mode: %w", q, err)
	}
	confirms := make(chan amqp.Confirmation, 1)
	ch.NotifyPublish(confirms)
	body, err := EncodeMailbox(m)
	if err != nil {
		return fmt.Errorf("mailbox %s: encode: %w", q, err)
	}
	pub := amqp.Publishing{
		ContentType:   "application/json",
		Body:          body,
		DeliveryMode:  amqp.Persistent,
		Timestamp:     m.CreatedAt,
		CorrelationId: m.Corr,
	}
	if err := ch.PublishWithContext(ctx, c.Exchange(), q+".default", false, false, pub); err != nil {
		return fmt.Errorf("mailbox %s: publish: %w", q, err)
	}
	select {
	case cf := <-confirms:
		if !cf.Ack {
			return fmt.Errorf("mailbox %s: broker nacked the message", q)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("mailbox %s: timed out waiting for confirm", q)
	case <-ctx.Done():
		return fmt.Errorf("mailbox %s: waiting for confirm: %w", q, ctx.Err())
	}
	return nil
}

// mailboxNotFound reports whether err is an AMQP channel 404 (the inbox was
// never created — normal for an agent that has never received anything).
func mailboxNotFound(err error) bool {
	var pe *amqp.Error
	if errors.As(err, &pe) {
		return pe.Code == int(amqp.NotFound)
	}
	return false
}

// PeekMailbox shows up to n messages on the inbox without removing them.
// An empty (or not-yet-created) inbox returns no messages and no error.
// The batch is requeued in reverse to restore front-of-queue order (the
// switchboard's PeekFlow pattern).
func (c *Conn) PeekMailbox(agent string, n int) ([]MailboxMessage, error) {
	if n < 1 {
		n = 1
	}
	if !ValidMailboxAgent(agent) {
		return nil, fmt.Errorf("mailbox: %q is not a valid agent identity", agent)
	}
	q := MailboxQueueName(agent)
	ch, err := c.NewChannel()
	if err != nil {
		return nil, fmt.Errorf("mailbox %s: %w", q, err)
	}
	defer ch.Close()
	var dels []amqp.Delivery
	for len(dels) < n {
		del, ok, err := ch.Get(q, false)
		if err != nil {
			if mailboxNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("mailbox %s: get: %w", q, err)
		}
		if !ok {
			break
		}
		dels = append(dels, del)
	}
	if len(dels) == 0 {
		return nil, nil
	}
	out := make([]MailboxMessage, 0, len(dels))
	for _, del := range dels {
		m, err := DecodeMailbox(del.Body)
		if err != nil {
			out = append(out, MailboxMessage{ID: "undecodable", Body: string(del.Body)})
			continue
		}
		out = append(out, m)
	}
	for i := len(dels) - 1; i >= 0; i-- {
		if err := dels[i].Nack(false, true); err != nil {
			log.Printf("queue: mailbox %s: requeue: %v", q, err)
		}
	}
	return out, nil
}

// PopMailbox removes up to n messages from the inbox (acking them) and
// returns them in order.
func (c *Conn) PopMailbox(agent string, n int) ([]MailboxMessage, error) {
	if n < 1 {
		n = 1
	}
	if !ValidMailboxAgent(agent) {
		return nil, fmt.Errorf("mailbox: %q is not a valid agent identity", agent)
	}
	q := MailboxQueueName(agent)
	ch, err := c.NewChannel()
	if err != nil {
		return nil, fmt.Errorf("mailbox %s: %w", q, err)
	}
	defer ch.Close()
	var out []MailboxMessage
	for len(out) < n {
		del, ok, err := ch.Get(q, true)
		if err != nil {
			if mailboxNotFound(err) {
				break
			}
			return nil, fmt.Errorf("mailbox %s: get: %w", q, err)
		}
		if !ok {
			break
		}
		m, err := DecodeMailbox(del.Body)
		if err != nil {
			return nil, fmt.Errorf("mailbox %s: decode: %w", q, err)
		}
		out = append(out, m)
	}
	return out, nil
}
