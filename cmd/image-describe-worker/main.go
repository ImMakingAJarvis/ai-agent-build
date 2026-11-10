// image-describe-worker is the image.describe capability worker (plan
// task EVB-21): the GPU and CPU placements behind cap.image.describe.
// It consumes cap.image.describe, describes each input image with a
// small local VLM (Qwen3-VL-2B on the GPU placement, Qwen3-VL-8B on the
// CPU fallback — picked by the bench in
// docs/benchmarks/image-describe-vlm-benchmark.md), and replies
// with a Result whose Artifacts point at one JSON per image plus an
// index file — references, not bytes (invariant 7).
//
// The model is the worker's own: it spawns llama.cpp's llama-server as
// a coprocess (internal/vlmserver), so there is no separate container to
// manage. The coprocess comes up on the first request (Load / first
// Describe) and goes down after the idle window (Unload), handing the
// GPU back — the same discipline as the brain-swap windows.
//
// One binary, two placements (IDESC_MODE):
//
//	gpu  location gpu — consumer priority 10; Qwen3-VL-2B Q4_K_M + F16
//	     mmproj on the 4090's SPARE VRAM (measured 3146 MiB — see
//	     docs/benchmarks/image-describe-vlm-benchmark.md). The Lease
//	     checks nvidia-smi free VRAM before every consume session: when
//	     the brain is in a conversation and the spare is gone, the
//	     worker refuses the session and the broker delivers the request
//	     to the cpu placement instead (consumer priority 1).
//	cpu  location cpu — consumer priority 1; Qwen3-VL-8B fully in host
//	     RAM (--n-gpu-layers 0 --no-mmproj-offload: measured 580 MiB of
//	     residual CUDA-context VRAM, never blocked on the GPU). Slower
//	     than the GPU placement; on the CPU it beats the 4B where it
//	     matters — the cold start (one slow image vs three, the bench doc
//	     has the numbers) — with slightly better captions too.
//
// Request input (envelope v2):
//
//	InputRefs  []string  one or more paths (files or directories) in the
//	         shared artifacts root; a directory is scanned for images
//	Input["image"] string  optional — one explicit image path (accepts the
//	         same refs; the folder is the primary form)
//
// Output (in the request's OutDir):
//
//	index.json   {capability, count, worker, model, created, images:
//	             [{file, result_file, caption, tags}]}
//	<stem>.json  one per image — {image, caption, prompt, tags, seconds,
//	             model, worker}
//
// A request whose refs point at nothing (or a ref that does not exist
// inside the container) is a bad message: the handler errors, the retry
// budget burns, and it dead-letters with the reason (invariant 9).
//
// Configuration (env):
//
//	RABBITMQ_URL            the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	IDESC_MODE              gpu | cpu. Default gpu.
//	IDESC_MODEL             vision GGUF. Default by mode: Qwen3VL-2B-Instruct-Q4_K_M.gguf (gpu),
//	                          Qwen3VL-8B-Instruct-Q4_K_M.gguf (cpu), under /opt/jarvis-vlm/models/.
//	IDESC_MMPROJ            its projector. Default by mode: the matching mmproj-...-F16.gguf.
//	IDESC_LLAMA_SERVER      llama-server binary. Default /opt/jarvis-engine/bin/llama-server
//	IDESC_PORT              the coprocess port (in-container). Default 11510.
//	IDESC_CTX               --ctx-size. Default 4096.
//	IDESC_THREADS           --threads (0 = llama.cpp default).
//	IDESC_MIN_FREE_MB       GPU lease: minimum free VRAM (MiB). Default 4096.
//	IDESC_SMI               nvidia-smi binary for the lease. Default nvidia-smi.
//	IDESC_OUT_DIR           artifact root for requests without an OutDir.
//	                          Default ~/.local/share/jarvis/artifacts.
//	IDESC_METRICS_ADDR      serve Prometheus /metrics here. Default: off.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	capability = "image.describe"
	workerName = "image.describe@%s/image-describe-worker" // per placement

	modelDir = "/opt/jarvis-vlm/models"
)

