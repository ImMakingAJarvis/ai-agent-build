// Package slot decides which engine slot a job runs in, and when a job's
// KV cache is saved to a snapshot or restored from one (ELR-1, M2.3a,
// card ELR-21).
//
// This is the API stub: the behaviour is pinned by the (currently
// skipped) tests in slot_test.go and implemented by card M2.3b. The
// rules those tests pin:
//
//   - A job that has a warm slot still holding its KV cache gets it back
//     with Warm true (no save, no restore).
//   - A job with no warm slot takes a cold free slot when one exists.
//   - When every slot is warm for other jobs, the manager evicts the
//     least recently released one; a job whose cache holds at least
//     SnapshotMinTokens is saved to a snapshot named "<jobID>.bin" (with
//     the engine's current fingerprint) before its slot is taken.
//   - A job that acquires a slot and has a snapshot whose fingerprint
//     matches the engine's current model gets it restored, with
//     Restored true; a mismatched snapshot is dropped, not restored.
//   - With no free slot and nothing evictable, Acquire returns
//     ErrNoSlot.
package slot

import (
	"context"
	"errors"

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

// Manager assigns jobs to slots.
type Manager struct {
	/* unexported */
}

// New returns a Manager for cfg's slots, backed by eng and jobs.
func New(cfg Config, eng engine.Engine, jobs Jobs) *Manager {
	return &Manager{}
}

// Acquire assigns a slot to jobID, restoring its snapshot when one is
// saved and its fingerprint matches the engine's current model.
// (M2.3b)
func (m *Manager) Acquire(ctx context.Context, jobID string) (Lease, error) {
	return Lease{}, errors.New("not implemented")
}

// Release returns a slot to the pool, reporting the tokens the job's KV
// cache holds when the lease ends. (M2.3b)
func (m *Manager) Release(l Lease, tokens int) {}

// Forget drops the manager's records of jobID: its warm slot and its
// snapshot. (M2.3b)
func (m *Manager) Forget(jobID string) {}

// Warm maps each slot the manager holds to the jobID whose KV cache is
// warm in it. (M2.3b)
func (m *Manager) Warm() map[int]string { return nil }
