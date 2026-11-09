// Package photo holds the shared conventions of the queued photo pipeline:
// where a photo job's (brain-expanded) prompt and its optional trailing
// instruction (follow-up) are stored, so the code executor that runs the job
// (jarvis-photo) can read them back without a long prompt being passed on a
// command line (and shell-quoted). Photo jobs run as plain code — no model
// round at all: the executor starts the art service if it is down, waits for
// the model to load, generates, delivers to Discord, and queues the
// follow-up if there is one (internal/agentjob Runner.runPhoto).
package photo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PromptSubdir is the directory, under the jarvis data dir, that holds one
// .prompt file (and optional .followup file) per photo job.
const PromptSubdir = "photos"

// Tag marks a job as photo work. The agent-run queue watcher brackets the
// ready photo-tagged batch with a brain-swap window (the 4090 is free then),
// and each job in the batch runs as code: the jarvis-photo executor, not a
// model round.
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

// SentPath is the marker a photo job's run leaves behind once its image was
// generated AND delivered to Discord (jarvis-photo writes it). The run
// wrapper (agentjob Runner.finish) treats a photo run that ends without the
// marker as a failure, not a done job: the executor exits 0 only on
// delivery, but the marker is what makes that guarantee checkable after the
// fact — without it a failed photo would be recorded done, nothing sent, no
// retry, the image silently lost.
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
// start of a run so that only this run's delivery can count: a re-run that
// fails again must fail the job, not pass on the first run's marker.
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

// Followup is a photo job's trailing instruction: once the image has been
// generated and delivered, the executor enqueues it as a new job, giving the
// job the image (the path substituted into {{image}}, or appended when the
// text has no placeholder).
//
// The body is free-form instruction text. It may begin with header lines —
// driver:, class:, priority:, title: — that set the follow-up job's spec;
// any other line (or a blank line) ends the header and starts the text.
//
//	driver: local
//	class: build
//	priority: 900
//	title: caption the new photo
//
//	Caption {{image}} and post it to #gallery.
type Followup struct {
	Driver       string
	Class        string
	Priority     int
	Title        string
	Instructions string
}

// FollowupPath is the file holding a photo job's trailing instruction:
// <data>/photos/<id>.followup. It is optional — a photo job without one
// simply ends after delivery.
func FollowupPath(dataDir, id string) string {
	return filepath.Join(dataDir, PromptSubdir, id+".followup")
}

// WriteFollowup stores a photo job's trailing instruction, creating the
// photos directory.
func WriteFollowup(dataDir, id, body string) error {
	p := FollowupPath(dataDir, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(body), 0o644)
}

// ReadFollowup loads a photo job's trailing instruction, verbatim (headers
// included). It returns os.ErrNotExist when the file is missing.
func ReadFollowup(dataDir, id string) (string, error) {
	b, err := os.ReadFile(FollowupPath(dataDir, id))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseFollowup splits a follow-up body into its optional header and the
// instruction text.
func ParseFollowup(body string) (*Followup, error) {
	fu := &Followup{}
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			break // free text: the instruction has started
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		isHeader := false
		switch key {
		case "driver":
			fu.Driver = val
			isHeader = true
		case "class":
			fu.Class = val
			isHeader = true
		case "title":
			fu.Title = val
			isHeader = true
		case "priority":
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("priority: %q is not a number", val)
			}
			fu.Priority = n
			isHeader = true
		}
		if !isHeader {
			break // a free-text line that happens to contain a colon — keep it
		}
		i++
	}
	fu.Instructions = strings.TrimSpace(strings.Join(lines[i:], "\n"))
	if fu.Instructions == "" {
		return nil, errors.New("follow-up has no instruction text")
	}
	return fu, nil
}

// StandardInstructions is the canonical job text of a photo job. Photo work
// runs as code (the jarvis-photo executor), so the text only documents what
// happens; the executor never needs a model to interpret it.
func StandardInstructions(id string) string {
	return "Photo job (code executor, no model round): run\n\n" +
		"    jarvis-photo make --job " + id + "\n\n" +
		"It reads the job's prompt, starts the art service if it is down and " +
		"waits for the model to load, generates the image (saved to disk and " +
		"in the gallery), sends it to the owner's Discord DM, and — if the job " +
		"has a follow-up (photos/" + id + ".followup) — queues it as a new " +
		"job with the image path."
}