// modelForMode is the bench's pick (see docs/benchmarks): the GPU
// placement runs the 2B because it fits in the 4090's spare VRAM even
// with a busy brain; the CPU fallback runs the 8B because it passed every
// truth the 4B did with slightly better captions and the better cold
// start (the worker's requests are usually the first of a cold session —
// the bench doc has the numbers).
func modelForMode(mode string) string {
	if mode == "cpu" {
		return "qwen3-vl-8b"
	}
	return "qwen3-vl-2b"
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".bmp": true,
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

// connectWithRetry dials the broker, backing off until it is reachable
// (the compose stack may bring rabbitmq up a little after the worker).
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("image-describe-worker: broker not ready: %v", err)
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

// freeVRAMMB reads the first GPU's free memory (MiB) from nvidia-smi.
func freeVRAMMB(smi string) (int, error) {
	out, err := exec.Command(smi, "--query-gpu=memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	n, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi free VRAM %q: %w", line, err)
	}
	return n, nil
}

// gpuLease is the GPU placement's Lease: it grants only when the 4090 has
// at least minFree MiB free — i.e. when the brain is idle (or out). When
// it refuses, the SDK backs off and the message stays available for the
// lower-priority CPU placement (consumer priority routing).
func gpuLease(smi string, minFree int) func(context.Context) error {
	return func(ctx context.Context) error {
		free, err := freeVRAMMB(smi)
		if err != nil {
			return err
		}
		if free < minFree {
			return fmt.Errorf("insufficient free VRAM: %d MiB free, %d MiB needed (brain in use?)", free, minFree)
		}
		return nil
	}
}

// expandRefs turns the request's image input into a sorted, de-duplicated
// list of image files. A directory ref is scanned for image extensions;
// an explicit file ref must itself be an image.
func expandRefs(req queue.Message) ([]string, error) {
	var refs []string
	if s, ok := req.Input["image"].(string); ok && strings.TrimSpace(s) != "" {
		refs = append(refs, strings.TrimSpace(s))
	}
	refs = append(refs, req.InputRefs...)
	if len(refs) == 0 {
		return nil, errors.New(`no images: give --input-ref <folder-or-image> (or Input["image"])`)
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
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
				if e.IsDir() || !imageExts[strings.ToLower(filepath.Ext(e.Name()))] {
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
		if !imageExts[strings.ToLower(filepath.Ext(r))] {
			return nil, fmt.Errorf("input ref %q is not an image file", r)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("input refs contain no image files")
	}
	sort.Strings(out)
	return out, nil
}

// handler does one image.describe request.
type handler struct {
	srv    *vlmserver.Server
	worker string
	model  string
}

// imageResult is the per-image artifact JSON.
type imageResult struct {
	Image   string   `json:"image"`
	Caption string   `json:"caption"`
	Prompt  string   `json:"prompt"`
	Tags    []string `json:"tags"`
	Seconds float64  `json:"seconds"`
	Model   string   `json:"model"`
	Worker  string   `json:"worker"`
}

// indexImage is one entry of index.json.
type indexImage struct {
	File       string   `json:"file"`
	ResultFile string   `json:"result_file"`
	Caption    string   `json:"caption"`
	Tags       []string `json:"tags"`
}

// indexFile is the request's index artifact.
type indexFile struct {
	Capability string       `json:"capability"`
	Count      int          `json:"count"`
	Worker     string       `json:"worker"`
	Model      string       `json:"model"`
	Created    time.Time    `json:"created"`
	Images     []indexImage `json:"images"`
}

func (h handler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	paths, err := expandRefs(req)
	if err != nil {
		return queue.Result{}, fmt.Errorf("image.describe: %w", err)
	}
	log.Printf("image-describe-worker: %s: describing %d image(s)", req.CorrelationID, len(paths))

	// The SDK assumes the worker starts loaded, but this worker's
	// resources are its coprocess — bring it up here (idempotent; it is
	// also the Load hook).
	if err := h.srv.Start(ctx); err != nil {
		return queue.Result{}, err
	}

	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}

	idx := indexFile{Capability: capability, Worker: h.worker, Model: h.model, Created: time.Now()}
	artifacts := make([]string, 0, len(paths)+1)
	for _, p := range paths {
		start := time.Now()
		d, err := h.srv.Describe(ctx, p)
		if err != nil {
			return queue.Result{}, err
		}
		stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		resPath := filepath.Join(req.OutDir, stem+".json")
		res := imageResult{Image: p, Caption: d.Caption, Prompt: d.Prompt, Tags: d.Tags,
			Seconds: time.Since(start).Seconds(), Model: h.model, Worker: h.worker}
		if res.Tags == nil {
			res.Tags = []string{}
		}
		b, err := json.Marshal(res)
		if err != nil {
			return queue.Result{}, err
		}
		if err := os.WriteFile(resPath, b, 0o644); err != nil {
			return queue.Result{}, fmt.Errorf("write %s: %w", resPath, err)
		}
		artifacts = append(artifacts, resPath)
		idx.Images = append(idx.Images, indexImage{File: p, ResultFile: resPath, Caption: d.Caption, Tags: res.Tags})
	}
	idx.Count = len(idx.Images)

	indexPath := filepath.Join(req.OutDir, "index.json")
	b, err := json.Marshal(idx)
	if err != nil {
		return queue.Result{}, err
	}
	if err := os.WriteFile(indexPath, b, 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", indexPath, err)
	}
	artifacts = append(artifacts, indexPath)

	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: artifacts,
		Summary:   fmt.Sprintf("described %d image(s) (%s)", len(paths), h.worker),
		Data: map[string]any{
			"count":      len(paths),
			"index_file": indexPath,
		},
	}, nil
}

