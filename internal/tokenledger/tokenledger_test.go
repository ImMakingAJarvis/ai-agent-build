package tokenledger

import (
	"os"
	"path/filepath"
	"testing"
)

func mustLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := New(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, filepath.Join(dir, "state.json")
}

func total(l *Ledger, engine string, k Kind) uint64 {
	totals := l.Totals()
	return totals[engine][k]
}

func TestObserveFirstSeenSeedsCumulative(t *testing.T) {
	l, _ := mustLedger(t)
	l.Observe("brain", map[Kind]uint64{KindPrompt: 22351, KindGenerated: 4491})
	if got := total(l, "brain", KindPrompt); got != 22351 {
		t.Fatalf("prompt cumulative = %d, want 22351", got)
	}
	if got := total(l, "brain", KindGenerated); got != 4491 {
		t.Fatalf("generated cumulative = %d, want 4491", got)
	}
}

func TestObserveAccumulatesAndHandlesReset(t *testing.T) {
	l, _ := mustLedger(t)
	// Normal growth: 100 -> 150.
	l.Observe("brain", map[Kind]uint64{KindPrompt: 100})
	l.Observe("brain", map[Kind]uint64{KindPrompt: 150})
	if got := total(l, "brain", KindPrompt); got != 150 {
		t.Fatalf("cumulative = %d, want 150", got)
	}
	// Engine restarts: counter re-arms at 140 (< last 150).
	l.Observe("brain", map[Kind]uint64{KindPrompt: 140})
	if got := total(l, "brain", KindPrompt); got != 290 {
		t.Fatalf("cumulative after reset = %d, want 290 (150+140)", got)
	}
	// Growth in the fresh run: 140 -> 160.
	l.Observe("brain", map[Kind]uint64{KindPrompt: 160})
	if got := total(l, "brain", KindPrompt); got != 310 {
		t.Fatalf("cumulative = %d, want 310 (150+160)", got)
	}
}

func TestObserveMissingKindsUntouched(t *testing.T) {
	l, _ := mustLedger(t)
	l.Observe("igpu", map[Kind]uint64{KindPrompt: 500})
	// A poll that only reports generated (engine binary lacks the
	// prompt counter) must not zero or move the prompt total.
	l.Observe("igpu", map[Kind]uint64{KindGenerated: 30})
	if got := total(l, "igpu", KindPrompt); got != 500 {
		t.Fatalf("prompt cumulative = %d, want 500 (untouched)", got)
	}
	if got := total(l, "igpu", KindGenerated); got != 30 {
		t.Fatalf("generated cumulative = %d, want 30", got)
	}
}

func TestEnginesAreIndependent(t *testing.T) {
	l, _ := mustLedger(t)
	l.Observe("brain", map[Kind]uint64{KindPrompt: 100})
	l.Observe("igpu", map[Kind]uint64{KindPrompt: 40})
	if got := total(l, "brain", KindPrompt); got != 100 {
		t.Fatalf("brain = %d, want 100", got)
	}
	if got := total(l, "igpu", KindPrompt); got != 40 {
		t.Fatalf("igpu = %d, want 40", got)
	}
}

func TestSaveAndReload(t *testing.T) {
	l, path := mustLedger(t)
	l.Observe("brain", map[Kind]uint64{KindPrompt: 100, KindGenerated: 5})
	if err := l.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	l2, err := New(path)
	if err != nil {
		t.Fatalf("New(reload): %v", err)
	}
	if got := total(l2, "brain", KindPrompt); got != 100 {
		t.Fatalf("reloaded prompt = %d, want 100", got)
	}
	// Growth continues from the reloaded base.
	l2.Observe("brain", map[Kind]uint64{KindPrompt: 130})
	if got := total(l2, "brain", KindPrompt); got != 130 {
		t.Fatalf("cumulative after reload = %d, want 130", got)
	}
}

func TestCorruptStateStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := New(path)
	if err != nil {
		t.Fatalf("New with corrupt state: %v", err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file was not preserved: %v", err)
	}
	l.Observe("brain", map[Kind]uint64{KindPrompt: 7})
	if got := total(l, "brain", KindPrompt); got != 7 {
		t.Fatalf("cumulative = %d, want 7", got)
	}
}

func TestParseCounters(t *testing.T) {
	body := `# HELP llamacpp:prompt_tokens_total Total number of tokens processed (prompt).
# TYPE llamacpp:prompt_tokens_total counter
llamacpp:prompt_tokens_total 22351
llamacpp:prompt_tokens_cached_total 86609
llamacpp:prompt_seconds_total 7.80483
llamacpp:tokens_predicted_total 4491
llamacpp:tokens_predicted_seconds_total 104.856
llamacpp:n_decode_total 4509
llamacpp:predicted_tokens_seconds 42.4895
llamacpp:requests_processing 0
`
	got, err := ParseCounters(body)
	if err != nil {
		t.Fatalf("ParseCounters: %v", err)
	}
	want := map[Kind]uint64{
		KindPrompt:       22351,
		KindPromptCached: 86609,
		KindGenerated:    4491,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("kind %s = %d, want %d", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d kinds (%v), want exactly %d (%v)", len(got), got, len(want), want)
	}
}

func TestParseCountersEmpty(t *testing.T) {
	got, err := ParseCounters("")
	if err != nil {
		t.Fatalf("ParseCounters: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsed %v, want nothing", got)
	}
}

func TestParseCountersScientificNotation(t *testing.T) {
	// llama.cpp prints large counters with 6 significant digits in
	// scientific notation (verified live: "1.72169e+06").
	body := "llamacpp:prompt_tokens_total 172169\nllamacpp:tokens_predicted_total 1.72169e+06\nllamacpp:prompt_tokens_cached_total 2e+07\n"
	got, err := ParseCounters(body)
	if err != nil {
		t.Fatalf("ParseCounters: %v", err)
	}
	if got[KindPrompt] != 172169 {
		t.Errorf("prompt = %d, want 172169", got[KindPrompt])
	}
	if got[KindGenerated] != 1721690 {
		t.Errorf("generated = %d, want 1721690 (quantised by source)", got[KindGenerated])
	}
	if got[KindPromptCached] != 20000000 {
		t.Errorf("cached = %d, want 20000000", got[KindPromptCached])
	}
}
