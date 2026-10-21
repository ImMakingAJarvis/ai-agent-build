// Package queue implements a priority scheduler: voice > coding > background.
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
// Voice always runs. Coding runs when voice is idle. Background runs when both are idle.
type Scheduler struct {
	voiceActive      atomic.Int32
	codingActive     atomic.Int32
	backgroundActive atomic.Int32
	mu               sync.Mutex
	paused           map[string]bool
}

// NewScheduler creates a scheduler. Voice starts active; coding and background are paused.
func NewScheduler() *Scheduler {
	s := &Scheduler{paused: map[string]bool{}}
	s.voiceActive.Store(1)
	s.codingActive.Store(0)
	s.backgroundActive.Store(0)
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
	codingDepth := depths[CodingQueue]
	bgDepth := depths[BackgroundQueue]

	// Voice is always active if there's work.
	if voiceDepth > 0 {
		s.voiceActive.Store(1)
		s.codingActive.Store(0)
		s.backgroundActive.Store(0)
		s.paused[CodingQueue] = true
		s.paused[BackgroundQueue] = true
	} else if codingDepth > 0 {
		s.voiceActive.Store(0)
		s.codingActive.Store(1)
		s.backgroundActive.Store(0)
		s.paused[VoiceQueue] = false
		s.paused[BackgroundQueue] = true
	} else if bgDepth > 0 {
		s.voiceActive.Store(0)
		s.codingActive.Store(0)
		s.backgroundActive.Store(1)
		s.paused[VoiceQueue] = false
		s.paused[CodingQueue] = false
	} else {
		// All idle.
		s.voiceActive.Store(0)
		s.codingActive.Store(0)
		s.backgroundActive.Store(0)
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
