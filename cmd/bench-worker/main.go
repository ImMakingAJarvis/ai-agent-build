// bench-worker is the bench.run capability worker (plan task EVB-24): the
// last Phase 3 worker — benchmarks speak the cap pattern too. It consumes
// cap.bench.run and runs the existing benchmark tooling as a thin
// adapter, per request:
//
//	bench "llama-bench" (default) — the host's pinned llama.cpp
//	    llama-bench (the tool scripts/igpu-bench.sh drives) over a model
//	    GGUF, on the iGPU (Vulkan0 in-container — the container carries
//	    only the mesa ICD, so the 4090 is not visible from here; a 4090
//	    bench is the Phase 4 resourced story) or CPU-only.
//	bench "live" — scripts/benchmark_q6.py against a serving engine
//	    (default: the brain container at http://jarvis-engine:11434,
//	    reachable by name on the compose network; the iGPU helper is
//	    http://jarvis-engine-igpu:11435). It drives the engine while it
//	    serves, so a brain-targeted run is slow for the brain's own
//	    conversation while it lasts — point base_url at the iGPU helper
//	    for a cheap full-path test.
//
// Results go to the requester's reply queue (envelope ReplyTo,
// flow.<requester>) — the scores in the Result's Data, a one-line
// summary, and the raw tool output kept as a report artifact plus a
// parsed index under the request's OutDir for compatibility with the
// old ad-hoc file output. No ad-hoc result files are written anywhere
// else (the old ~/.local/share/jarvis/benchmarks.csv and hand-written
// docs/benchmarks/*.md paths are gone for new runs).
//
// Request input (envelope v2), Input map:
//
//	["bench"]         "llama-bench" (default) | "live"
//	["model"]         llama-bench: a GGUF path (visible in the container)
//	                  or an Ollama library name like "llama3.2:1b"
//	                  (resolved through the mounted Ollama store, no copy)
//	["device"]        llama-bench: "igpu" (default) | "cpu"
//	["prompt_tokens"] int, default 512
//	["gen_tokens"]    int, default 512
//	["runs"]          int, default 3
//	["base_url"]      live: engine base URL, default http://localhost:11434
//	["fill_tokens"]   live: long-context fill, default 0 (off)
//
// Output (in the request's OutDir):
//
//	report.md   the raw tool output with a parameter header
//	index.json  {capability, bench, worker, model, device, params, scores, ...}
//
// A request that names a bench the worker does not know, a model it
// cannot resolve, or a tool that exits non-zero is a handler error: the
// retry budget burns and it dead-letters with the reason (invariant 9).
//
// Configuration (env):
//
//	RABBITMQ_URL            the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	BW_LLAMA_BENCH          the llama-bench binary. Default /opt/jarvis-igpu/bin/llama-bench.
//	BW_OLLAMA_MODELS        Ollama model store root (for name resolution).
//	                          Default /usr/share/ollama/.ollama/models.
//	BW_LIVE_SCRIPT          benchmark_q6.py. Default /usr/local/lib/jarvis-bench/benchmark_q6.py.
//	BW_PYTHON               interpreter for the live bench. Default python3.
//	RESOURCED_URL           the resourced lease broker (plan §3.6); the
//	                          worker gates its consuming on a lease from it.
//	                          Default: off (un-gated, the pre-lease behavior).
//	CAPABILITIES_FILE       the capability catalog the lease's memory spec
//	                          comes from. Needed when RESOURCED_URL is set.
//	BW_OUT_DIR              artifact root for requests without an OutDir.
//	                          Default ~/.local/share/jarvis/artifacts.
//	BW_METRICS_ADDR         serve Prometheus /metrics here. Default: off.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/capworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const (
	capability = "bench.run"
	workerName = "bench.run@cpu/bench-worker"

	// modelID is the tool id the capability points at (like "echo" for
	// the test worker): the scores are produced by the benchmark
	// tooling, the model under test is per-request input.
	modelID    = "llama-bench"
	benchBin   = "/opt/jarvis-igpu/bin/llama-bench"
	ollamaRoot = "/usr/share/ollama/.ollama/models"
	liveScript = "/usr/local/lib/jarvis-bench/benchmark_q6.py"

	benchLlamaBench = "llama-bench"
	benchLive       = "live"

	deviceIGPU = "igpu"
	deviceCPU  = "cpu"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// connectWithRetry dials the broker, backing off until it is reachable
