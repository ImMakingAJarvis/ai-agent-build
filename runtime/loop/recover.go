// recover.go recovers the operations of a loop job after loopd restarted
// (ELR-1, M3.1b, card ELR-32): the ops a torn checkpoint left missing are
// rebuilt by Store.RecoverLoop, an op whose process survived the restart
// is adopted (a watcher reports it done when the process exits), an op
// whose process is gone is lost, a pending op is started again, and every
// job that ends up ready is put back in the ready list.
package loop

import (
	"os"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// recoverPoll is how often the watcher of an adopted op checks whether
// its process is still alive.
const recoverPoll = 2 * time.Second

// recoveredHead is the first line of the output of an adopted op: we did
// not start the process, so its exit code is unknown.
const recoveredHead = "[finished while loopd was restarting; exit code unknown]"

// Recover puts every job back on the rails after loopd restarted:
// Store.RecoverLoop rebuilds the missing ops, each running op is adopted
// or lost, each pending op is started again through startOp, and every
// job whose status is ready is submitted.
func (l *Loop) Recover(jobIDs []string) error {
	for _, jobID := range jobIDs {
		if err := l.recoverJob(jobID); err != nil {
			return err
		}
	}
	return nil
}

// recoverJob recovers one job (see Recover).
func (l *Loop) recoverJob(jobID string) error {
	if err := l.deps.Store.RecoverLoop(jobID); err != nil {
		return err
	}
	jobOps, err := l.deps.Store.Ops(jobID)
	if err != nil {
		return err
	}
	for _, op := range jobOps {
		switch op.Status {
		case agentjob.OpRunning:
			if err := l.recoverRunningOp(jobID, op); err != nil {
				return err
			}
		case agentjob.OpPending:
			if err := l.startOp(jobID, op); err != nil {
				return err
			}
		}
	}
	// Every op is now finished, running under a watcher, or running in
	// the Runner: a job whose status is ready goes back in the ready
	// list (the jobs HandleOpDone woke are already submitted).
	j, err := l.deps.Store.Get(jobID)
	if err != nil {
		return err
	}
	if j.Status == agentjob.Ready {
		return l.Submit(jobID)
	}
	return nil
}

// recoverRunningOp disposes of one op that was running when loopd died:
// its PID known and the process alive, a watcher adopts it and reports
// it done when the process exits; the PID missing (0) or the process
// gone, the op is lost.
func (l *Loop) recoverRunningOp(jobID string, op agentjob.LoopOp) error {
	if op.PID > 0 && processAlive(op.PID) {
		go l.watchRecoveredOp(jobID, op.ID, op.PID)
		return nil
	}
	return l.HandleOpDone(jobID, op.ID, ops.Result{
		Status: agentjob.OpLost,
		Error:  "operation was lost when loopd restarted",
	})
}

// watchRecoveredOp polls the adopted process every recoverPoll and, when
// it is gone, reports the op done with the tail of its log: we did not
// start the process, so its exit code is unknown and the first line of
// the output says so.
func (l *Loop) watchRecoveredOp(jobID, opID string, pid int) {
	for processAlive(pid) {
		time.Sleep(recoverPoll)
	}
	out := recoveredHead
	if tail := lastLogTail(l.deps.Store.OpLogPath(jobID, opID), 64<<10); tail != "" {
		out += "\n" + tail
	}
	_ = l.HandleOpDone(jobID, opID, ops.Result{Status: agentjob.OpDone, Output: out})
}

// processAlive reports whether a process with pid exists.
func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// lastLogTail returns the last n bytes of the file at path, or "" when
// the file is missing or unreadable (the op never produced output).
func lastLogTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
