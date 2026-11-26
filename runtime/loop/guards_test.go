// guards_test.go covers the unusable-reply and repeat-call guards
// (card ELR-88, M3.4d): the two pure counters over the message log, and
// the tick/Step behavior each guard drives — an empty reply is answered
// and retried, three bad replies in a row fail the job, a bad tool
// argument reaches the model, an engine error leaves a note, and a
// repeat run gets one warning before it fails.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/fake"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// userMsg builds a user message the way the log stores one: a JSON
// string.
func userMsg(s string) agentjob.LoopMessage {
	b, _ := json.Marshal(s)
	return agentjob.LoopMessage{Role: "user", Content: b}
}

// assistantMsg builds an assistant message the way a tick checkpoints
// one: the assistantContent object.
func assistantMsg(text string, calls ...engine.ToolCall) agentjob.LoopMessage {
	b, _ := json.Marshal(assistantContent{Content: text, ToolCalls: calls})
	return agentjob.LoopMessage{Role: "assistant", Content: b}
}

// toolMsg builds a tool-result message the way HandleOpDone stores one.
func toolMsg(callID, out string) agentjob.LoopMessage {
	b, _ := json.Marshal(toolContent{ToolCallID: callID, Content: out})
	return agentjob.LoopMessage{Role: "tool", Content: b}
}

