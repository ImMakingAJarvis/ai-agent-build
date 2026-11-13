package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The llama-bench table with -dev (the in-container iGPU case): the dev
// column is present, the t/s cells carry a ± spread, and non-table lines
// (device detection, the build footer) must be ignored.
const llamaBenchIGPU = `
| model                          |       size |     params | backend    | ngl | dev          |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | ------------ | --------------: | -------------------: |
| qwen2.5-3b-q4_k_m (3.4B)       | 1.9 GiB    | 3.4B       | AVX2       | 99  | Vulkan0      |          pp512 |           108.70 ± 0.00 |
| qwen2.5-3b-q4_k_m (3.4B)       | 1.9 GiB    | 3.4B       | AVX2       | 99  | Vulkan0      |          tg512 |            3.50 ± 0.01 |
build: cf24317 (1)
`

// The CPU case has no dev column (llama-bench omits it without -dev).
const llamaBenchCPU = `
| model                 |       size |     params | backend    | ngl |            test |                  t/s |
| --------------------- | ---------: | ---------: | ---------- | --: | --------------: | -------------------: |
| llama3.2-1b (1.24B)   | 836.4 MiB  | 1.24B      | AVX2       |   0 |          pp128 |            9.12 ± 0.03 |
| llama3.2-1b (1.24B)   | 836.4 MiB  | 1.24B      | AVX2       |   0 |           tg32 |            2.41 ± 0.02 |
`

func TestParseLlamaBench(t *testing.T) {
	t.Run("igpu table", func(t *testing.T) {
		rows, err := parseLlamaBench(llamaBenchIGPU)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
		}
		pp, tg := rows[0], rows[1]
		if pp.Test != "pp512" || pp.TPS != 108.70 || pp.Spread != 0 {
			t.Errorf("pp row: %+v", pp)
		}
		if tg.Test != "tg512" || tg.TPS != 3.50 || tg.Spread != 0.01 {
			t.Errorf("tg row: %+v", tg)
		}
		if pp.Model != "qwen2.5-3b-q4_k_m (3.4B)" || pp.Dev != "Vulkan0" || pp.Backend != "AVX2" {
			t.Errorf("row metadata: %+v", pp)
		}
	})

	t.Run("cpu table without dev column", func(t *testing.T) {
		rows, err := parseLlamaBench(llamaBenchCPU)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].Dev != "" || rows[0].Test != "pp128" || rows[0].TPS != 9.12 {
			t.Fatalf("got %+v", rows)
		}
	})

	t.Run("no rows is an error", func(t *testing.T) {
		// A failed model load still prints the header and exits 0.
		out := `
| model                          |       size |     params | backend    | ngl | dev          |            test |                  t/s |
| ------------------------------ | ---------: | ---------: | ---------- | --: | ------------ | --------------: | -------------------: |
`
		if _, err := parseLlamaBench(out); err == nil {
			t.Fatal("want an error for a table with no result rows")
		}
	})
}

// A realistic benchmark_q6.py transcript (the live bench path).
const liveBenchOut = `
| Run | Prompt tok | Prompt tok/s | Gen tok | Gen tok/s | Wall s |
|---|---|---|---|---|---|
| 1 | 3921 | 153 | 287 | 8.5 | 41.2 |
| 2 | 3921 | 152 | 301 | 8.4 | 42.0 |

task PASS (12 tok, 1.5s): Reply with only the number: what is 17*23 + 4?
task FAIL (245 tok, 30.1s): Write a Python function is_palindrome(s) ignoring c
tool_call PASS: [{"id": "call_1", "type": "function"}]

long-context: prompt 8471 tok at 150 tok/s, decode 8.3 tok/s, wall 62.1s, needle PASS
`

func TestParseLiveBench(t *testing.T) {
	s, err := parseLiveBench(liveBenchOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Runs) != 2 {
		t.Fatalf("got %d runs, want 2: %+v", len(s.Runs), s.Runs)
	}
	r := s.Runs[0]
	if r.PromptN != 3921 || r.PromptTS != 153 || r.GenN != 287 || r.GenTS != 8.5 || r.WallS != 41.2 {
		t.Errorf("run 1: %+v", r)
	}
	if len(s.Tasks) != 2 || !s.Tasks[0].Pass || s.Tasks[0].GenTok != 12 || s.Tasks[1].Pass {
		t.Errorf("tasks: %+v", s.Tasks)
	}
	if !s.ToolCall {
		t.Error("tool_call should parse as PASS")
	}
	if s.LongCtx == nil {
		t.Fatal("long-context line missing")
	}
	lc := s.LongCtx
	if lc.PromptN != 8471 || lc.PromptTS != 150 || lc.DecodeTS != 8.3 || lc.WallS != 62.1 || !lc.Needle {
		t.Errorf("long context: %+v", lc)
	}
}

func TestParseLiveBenchEmpty(t *testing.T) {
	if _, err := parseLiveBench("nothing to see\n"); err == nil {
		t.Fatal("want an error for output with no runs or tasks")
	}
}

