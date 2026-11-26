// Package fake is a scripted, in-memory engine.Engine for tests (ELR-1,
// M2.2, card ELR-20). It needs no GPU and no network: a test scripts the
// replies for a conversation key, and the engine replays them on a
// simulated clock, so every later card can be tested fast and exactly.
//
// A conversation's key is the Content of the FIRST message with role
// "user" in the ChatRequest. Chat pops the next scripted Step for that
// key, advances the simulated clock by the Step's Delay (it never
// sleeps), records the call on the timeline, and sets the slot's key
// and tokens. The returned Stream yields one text chunk (the reply
// content) and then the Done chunk; a Step whose Err is set yields a
// single chunk with that error and no Done chunk.
package fake

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// Step is one scripted reply of a conversation.
type Step struct {
	Reply        engine.Message
	PromptTokens int
	OutputTokens int
	Delay        time.Duration
	Err          error
}

// Use is one Chat call, in the order the calls were made.
type Use struct {
	Slot  int
	Key   string
	Start time.Time
	End   time.Time
}

// savedSlot is what a snapshot holds: the slot's key and tokens.
type savedSlot struct {
	key    string
	tokens int
}

// Engine is a scripted engine for tests. It is safe for concurrent use.
type Engine struct {
	mu       sync.Mutex
	slots    int
	now      time.Time
	queues   map[string][]Step
	timeline []Use
	slotKey  []string
	slotTok  []int
	saved    map[string]savedSlot
	info     engine.ModelInfo
}

// New returns an Engine with the given number of slots. The simulated
// clock starts at 2026-01-01 00:00 UTC.
func New(slots int) *Engine {
	return &Engine{
		slots:   slots,
		now:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		queues:  map[string][]Step{},
		slotKey: make([]string, slots),
		slotTok: make([]int, slots),
		saved:   map[string]savedSlot{},
		info:    engine.ModelInfo{ID: "fake", ContextTokens: 131072},
	}
}

// Script queues the given replies for the conversation with the given
// key. Chats pop them in order.
func (e *Engine) Script(key string, steps ...Step) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queues[key] = append(e.queues[key], steps...)
}

// Now returns the simulated clock.
func (e *Engine) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

// Advance moves the simulated clock forward by d.
func (e *Engine) Advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

// Timeline returns every Chat call so far, in order.
func (e *Engine) Timeline() []Use {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Use, len(e.timeline))
	copy(out, e.timeline)
	return out
}

// Saved returns a snapshot name -> its tokens, for the SaveSlot calls
// made so far.
func (e *Engine) Saved() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.saved))
	for name, s := range e.saved {
		out[name] = s.tokens
	}
	return out
}

// SlotTokens returns the tokens currently in the slot's KV cache.
func (e *Engine) SlotTokens(slot int) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkSlot(slot); err != nil {
		return 0, err
	}
	return e.slotTok[slot], nil
}

// SlotKey returns the key of the conversation the slot currently holds.
func (e *Engine) SlotKey(slot int) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkSlot(slot); err != nil {
		return "", err
	}
	return e.slotKey[slot], nil
}

// SetInfo replaces the ModelInfo that Info returns (for tests that
// want a different model identity).
func (e *Engine) SetInfo(info engine.ModelInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.info = info
}

// checkSlot validates a slot number; it must hold the lock.
func (e *Engine) checkSlot(slot int) error {
	if slot < 0 || slot >= e.slots {
		return fmt.Errorf("fake: slot %d out of range (engine has %d slots)", slot, e.slots)
	}
	return nil
}

// keyOf is the Content of the first user message in req.
func keyOf(req engine.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// Info reports the loaded model and the engine build.
func (e *Engine) Info(ctx context.Context) (engine.ModelInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.info, nil
}

// Caps reports the optional behaviours this backend supports.
func (e *Engine) Caps() engine.Caps {
	return engine.Caps{SlotSave: true}
}

// Slots reports the state of every engine slot.
func (e *Engine) Slots(ctx context.Context) ([]engine.SlotState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]engine.SlotState, e.slots)
	for i := range out {
		out[i] = engine.SlotState{
			ID:     i,
			Busy:   e.slotKey[i] != "",
			Tokens: e.slotTok[i],
		}
	}
	return out, nil
}

// Chat pops the next scripted Step for the request's key (an error when
// the script is exhausted) and returns a Stream that replays it. The
// call records a Use on the timeline and advances the simulated clock
// by the Step's Delay; it never sleeps.
func (e *Engine) Chat(ctx context.Context, req engine.ChatRequest) (engine.Stream, error) {
	key := keyOf(req)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkSlot(req.Slot); err != nil {
		return nil, err
	}
	q := e.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("fake: no scripted reply for %q", key)
	}
	step := q[0]
	e.queues[key] = q[1:]

	use := Use{Slot: req.Slot, Key: key, Start: e.now, End: e.now.Add(step.Delay)}
	e.now = use.End
	e.slotKey[req.Slot] = key
	e.slotTok[req.Slot] = step.PromptTokens + step.OutputTokens
	e.timeline = append(e.timeline, use)

	chunks := []engine.Chunk{}
	if step.Err != nil {
		chunks = append(chunks, engine.Chunk{Err: step.Err})
	} else {
		msg := step.Reply
		chunks = append(chunks,
			engine.Chunk{Text: step.Reply.Content},
			engine.Chunk{
				Done:         true,
				Message:      &msg,
				PromptTokens: step.PromptTokens,
				OutputTokens: step.OutputTokens,
			},
		)
	}
	return &fakeStream{chunks: chunks}, nil
}

// SaveSlot records the slot's key and tokens under name.
func (e *Engine) SaveSlot(ctx context.Context, slot int, name string) (engine.SnapshotMeta, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkSlot(slot); err != nil {
		return engine.SnapshotMeta{}, err
	}
	e.saved[name] = savedSlot{key: e.slotKey[slot], tokens: e.slotTok[slot]}
	return engine.SnapshotMeta{File: name, Tokens: e.slotTok[slot]}, nil
}

// RestoreSlot sets the slot's key and tokens from a saved name.
func (e *Engine) RestoreSlot(ctx context.Context, slot int, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkSlot(slot); err != nil {
		return err
	}
	s, ok := e.saved[name]
	if !ok {
		return fmt.Errorf("fake: no saved snapshot %q", name)
	}
	e.slotKey[slot] = s.key
	e.slotTok[slot] = s.tokens
	return nil
}

// EraseSlot drops the slot's KV cache.
func (e *Engine) EraseSlot(ctx context.Context, slot int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkSlot(slot); err != nil {
		return err
	}
	e.slotKey[slot] = ""
	e.slotTok[slot] = 0
	return nil
}

// Health reports whether the backend is ready to serve: it always is.
func (e *Engine) Health(ctx context.Context) error {
	return nil
}

// fakeStream replays the chunks Chat computed; Recv returns io.EOF
// after the last one. Close is a no-op.
type fakeStream struct {
	chunks []engine.Chunk
	i      int
}

// Recv returns the next chunk; io.EOF follows the last chunk.
func (s *fakeStream) Recv() (engine.Chunk, error) {
	if s.i >= len(s.chunks) {
		return engine.Chunk{}, io.EOF
	}
	ch := s.chunks[s.i]
	s.i++
	return ch, nil
}

// Close releases the underlying request early.
func (s *fakeStream) Close() error { return nil }

var _ engine.Engine = (*Engine)(nil)