func TestTrailingBad(t *testing.T) {
	note := "[runtime] your last reply could not be used: boom. Reply again."
	cases := []struct {
		name string
		msgs []agentjob.LoopMessage
		want int
	}{
		{"empty log", nil, 0},
		{"plain user message", []agentjob.LoopMessage{userMsg("count")}, 0},
		{"one note", []agentjob.LoopMessage{userMsg(note)}, 1},
		{"two notes after an assistant", []agentjob.LoopMessage{
			assistantMsg("ok"), userMsg(note), userMsg(emptyReplyNote),
		}, 2},
		{"empty replies and their notes in a row", []agentjob.LoopMessage{
			assistantMsg(""), userMsg(note), assistantMsg(""), userMsg(emptyReplyNote),
		}, 2},
		{"a usable reply ends the run", []agentjob.LoopMessage{
			assistantMsg(""), userMsg(note), assistantMsg("done"),
		}, 0},
		{"run stops at a plain user message", []agentjob.LoopMessage{
			userMsg(note), userMsg("plain"), userMsg(emptyReplyNote),
		}, 1},
		{"last message is an assistant", []agentjob.LoopMessage{
			userMsg(note), assistantMsg("done"),
		}, 0},
		{"a repeat warning is not a bad reply", []agentjob.LoopMessage{
			userMsg(repeatWarnNote(3)),
		}, 0},
	}
	for _, tc := range cases {
		if got := trailingBad(tc.msgs); got != tc.want {
			t.Errorf("%s: trailingBad = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestTrailingSame(t *testing.T) {
	shell := func(id string) engine.ToolCall {
		return engine.ToolCall{ID: id, Name: "shell", Arguments: json.RawMessage(`{"cmd":"ls"}`)}
	}
	other := engine.ToolCall{ID: "b", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}
	shellAlt := engine.ToolCall{ID: "a2", Name: "shell", Arguments: json.RawMessage(`{"cmd":"pwd"}`)}
	cases := []struct {
		name string
		msgs []agentjob.LoopMessage
		want int
	}{
		{"empty log", nil, 0},
		{"one call", []agentjob.LoopMessage{
			assistantMsg("", shell("a")),
		}, 1},
		{"two identical calls, results between", []agentjob.LoopMessage{
			toolMsg("t0", "ok"),
			assistantMsg("", shell("a")),
			toolMsg("t1", "ok"),
			assistantMsg("", shell("b")),
		}, 2},
		{"different arguments", []agentjob.LoopMessage{
			assistantMsg("", shell("a")),
			assistantMsg("", shellAlt),
		}, 1},
		{"different tool", []agentjob.LoopMessage{
			assistantMsg("", other),
			assistantMsg("", shell("a")),
		}, 1},
		{"different call count", []agentjob.LoopMessage{
			assistantMsg("", shell("a"), other),
			assistantMsg("", shell("b")),
		}, 1},
		{"order matters", []agentjob.LoopMessage{
			assistantMsg("", shell("a"), other),
			assistantMsg("", other, shell("b")),
		}, 1},
		{"a plain answer ends the run", []agentjob.LoopMessage{
			assistantMsg("", shell("a")),
			assistantMsg("done"),
		}, 0},
		{"the newest call is a plain answer", []agentjob.LoopMessage{
			assistantMsg("done"),
		}, 0},
	}
	for _, tc := range cases {
		if got := trailingSame(tc.msgs); got != tc.want {
			t.Errorf("%s: trailingSame = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestEmptyReplyIsNotDone: a reply with neither text nor tool calls is
// checkpointed, the job goes back to ready with a note for the model,
// and the next prompt carries that note.
func TestEmptyReplyIsNotDone(t *testing.T) {
	eng := fake.New(1)
	rec := &recEngine{Engine: eng}
	l, store, _ := newTickEnv(t, rec)
	eng.Script(tickInstructions,
		fake.Step{Reply: engine.Message{Role: "assistant"}, PromptTokens: 100, OutputTokens: 1},
		fake.Step{Reply: engine.Message{Role: "assistant", Content: "finished"}, PromptTokens: 100, OutputTokens: 5},
	)
	l.Submit(tickJobID)

	ran, err := l.Step(context.Background())
	if !ran || err != nil {
		t.Fatalf("step = %v, %v; want the empty tick to run", ran, err)
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("status = %q, want ready (an empty reply is not done)", j.Status)
	}
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	// The log is: the instructions (Submit), the empty reply, the note.
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want the empty reply and the note", len(msgs))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want the assistant reply", msgs[1].Role)
	}
	var note string
	if err := json.Unmarshal(msgs[2].Content, &note); err != nil {
		t.Fatalf("note = %q, want a JSON string: %v", msgs[2].Content, err)
	}
	if msgs[2].Role != "user" || note != emptyReplyNote {
		t.Errorf("note = %q (role %q), want the empty-reply note", note, msgs[2].Role)
	}

	// The job was put back in the ready list: the follow-up tick runs,
	// and its prompt carries the note.
	ran, err = l.Step(context.Background())
	if !ran || err != nil {
		t.Fatalf("step 2 = %v, %v; want the follow-up tick", ran, err)
	}
	found := false
	for _, m := range rec.requests()[1].Messages {
		if m.Role == "user" && strings.Contains(string(m.Content), "your reply was empty") {
			found = true
		}
	}
	if !found {
		t.Error("second prompt has no empty-reply note")
	}
	j, _ = store.Get(tickJobID)
	if j.Status != agentjob.Done || j.Summary != "finished" {
		t.Errorf("status = %q summary = %q, want done/finished", j.Status, j.Summary)
	}
}

// TestThreeEmptyRepliesFailJob: after three unusable replies in a row
// the guard fails the job before asking the model a fourth time.
func TestThreeEmptyRepliesFailJob(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng)
	for i := 0; i < 3; i++ {
		eng.Script(tickInstructions, fake.Step{Reply: engine.Message{Role: "assistant"}, PromptTokens: 100, OutputTokens: 1})
	}
	l.Submit(tickJobID)
	for i := 0; i < 3; i++ {
		ran, err := l.Step(context.Background())
		if !ran || err != nil {
			t.Fatalf("step %d = %v, %v", i+1, ran, err)
		}
	}
	ran, err := l.Step(context.Background())
	if err != nil {
		t.Fatalf("step 4 err = %v, want the guard to fail the job without an error", err)
	}
	_ = ran
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Failed {
		t.Fatalf("status = %q, want failed", j.Status)
	}
	if j.Error != "model gave 3 unusable replies in a row" {
		t.Errorf("error = %q, want the bad-reply error", j.Error)
	}
	if n := len(eng.Timeline()); n != 3 {
		t.Errorf("engine calls = %d, want 3 (the guard fails before the 4th)", n)
	}
}

// TestBadArgumentsReachModel: a tool call whose arguments are not a JSON
// object is not started, the failure reaches the model as a tool error,
// and the job recovers when the model resends the call.
func TestBadArgumentsReachModel(t *testing.T) {
	eng := fake.New(1)
	rec := &recEngine{Engine: eng}
	l, store, runner := newTickEnv(t, rec, captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone, Output: "ok"}})
	eng.Script(tickInstructions,
		fake.Step{
			Reply: engine.Message{Role: "assistant", ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`"just a string"`)},
			}},
			PromptTokens: 100, OutputTokens: 20,
		},
		fake.Step{Reply: engine.Message{Role: "assistant", Content: "fixed"}, PromptTokens: 100, OutputTokens: 5},
	)
	l.Submit(tickJobID)

	ran, err := l.Step(context.Background())
	if !ran || err != nil {
		t.Fatalf("step = %v, %v", ran, err)
	}
	lops, err := store.Ops(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lops) != 1 || lops[0].Status != agentjob.OpFailed {
		t.Fatalf("ops = %+v, want one failed op", lops)
	}
	if lops[0].StartedAt != nil {
		t.Error("op was started, want not started")
	}
	if got := runner.Running(); len(got) != 0 {
		t.Errorf("runner running = %v, want none", got)
	}

	// The job is back in the ready list (the resubmit in Step) and the
	// next prompt carries the failure.
	ran, err = l.Step(context.Background())
	if !ran || err != nil {
		t.Fatalf("step 2 = %v, %v", ran, err)
	}
	found := false
	for _, m := range rec.requests()[1].Messages {
		if m.Role == "tool" &&
			strings.Contains(string(m.Content), "arguments are not a JSON object") &&
			strings.Contains(string(m.Content), "Send the call again with valid JSON") {
			found = true
		}
	}
	if !found {
		t.Error("second prompt has no bad-arguments error")
	}
	j, _ := store.Get(tickJobID)
	if j.Status != agentjob.Done || j.Summary != "fixed" {
		t.Errorf("status = %q summary = %q, want done/fixed", j.Status, j.Summary)
	}
}

