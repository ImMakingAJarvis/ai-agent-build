// Package reviewstate keeps the review state of a pull request: one JSON
// file per pull request number under a directory (default
// ~/.local/share/jarvis/review/), in the same on-disk JSON style as the
// agent job store (internal/agentjob). It also holds the revival log
// (revivals.jsonl), which the five-minute check appends to and the end of
// day digest reads.
package reviewstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Record is the review state of one pull request.
type Record struct {
	PR            int    `json:"pr"`
	Branch        string `json:"branch"`
	Head          string `json:"head"` // the current SHA
	AuthorModel   string `json:"author_model"`
	ReviewerModel string `json:"reviewer_model"`
	// Token is the model holding the turn: either the author model id or
	// the reviewer model id.
	Token string `json:"token"`
	// Stamps are SHAs a model confirmed, or empty when it has not.
	AuthorStamp   string `json:"author_stamp"`
	ReviewerStamp string `json:"reviewer_stamp"`
	Rounds        int    `json:"rounds"`
	OpenPoints    int    `json:"open_points"`
	PlaneItem     string `json:"plane_item"` // a display id like "REV-4"
	Held          bool   `json:"held"`       // Fable not released
	TurnPushed    bool   `json:"turn_pushed"`
}

// DefaultDir is where review records live on this machine.
func DefaultDir() string {
	if d := os.Getenv("JARVIS_DATA_DIR"); d != "" {
		return filepath.Join(d, "review")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	return filepath.Join(home, ".local", "share", "jarvis", "review")
}

func recordPath(dir string, pr int) string {
	return filepath.Join(dir, fmt.Sprintf("pr-%d.json", pr))
}

// Save writes the record for r.PR atomically: a temp file in the same
// directory, then rename. The file mode is 0600.
func Save(dir string, r Record) error {
	if r.PR <= 0 {
		return fmt.Errorf("reviewstate: bad PR number %d", r.PR)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "pr-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, recordPath(dir, r.PR))
}

// Load reads the record for PR pr. A missing file is not an error in the
// sense of "stop": it returns a zero record and os.ErrNotExist, which the
// caller can see with errors.Is.
func Load(dir string, pr int) (Record, error) {
	b, err := os.ReadFile(recordPath(dir, pr))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, os.ErrNotExist
		}
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, err
	}
	return r, nil
}
