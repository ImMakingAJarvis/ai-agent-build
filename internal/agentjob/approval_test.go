package agentjob

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApprovalHashStable: the same tool and args always give the same
// hash — including the same JSON written with different formatting, since
// the args are compacted before hashing — and the hash is a 64-char hex
// sha256.
func TestApprovalHashStable(t *testing.T) {
	args := json.RawMessage(`{"cmd":"ls -la"}`)
	h1 := ApprovalHash("bash", args)
	h2 := ApprovalHash("bash", args)
	if h1 != h2 {
		t.Fatalf("hash not stable: %q vs %q", h1, h2)
	}
	if len(h1) != 64 || h1 != strings.ToLower(h1) {
		t.Fatalf("hash is not a 64-char lowercase hex sha256: %q", h1)
	}
	if _, err := hex.DecodeString(h1); err != nil {
		t.Fatalf("hash is not hex: %v", err)
	}
	h3 := ApprovalHash("bash", json.RawMessage(`{ "cmd" : "ls -la" }`))
	if h3 != h1 {
		t.Errorf("same args, different formatting: %q vs %q", h1, h3)
	}
}

// TestApprovalHashDiffersByArgs: the same tool with different args gives
// a different hash.
func TestApprovalHashDiffersByArgs(t *testing.T) {
	a := ApprovalHash("bash", json.RawMessage(`{"cmd":"ls"}`))
	b := ApprovalHash("bash", json.RawMessage(`{"cmd":"rm -rf /tmp/x"}`))
	if a == b {
		t.Errorf("different args gave the same hash %q", a)
	}
}

// TestApprovalHashDiffersByTool: different tools with the same args give
// a different hash.
func TestApprovalHashDiffersByTool(t *testing.T) {
	args := json.RawMessage(`{"cmd":"ls"}`)
	a := ApprovalHash("bash", args)
	b := ApprovalHash("read", args)
	if a == b {
		t.Errorf("different tools gave the same hash %q", a)
	}
}

