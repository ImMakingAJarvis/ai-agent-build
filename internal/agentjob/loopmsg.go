package agentjob

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LoopMessage is one line of a job's append-only conversation log
// (messages.jsonl): a single JSON object per line, in send order.
type LoopMessage struct {
	Seq     int             `json:"seq"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Tokens  int             `json:"tokens,omitempty"`
	At      time.Time       `json:"at"`
}

// MessagesPath is where a job's conversation log lives.
func (s *Store) MessagesPath(id string) string {
	return filepath.Join(s.JobDir(id), "messages.jsonl")
}

// AppendMessages appends msgs to a job's conversation log. It numbers them
// after whatever is already there (Seq continues 1, 2, 3 ...), stamps a
// zero At with now (UTC), and writes every line in a single Write call so a
// batch lands together. It returns the messages with their Seq set.
func (s *Store) AppendMessages(id string, msgs ...LoopMessage) ([]LoopMessage, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("agentjob: bad job id %q", id)
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	existing, err := s.Messages(id)
	if err != nil {
		return nil, err
	}
	last := 0
	if n := len(existing); n > 0 {
		last = existing[n-1].Seq
	}
	now := time.Now().UTC()
	for i := range msgs {
		msgs[i].Seq = last + 1 + i
		if msgs[i].At.IsZero() {
			msgs[i].At = now
		}
	}
	// Build the whole batch up front so it goes out in one Write.
	var buf bytes.Buffer
	for i := range msgs {
		line, err := json.Marshal(&msgs[i])
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(s.JobDir(id), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.MessagesPath(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	// One Write: the whole batch lands together (or not at all).
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return msgs, nil
}

// Messages reads a job's conversation log, one message per line. A missing
// file returns nil, nil. A final line that is not valid JSON (a write cut
// off by a crash) is ignored; any other bad line is an error. It reads with
// bufio.Reader so a line may be longer than bufio.Scanner's 64 KB limit.
func (s *Store) Messages(id string) ([]LoopMessage, error) {
	if !ValidID(id) {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(s.MessagesPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
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
				return nil, rerr
			}
			break
		}
	}

	out := make([]LoopMessage, 0, len(lines))
	for i, line := range lines {
		var m LoopMessage
		if err := json.Unmarshal(line, &m); err != nil {
			if i == len(lines)-1 {
				// Torn tail: the last write was cut off mid-line.
				continue
			}
			return nil, fmt.Errorf("agentjob: %s line %d: %w",
				filepath.Base(s.MessagesPath(id)), i+1, err)
		}
		out = append(out, m)
	}
	if out == nil {
		return nil, nil
	}
	return out, nil
}
