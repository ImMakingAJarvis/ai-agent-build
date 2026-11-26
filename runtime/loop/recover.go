// recover.go recovers the operations of a loop job after loopd restarted
// (ELR-1, M3.1b, card ELR-32): the ops a torn checkpoint left missing are
// rebuilt by Store.RecoverLoop, an op whose process survived the restart
// is adopted (a watcher finishes it when the process exits), an op whose
// process is gone is finished from its .exit file or lost, a pending op
// is started again, and every job that ends up ready is put back in the
// ready list.
package loop

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

// recoverPoll is how often the watcher of an adopted op checks whether
// its process is still alive.
const recoverPoll = 2 * time.Second

// recoveredHead is the first line of the output of an op that finished
// while loopd was down: the shell wrapper recorded its real exit code on
// disk, so the op is reported done or failed with that code (card
// ELR-90).
const recoveredHead = "[finished while loopd was restarting]"

// lostOpText is the error a lost op reports: the command may have taken
// effect before loopd died, so the model must check the result before
// repeating the operation (card ELR-90).
const lostOpText = "operation was lost when loopd restarted. It may or may not have taken effect. Check the result first (for example git status, git log origin/<branch>, gh pr list) and repeat it only if it did not happen."

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
// its PID known and the process alive, a watcher adopts it and finishes
// it when the process exits; the PID missing (0) or the process gone,
// the op is finished right away (card ELR-90).
func (l *Loop) recoverRunningOp(jobID string, op agentjob.LoopOp) error {
	if op.PID > 0 && processAlive(op.PID) {
		go l.watchRecoveredOp(jobID, op.ID, op.PID)
		return nil
	}
	return l.finishGoneOp(jobID, op.ID)
}

// watchRecoveredOp polls the adopted process every recoverPoll and, when
// it is gone, finishes the op the same way as a process that was already
// gone at recovery time.
func (l *Loop) watchRecoveredOp(jobID, opID string, pid int) {
	for processAlive(pid) {
		time.Sleep(recoverPoll)
	}
	_ = l.finishGoneOp(jobID, opID)
}

// finishGoneOp reports an op whose process is gone (card ELR-90): the
// shell wrapper wrote the command's exit code to <log>.exit as soon as
// it finished, so a command that finished while loopd was down is
// reported done (code 0) or failed with the real code and the tail of
// its log; only when the file is missing was the op lost, and then the
// model is told to check the result before repeating it.
func (l *Loop) finishGoneOp(jobID, opID string) error {
	logPath := l.deps.Store.OpLogPath(jobID, opID)
	if code, ok := readExitFile(logPath + ".exit"); ok {
		res := ops.Result{Status: agentjob.OpDone, ExitCode: code}
		if code != 0 {
			res.Status = agentjob.OpFailed
			res.Error = fmt.Sprintf("command failed with exit code %d", code)
		}
		out := recoveredHead
		if tail := lastLogTail(logPath, 64<<10); tail != "" {
			out += "\n" + tail
		}
		res.Output = out
		return l.HandleOpDone(jobID, opID, res)
	}
	return l.HandleOpDone(jobID, opID, ops.Result{
		Status: agentjob.OpLost,
		Error:  lostOpText,
	})
}

// readExitFile reads the exit code the shell wrapper wrote to
// <log path>.exit (card ELR-90); ok is false when the file is missing
// or does not hold a number.
func readExitFile(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return code, true
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
