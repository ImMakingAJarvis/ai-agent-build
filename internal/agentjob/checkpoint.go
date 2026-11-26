package agentjob

import (
	"encoding/json"
	"errors"
)

// Checkpoint saves one tick of the loop (ELR M1.2d): the new messages,
// the operations they start, and the new state. The store has no
// transactions, so the order of the writes is the design: messages
// first, then ops, then the state — a crash anywhere in the middle
// leaves a job that RecoverLoop can put back on the rails.
type Checkpoint struct {
	Messages []LoopMessage
	Ops      []LoopOp
	State    State
}

// LoopSchemaVersion is the layout version of the loop files (ELR M1.2f)
// that this runtime writes and understands: the messages log, the ops,
// and the state that ties them together.
const LoopSchemaVersion = 1

// ErrNewerSchema is returned when a job's loop files carry a schema
// version newer than this runtime understands (LoopSchema >
// LoopSchemaVersion): the runtime refuses to touch the job rather than
// corrupt it.
var ErrNewerSchema = errors.New("job was written by a newer runtime")

// failAfter is a test hook: Checkpoint returns errInjected right after
// finishing step n (1 = messages appended, 2 = ops written). 0 = never.
var failAfter int

// errInjected is the error the failAfter hook makes Checkpoint return.
var errInjected = errors.New("injected failure")

// Checkpoint saves one tick of the loop (see Checkpoint). It writes the
// three parts in the crash-safe order — messages, then ops, then the
// state — and returns an error from any step at once; the steps already
// written stay, and RecoverLoop rebuilds the rest from the message log.
// Before writing anything it refuses a job whose loop files carry a
// schema version newer than LoopSchemaVersion (ErrNewerSchema).
func (s *Store) Checkpoint(id string, cp Checkpoint) error {
	job, err := s.Get(id)
	if err != nil {
		return err
	}
	if job.State.LoopSchema > LoopSchemaVersion {
		return ErrNewerSchema
	}
	if _, err := s.AppendMessages(id, cp.Messages...); err != nil {
		return err
	}
	if failAfter == 1 {
		return errInjected
	}
	for _, op := range cp.Ops {
		if err := s.PutOp(id, op); err != nil {
			return err
		}
	}
	if failAfter == 2 {
		return errInjected
	}
	cp.State.LoopSchema = LoopSchemaVersion
	return s.SetStatus(id, cp.State)
}

// RecoverLoop puts a job back on the rails after a crash mid-checkpoint
// (ELR M1.2d). The message log is the truth: it rebuilds the ops the
// last assistant message's tool calls never got, and leaves the job
// waiting on its unanswered tool calls (or ready when it has none). It
// refuses a job whose loop files carry a schema version newer than
// LoopSchemaVersion (ErrNewerSchema).
func (s *Store) RecoverLoop(id string) error {
	job, err := s.Get(id)
	if err != nil {
		return err
	}
	if job.State.LoopSchema > LoopSchemaVersion {
		return ErrNewerSchema
	}
	msgs, err := s.Messages(id)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}

	// The last assistant message is the newest tick: its tool calls are
	// the ones that may still lack ops.
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			lastAssistant = i
			break
		}
	}

	// Decode that message's content as a tool-call message. A plain-text
	// answer is not a tool-call object and decodes as no tool calls.
	type toolCall struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var calls []toolCall
	if lastAssistant >= 0 {
		var last struct {
			ToolCalls []toolCall `json:"tool_calls"`
		}
		if err := json.Unmarshal(msgs[lastAssistant].Content, &last); err == nil {
			calls = last.ToolCalls
		}
	}

	// Rebuild the ops the checkpoint never wrote: every tool call with
	// no op yet gets a pending op named after the call.
	ops, err := s.Ops(id)
	if err != nil {
		return err
	}
	for _, tc := range calls {
		known := false
		for _, op := range ops {
			if op.ToolCallID == tc.ID {
				known = true
				break
			}
		}
		if known {
			continue
		}
		op := LoopOp{ID: tc.ID, ToolCallID: tc.ID, Tool: tc.Name, Args: tc.Arguments, Status: OpPending}
		if err := s.PutOp(id, op); err != nil {
			return err
		}
		ops = append(ops, op)
	}

	// A call is answered when a later tool message carries its
	// tool_call_id.
	answered := 0
	for _, tc := range calls {
		for j := lastAssistant + 1; j < len(msgs); j++ {
			if msgs[j].Role != "tool" {
				continue
			}
			var tool struct {
				ToolCallID string `json:"tool_call_id"`
			}
			if json.Unmarshal(msgs[j].Content, &tool) == nil && tool.ToolCallID == tc.ID {
				answered++
				break
			}
		}
	}

	// Only a running or waiting job needs putting back; in any other
	// state the loop already left it where it belongs.
	j, err := s.Get(id)
	if err != nil {
		return err
	}
	if j.Status != Running && j.Status != Waiting {
		return nil
	}
	target := j.State
	if len(calls) > answered {
		target.Status = Waiting
		target.WaitReason = WaitOperation
	} else {
		target.Status = Ready
		target.WaitReason = ""
	}
	if target.Status == j.Status && target.WaitReason == j.WaitReason {
		return nil // nothing changes
	}
	return s.SetStatus(id, target)
}