func TestParseParams(t *testing.T) {
	t.Run("llama-bench defaults", func(t *testing.T) {
		p, err := parseParams(map[string]any{"model": "llama3.2:1b"})
		if err != nil {
			t.Fatal(err)
		}
		if p.bench != benchLlamaBench || p.device != deviceIGPU || p.prompt != 512 || p.gen != 512 || p.runs != 3 {
			t.Errorf("defaults: %+v", p)
		}
	})

	t.Run("model is required", func(t *testing.T) {
		if _, err := parseParams(map[string]any{}); err == nil {
			t.Fatal("want an error when the llama-bench model is missing")
		}
	})

	t.Run("unknown bench and device", func(t *testing.T) {
		if _, err := parseParams(map[string]any{"bench": "nope", "model": "m.gguf"}); err == nil {
			t.Error("want an error for an unknown bench")
		}
		if _, err := parseParams(map[string]any{"model": "m.gguf", "device": "4090"}); err == nil {
			t.Error("want an error for device 4090 (not visible from this worker)")
		}
	})

	t.Run("numeric inputs from JSON", func(t *testing.T) {
		p, err := parseParams(map[string]any{
			"model": "m.gguf", "device": "cpu",
			"prompt_tokens": float64(128), "gen_tokens": "64", "runs": 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if p.prompt != 128 || p.gen != 64 || p.runs != 1 || p.device != deviceCPU {
			t.Errorf("got %+v", p)
		}
	})

	t.Run("bad numbers", func(t *testing.T) {
		if _, err := parseParams(map[string]any{"model": "m.gguf", "runs": float64(2.5)}); err == nil {
			t.Error("want an error for a non-integer runs")
		}
		if _, err := parseParams(map[string]any{"model": "m.gguf", "runs": 0}); err == nil {
			t.Error("want an error for runs = 0")
		}
	})

	t.Run("live bench", func(t *testing.T) {
		p, err := parseParams(map[string]any{"bench": "live", "base_url": "http://jarvis-engine-igpu:11435", "runs": 1})
		if err != nil {
			t.Fatal(err)
		}
		if p.bench != benchLive || p.base != "http://jarvis-engine-igpu:11435" || p.fill != 0 {
			t.Errorf("got %+v", p)
		}
		if _, err := parseParams(map[string]any{"bench": "live", "base_url": "http://jarvis-engine:11434"}); err != nil {
			t.Errorf("default base_url should work: %v", err)
		}
	})
}

// fakeOllamaStore lays out a minimal Ollama model store under dir.
func fakeOllamaStore(t *testing.T, fam, tag string) (root, blob string) {
	t.Helper()
	root = t.TempDir()
	blob = filepath.Join(root, "blobs", "sha256-bbb")
	mustWrite := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `{"layers":[{"digest":"sha256-aaa","size":100},{"digest":"sha256-bbb","size":500}]}`
	mustWrite(filepath.Join(root, "manifests", "registry.ollama.ai", "library", fam, tag), manifest)
	mustWrite(blob, "fake gguf")
	return root, blob
}

func TestResolveModel(t *testing.T) {
	root, blob := fakeOllamaStore(t, "llama3.2", "1b")

	t.Run("file path passes through", func(t *testing.T) {
		got, err := resolveModel(root, blob)
		if err != nil || got != blob {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("ollama name resolves to the largest layer", func(t *testing.T) {
		got, err := resolveModel(root, "llama3.2:1b")
		if err != nil {
			t.Fatal(err)
		}
		if got != blob {
			t.Errorf("got %q, want %q (the 500-byte layer)", got, blob)
		}
	})

	t.Run("bare name means the latest tag", func(t *testing.T) {
		// Add a latest tag to the store.
		manifest := filepath.Join(root, "manifests", "registry.ollama.ai", "library", "llama3.2", "latest")
		if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifest, []byte(`{"layers":[{"digest":"sha256-bbb","size":500}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveModel(root, "llama3.2")
		if err != nil {
			t.Fatal(err)
		}
		if got != blob {
			t.Errorf("got %q, want %q", got, blob)
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		if _, err := resolveModel(root, "nope:1"); err == nil {
			t.Error("want an error for a model with no manifest")
		}
		if _, err := resolveModel(root, "not-a-file-or-name"); err == nil {
			t.Error("want an error for an unresolvable name")
		}
	})
}

// TestIndexRoundTrip pins the index.json shape the gate checks: the
// scores must survive the JSON round trip as numbers, not strings.
func TestIndexScoresRoundTrip(t *testing.T) {
	rows, err := parseLlamaBench(llamaBenchIGPU)
	if err != nil {
		t.Fatal(err)
	}
	s := map[string]any{}
	for _, r := range rows {
		s[r.Test] = map[string]any{"tps": r.TPS, "spread": r.Spread}
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["pp512"]["tps"].(float64) != 108.70 {
		t.Errorf("pp512 tps after round trip: %v", back["pp512"]["tps"])
	}
}
