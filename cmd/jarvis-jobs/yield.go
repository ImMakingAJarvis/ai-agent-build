package main

// yield / waiting / wake — the per-job continuation and the waiting state
// (event-driven brain plan §3.5, EVB-13). A brain round that ends on
// purpose yields: it writes the job's continuation (what it was doing, what
// is in flight, the exact next step) and parks the job on the results it
// asked for. The continuation is the precondition of the yield — invariant
// 8: "Continuation is written before yield, jarvis-jobs yield refuses
// otherwise."

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// defaultWaitUntil is the wait deadline a yield gets when --wait-until is
// not passed: it matches the plan's default max brain-off window (2h).
// Invariant 2 (no job waits forever) is enforced by Store.Yield either way.
const defaultWaitUntil = "2h"

// cmdYield writes the job's continuation and parks it waiting on the named
// results:
//
//	jarvis-jobs yield <id> --wait-on corr[,corr...] --note TEXT
//	                 [--in-flight TEXT] [--next TEXT] [--results TEXT]
//	                 [--gotchas TEXT] [--wait-until 2h|RFC3339]
//
// It writes jobs/<id>/continuation.md — scaffolding the sections Goal /
// Done so far / In flight (corr ids) / What to do with each result / Next
// steps / Gotchas, prefilled from the job's title, the note and the
// per-corr placeholders (the way jarvis-swap handoff prefills its
// template) — and jobs/<id>/continuation.json, then flips the status to
// waiting via Store.Yield. Section flags fill their section when the
// continuation does not exist yet; when a continuation.md already exists
// (the round wrote it in its own words, or an earlier yield scaffolded it)
// the flags override only the sections they name and the file's own text
// fills the rest.
//
// The refusal: after writing the file, the CLI reads it back and checks
// the "In flight" and "Next steps" sections carry real content. Either
// empty — no flag, no file text — and the yield is refused: the status
// stays where it was, no continuation.json is written, and the (possibly
// freshly scaffolded) file is left on disk to be filled in and retried.
func cmdYield(store *agentjob.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: jarvis-jobs yield <id> --wait-on corr[,corr...] --note TEXT [--in-flight TEXT] [--next TEXT] [--results TEXT] [--gotchas TEXT] [--wait-until 2h|RFC3339]")
	}
	id := args[0]
	fs := flag.NewFlagSet("yield", flag.ContinueOnError)
	waitOn := fs.String("wait-on", "", "comma-separated correlation ids the job waits for (required)")
	note := fs.String("note", "", "the round's note for its successor (required)")
	inFlight := fs.String("in-flight", "", "what is in flight — fills the \"In flight\" section")
	next := fs.String("next", "", "the exact next step — fills the \"Next steps\" section")
	results := fs.String("results", "", "what to do with each result — fills the \"What to do with each result\" section")
	gotchas := fs.String("gotchas", "", "gotchas for the next round — fills the \"Gotchas\" section")
	waitUntil := fs.String("wait-until", defaultWaitUntil, "wait deadline: a duration (2h) or RFC3339 timestamp (default 2h)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *waitOn == "" || *note == "" {
		return fmt.Errorf("usage: jarvis-jobs yield <id> --wait-on corr[,corr...] --note TEXT [--in-flight TEXT] [--next TEXT] [--results TEXT] [--gotchas TEXT] [--wait-until 2h|RFC3339]")
	}
	var corrs []string
	for _, c := range strings.Split(*waitOn, ",") {
		if c = strings.TrimSpace(c); c != "" {
			corrs = append(corrs, c)
		}
	}
	if len(corrs) == 0 {
		return fmt.Errorf("yield: --wait-on names no correlation ids")
	}
	dl, err := parseWaitUntil(*waitUntil)
	if err != nil {
		return fmt.Errorf("yield: bad --wait-until: %v", err)
	}

	j, err := store.Get(id)
	if err != nil {
		return err
	}

	// Resolve the continuation's sections: a flag, if given, wins; then the
	// existing continuation.md's section (a round may have written the file
	// in its own words); then the scaffold prefill — the job's title, the
	// note, the per-corr placeholder.
	existing := map[string]string{}
	mdPath := store.ContinuationMDPath(id)
	mdExisted := false
	if b, err := os.ReadFile(mdPath); err == nil {
		mdExisted = true
		existing = agentjob.ParseContinuationMD(string(b))
	}
	content := map[string]string{
		agentjob.SectionGoal:    firstNonEmpty(existing[agentjob.SectionGoal], j.Title),
		agentjob.SectionDone:    firstNonEmpty(*note, existing[agentjob.SectionDone], "- (nothing recorded)"),
		agentjob.SectionFlight:  firstNonEmpty(*inFlight, existing[agentjob.SectionFlight]),
		agentjob.SectionResults: firstNonEmpty(*results, existing[agentjob.SectionResults]),
		agentjob.SectionNext:    firstNonEmpty(*next, existing[agentjob.SectionNext]),
		agentjob.SectionGotchas: firstNonEmpty(*gotchas, existing[agentjob.SectionGotchas], "- (none recorded)"),
	}
	if content[agentjob.SectionResults] == "" {
		var b strings.Builder
		for _, c := range corrs {
			fmt.Fprintf(&b, "- %s: (what to do when it arrives)\n", c)
		}
		content[agentjob.SectionResults] = strings.TrimRight(b.String(), "\n")
	}
	if err := os.WriteFile(mdPath, []byte(agentjob.RenderContinuationMD(id, content)), 0o644); err != nil {
		return err
	}

	// Invariant 8 — check the FILE before flipping the status. The check
	// runs on what is on disk, not on the in-memory render.
	b, err := os.ReadFile(mdPath)
	if err != nil {
		return err
	}
	inFlightFilled, nextFilled := agentjob.ContinuationMDFilled(string(b))
	if !inFlightFilled || !nextFilled {
		empty := []string{}
		if !inFlightFilled {
			empty = append(empty, "\"In flight\"")
		}
		if !nextFilled {
			empty = append(empty, "\"Next steps\"")
		}
		return fmt.Errorf("yield refused: %s of %s is empty — fill it in and re-run (the continuation must be written before the yield, invariant 8)",
			strings.Join(empty, " and "), mdPath)
	}

	// continuation.json: the machine-readable twin. RoundCount counts the
	// rounds in this continuation chain — 1 at the first yield, +1 on every
	// re-yield after a wake. ResultRefs seeds from the results that have
	// already arrived (a result that raced ahead of the yield must not be
	// lost).
	roundCount := 1
	if prev, err := store.ReadContinuation(id); err == nil {
		roundCount = prev.RoundCount + 1
	}
	refs := map[string]string{}
	if res, err := store.RecordedResults(id); err == nil {
		for corr, r := range res {
			refs[corr] = r.Ref
		}
	}
	c := &agentjob.Continuation{
		JobID:      id,
		Goal:       j.Title,
		WaitOn:     corrs,
		WaitUntil:  dl.UTC().Format(time.RFC3339),
		ResultRefs: refs,
		RoundCount: roundCount,
		LastDriver: j.RanWith,
		Note:       *note,
		At:         time.Now().UTC().Format(time.RFC3339),
	}
	jsonExisted := false
	if _, err := os.Stat(store.ContinuationJSONPath(id)); err == nil {
		jsonExisted = true
	}
	if err := store.WriteContinuation(c); err != nil {
		return err
	}
	if err := store.Yield(id, corrs, dl, *note); err != nil {
		// The store refused the yield (the job is not running, the
		// deadline is bad): roll back the files this invocation created so
		// a refused yield leaves no half state. A continuation.md the
		// round wrote itself is not ours to delete.
		if !jsonExisted {
			os.Remove(store.ContinuationJSONPath(id))
		}
		if !mdExisted {
			os.Remove(mdPath)
		}
		return err
	}
	if j2, err := store.Get(id); err == nil {
		j = j2
	}
	fmt.Printf("yielded: %s — waiting on %s until %s (deadline %s)\n",
		id, strings.Join(corrs, ", "), c.WaitUntil, agentjob.WaitRemaining(j, time.Now()))
	return nil
}

