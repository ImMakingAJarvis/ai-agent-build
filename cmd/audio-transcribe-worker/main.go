// audio-transcribe-worker is the audio.transcribe capability worker (plan
// task EVB-22): the CPU placement behind cap.audio.transcribe. It consumes
// cap.audio.transcribe, transcribes each input clip with faster-whisper
// (Whisper small, CTranslate2 int8 on CPU — picked by the bench in
// docs/benchmarks/audio-transcribe-whisper-benchmark.md: 3.6x faster than
// whisper.cpp at equal WER, ~760 MB peak RSS), and replies with a Result
// whose Artifacts point at one JSON per clip, one plain-text transcript
// per clip, and an index file — references, not bytes (invariant 7).
//
// It is the first capability that runs alongside the brain with no window:
// no GPU and no swap window (needs_brain_out: false) — its resourced lease
// (plan §3.6) only draws on the shared RAM pool, so it is granted unless
// the host RAM ledger is full.
//
// The engine is faster-whisper running as a per-request Python subprocess
// (transcribe.py, shipped in the image): the model is loaded per
// invocation (~2.2 s for small) and the process exits when the request is
// done. No coprocess daemon, nothing resident between requests — so there
// is no idle-unload (IdleS 0, no Load/Unload) and the worker holds no
// model RAM at idle.
//
// Request input (envelope v2):
//
//	InputRefs  []string  one or more paths (files or directories) — camera
//	         clips (mp4/mov/mkv/...) or audio (wav/mp3/m4a/...); a
//	         directory is scanned for audio/video extensions. The refs are
//	         read-only: the worker never modifies its inputs.
//	Input["audio"] string  optional — one explicit clip path
//
// Output (in the request's OutDir):
//
//	index.json   {capability, count, worker, model, created, clips:
//	             [{file, result_file, text_file, text, seconds,
//	              duration_s, segments}]}
//	<stem>.json  one per clip — the faster-whisper result {file, language,
//	             duration, segments, text, ...}
//	<stem>.txt   the plain-text transcript, one per clip
//
// A request whose refs point at nothing (or a ref that does not exist
// inside the container) is a bad message: the handler errors, the retry
// budget burns, and it dead-letters with the reason (invariant 9).
//
// Configuration (env):
//
//	RABBITMQ_URL            the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	AT_MODEL_DIR            model root. Default /opt/jarvis-whisper/models.
//	AT_MODEL                model subdir (a CT2 directory: model.bin + config.json). Default small.
//	AT_THREADS              ctranslate2 cpu_threads. Default 16 (bench setting).
//	AT_BEAM_SIZE            beam_size. Default 5.
//	AT_PYTHON               interpreter for transcribe.py. Default python3.
//	AT_SCRIPT               transcribe.py. Default /usr/local/lib/jarvis-whisper/transcribe.py.
//	RESOURCED_URL           the resourced lease broker (plan §3.6); the
//	                          worker gates its consuming on a lease from it.
//	                          Default: off (un-gated, the pre-lease behavior).
//	CAPABILITIES_FILE       the capability catalog the lease's memory spec
//	                          comes from. Needed when RESOURCED_URL is set.
//	AT_OUT_DIR              artifact root for requests without an OutDir.
//	                          Default ~/.local/share/jarvis/artifacts.
//	AT_METRICS_ADDR         serve Prometheus /metrics here. Default: off.
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
)

const (
	capability = "audio.transcribe"
	workerName = "audio.transcribe@cpu/audio-transcribe-worker"

	// modelID is the models.yaml registry entry the capability points at
	// (the worker runs it through faster-whisper, not a serving engine).
	modelID    = "whisper-small"
	modelDir   = "/opt/jarvis-whisper/models"
	scriptPath = "/usr/local/lib/jarvis-whisper/transcribe.py"
)

// audioExts: containers PyAV decodes for us (the bench clip was a WAV;
// the camera clips are UniFi MP4s). A file ref must have one of these;
// a directory ref is scanned for them.
var audioExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".avi": true, ".flv": true,
	".webm": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".3gp": true,
	".wav": true, ".mp3": true, ".m4a": true, ".aac": true, ".ogg": true,
	".oga": true, ".flac": true, ".opus": true, ".mka": true,
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
		log.Printf("audio-transcribe-worker: broker not ready: %v", err)
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

