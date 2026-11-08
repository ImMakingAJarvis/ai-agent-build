// Package tokenledger folds llama.cpp engine token counters into a
// persistent all-time total.
//
// The engine's own /metrics counters (llamacpp:*_total) reset whenever
// the engine process restarts (brain swaps, model changes, reboots), so
// they only describe the current run. The ledger polls each engine's
// metrics endpoint, folds the counter deltas into per-engine cumulatives
// persisted to a JSON state file, and re-exposes those cumulatives as a
// Prometheus counter that never resets. Grafana then shows the historical
// all-time total plus per-day/week/month buckets (increase over [1d],
// [1w], [1M]) without depending on Prometheus retention for the headline
// number.
package tokenledger

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Kind identifies one of the token counters tracked per engine.
type Kind string

const (
	KindPrompt       Kind = "prompt"        // tokens processed on the prompt side
	KindGenerated    Kind = "generated"     // tokens decoded/predicted
	KindPromptCached Kind = "prompt_cached" // prompt tokens served from cache
)

// Counters maps a tracked kind to the raw counter name exposed by
// llama-server's /metrics endpoint.
var Counters = map[Kind]string{
	KindPrompt:       "llamacpp:prompt_tokens_total",
	KindGenerated:    "llamacpp:tokens_predicted_total",
	KindPromptCached: "llamacpp:prompt_tokens_cached_total",
}

// stateEntry is the persisted state for one engine + counter.
type stateEntry struct {
	Last       uint64 `json:"last"`       // last raw value observed
	Cumulative uint64 `json:"cumulative"` // all-time total since first observation
}

type state struct {
	Engines map[string]map[Kind]stateEntry `json:"engines"`
}

// Ledger accumulates engine counters into persistent all-time totals.
// All methods are safe for concurrent use.
type Ledger struct {
	mu    sync.Mutex
	st    *state
	path  string
	saves int // state writes since load; 0 = nothing changed yet
}

// New loads the ledger from path (missing or corrupt file starts fresh,
// except corrupt files are preserved as <name>.corrupt for inspection).
func New(path string) (*Ledger, error) {
	l := &Ledger{path: path, st: &state{Engines: map[string]map[Kind]stateEntry{}}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, l.st); err != nil {
		_ = os.Rename(path, path+".corrupt")
		l.st = &state{Engines: map[string]map[Kind]stateEntry{}}
		return l, nil
	}
	if l.st.Engines == nil {
		l.st.Engines = map[string]map[Kind]stateEntry{}
	}
	return l, nil
}

// Observe folds one poll of raw counter values for engine into the
// cumulatives. Kinds absent from values are left untouched (an engine
// binary that lacks a counter does not zero its history).
//
// A value lower than the last observed value means the engine restarted
// and the counter re-armed: the new value is the total of the fresh run,
// so it is added on top. At most one poll interval of the old run is
// lost at a restart, which is acceptable.
func (l *Ledger) Observe(engine string, values map[Kind]uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	eng, ok := l.st.Engines[engine]
	if !ok {
		eng = map[Kind]stateEntry{}
		l.st.Engines[engine] = eng
	}
	for kind, v := range values {
		e, seen := eng[kind]
		switch {
		case !seen:
			e = stateEntry{Last: v, Cumulative: v}
		case v >= e.Last:
			e.Cumulative += v - e.Last
			e.Last = v
		default: // counter reset (engine restart)
			e.Cumulative += v
			e.Last = v
		}
		eng[kind] = e
	}
	l.saves++
}

// Totals returns a copy of the current per-engine, per-kind cumulatives.
func (l *Ledger) Totals() map[string]map[Kind]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]map[Kind]uint64{}
	for eng, m := range l.st.Engines {
		out[eng] = map[Kind]uint64{}
		for k, e := range m {
			out[eng][k] = e.Cumulative
		}
	}
	return out
}

// Save persists the state atomically (tmp file + rename). It is a no-op
// when nothing has changed since load or the last save.
func (l *Ledger) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.saves == 0 {
		return nil
	}
	data, err := json.MarshalIndent(l.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	l.saves = 0
	return nil
}

// ParseCounters extracts the tracked llamacpp:* counters from a
// Prometheus text-format /metrics body. Counter names contain ':' and
// are written unquoted by llama.cpp, so the body is scanned line by
// line for an exact "name value" match. Kinds whose counter is absent
// are omitted from the result.
//
// Values are parsed as floats: llama.cpp prints them with 6
// significant digits and switches to scientific notation at 1e6
// ("1.72169e+06"), so the last digit(s) of large counters are
// quantised by the source itself. The accumulated total therefore
// drifts by at most half a quantisation step per crossing — fine for
// dashboard-scale numbers.
func ParseCounters(body string) (map[Kind]uint64, error) {
	out := map[Kind]uint64{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for kind, name := range Counters {
			if !strings.HasPrefix(line, name+" ") {
				continue
			}
			raw := strings.TrimSpace(line[len(name)+1:])
			// Guard against a longer name that shares this prefix
			// (e.g. llamacpp:prompt_tokens_seconds_total).
			if strings.ContainsAny(raw, "{}") {
				continue
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("tokenledger: parse %s: %w", name, err)
			}
			out[kind] = uint64(math.Round(v))
		}
	}
	return out, nil
}
