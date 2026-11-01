// plane-epic creates a new epic board — the board standard (one Plane
// project per epic, see docs/work-board/NOTES.md):
//
//  1. the project (created with the given identifier; Plane does not
//     auto-generate identifiers)
//  2. the standard board: 8 states + the "epic" label
//  3. every workspace member added as an editor (viewers cannot move
//     items)
//  4. the epic item itself (Backlog, epic label)
//  5. PLANE_PROJECTS in the agent env file gains the identifier, and
//     plane-bridge is restarted so it watches the new project
//
// Usage:
//
//	plane-epic create <title> --identifier XXX [--description ...]
//	                          [--no-env] [--no-restart]
//
// Re-running with the same title/identifier is a no-op for everything
// that already exists. Config comes from the PLANE_* environment,
// falling back to ~/.config/jarvis/agent.env via plane.FromEnv.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planeepic"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "create" {
		usage()
	}
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	identifier := fs.String("identifier", "", "project identifier (required, e.g. T2I)")
	description := fs.String("description", "", "epic item description")
	noEnv := fs.Bool("no-env", false, "do not update PLANE_PROJECTS in the env file")
	noRestart := fs.Bool("no-restart", false, "do not restart plane-bridge")
	fs.Parse(os.Args[2:])
	if fs.NArg() != 1 || *identifier == "" {
		usage()
	}
	title := fs.Arg(0)
	if err := run(title, *identifier, *description, !*noEnv, !*noRestart); err != nil {
		fmt.Fprintln(os.Stderr, "plane-epic:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  plane-epic create <title> --identifier XXX [--description ...]
                           [--no-env] [--no-restart]`)
	os.Exit(1)
}

func run(title, identifier, description string, updateEnv, restart bool) error {
	c, err := plane.FromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	project, err := planeepic.EnsureProject(ctx, c, title, identifier)
	if err != nil {
		return err
	}
	fmt.Printf("project: %s (%s) id=%s\n", project.Name, project.Identifier, project.ID)

	if err := planeepic.EnsureBoard(ctx, c, project.ID); err != nil {
		return err
	}
	fmt.Println("board: standard states + epic label ensured")

	// Everyone in the workspace can edit the new board.
	wsMembers, err := c.ListWorkspaceMembers(ctx)
	if err != nil {
		return err
	}
	var emails []string
	for _, m := range wsMembers {
		emails = append(emails, m.Email)
	}
	added, skipped, err := planeepic.EnsureMembers(ctx, c, project.ID, emails)
	if err != nil {
		return err
	}
	fmt.Printf("members: %d editor(s) added", len(added))
	if len(skipped) > 0 {
		fmt.Printf(" (%d skipped, not workspace members: %s)", len(skipped), strings.Join(skipped, ", "))
	}
	fmt.Println()

	// The epic item: Backlog + epic label, so the board's plan_epic
	// trigger works on it like any other epic.
	epic, err := ensureEpicItem(ctx, c, project, title, description)
	if err != nil {
		return err
	}
	fmt.Printf("epic item: %s %s\n", project.Identifier+"-"+fmt.Sprint(epic.SequenceID), c.ItemLink(project, epic))

	if updateEnv {
		if err := addProjectToEnvFile(identifier); err != nil {
			return err
		}
		fmt.Println("env: PLANE_PROJECTS updated in", envFilePath())
	}
	if restart {
		if err := restartBridge(); err != nil {
			return fmt.Errorf("restarting plane-bridge (env updated; restart manually): %w", err)
		}
		fmt.Println("plane-bridge restarted")
	}
	return nil
}

// ensureEpicItem finds an existing epic item with the same name
// (case-insensitive, epic label set) or creates it.
func ensureEpicItem(ctx context.Context, c *plane.Client, p *plane.Project, title, description string) (*plane.WorkItem, error) {
	epicLabel, err := c.LabelByName(ctx, p.ID, planeepic.EpicLabelName)
	if err != nil {
		return nil, err
	}
	backlog, err := c.StateByName(ctx, p.ID, "Backlog")
	if err != nil {
		return nil, err
	}
	items, err := c.ListWorkItems(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if !strings.EqualFold(items[i].Name, title) {
			continue
		}
		for _, l := range items[i].Labels {
			if l == epicLabel.ID {
				return &items[i], nil
			}
		}
	}
	wi := plane.WorkItemCreate{Name: title, State: backlog.ID, Labels: []string{epicLabel.ID}}
	if strings.TrimSpace(description) != "" {
		wi.DescriptionHTML = "<p>" + strings.TrimSpace(description) + "</p>"
	}
	return c.CreateWorkItem(ctx, p.ID, wi)
}

// --- env file + service -------------------------------------------------------------

func envFilePath() string {
	if p := strings.TrimSpace(os.Getenv("PLANE_ENV_FILE")); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.config/jarvis/agent.env"
}

// addProjectToEnvFile appends the identifier to the PLANE_PROJECTS value
// in the agent env file (creating the line if missing).
func addProjectToEnvFile(identifier string) error {
	path := envFilePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading env file: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	found := false
	for i, line := range lines {
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "PLANE_PROJECTS" {
			continue
		}
		found = true
		parts := strings.Split(val, ",")
		for _, part := range parts {
			if strings.TrimSpace(part) == identifier {
				return nil // already watching it
			}
		}
		if strings.TrimSpace(val) == "" {
			lines[i] = "PLANE_PROJECTS=" + identifier
		} else {
			lines[i] = "PLANE_PROJECTS=" + strings.TrimRight(val, ",") + "," + identifier
		}
		break
	}
	if !found {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "PLANE_PROJECTS="+identifier)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing env file: %w", err)
	}
	return nil
}

// restartBridge restarts plane-bridge (plain systemctl first, sudo as
// the fallback for machines where the agent cannot manage the unit
// directly).
func restartBridge() error {
	if _, err := exec.Command("systemctl", "restart", "plane-bridge").CombinedOutput(); err == nil {
		return nil
	}
	out, err := exec.Command("sudo", "-n", "systemctl", "restart", "plane-bridge").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
