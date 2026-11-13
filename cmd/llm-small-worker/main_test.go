package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/vlmserver"
)

func TestExpandTextRefs(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	txt := write("a.txt")
	md := write("b.md")
	write("c.png") // not a text file

	t.Run("directory", func(t *testing.T) {
		got, err := expandTextRefs(queue.Message{InputRefs: []string{dir}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != txt || got[1] != md {
			t.Errorf("got %v, want [%s %s] (sorted, text only)", got, txt, md)
		}
	})

	t.Run("explicit-file", func(t *testing.T) {
		got, err := expandTextRefs(queue.Message{InputRefs: []string{txt}})
		if err != nil || len(got) != 1 || got[0] != txt {
			t.Errorf("got %v err %v", got, err)
		}
	})

	t.Run("dedup-and-mixed", func(t *testing.T) {
		got, err := expandTextRefs(queue.Message{InputRefs: []string{dir, txt}})
		if err != nil || len(got) != 2 {
			t.Errorf("got %v err %v", got, err)
		}
	})

	t.Run("missing-ref", func(t *testing.T) {
		if _, err := expandTextRefs(queue.Message{InputRefs: []string{filepath.Join(dir, "nope.txt")}}); err == nil {
			t.Error("expected error for a missing ref")
		}
	})

	t.Run("non-text-file-ref", func(t *testing.T) {
		png := write("d.png")
		if _, err := expandTextRefs(queue.Message{InputRefs: []string{png}}); err == nil {
			t.Error("expected error for a non-text file ref")
		}
	})

	t.Run("dir-with-no-text", func(t *testing.T) {
		empty := t.TempDir()
		os.WriteFile(filepath.Join(empty, "pic.png"), []byte("x"), 0o644)
		if _, err := expandTextRefs(queue.Message{InputRefs: []string{empty}}); err == nil {
			t.Error("expected error for a directory without text files")
		}
	})

	t.Run("nothing", func(t *testing.T) {
		if _, err := expandTextRefs(queue.Message{}); err == nil {
			t.Error("expected error with no input at all")
		}
	})
}

func TestLoadInputs(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, make([]byte, 200*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("small-uncut", func(t *testing.T) {
		small := filepath.Join(dir, "small.txt")
		os.WriteFile(small, []byte("hello"), 0o644)
		body, truncated, err := loadInputs([]string{small})
		if err != nil {
			t.Fatal(err)
		}
		if truncated || !strings.Contains(body, "===== small.txt =====") || !strings.Contains(body, "hello") {
			t.Errorf("body = %q, truncated = %v", body, truncated)
		}
	})

	t.Run("cap-and-marker", func(t *testing.T) {
		body, truncated, err := loadInputs([]string{big})
		if err != nil {
			t.Fatal(err)
		}
		if !truncated {
			t.Error("expected truncation for a 200 KB file")
		}
		if len(body) > inputCapBytes+64 {
			t.Errorf("body %d bytes exceeds the cap", len(body))
		}
		if !strings.Contains(body, "truncated") {
			t.Error("missing the truncation marker")
		}
	})
}

func TestTaskPrompt(t *testing.T) {
	if p := taskPrompt("summarise", "", "BODY"); !strings.Contains(p, "Summarise") || strings.Contains(p, "JSON") {
		t.Errorf("summarise prompt must be plain text: %q", p)
	}
	if p := taskPrompt("classify", "", "BODY"); !strings.Contains(p, "JSON only") || !strings.Contains(p, `"label"`) {
		t.Errorf("classify prompt must demand JSON: %q", p)
	}
	if p := taskPrompt("extract", "extract: date, amount", "BODY"); !strings.Contains(p, "date, amount") {
		t.Errorf("extract prompt must carry the instructions: %q", p)
	}
	if p := taskPrompt("rewrite as haiku", "", "BODY"); !strings.Contains(p, "rewrite as haiku") {
		t.Errorf("custom task must be passed through: %q", p)
	}
	if p := taskPrompt("summarise", "  ", "BODY"); strings.Contains(p, "Instructions:") {
		t.Errorf("blank instructions must be dropped: %q", p)
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		key  string
		val  any
		fail bool
	}{
		{"plain", `{"label":"bug","reason":"it crashes"}`, "label", "bug", false},
		{"fenced", "```json\n{\"label\":\"ops\",\"reason\":\"disk\"}\n```", "label", "ops", false},
		{"prose-around", `Here you go: {"label":"security"} — done`, "label", "security", false},
		{"truncated", `{"label":"un`, "", nil, true},
		{"no-json", "I would classify this as a bug.", "", nil, true},
		{"empty", "", "", nil, true},
		{"array", `[1,2]`, "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := vlmserver.ExtractJSONObject(c.in)
			if c.fail {
				if err == nil {
					t.Fatalf("expected error, got %v", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m[c.key] != c.val {
				t.Errorf("%s = %v, want %v", c.key, m[c.key], c.val)
			}
		})
	}
}

// fakeLLM serves /health and /v1/chat/completions like llama-server does,
// returning one canned reply for every conversation.
func fakeLLM(t *testing.T, reply string) *vlmserver.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if len(req.Messages) == 0 {
			http.Error(w, "no messages", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &vlmserver.Server{Model: "fake", BaseURL: srv.URL}
}

func writeInput(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHandlerSummarise(t *testing.T) {
	in := writeInput(t, t.TempDir(), "notes.txt", "The disk filled up; we cleared old logs.")
	out := t.TempDir()
	h := handler{srv: fakeLLM(t, "A report about a full disk and the fix."), worker: "llm.small@gpu/llm-small-worker", model: modelID, maxTokens: 256, temp: 0.2}

	res, err := h.Handle(context.Background(), queue.Message{
		CorrelationID: "corr-test",
		Input:         map[string]any{"task": "summarise", "instructions": "one sentence"},
		InputRefs:     []string{in},
		OutDir:        out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != queue.ResultOK {
		t.Errorf("status = %s", res.Status)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0] != filepath.Join(out, "result.json") {
		t.Errorf("artifacts = %v", res.Artifacts)
	}
	r, ok := res.Data["result"].(string)
	if !ok || !strings.Contains(r, "disk") {
		t.Errorf("Data[result] = %v, want the summary text", res.Data["result"])
	}
	if res.Data["task"] != "summarise" || res.Data["parse_error"] != nil {
		t.Errorf("Data = %v", res.Data)
	}
	if !strings.Contains(res.Summary, "summarise") || !strings.Contains(res.Summary, "1 file(s)") {
		t.Errorf("summary = %q", res.Summary)
	}

	var art taskResult
	b, err := os.ReadFile(res.Artifacts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &art); err != nil {
		t.Fatalf("result.json does not parse: %v", err)
	}
	if art.Capability != capability || art.Model != modelID || art.Worker != h.worker {
		t.Errorf("artifact = %+v", art)
	}
	if len(art.Files) != 1 || art.Files[0] != in {
		t.Errorf("artifact files = %v", art.Files)
	}
	if s, _ := art.Result.(string); !strings.Contains(s, "disk") {
		t.Errorf("artifact result = %v", art.Result)
	}
}

func TestHandlerClassifyJSON(t *testing.T) {
	in := writeInput(t, t.TempDir(), "a.md", "Disk space is low on /var.")
	out := t.TempDir()
	h := handler{srv: fakeLLM(t, `{"label":"ops","reason":"mentions disk space"}`), worker: "w", model: modelID}

	res, err := h.Handle(context.Background(), queue.Message{
		CorrelationID: "c", Input: map[string]any{"task": "classify"}, InputRefs: []string{in}, OutDir: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.Data["result"].(map[string]any)
	if !ok || m["label"] != "ops" {
		t.Errorf("Data[result] = %v, want the parsed JSON object", res.Data["result"])
	}
	if res.Data["parse_error"] != nil {
		t.Errorf("no parse_error expected: %v", res.Data)
	}
}

func TestHandlerClassifyFenced(t *testing.T) {
	in := writeInput(t, t.TempDir(), "a.md", "x")
	h := handler{srv: fakeLLM(t, "```json\n{\"label\":\"sec\"}\n```"), worker: "w", model: modelID}
	res, err := h.Handle(context.Background(), queue.Message{
		CorrelationID: "c", Input: map[string]any{"task": "classify"}, InputRefs: []string{in}, OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.Data["result"].(map[string]any)
	if !ok || m["label"] != "sec" {
		t.Errorf("fenced JSON must be recovered: %v", res.Data["result"])
	}
}

func TestHandlerClassifyBrokenJSON(t *testing.T) {
	in := writeInput(t, t.TempDir(), "a.md", "x")
	h := handler{srv: fakeLLM(t, `{"label":"un`), worker: "w", model: modelID}
	res, err := h.Handle(context.Background(), queue.Message{
		CorrelationID: "c", Input: map[string]any{"task": "classify"}, InputRefs: []string{in}, OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Data["parse_error"] != true {
		t.Errorf("parse_error expected: %v", res.Data)
	}
	if s, ok := res.Data["result"].(string); !ok || s == "" {
		t.Errorf("broken JSON must fall back to the raw text: %v", res.Data["result"])
	}
}

func TestHandlerBadMessage(t *testing.T) {
	h := handler{srv: fakeLLM(t, "x"), worker: "w", model: modelID}
	dir := t.TempDir()

	if _, err := h.Handle(context.Background(), queue.Message{CorrelationID: "c1", InputRefs: []string{dir}}); err == nil {
		t.Error("expected an error for a request without a task")
	}
	if _, err := h.Handle(context.Background(), queue.Message{CorrelationID: "c2", Input: map[string]any{"task": "summarise"}}); err == nil {
		t.Error("expected an error for a request without input refs")
	}
}

func TestNGLForMode(t *testing.T) {
	if nglForMode("cpu") != 0 {
		t.Error("cpu mode must run with 0 GPU layers")
	}
	if nglForMode("gpu") != 99 {
		t.Error("gpu mode must load all layers")
	}
}

// TestStartValidation pins the vlmserver change for text-only models:
// MMProj is optional (no projector flag), while Binary+Model are still
// required.
func TestStartValidation(t *testing.T) {
	srv := &vlmserver.Server{Binary: "/bin/sleep"}
	if err := srv.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing model must be a validation error, got %v", err)
	}
	// No mmproj is fine: the server is attempted (sleep never serves
	// /health, so Start fails with a timeout — not a validation error).
	srv = &vlmserver.Server{Binary: "/bin/sleep", Model: "m", Port: 1, HealthTimeout: 2 * time.Second}
	if err := srv.Start(context.Background()); err == nil || strings.Contains(err.Error(), "required") {
		t.Fatalf("a text model without mmproj must pass validation: %v", err)
	}
}
