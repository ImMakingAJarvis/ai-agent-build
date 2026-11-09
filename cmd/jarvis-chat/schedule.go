package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

const chatJobPriority = 80

type scheduleBlock struct {
	Title        string
	Instructions string
}

func cutBuildFix(content string) (kind, rest string, ok bool) {
	low := strings.ToLower(content)
	switch {
	case strings.HasPrefix(low, "!build"):
		return "build", strings.TrimSpace(content[len("!build"):]), true
	case strings.HasPrefix(low, "!fix"):
		return "fix", strings.TrimSpace(content[len("!fix"):]), true
	default:
		return "", "", false
	}
}

func titleAndBody(rest string) (title, body string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", ""
	}
	first, after, _ := strings.Cut(rest, "\n")
	title = strings.TrimSpace(first)
	if len(title) > 80 {
		title = strings.TrimSpace(title[:80])
	}
	body = strings.TrimSpace(after)
	if body == "" {
		body = rest
	}
	return title, body
}

func parseSchedule(text string) (block scheduleBlock, found, malformed bool, stripped string) {
	stripped = text
	body, foundFence := extractFence(text, "schedule")
	if !foundFence {
		body, foundFence = extractPrefixBlock(text)
	}
	if !foundFence {
		return scheduleBlock{}, false, false, text
	}
	title, instructions := parseTitleInstructions(body)
	if title == "" || instructions == "" {
		return scheduleBlock{}, true, true, stripSchedule(text)
	}
	return scheduleBlock{Title: title, Instructions: instructions}, true, false, stripSchedule(text)
}

func extractFence(text, lang string) (body string, found bool) {
	low := strings.ToLower(text)
	open := "```" + strings.ToLower(lang)
	i := strings.Index(low, open)
	if i < 0 {
		return "", false
	}
	start := i + len(open)
	// skip optional newline after the fence tag
	rest := text[start:]
	if r := strings.TrimLeft(rest, " \t"); strings.HasPrefix(r, "\n") {
		rest = strings.TrimPrefix(strings.TrimLeft(rest, " \t"), "\n")
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return strings.TrimSpace(rest), true
	}
	return strings.TrimSpace(rest[:end]), true
}

func extractPrefixBlock(text string) (body string, found bool) {
	low := strings.ToLower(text)
	i := strings.Index(low, "schedule:")
	if i < 0 {
		return "", false
	}
	// require start of line
	if i > 0 && text[i-1] != '\n' {
		return "", false
	}
	return strings.TrimSpace(text[i+len("schedule:"):]), true
}

func parseTitleInstructions(body string) (title, instructions string) {
	var inst []string
	inInst := false
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		low := strings.ToLower(trim)
		switch {
		case strings.HasPrefix(low, "title:"):
			title = strings.TrimSpace(trim[len("title:"):])
			inInst = false
		case strings.HasPrefix(low, "instructions:"):
			rest := strings.TrimSpace(trim[len("instructions:"):])
			inInst = true
			if rest != "" {
				inst = append(inst, rest)
			}
		case inInst:
			inst = append(inst, line)
		}
	}
	return title, strings.TrimSpace(strings.Join(inst, "\n"))
}

func stripSchedule(text string) string {
	low := strings.ToLower(text)
	if i := strings.Index(low, "```schedule"); i >= 0 {
		endRel := strings.Index(text[i+3:], "```")
		if endRel >= 0 {
			end := i + 3 + endRel + 3
			return strings.TrimSpace(text[:i] + text[end:])
		}
		return strings.TrimSpace(text[:i])
	}
	if i := strings.Index(low, "schedule:"); i >= 0 && (i == 0 || text[i-1] == '\n') {
		return strings.TrimSpace(text[:i])
	}
	return strings.TrimSpace(text)
}

func newChatJobID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("chat-%d-%s", time.Now().Unix(), hex.EncodeToString(b))
}

// agentRunUp reports whether the host agent-run loop is alive. The chat
// daemon runs in a container, so the check only works because the service
// shares the host PID namespace (docker-compose `pid: host`) and the image
// carries procps (docker/jarvis-go/Dockerfile) — without either, pgrep
// cannot see host processes and the check always reports "not running".
//
// The pattern is anchored to the loop's main process: the systemd unit
// runs the bash script itself, so its command line is exactly
// "bash /usr/local/bin/agent-run". Unanchored, any unrelated command line
// that merely mentions the path (a worker round editing agent-run, a grep
// in a journal, ...) would count as the loop being up.
func agentRunUp() bool {
	return exec.Command("pgrep", "-f", "^bash /usr/local/bin/agent-run").Run() == nil
}