// (the compose stack may bring rabbitmq up a little after the worker).
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("bench-worker: broker not ready: %v", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// benchParams is the request's input after defaults and validation.
type benchParams struct {
	bench  string
	model  string
	device string
	prompt int
	gen    int
	runs   int
	base   string // live: engine base URL
	fill   int    // live: long-context fill tokens
}

func inputStr(in map[string]any, key, def string) string {
	if v, ok := in[key].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func inputInt(in map[string]any, key string, def int) (int, error) {
	v, ok := in[key]
	if !ok {
		return def, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case float64: // JSON numbers
		if n != float64(int(n)) {
			return 0, fmt.Errorf("input %q is not an integer", key)
		}
		return int(n), nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("input %q %q is not an integer", key, n)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("input %q is not an integer", key)
	}
}

// parseParams fills the request's bench parameters (defaults + validation).
func parseParams(in map[string]any) (benchParams, error) {
	p := benchParams{
		bench:  inputStr(in, "bench", benchLlamaBench),
		model:  inputStr(in, "model", ""),
		device: inputStr(in, "device", deviceIGPU),
		prompt: 512,
		gen:    512,
		runs:   3,
		base:   "http://jarvis-engine:11434", // the brain, by container name
		fill:   0,
	}
	var err error
	if p.prompt, err = inputInt(in, "prompt_tokens", p.prompt); err != nil {
		return p, err
	}
	if p.gen, err = inputInt(in, "gen_tokens", p.gen); err != nil {
		return p, err
	}
	if p.runs, err = inputInt(in, "runs", p.runs); err != nil {
		return p, err
	}
	if p.fill, err = inputInt(in, "fill_tokens", p.fill); err != nil {
		return p, err
	}
	p.base = inputStr(in, "base_url", p.base)
	switch p.bench {
	case benchLlamaBench:
		if p.model == "" {
			return p, errors.New(`bench.run: llama-bench needs Input["model"] (a GGUF path or an Ollama name like "llama3.2:1b")`)
		}
		switch p.device {
		case deviceIGPU, deviceCPU:
		default:
			return p, fmt.Errorf("bench.run: device %q is not igpu or cpu (the 4090 is not visible from this worker)", p.device)
		}
		if p.prompt <= 0 || p.gen <= 0 || p.runs <= 0 {
			return p, errors.New("bench.run: prompt_tokens, gen_tokens and runs must be > 0")
		}
	case benchLive:
		if p.base == "" {
			return p, errors.New(`bench.run: live bench needs Input["base_url"]`)
		}
		if _, err := url.ParseRequestURI(p.base); err != nil {
			return p, fmt.Errorf("bench.run: base_url %q: %w", p.base, err)
		}
		if p.runs <= 0 || p.fill < 0 {
			return p, errors.New("bench.run: runs must be > 0 and fill_tokens >= 0")
		}
	default:
		return p, fmt.Errorf("bench.run: bench %q is not %q or %q", p.bench, benchLlamaBench, benchLive)
	}
	return p, nil
}

// resolveModel turns the request's model into a GGUF path the container
// can read: a file path passes through, an Ollama library name
// ("llama3.2:1b") is resolved through the mounted store — the manifest's
// largest layer blob IS a valid GGUF, so no copy is needed (the old
// igpu-bench.sh copied the blob; this does not).
func resolveModel(ollamaModels, name string) (string, error) {
	if st, err := os.Stat(name); err == nil && !st.IsDir() {
		return name, nil
	}
	fam, tag, ok := strings.Cut(name, ":")
	if !ok {
		tag = "latest" // bare Ollama names mean the latest tag
	}
	if fam == "" || tag == "" {
		return "", fmt.Errorf("model %q: not a file in this container and not an Ollama name (family:tag)", name)
	}
	manifest := filepath.Join(ollamaModels, "manifests", "registry.ollama.ai", "library", fam, tag)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return "", fmt.Errorf("model %q: no Ollama manifest at %s", name, manifest)
	}
	var m struct {
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("model %q: parse manifest: %w", name, err)
	}
	if len(m.Layers) == 0 {
		return "", fmt.Errorf("model %q: manifest has no layers", name)
	}
	biggest := m.Layers[0]
	for _, l := range m.Layers[1:] {
		if l.Size > biggest.Size {
			biggest = l
		}
	}
	blob := filepath.Join(ollamaModels, "blobs", strings.Replace(biggest.Digest, "sha256:", "sha256-", 1))
	if _, err := os.Stat(blob); err != nil {
		return "", fmt.Errorf("model %q: blob %s not in Ollama store", name, filepath.Base(blob))
	}
	return blob, nil
}

// llamaBenchRow is one result line of llama-bench's table.
type llamaBenchRow struct {
	Model   string
	Size    string
	Backend string
	Dev     string
	Test    string  // "pp512" / "tg512"
	TPS     float64 // tokens/s (median of the runs)
	Spread  float64 // the ± value
}

// tsValue splits a "108.70 ± 0.00" cell into value and spread.
func tsValue(cell string) (float64, float64, error) {
	parts := strings.Split(strings.TrimSpace(cell), "±")
	v, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, err
	}
	if len(parts) > 1 {
		s, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return 0, 0, err
		}
		return v, s, nil
	}
	return v, 0, nil
}