// TestEngineErrorAddsNote: a failed model round leaves the model one
// note about the failure, the job goes back to ready, and the next
// prompt carries the note.
func TestEngineErrorAddsNote(t *testing.T) {
	eng := fake.New(1)
	rec := &recEngine{Engine: eng}
	l, store, _ := newTickEnv(t, rec)
	boom := errors.New("engine down")
	eng.Script(tickInstructions,
		fake.Step{Err: boom},
		fake.Step{Reply: engine.Message{Role: "assistant", Content: "back"}, PromptTokens: 100, OutputTokens: 5},
	)
	l.Submit(tickJobID)

	ran, err := l.Step(context.Background())
	if err == nil {
		t.Fatal("step err = nil, want the engine error")
	}
	if ran {
		t.Error("ran = true, want false")
	}
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Ready {
		t.Errorf("status = %q, want ready", j.Status)
	}
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	// The log is: the instructions (Submit), the one note.
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the one note", len(msgs))
	}
	var note string
	if err := json.Unmarshal(msgs[1].Content, &note); err != nil {
		t.Fatalf("note = %q, want a JSON string: %v", msgs[1].Content, err)
	}
	if !strings.Contains(note, "your last reply could not be used: engine down") {
		t.Errorf("note = %q, want the engine-error note", note)
	}

	ran, err = l.Step(context.Background())
	if !ran || err != nil {
		t.Fatalf("step 2 = %v, %v; want the retry tick", ran, err)
	}
	found := false
	for _, m := range rec.requests()[1].Messages {
		if m.Role == "user" && strings.Contains(string(m.Content), "could not be used") {
			found = true
		}
	}
	if !found {
		t.Error("second prompt has no engine-error note")
	}
	j, _ = store.Get(tickJobID)
	if j.Status != agentjob.Done || j.Summary != "back" {
		t.Errorf("status = %q summary = %q, want done/back", j.Status, j.Summary)
	}
}

// TestRepeatWarnThenFail: a model that sends the same tool call for ever
// gets one warning at repeat_warn and a failed job at repeat_fail — with
// the engine called at most repeat_fail times in total.
func TestRepeatWarnThenFail(t *testing.T) {
	eng := fake.New(1)
	l, store, _ := newTickEnv(t, eng, captureTool{name: "shell", res: ops.Result{Status: agentjob.OpDone, Output: "ok"}})
	for i := 0; i < 10; i++ {
		eng.Script(tickInstructions, oneCallScript(fmt.Sprintf("call_%d", i), "shell"))
	}
	l.Submit(tickJobID)
	for i := 0; i < 5; i++ {
		ran, err := l.Step(context.Background())
		if !ran || err != nil {
			t.Fatalf("step %d = %v, %v", i+1, ran, err)
		}
		if err := l.HandleOpDone(tickJobID, fmt.Sprintf("call_%d", i), ops.Result{Status: agentjob.OpDone, Output: "ok"}); err != nil {
			t.Fatalf("HandleOpDone(call_%d): %v", i, err)
		}
	}
	ran, err := l.Step(context.Background())
	if err != nil {
		t.Fatalf("step 6 err = %v, want the guard to fail the job without an error", err)
	}
	_ = ran
	j, err := store.Get(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != agentjob.Failed {
		t.Fatalf("status = %q, want failed", j.Status)
	}
	if j.Error != "model repeated the same tool call 5 times" {
		t.Errorf("error = %q, want the repeat error", j.Error)
	}
	if n := len(eng.Timeline()); n > 5 {
		t.Errorf("engine calls = %d, want at most 5", n)
	}
	// The warning note was appended exactly once.
	msgs, err := store.Messages(tickJobID)
	if err != nil {
		t.Fatal(err)
	}
	warns := 0
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(string(m.Content), "you sent the same tool call") {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("warning notes = %d, want 1", warns)
	}
}
