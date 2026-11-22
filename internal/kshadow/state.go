package kshadow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is kshadow's persisted state, saved atomically to KSHADOW_STATE
// (default /data/kshadow-state.json, bind-mounted from the host so
// `jarvis-kq parity` and the dashboard tile read it). The counters
// survive restarts, so the parity window is the whole shadow period
// (the 48h gate), not the time since the last restart.
type State struct {
	StartedAt time.Time              `json:"started_at"`
	Queues    map[string]*QueueState `json:"queues"`
}

// QueueState is one source queue's shadow accounting. Seq counts messages
// received on the mirror (== messages published to the queue's exchange
// family); Written counts successful Kafka writes; Mismatch counts sampled
// read-backs whose body differed. BaselinePub/BaselineDeliver are the
// RabbitMQ management counters captured when kshadow first saw the queue
// (zero until then — the management API reports cumulative counters).
type QueueState struct {
	Exchange string    `json:"exchange"`
	Seq      int64     `json:"seq"`
	Written  int64     `json:"written"`
	Mismatch int64     `json:"mismatch"`
	LastID   string    `json:"last_id"`
	LastAt   time.Time `json:"last_at"`

	BaselinePub     int64 `json:"baseline_pub"`
	BaselineDeliver int64 `json:"baseline_deliver"`
	// WatermarkSeq is the Seq of the last record actually written to
	// kq.watermark (the mirror throttles watermark writes).
	WatermarkSeq int64 `json:"watermark_seq"`
}

// NewState returns an empty state stamped with the first-seen time.
func NewState() *State {
	return &State{StartedAt: time.Now().UTC(), Queues: map[string]*QueueState{}}
}

// Queue returns (creating if needed) the state for a source queue.
func (s *State) Queue(name string) *QueueState {
	q, ok := s.Queues[name]
	if !ok {
		q = &QueueState{}
		s.Queues[name] = q
	}
	return q
}

// Save writes the state atomically (temp file + rename in the same
// directory, so a crash never leaves a half-written file).
func (s *State) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("state: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// LoadState reads persisted state; a missing or unreadable file yields a
// fresh state (the counters are accounting, not a source of truth — the
// management API and the Kafka log remain).
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewState(), nil
		}
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("state: unmarshal %s: %w", path, err)
	}
	if s.Queues == nil {
		s.Queues = map[string]*QueueState{}
	}
	return &s, nil
}