// parseLlamaBench parses llama-bench's result table out of its stdout.
// The column set varies (a "dev" column appears only when -dev is given,
// and some builds drop "params"), so it is located from the header row
// rather than fixed offsets. Non-table lines (device detection, the
// "build:" footer) are ignored.
func parseLlamaBench(stdout string) ([]llamaBenchRow, error) {
	var rows []llamaBenchRow
	var col map[string]int
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := splitRow(line)
		if col == nil {
			// The header row: remember where each column is.
			col = make(map[string]int)
			for i, c := range cells {
				col[strings.ToLower(strings.TrimSpace(c))] = i
			}
			if _, ok := col["test"]; !ok {
				col = nil
				continue
			}
			continue
		}
		if isSeparator(cells) {
			continue
		}
		get := func(name string) string {
			i, ok := col[name]
			if !ok || i >= len(cells) {
				return ""
			}
			return strings.TrimSpace(cells[i])
		}
		test := get("test")
		if !regexp.MustCompile(`^(pp|tg)\d+$`).MatchString(test) {
			continue
		}
		tps, spread, err := tsValue(get("t/s"))
		if err != nil {
			return nil, fmt.Errorf("parse llama-bench row %q: %w", line, err)
		}
		rows = append(rows, llamaBenchRow{
			Model: get("model"), Size: get("size"), Backend: get("backend"),
			Dev: get("dev"), Test: test, TPS: tps, Spread: spread,
		})
	}
	if len(rows) == 0 {
		return nil, errors.New("no result rows in llama-bench output")
	}
	return rows, nil
}

