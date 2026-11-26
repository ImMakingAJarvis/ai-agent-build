// Package slot decides which engine slot a job runs in, and when a job's
// KV cache is saved to a snapshot or restored from one (ELR-1, M2.3,
// cards ELR-21/ELR-22).
//
// The Manager keeps, under one mutex, one record per configured slot:
// the job currently RUNNING a tick in it (holder, "" if none), the job
// whose KV cache is still in it (warm, "" if none), the tokens that
// cache holds, and a lastReleased counter that increases on every
// Release. The rules the tests pin:
//
//   - A job that has a warm slot still holding its KV cache gets it back
//     with Warm true (no save, no restore, no engine calls at all).
//   - A job with no warm slot takes a cold free slot when one exists
//     (lowest number first).
//   - When every slot is warm for other jobs, the manager evicts the
//     least recently released one; a job whose cache holds at least
//     SnapshotMinTokens is saved to a snapshot named "<jobID>.bin" (with
//     the engine's current fingerprint) before its slot is taken.
//   - A job that acquires a slot and has a snapshot whose fingerprint
//     matches the engine's current model gets it restored, with
//     Restored true; a mismatched snapshot is dropped, not restored.
//   - With no free slot and nothing evictable, Acquire returns
//     ErrNoSlot.
//
// The engine and jobs calls run while holding the mutex: ticks are long
// and acquires are rare, so the simple-and-correct order wins.
package slot

import (
	"context"
	"errors"
	"sync"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// Config names the slots this manager may use and the smallest KV cache
// worth saving: a job released with fewer tokens than SnapshotMinTokens
// is evicted without a snapshot, because re-prefilling it is cheaper
// than saving and restoring it.
type Config struct {
	Slots             []int
	SnapshotMinTokens int
}

// JobState is what the job store records about a job's slot history:
// whether it is running now, the tokens its KV cache holds, and the
// snapshot it can be restored from ("" if none) with the fingerprint of
// the model that wrote it.
type JobState struct {
	ID                  string
	Running             bool
	Tokens              int
	Snapshot            string
	SnapshotFingerprint string
}

// Jobs is the job store the slot manager reads and writes.
type Jobs interface {
	// Get returns the job's recorded state.
	Get(id string) (JobState, error)
	// SetSnapshot records the snapshot file, the fingerprint of the
	// model that wrote it, and the tokens it holds.
	SetSnapshot(id, file, fingerprint string, tokens int) error
	// SetWarmSlot records the slot the job's KV cache is currently warm
	// in; a nil pointer clears it.
	SetWarmSlot(id string, slot *int) error
}

// Lease is a job's hold on a slot for one run.
type Lease struct {
	JobID    string
	Slot     int
	Warm     bool // the slot already held the job's KV cache
	Restored bool // the slot was refilled from the job's snapshot
}

// ErrNoSlot is returned by Acquire when every slot the manager may use
// is held by a running job and none can be evicted.
var ErrNoSlot = errors.New("slot: none free")

// slotRecord is the manager's bookkeeping for one engine slot.
type slotRecord struct {
	num          int
	holder       string // job RUNNING a tick in the slot, "" if none
	warm         string // job whose KV cache is in the slot, "" if none
	tokens       int    // tokens the warm cache holds
	lastReleased int    // release counter value of the last Release
}

// Manager assigns jobs to slots.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	eng      engine.Engine
	jobs     Jobs
	slots    []*slotRecord
	releases int // increases on every Release
}

// New returns a Manager for cfg's slots, backed by eng and jobs.
func New(cfg Config, eng engine.Engine, jobs Jobs) *Manager {
	m := &Manager{cfg: cfg, eng: eng, jobs: jobs}
	m.slots = make([]*slotRecord, len(cfg.Slots))
	for i, n := range cfg.Slots {
		m.slots[i] = &slotRecord{num: n}
	}
	return m
}

