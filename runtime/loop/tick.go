// tick.go runs ONE tick for ONE job (ELR-1, M3.4b, card ELR-30): get a
// slot, ask the model, save the result, start the tools, give the slot
// back. The tick never blocks on a tool: the ops run in the Runner's
// goroutines and the job comes back through HandleOpDone, which is why a
// waiting job holds no slot (gate_test.go proves the consequence).
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// systemPrompt is the first message of every tick of every job. It must
// not change while a job runs: the engine's prompt cache depends on the
// prefix being byte-identical between ticks.
const systemPrompt = "You are an autonomous agent. Use the tools to do the task. When the task is finished, reply with a short summary and no tool call."

// assistantContent is the JSON object a tick stores as its assistant
// message's content: the reply text and the tool calls it made (the
// shape RecoverLoop decodes when rebuilding a torn checkpoint).
type assistantContent struct {
	Content   string            `json:"content,omitempty"`
	ToolCalls []engine.ToolCall `json:"tool_calls,omitempty"`
}

// toolContent is the JSON object stored as a tool message's content
// (card m3-4c writes it when an op finishes): which call it answers and
// the text the model reads.
type toolContent struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// tick runs one tick for jobID in the given order: acquire a slot, mark
// the job running, ask the model, checkpoint the result (message, ops,
// state), release the slot, start the tools. slot.ErrNoSlot is returned
// unchanged: the job stays where it is and the next ready job may take
// the slot.
func (l *Loop) tick(ctx context.Context, jobID string) error {
	lease, err := l.deps.Slots.Acquire(ctx, jobID)
	if err != nil {
		return err
	}

	job, err := l.deps.Store.Get(jobID)
	if err != nil {
		l.deps.Slots.Release(lease, 0)
		return err
	}
	oldTokens := job.State.Tokens

	// giveBack rolls the tick back when the model round does not finish:
	// the slot goes back with the tokens the job had BEFORE this tick,
	// and the job goes back to ready, so Step can offer it again.
	giveBack := func() {
		l.deps.Slots.Release(lease, oldTokens)
		st := job.State
		st.Status = agentjob.Ready
		_ = l.deps.Store.SetStatus(jobID, st)
	}

	// The job is running from here until the checkpoint lands or the
	// tick rolls back.
	st := job.State
	st.Status = agentjob.Running
	if err := l.deps.Store.SetStatus(jobID, st); err != nil {
		giveBack()
		return err
	}

	// Ask the model and read the stream to the Done chunk.
	reply, tokens, err := l.ask(ctx, jobID, lease.Slot)
	if err != nil {
		giveBack()
		return err
	}

	// The checkpoint: one assistant message, one pending op per tool
	// call, and the new state — waiting on the calls, or done with the
	// reply as its summary.
	content, err := json.Marshal(assistantContent{Content: reply.Content, ToolCalls: reply.ToolCalls})
	if err != nil {
		giveBack()
		return err
	}
	tickOps := make([]agentjob.LoopOp, 0, len(reply.ToolCalls))
	for _, tc := range reply.ToolCalls {
		tickOps = append(tickOps, agentjob.LoopOp{
			ID: tc.ID, ToolCallID: tc.ID, Tool: tc.Name, Args: tc.Arguments, Status: agentjob.OpPending,
		})
	}
	cs := job.State
	cs.Tokens = tokens
	if len(tickOps) > 0 {
		cs.Status = agentjob.Waiting
		cs.WaitReason = agentjob.WaitOperation
	} else {
		cs.Status = agentjob.Done
		cs.WaitReason = ""
		cs.Summary = reply.Content
	}
	if err := l.deps.Store.Checkpoint(jobID, agentjob.Checkpoint{
		Messages: []agentjob.LoopMessage{{Role: "assistant", Content: content, Tokens: tokens}},
		Ops:      tickOps,
		State:    cs,
	}); err != nil {
		giveBack()
		return err
	}

	// The slot is free for the next ready job; it keeps the job's KV
	// cache with the tokens this tick left in it.
	l.deps.Slots.Release(lease, tokens)

	// Start the tools, one Runner call per op.
	for _, op := range tickOps {
		if err := l.startOp(jobID, op); err != nil {
			return err
		}
	}
	return nil
}