func enqueueConfirm(id string) string {
	if agentRunUp() {
		return fmt.Sprintf("Queued agent job %s for the worker. agent-run is up; it will pick this up at the next checkpoint.", id)
	}
	return fmt.Sprintf("Queued agent job %s for the worker. queued; agent-run is not running, so it will wait.", id)
}

func enqueueFromChat(in chat.Incoming, class, title, instructions string) (id string, err error) {
	if title == "" {
		return "", fmt.Errorf("empty title")
	}
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return "", err
	}
	id = newChatJobID()
	_, err = store.Ingest(agentjob.Spec{
		ID:               id,
		Title:            title,
		Instructions:     instructions,
		Driver:           "local",
		Class:            class,
		Priority:         chatJobPriority,
		Source:           "discord",
		DiscordChannelID: in.ChannelID,
		DiscordMessageID: in.MessageID,
	})
	return id, err
}

// photoJobPriority / photoNowJobPriority place photo work in the queue. A
// plain !photo sits at the top of the build band — after every fix, ahead of
// ordinary build work ("lower than fix, higher than build") — while a
// !photonow sits at the top of the fix band, ahead of ordinary fixes, so it
// is pulled before anything else once the current round is interrupted.
const (
	photoJobPriority    = 999
	photoNowJobPriority = 1000
)

// enqueuePhotoJob stores a queued photo: the brain-expanded prompt goes to a
// sidecar file (the iGPU round reads it via jarvis-photo), and the job
// itself is tagged photo so the swap-window watcher brackets it. size is
// the requested frame ("16x9", ...) — stored to its own sidecar file — or
// "" for the service default (square). now=true places the job at the top
// of the fix band instead of the top of the build band.
func enqueuePhotoJob(in chat.Incoming, raw, usedPrompt, size string, now bool) (id string, err error) {
	if strings.TrimSpace(usedPrompt) == "" {
		return "", fmt.Errorf("empty prompt")
	}
	dataDir := agentjob.DataDir()
	store, err := agentjob.Open(dataDir)
	if err != nil {
		return "", err
	}
	id = newChatJobID()
	// Store the prompt first so the round can always read it back.
	if err := photo.WritePrompt(dataDir, id, usedPrompt); err != nil {
		return "", err
	}
	if size != "" {
		if err := photo.WriteSize(dataDir, id, size); err != nil {
			return "", err
		}
	}
	class, prio := agentjob.ClassBuild, photoJobPriority
	if now {
		class, prio = agentjob.ClassFix, photoNowJobPriority
	}
	_, err = store.Ingest(agentjob.Spec{
		ID:               id,
		Title:            "photo: " + photoTitle(raw, size),
		Instructions:     photoJobInstructions(id),
		Driver:           "igpu",
		Class:            class,
		Priority:         prio,
		Source:           "discord",
		DiscordChannelID: in.ChannelID,
		DiscordMessageID: in.MessageID,
		Tags:             []string{photo.Tag},
	})
	return id, err
}

// photoJobInstructions is what the iGPU round reads for a photo job: run the
// executor, which generates the photo on the art service (the 4090 is free
// during the brain-swap window) and delivers the finished image to the
// owner's Discord DM.
func photoJobInstructions(id string) string {
	return "This is a queued photo job. Do exactly one thing and nothing else:\n\n" +
		"    jarvis-photo make --job " + id + "\n\n" +
		"It reads the job's prompt, generates the photo on the art service (the brain is swapped out, so the 4090 is free) and sends the finished image to the owner's Discord DM.\n\n" +
		"When it prints 'sent' and exits 0 you are done — reply: photo sent. If it exits non-zero, reply with the error it printed and nothing else."
}

// photoTitle builds a photo job's title: the trimmed raw prompt, with the
// size in front (when set) so a 16x9 photo is recognizable in the queue.
func photoTitle(raw, size string) string {
	t := shortTitle(raw)
	if size != "" {
		t = size + " " + t
	}
	return t
}

// shortTitle trims a photo prompt for use as the job title.
func shortTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func applySchedule(answer string, in chat.Incoming) (visible, extra string) {
	block, found, malformed, stripped := parseSchedule(answer)
	if !found {
		return answer, ""
	}
	visible = stripped
	if malformed {
		return visible, "SCHEDULE block is malformed (need title: and instructions:). Did not enqueue."
	}
	id, err := enqueueFromChat(in, agentjob.ClassBuild, block.Title, block.Instructions)
	if err != nil {
		return visible, "Could not enqueue the SCHEDULE job: " + err.Error()
	}
	return visible, enqueueConfirm(id)
}

// used by tests to assert pause-signal is not created
func pauseSignalPath() string {
	conf := os.Getenv("JARVIS_CONF")
	if conf == "" {
		return ""
	}
	return conf + "/pause-signal"
}