// splitRow splits a markdown table row into trimmed cells (the leading
// and trailing pipes drop).
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var out []string
	for _, c := range strings.Split(line, "|") {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if c == "" || strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

// liveRun is one timed run of the live bench's prompt table.
type liveRun struct {
	PromptN  int     `json:"prompt_n"`
	PromptTS float64 `json:"prompt_tps"`
	GenN     int     `json:"gen_n"`
	GenTS    float64 `json:"gen_tps"`
	WallS    float64 `json:"wall_s"`
}

// liveTask is one of the live bench's small correctness tasks.
type liveTask struct {
	Name   string  `json:"name"`
	Pass   bool    `json:"pass"`
	GenTok int     `json:"gen_tok"`
	WallS  float64 `json:"wall_s"`
}

// liveScores is the parsed output of benchmark_q6.py.
type liveScores struct {
	Runs     []liveRun  `json:"runs"`
	Tasks    []liveTask `json:"tasks"`
	ToolCall bool       `json:"tool_call"`
	LongCtx  *struct {
		PromptN  int     `json:"prompt_n"`
		PromptTS float64 `json:"prompt_tps"`
		DecodeTS float64 `json:"decode_tps"`
		WallS    float64 `json:"wall_s"`
		Needle   bool    `json:"needle_pass"`
	} `json:"long_context,omitempty"`
}

var (
	liveTaskRe = regexp.MustCompile(`^task (PASS|FAIL) \((\d+) tok, ([\d.]+)s\): (.+)$`)
	liveToolRe = regexp.MustCompile(`^tool_call (PASS|FAIL):`)
	liveLongRe = regexp.MustCompile(`^long-context: prompt (\d+) tok at ([\d.]+) tok/s, decode ([\d.]+) tok/s, wall ([\d.]+)s, needle (PASS|FAIL)$`)
)

// parseLiveBench parses benchmark_q6.py's stdout: the per-run prompt
// table, the correctness tasks, the tool-call check and the optional
// long-context run.
func parseLiveBench(stdout string) (*liveScores, error) {
	s := &liveScores{}
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(line, "|") {
			cells := splitRow(line)
			if len(cells) < 6 {
				continue
			}
			// Header and separator rows do not parse as the shape below.
			promptN, err1 := strconv.Atoi(cells[1])
			promptTS, err2 := strconv.ParseFloat(cells[2], 64)
			genN, err3 := strconv.Atoi(cells[3])
			genTS, err4 := strconv.ParseFloat(cells[4], 64)
			wall, err5 := strconv.ParseFloat(cells[5], 64)
			if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
				continue
			}
			s.Runs = append(s.Runs, liveRun{promptN, promptTS, genN, genTS, wall})
			continue
		}
		if m := liveTaskRe.FindStringSubmatch(line); m != nil {
			tok, _ := strconv.Atoi(m[2])
			wall, _ := strconv.ParseFloat(m[3], 64)
			s.Tasks = append(s.Tasks, liveTask{
				Name: strings.TrimSpace(m[4]), Pass: m[1] == "PASS", GenTok: tok, WallS: wall,
			})
			continue
		}
		if m := liveToolRe.FindStringSubmatch(line); m != nil {
			s.ToolCall = m[1] == "PASS"
			continue
		}
		if m := liveLongRe.FindStringSubmatch(line); m != nil {
			pn, _ := strconv.Atoi(m[1])
			pts, _ := strconv.ParseFloat(m[2], 64)
			dts, _ := strconv.ParseFloat(m[3], 64)
			wall, _ := strconv.ParseFloat(m[4], 64)
			s.LongCtx = &struct {
				PromptN  int     `json:"prompt_n"`
				PromptTS float64 `json:"prompt_tps"`
				DecodeTS float64 `json:"decode_tps"`
				WallS    float64 `json:"wall_s"`
				Needle   bool    `json:"needle_pass"`
			}{pn, pts, dts, wall, m[5] == "PASS"}
		}
	}
	if len(s.Runs) == 0 && len(s.Tasks) == 0 {
		return nil, errors.New("no timed runs or tasks in the live bench output")
	}
	return s, nil
}

// runCmd runs one benchmark subprocess and returns its stdout and
// stderr. The stderr (device detection, progress) is mirrored to the
// worker's stderr; both are kept for the report artifact.
func runCmd(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = newTee(os.Stderr, &errb)
	log.Printf("bench-worker: running %s %s", name, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return out.String(), errb.String(), fmt.Errorf("%s %s: %w (stderr: %s)", name, strings.Join(args, " "), err, tail(errb.String(), 500))
	}
	return out.String(), errb.String(), nil
}

