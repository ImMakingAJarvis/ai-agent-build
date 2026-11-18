package dashboard

import (
	"bytes"
	"net/http"
	"strconv"
)

// /benchmarks is a static, dark-themed report of every MoE benchmark run on
// this machine: the Ollama A/B bench (qwen3:30b-a3b MoE vs the qwen3.6:27b
// dense brain, docs/moe-benchmark.md) and the two FreeToken VRAM-cache
// sweeps — Qwen 3.8 Flash Next and GLM 5.3 flash (docs/benchmarks/). Every
// number below is transcribed verbatim from those docs; the page is a
// report, not a live view, so the handler renders fixed data with no I/O.

// moeTable is one data table: a caption, headers, rows, an optional avg row
// and an optional footnote (the "* prompt avg skewed by cold load" notes).
type moeTable struct {
	Caption  string
	Headers  []string
	Rows     [][]string
	Avg      []string
	Footnote string
}

// moeBar is one directly-labelled bar in a chart.
type moeBar struct {
	Label  string
	Value  string
	Width  string // CSS width, e.g. "100%"
	Series int    // 1..3 → --series-N
}

// moeChart is one single-metric horizontal bar chart.
type moeChart struct {
	Title string
	Unit  string
	Bars  []moeBar
}

// bwGroup is one expert-format group of the GLM bandwidth chart: a CPU-MoE
// bar (series 1) and a PCIe-gather bar (series 2).
type bwGroup struct {
	Name string
	Cpu  moeBar
	Pci  moeBar
}

// moeBW is the GLM ft bench bandwidth bench: host ceilings, per-format rows,
// and the grouped bar chart.
type moeBW struct {
	Ceilings string
	Table    moeTable
	Note     string
	Groups   []bwGroup
}

// moePoint is one VRAM-sweep cache point.
type moePoint struct {
	ID      string // "Q-1", "GLM-4"
	Target  string // "~100 MB free"
	Meta    string // "run 2026-11-04 · r = 0.9825 · 137 MiB free after load"
	Outcome string // chip text: "served" | "OOM" | "not run"
	State   string // chip class: good | warn | crit
	MaxCtx  string // largest prompt served
	Note    string
	Table   *moeTable
}

// moeAttempt is a failed attempt recorded before the sweep (the Qwen Ollama
// pull).
type moeAttempt struct {
	Title string
	Body  string
}

// moeModel is one benchmarked model: the sweep points plus, for GLM, the
// provenance and bandwidth bench.
type moeModel struct {
	Name       string
	Meta       string
	Note       string
	Attempt    *moeAttempt
	Provenance string
	Shards     *moeTable
	WhyQuant   string
	BW         *moeBW
	Points     []moePoint
}

// abModel is one Ollama A/B model with its 3-run table.
type abModel struct {
	Name  string
	Meta  string
	Table moeTable
}

// moeOllama is the Ollama A/B section.
type moeOllama struct {
	Setup   [][2]string
	Models  []abModel
	Charts  []moeChart
	Summary moeTable
}

// moeBench is everything the /benchmarks page renders.
type moeBench struct {
	Tiles   [][2]string
	Ollama  moeOllama
	Qwen    moeModel
	Glm     moeModel
	Sources []string
}

// barWidth renders v's share of max as a CSS width (e.g. "48.5%").
func barWidth(v, max float64) string {
	if max <= 0 {
		return "0%"
	}
	return strconv.FormatFloat(v/max*100, 'f', 1, 64) + "%"
}

