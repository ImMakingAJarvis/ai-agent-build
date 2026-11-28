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
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

// emptyReplyNote is the user message appended after a tick whose reply
// has neither text nor tool calls (card ELR-88): the job is not done,
// the model gets one more try, and the guard in Step fails it if the
// empty replies keep coming.
const emptyReplyNote = "[runtime] your reply was empty. Continue the task, or reply with a short summary if it is finished."

// clip500 shortens s to at most 500 runes, so a runtime note stays a
// small user message no matter how long the error text is.
func clip500(s string) string {
	r := []rune(s)
	if len(r) > 500 {
		return string(r[:500])
	}
	return s
}

// appendUserNote appends one user message holding a plain text note, in
// the JSON string shape every user message in the log has.
func (l *Loop) appendUserNote(jobID, note string) error {
	b, err := json.Marshal(note)
	if err != nil {
		return err
	}
	_, err = l.deps.Store.AppendMessages(jobID, agentjob.LoopMessage{Role: "user", Content: b})
	return err
}

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

// acceptIDPrefix is the id of an acceptance-check op (card ELR-89):
// accept-<n>, with the commands taken from the job's spec (M1.2a),
// never from the model.
const acceptIDPrefix = "accept-"

// nextAcceptID returns the id of the job's next acceptance op:
// accept-<n> with n one past the count of its existing accept- ops
// (card ELR-89).
func (l *Loop) nextAcceptID(jobID string) (string, error) {
	ops, err := l.afterEditOps(jobID)
	if err != nil {
		return "", err
	}
	n := 0
	for _, op := range ops {
		if strings.HasPrefix(op.ID, acceptIDPrefix) {
			n++
		}
	}
	return acceptIDPrefix + strconv.Itoa(n+1), nil
}

