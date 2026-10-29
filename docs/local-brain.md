# Local brain (stage 2)

## What was installed
- **Ollama 0.40.7** via `scripts/install_ollama.sh` (systemd service `ollama`, enabled at boot, listening on `localhost:11434`; Anthropic API at `/v1/messages` needs >= 0.14.0).
- systemd drop-in `/etc/systemd/system/ollama.service.d/jarvis.conf`: `OLLAMA_KEEP_ALIVE=-1` (models stay loaded forever; the idle unloader comes in Phase 3) and `OLLAMA_FLASH_ATTENTION=1`.
- **llama.cpp server** (`jarvis-engine` systemd service) runs the brain model directly from a GGUF file at `/opt/jarvis-engine/models/jarvis-brain.gguf`. Service defined in `systemd/jarvis-engine.service` with `--ctx-size 131072`, `--parallel 2`, `--kv-unified`, and q4_0 cache types.
- Model `jarvis-brain`, created from `models/brain.Modelfile` (`FROM qwen3.8:27b`, `num_ctx 131072`, `num_gpu 999`).
- `~/.config/jarvis/local.env`:
  ```
  LOCAL_MODEL=jarvis-brain
  LOCAL_CODER_MODEL=
  ```
- `scripts/agent-local` now sets `CLAUDE_CODE_MAX_CONTEXT_TOKENS=131072` (override with `LOCAL_CONTEXT_TOKENS` in local.env). Without it Claude Code assumes a 200k window for the unknown model name and would not compact before overflowing. It also resolves its own path for `check`. The copy in `/usr/local/bin` was refreshed from the repo.

## Models and why
- **Brain: qwen3.8:27b** (dense 27B, Q4_K_M, ~16.5 GB, vision/tools/thinking, 256K native). Runs at Q4_K_M with a 128K context (~20.4 GB VRAM; 256K halves decode speed, see docs/benchmarks/qwen38-27b-q4-128k-256k.md). The GGUF is `Qwen3.8-27B-UD-Q4_K_M.gguf` from `unsloth/Qwen3.8-27B-GGUF`. The earlier Q6_K build is kept as `jarvis-brain.gguf.bak.q6`; the older Qwen 3.6 Q4 as `jarvis-brain.gguf.bak.q4`.
- **Coder recommendation: no separate coding model for now.** qwen3.8:27b is marketed for agentic coding, is already loaded, and a second model would need a 24 GB GPU swap (or a CPU-offloaded 80B-class model, `qwen3-coder-next`, which is much slower and is 8 months old). Start with the brain doing the coding (`LOCAL_CODER_MODEL=` empty). Revisit `qwen3-coder-next` only if the brain's code quality or speed proves insufficient in stage 3; then pull it after you agree.

## Context and VRAM
- The engine loads `models/qwen3.8-chat-template.jinja` (installed at `/opt/jarvis-engine/templates/`) via `--chat-template-file`. Claude Code compatibility: non-leading system messages render in place; jinja `reasoning_effort` values the template does not know (`unknown`, `max`, `auto`) clamp to `xhigh` instead of HTTP 500. Per-job effort is `off|low|medium|high` (`jarvis-jobs add|run -effort`, `enqueue --effort`). Coding defaults to medium; voice/chat send `chat_template_kwargs.enable_thinking=false`. `agent-local` maps that onto Claude Code `--effort` (never `--reasoning-effort`, which Claude 2.1 rejects) and `CLAUDE_CODE_EFFORT_LEVEL`; `off` sets `CLAUDE_CODE_DISABLE_THINKING=1`. `high`/`max` become jinja `xhigh`.
- Context 131072 tokens (128K). `--ctx-size 131072` on the llama.cpp server.
- `nvidia-smi`: ~18.5 GB of 24.5 GB used (about 6 GB headroom), decode ~48 tok/s (Q6_K was ~17 tok/s). Nothing else big should use the GPU while the brain is loaded.
- Thinking is on by default for this model; that can be slow but helps decisions.

## Running it yourself
```
agent-local check              # proves tool use; exit 0 = ok
agent-local -p "your prompt"   # Claude Code on the local model
agent-local                    # interactive
```

## The igpu driver (rounds on the small iGPU model)

`agent-run` can run a stage-3 round on the 3B helper on the Ryzen iGPU
(`jarvis-engine-igpu`, port 11435, see docs/igpu-model-handler.md)
instead of the 27B brain or the API. Pick **"IGPU model"** in the round
checkpoint, or run a job directly:

```
jarvis-jobs run <job-id> -driver igpu
```

It is the right choice for rounds that must keep the 4090 free (e.g. the
MoE VRAM sweeps: the round runs on the iGPU while the sweep owns the
card) or that should not cost API. The brain may be stopped while an
igpu round runs — that is expected.

Mechanics: the driver runs the same `agent-local` path as the local
driver, with `ENGINE_URL` pointed at `http://localhost:11435`,
`LOCAL_MODEL=jarvis-igpu`, `LOCAL_CONTEXT_TOKENS=32768` (the engine's
`--ctx-size`) and `CLAUDE_CODE_MAX_OUTPUT_TOKENS=16384` (a single
response must never overflow the 32K window). `agent-local` honours
pre-set values over `~/.config/jarvis/local.env`. Overrides:
`IGPU_MODEL` (model name), `IGPU_URL` (engine URL — e.g. to point at a
bigger deep-helper engine later), `IGPU_CONTEXT_TOKENS` (keep in sync
with the unit's `--ctx-size`).

Expectations for the small model: mechanical jobs work well
(`jarvis-helper --status` for its health); deep design work should
hand off to the local model or Sonnet. The iGPU has only 512 MB of
VRAM — weights and KV cache live in shared system RAM, so context size
costs RAM, not VRAM.

## Undo
```
sudo systemctl disable --now ollama
sudo rm -rf /etc/systemd/system/ollama.service.d
ollama rm jarvis-brain qwen3.6:27b     # before disabling, or delete /usr/share/ollama/.ollama/models
rm ~/.config/jarvis/local.env
sudo rm /usr/local/bin/ollama /etc/systemd/system/ollama.service   # full uninstall
```