func moeBenchData() moeBench {
	runHeaders := []string{"Run", "Prompt tokens", "Prompt tok/s", "Gen tokens", "Gen tok/s", "Total time", "Load"}
	genHeaders := []string{"Run", "Prompt tokens", "Prompt tok/s", "Gen tokens", "Gen tok/s", "Total"}

	ollama := moeOllama{
		Setup: [][2]string{
			{"GPU", "NVIDIA RTX 4090 (24.5 GB VRAM)"},
			{"Ollama", "0.40.7"},
			{"GPU overhead", "300 MB (OLLAMA_GPU_OVERHEAD=314572800)"},
			{"Flash attention", "Enabled"},
			{"Keep alive", "-1 (forever)"},
			{"Temperature", "0.7"},
			{"Prompt", "5,414 chars / 526 words (code review request, ~150 lines)"},
			{"Runs", "3 per model (run 1 = cold load, 2–3 = cached)"},
		},
		Models: []abModel{
			{
				Name: "jarvis-moe",
				Meta: "Mixture of Experts · qwen3:30b-a3b · Q4 · ~23 GB · ctx 49,152",
				Table: moeTable{
					Headers: runHeaders,
					Rows: [][]string{
						{"1 (cold)", "1,191", "4,818", "9,052", "211.9", "47.90s", "4,914ms"},
						{"2 (cached)", "1,191", "163,621", "10,261", "209.9", "49.28s", "1ms"},
						{"3 (cached)", "1,191", "160,016", "7,705", "214.4", "36.37s", "1ms"},
					},
					Avg:      []string{"Avg", "1,191", "109,485*", "9,006", "212.1", "44.52s", "—"},
					Footnote: "* Prompt avg skewed by cold load; cached runs ~162K tok/s.",
				},
			},
			{
				Name: "jarvis-brain",
				Meta: "Dense · qwen3.6:27b · Q4_K_M · ~17 GB · ctx 65,536",
				Table: moeTable{
					Headers: runHeaders,
					Rows: [][]string{
						{"1 (cold)", "1,299", "1,924", "5,581", "103.6", "58.75s", "4,205ms"},
						{"2 (cached)", "1,299", "12,075", "5,527", "102.0", "54.69s", "0ms"},
						{"3 (cached)", "1,299", "12,390", "6,245", "103.1", "61.03s", "0ms"},
					},
					Avg:      []string{"Avg", "1,299", "8,797*", "5,784", "102.9", "58.16s", "—"},
					Footnote: "* Prompt avg skewed by cold load; cached runs ~12.2K tok/s.",
				},
			},
		},
		Charts: []moeChart{
			{
				Title: "Generation throughput",
				Unit:  "tok/s",
				Bars: []moeBar{
					{Label: "jarvis-moe", Value: "212.1", Width: "100%", Series: 1},
					{Label: "jarvis-brain", Value: "102.9", Width: barWidth(102.9, 212.1), Series: 2},
				},
			},
			{
				Title: "Cached prompt throughput",
				Unit:  "tok/s",
				Bars: []moeBar{
					{Label: "jarvis-moe (cached)", Value: "~162,000", Width: "100%", Series: 1},
					{Label: "jarvis-brain (cached)", Value: "~12,200", Width: barWidth(12200, 162000), Series: 2},
				},
			},
		},
		Summary: moeTable{
			Headers: []string{"Metric", "jarvis-moe", "jarvis-brain", "Winner"},
			Rows: [][]string{
				{"Gen throughput", "212.1 tok/s", "102.9 tok/s", "moe 2.06x"},
				{"Prompt throughput (cached)", "~162K tok/s", "~12.2K tok/s", "moe 13.3x"},
				{"Avg gen tokens", "9,006", "5,784", "moe 1.56x"},
				{"Avg total time", "44.5s", "58.2s", "moe 1.31x"},
				{"Context window", "49,152", "65,536", "brain 33% more"},
				{"VRAM used", "~23.5 GB", "~17 GB", "brain uses less"},
				{"Model size", "~18 GB", "~17 GB", "similar"},
			},
		},
	}

	qwen := moeModel{
		Name: "Qwen 3.8 Flash Next",
		Meta: "MoE ~128B (Ollama tag 125b-mlx) · UD-IQ4_XS · ~88 GB, 3-part GGUF · FreeToken 0.2.1",
		Note: "ft serve on port 11436 with --moe-strategy hybrid --moe-cache-auto. The model is a 3-part GGUF and the FIRST shard (…-00001-of-00003.gguf) must be passed as the model path — the folder fails with \"no model_type in config.json\". The brain engine (compose jarvis-engine) was stopped for every run.",
		Attempt: &moeAttempt{
			Title: "2026-10-23 — Ollama pull attempt: failed",
			Body:  "The only Ollama tag is qwen3.8-flash-next:125b-mlx (105 GB, MLX — Apple Silicon only) and the model is cloud-only on Ollama, so the pull fails (no compatible tag for Linux/CUDA). VRAM analysis: ~64 GB at Q4 (128B MoE) vs the 24.5 GB GPU → it cannot run via Ollama. Benchmark skipped.",
		},
		Points: []moePoint{
			{
				ID:      "Q-1",
				Target:  "~100 MB free",
				Meta:    "run 2026-11-04 · r = 0.9825 · 137 MiB free after load",
				Outcome: "OOM",
				State:   "crit",
				MaxCtx:  "281 tokens",
				Note:    "The standard prompt (5,820 chars, 1,496 tok) OOMs the scheduler worker on first prefill (grouped_gemma_rmsnorm, tried 30 MiB) at every ratio in the 100 ± 40 MB band; a tiny 54-token prompt serves, and free VRAM falls ~30 MiB on the first request. Indicative run at r = 0.97 (390 MiB free — NOT this sweep point): prompt + first token 2.4 s, 256 gen tokens in 7.1 s → ~35 tok/s.",
			},
			{
				ID:      "Q-2",
				Target:  "~150 MB free",
				Meta:    "run 2026-11-04 · r = 0.982 · 150 MiB free after load",
				Outcome: "OOM",
				State:   "crit",
				MaxCtx:  "259 tokens",
				Note:    "The standard prompt OOMs on first prefill (tried 30 MiB, 108 MiB free); the largest prompt served is 259 tokens (900 chars). KV is not the limit (8,192 KV tokens allocated) — prefill activation headroom is.",
				Table: &moeTable{
					Caption: "Substitute 3-run table (259-token prompt, max_tokens 256, temp 0.7)",
					Headers: genHeaders,
					Rows: [][]string{
						{"1 (cold)", "259", "105.4", "256", "44.5", "8.21 s"},
						{"2", "259", "115.8", "256", "53.6", "7.01 s"},
						{"3", "259", "115.9", "256", "53.4", "7.02 s"},
					},
					Footnote: "Prompt tok/s is a lower bound: prompt tokens ÷ time of a max_tokens=1 call, so it includes first-token latency. Not comparable to the Ollama numbers (different model, much shorter prompt).",
				},
			},
			{
				ID:      "Q-3",
				Target:  "~200 MB free",
				Meta:    "not attempted",
				Outcome: "not run",
				State:   "warn",
				MaxCtx:  "—",
				Note:    "no report produced — the point left no file under docs/benchmarks/. Sibling points bracket it: the standard prompt fails below ~390 MB and serves once warmed at ~234 MB.",
			},
			{
				ID:      "Q-4",
				Target:  "~250 MB free",
				Meta:    "run 2026-11-10 · r = 0.9775 · ~234 MiB free after load",
				Outcome: "served",
				State:   "good",
				MaxCtx:  "~4,501 tokens (hard cap 8,256)",
				Note:    "A COLD server OOMs the scheduler worker on the standard prompt (reproduced twice); a warmed server (ramp 281 → 524 → 761 → 1,010 tok) serves it, so the table is warm — run 1 is not a true cold load. Hard cap regardless of VRAM: the server rejects prompts over 8,256 tokens (prompt + generation, default --kv-reserve-tokens KV budget) with HTTP 400 context_length_exceeded. Treat ~3–4.5K as the practical range — the headroom is prefill activation memory, not KV.",
				Table: &moeTable{
					Caption: "3-run table (standard prompt, warmed server, max_tokens 256, temp 0.7)",
					Headers: genHeaders,
					Rows: [][]string{
						{"1 (warm)", "1,496", "662.6", "256", "47.0", "7.71 s"},
						{"2", "1,496", "665.7", "256", "52.3", "7.14 s"},
						{"3", "1,496", "666.2", "256", "51.2", "7.25 s"},
					},
					Footnote: "Prompt tok/s is a lower bound (includes first-token latency); generation tok/s = 256 ÷ (total − prefill call).",
				},
			},
		},
	}

	glm := moeModel{
		Name:       "GLM 5.3 Flash",
		Meta:       "320B total / 18B active · arch glm5_next · UD-IQ2_XXS · ~101.8 GB, 4 shards · FreeToken ft serve, port 11436",
		Provenance: "Hugging Face unsloth/GLM-5.3-Flash-GGUF (base zai-org/GLM-5.3-Flash), revision a38483c8cd5df544f53d70fb281afe97369d5ab6. On disk at /home/agent/models/glm5.3-flash/UD-IQ2_XXS/ — the first shard is passed as --model. All four shard sizes match the HF API byte-for-byte.",
		Shards: &moeTable{
			Caption: "Shards (UD-IQ2_XXS)",
			Headers: []string{"Shard", "Bytes"},
			Rows: [][]string{
				{"GLM-5.3-Flash-UD-IQ2_XXS-00001-of-00004.gguf", "9,429,984"},
				{"GLM-5.3-Flash-UD-IQ2_XXS-00002-of-00004.gguf", "49,896,298,080"},
				{"GLM-5.3-Flash-UD-IQ2_XXS-00003-of-00004.gguf", "49,248,507,840"},
				{"GLM-5.3-Flash-UD-IQ2_XXS-00004-of-00004.gguf", "2,690,716,000"},
			},
			Avg: []string{"Total", "≈ 101.8 GB (94.8 GiB)"},
		},
		WhyQuant: "Why UD-IQ2_XXS: UD-IQ4_XS would be ~146 GiB (5 shards) — more than the 24 GiB VRAM + 122.7 GiB RAM can hold, so it cannot be weights-in-RAM with experts hot on the GPU. UD-IQ2_XXS (95 GiB) keeps ~70 GiB resident in RAM with ~23 GiB on the GPU. The Qwen points used a 4-bit quant, so GLM numbers are not bit-width-comparable.",
		BW: &moeBW{
			Ceilings: "Run with the brain up (3.7 GiB VRAM free): host ceilings CPU STREAM read 44.8 GB/s, PCIe linear H2D 26.8 / D2H 26.3 GB/s.",
			Table: moeTable{
				Headers: []string{"Expert format", "Expert size", "CPU-MoE", "PCIe-gather", "CPU/PCIe", "Backend", "Hybrid fetches"},
				Rows: [][]string{
					{"gate_up=IQ2_XXS, down=IQ3_XXS", "7.19 MB", "28.6 GB/s", "25.3 GB/s", "1.13x", "offload", "52.9% of misses"},
					{"gate_up=IQ2_S, down=IQ4_XS", "9.38 MB", "38.8 GB/s", "25.6 GB/s", "1.51x", "offload", "45.7% of misses"},
					{"gate_up=IQ2_XXS, down=IQ4_XS", "8.38 MB", "34.8 GB/s", "25.6 GB/s", "1.36x", "offload", "48.8% of misses"},
				},
			},
			Note: "All three CPU/PCIe ratios (1.13–1.51x) sit below the 2.0x threshold, so the backend resolves to offload for every format. Overlapped (CPU-MoE + PCIe) fetches would cover ~46–53% of misses (52.9% / 45.7% / 48.8%).",
			Groups: []bwGroup{
				{
					Name: "gate_up=IQ2_XXS, down=IQ3_XXS",
					Cpu:  moeBar{Value: "28.6", Width: barWidth(28.6, 38.8), Series: 1},
					Pci:  moeBar{Value: "25.3", Width: barWidth(25.3, 38.8), Series: 2},
				},
				{
					Name: "gate_up=IQ2_S, down=IQ4_XS",
					Cpu:  moeBar{Value: "38.8", Width: barWidth(38.8, 38.8), Series: 1},
					Pci:  moeBar{Value: "25.6", Width: barWidth(25.6, 38.8), Series: 2},
				},
				{
					Name: "gate_up=IQ2_XXS, down=IQ4_XS",
					Cpu:  moeBar{Value: "34.8", Width: barWidth(34.8, 38.8), Series: 1},
					Pci:  moeBar{Value: "25.6", Width: barWidth(25.6, 38.8), Series: 2},
				},
			},
		},
		Points: []moePoint{
			{
				ID:      "GLM-2",
				Target:  "~100 MB free",
				Meta:    "run 2026-11-10 · serving benchmark not run",
				Outcome: "not run",
				State:   "warn",
				MaxCtx:  "—",
				Note:    "The point recorded provenance and the ft bench bw bandwidth bench only; the benchmark section was left to be filled in.",
			},
			{
				ID:      "GLM-3",
				Target:  "~150 MB free",
				Meta:    "not run",
				Outcome: "not run",
				State:   "warn",
				MaxCtx:  "—",
				Note:    "Not run — no report produced.",
			},
			{
				ID:      "GLM-4",
				Target:  "~200 MB free",
				Meta:    "run 2026-11-10 · r = 0.975 · ~200 MiB free after load · moe_cache_size 1539",
				Outcome: "OOM",
				State:   "crit",
				MaxCtx:  "0 tokens",
				Note:    "NOTHING served — not even a 20-character prompt: the first-request Triton autotune of the KDA kernel needs a 256 MiB scratch allocation (202 MiB free) and the worker dies. --moe-cache-auto resolved moe_cache_size 1539 (3 pools, 8,640-token KV); the engine starts and answers /v1/models but cannot run a forward pass.",
			},
			{
				ID:      "GLM-5",
				Target:  "~250 MB free",
				Meta:    "run 2026-11-10 · r = 0.972 · 247–284 MiB free after load",
				Outcome: "OOM",
				State:   "crit",
				MaxCtx:  "0 tokens",
				Note:    "Five starts (r = 0.972 ×3, 0.971, 0.9715), nothing served: KDA autotune OOM at every start (256 MiB scratch); one start got past the autotune, then ggml_cuda_mul_mat_q OOM — 128 MiB needed, 134 MiB free. The engine needs >~300 MiB free at idle for the autotune to be reliable.",
			},
			{
				ID:      "GLM-6",
				Target:  "~300 MB free",
				Meta:    "run 2026-11-10 · r = 0.970 · 344 MiB free after load",
				Outcome: "OOM",
				State:   "crit",
				MaxCtx:  "0 tokens",
				Note:    "The 256 MiB autotune allocation failed with 317.31 MiB nominally free — once CUDA context and allocator fragmentation are counted the floor is above ~340 MiB. Nothing served.",
			},
			{
				ID:      "GLM-7",
				Target:  "~400 MB free",
				Meta:    "not run",
				Outcome: "not run",
				State:   "warn",
				MaxCtx:  "—",
				Note:    "Not run — the docs predict 400 MB as the first realistic working point.",
			},
		},
	}

	return moeBench{
		Tiles: [][2]string{
			{"2.06×", "gen throughput — MoE vs dense brain"},
			{"~13.3×", "cached prompt speed — MoE vs brain"},
			{"2 · 7", "MoE sweeps · points attempted"},
			{"2", "sweep points produced 3-run tables"},
		},
		Ollama: ollama,
		Qwen:   qwen,
		Glm:    glm,
		Sources: []string{
			"docs/moe-benchmark.md",
			"docs/benchmarks/qwen38-flash-next-results.md",
			"docs/benchmarks/qwen38-flash-next-vram-sweep-100mb.md",
			"docs/benchmarks/qwen38-flash-next-vram-sweep-150mb.md",
			"docs/benchmarks/qwen38-flash-next-vram-sweep-250mb.md",
			"docs/benchmarks/glm53-flash-vram-sweep-100mb.md",
			"docs/benchmarks/glm53-flash-vram-sweep-200mb.md",
			"docs/benchmarks/glm53-flash-vram-sweep-250mb.md",
			"docs/benchmarks/glm53-flash-vram-sweep-300mb.md",
			"docs/benchmarks/moe-models-to-benchmark.md",
		},
	}
}

// handleMoeBenchPage serves GET /benchmarks: the full MoE benchmark report,
// rendered from fixed data (no I/O) into the "moebench" template.
func (s *Server) handleMoeBenchPage(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "moebench", moeBenchData()); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}
