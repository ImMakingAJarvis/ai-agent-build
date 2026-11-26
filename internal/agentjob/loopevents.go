// A job's applied-events log (events.jsonl): the record that a loop event
// was applied — its effect saved — so the loop.events consumer (ELR-1,
// M1.3b) can ack a delivery only after the effect is durable and a
// redelivery of an already-applied event is a no-op. Same shape as the
// conversation log (loopmsg.go): one JSON object per line, appended with
// O_APPEND and synced.
package agentjob

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// appliedEvent is one line of a job's events.jsonl: the id of the loop
// event that was applied, its type (for the record), and when it was
// recorded.
type appliedEvent struct {
	ID   string    `json:"id"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}

// EventsPath is where a job's applied-events log lives.
func (s *Store) EventsPath(id string) string {
	return filepath.Join(s.JobDir(id), "events.jsonl")
}

// EventApplied reports whether eventID is recorded as applied in the
// job's events.jsonl (a line with this id). A missing file means not
// applied. The consumer calls this first and MarkEventApplied last, so a
// crash between applying the effect and acking the delivery redelivers an
// event whose record already exists — the consumer skips it instead of
// applying it twice.
func (s *Store) EventApplied(jobID, eventID string) (bool, error) {
	if !ValidID(jobID) {
		return false, fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	f, err := os.Open(s.EventsPath(jobID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var lines [][]byte
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			lines = append(lines, line)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return false, rerr
			}
			break
		}
	}
	for i, line := range lines {
		var e appliedEvent
		if err := json.Unmarshal(line, &e); err != nil {
			if i == len(lines)-1 {
				// Torn tail: MarkEventApplied's write was cut off by a
				// crash — treat the half-line as not applied (the effect
				// of that event was not saved either).
				continue
			}
			return false, fmt.Errorf("agentjob: %s line %d: %w",
				"events.jsonl", i+1, err)
		}
		if e.ID == eventID {
			return true, nil
		}
	}
	return false, nil
}

// MarkEventApplied appends one line to the job's events.jsonl recording
// that eventID (of type typ) was applied: {"id":..., "type":...,
// "at":...}. The line goes out in a single Write on an O_APPEND open and
// the file is synced before it is closed, so the record is durable before
// the consumer may ack the delivery (ack only after the effect is saved).
func (s *Store) MarkEventApplied(jobID, eventID, typ string) error {
	if !ValidID(jobID) {
		return fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	if eventID == "" {
		return fmt.Errorf("agentjob: no event id")
	}
	if err := os.MkdirAll(s.JobDir(jobID), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(appliedEvent{ID: eventID, Type: typ, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(s.EventsPath(jobID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
