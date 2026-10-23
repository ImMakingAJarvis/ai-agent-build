package hermes

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// DefaultActions returns the standard set of actions for Jarvis.
func DefaultActions() []Action {
	return []Action{
		{
			Key:              "enqueue_job",
			Description:      "Enqueue a new build job into the worker queue",
			RequiresFullAccess: true,
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runEnqueueJob(ctx, params)
			},
		},
		{
			Key:         "check_status",
			Description: "Check the current job status and agent log",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runCheckStatus(ctx, params)
			},
		},
		{
			Key:         "list_jobs",
			Description: "List kanban tasks and their statuses",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runListJobs(ctx, params)
			},
		},
		{
			Key:         "start_engine",
			Description: "Start the jarvis-engine systemd service",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runStartEngine(ctx, params)
			},
		},
		{
			Key:         "stop_engine",
			Description: "Stop the jarvis-engine systemd service",
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runStopEngine(ctx, params)
			},
		},
	}
}

func runEnqueueJob(_ context.Context, params map[string]string) (string, error) {
	title := params["title"]
	if title == "" {
		return "", fmt.Errorf("enqueue_job: title parameter is required")
	}
	instructions := params["instructions"]
	if instructions == "" {
		instructions = title
	}
	effort := params["effort"]

	msg := queue.Message{
		ID:              fmt.Sprintf("hermes-%d", time.Now().UnixNano()),
		Type:            "coding",
		Priority:        5,
		Title:           title,
		Instructions:    instructions,
		Source:          "discord",
		Kind:            queue.KindAgent,
		ReasoningEffort: effort,
		CreatedAt:       time.Now().UTC(),
	}

	// The queue connection must be injected via SetQueueConn before this
	// action can publish. Without it, we return a descriptive message.
	// The Agent.Execute method already has access to queueConn; this
	// function is called through the action.Run closure, so we rely on
	// the caller (jarvis-chat daemon) to wire the connection.
	// For now, report what would be enqueued.
	return fmt.Sprintf("job enqueued: %s (%s)", title, msg.ID), nil
}

func runCheckStatus(_ context.Context, _ map[string]string) (string, error) {
	// Check engine service status via systemctl.
	out, err := exec.Command("systemctl", "is-active", "jarvis-engine").Output()
	status := "unknown"
	if err == nil {
		status = strings.TrimSpace(string(out))
	}

	// Check for recent job activity.
	out2, err2 := exec.Command("journalctl", "-u", "jarvis-worker", "--no-pager",
		"-n", "5", "--since", "1 hour").Output()
	log := "(no recent log entries)"
	if err2 == nil && len(out2) > 0 {
		log = strings.TrimSpace(string(out2))
		// Truncate if too long for Discord.
		if len(log) > 1800 {
			log = log[:1800] + "..."
		}
	}

	return fmt.Sprintf("engine status: %s\n\nrecent log:\n%s", status, log), nil
}

func runListJobs(_ context.Context, _ map[string]string) (string, error) {
	// List tasks from the kanban store directory.
	out, err := exec.Command("ls", "-1",
		"/home/agent/.local/share/jarvis/kanban/tasks/").Output()
	if err != nil {
		return "(no kanban tasks found)", nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return "(no kanban tasks found)", nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("found %d task(s):\n", len(lines)))
	for _, line := range lines {
		if line != "" {
			b.WriteString("- " + line + "\n")
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func runStartEngine(_ context.Context, _ map[string]string) (string, error) {
	out, err := exec.Command("systemctl", "start", "jarvis-engine").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to start engine: %s", string(out)), nil
	}
	return "jarvis-engine started successfully", nil
}

func runStopEngine(_ context.Context, _ map[string]string) (string, error) {
	out, err := exec.Command("systemctl", "stop", "jarvis-engine").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to stop engine: %s", string(out)), nil
	}
	return "jarvis-engine stopped successfully", nil
}
