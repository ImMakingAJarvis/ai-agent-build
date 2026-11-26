package llamacpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
)

// newServer starts an httptest server with cleanup registered.
func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// sseServer serves the given SSE payload for every POST, flushing the
// headers before writing the body so Chat returns while the payload is
// still being "generated".
func sseServer(t *testing.T, payload string) *Client {
	t.Helper()
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		io.WriteString(w, payload)
	}))
	return New(srv.URL)
}

// drain pulls the stream to its end and returns every chunk.
func drain(t *testing.T, s engine.Stream) []engine.Chunk {
	t.Helper()
	defer s.Close()
	var out []engine.Chunk
	for {
		ch, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ch.Err != nil {
			out = append(out, ch)
			return out
		}
		out = append(out, ch)
		if ch.Done {
			// The Done chunk is the last one; the next Recv is EOF.
			continue
		}
	}
}

// TestChatRequestBody pins the /v1/chat/completions body: the slot
// extensions on every request, the stream options, and the OpenAI shape
// of tools and tool calls (arguments as a JSON string).
func TestChatRequestBody(t *testing.T) {
	var bodies []map[string]any
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	c := New(srv.URL)

	req := engine.ChatRequest{
		Slot:        2,
		CachePrompt: true,
		MaxTokens:   512,
		Thinking:    true,
		Messages: []engine.Message{
			{Role: "system", Content: "you are a test"},
			{
				Role: "assistant",
				ToolCalls: []engine.ToolCall{
					{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)},
				},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
		Tools: []engine.ToolSpec{
			{
				Name:        "get_weather",
				Description: "weather for a city",
				Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
		},
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// A second request without tools or max_tokens: those keys must be
	// absent from the body.
	plain := engine.ChatRequest{Messages: []engine.Message{{Role: "user", Content: "hi"}}}
	if _, err := c.Chat(context.Background(), plain); err != nil {
		t.Fatalf("Chat (plain): %v", err)
	}

	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(bodies))
	}
	got := bodies[0]

	if got["id_slot"] != float64(2) {
		t.Errorf("id_slot = %v, want 2", got["id_slot"])
	}
	if got["cache_prompt"] != true {
		t.Errorf("cache_prompt = %v, want true", got["cache_prompt"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true", got["stream"])
	}
	so, _ := got["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage true", got["stream_options"])
	}
	if got["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512", got["max_tokens"])
	}
	ck, _ := got["chat_template_kwargs"].(map[string]any)
	if ck == nil || ck["enable_thinking"] != true {
		t.Errorf("chat_template_kwargs = %v, want enable_thinking true", got["chat_template_kwargs"])
	}

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want one entry", got["tools"])
	}
	tm := tools[0].(map[string]any)
	if tm["type"] != "function" {
		t.Errorf("tools[0].type = %v, want \"function\"", tm["type"])
	}
	fn, _ := tm["function"].(map[string]any)
	if fn == nil || fn["name"] != "get_weather" || fn["description"] != "weather for a city" {
		t.Errorf("tools[0].function = %v, want name/description set", tm["function"])
	}
	if fn != nil {
		var wantParams map[string]any
		if err := json.Unmarshal([]byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`), &wantParams); err != nil {
			t.Fatalf("bad wantParams literal: %v", err)
		}
		gotParams, _ := fn["parameters"].(map[string]any)
		if !reflect.DeepEqual(gotParams, wantParams) {
			t.Errorf("tools[0].function.parameters = %v, want %v", gotParams, wantParams)
		}
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want 3", got["messages"])
	}
	am, _ := msgs[1].(map[string]any)
	tcs, _ := am["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("messages[1].tool_calls = %v, want one call", am["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Errorf("tool call id/type = %v/%v, want call_1/function", tc["id"], tc["type"])
	}
	tfn, _ := tc["function"].(map[string]any)
	if tfn == nil || tfn["name"] != "get_weather" || tfn["arguments"] != `{"city":"Paris"}` {
		t.Errorf("tool call function = %v, want name get_weather and arguments as a JSON string", tc["function"])
	}
	tm2, _ := msgs[2].(map[string]any)
	if tm2["role"] != "tool" || tm2["tool_call_id"] != "call_1" {
		t.Errorf("messages[2] = %v, want role tool with tool_call_id call_1", tm2)
	}

	plainBody := bodies[1]
	if _, ok := plainBody["tools"]; ok {
		t.Errorf("plain body has tools key, want it absent")
	}
	if _, ok := plainBody["max_tokens"]; ok {
		t.Errorf("plain body has max_tokens key, want it absent")
	}
	if plainBody["id_slot"] != float64(0) {
		t.Errorf("plain body id_slot = %v, want 0", plainBody["id_slot"])
	}
}

// TestChatStreamText streams three content deltas then [DONE]: Recv
// yields each delta, one Done chunk with the full message, then EOF.
func TestChatStreamText(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null}}]}\n\n")
	for _, part := range []string{"Hel", "lo", " world"} {
		fmt.Fprintf(&sb, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
	}
	sb.WriteString("data: [DONE]\n\n")
	c := sseServer(t, sb.String())

	stream, err := c.Chat(context.Background(), engine.ChatRequest{
		Messages: []engine.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var chunks []engine.Chunk
	for {
		ch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, ch)
	}
	var texts []string
	var done *engine.Chunk
	for i := range chunks {
		if chunks[i].Done {
			done = &chunks[i]
			continue
		}
		texts = append(texts, chunks[i].Text)
	}
	if strings.Join(texts, "") != "Hello world" {
		t.Errorf("text deltas = %q, want %q", strings.Join(texts, ""), "Hello world")
	}
	if len(texts) != 3 {
		t.Errorf("got %d text chunks, want 3", len(texts))
	}
	if done == nil {
		t.Fatal("no Done chunk")
	}
	if done.Message == nil || done.Message.Role != "assistant" || done.Message.Content != "Hello world" {
		t.Errorf("Done message = %+v, want assistant / \"Hello world\"", done.Message)
	}
	if len(done.Message.ToolCalls) != 0 {
		t.Errorf("Done message has %d tool calls, want 0", len(done.Message.ToolCalls))
	}
}

// TestChatStreamToolCalls replays the fixture captured from the box's
// engine and expects the two tool calls it contains, assembled from
// the per-index deltas.
func TestChatStreamToolCalls(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "toolcalls.sse"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write(data)
	}))
	c := New(srv.URL)

	stream, err := c.Chat(context.Background(), engine.ChatRequest{
		Slot:     1,
		Messages: []engine.Message{{Role: "user", Content: "weather in Paris and time in Tokyo"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	chunks := drain(t, stream)

	var texts []string
	var done *engine.Chunk
	for i := range chunks {
		if chunks[i].Done {
			done = &chunks[i]
			continue
		}
		if chunks[i].Text != "" {
			texts = append(texts, chunks[i].Text)
		}
	}
	if len(texts) != 0 {
		t.Errorf("unexpected text chunks: %q", strings.Join(texts, ""))
	}
	if done == nil {
		t.Fatal("no Done chunk")
	}
	msg := done.Message
	if msg == nil || msg.Role != "assistant" {
		t.Fatalf("Done message = %+v, want an assistant message", msg)
	}
	want := []struct {
		id, name, args string
	}{
		{"P3W6l6cyw5F9w5nt1mKAMOFwwGcp1UPr", "get_weather", `{"city":"Paris"}`},
		{"wOuJ3uQLGgOJkyG86ABxOmMPALCIfc43", "get_time", `{"city":"Tokyo"}`},
	}
	if len(msg.ToolCalls) != len(want) {
		t.Fatalf("tool calls = %d, want %d", len(msg.ToolCalls), len(want))
	}
	for i, w := range want {
		tc := msg.ToolCalls[i]
		if tc.ID != w.id || tc.Name != w.name {
			t.Errorf("tool call %d = %s %s, want %s %s", i, tc.ID, tc.Name, w.id, w.name)
		}
		if string(tc.Arguments) != w.args {
			t.Errorf("tool call %d arguments = %s, want %s", i, tc.Arguments, w.args)
		}
	}
	// The fixture carries no usage object.
	if done.PromptTokens != 0 || done.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0 (fixture carries no usage)", done.PromptTokens, done.OutputTokens)
	}
}

// TestChatBadArguments ends the stream with a tool call whose collected
// arguments are not valid JSON: the terminal chunk carries an error
// that includes the bad string, and no Done chunk is sent.
func TestChatBadArguments(t *testing.T) {
	c := sseServer(t,
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n"+
			"data: [DONE]\n\n")

	stream, err := c.Chat(context.Background(), engine.ChatRequest{
		Messages: []engine.Message{{Role: "user", Content: "weather please"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	chunks := drain(t, stream)

	var errChunk *engine.Chunk
	for i := range chunks {
		if chunks[i].Done {
			t.Errorf("chunk %d is Done despite invalid arguments", i)
		}
		if chunks[i].Err != nil {
			errChunk = &chunks[i]
		}
	}
	if errChunk == nil {
		t.Fatal("no error chunk for invalid tool call arguments")
	}
	if !strings.Contains(errChunk.Err.Error(), `{"city":`) {
		t.Errorf("error %q does not include the bad arguments", errChunk.Err)
	}
}

// TestChatCancel: the server holds the stream open; cancelling the ctx
// makes Recv return an error well within one second.
func TestChatCancel(t *testing.T) {
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(3 * time.Second) // hold the stream open
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n")
	}))
	c := New(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Chat(ctx, engine.ChatRequest{
		Messages: []engine.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the request reach the sleeping handler
	start := time.Now()
	cancel()
	var gotErr error
	for {
		ch, err := stream.Recv()
		if err != nil {
			gotErr = err
			break
		}
		if ch.Err != nil {
			gotErr = ch.Err
			break
		}
		if time.Since(start) > time.Second {
			t.Fatal("Recv did not return an error within 1s of cancel")
		}
	}
	if gotErr == nil {
		t.Fatal("no error after cancel")
	}
	stream.Close()
}
