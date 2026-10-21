// Package queue provides JSON encoding/decoding helpers for job messages.
package queue

import (
	"encoding/json"
	"fmt"
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
