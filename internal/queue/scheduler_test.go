package queue

import "testing"

// priorityOrder lists the queues from highest to lowest priority. It is the
// contract under test: a queue is unpaused exactly when no higher-priority
// queue has work, so the worker always pulls from the highest non-empty queue.
var priorityOrder = []string{VoiceQueue, FixQueue, CodingQueue, BackgroundQueue}

func TestNewSchedulerInitialPauses(t *testing.T) {
	s := NewScheduler()
	if s.IsPaused(VoiceQueue) {
		t.Error("voice should start unpaused")
	}
	for _, q := range []string{FixQueue, CodingQueue, BackgroundQueue} {
		if !s.IsPaused(q) {
			t.Errorf("queue %s should start paused", q)
		}
	}
}

// TestSetDepthsInvariant checks the full contract for every combination of
// empty/non-empty queues: a queue is paused iff some strictly higher-priority
// queue has work. This is what keeps the highest non-empty queue unpaused.
func TestSetDepthsInvariant(t *testing.T) {
	for mask := 0; mask < 16; mask++ {
		depths := map[string]int{}
		for i, q := range priorityOrder {
			if mask&(1<<i) != 0 {
				depths[q] = 1
			}
		}
		s := NewScheduler()
		s.SetDepths(depths)
		for _, q := range priorityOrder {
			want := false
			for _, h := range priorityOrder {
				if h == q {
					break
				}
				if depths[h] > 0 {
					want = true
					break
				}
			}
			if got := s.IsPaused(q); got != want {
				t.Errorf("mask=%04b queue=%s: IsPaused=%v want %v", mask, q, got, want)
			}
		}
	}
}

// TestSetDepthsFixUnpausesItself is the regression for "the fix queue isn't
// taking priority in the worker": a fix job is the highest-priority pending
// work, but the fix queue starts paused (NewScheduler) and the old SetDepths
// never cleared that pause, so the worker waited the full pause before pulling
// it. The fix queue must be unpaused as soon as it is the highest non-empty queue.
func TestSetDepthsFixUnpausesItself(t *testing.T) {
	s := NewScheduler()
	if !s.IsPaused(FixQueue) {
		t.Fatal("precondition: fix queue starts paused")
	}
	// A fix job is the only pending work.
	s.SetDepths(map[string]int{FixQueue: 1})
	if s.IsPaused(FixQueue) {
		t.Error("fix queue must be unpaused when it is the highest non-empty queue")
	}
	if !s.IsPaused(CodingQueue) || !s.IsPaused(BackgroundQueue) {
		t.Error("coding and background must stay paused while fix has work")
	}
	// A build (coding) job arrives too — fix still wins.
	s.SetDepths(map[string]int{FixQueue: 1, CodingQueue: 1})
	if s.IsPaused(FixQueue) {
		t.Error("fix queue must stay unpaused while coding also has work")
	}
	if !s.IsPaused(CodingQueue) {
		t.Error("coding must be paused while fix has work")
	}
}

// TestSetDepthsVoicePreemptsAll confirms voice holds top priority: while it has
// work, every lower queue is paused and voice itself is never paused.
func TestSetDepthsVoicePreemptsAll(t *testing.T) {
	s := NewScheduler()
	s.SetDepths(map[string]int{VoiceQueue: 1, FixQueue: 1, CodingQueue: 1, BackgroundQueue: 1})
	if s.IsPaused(VoiceQueue) {
		t.Error("voice must never be paused")
	}
	for _, q := range priorityOrder[1:] {
		if !s.IsPaused(q) {
			t.Errorf("queue %s must be paused while voice has work", q)
		}
	}
}

// TestSetDepthsAllIdleUnpausesEverything covers the empty-queue case: with no
// pending work no queue is paused, so a newly arriving job can start.
func TestSetDepthsAllIdleUnpausesEverything(t *testing.T) {
	s := NewScheduler()
	// Start from a fully paused lower tier, then drain to idle.
	s.SetDepths(map[string]int{VoiceQueue: 1, FixQueue: 1, CodingQueue: 1, BackgroundQueue: 1})
	s.SetDepths(map[string]int{})
	for _, q := range priorityOrder {
		if s.IsPaused(q) {
			t.Errorf("queue %s should be unpaused when all queues are idle", q)
		}
	}
}