// tail returns the last n chars of s (for error messages).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// teeWriter tees a subprocess's stderr to the worker log and a buffer.
type teeWriter struct {
	log *os.File
	buf *strings.Builder
}

func newTee(f *os.File, b *strings.Builder) *teeWriter {
	return &teeWriter{log: f, buf: b}
}

func (m *teeWriter) Write(p []byte) (int, error) {
	if _, err := m.buf.Write(p); err != nil {
		return 0, err
	}
	return m.log.Write(p)
}

// handler does one bench.run request.
type handler struct {
	worker     string
	llamaBench string
	ollamaRoot string
	python     string
	liveScript string
}

// Handle runs the requested bench and returns the Result (the SDK
// stamps the envelope fields and publishes it to the request's ReplyTo).
func (h handler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	p, err := parseParams(req.Input)
	if err != nil {
		return queue.Result{}, err
	}
	log.Printf("bench-worker: %s: %s bench (model=%s device=%s runs=%d)", req.CorrelationID, p.bench, p.model, p.device, p.runs)

	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}

	var (
		stdout, stderr string
		summary        string
		data           = map[string]any{
			"bench":  p.bench,
			"worker": h.worker,
		}
	)
	switch p.bench {
	case benchLlamaBench:
		model, rerr := resolveModel(h.ollamaRoot, p.model)
		if rerr != nil {
			return queue.Result{}, rerr
		}
		args := []string{"-m", model}
		if p.device == deviceIGPU {
			args = append(args, "-dev", "Vulkan0", "-ngl", "99")
		}
		args = append(args, "-p", strconv.Itoa(p.prompt), "-n", strconv.Itoa(p.gen), "-r", strconv.Itoa(p.runs))
		stdout, stderr, err = runCmd(ctx, h.llamaBench, args...)
		if err != nil {
			return queue.Result{}, err
		}
		rows, perr := parseLlamaBench(stdout)
		if perr != nil {
			return queue.Result{}, fmt.Errorf("bench.run: %w", perr)
		}
		s := map[string]any{}
		for _, r := range rows {
			s[r.Test] = map[string]any{"tps": r.TPS, "spread": r.Spread}
		}
		data["model"] = p.model
		data["model_file"] = model
		data["model_name"] = rows[0].Model
		data["model_size"] = rows[0].Size
		data["device"] = p.device
		data["prompt_tokens"] = p.prompt
		data["gen_tokens"] = p.gen
		data["runs"] = p.runs
		data["scores"] = s
		parts := make([]string, 0, len(rows))
		for _, r := range rows {
			parts = append(parts, fmt.Sprintf("%s %.1f t/s", r.Test, r.TPS))
		}
		summary = fmt.Sprintf("llama-bench %s [%s]: %s (%d runs)", rows[0].Model, p.device, strings.Join(parts, ", "), p.runs)
	case benchLive:
		args := []string{h.liveScript, p.base, strconv.Itoa(p.runs), strconv.Itoa(p.fill)}
		stdout, stderr, err = runCmd(ctx, h.python, args...)
		if err != nil {
			return queue.Result{}, err
		}
		ls, perr := parseLiveBench(stdout)
		if perr != nil {
			return queue.Result{}, fmt.Errorf("bench.run: %w", perr)
		}
		data["base_url"] = p.base
		data["runs"] = p.runs
		data["fill_tokens"] = p.fill
		data["scores"] = ls
		fails := 0
		for _, t := range ls.Tasks {
			if !t.Pass {
				fails++
			}
		}
		summary = fmt.Sprintf("live bench %s: %d run(s), %d/%d tasks pass%s", p.base, len(ls.Runs), len(ls.Tasks)-fails, len(ls.Tasks), toolCallNote(ls))
	}

	// The report artifact: the raw tool output (the old ad-hoc file
	// output, kept under OutDir) with a parameter header.
	var rep strings.Builder
	fmt.Fprintf(&rep, "# bench.run report\n\n")
	fmt.Fprintf(&rep, "- correlation: %s\n", req.CorrelationID)
	fmt.Fprintf(&rep, "- bench: %s\n", p.bench)
	if p.bench == benchLlamaBench {
		fmt.Fprintf(&rep, "- model: %s (%s)\n", p.model, p.device)
		fmt.Fprintf(&rep, "- workload: pp%d tg%d x%d\n", p.prompt, p.gen, p.runs)
	} else {
		fmt.Fprintf(&rep, "- base_url: %s\n", p.base)
		fmt.Fprintf(&rep, "- workload: %d run(s), fill %d\n", p.runs, p.fill)
	}
	fmt.Fprintf(&rep, "- worker: %s\n", h.worker)
	fmt.Fprintf(&rep, "- finished: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	if stderr != "" {
		fmt.Fprintf(&rep, "## stderr\n\n```\n%s```\n\n", strings.TrimRight(stderr, "\n"))
	}
	fmt.Fprintf(&rep, "## output\n\n```\n%s```\n", strings.TrimRight(stdout, "\n"))

	reportPath := filepath.Join(req.OutDir, "report.md")
	if err := os.WriteFile(reportPath, []byte(rep.String()), 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", reportPath, err)
	}

	// The index artifact: the parsed scores + parameters as JSON.
	idx := map[string]any{
		"capability":  capability,
		"correlation": req.CorrelationID,
		"worker":      h.worker,
		"created":     time.Now().UTC(),
	}
	for k, v := range data {
		idx[k] = v
	}
	idx["report_file"] = reportPath
	idxBytes, err := json.Marshal(idx)
	if err != nil {
		return queue.Result{}, err
	}
	indexPath := filepath.Join(req.OutDir, "index.json")
	if err := os.WriteFile(indexPath, idxBytes, 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", indexPath, err)
	}

	data["report_file"] = reportPath
	data["index_file"] = indexPath
	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: []string{reportPath, indexPath},
		Summary:   summary,
		Data:      data,
	}, nil
}