// expandRefs turns the request's audio input into a sorted, de-duplicated
// list of clip files. A directory ref is scanned for audio/video
// extensions; an explicit file ref must itself be a clip. The refs are
// read-only — this never writes to its inputs.
func expandRefs(req queue.Message) ([]string, error) {
	var refs []string
	if s, ok := req.Input["audio"].(string); ok && strings.TrimSpace(s) != "" {
		refs = append(refs, strings.TrimSpace(s))
	}
	refs = append(refs, req.InputRefs...)
	if len(refs) == 0 {
		return nil, errors.New(`no audio: give --input-ref <folder-or-clip> (or Input["audio"])`)
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
				if e.IsDir() || !audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
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
		if !audioExts[strings.ToLower(filepath.Ext(r))] {
			return nil, fmt.Errorf("input ref %q is not an audio/video file", r)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("input refs contain no audio files")
	}
	sort.Strings(out)
	return out, nil
}

// handler does one audio.transcribe request.
type handler struct {
	worker      string // for the artifacts and the Result
	modelID     string // registry id (models.yaml), for the index artifact
	python      string
	script      string
	modelDir    string // root; the model's subdir is appended per request
	modelSubdir string
	threads     int
	beam        int
}

// transcribeResult is the JSON transcribe.py writes (the fields the
// worker reads back; the script writes a few more for the record).
type transcribeResult struct {
	File        string  `json:"file"`
	Language    string  `json:"language"`
	Duration    float64 `json:"duration"`
	TranscribeS float64 `json:"transcribe_seconds"`
	LoadS       float64 `json:"model_load_seconds"`
	Segments    []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
	Text string `json:"text"`
}

// indexClip is one entry of index.json.
type indexClip struct {
	File       string  `json:"file"`
	ResultFile string  `json:"result_file"`
	TextFile   string  `json:"text_file"`
	Text       string  `json:"text"`
	Seconds    float64 `json:"seconds"`
	DurationS  float64 `json:"duration_s"`
	Segments   int     `json:"segments"`
}

// indexFile is the request's index artifact.
type indexFile struct {
	Capability string      `json:"capability"`
	Count      int         `json:"count"`
	Worker     string      `json:"worker"`
	Model      string      `json:"model"`
	Created    time.Time   `json:"created"`
	Clips      []indexClip `json:"clips"`
}

// runTranscribe spawns the per-request faster-whisper subprocess:
// python3 transcribe.py <clip> <out.json> <model dir> <threads> <beam>.
// The subprocess's stderr (progress lines) goes straight to the worker's
// stderr; its exit status and the written JSON are the contract.
func (h handler) runTranscribe(ctx context.Context, clip, outPath string) error {
	args := []string{h.script, clip, outPath, filepath.Join(h.modelDir, h.modelSubdir),
		strconv.Itoa(h.threads), strconv.Itoa(h.beam)}
	cmd := exec.CommandContext(ctx, h.python, args...)
	cmd.Stderr = os.Stderr
	log.Printf("audio-transcribe-worker: %s: %s %s", "transcribe", h.python, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("transcribe %s: %w", clip, err)
	}
	return nil
}

func (h handler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	paths, err := expandRefs(req)
	if err != nil {
		return queue.Result{}, fmt.Errorf("audio.transcribe: %w", err)
	}
	log.Printf("audio-transcribe-worker: %s: transcribing %d clip(s)", req.CorrelationID, len(paths))

	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}

	idx := indexFile{Capability: capability, Worker: h.worker, Model: h.modelID, Created: time.Now()}
	artifacts := make([]string, 0, len(paths)*2+1)
	var totalDur float64
	var totalSegs int
	for _, p := range paths {
		start := time.Now()
		stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		resPath := filepath.Join(req.OutDir, stem+".json")
		txtPath := filepath.Join(req.OutDir, stem+".txt")

		if err := h.runTranscribe(ctx, p, resPath); err != nil {
			return queue.Result{}, err
		}
		b, err := os.ReadFile(resPath)
		if err != nil {
			return queue.Result{}, fmt.Errorf("read %s: %w", resPath, err)
		}
		var tr transcribeResult
		if err := json.Unmarshal(b, &tr); err != nil {
			return queue.Result{}, fmt.Errorf("parse %s: %w", resPath, err)
		}
		if err := os.WriteFile(txtPath, []byte(tr.Text), 0o644); err != nil {
			return queue.Result{}, fmt.Errorf("write %s: %w", txtPath, err)
		}

		artifacts = append(artifacts, resPath, txtPath)
		totalDur += tr.Duration
		totalSegs += len(tr.Segments)
		idx.Clips = append(idx.Clips, indexClip{
			File: p, ResultFile: resPath, TextFile: txtPath,
			Text: tr.Text, Seconds: time.Since(start).Seconds(),
			DurationS: tr.Duration, Segments: len(tr.Segments),
		})
		log.Printf("audio-transcribe-worker: %s: %s: %d segment(s), %.1fs audio in %.1fs",
			req.CorrelationID, filepath.Base(p), len(tr.Segments), tr.Duration, time.Since(start).Seconds())
	}
	idx.Count = len(idx.Clips)

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
		Summary:   fmt.Sprintf("transcribed %d clip(s), %.0fs audio (%s)", len(paths), totalDur, h.worker),
		Data: map[string]any{
			"count":            len(paths),
			"index_file":       indexPath,
			"total_duration_s": totalDur,
			"segments":         totalSegs,
		},
	}, nil
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("audio-transcribe-worker: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("audio-transcribe-worker: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "audio-transcribe-worker")
	if err != nil {
		log.Printf("audio-transcribe-worker: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	// Lease gating (plan §3.6): the cpu placement's lease draws only on
	// the shared RAM pool (the catalog's ram_mb). No broker
	// (RESOURCED_URL unset) or a catalog problem = un-gated, the
	// pre-lease behavior.
	leaseBroker, leaseSpec, lerr := capworker.LeaseFor(
		os.Getenv("RESOURCED_URL"), os.Getenv("CAPABILITIES_FILE"),
		capability, "cpu", workerName)
	if lerr != nil {
		log.Printf("audio-transcribe-worker: lease: %v (running un-gated)", lerr)
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
		MetricsAddr: os.Getenv("AT_METRICS_ADDR"),
		Handler: handler{
			worker:      workerName,
			modelID:     modelID,
			python:      envOr("AT_PYTHON", "python3"),
			script:      envOr("AT_SCRIPT", scriptPath),
			modelDir:    envOr("AT_MODEL_DIR", modelDir),
			modelSubdir: envOr("AT_MODEL", "small"),
			threads:     envInt("AT_THREADS", 16),
			beam:        envInt("AT_BEAM_SIZE", 5),
		},
		OutDirBase: envOr("AT_OUT_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")),
		Log:        log.Default(),
	}

	if err := capworker.New(cfg).Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("audio-transcribe-worker: run: %v", err)
	}
	log.Printf("audio-transcribe-worker: stopped")
}
