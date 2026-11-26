package agentjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LoopOp is one operation of a loop job: a tool call running outside the
// model (M1.2c). It lives at <JobDir>/ops/<ID>.json, and the stream the
// operation writes while it runs goes to OpLogPath (<ID>.log, not JSON).
type LoopOp struct {
	ID           string          `json:"id"`
	ToolCallID   string          `json:"tool_call_id"`
	Tool         string          `json:"tool"`
	Args         json.RawMessage `json:"args"`
	Status       string          `json:"status"`
	PID          int             `json:"pid,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	Deadline     *time.Time      `json:"deadline,omitempty"`
	LastProgress *time.Time      `json:"last_progress,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
}

// Operation statuses. Pending and awaiting_approval are the states an op
// sits in before (or waiting on) running; done, failed, timeout,
// cancelled and lost are terminal (LoopOp.Finished).
const (
	OpPending          = "pending"
	OpAwaitingApproval = "awaiting_approval"
	OpRunning          = "running"
	OpDone             = "done"
	OpFailed           = "failed"
	OpTimeout          = "timeout"
	OpCancelled        = "cancelled"
	OpLost             = "lost"
)

// Finished reports whether the op reached a terminal status: done,
// failed, timeout, cancelled or lost.
func (o LoopOp) Finished() bool {
	switch o.Status {
	case OpDone, OpFailed, OpTimeout, OpCancelled, OpLost:
		return true
	}
	return false
}

// LoopApproval is an approval request for an op: the loop asks the owner
// (or a guard) before the operation runs. It lives at
// <JobDir>/approvals/<ID>.json.
type LoopApproval struct {
	ID        string          `json:"id"`
	OpID      string          `json:"op_id"`
	Request   json.RawMessage `json:"request"`
	Status    string          `json:"status"` // "pending", "approved", "denied"
	DecidedBy string          `json:"decided_by,omitempty"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
	RemindAt  *time.Time      `json:"remind_at,omitempty"`
}

// validOpID reports whether id may name an op or approval file: non-empty
// and carrying no path separator or parent-dir reference, so the file
// always stays inside its ops/ or approvals/ directory.
func validOpID(id string) bool {
	return id != "" && !strings.Contains(id, "/") && !strings.Contains(id, "..")
}

// PutOp writes the op to <JobDir>/ops/<op.ID>.json with the package's
// atomic writeJSON, creating the ops directory if needed.
func (s *Store) PutOp(jobID string, op LoopOp) error {
	if !ValidID(jobID) {
		return fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	if !validOpID(op.ID) {
		return fmt.Errorf("agentjob: bad op id %q", op.ID)
	}
	if err := os.MkdirAll(filepath.Join(s.JobDir(jobID), "ops"), 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.JobDir(jobID), "ops", op.ID+".json"), op)
}

// GetOp reads the op named opID. It rejects bad ids and returns an error
// when the op file is missing.
func (s *Store) GetOp(jobID, opID string) (LoopOp, error) {
	var op LoopOp
	if !ValidID(jobID) {
		return op, fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	if !validOpID(opID) {
		return op, fmt.Errorf("agentjob: bad op id %q", opID)
	}
	b, err := os.ReadFile(filepath.Join(s.JobDir(jobID), "ops", opID+".json"))
	if err != nil {
		return op, err
	}
	if err := json.Unmarshal(b, &op); err != nil {
		return op, err
	}
	return op, nil
}

// Ops returns every op of the job sorted by ID. A job whose ops directory
// does not exist yet returns nil, nil; files ending in .tmp (an in-flight
// writeJSON) or .log (an operation's stream) are not ops and are skipped.
func (s *Store) Ops(jobID string) ([]LoopOp, error) {
	if !ValidID(jobID) {
		return nil, fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	ents, err := os.ReadDir(filepath.Join(s.JobDir(jobID), "ops"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ops []LoopOp
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".log") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.JobDir(jobID), "ops", name))
		if err != nil {
			return nil, err
		}
		var op LoopOp
		if err := json.Unmarshal(b, &op); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	sort.Slice(ops, func(a, b int) bool { return ops[a].ID < ops[b].ID })
	return ops, nil
}

// OpLogPath is where the op's output stream is written:
// <JobDir>/ops/<opID>.log.
func (s *Store) OpLogPath(jobID, opID string) string {
	return filepath.Join(s.JobDir(jobID), "ops", opID+".log")
}

// PutApproval writes the approval to <JobDir>/approvals/<a.ID>.json with
// the package's atomic writeJSON, creating the approvals directory if
// needed.
func (s *Store) PutApproval(jobID string, a LoopApproval) error {
	if !ValidID(jobID) {
		return fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	if !validOpID(a.ID) {
		return fmt.Errorf("agentjob: bad approval id %q", a.ID)
	}
	if err := os.MkdirAll(filepath.Join(s.JobDir(jobID), "approvals"), 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.JobDir(jobID), "approvals", a.ID+".json"), a)
}

// Approvals returns every approval of the job sorted by ID. A job whose
// approvals directory does not exist yet returns nil, nil; .tmp and .log
// files are skipped, as in Ops.
func (s *Store) Approvals(jobID string) ([]LoopApproval, error) {
	if !ValidID(jobID) {
		return nil, fmt.Errorf("agentjob: bad job id %q", jobID)
	}
	ents, err := os.ReadDir(filepath.Join(s.JobDir(jobID), "approvals"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var aps []LoopApproval
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".log") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.JobDir(jobID), "approvals", name))
		if err != nil {
			return nil, err
		}
		var a LoopApproval
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		aps = append(aps, a)
	}
	sort.Slice(aps, func(i, j int) bool { return aps[i].ID < aps[j].ID })
	return aps, nil
}
