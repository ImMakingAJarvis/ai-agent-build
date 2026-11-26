// Package engine is the Go interface every model backend implements
// (ELR-1, M2.1a, card ELR-17). It is types only: no backend is wired to
// it yet. The llama.cpp client (M2.1b/M2.1c) and the fake test engine
// (M2.2) both satisfy Engine. The loop (M3.4) is the first caller.
//
// The contract the backends honour:
//   - Chat returns a Stream. Recv yields the text chunks as they arrive,
//     then one final chunk with Done true carrying the complete Message
//     (including any tool calls) and the token counts, and returns
//     io.EOF on the next call. A mid-stream failure surfaces as Err set
//     on a chunk and/or a non-nil error from Recv; Close always releases
//     the underlying request, even after an error.
//   - Fingerprint identifies the loaded model precisely enough that a
//     change in model, context, quant (KV type), engine build or file
//     size changes it — the loop uses that to know a slot's KV cache is
//     stale and must be re-prefilled.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Message is one message in a chat conversation, in the wire shape the
// backends exchange. Role is "system", "user", "assistant" or "tool".
// ToolCalls is set on assistant messages that invoke tools; ToolCallID
// is set on tool-result messages to link them to the call they answer.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a tool invocation an assistant message carries. Arguments
// is the raw JSON object the model produced; the loop parses it against
// the ToolSpec's schema before running the tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolSpec declares a tool to the model. Schema is a JSON Schema object
// describing the Arguments the tool accepts.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ChatRequest is one chat turn. Slot names the engine slot the
// conversation lives in (the engine keeps its KV cache there); a
// conversation in the same slot keeps its prefix cached.
type ChatRequest struct {
	Slot        int
	Messages    []Message
	Tools       []ToolSpec
	CachePrompt bool
	MaxTokens   int
	Thinking    bool
}

// Chunk is one piece of a streamed reply. Text carries the delta; the
// final chunk has Done true and carries the complete Message (with any
// tool calls) and the prompt/output token counts. Err, when set,
// records a failure that ended the stream.
type Chunk struct {
	Text         string
	Done         bool
	Message      *Message
	PromptTokens int
	OutputTokens int
	Err          error
}

// Stream is a running chat reply. Recv blocks until the next chunk is
// ready and returns io.EOF after the Done chunk. Close releases the
// underlying request early (it is always safe to call, including after
// an error or after EOF).
type Stream interface {
	Recv() (Chunk, error)
	Close() error
}

// SlotState is the state of one engine slot: its ID, whether a
// conversation currently occupies it, and the tokens in its KV cache.
type SlotState struct {
	ID     int
	Busy   bool
	Tokens int
}

// ModelInfo identifies the loaded model and the engine serving it.
// Fingerprint over these fields is what the loop compares to decide a
// slot's cached context is stale.
type ModelInfo struct {
	ID            string
	ContextTokens int
	KVType        string
	Build         string
	ModelFile     string
	ModelBytes    int64
}

// Fingerprint is a short content hash of the ModelInfo: the first 16
// hex chars of the sha256 of ID, ContextTokens, KVType, Build,
// ModelFile and ModelBytes joined with "|".
func (m ModelInfo) Fingerprint() string {
	s := strings.Join([]string{
		m.ID,
		strconv.Itoa(m.ContextTokens),
		m.KVType,
		m.Build,
		m.ModelFile,
		strconv.FormatInt(m.ModelBytes, 10),
	}, "|")
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// SnapshotMeta describes a saved slot snapshot: the file it was written
// to, the tokens it holds, its size, and the seconds saving took.
type SnapshotMeta struct {
	File    string
	Tokens  int
	Bytes   int64
	Seconds float64
}

// Caps reports which optional engine behaviours a backend supports. The
// loop degrades gracefully when one is false (e.g. without SlotSave it
// re-prefills from scratch after a restart instead of restoring a
// snapshot).
type Caps struct {
	SlotSave         bool
	SleepWake        bool
	Priority         bool
	ExpertOffload    bool
	VariableTickCost bool
}

// Engine is the interface every model backend implements.
type Engine interface {
	// Info reports the loaded model and the engine build.
	Info(ctx context.Context) (ModelInfo, error)
	// Caps reports the optional behaviours this backend supports.
	Caps() Caps
	// Slots reports the state of every engine slot.
	Slots(ctx context.Context) ([]SlotState, error)
	// Chat starts a streaming reply for req and returns its Stream.
	Chat(ctx context.Context, req ChatRequest) (Stream, error)
	// SaveSlot writes slot's KV cache to a named snapshot file.
	SaveSlot(ctx context.Context, slot int, name string) (SnapshotMeta, error)
	// RestoreSlot loads a named snapshot file into slot.
	RestoreSlot(ctx context.Context, slot int, name string) error
	// EraseSlot drops slot's KV cache.
	EraseSlot(ctx context.Context, slot int) error
	// Health reports whether the backend is ready to serve.
	Health(ctx context.Context) error
}
