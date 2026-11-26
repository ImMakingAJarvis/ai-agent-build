package agentjob

import "errors"

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

// failAfter is a test hook: Checkpoint returns errInjected right after
// finishing step n (1 = messages appended, 2 = ops written). 0 = never.
var failAfter int

// errInjected is the error the failAfter hook makes Checkpoint return.
var errInjected = errors.New("injected failure")

// Checkpoint saves one tick of the loop (see Checkpoint).
func (s *Store) Checkpoint(id string, cp Checkpoint) error {
	return errors.New("not implemented")
}

// RecoverLoop puts a job back on the rails after a crash mid-checkpoint
// (ELR M1.2d): it rebuilds anything the checkpoint never wrote and
// leaves the job in the state the loop can continue from.
func (s *Store) RecoverLoop(id string) error {
	return errors.New("not implemented")
}