// parseWaitUntil accepts a duration relative to now ("2h", "45m") or an
// RFC3339 timestamp. Store.Yield enforces that the result is in the future
// (invariant 2).
func parseWaitUntil(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("want a duration (2h) or an RFC3339 timestamp, got %q", s)
	}
	return time.Now().UTC().Add(d), nil
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// cmdWaiting lists the waiting jobs — id, title, what the job waits on, how
// long it has been waiting, and its deadline:
//
//	<ID>  <title>
//	    waiting on c1, c2 — 25m (deadline 2026-11-09T00:05:00Z, in 95m)
//
// Oldest wait first. Prints nothing (and exits 0) when no job is waiting —
// the same convention as photo-ready.
func cmdWaiting(store *agentjob.Store, w io.Writer) error {
	jobs, err := store.List()
	if err != nil {
		return err
	}
	var waiting []agentjob.Job
	for _, j := range jobs {
		if j.Status == agentjob.Waiting {
			waiting = append(waiting, j)
		}
	}
	sort.Slice(waiting, func(i, k int) bool {
		return waiting[i].WaitingSince < waiting[k].WaitingSince
	})
	now := time.Now()
	for _, j := range waiting {
		fmt.Fprintf(w, "%s  %s\n", j.ID, j.Title)
		on := "on nothing (manual wake only)"
		if len(j.WaitOn) > 0 {
			on = "on " + strings.Join(j.WaitOn, ", ")
		}
		dl := j.WaitDeadline()
		deadline := agentjob.WaitRemaining(j, now)
		if !dl.IsZero() {
			deadline = dl.UTC().Format(time.RFC3339) + " (" + deadline + ")"
		}
		fmt.Fprintf(w, "    waiting %s — %s (deadline %s)\n", on, agentjob.WaitAge(j, now), deadline)
	}
	return nil
}

