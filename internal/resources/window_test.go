package resources

// The window policy (plan §3.6 rules 3–4) as a decision table — the
// acceptance cases of task EVB-27: open only with queued GPU work and no
// running local round; close on drain (after min_window), on max_window,
// and on a brain job's max_brain_wait; hold everywhere else.

import (
	"testing"
	"time"
)

func testWindowCfg() WindowConfig {
	return DefaultWindowConfig() // 2h / 20m / 2m — the plan defaults
}

func TestDecideWindowTable(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		in         WindowInput
		wantAction string
		wantFor    []string // open: the for-list
	}{
		{
			name:       "opens: queued GPU work, no local round (rule 3+4)",
			in:         WindowInput{Now: now, CapDepth: map[string]int{"image.generate": 3}},
			wantAction: WindowOpen,
			wantFor:    []string{"image.generate"},
		},
		{
			name: "opens with several caps: the for-list is sorted",
			in: WindowInput{
				Now:      now,
				CapDepth: map[string]int{"zeta.cap": 1, "image.generate": 2},
			},
			wantAction: WindowOpen,
			wantFor:    []string{"image.generate", "zeta.cap"},
		},
		{
			name:       "does not open: a local round is running (rule 3)",
			in:         WindowInput{Now: now, CapDepth: map[string]int{"image.generate": 5}, RunningLocal: 1},
			wantAction: WindowHold,
		},
		{
			name:       "does not open: nothing queued",
			in:         WindowInput{Now: now, CapDepth: map[string]int{"image.generate": 0}},
			wantAction: WindowHold,
		},
		{
			name: "closes: drained and the window is older than min_window",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-3 * time.Minute),
				Now:        now,
			},
			wantAction: WindowClose,
		},
		{
			name: "holds: drained but younger than min_window (no thrash)",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-30 * time.Second),
				Now:        now,
			},
			wantAction: WindowHold,
		},
		{
			name: "holds: work still queued, inside max_window",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-30 * time.Minute),
				Now:        now,
				CapDepth:   map[string]int{"image.generate": 1},
			},
			wantAction: WindowHold,
		},
		{
			name: "closes: max_window reached even with work still queued",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-2 * time.Hour - time.Second),
				Now:        now,
				CapDepth:   map[string]int{"image.generate": 9},
			},
			wantAction: WindowClose,
		},
		{
			name: "closes: a ready brain job has waited past max_brain_wait",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-10 * time.Minute),
				Now:        now,
				CapDepth:   map[string]int{"image.generate": 2},
				ReadyLocal: []ReadyJob{{ID: "job-42", ReadySince: now.Add(-21 * time.Minute)}},
			},
			wantAction: WindowClose,
		},
		{
			name: "closes on the oldest brain job's wait (not the newest)",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-10 * time.Minute),
				Now:        now,
				ReadyLocal: []ReadyJob{
					{ID: "new", ReadySince: now.Add(-5 * time.Minute)},
					{ID: "old", ReadySince: now.Add(-40 * time.Minute)},
				},
			},
			wantAction: WindowClose,
		},
		{
			name: "holds: brain job ready but its wait is under max_brain_wait",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-10 * time.Minute),
				Now:        now,
				CapDepth:   map[string]int{"image.generate": 1},
				ReadyLocal: []ReadyJob{{ID: "job-7", ReadySince: now.Add(-5 * time.Minute)}},
			},
			wantAction: WindowHold,
		},
		{
			name: "closes: a stale marker without an opened_at",
			in: WindowInput{
				WindowOpen: true,
				Now:        now,
			},
			wantAction: WindowClose,
		},
		{
			name: "holds: open, drained at exactly min_window is old enough",
			in: WindowInput{
				WindowOpen: true,
				OpenedAt:   now.Add(-2 * time.Minute),
				Now:        now,
			},
			wantAction: WindowClose,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideWindow(testWindowCfg(), tt.in)
			if got.Action != tt.wantAction {
				t.Fatalf("action = %q (%s), want %q", got.Action, got.Reason, tt.wantAction)
			}
			if tt.wantAction == WindowOpen {
				if len(got.For) != len(tt.wantFor) {
					t.Fatalf("for = %v, want %v", got.For, tt.wantFor)
				}
				for i := range tt.wantFor {
					if got.For[i] != tt.wantFor[i] {
						t.Fatalf("for = %v, want %v", got.For, tt.wantFor)
					}
				}
			}
		})
	}
}

// TestDecideWindowBrainWaitBeatsDrained: with both a long brain wait and
// a drained queue, the brain-wait reason wins (the job is the reason the
// window ends). The action is close either way; this pins the reason.
func TestDecideWindowBrainWaitBeatsDrained(t *testing.T) {
	now := time.Now()
	got := DecideWindow(testWindowCfg(), WindowInput{
		WindowOpen: true,
		OpenedAt:   now.Add(-30 * time.Minute),
		Now:        now,
		ReadyLocal: []ReadyJob{{ID: "j1", ReadySince: now.Add(-25 * time.Minute)}},
	})
	if got.Action != WindowClose {
		t.Fatalf("action = %q, want close", got.Action)
	}
	if got.Reason != "brain job j1 ready and waited 25m0s (max 20m0s)" {
		t.Errorf("reason = %q, want the brain-wait reason", got.Reason)
	}
}

// TestDefaultWindowConfig: the plan §7.2 defaults (2h / 20m / 2m).
func TestDefaultWindowConfig(t *testing.T) {
	cfg := DefaultWindowConfig()
	if cfg.MaxWindow != 2*time.Hour || cfg.MaxBrainWait != 20*time.Minute || cfg.MinWindow != 2*time.Minute {
		t.Errorf("defaults = %+v, want 2h / 20m / 2m", cfg)
	}
}