func main() {
	mode := strings.ToLower(envOr("IDESC_MODE", "gpu"))
	if mode != "gpu" && mode != "cpu" {
		log.Fatalf("image-describe-worker: IDESC_MODE must be gpu or cpu, got %q", mode)
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("image-describe-worker: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("image-describe-worker: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "image-describe-worker-"+mode)
	if err != nil {
		log.Printf("image-describe-worker: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	model := modelForMode(mode)
	srv := &vlmserver.Server{
		Binary:          envOr("IDESC_LLAMA_SERVER", "/opt/jarvis-engine/bin/llama-server"),
		Model:           envOr("IDESC_MODEL", filepath.Join(modelDir, modelFileForMode(mode, "gguf"))),
		MMProj:          envOr("IDESC_MMPROJ", filepath.Join(modelDir, modelFileForMode(mode, "mmproj"))),
		Port:            envInt("IDESC_PORT", 11510),
		NGL:             nglForMode(mode),
		NoMMProjOffload: mode == "cpu",
		CtxSize:         envInt("IDESC_CTX", 4096),
		Threads:         envInt("IDESC_THREADS", 0),
		Log:             log.Default(),
	}

	cfg := capworker.Config{
		Cap:         capability,
		Name:        fmt.Sprintf(workerName, mode),
		Model:       model,
		Location:    mode, // gpu → consumer priority 10, cpu → 1
		Conn:        conn,
		Events:      events,
		MetricsAddr: os.Getenv("IDESC_METRICS_ADDR"),
		IdleS:       300 * time.Second, // unload the model 5 min after the last request
		Load:        srv.Start,
		Unload:      func(ctx context.Context) error { return srv.Stop() },
		Handler:     handler{srv: srv, worker: fmt.Sprintf(workerName, mode), model: model},
		OutDirBase:  envOr("IDESC_OUT_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")),
		Log:         log.Default(),
	}
	if mode == "gpu" {
		cfg.Lease = gpuLease(envOr("IDESC_SMI", "nvidia-smi"), envInt("IDESC_MIN_FREE_MB", 4096))
	}

	if err := capworker.New(cfg).Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("image-describe-worker: run: %v", err)
	}
	log.Printf("image-describe-worker: stopped")
}

// nglForMode: GPU loads every layer onto the 4090; CPU keeps them all in
// host RAM (the cpu placement's residual VRAM is the CUDA-context floor
// measured in the bench — 488 MiB for the 2B, 580 for the 8B; the binary
// always initializes the CUDA backend, whatever --n-gpu-layers says).
func nglForMode(mode string) int {
	if mode == "cpu" {
		return 0
	}
	return 99
}

// modelFileForMode is the file name under /opt/jarvis-vlm/models for the
// mode's vision GGUF ("gguf") or its F16 projector ("mmproj").
func modelFileForMode(mode, kind string) string {
	size := "2B"
	if mode == "cpu" {
		size = "8B"
	}
	if kind == "mmproj" {
		return "mmproj-Qwen3VL-" + size + "-Instruct-F16.gguf"
	}
	return "Qwen3VL-" + size + "-Instruct-Q4_K_M.gguf"
}