func toolCallNote(ls *liveScores) string {
	if !ls.ToolCall {
		return ", tool_call FAIL"
	}
	return ""
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("bench-worker: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("bench-worker: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "bench-worker")
	if err != nil {
		log.Printf("bench-worker: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	// Lease gating (plan §3.6): the placement's lease draws on the shared
	// RAM pool (the bench loads weights through system RAM — the iGPU is
	// UMA, and the cpu path is RAM-resident). No broker (RESOURCED_URL
	// unset) or a catalog problem = un-gated, the pre-lease behavior.
	leaseBroker, leaseSpec, lerr := capworker.LeaseFor(
		os.Getenv("RESOURCED_URL"), os.Getenv("CAPABILITIES_FILE"),
		capability, "cpu", workerName)
	if lerr != nil {
		log.Printf("bench-worker: lease: %v (running un-gated)", lerr)
	}

	cfg := capworker.Config{
		Cap:         capability,
		Name:        workerName,
		Model:       modelID,
		Location:    "cpu", // consumer priority 1
		Conn:        conn,
		Events:      events,
		LeaseBroker: leaseBroker,
		LeaseSpec:   leaseSpec,
		MetricsAddr: os.Getenv("BW_METRICS_ADDR"),
		Handler: handler{
			worker:     workerName,
			llamaBench: envOr("BW_LLAMA_BENCH", benchBin),
			ollamaRoot: envOr("BW_OLLAMA_MODELS", ollamaRoot),
			python:     envOr("BW_PYTHON", "python3"),
			liveScript: envOr("BW_LIVE_SCRIPT", liveScript),
		},
		OutDirBase: envOr("BW_OUT_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")),
		Log:        log.Default(),
	}

	if err := capworker.New(cfg).Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("bench-worker: run: %v", err)
	}
	log.Printf("bench-worker: stopped")
}
