# FreeToken runner — install and benchmark invocations

FreeToken (FlashML) — local MoE-offload LLM inference runtime with
OpenAI/Anthropic-compatible APIs. Installed 2026-10-27 (job
chat-1793040169-43b4). Qwen3.8-Flash-Next is a known-good family in
FreeToken (see `/home/agent/freetoken/docs/models.md`).

## Install layout

| What | Where |
|---|---|
| Source clone | `/home/agent/freetoken` — github.com/FlashML-org/FreeToken, release 0.2.1 (commit `ba31db2`), clean tree |
| Benchmark venv (0.2.1, editable) | `/home/agent/ftvenv` — `pip install -e /home/agent/freetoken`; CLI at `/home/agent/ftvenv/bin/ft` |
| Legacy venv (0.1.1, wheel) | `/opt/freetoken-venv` — still referenced by `jarvis-moe-engine.service` (unit stopped as of 10-26; re-point to `ftvenv` before re-arming) |
| Model | `/home/agent/models/qwen3.8-flash-next/UD-IQ4_XS/` — 3-part split GGUF, ~100 GB (shard 3 updated 10-26) |
| Kernel cache | `~/.cache/tvm-ffi/freetoken__*` — built by prior runs |
| bench-bw profiles | `~/.cache/freetoken/benchbw/<gpu-uuid>.json` (written on first `ft bench bw`) |

CLI subcommands: `serve`, `shell`, `ctl`, `daemon`, `launch`,
`checkpoint`, `bench`. Full flag reference:
`/home/agent/freetoken/docs/cli.md`.

## Verified (2026-10-27)

- `/home/agent/ftvenv/bin/ft --version` → `freetoken version 0.2.1`
- `/home/agent/ftvenv/bin/ft --help` → clean subcommand listing
- `import freetoken` from a neutral cwd resolves to
  `/home/agent/freetoken/python/freetoken/__init__.py` (editable install
  re-pointed when the clone moved out of `/home/agent/tmp/`)

No benchmark has been run yet, per the job instruction.

## Exact invocations for the benchmark round

FreeToken's built-in benchmark is the CPU-vs-PCIe bandwidth calibration
that `--moe-strategy hybrid` uses to size expert-miss fetches:

```bash
# per-checkpoint detail (every MoE layer kind of our GGUF)
/home/agent/ftvenv/bin/ft bench bw --model /home/agent/models/qwen3.8-flash-next/UD-IQ4_XS/Qwen3.8-Flash-Next-UD-IQ4_XS-00001-of-00003.gguf

# canonical geometry per expert format (no model needed)
/home/agent/ftvenv/bin/ft bench bw
```

Profile lands in `~/.cache/freetoken/benchbw/<gpu-uuid>.json`; re-runs
replace only the rows benched. Note: the model is a 3-part split GGUF —
pass the first shard (or the folder) and confirm resolution on first run.

The queued VRAM-spare benchmark (job chat-1792976974-392b: TPS at 100/150/
200/250/300 MB of free VRAM kept) is a `ft serve` sweep, e.g.:

```bash
/home/agent/ftvenv/bin/ft serve \
  --model /home/agent/models/qwen3.8-flash-next/UD-IQ4_XS \
  --port 11435 --moe-strategy hybrid --moe-cache-auto \
  --memory-ratio <0.99-ish, so free VRAM lands at the target MB>
```

Relevant flags (`docs/cli.md`): `--memory-ratio` (fraction of free VRAM
the engine may use), `--moe-cache-mem` / `--moe-cache-size` /
`--moe-cache-rate` / `--moe-cache-auto` (expert-cache sizing),
`--kv-reserve-tokens` (KV floor before auto-fill), `--num-tokens`
(KV capacity override).

## Caveats

- Do not start `ft serve` while the main engine (`llama-server`, ~18.5 GB
  of the 24 GB VRAM) is running — the card OOMs. Stop `jarvis-engine`
  first; that breaks local agent runs, so schedule the sweep accordingly.
- `jarvis-moe-engine.service` (port 11435, `Qwen/Qwen3.8-Flash-Next` from
  HF) pins the old 0.1.1 venv — update `ExecStart` to
  `/home/agent/ftvenv/bin/python3` (or re-create the /opt venv at 0.2.1)
  before re-enabling it.