// cmdWake is the manual wake — for humans; the switchboard wakes
// automatically once it lands (plan task 14). It asks Store.Wake to wake the
// job, which honours the same gate as the automatic path: the job wakes
// (back to ready, priority bumped) when every WaitOn result has arrived or
// the deadline has passed, and stays waiting otherwise — the operator is
// told what is missing and that `jarvis-jobs requeue <id>` forces it ready.
func cmdWake(store *agentjob.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: jarvis-jobs wake <id>")
	}
	id := args[0]
	j, err := store.Get(id)
	if err != nil {
		return err
	}
	if j.Status != agentjob.Waiting {
		return fmt.Errorf("job %s is %s, not waiting", id, j.Status)
	}
	rec, err := store.Wake(id, "manual")
	if errors.Is(err, agentjob.ErrWaitOnUnmet) {
		missing, merr := store.MissingResults(id)
		if merr != nil {
			missing = []string{strings.Join(j.WaitOn, ", ")}
		}
		return fmt.Errorf("still waiting: %s not in yet (deadline %s) — the job stays waiting; `jarvis-jobs requeue %s` forces it ready",
			strings.Join(missing, ", "), agentjob.WaitRemaining(j, time.Now()), id)
	}
	if err != nil {
		return err
	}
	corrs := make([]string, 0, len(rec.Results))
	for c := range rec.Results {
		corrs = append(corrs, c)
	}
	sort.Strings(corrs)
	fmt.Printf("woken: %s — %s\n", id, rec.Reason)
	for _, c := range corrs {
		fmt.Printf("  %s -> %s\n", c, rec.Results[c])
	}
	return nil
}
