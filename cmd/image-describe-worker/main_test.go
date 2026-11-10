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

func TestParseDescription(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		caption string
		tags    []string // when non-nil, tags must match exactly
		fail    bool
	}{
		{"plain", `{"caption":"A red circle.","prompt":"red circle on gray","tags":["circle","red"]}`, "A red circle.", nil, false},
		{"fenced", "```json\n{\"caption\":\"A sign.\",\"prompt\":\"sign\",\"tags\":[\"sign\"]}\n```", "A sign.", nil, false},
		{"prose-around", `Here you go: {"caption":"A fox.","prompt":"fox","tags":[]} — hope that helps`, "A fox.", nil, false},
		// The real bench failure: Qwen3-VL-2B looped its tags and hit
		// max_tokens mid-array. The lenient path must recover the
		// caption and the complete tags, dropping the truncated one.
		{"truncated-json", `{"caption":"A photo of a person walking a dog in a park.","prompt":"a person walking a dog in a park, natural light","tags":["person","dog","park","person","dog","pers`,
			"A photo of a person walking a dog in a park.",
			[]string{"person", "dog", "park", "person", "dog"}, false},
		{"bad-json", `{"caption": "unclosed`, "", nil, true},
		{"no-json", "I cannot see the image.", "", nil, true},
		{"empty-caption", `{"caption":"","prompt":"p","tags":[]}`, "", nil, true},
		{"empty", "", "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := vlmserver.ParseDescription(c.in)
			if c.fail {
				if err == nil {
					t.Fatalf("expected error, got %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Caption != c.caption {
				t.Errorf("caption = %q, want %q", d.Caption, c.caption)
			}
			if c.tags != nil {
				if len(d.Tags) != len(c.tags) {
					t.Errorf("tags = %v, want %v", d.Tags, c.tags)
				} else {
					for i := range c.tags {
						if d.Tags[i] != c.tags[i] {
							t.Errorf("tags = %v, want %v", d.Tags, c.tags)
							break
						}
					}
				}
			}
		})
	}
}