// Acquire assigns a slot to jobID, restoring its snapshot when one is
// saved and its fingerprint matches the engine's current model.
func (m *Manager) Acquire(ctx context.Context, jobID string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var (
		s       *slotRecord
		lease   Lease
		evicted bool
	)

	// 1. The job's own warm slot, still free: it already holds the job's
	// KV cache, so the lease is warm and costs no engine calls.
	for _, st := range m.slots {
		if st.holder == "" && st.warm == jobID {
			s = st
			lease = Lease{JobID: jobID, Slot: st.num, Warm: true}
			break
		}
	}

	// 2. Else a cold slot, lowest number first.
	if s == nil {
		for _, st := range m.slots {
			if st.holder == "" && st.warm == "" {
				s = st
				break
			}
		}
	}

	// 3. Else evict the least recently released free slot.
	if s == nil {
		for _, st := range m.slots {
			if st.holder != "" {
				continue
			}
			if s == nil || st.lastReleased < s.lastReleased {
				s = st
			}
		}
		if s == nil {
			return Lease{}, ErrNoSlot
		}
		evicted = true
		w := s.warm
		if s.tokens >= m.cfg.SnapshotMinTokens {
			info, err := m.eng.Info(ctx)
			if err != nil {
				return Lease{}, err
			}
			if _, err := m.eng.SaveSlot(ctx, s.num, w+".bin"); err != nil {
				return Lease{}, err
			}
			if err := m.jobs.SetSnapshot(w, w+".bin", info.Fingerprint(), s.tokens); err != nil {
				return Lease{}, err
			}
		}
		if err := m.jobs.SetWarmSlot(w, nil); err != nil {
			return Lease{}, err
		}
		s.warm = ""
	}

	if !lease.Warm {
		// A slot taken in step 2 or 3 is a cold lease; its Restored
		// flag is set below when the job's snapshot comes back.
		lease = Lease{JobID: jobID, Slot: s.num}
	}
	if !lease.Warm {
		// A job the store has not recorded yet (a fresh job) has no
		// snapshot: a Get failure is the empty state, not a failure.
		js, err := m.jobs.Get(jobID)
		if err != nil {
			js = JobState{}
		}
		if js.Snapshot != "" {
			info, err := m.eng.Info(ctx)
			if err != nil {
				return Lease{}, err
			}
			if js.SnapshotFingerprint == info.Fingerprint() {
				if err := m.eng.RestoreSlot(ctx, s.num, js.Snapshot); err != nil {
					return Lease{}, err
				}
				lease.Restored = true
			} else {
				// Stale: the tick will re-read the prompt, so drop the
				// snapshot from the store.
				if err := m.jobs.SetSnapshot(jobID, "", "", 0); err != nil {
					return Lease{}, err
				}
			}
		} else if evicted {
			// The evicted cache was too small to save, so the slot still
			// holds it: drop it before the new job's tick.
			if err := m.eng.EraseSlot(ctx, s.num); err != nil {
				return Lease{}, err
			}
		}
	}

	// Record the warm slot before taking the holder, so a jobs error
	// leaves the slot with holder == "".
	num := s.num
	if err := m.jobs.SetWarmSlot(jobID, &num); err != nil {
		return Lease{}, err
	}
	s.holder = jobID
	s.warm = jobID
	return lease, nil
}

// Release returns a slot to the pool, reporting the tokens the job's KV
// cache holds when the lease ends. The job stays warm in the slot.
func (m *Manager) Release(l Lease, tokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.slots {
		if st.num != l.Slot {
			continue
		}
		st.holder = ""
		st.tokens = tokens
		m.releases++
		st.lastReleased = m.releases
		return
	}
}

// Forget drops the manager's record of jobID's warm slot, in every slot
// that is not running a tick: the slot is no longer offered to the job,
// and its next eviction treats the cache as any other's.
func (m *Manager) Forget(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.slots {
		if st.holder == "" && st.warm == jobID {
			st.warm = ""
		}
	}
}

// Warm maps each slot the manager holds to the jobID whose KV cache is
// warm in it.
func (m *Manager) Warm() map[int]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]string, len(m.slots))
	for _, st := range m.slots {
		if st.warm != "" {
			out[st.num] = st.warm
		}
	}
	return out
}
