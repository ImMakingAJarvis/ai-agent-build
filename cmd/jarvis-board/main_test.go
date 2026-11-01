package main

import "testing"

func TestCommentHTML(t *testing.T) {
	cases := []struct {
		model, msg, want string
	}{
		{"jarvis-brain", "moved to In Progress", "<p>jarvis-brain: moved to In Progress</p>"},
		{"Qwen", `a <b> "quoted" & 'single'`, `<p>Qwen: a &lt;b&gt; &#34;quoted&#34; &amp; &#39;single&#39;</p>`},
		{"jarvis-brain", "line one\nline two", "<p>jarvis-brain: line one<br>line two</p>"},
	}
	for _, tc := range cases {
		got := commentHTML(tc.model, tc.msg)
		if got != tc.want {
			t.Errorf("commentHTML(%q, %q) = %q, want %q", tc.model, tc.msg, got, tc.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "AAB", "B"); got != "AAB" {
		t.Errorf("firstNonEmpty = %q, want AAB", got)
	}
	if got := firstNonEmpty("", "  "); got != "" {
		t.Errorf("firstNonEmpty(no values) = %q, want empty", got)
	}
}

func TestMessageJoinsArgs(t *testing.T) {
	if got := message([]string{"moved", "AAB-4", "to", "In Progress"}); got != "moved AAB-4 to In Progress" {
		t.Errorf("message = %q", got)
	}
}