func TestExpandRefs(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	png := write("a.png")
	jpg := write("b.jpg")
	write("notes.txt")
	write("sub") // a directory entry is not an image

	t.Run("directory", func(t *testing.T) {
		got, err := expandRefs(queue.Message{InputRefs: []string{dir}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != png || got[1] != jpg {
			t.Errorf("got %v, want [%s %s] (sorted, images only)", got, png, jpg)
		}
	})

	t.Run("explicit-file", func(t *testing.T) {
		got, err := expandRefs(queue.Message{Input: map[string]any{"image": png}})
		if err != nil || len(got) != 1 || got[0] != png {
			t.Errorf("got %v err %v", got, err)
		}
	})

	t.Run("dedup-and-mixed", func(t *testing.T) {
		got, err := expandRefs(queue.Message{InputRefs: []string{dir, png}})
		if err != nil || len(got) != 2 {
			t.Errorf("got %v err %v", got, err)
		}
	})

	t.Run("missing-ref", func(t *testing.T) {
		if _, err := expandRefs(queue.Message{InputRefs: []string{filepath.Join(dir, "nope.png")}}); err == nil {
			t.Error("expected error for a missing ref")
		}
	})

	t.Run("non-image-file-ref", func(t *testing.T) {
		txt := write("c.txt")
		if _, err := expandRefs(queue.Message{InputRefs: []string{txt}}); err == nil {
			t.Error("expected error for a non-image file ref")
		}
	})

	t.Run("dir-with-no-images", func(t *testing.T) {
		empty := t.TempDir()
		os.WriteFile(filepath.Join(empty, "readme.md"), []byte("x"), 0o644)
		if _, err := expandRefs(queue.Message{InputRefs: []string{empty}}); err == nil {
			t.Error("expected error for a directory without images")
		}
	})

	t.Run("nothing", func(t *testing.T) {
		if _, err := expandRefs(queue.Message{}); err == nil {
			t.Error("expected error with no input at all")
		}
	})
}

// fakeVLM serves /health and /v1/chat/completions like llama-server does,
// returning one canned description per image (derived from the data URI's
// length, so the test can tell the images apart).
func fakeVLM(t *testing.T) *vlmserver.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var imgLen int
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if c.Type == "image_url" && strings.HasPrefix(c.ImageURL.URL, "data:image/") {
					imgLen = len(c.ImageURL.URL)
				}
			}
		}
		content, _ := json.Marshal(map[string]any{
			"caption": "image #" + itoa(imgLen),
			"prompt":  "a recreated image #" + itoa(imgLen),
			"tags":    []string{"test", "vlm"},
		})
		resp := map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": string(content)}}}}
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &vlmserver.Server{
		Model:   "fake",
		MMProj:  "fake",
		BaseURL: srv.URL,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestHandlerDescribesAllImages(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"one.png", "two.jpg"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("fake-image-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	h := handler{srv: fakeVLM(t), worker: "image.describe@gpu/image-describe-worker", model: "qwen3-vl-2b"}

	res, err := h.Handle(context.Background(), queue.Message{
		CorrelationID: "corr-test",
		InputRefs:     []string{dir},
		OutDir:        out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != queue.ResultOK {
		t.Errorf("status = %s", res.Status)
	}
	// Artifacts: one per image + the index, index last.
	if len(res.Artifacts) != 3 {
		t.Fatalf("artifacts = %v", res.Artifacts)
	}
	idxPath := filepath.Join(out, "index.json")
	if res.Artifacts[len(res.Artifacts)-1] != idxPath {
		t.Errorf("last artifact = %s, want %s", res.Artifacts[len(res.Artifacts)-1], idxPath)
	}

	var idx indexFile
	b, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatalf("index.json does not parse: %v", err)
	}
	if idx.Capability != "image.describe" || idx.Count != 2 || len(idx.Images) != 2 {
		t.Errorf("index = %+v", idx)
	}
	if !strings.HasPrefix(idx.Images[0].Caption, "image #") || idx.Images[0].Tags == nil {
		t.Errorf("index image entry = %+v", idx.Images[0])
	}

	// Per-image artifact: caption/prompt/tags + provenance.
	var one imageResult
	onePath := filepath.Join(out, "one.json")
	b, err = os.ReadFile(onePath)
	if err != nil {
		t.Fatalf("missing per-image artifact: %v", err)
	}
	if err := json.Unmarshal(b, &one); err != nil {
		t.Fatal(err)
	}
	if one.Image != filepath.Join(dir, "one.png") || !strings.HasPrefix(one.Caption, "image #") || one.Model != h.model || one.Worker != h.worker {
		t.Errorf("one.json = %+v", one)
	}
}

func TestHandlerBadMessageFails(t *testing.T) {
	h := handler{srv: fakeVLM(t), worker: "test"}
	if _, err := h.Handle(context.Background(), queue.Message{CorrelationID: "c2", OutDir: t.TempDir()}); err == nil {
		t.Error("expected an error for a request with no images")
	}
}

// TestFreeVRAMLease checks the GPU lease against a fake nvidia-smi.
func TestFreeVRAMLease(t *testing.T) {
	smi := filepath.Join(t.TempDir(), "smi")
	write := func(free string) {
		if err := os.WriteFile(smi, []byte("#!/bin/sh\necho "+free), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	write("4200")
	if err := gpuLease(smi, 4096)(ctx); err != nil {
		t.Errorf("4200 free vs 4096 needed: %v", err)
	}
	write("2000")
	if err := gpuLease(smi, 4096)(ctx); err == nil {
		t.Error("expected the lease to refuse 2000 free vs 4096 needed")
	}
	write("garbage")
	if err := gpuLease(smi, 4096)(ctx); err == nil {
		t.Error("expected the lease to fail on unparsable nvidia-smi output")
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

func TestModelSelection(t *testing.T) {
	if modelForMode("gpu") != "qwen3-vl-2b" {
		t.Errorf("gpu placement should run the 2B: %s", modelForMode("gpu"))
	}
	if modelForMode("cpu") != "qwen3-vl-4b" {
		t.Errorf("cpu placement should run the 4B: %s", modelForMode("cpu"))
	}
	if got := modelFileForMode("gpu", "gguf"); got != "Qwen3VL-2B-Instruct-Q4_K_M.gguf" {
		t.Errorf("gpu gguf file = %s", got)
	}
	if got := modelFileForMode("cpu", "mmproj"); got != "mmproj-Qwen3VL-4B-Instruct-F16.gguf" {
		t.Errorf("cpu mmproj file = %s", got)
	}
}

// TestServerStartStop exercises the coprocess manager against a tiny fake
// "server" (a shell script that answers /health is not possible in sh, so
// the fake is a Go-free stand-in: a long-sleeping process that the manager
// must start, observe as unhealthy, and stop cleanly). This pins the
// lifecycle semantics the SDK's Load/Unload hooks rely on.
func TestServerStartStop(t *testing.T) {
	srv := &vlmserver.Server{
		Binary:        "/bin/sleep",
		Model:         "m",
		MMProj:        "p",
		Port:          1, // unused: the fake never serves /health
		HealthTimeout: 2 * time.Second,
	}
	// sleep is not a server: Start must time out (bounded) and leave no
	// process behind.
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail for a non-server binary")
	}
	if srv.Started() {
		t.Error("server must not be marked started after a failed Start")
	}
	// Stop on a never-started server is a no-op.
	if err := srv.Stop(); err != nil {
		t.Errorf("Stop on stopped server: %v", err)
	}
}
