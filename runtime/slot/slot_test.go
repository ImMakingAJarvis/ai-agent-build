// Tests for the slot manager (ELR-1, M2.3a, card ELR-21). They are
// written before the implementation and stay skipped until card M2.3b
// makes them pass: every test starts with t.Skip("enabled by card
// m2-3b"). SnapshotMinTokens is 1000 in every test unless said
// otherwise.
//
// The tests run on the fake engine (runtime/engine/fake) and a small
// in-memory Jobs. A fake slot's KV cache only gains content through a
// scripted Chat or a RestoreSlot, so the tests that assert on what the
// engine saved or restored drive one scripted turn per job: that is what
// makes "the engine restored A.bin" and "no restore call" observable.
package slot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
)

// memJobs is the small in-memory Jobs the tests use: it records each
// job's snapshot and warm slot as the manager writes them.
type memJobs struct {
	mu   sync.Mutex
	st   map[string]JobState
	warm map[string]*int
}

func newMemJobs() *memJobs {
	return &memJobs{st: map[string]JobState{}, warm: map[string]*int{}}
}

func (j *memJobs) Get(id string) (JobState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	st, ok := j.st[id]
	if !ok {
		return JobState{}, fmt.Errorf("memJobs: no job %q", id)
	}
	return st, nil
}

func (j *memJobs) SetSnapshot(id, file, fingerprint string, tokens int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := j.st[id]
	st.ID = id
	st.Snapshot = file
	st.SnapshotFingerprint = fingerprint
	st.Tokens = tokens
	j.st[id] = st
	return nil
}

func (j *memJobs) SetWarmSlot(id string, sp *int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.warm[id] = sp
	return nil
}

// snapshotOf returns the snapshot the manager recorded for the job: the
// file name, the fingerprint it was saved with and the tokens it holds.
func (j *memJobs) snapshotOf(id string) (file, fingerprint string, tokens int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := j.st[id]
	return st.Snapshot, st.SnapshotFingerprint, st.Tokens
}

// warmSlotOf returns the slot the manager recorded as warm for the job,
// or nil if it recorded none.
func (j *memJobs) warmSlotOf(id string) *int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.warm[id]
}

// chat runs one scripted turn of key in slot and drains the stream, so
// the fake's slot holds key's prompt+output tokens afterwards.
func chat(t *testing.T, eng *fake.Engine, ctx context.Context, slot int, key string, prompt, output int) {
	t.Helper()
	eng.Script(key, fake.Step{
		Reply:        engine.Message{Role: "assistant", Content: "ok"},
		PromptTokens: prompt,
		OutputTokens: output,
	})
	s, err := eng.Chat(ctx, engine.ChatRequest{Slot: slot, Messages: []engine.Message{{Role: "user", Content: key}}})
	if err != nil {
		t.Fatalf("Chat(%q): %v", key, err)
	}
	defer s.Close()
	for {
		ch, err := s.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("Recv(%q): %v", key, err)
		}
		if ch.Err != nil {
			t.Fatalf("Recv(%q): chunk error %v", key, ch.Err)
		}
	}
}

// fingerprintOf is the fingerprint of the model the fake currently reports.
func fingerprintOf(t *testing.T, eng *fake.Engine) string {
	t.Helper()
	info, err := eng.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	return info.Fingerprint()
}

// TestAcquireCold: one slot, job A never ran: a cold lease in slot 0.
func TestAcquireCold(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)

	l, err := m.Acquire(context.Background(), "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	want := Lease{JobID: "A", Slot: 0}
	if l != want {
		t.Errorf("Acquire(A) = %+v, want %+v (cold)", l, want)
	}
}

// TestAcquireWarm: A acquires, releases with 5000 tokens, acquires
// again: warm again, and the engine saved and restored nothing.
func TestAcquireWarm(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	m.Release(l, 5000)

	l2, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A) again: %v", err)
	}
	if !l2.Warm {
		t.Errorf("second Acquire(A): Warm = false, want true")
	}
	if saved := eng.Saved(); len(saved) != 0 {
		t.Errorf("engine saved %v, want no save", saved)
	}
	tok, _ := eng.SlotTokens(0)
	key, _ := eng.SlotKey(0)
	if key != "" || tok != 0 {
		t.Errorf("engine slot 0 holds %q/%d, want empty (no restore)", key, tok)
	}
}

// TestAcquireBusy: A holds the only slot and never releases it; B gets
// ErrNoSlot.
func TestAcquireBusy(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	if l.Slot != 0 {
		t.Fatalf("Acquire(A): Slot = %d, want 0", l.Slot)

	}

	if _, err := m.Acquire(ctx, "B"); !errors.Is(err, ErrNoSlot) {
		t.Errorf("Acquire(B) err = %v, want ErrNoSlot", err)
	}
}

// TestEvictSaves: A releases with 5000 tokens; B takes the slot: A is
// evicted, its snapshot "A.bin" is saved with the engine's fingerprint
// and 5000 tokens, and A's warm slot is cleared.
func TestEvictSaves(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	m.Release(l, 5000)

	if _, err := m.Acquire(ctx, "B"); err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}

	saved := eng.Saved()
	if len(saved) != 1 {
		t.Fatalf("engine saved %v, want exactly one snapshot", saved)
	}
	if _, ok := saved["A.bin"]; !ok {
		t.Errorf("saved snapshots %v, want one named %q", saved, "A.bin")
	}

	file, snapFP, tokens := jobs.snapshotOf("A")
	wantFP := fingerprintOf(t, eng)
	if file != "A.bin" || snapFP != wantFP || tokens != 5000 {
		t.Errorf("jobs snapshot for A = (%q, %q, %d), want (%q, %q, 5000)", file, snapFP, tokens, "A.bin", wantFP)
	}
	if sp := jobs.warmSlotOf("A"); sp != nil {
		t.Errorf("A's warm slot = %d, want nil", *sp)
	}
}

