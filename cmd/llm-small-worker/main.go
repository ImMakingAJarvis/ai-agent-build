// llm-small-worker is the llm.small capability worker (plan task EVB-23):
// the GPU-spare and CPU placements behind cap.llm.small. It consumes
// cap.llm.small and runs small structured LLM sub-tasks (summarise,
// classify, extract — or any custom verb) with a 7B instruct model
// (Qwen2.5-7B-Instruct Q4_K_M — the job's candidate class, chosen and
// measured in docs/benchmarks/llm-small-model-benchmark.md), and replies
// with a Result whose Data carries the structured result and whose
// Artifacts point at a result file — references, not bytes (invariant 7).
//
// BOUNDARY (plan §4, Phase 3): the iGPU helper (Qwen2.5-3B on :11435)
// stays the CONVERSATION fallback during the brain's swap windows —
// llm.small is for BATCH sub-tasks only. The llm.small entry in
// config/capabilities.yaml carries the same note for the model-facing
// catalog.
//
// The model is the worker's own: it spawns llama.cpp's llama-server as a
// coprocess (internal/vlmserver), the same pattern as the image.describe
// worker — no separate container to manage. The coprocess comes up on the
// first request (Load / first Chat) and goes down after the idle window
// (Unload), handing the GPU back.
//
// One binary, two placements (LMS_MODE), same model file in both:
//
//	gpu  location gpu — consumer priority 10; the 7B fully on the 4090's
//	     SPARE VRAM (measured ~5.4 GB, bench doc). A resourced lease
//	     (plan §3.6) gates every consume session on the 5.5 GB catalog
//	     footprint — the plan's co-scheduling scenario (the brain out
//	     for a 12 GB image job leaves ~10 GB free in the ledger). While
//	     the brain's conversation has eaten the spare (its standing
//	     lease), the worker holds no lease, does not consume, and the
//	     broker delivers the request to the cpu placement instead
//	     (consumer priority 1). A denial is a routing decision, never an
//	     OOM: the coprocess is only started after the lease granted.
//	cpu  location cpu — consumer priority 1; the same 7B fully in host
//	     RAM (--n-gpu-layers 0; measured residual CUDA-context VRAM ~630
//	     MiB — the binary always inits the CUDA backend, whatever the
//	     layer count). Slower (~2 tok/s at 16 threads, bench) — the
//	     fallback, not the fast path.
//
// Request input (envelope v2):
//
//	Input["task"]         string  required — summarise | classify | extract
//	         | any custom verb (custom verbs get the plain-text contract)
//	Input["instructions"] string  optional — task-specific instructions
//	         (e.g. "summarise in one sentence", "extract: date, amount")
//	InputRefs              []string  one or more text files (or directories
//	         scanned for text files) in the shared artifacts root
//
// Output (in the request's OutDir):
//
//	result.json  {capability, task, model, worker, created, files, result,
//	             input_truncated, seconds} — result is the reply text
//	             (summarise / custom) or the parsed JSON object
//	             (classify / extract)
//
// A request without a task or with refs that hold no text file is a bad
// message: the handler errors, the retry budget burns, and it
// dead-letters with the reason (invariant 9).
//
// Configuration (env):
//
//	RABBITMQ_URL            the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	LMS_MODE                gpu | cpu. Default gpu.
//	LMS_MODEL               the instruct GGUF (the first part of a split
//	                          file is fine — llama-server loads the siblings).
//	                          Default /opt/jarvis-llm-small/models/qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf.
//	LMS_LLAMA_SERVER        llama-server binary. Default /opt/jarvis-engine/bin/llama-server
//	LMS_PORT                the coprocess port (in-container). Default 11520.
//	LMS_CTX                 --ctx-size. Default 8192.
//	LMS_THREADS             --threads (0 = llama.cpp's default).
//	RESOURCED_URL           the resourced lease broker (plan §3.6); the
//	                          worker gates its consuming on a lease from it.
//	                          Default: off (un-gated, the pre-lease behavior).
//	CAPABILITIES_FILE       the capability catalog the lease's memory spec
//	                          comes from. Needed when RESOURCED_URL is set.
//	LMS_MAX_TOKENS          completion budget. Default 1024.
//	LMS_TEMPERATURE         sampling temperature. Default 0.2.
//	LMS_OUT_DIR             artifact root for requests without an OutDir.
//	                          Default ~/.local/share/jarvis/artifacts.
//	LMS_METRICS_ADDR        serve Prometheus /metrics here. Default: off.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/capworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/vlmserver"
)

const (
	capability = "llm.small"
	workerName = "llm.small@%s/llm-small-worker" // per placement

	modelDir = "/opt/jarvis-llm-small/models"
	modelID  = "qwen2.5-7b-instruct"

	// defaultModelFile is the first part of the split Q4_K_M file:
	// llama-server loads the -00002 sibling itself (verified in the
	// bench). The 7B is the same file in both placements — only the
	// layer count differs.
	defaultModelFile = "qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf"

	// inputCapBytes bounds the combined input text (~27K tokens at 7B
	// scale): far more than a sub-task needs, and it keeps the prompt
	// inside the coprocess's 8K context with room for the reply.
	inputCapBytes = 100 * 1024
)

