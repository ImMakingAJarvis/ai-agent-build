// Package planlabels keeps the Plane `plan:<model>` labels in sync with
// the model registry (Phase A3 of docs/work-board/PLAN.md): one label per
// enabled planner model, created when a model is enabled and removed when
// it is disabled. Labels are per-project in Plane, so Sync runs against
// the set of projects the caller owns — for plane-bridge, the watched
// projects. The bridge runs it at start and on every change to
// config/models.yaml (see the Watcher).
package planlabels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// Prefix marks the labels this package owns; Sync leaves every other
// label (epic, model:, run:, ...) alone.
const Prefix = "plan:"

// Color is what new plan: labels get — the Planning column's sky.
const Color = "#0EA5E9"

// LabelName is the Plane label name for a model id.
func LabelName(modelID string) string { return Prefix + modelID }

// Sync makes the plan: labels of every project match the enabled planner
// models in f: missing labels are created, plan: labels whose model is no
// longer an enabled planner are removed (work items carrying one lose the
// label, as Plane drops the association). It is idempotent — a run against
// an already-synced board changes nothing.
func Sync(ctx context.Context, c *plane.Client, projectIDs []string, f *registry.File, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	planners, err := f.ByRole("planner")
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(planners))
	for _, m := range planners {
		want[LabelName(m.ID)] = true
	}
	for _, projectID := range projectIDs {
		labels, err := c.ListLabels(ctx, projectID)
		if err != nil {
			return fmt.Errorf("planlabels: listing labels: %w", err)
		}
		present := make(map[string]string, len(labels)) // plan: label name → id
		for _, l := range labels {
			if strings.HasPrefix(l.Name, Prefix) {
				present[l.Name] = l.ID
			}
		}
		// Remove labels whose model is no longer an enabled planner.
		for _, l := range labels {
			if !strings.HasPrefix(l.Name, Prefix) || want[l.Name] {
				continue
			}
			if err := c.DeleteLabel(ctx, projectID, l.ID); err != nil {
				return fmt.Errorf("planlabels: deleting stale label %q: %w", l.Name, err)
			}
			log.Info("plan label removed (model no longer an enabled planner)",
				"project", projectID, "label", l.Name)
		}
		// Create the missing ones, in registry file order.
		for _, m := range planners {
			name := LabelName(m.ID)
			if _, ok := present[name]; ok {
				continue
			}
			if _, err := c.CreateLabel(ctx, projectID, name, Color); err != nil {
				return fmt.Errorf("planlabels: creating label %q: %w", name, err)
			}
			log.Info("plan label added (model enabled as planner)",
				"project", projectID, "label", name)
		}
	}
	return nil
}

// SyncFunc applies one registry file to the board.
type SyncFunc func(ctx context.Context, f *registry.File) error

// Watcher watches a registry file and keeps the board in sync with it: it
// syncs the file on the first check, re-syncs on every file change (mtime
// or size), and retries the last good file while a sync is failing — so a
// Plane outage at boot, or a failed sync that the file change that caused
// it never repeated, does not stall the sync. A missing or unparseable
// file is logged and kept out of the sync (the last good state stays
// applied) without aborting the watch.
type Watcher struct {
	Path     string        // the registry file (config/models.yaml)
	Interval time.Duration // poll period
	Log      *slog.Logger
	Sync     SyncFunc

	fi      os.FileInfo // last file that was loaded (nil = none yet)
	missing bool        // file absent at the last check (warned once)
	failing bool        // last sync failed; the next check retries
}

// NewWatcher builds a Watcher; interval <= 0 defaults to 15 s.
func NewWatcher(path string, interval time.Duration, log *slog.Logger, sync SyncFunc) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &Watcher{Path: path, Interval: interval, Log: log, Sync: sync}
}

// Run checks the file immediately and then on every tick until ctx is
// done. File and sync problems are logged and retried, never returned;
// Run returns ctx.Err() when the context finishes.
func (w *Watcher) Run(ctx context.Context) error {
	w.check(ctx, true)
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.check(ctx, false)
		}
	}
}

// check reloads the file when it changed (or force is set) and syncs it
// when a sync is due — a change, or a previously failed sync.
func (w *Watcher) check(ctx context.Context, force bool) {
	fi, err := os.Stat(w.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if !w.missing {
				w.missing = true
				w.Log.Warn("plan labels: models file missing, waiting for it", "path", w.Path)
			}
		} else {
			w.Log.Warn("plan labels: stat models file failed", "path", w.Path, "err", err)
		}
		return
	}
	w.missing = false
	changed := force || w.fi == nil || fi.ModTime() != w.fi.ModTime() || fi.Size() != w.fi.Size()
	if !changed && !w.failing {
		return
	}
	data, err := os.ReadFile(w.Path)
	if err != nil {
		w.Log.Warn("plan labels: reading models file failed", "path", w.Path, "err", err)
		return
	}
	f, err := registry.Parse(data)
	if err != nil {
		w.Log.Warn("plan labels: models file invalid, keeping the last good sync",
			"path", w.Path, "err", err)
		return
	}
	w.fi = fi
	if ctx.Err() != nil {
		return
	}
	w.failing = false
	if err := w.Sync(ctx, f); err != nil {
		w.failing = true
		w.Log.Warn("plan label sync failed, will retry", "err", err)
	} else {
		w.Log.Info("plan label sync done", "path", w.Path)
	}
}
