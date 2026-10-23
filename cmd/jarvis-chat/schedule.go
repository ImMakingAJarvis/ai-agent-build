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
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planner"
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
	case strings.HasPrefix(low, "!plan"):
		return "plan", strings.TrimSpace(content[len("!plan"):]), true
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

func agentRunUp() bool {
	return exec.Command("pgrep", "-f", "/usr/local/bin/agent-run").Run() == nil
}

func enqueueConfirm(id string) string {
	if agentRunUp() {
		return fmt.Sprintf("Queued agent job %s for the worker. agent-run is up; it will pick this up at the next checkpoint.", id)
	}
	return fmt.Sprintf("Queued agent job %s for the worker. queued; agent-run is not running, so it will wait.", id)
}

func enqueueFromChat(in chat.Incoming, title, instructions string) (id string, err error) {
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
		Priority:         chatJobPriority,
		Source:           "discord",
		DiscordChannelID: in.ChannelID,
		DiscordMessageID: in.MessageID,
	})
	return id, err
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
	id, err := enqueueFromChat(in, block.Title, block.Instructions)
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

// planFromChat sends the task description through the planner, which
// breaks it into difficulty-scored chunks and enqueues each as an agent
// job. Returns the list of enqueued job IDs.
func planFromChat(taskDescription string) ([]string, error) {
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return nil, fmt.Errorf("open job store: %w", err)
	}
	p := planner.New(planner.Config{
		Store:  store,
		Source: "discord",
	})
	plan, err := p.PlanTask(taskDescription)
	if err != nil {
		return nil, fmt.Errorf("analyze: %w", err)
	}
	ids, err := p.EnqueuePlan(plan)
	if err != nil {
		return nil, fmt.Errorf("enqueue chunks: %w", err)
	}
	return ids, nil
}

// planConfirm formats a confirmation message for a planned set of jobs.
func planConfirm(ids []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Planned %d chunk(s):", len(ids)))
	for i, id := range ids {
		b.WriteString(fmt.Sprintf("\n  %d. %s", i+1, id))
	}
	agentStatus := "agent-run is up; it will pick these up."
	if !agentRunUp() {
		agentStatus = "queued; agent-run is not running, so they will wait."
	}
	b.WriteString("\n\n" + agentStatus)
	return b.String()
}
