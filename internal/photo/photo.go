// Package photo holds the shared conventions of the queued photo pipeline:
// where a photo job's (brain-expanded) prompt is stored, so the iGPU round
// that runs the job can read it back without a long prompt being passed on a
// command line (and shell-quoted).
package photo

import (
	"os"
	"path/filepath"
	"strings"
)

// PromptSubdir is the directory, under the jarvis data dir, that holds one
// .prompt file per photo job.
const PromptSubdir = "photos"

// Tag marks a job as photo work. The agent-run queue watcher brackets the
// ready photo-tagged batch with a brain-swap window (the 4090 is free then),
// so the photo generates on the iGPU helper's round while the brain is out.
const Tag = "photo"

// PromptPath is the file holding a photo job's prompt:
// <data>/photos/<id>.prompt.
func PromptPath(dataDir, id string) string {
	return filepath.Join(dataDir, PromptSubdir, id+".prompt")
}

// WritePrompt stores a photo job's prompt, creating the photos directory.
func WritePrompt(dataDir, id, prompt string) error {
	p := PromptPath(dataDir, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(prompt), 0o644)
}

// ReadPrompt loads a photo job's prompt. It returns os.ErrNotExist when the
// file is missing.
func ReadPrompt(dataDir, id string) (string, error) {
	b, err := os.ReadFile(PromptPath(dataDir, id))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
