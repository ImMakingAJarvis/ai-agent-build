package boardnote

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestRefFromTitle(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"Plane: run task T2I-3 (generate the image)", "T2I-3"},
		{"EVB-16: agent-run resumes with continuation", "EVB-16"},
		{"EVB-17 (plan task 16) — board-mapped", "EVB-17"},
		{"two refs T2I-3 and EVB-1 keep the first", "T2I-3"},
		{"no board ref in this title", ""},
		{"plan-plan-event-driven-brain-1794016330-16", ""}, // job id: lowercase, no ref
		{"fix PR #211 and close it", ""},                  // PR numbers are not board refs
		{"ABC-123: long identifier, still a ref", "ABC-123"},
		{"A-1 is too short an identifier", ""},
		{"abcdefghijk-1 is too long an identifier", ""},
	}
	for _, tc := range cases {
		if got := RefFromTitle(tc.title); got != tc.want {
			t.Errorf("RefFromTitle(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

func TestYieldMessage(t *testing.T) {
	msg := YieldMessage("- corr-1: photo generation on the 4090\n- and more detail\n",
		"- verify the photos\n- post the PR\n")
	if want := "waiting on corr-1: photo generation on the 4090, will resume to verify the photos"; msg != want {
		t.Errorf("YieldMessage = %q, want %q", msg, want)
	}
	// Empty sections fall back to plain words, never to blanks.
	if got := YieldMessage("", ""); got != "waiting on the results in flight, will resume to its next step" {
		t.Errorf("YieldMessage(empty) = %q", got)
	}
	// A long first line is truncated to 120 chars with an ellipsis.
	long := strings.Repeat("x", 200)
	if got := YieldMessage(long, "next"); len(got) > 120+50 {
		t.Errorf("YieldMessage did not truncate: %d chars", len(got))
	}
}

func TestWakeMessages(t *testing.T) {
	// All results in: one line per corr, sorted.
	rec := agentjob.WakeRecord{Reason: agentjob.WakeAll, Results: map[string]string{
		"corr-b": "b-result",
		"corr-a": "a-result",
	}}
	got := WakeMessages(rec, 0)
	if len(got) != 2 || got[0] != "resumed, got corr-a: a-result" || got[1] != "resumed, got corr-b: b-result" {
		t.Errorf("WakeMessages(all) = %v", got)
	}
	// Timeout: one note, the missing count carried through.
	got = WakeMessages(agentjob.WakeRecord{Reason: agentjob.WakeTimeout}, 2)
	if len(got) != 1 || got[0] != "resumed — wait timed out (2 result(s) missing)" {
		t.Errorf("WakeMessages(timeout) = %v", got)
	}
	// A manual wake with no recorded results: a bare resumed line.
	got = WakeMessages(agentjob.WakeRecord{Reason: agentjob.WakeAll}, 0)
	if len(got) != 1 || got[0] != "resumed" {
		t.Errorf("WakeMessages(bare) = %v", got)
	}
}

func TestCommentHTML(t *testing.T) {
	got := commentHTML("jarvis-brain", "waiting on <corr> & more")
	want := "<p>jarvis-brain: waiting on &lt;corr&gt; &amp; more</p>"
	if got != want {
		t.Errorf("commentHTML = %q, want %q", got, want)
	}
	// Newlines become <br>, like jarvis-board.
	got = commentHTML("m", "a\nb")
	if want := "<p>m: a<br>b</p>"; got != want {
		t.Errorf("commentHTML newline = %q, want %q", got, want)
	}
}

func TestPostNoBoardRefIsNoop(t *testing.T) {
	// A title without a board ref posts nothing and must not error — even
	// without any Plane config.
	j := agentjob.Job{Spec: agentjob.Spec{Title: "no board ref here"}}
	if err := PostYield(j, "in flight", "next"); err != nil {
		t.Errorf("PostYield (unmapped) = %v, want nil", err)
	}
	if err := PostWake(j, agentjob.WakeRecord{Reason: agentjob.WakeAll}, 0); err != nil {
		t.Errorf("PostWake (unmapped) = %v, want nil", err)
	}
}
