// chat.go is the streaming half of the llama.cpp client (ELR-1, M2.1c,
// card ELR-19). Chat posts to /v1/chat/completions and parses the
// server-sent-events reply into engine.Chunks. The wire shapes follow
// the OpenAI convention and the fixtures in docs/runtime/fixtures
// (toolcalls.sse, captured from the box's engine); id_slot is the
// llama-server slot-pinning extension (see internal/models/slot.go).
// With Chat, *Client satisfies engine.Engine (asserted below).
package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// wireToolCall is one tool call in the request messages, in the OpenAI
// shape: arguments is the raw JSON as a STRING, not a nested object.
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatMessage is one request message in the wire shape: role, content,
// tool_calls and tool_call_id.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireTool is one entry of the request "tools" array.
type wireTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// chatRequest is the /v1/chat/completions body. id_slot and
// cache_prompt are llama-server extensions sent on every request;
// max_tokens and tools are present only when set.
type chatRequest struct {
	Messages           []chatMessage  `json:"messages"`
	Stream             bool           `json:"stream"`
	StreamOptions      map[string]any `json:"stream_options"`
	IDSlot             int            `json:"id_slot"`
	CachePrompt        bool           `json:"cache_prompt"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	Tools              []wireTool     `json:"tools,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
}

// sseChunk is one "data: " JSON object of the streamed reply. Only the
// fields the client consumes are decoded; the rest (id, timings, ...)
// is ignored.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// toolCallAccum collects the streamed pieces of one tool call, keyed by
// the delta's index: the first piece carries id and function.name,
// later pieces append to function.arguments.
type toolCallAccum struct {
	id        string
	name      string
	arguments strings.Builder
}

// chatStream is the Stream for one Chat call. A reader goroutine parses
// the SSE body and pushes chunks; Recv pulls them, and io.EOF follows
// the Done chunk.
type chatStream struct {
	ctx    context.Context // derived from the caller's; bounds the request
	cancel context.CancelFunc
	resp   *http.Response
	chunks chan engine.Chunk
	quit   chan struct{} // closed by Close, so in-flight sends can unblock
	once   sync.Once
}

// Chat starts a streaming reply for req and returns its Stream. The
// reply is parsed as server-sent events; see the package docs and
// docs/runtime/fixtures/toolcalls.sse for the wire shape.
func (c *Client) Chat(ctx context.Context, req engine.ChatRequest) (engine.Stream, error) {
	body := chatRequest{
		Stream:             true,
		StreamOptions:      map[string]any{"include_usage": true},
		IDSlot:             req.Slot,
		CachePrompt:        req.CachePrompt,
		MaxTokens:          req.MaxTokens,
		ChatTemplateKwargs: map[string]any{"enable_thinking": req.Thinking},
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var wc wireToolCall
			wc.ID = tc.ID
			wc.Type = "function"
			wc.Function.Name = tc.Name
			wc.Function.Arguments = string(tc.Arguments)
			cm.ToolCalls = append(cm.ToolCalls, wc)
		}
		body.Messages = append(body.Messages, cm)
	}
	if len(req.Tools) > 0 {
		body.Tools = make([]wireTool, 0, len(req.Tools))
		for _, ts := range req.Tools {
			var wt wireTool
			wt.Type = "function"
			wt.Function.Name = ts.Name
			wt.Function.Description = ts.Description
			wt.Function.Parameters = ts.Schema
			body.Tools = append(body.Tools, wt)
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	// The shared HTTP client carries a 10s timeout meant for the short
	// control calls; a reply stream is bounded by ctx instead.
	hc := *c.HTTP
	hc.Timeout = 0
	ctx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("chat: %w", err)
	}
	if resp.StatusCode > 299 {
		err := httpError(resp)
		resp.Body.Close()
		cancel()
		return nil, err
	}
	s := &chatStream{
		ctx:    ctx,
		cancel: cancel,
		resp:   resp,
		chunks: make(chan engine.Chunk, 16),
		quit:   make(chan struct{}),
	}
	go s.read()
	return s, nil
}

// Recv returns the next chunk; io.EOF follows the Done chunk. A
// cancelled ctx makes it return an error without waiting for the
// server.
func (s *chatStream) Recv() (engine.Chunk, error) {
	select {
	case <-s.ctx.Done():
		s.Close()
		return engine.Chunk{}, s.ctx.Err()
	case ch, ok := <-s.chunks:
		if !ok {
			return engine.Chunk{}, io.EOF
		}
		return ch, nil
	}
}

// Close releases the underlying request early. It is safe to call any
// number of times, including after an error or after EOF.
func (s *chatStream) Close() error {
	var err error
	s.once.Do(func() {
		close(s.quit)
		err = s.resp.Body.Close()
	})
	return err
}

// send delivers ch to Recv, or drops it if the stream was Closed first
// (so the reader goroutine never blocks on an abandoned stream).
func (s *chatStream) send(ch engine.Chunk) {
	select {
	case s.chunks <- ch:
	case <-s.quit:
	}
}

// read parses the SSE body until [DONE] (or the body ends), pushes a
// Chunk per content delta and any error chunk, and finally the Done
// chunk with the assembled assistant message; Recv then sees io.EOF.
func (s *chatStream) read() {
	defer close(s.chunks)
	var (
		text   strings.Builder
		calls  = map[int]*toolCallAccum{}
		prompt int
		output int
	)
	// finish sends the terminal chunk: either the Done chunk with the
	// full assistant message, or an error chunk when a collected
	// arguments string is not valid JSON.
	finish := func() {
		idxs := make([]int, 0, len(calls))
		for i := range calls {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		var toolCalls []engine.ToolCall
		for _, i := range idxs {
			a := calls[i]
			args := a.arguments.String()
			if !json.Valid([]byte(args)) {
				s.send(engine.Chunk{Err: fmt.Errorf("tool call %s: arguments are not valid JSON: %s", a.id, args)})
				return
			}
			toolCalls = append(toolCalls, engine.ToolCall{
				ID:        a.id,
				Name:      a.name,
				Arguments: json.RawMessage(args),
			})
		}
		s.send(engine.Chunk{
			Done:         true,
			Message:      &engine.Message{Role: "assistant", Content: text.String(), ToolCalls: toolCalls},
			PromptTokens: prompt,
			OutputTokens: output,
		})
	}
	sc := bufio.NewScanner(s.resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			finish()
			return
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			s.send(engine.Chunk{Err: fmt.Errorf("parse stream chunk: %w", err)})
			return
		}
		if ch.Usage != nil {
			prompt = ch.Usage.PromptTokens
			output = ch.Usage.CompletionTokens
		}
		if len(ch.Choices) == 0 {
			continue
		}
		delta := ch.Choices[0].Delta
		if delta.Content != nil {
			text.WriteString(*delta.Content)
			s.send(engine.Chunk{Text: *delta.Content})
		}
		for _, tc := range delta.ToolCalls {
			a := calls[tc.Index]
			if a == nil {
				a = &toolCallAccum{}
				calls[tc.Index] = a
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.arguments.WriteString(tc.Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil {
		// A cancelled ctx already surfaces from Recv's own select;
		// anything else (connection reset, ...) is reported here.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.send(engine.Chunk{Err: fmt.Errorf("read stream: %w", err)})
		}
		return
	}
	// The body ended without [DONE]: deliver what was collected.
	finish()
}

// Compile-time check: with Chat, the client is a full engine.Engine.
var _ engine.Engine = (*Client)(nil)
