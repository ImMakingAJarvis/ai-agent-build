// NoBrakes 5/6: the audit log. Every job the nightly gate auto-enqueues is
// appended here (durable JSONL, one entry per line) so autonomous execution
// is reviewable after the fact: what ran, when, and that it was automatic.
package nobrakes

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// TriggerAutoMode is the trigger value recorded on every entry the nightly
// gate writes: the job ran because auto_mode was on, not because anyone
// asked for it.
const TriggerAutoMode = "auto_mode"

// AuditEntry is one line of the audit log: a job the nightly gate
// auto-enqueued.
type AuditEntry struct {
	// Timestamp is when the gate ran (UTC, RFC 3339).
	Timestamp string `json:"timestamp"`
	// Window is the UTC date ("YYYY-MM-DD") of the nightly window the pass
	// processed, so entries can be grouped per nightly batch.
	Window string `json:"window"`
	// JobID and Title identify the job as it was enqueued.
	JobID string `json:"job_id"`
	Title string `json:"title"`
	// Trigger is TriggerAutoMode for gate-written entries; the field exists
	// so a future trigger (a scheduled run, an operator command) can be told
	// apart in the same log.
	Trigger string `json:"trigger"`
}

// AuditRecorder records auto-executed jobs. *AuditLog is the durable-file
// implementation; tests may pass fakes. AutoEnqueue accepts nil for "no
// audit" and must treat it as a no-op.
type AuditRecorder interface {
	Record(AuditEntry) error
}

// AuditLog appends AuditEntry lines (one JSON object per line) to a durable
// file. Each entry is a single O_APPEND write, so a crash cannot corrupt
// earlier entries; a torn final line is skipped on read.
type AuditLog struct {
	path string
	f    *os.File
}

// DefaultAuditPath is the audit log location for a Jarvis data dir:
// <dataDir>/nobrakes/audit.jsonl.
func DefaultAuditPath(dataDir string) string {
	return filepath.Join(dataDir, "nobrakes", "audit.jsonl")
}

// NewAuditLog opens (creating the file and its parent directory) the audit
// log at path for appending. The gate opens one per pass and closes it
// before returning; a nil *AuditLog is the valid "no audit" mode.
func NewAuditLog(path string) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &AuditLog{path: path, f: f}, nil
}

// Path returns the log file on disk.
func (l *AuditLog) Path() string { return l.path }

// Close releases the file.
func (l *AuditLog) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// Record appends one entry. A nil receiver records nothing: AutoEnqueue may
// be called with a nil *AuditLog (e.g. when the log cannot be opened).
// Callers treat a failure as "the job ran, the record is missing" — they
// log it and keep going, never un-running an already-enqueued job.
func (l *AuditLog) Record(e AuditEntry) error {
	if l == nil {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = l.f.Write(append(line, '\n'))
	return err
}

// RecentAudit returns the last n entries of the log at path, in file order
// (n <= 0: all entries). A missing file reads as no entries, not an error;
// a corrupt line — a torn final write after a crash — is skipped so one bad
// line cannot hide the rest.
func RecentAudit(path string, n int) ([]AuditEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []AuditEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		all = append(all, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