// decideSetup ingests a running job and a pending approval bound to the
// hash of (tool, args), and returns the hash.
func decideSetup(t *testing.T, s *Store, jobID, apID, tool string, args json.RawMessage) string {
	t.Helper()
	if added, err := s.Ingest(Spec{ID: jobID, Title: "t", Instructions: "i"}); err != nil || !added {
		t.Fatalf("Ingest = %v, %v", added, err)
	}
	if _, err := s.Claim(jobID, "r1", "local"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(jobID, State{Status: Running}); err != nil {
		t.Fatal(err)
	}
	hash := ApprovalHash(tool, args)
	if err := s.PutApproval(jobID, LoopApproval{
		ID: apID, OpID: "op-1",
		Request: json.RawMessage(`{}`),
		Status:  ApprovalPending, Hash: hash,
	}); err != nil {
		t.Fatal(err)
	}
	return hash
}

// loadApproval reads the approval file back the way DecideApproval does.
func loadApproval(t *testing.T, s *Store, jobID, apID string) LoopApproval {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.JobDir(jobID), "approvals", apID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var a LoopApproval
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestDecideApproves: a pending approval with a matching hash is decided
// approved — (true, nil), Status/DecidedBy/DecidedAt set, file updated.
func TestDecideApproves(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := decideSetup(t, s, "job1", "ap-1", "bash", json.RawMessage(`{"cmd":"ls"}`))
	changed, err := s.DecideApproval("job1", "ap-1", hash, "nathan", "", true)
	if err != nil || !changed {
		t.Fatalf("DecideApproval = %v, %v; want true, nil", changed, err)
	}
	a := loadApproval(t, s, "job1", "ap-1")
	if a.Status != ApprovalApproved || a.DecidedBy != "nathan" || a.DecidedAt == nil {
		t.Errorf("approval after approve = %+v", a)
	}
	if a.Hash != hash {
		t.Errorf("hash changed by the decision: %q", a.Hash)
	}
}

// TestDecideDenyKeepsNote: a denial is stored with its note.
func TestDecideDenyKeepsNote(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := decideSetup(t, s, "job1", "ap-1", "bash", json.RawMessage(`{"cmd":"ls"}`))
	changed, err := s.DecideApproval("job1", "ap-1", hash, "nathan", "too risky", false)
	if err != nil || !changed {
		t.Fatalf("DecideApproval = %v, %v; want true, nil", changed, err)
	}
	a := loadApproval(t, s, "job1", "ap-1")
	if a.Status != ApprovalDenied || a.Note != "too risky" || a.DecidedBy != "nathan" {
		t.Errorf("approval after deny = %+v", a)
	}
}

// TestDecideHashMismatchRefusedAndUnchanged: a decision whose hash does
// not match the stored one is refused and writes nothing — the approval
// is still pending with no decider.
func TestDecideHashMismatchRefusedAndUnchanged(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stored := decideSetup(t, s, "job1", "ap-1", "bash", json.RawMessage(`{"cmd":"ls"}`))
	wrong := ApprovalHash("bash", json.RawMessage(`{"cmd":"rm -rf /"}`))
	if wrong == stored {
		t.Fatalf("test setup: wrong hash equals the stored one")
	}
	changed, err := s.DecideApproval("job1", "ap-1", wrong, "nathan", "", true)
	if !errors.Is(err, ErrApprovalHashMismatch) {
		t.Fatalf("err = %v, want ErrApprovalHashMismatch", err)
	}
	if changed {
		t.Error("changed = true, want false")
	}
	a := loadApproval(t, s, "job1", "ap-1")
	if a.Status != ApprovalPending || a.DecidedBy != "" || a.DecidedAt != nil || a.Note != "" {
		t.Errorf("approval changed by a mismatched decision: %+v", a)
	}
}

// TestSecondDecisionIsNoop: once decided, a further decision — even by
// another decider with a different vote — changes nothing and returns
// (false, nil); DecidedBy keeps the first decider.
func TestSecondDecisionIsNoop(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := decideSetup(t, s, "job1", "ap-1", "bash", json.RawMessage(`{"cmd":"ls"}`))
	if changed, err := s.DecideApproval("job1", "ap-1", hash, "nathan", "ok", true); err != nil || !changed {
		t.Fatalf("first decision = %v, %v; want true, nil", changed, err)
	}
	changed, err := s.DecideApproval("job1", "ap-1", hash, "guard", "no", false)
	if err != nil || changed {
		t.Fatalf("second decision = %v, %v; want false, nil", changed, err)
	}
	a := loadApproval(t, s, "job1", "ap-1")
	if a.Status != ApprovalApproved || a.DecidedBy != "nathan" || a.Note != "ok" {
		t.Errorf("second decision overwrote the first: %+v", a)
	}
}

// TestDecideAfterJobStoppedIsClosed: a stopped job (the store's parked
// status) has lapsed its approval — ErrApprovalClosed, nothing written.
func TestDecideAfterJobStoppedIsClosed(t *testing.T) {
	decideAfterTerminalJob(t, Parked)
}

// TestDecideAfterJobDoneIsClosed: same, for a done job.
func TestDecideAfterJobDoneIsClosed(t *testing.T) {
	decideAfterTerminalJob(t, Done)
}

// TestDecideAfterJobFailedIsClosed: same, for a failed job.
func TestDecideAfterJobFailedIsClosed(t *testing.T) {
	decideAfterTerminalJob(t, Failed)
}

// decideAfterTerminalJob decides an approval after the job reached the
// given terminal status and checks the refusal writes nothing.
func decideAfterTerminalJob(t *testing.T, status string) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := decideSetup(t, s, "job1", "ap-1", "bash", json.RawMessage(`{"cmd":"ls"}`))
	if err := s.SetStatus("job1", State{Status: status}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.DecideApproval("job1", "ap-1", hash, "nathan", "", true)
	if !errors.Is(err, ErrApprovalClosed) {
		t.Fatalf("err = %v, want ErrApprovalClosed (job %s)", err, status)
	}
	if changed {
		t.Error("changed = true, want false")
	}
	a := loadApproval(t, s, "job1", "ap-1")
	if a.Status != ApprovalPending || a.DecidedBy != "" || a.DecidedAt != nil {
		t.Errorf("approval changed after the job went %s: %+v", status, a)
	}
}

// TestOldApprovalFileLoads: an approval file written before Hash,
// ExpiresAt and Note existed (no hash key at all) still loads, with the
// new fields at their zero values, and DecideApproval reads it back the
// same way.
func TestOldApprovalFileLoads(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.Ingest(Spec{ID: "job1", Title: "t", Instructions: "i"}); err != nil || !added {
		t.Fatalf("Ingest = %v, %v", added, err)
	}
	old := `{"id":"ap-1","op_id":"op-1","request":{"tool":"rm"},"status":"pending"}`
	approvals := filepath.Join(s.JobDir("job1"), "approvals")
	if err := os.MkdirAll(approvals, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(approvals, "ap-1.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	all, err := s.Approvals("job1")
	if err != nil || len(all) != 1 {
		t.Fatalf("Approvals = %v, %v; want one approval", all, err)
	}
	a := all[0]
	if a.ID != "ap-1" || a.Status != "pending" {
		t.Errorf("old file did not load: %+v", a)
	}
	if a.Hash != "" || a.ExpiresAt != nil || a.Note != "" {
		t.Errorf("new fields not empty on an old file: %+v", a)
	}
	// The empty stored hash only matches an empty decision hash — a
	// mismatch refusal, not a silent decision.
	changed, err := s.DecideApproval("job1", "ap-1", ApprovalHash("rm", json.RawMessage(`{}`)), "nathan", "", true)
	if !errors.Is(err, ErrApprovalHashMismatch) {
		t.Fatalf("deciding an unhashed approval: err = %v, want ErrApprovalHashMismatch", err)
	}
	if changed {
		t.Error("changed = true, want false")
	}
}
