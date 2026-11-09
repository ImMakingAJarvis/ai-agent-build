// Package queue provides JSON encoding/decoding helpers for job messages.
package queue

import (
	"encoding/json"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

func encodeMessage(m Message) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal Message: %w", err)
	}
	return b, nil
}

// DecodeMessage parses a JSON-delivered body into a Message.
func DecodeMessage(body []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("unmarshal Message: %w", err)
	}
	return m, nil
}

// EncodeResult serializes a Result to the JSON body published to a request's
// ReplyTo queue.
func EncodeResult(r Result) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal Result: %w", err)
	}
	return b, nil
}

// DecodeResult parses a JSON result body into a Result.
func DecodeResult(body []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(body, &r); err != nil {
		return Result{}, fmt.Errorf("unmarshal Result: %w", err)
	}
	return r, nil
}

// newPublishing builds the AMQP envelope for m. It mirrors the v2 JSON
// fields CorrelationID and ReplyTo onto the native AMQP properties, so a
// CLI, a Python worker, or the broker's own logs see the same values the
// JSON body carries (plan §3.3). priority is this publish path's wire
// priority (0 = the queue's default; flow and location queues are not
// priority queues and pass 0). Empty v2 fields stay absent on the wire too:
// an unset CorrelationID or ReplyTo leaves the AMQP property empty, exactly
// as omitempty keeps it out of the JSON.
func newPublishing(m Message, priority uint8) (amqp.Publishing, error) {
	body, err := encodeMessage(m)
	if err != nil {
		return amqp.Publishing{}, err
	}
	return amqp.Publishing{
		ContentType:   "application/json",
		Body:          body,
		Priority:      priority,
		DeliveryMode:  amqp.Persistent,
		Timestamp:     m.CreatedAt,
		CorrelationId: m.CorrelationID,
		ReplyTo:       m.ReplyTo,
	}, nil
}
