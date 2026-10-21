# Local brain (stage 2)

## What was installed
- **Ollama 0.40.7** via `scripts/install_ollama.sh` (systemd service `ollama`, enabled at boot, listening on `localhost:11434`; Anthropic API at `/v1/messages` needs >= 0.14.0).
- systemd drop-in `/etc/systemd/system/ollama.service.d/jarvis.conf`: `OLLAMA_KEEP_ALIVE=-1` (models stay loaded forever; the idle unloader comes in Phase 3) and `OLLAMA_FLASH_ATTENTION=1`.
- Model `jarvis-brain`, created from `models/brain.Modelfile` (`FROM qwen3.6:27b`, `num_ctx 65536`, `num_gpu 999`).
- `~/.config/jarvis/local.env`:
  ```
  LOCAL_MODEL=jarvis-brain
  LOCAL_CODER_MODEL=
  ```
- `scripts/agent-local` now sets `CLAUDE_CODE_MAX_CONTEXT_TOKENS=65536` (override with `LOCAL_CONTEXT_TOKENS` in local.env). Without it Claude Code assumes a 200k window for the unknown model name and would not compact before overflowing the 64K. It also resolves its own path for `check`. The copy in `/usr/local/bin` was refreshed from the repo.

## Models and why
- **Brain: qwen3.6:27b** (dense 27B, Q4_K_M, ~17 GB, vision/tools/thinking, 256K native). It is the newest Qwen in the Ollama library, supports tools, and passes `agent-local check` (Bash tool call through Claude Code). Alternatives seen in the library but not tried: qwen3.6:35b-a3b (24 GB, too big with context), glm-4.7-flash, nemotron-3.5-lightning, devstral-small-2.
- **Coder recommendation: no separate coding model for now.** qwen3.6:27b is marketed for agentic coding, is already loaded, and a second model would need a 24 GB GPU swap (or a CPU-offloaded 80B-class model, `qwen3-coder-next`, which is much slower and is 8 months old). Start with the brain doing the coding (`LOCAL_CODER_MODEL=` empty). Revisit `qwen3-coder-next` only if the brain's code quality or speed proves insufficient in stage 3; then pull it after you agree.

## Context and VRAM
- Context 65536 tokens. `ollama ps`: 17 GB, 100% GPU, context 65536, until Forever.
- `nvidia-smi`: ~22.7 GB of 24.5 GB used (about 1.8 GB headroom). Nothing else big should use the GPU while the brain is loaded.
- Thinking is on by default for this model; that can be slow but helps decisions.

## Running it yourself
```
agent-local check              # proves tool use; exit 0 = ok
agent-local -p "your prompt"   # Claude Code on the local model
agent-local                    # interactive
```

## Undo
```
sudo systemctl disable --now ollama
sudo rm -rf /etc/systemd/system/ollama.service.d
ollama rm jarvis-brain qwen3.6:27b     # before disabling, or delete /usr/share/ollama/.ollama/models
rm ~/.config/jarvis/local.env
sudo rm /usr/local/bin/ollama /etc/systemd/system/ollama.service   # full uninstall
```