// textExts: the file kinds a sub-task reasonably reads. Binary formats
// (images, audio) have their own capabilities.
var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".json": true,
	".csv": true, ".tsv": true, ".log": true, ".yaml": true,
	".yml": true, ".xml": true, ".html": true, ".text": true,
}

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

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
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
		log.Printf("llm-small-worker: broker not ready: %v", err)
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

// expandTextRefs turns the request's refs into a sorted, de-duplicated
// list of text files. A directory ref is scanned for text extensions;
// an explicit file ref must itself be a text file.
func expandTextRefs(req queue.Message) ([]string, error) {
	if len(req.InputRefs) == 0 {
		return nil, errors.New("no input files: give --input-ref <file-or-folder>")
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range req.InputRefs {
		st, err := os.Stat(r)
		if err != nil {
			return nil, fmt.Errorf("input ref %q: %w", r, err)
		}
		if st.IsDir() {
			entries, err := os.ReadDir(r)
			if err != nil {
				return nil, fmt.Errorf("read dir %q: %w", r, err)
			}
			for _, e := range entries {
				if e.IsDir() || !textExts[strings.ToLower(filepath.Ext(e.Name()))] {
					continue
				}
				p := filepath.Join(r, e.Name())
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
			continue
		}
		if !textExts[strings.ToLower(filepath.Ext(r))] {
			return nil, fmt.Errorf("input ref %q is not a text file", r)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("input refs contain no text files")
	}
	sort.Strings(out)
	return out, nil
}

// loadInputs reads the files into one prompt block, capping the combined
// size at inputCapBytes (a sub-task that needs more should chunk itself).
// It reports whether anything was cut.
func loadInputs(paths []string) (body string, truncated bool, err error) {
	var sb strings.Builder
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", p, err)
		}
		header := "===== " + filepath.Base(p) + " =====\n"
		if sb.Len()+len(header) > inputCapBytes {
			truncated = true
			break
		}
		sb.WriteString(header)
		// Reserve 64 bytes for the truncation marker.
		if allow := inputCapBytes - sb.Len() - 64; len(b) > allow {
			b = b[:allow]
			truncated = true
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	if truncated {
		sb.WriteString("[input truncated: combined size exceeds the 100 KB cap]\n")
	}
	return sb.String(), truncated, nil
}

// taskPrompt builds the user turn for one task. Structured tasks
// (classify, extract) get a JSON-only contract; summarise and custom
// verbs get plain text.
func taskPrompt(task, instructions, body string) string {
	var b strings.Builder
	switch strings.ToLower(task) {
	case "summarise":
		b.WriteString("Summarise the input text below. Be faithful and concise — a short paragraph or a few bullet points. Respond with the summary only, no preamble.")
	case "classify":
		b.WriteString(`Classify the input text below. Respond with JSON only, no markdown: {"label": "<short label>", "reason": "<one sentence>"}`)
	case "extract":
		b.WriteString("Extract the requested information from the input text below. Respond with a JSON object only, no markdown — one key per requested item; use null for anything not present.")
	default:
		fmt.Fprintf(&b, "Perform this task on the input text below: %s", task)
	}
	if strings.TrimSpace(instructions) != "" {
		b.WriteString(" Instructions: " + strings.TrimSpace(instructions))
	}
	b.WriteString("\n\nInput text:\n")
	b.WriteString(body)
	return b.String()
}

// structured reports whether the task's reply must be a JSON object.
func structured(task string) bool {
	switch strings.ToLower(task) {
	case "classify", "extract":
		return true
	}
	return false
}

// handler does one llm.small request.
type handler struct {
	srv       *vlmserver.Server
	worker    string
	model     string
	maxTokens int
	temp      float64
}

// taskResult is the result.json artifact.
type taskResult struct {
	Capability string    `json:"capability"`
	Task       string    `json:"task"`
	Model      string    `json:"model"`
	Worker     string    `json:"worker"`
	Created    time.Time `json:"created"`
	Files      []string  `json:"files"`
	Result     any       `json:"result"`
	Truncated  bool      `json:"input_truncated,omitempty"`
	Seconds    float64   `json:"seconds"`
}

func (h handler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	task := strings.TrimSpace(getString(req.Input, "task"))
	if task == "" {
		return queue.Result{}, errors.New(`llm.small: Input["task"] is required (summarise | classify | extract | a custom verb)`)
	}
	instructions := getString(req.Input, "instructions")
	paths, err := expandTextRefs(req)
	if err != nil {
		return queue.Result{}, fmt.Errorf("llm.small: %w", err)
	}
	body, truncated, err := loadInputs(paths)
	if err != nil {
		return queue.Result{}, err
	}
	log.Printf("llm-small-worker: %s: task %q on %d file(s)", req.CorrelationID, task, len(paths))

	// The SDK assumes the worker starts loaded, but this worker's
	// resources are its coprocess — bring it up here (idempotent; it is
	// also the Load hook).
	if err := h.srv.Start(ctx); err != nil {
		return queue.Result{}, err
	}

	start := time.Now()
	reply, err := h.srv.Chat(ctx, []vlmserver.ChatMessage{
		{Role: "user", Content: taskPrompt(task, instructions, body)},
	}, h.maxTokens, h.temp)
	if err != nil {
		return queue.Result{}, err
	}

	// Plain-text tasks keep the reply verbatim; structured tasks parse
	// it, falling back to the raw text (flagged) when the model did not
	// honour the JSON contract.
	var result any = strings.TrimSpace(reply)
	if structured(task) {
		if m, perr := vlmserver.ExtractJSONObject(reply); perr != nil {
			log.Printf("llm-small-worker: %s: reply is not JSON (%v) — returning the raw text", req.CorrelationID, perr)
		} else {
			result = m
		}
	}

	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}
	res := taskResult{
		Capability: capability, Task: task, Model: h.model, Worker: h.worker,
		Created: time.Now(), Files: paths, Result: result, Truncated: truncated,
		Seconds: time.Since(start).Seconds(),
	}
	resPath := filepath.Join(req.OutDir, "result.json")
	b, err := json.Marshal(res)
	if err != nil {
		return queue.Result{}, err
	}
	if err := os.WriteFile(resPath, b, 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", resPath, err)
	}

	data := map[string]any{
		"task":        task,
		"result":      result,
		"files":       paths,
		"result_file": resPath,
	}
	if structured(task) {
		if _, isObj := result.(map[string]any); !isObj {
			data["parse_error"] = true
		}
	}
	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: []string{resPath},
		Summary:   fmt.Sprintf("%s %d file(s) (%s)", task, len(paths), h.worker),
		Data:      data,
	}, nil
}

func getString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func main() {
	mode := strings.ToLower(envOr("LMS_MODE", "gpu"))
	if mode != "gpu" && mode != "cpu" {
		log.Fatalf("llm-small-worker: LMS_MODE must be gpu or cpu, got %q", mode)
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("llm-small-worker: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("llm-small-worker: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "llm-small-worker-"+mode)
	if err != nil {
		log.Printf("llm-small-worker: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	srv := &vlmserver.Server{
		Binary:  envOr("LMS_LLAMA_SERVER", "/opt/jarvis-engine/bin/llama-server"),
		Model:   envOr("LMS_MODEL", filepath.Join(modelDir, defaultModelFile)),
		Port:    envInt("LMS_PORT", 11520),
		NGL:     nglForMode(mode),
		CtxSize: envInt("LMS_CTX", 8192),
		Threads: envInt("LMS_THREADS", 0),
		Log:     log.Default(),
	}

	// Lease gating (plan §3.6): resourced decides whether this placement
	// may consume — the gpu placement on free 4090 VRAM, the cpu
	// placement on the shared RAM pool (the catalog footprints). No
	// broker (RESOURCED_URL unset) or a catalog problem = un-gated, the
	// pre-lease behavior.
	leaseBroker, leaseSpec, lerr := capworker.LeaseFor(
		os.Getenv("RESOURCED_URL"), os.Getenv("CAPABILITIES_FILE"),
		capability, mode, fmt.Sprintf(workerName, mode))
	if lerr != nil {
		log.Printf("llm-small-worker: lease: %v (running un-gated)", lerr)
	}

	cfg := capworker.Config{
		Cap:         capability,
		Name:        fmt.Sprintf(workerName, mode),
		Model:       modelID,
		Location:    mode, // gpu → consumer priority 10, cpu → 1
		Conn:        conn,
		Events:      events,
		LeaseBroker: leaseBroker,
		LeaseSpec:   leaseSpec,
		MetricsAddr: os.Getenv("LMS_METRICS_ADDR"),
		IdleS:       300 * time.Second, // unload the model 5 min after the last request
		Load:        srv.Start,
		Unload:      func(ctx context.Context) error { return srv.Stop() },
		Handler: handler{
			srv: srv, worker: fmt.Sprintf(workerName, mode), model: modelID,
			maxTokens: envInt("LMS_MAX_TOKENS", 1024), temp: envFloat("LMS_TEMPERATURE", 0.2),
		},
		OutDirBase: envOr("LMS_OUT_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")),
		Log:        log.Default(),
	}

	if err := capworker.New(cfg).Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("llm-small-worker: run: %v", err)
	}
	log.Printf("llm-small-worker: stopped")
}

// nglForMode: GPU loads every layer onto the 4090; CPU keeps them all in
// host RAM (the cpu placement's residual VRAM is the CUDA-context floor
// measured in the bench, ~630 MiB; the binary always initializes the CUDA
// backend, whatever --n-gpu-layers says).
func nglForMode(mode string) int {
	if mode == "cpu" {
		return 0
	}
	return 99
}
