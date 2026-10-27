// Package queue implements a priority scheduler: voice > fix > coding > background.
//
// The scheduler does not consume messages itself; it tells the consumer which
// queues are active. The consumer only pulls from the highest-priority
// non-empty queue. When a lower-priority queue is active and a higher-priority
// message arrives, the consumer pauses the lower queue.
package queue

import (
	"log"
	"sync"
	"sync/atomic"
)

// Scheduler tracks which queues are allowed to process jobs.
// Voice always runs. Fix runs when voice is idle. Coding runs when voice and fix are idle. Background runs when all are idle.
type Scheduler struct {
	voiceActive      atomic.Int32
	fixActive        atomic.Int32
	codingActive     atomic.Int32
	backgroundActive atomic.Int32
	mu               sync.Mutex
	paused           map[string]bool
}

// NewScheduler creates a scheduler. Voice starts active; fix, coding and background are paused.
func NewScheduler() *Scheduler {
	s := &Scheduler{paused: map[string]bool{}}
	s.voiceActive.Store(1)
	s.fixActive.Store(0)
	s.codingActive.Store(0)
	s.backgroundActive.Store(0)
	s.paused[FixQueue] = true
	s.paused[CodingQueue] = true
	s.paused[BackgroundQueue] = true
	return s
}

// QueueDepthCallback is called by the consumer to report how many messages
// are waiting in a queue. The scheduler uses this to decide when to
// activate a lower-priority queue.
type QueueDepthCallback func(string) int

// SetDepths updates the scheduler with current queue depths and adjusts
// which queues are active.
func (s *Scheduler) SetDepths(depths map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	voiceDepth := depths[VoiceQueue]
	fixDepth := depths[FixQueue]
	codingDepth := depths[CodingQueue]
	bgDepth := depths[BackgroundQueue]

	// A queue is paused while any strictly higher-priority queue still has
	// work (voice > fix > coding > background). Setting each flag from the
	// depths (rather than only the higher queues) guarantees the
	// highest-priority non-empty queue is always unpaused: previously the
	// active queue was left in its prior state, so a queue that started
	// paused (as NewScheduler leaves fix/coding/background) stayed paused
	// and its jobs waited out the full pause before the worker pulled them.
	s.paused[VoiceQueue] = false
	s.paused[FixQueue] = voiceDepth > 0
	s.paused[CodingQueue] = voiceDepth > 0 || fixDepth > 0
	s.paused[BackgroundQueue] = voiceDepth > 0 || fixDepth > 0 || codingDepth > 0

	// Name the single highest-priority queue that currently has work.
	s.voiceActive.Store(0)
	s.fixActive.Store(0)
	s.codingActive.Store(0)
	s.backgroundActive.Store(0)
	switch {
	case voiceDepth > 0:
		s.voiceActive.Store(1)
	case fixDepth > 0:
		s.fixActive.Store(1)
	case codingDepth > 0:
		s.codingActive.Store(1)
	case bgDepth > 0:
		s.backgroundActive.Store(1)
	}
}

// IsPaused returns whether the consumer should skip a queue.
func (s *Scheduler) IsPaused(queue string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused[queue]
}

// JobStarted signals that a job has begun on the given queue.
func (s *Scheduler) JobStarted(queue string) {
	log.Printf("scheduler: job started on %s", queue)
}

// JobFinished signals that a job on the given queue completed.
// The scheduler re-evaluates priorities.
func (s *Scheduler) JobFinished(queue string) {
	log.Printf("scheduler: job finished on %s — re-evaluating priorities", queue)
}