// TestEvictSmallDoesNotSave: A releases with 500 tokens (< 1000); B
// takes the slot: nothing is saved and A has no snapshot.
func TestEvictSmallDoesNotSave(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	m.Release(l, 500)

	if _, err := m.Acquire(ctx, "B"); err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}

	if saved := eng.Saved(); len(saved) != 0 {
		t.Errorf("engine saved %v, want no save", saved)
	}
	file, fp, _ := jobs.snapshotOf("A")
	if file != "" || fp != "" {
		t.Errorf("jobs snapshot for A = (%q, %q), want none", file, fp)
	}
}

// TestRestore: A releases with 5000 tokens; B takes the slot and then
// releases; A acquires again: its snapshot "A.bin" is restored into the
// slot.
func TestRestore(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	chat(t, eng, ctx, l.Slot, "A", 4000, 1000) // A's 5000 tokens into the slot
	m.Release(l, 5000)

	lb, err := m.Acquire(ctx, "B")
	if err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}
	chat(t, eng, ctx, lb.Slot, "B", 400, 100) // B's 500 tokens: the slot now holds B
	m.Release(lb, 500)

	l2, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A) again: %v", err)
	}
	if !l2.Restored {
		t.Errorf("second Acquire(A): Restored = false, want true")
	}
	tok, _ := eng.SlotTokens(l2.Slot)
	key, _ := eng.SlotKey(l2.Slot)
	if key != "A" || tok != 5000 {
		t.Errorf("engine slot %d holds %q/%d after Acquire, want %q/5000 (A.bin restored)", l2.Slot, key, tok, "A")
	}
}

// TestRestoreFingerprintMismatch: as TestRestore, but the engine's
// model changes (different ContextTokens) before A acquires again: the
// snapshot is stale, so it is not restored and is cleared from Jobs.
func TestRestoreFingerprintMismatch(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	chat(t, eng, ctx, l.Slot, "A", 4000, 1000)
	m.Release(l, 5000)

	lb, err := m.Acquire(ctx, "B")
	if err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}
	chat(t, eng, ctx, lb.Slot, "B", 400, 100)
	m.Release(lb, 500)

	eng.SetInfo(engine.ModelInfo{ID: "fake", ContextTokens: 32768})

	l2, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A) again: %v", err)
	}
	if l2.Restored {
		t.Errorf("second Acquire(A): Restored = true, want false (stale fingerprint)")
	}
	tok, _ := eng.SlotTokens(l2.Slot)
	key, _ := eng.SlotKey(l2.Slot)
	if key != "B" || tok != 500 {
		t.Errorf("engine slot %d holds %q/%d, want %q/500 (no restore call)", l2.Slot, key, tok, "B")
	}
	file, fp, _ := jobs.snapshotOf("A")
	if file != "" || fp != "" {
		t.Errorf("jobs snapshot for A = (%q, %q), want cleared", file, fp)
	}
}

// TestPrefersColdSlot: two slots, A warm in slot 0; B acquires: it gets
// the cold slot 1 and A's snapshot is not saved.
func TestPrefersColdSlot(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(2)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0, 1}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	if l.Slot != 0 {
		t.Fatalf("Acquire(A): Slot = %d, want 0", l.Slot)
	}
	m.Release(l, 5000) // A is warm in slot 0, its cache savable

	lb, err := m.Acquire(ctx, "B")
	if err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}
	if lb.Slot != 1 {
		t.Errorf("Acquire(B): Slot = %d, want the cold slot 1", lb.Slot)
	}
	if saved := eng.Saved(); len(saved) != 0 {
		t.Errorf("engine saved %v, want nothing saved", saved)
	}
}

// TestEvictsLeastRecentlyUsed: two slots, A then B released; C acquires:
// it evicts A, which was released first.
func TestEvictsLeastRecentlyUsed(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(2)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0, 1}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	la, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	lb, err := m.Acquire(ctx, "B")
	if err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}
	m.Release(la, 5000) // A released first: it is the least recently used
	m.Release(lb, 500)

	lc, err := m.Acquire(ctx, "C")
	if err != nil {
		t.Fatalf("Acquire(C): %v", err)
	}
	if lc.Slot != la.Slot {
		t.Errorf("Acquire(C): Slot = %d, want A's evicted slot %d", lc.Slot, la.Slot)
	}
	saved := eng.Saved()
	if len(saved) != 1 {
		t.Fatalf("engine saved %v, want A's snapshot", saved)
	}
	if _, ok := saved["A.bin"]; !ok {
		t.Errorf("saved snapshots %v, want one named %q", saved, "A.bin")
	}
}

// TestForget: A releases, then Forget("A"): Warm() no longer lists A.
func TestForget(t *testing.T) {
	t.Skip("enabled by card m2-3b")
	eng := fake.New(1)
	jobs := newMemJobs()
	m := New(Config{Slots: []int{0}, SnapshotMinTokens: 1000}, eng, jobs)
	ctx := context.Background()

	l, err := m.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	m.Release(l, 5000)

	m.Forget("A")

	for s, id := range m.Warm() {
		if id == "A" {
			t.Errorf("Warm() still lists A in slot %d", s)
		}
	}
}
