// Package photo holds the shared conventions of the queued photo pipeline:
// where a photo job's (brain-expanded) prompt is stored, so the iGPU round
// that runs the job can read it back without a long prompt being passed on a
// command line (and shell-quoted).
package photo

import (
	"os"
	"path/filepath"
	"strings"
	"time"
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

// SizePath is the file holding a photo job's requested size token
// ("16x9", "1024x576", ...), next to the prompt: <data>/photos/<id>.size.
func SizePath(dataDir, id string) string {
	return filepath.Join(dataDir, PromptSubdir, id+".size")
}

// WriteSize stores a photo job's size token alongside its prompt.
func WriteSize(dataDir, id, size string) error {
	p := SizePath(dataDir, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(size), 0o644)
}

// ReadSize loads a photo job's size token. It returns os.ErrNotExist when
// the file is missing (the job was queued before sizes existed): callers
// treat that as "no size" (the art service default).
func ReadSize(dataDir, id string) (string, error) {
	b, err := os.ReadFile(SizePath(dataDir, id))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// SentPath is the marker a photo job's round leaves behind once its image
// was generated AND delivered to Discord (jarvis-photo writes it). The run
// wrapper (agentjob Runner.finish) treats a photo round that ends without
// the marker as a failure, not a done job: the iGPU agent exits 0 even when
// jarvis-photo failed, and without the marker a failed photo would be
// recorded done — nothing sent, no retry, the image silently lost.
func SentPath(dataDir, id string) string {
	return filepath.Join(dataDir, PromptSubdir, id+".sent")
}

// MarkSent records that the job's photo was delivered, replacing any stale
// marker from an earlier round.
func MarkSent(dataDir, id string) error {
	p := SentPath(dataDir, id)
	_ = os.Remove(p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// ClearSent removes a stale delivery marker. jarvis-photo calls it at the
// start of a round so that only this round's delivery can count: a re-run
// that fails again must fail the job, not pass on the first round's marker.
func ClearSent(dataDir, id string) error {
	return os.Remove(SentPath(dataDir, id))
}

// RefPath is the file holding a photo job's reference image (the raw bytes
// of the photo of the subject the generated image should keep), next to the
// prompt: <data>/photos/<id>.png. A reference means the job must generate
// with the reference-capable model (FLUX.2 [klein]) rather than the plain
// text-to-image one.
func RefPath(dataDir, id string) string {
	return filepath.Join(dataDir, PromptSubdir, id+".png")
}

// WriteRefBytes stores a photo job's reference image (the image bytes)
// alongside its prompt, creating the photos directory.
func WriteRefBytes(dataDir, id string, b []byte) error {
	p := RefPath(dataDir, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// RefFile returns the path to a photo job's reference image. It returns
// os.ErrNotExist when the job carries no reference (the common case: a plain
// text-to-image photo), which callers treat as "no reference".
func RefFile(dataDir, id string) (string, error) {
	p := RefPath(dataDir, id)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// JobInstructions is what the iGPU round reads for a photo job: run the
// executor, which generates the photo on the art service (the 4090 is free
// during the brain-swap window) and delivers the finished image to the
// owner's Discord DM. It lives here (not in the chat daemon) so every
// enqueue path — Discord, the dashboard, a manual jarvis-jobs add — writes
// the same instructions for the round.
func JobInstructions(id string) string {
	return "This is a queued photo job. Do exactly one thing and nothing else:\n\n" +
		"    jarvis-photo make --job " + id + "\n\n" +
		"It reads the job's prompt, generates the photo on the art service (the brain is swapped out, so the 4090 is free) and sends the finished image to the owner's Discord DM.\n\n" +
		"When it prints 'sent' and exits 0 you are done — reply: photo sent. If it exits non-zero, reply with the error it printed and nothing else."
}