// startOp starts one op of the job: a call for a tool the registry does
// not hold is not started (its result is an immediate failure the model
// reads on the next tick); a known tool is marked running with a
// started_at and handed to the Runner, which reports the result through
// HandleOpDone. The tool's OnPID callback records the command's PID on
// the op, so a restart can adopt the process (card m3-1b).
func (l *Loop) startOp(jobID string, op agentjob.LoopOp) error {
	if _, known := l.deps.Registry.Get(op.Tool); !known {
		return l.HandleOpDone(jobID, op.ID, ops.Result{
			Status: agentjob.OpFailed,
			Error:  "unknown tool " + op.Tool,
		})
	}
	job, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	now := l.deps.Now()
	op.Status = agentjob.OpRunning
	op.StartedAt = &now
	if err := l.deps.Store.PutOp(jobID, op); err != nil {
		return err
	}
	return l.deps.Runner.Start(ops.Call{
		Name:      op.Tool,
		JobID:     jobID,
		OpID:      op.ID,
		Workspace: job.Spec.Workspace,
		Args:      op.Args,
		LogPath:   l.deps.Store.OpLogPath(jobID, op.ID),
		OnPID: func(pid int) {
			cur, err := l.deps.Store.GetOp(jobID, op.ID)
			if err != nil {
				return
			}
			cur.PID = pid
			_ = l.deps.Store.PutOp(jobID, cur)
		},
	})
}

// ask builds the prompt, sends it to the model in the leased slot and
// reads the stream to the Done chunk, which carries the complete reply
// and the token counts.
func (l *Loop) ask(ctx context.Context, jobID string, slotNum int) (engine.Message, int, error) {
	msgs, err := l.prompt(jobID)
	if err != nil {
		return engine.Message{}, 0, err
	}
	speclist := l.deps.Registry.Specs()
	tools := make([]engine.ToolSpec, 0, len(speclist))
	for _, s := range speclist {
		tools = append(tools, engine.ToolSpec{Name: s.Name, Description: s.Description, Schema: s.Schema})
	}

	stream, err := l.deps.Engine.Chat(ctx, engine.ChatRequest{
		Slot:        slotNum,
		Messages:    msgs,
		Tools:       tools,
		CachePrompt: true,
	})
	if err != nil {
		return engine.Message{}, 0, err
	}
	defer stream.Close()

	var (
		reply  engine.Message
		tokens int
		done   bool
	)
	for {
		ch, rerr := stream.Recv()
		switch {
		case rerr != nil && !errors.Is(rerr, io.EOF):
			err = rerr
		case ch.Err != nil:
			err = ch.Err
		case ch.Done:
			if ch.Message != nil {
				reply = *ch.Message
			}
			tokens = ch.PromptTokens + ch.OutputTokens
			done = true
		case errors.Is(rerr, io.EOF):
			err = errors.New("engine: stream ended before the done chunk")
		}
		if err != nil || done {
			break
		}
	}
	return reply, tokens, err
}

// prompt builds the messages of one tick: the fixed system message, the
// job's instructions, then one engine.Message per saved LoopMessage.
// The first two messages are byte-identical on every tick of the same
// job (the engine's prompt cache depends on it): nothing that changes —
// no time, no status — goes in them.
func (l *Loop) prompt(jobID string) ([]engine.Message, error) {
	j, err := l.deps.Store.Get(jobID)
	if err != nil {
		return nil, err
	}
	msgs := []engine.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: j.Spec.Instructions},
	}
	saved, err := l.deps.Store.Messages(jobID)
	if err != nil {
		return nil, err
	}
	for _, m := range saved {
		var out engine.Message
		switch m.Role {
		case "assistant":
			var a assistantContent
			if err := json.Unmarshal(m.Content, &a); err != nil {
				// A content that is a bare JSON string is a plain
				// answer with no tool calls.
				var s string
				if serr := json.Unmarshal(m.Content, &s); serr != nil {
					return nil, err
				}
				a.Content = s
			}
			out = engine.Message{Role: "assistant", Content: a.Content, ToolCalls: a.ToolCalls}
		case "tool":
			var tc toolContent
			if err := json.Unmarshal(m.Content, &tc); err != nil {
				return nil, err
			}
			out = engine.Message{Role: "tool", ToolCallID: tc.ToolCallID, Content: tc.Content}
		default:
			continue // system and user come from the spec, not the log
		}
		msgs = append(msgs, out)
	}
	return msgs, nil
}