// tick runs one tick for jobID in the given order: acquire a slot, mark
// the job running, ask the model, checkpoint the result (message, ops,
// state), release the slot, start the tools. slot.ErrNoSlot is returned
// unchanged: the job stays where it is and the next ready job may take
// the slot.
func (l *Loop) tick(ctx context.Context, jobID string) error {
	start := l.deps.Now()
	lease, err := l.deps.Slots.Acquire(ctx, jobID)
	if err != nil {
		return err // no slot, no tick: nothing to record
	}

	// The tick is recorded when it ends, by which time the outcome is
	// known (final, tool_calls, error). A closure: defer evaluates its
	// arguments when the defer statement runs, so a plain call would
	// record the defaults, not the values the tick settled on.
	var (
		outcome   = tickOutcomeError
		promptTok int
		outputTok int
	)
	defer func() {
		l.deps.Metrics.recordTick(outcome, l.deps.Now().Sub(start), promptTok, outputTok, lease.Warm, lease.Restored)
	}()

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
	reply, promptTok, outputTok, err := l.ask(ctx, jobID, lease.Slot)
	if err != nil {
		// The model round could not be used (card ELR-88): before the job
		// goes back to ready, leave the model one note about it, so the
		// next tick can answer the failure instead of just retrying blind.
		if nerr := l.appendUserNote(jobID, "[runtime] your last reply could not be used: "+clip500(err.Error())+". Reply again."); nerr != nil {
			giveBack()
			return nerr
		}
		giveBack()
		return err
	}
	tokens := promptTok + outputTok

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
	switch {
	case len(tickOps) > 0:
		cs.Status = agentjob.Waiting
		cs.WaitReason = agentjob.WaitOperation
		outcome = tickOutcomeToolCalls
	case len(job.Spec.Acceptance) > 0 && strings.TrimSpace(reply.Content) != "":
		// The model claims the task is done, but the job carries
		// acceptance commands (M1.2a): a job is done only when they
		// pass, so the runtime runs them itself as one internal shell
		// op and the op's result settles the job (card ELR-89).
		id, aerr := l.nextAcceptID(jobID)
		if aerr != nil {
			giveBack()
			return aerr
		}
		args, aerr := json.Marshal(map[string]any{
			"cmd": strings.Join(job.Spec.Acceptance, " && "),
		})
		if aerr != nil {
			giveBack()
			return aerr
		}
		tickOps = append(tickOps, agentjob.LoopOp{
			ID: id, Tool: "shell", Args: args, Status: agentjob.OpPending,
		})
		cs.Status = agentjob.Waiting
		cs.WaitReason = agentjob.WaitOperation
		outcome = tickOutcomeToolCalls
	case strings.TrimSpace(reply.Content) != "":
		cs.Status = agentjob.Done
		cs.WaitReason = ""
		cs.Summary = reply.Content
		outcome = tickOutcomeFinal
	default:
		// An empty reply is not a finished job (card ELR-88): the tick is
		// checkpointed like any other, the job goes back to ready, and a
		// note (below) tells the model what to do next. The guard in Step
		// fails the job if the empty replies keep coming.
		cs.Status = agentjob.Ready
		cs.WaitReason = ""
		outcome = tickOutcomeError
	}
	if err := l.deps.Store.Checkpoint(jobID, agentjob.Checkpoint{
		Messages: []agentjob.LoopMessage{{Role: "assistant", Content: content, Tokens: tokens}},
		Ops:      tickOps,
		State:    cs,
	}); err != nil {
		giveBack()
		return err
	}
	if cs.Status == agentjob.Ready {
		if err := l.appendUserNote(jobID, emptyReplyNote); err != nil {
			l.deps.Slots.Release(lease, tokens)
			return err
		}
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
	var argsObj map[string]any
	if err := json.Unmarshal(op.Args, &argsObj); err != nil {
		// A call whose arguments are not a JSON object is not started
		// (card ELR-88): the failure reaches the model on the next tick,
		// the same way an unknown tool does, so the model can resend the
		// call with valid JSON.
		return l.HandleOpDone(jobID, op.ID, ops.Result{
			Status: agentjob.OpFailed,
			Error:  "arguments are not a JSON object: " + err.Error() + ". Send the call again with valid JSON.",
		})
	}
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
	// The permission gate (card ELR-107): for the tools the permission
	// layer targets, Decide runs before the op starts. Allow falls
	// through to the start below; Deny finishes the op at once with the
	// denial as its tool result; Ask parks the op as awaiting_approval
	// with a pending approval, and the job waits on it — the tick
	// released the slot before startOp, so a parked job holds none.
	if d, call := permGate(l.deps.Rules, op.Tool, argsObj, job.Spec.Workspace); d != perm.Allow {
		if d == perm.Deny {
			return l.HandleOpDone(jobID, op.ID, ops.Result{
				Status: agentjob.OpFailed,
				Error:  deniedReason(l.deps.Rules, call),
			})
		}
		return l.parkForApproval(jobID, op)
	}
	now := l.deps.Now()
	op.Status = agentjob.OpRunning
	op.StartedAt = &now
	if err := l.deps.Store.PutOp(jobID, op); err != nil {
		return err
	}
	err = l.deps.Runner.Start(ops.Call{
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
	if err != nil {
		return err
	}
	l.markOpStarted(op.ID)
	return nil
}

// permGate returns the permission decision for the call (card ELR-107)
// and the ToolCall Decide saw. Only the tools the permission layer
// targets are gated: the shell — its "cmd" presented under the
// "command" key Decide reads — and the file tools, which Decide reads
// by their "path". A tool outside the layer's model, or a call with no
// command or path to match, is not gated: it runs as today.
func permGate(rules perm.Rules, tool string, argsObj map[string]any, workspace string) (perm.Decision, perm.ToolCall) {
	call := perm.ToolCall{Tool: tool, Workspace: workspace}
	switch tool {
	case "shell":
		cmd, ok := argsObj["cmd"].(string)
		if !ok || cmd == "" {
			return perm.Allow, call
		}
		b, err := json.Marshal(map[string]string{"command": cmd})
		if err != nil {
			return perm.Allow, call
		}
		call.Args = b
	case "read_file", "write_file", "edit_file":
		p, ok := argsObj["path"].(string)
		if !ok || p == "" {
			return perm.Allow, call
		}
		// The path is the only field Decide reads; the rest of the args
		// (content, ...) stay out of the decision.
		b, err := json.Marshal(map[string]string{"path": p})
		if err != nil {
			return perm.Allow, call
		}
		call.Args = b
	default:
		return perm.Allow, call
	}
	return perm.Decide(rules, call), call
}

// deniedReason names the rule behind a Deny decision (card ELR-107):
// "denied: <tool> <pattern>" for the deny rule that matched, or the
// guard's refusal when a shell command was blocked that no rule named.
// Decide itself is pure and names no rule, so the rescan mirrors perm's
// matching over the same target Decide saw: the command (shell) or the
// path (file tools).
func deniedReason(rules perm.Rules, call perm.ToolCall) string {
	var target string
	var m map[string]any
	if json.Unmarshal(call.Args, &m) == nil {
		switch call.Tool {
		case "shell":
			target, _ = m["command"].(string)
		case "read_file", "write_file", "edit_file":
			target, _ = m["path"].(string)
		}
	}
	for _, r := range rules.Deny {
		if r.Tool != call.Tool || target == "" {
			continue
		}
		if ok, _ := path.Match(r.Pattern, target); ok {
			return "denied: " + r.Tool + " " + r.Pattern
		}
	}
	return "denied: " + call.Tool + " command blocked by the guard"
}

// parkForApproval parks an op the permission gate sent to the owner
// (card ELR-107): the op waits awaiting_approval, a pending LoopApproval
// carries the call, its hash and the remind and expiry times, and the
// job checkpoints Waiting on the approval. The slot was released by the
// tick before startOp, so the parked job holds none.
func (l *Loop) parkForApproval(jobID string, op agentjob.LoopOp) error {
	now := l.deps.Now()
	op.Status = agentjob.OpAwaitingApproval
	if err := l.deps.Store.PutOp(jobID, op); err != nil {
		return err
	}
	remindAt := now.Add(time.Duration(l.deps.Config.ApprovalRemindMinutes) * time.Minute)
	expiresAt := now.Add(time.Duration(l.deps.Config.ApprovalExpiryHours) * time.Hour)
	req, err := json.Marshal(struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}{op.Tool, op.Args})
	if err != nil {
		return err
	}
	a := agentjob.LoopApproval{
		ID:        op.ID,
		OpID:      op.ID,
		Request:   req,
		Status:    agentjob.ApprovalPending,
		Hash:      agentjob.ApprovalHash(op.Tool, op.Args),
		RemindAt:  &remindAt,
		ExpiresAt: &expiresAt,
	}
	if err := l.deps.Store.PutApproval(jobID, a); err != nil {
		return err
	}
	job, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	st := job.State
	st.Status = agentjob.Waiting
	st.WaitReason = agentjob.WaitApproval
	return l.deps.Store.SetStatus(jobID, st)
}

// ask builds the prompt, sends it to the model in the leased slot and
// reads the stream to the Done chunk, which carries the complete reply
// and the prompt and output token counts.
func (l *Loop) ask(ctx context.Context, jobID string, slotNum int) (engine.Message, int, int, error) {
	msgs, err := l.prompt(jobID)
	if err != nil {
		return engine.Message{}, 0, 0, err
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
		return engine.Message{}, 0, 0, err
	}
	defer stream.Close()

	var (
		reply     engine.Message
		promptTok int
		outputTok int
		done      bool
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
			promptTok = ch.PromptTokens
			outputTok = ch.OutputTokens
			done = true
		case errors.Is(rerr, io.EOF):
			err = errors.New("engine: stream ended before the done chunk")
		}
		if err != nil || done {
			break
		}
	}
	return reply, promptTok, outputTok, err
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
		case "user":
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				return nil, err
			}
			if s == j.Spec.Instructions {
				continue // the instructions are already the second message
			}
			// A user message the model did not ask for — the after_edit
			// check result (card ELR-37) reaches the model here.
			out = engine.Message{Role: "user", Content: s}
		default:
			continue // the system message comes from the constant, not the log
		}
		msgs = append(msgs, out)
	}
	return msgs, nil
}
