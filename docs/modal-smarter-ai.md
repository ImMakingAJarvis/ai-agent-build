# Modal as a "bigger brain" — investigation + plan (2026-11-21)

Job: chat-1795264274-41cb. Nathan asked for an investigation and a plan for
using [Modal](https://modal.com) and its free $30/month tier to bring a
smarter, bigger AI into this system. **Nothing is scheduled** — this doc is
the decision input. All Modal facts below were checked against modal.com on
2026-11-21 (sources at the end).

## TL;DR

Modal is a serverless GPU platform: you deploy a container (e.g. our
llama.cpp build + a 70B-class GGUF) and it gets a public HTTPS URL, scales to
zero when idle, and bills per second of GPU use. New workspaces get **$30 of
compute free every month** (recurring, not one-time), with a workspace
**budget cap** as a hard stop. At the A100-80GB rate ($2.50/hr) that is
**~12 hours/month of a much bigger model** for $0. Integration reuses the
whole existing round machinery: a new `modal` driver in `agentjob` that runs
the same `agent-local`/Claude Code harness pointed at the Modal endpoint —
the same shape as today's `igpu` driver. The 4090, the swap window, games
and photos are untouched.

## 1. What the system has today (model side)

| Driver | Model | Where | Notes |
|---|---|---|---|
| `local` (default) | Qwen3.8-27B Q4_K_M (27.3B) | 4090, llama.cpp `:11434`, 128K ctx | the brain; everything else shares this GPU |
| `igpu` | Qwen2.5-3B Q4_K_M | Ryzen iGPU, `:11435`, 32K ctx | small always-on helper |
| `sonnet` / `opus` | Anthropic frontier | API (opt-in, paid per token) | the existing "cloud is opt-in" path |

A job's `driver:` field picks the model (`internal/agentjob/run.go`,
`store.go:397`). Local/igpu rounds run the `claude` binary with
`ANTHROPIC_BASE_URL` pointed at the llama.cpp engine's Anthropic-compatible
endpoint (`scripts/agent-local`); sonnet/opus run the same harness on the
Anthropic API. The 4090 holds the 27B at ~18–19 GB of 24.5 GB — there is no
room for anything bigger locally, which is the gap this addresses.

## 2. What Modal is (verified 2026-11-21)

- Serverless containers with GPU; Python SDK; `modal deploy` uploads your
  image + a small app file; the app exposes a **public web endpoint**
  (HTTPS URL). Container scales to **zero when idle — no idle cost**.
- **Free tier (Starter): $30/month of compute (GPU/CPU/RAM), recurring
  monthly**, no pre-purchase. Overage is billed, so a **workspace budget
  cap** is the hard-stop mechanism. (Whether a payment method is required
  just to *use* the free allotment is not documented — verify at sign-up.)
- GPU $/hr (from per-second rates): T4 $0.59 · L4 $0.80 · A10 $1.10 ·
  L40S $1.95 · **A100-80GB $2.50** · RTX PRO 6000 (96 GB) $3.03 ·
  H100 $3.95 · B200 $6.25.
- CPU $0.047/core/hr; RAM $0.008/GiB/hr.
- Storage (Volumes, for model weights): **1 TiB/month free**, then
  $0.09/GiB/mo. Egress: **1 TiB/month free**, then $0.04/GiB.
- Limits (Starter): 100 containers, 10 concurrent GPUs, 200 apps.
- Function timeout: default 5 min, **max 24 h** (`timeout=86400`) — plenty
  for a long agentic round; warm retention is opt-in (`min_containers`),
  we would leave it at 0 (pay only while a round runs).

## 3. The math: what $30/month buys

| GPU | $/hr | $30 buys | Fits (Q4_K_M) |
|---|---|---|---|
| A100-80GB | $2.50 | **~12 h** | 70B dense (~40 GB) comfortably, 32K–64K ctx |
| RTX PRO 6000 96GB | $3.03 | ~10 h | 70B + bigger ctx, or 120B-class |
| H100 | $3.95 | ~7.6 h | faster 70B; H200/B200 could host 235B-MoE-class |
| L40S | $1.95 | ~15 h | 32B-class (48 GB) |

Usage pattern: a Modal round bills for the wall-clock time the server runs
(including tool-call gaps), plus the cold start. Estimate per heavy round:
cold start 1–4 min (container boot + 20–40 GB weight load from the Volume +
engine init — **to be measured in the spike**, not assumed) + 20–60 min of
work → roughly **12–24 heavy rounds/month on the free allotment**.
Idle months cost exactly $0.

## 4. Proposed architecture (if approved)

1. **`modal/` app** (small, in-repo): Python Modal app whose image is the
   same CUDA + llama.cpp **b11815** build we already pin
   (`scripts/install_engine.sh` / `docker/engine`), serving a 70B-class
   GGUF with the same OpenAI + Anthropic-compatible endpoints. Weights live
   in a **Modal Volume** (downloaded once; within the free 1 TiB).
   `timeout=7200+`, no warm containers.
2. **New `modal` driver** in `internal/agentjob` (`run.go`, `store.go`):
   identical in shape to `igpu` — `agent-local` with pre-set
   `ENGINE_URL=https://<app>.<...modal...>` and `LOCAL_MODEL=<model>`,
   plus a `stage3-modal.txt` brief. Job specs gain `driver: modal`.
3. **Secrets**: `MODAL_TOKEN_ID`/`MODAL_SECRET` in the dashboard Secrets
   page (`jarvis-secrets`), never in the repo.
4. **Spec + map**: `specs/modal.md` in the same PR as the driver (spec
   enforcement is real — `go test ./specs/` fails without it).
5. **Cost guard**: workspace budget cap = $30 from day one; `jarvis-stack-status`
   gains a Modal line (spend this month, endpoint health) so a runaway round
   is visible.

What does **not** change: the 4090, the brain, the swap window, photo jobs,
games, voice. `local` stays the default; `modal` is opt-in per job, exactly
like sonnet/opus today.

## 5. Pros

1. **A bigger model for the hard rounds, $0/month (within 12 h).** 70B-class
   (or better on RTX PRO 6000/H100) as an opt-in driver, recurring free
   allotment — no capex, no power bill, no second box to babysit.
2. **Zero contention with the 4090.** No swap-window interaction at all:
   photos, games and the brain keep the GPU 100% of the time they need it.
3. **Zero ops.** Serverless, scales to zero, no new compose service,
   systemd unit, watchdog target or install script to maintain.
4. **Tiny integration surface.** Same Claude Code harness, same briefs,
   same jarvis-* tools, same round loop; the diff is one driver case
   (the `igpu` driver is the template) + one small Modal app file.
5. **Pay-per-second, hard-capped.** Per-second billing, $0 idle, and a
   workspace budget cap makes the financial exposure a known constant.
6. **Spillover value.** The same app pattern could later host capability
   work that today needs the brain's swap window (e.g. `image.generate`
   "needs brain out") — photos without ever stopping the brain.
7. **Free storage/egress headroom.** 1 TiB Volume + 1 TiB egress/month
   covers the one-time model download and all round traffic.

## 6. Cons / risks (with mitigations)

1. **Cold start 1–4 min per round.** Container boot + weight load before
   the first token. Fine for batch rounds; **not** usable for voice or
   interactive chat. (Mitigation: it is a batch driver by design; measure
   real numbers in the spike.)
2. **Small monthly budget.** ~12 h on A100 ≈ 12–24 heavy rounds. If the
   value proves high, continued use is paid (~$2.50/hr) — an explicit
   decision point, not an automatic upgrade.
3. **Data flows through a third-party cloud.** Round prompts (job text,
   notes, conversation notes), files the agent reads, and tool output go
   to Modal's infrastructure; `modal deploy` also uploads the Docker build
   context. The repo itself is public, so code exposure is low — but the
   notes and job content are private. (Mitigation: keep secret-bearing and
   personal-content jobs off `driver: modal`; secrets page stays
   write-only and is never in a prompt.)
4. **Overage if the cap is forgotten.** A runaway job loop burns $30 in
   ~12 h of A100. (Mitigation: budget cap set at sign-up, before the first
   deploy; `jarvis-stack-status` shows monthly spend.)
5. **Free-tier GPU availability/region unknown.** Which GPUs are
   guaranteed on the free allotment is not documented; H100-class can be
   scarce. (Mitigation: spike pins the actually-available GPU; A100-80GB
   is the planning assumption.)
6. **Terms can change.** The $30/month is a current marketing term, not an
   SLA — it can shrink or vanish. (Mitigation: `modal` is an opt-in driver;
   if it dies, jobs requeue onto `local`/`sonnet` exactly like a down
   engine. No part of the system depends on it.)
7. **Redundancy question — we already have sonnet/opus.** The honest
   comparison: frontier API models are *smarter* than a 70B open-weights
   model and already wired in. Modal's unique value is **free GPU hours**
   and a local-model-class privacy story (open weights, your prompts —
   still via Modal, but no per-token frontier API). It complements, not
   replaces, the opt-in cloud drivers.
8. **New account/CLI surface.** A Modal account, CLI login on the box, two
   secrets, a dashboard to glance at. Small, but real.
9. **Mid-round failure = requeue.** If the Modal container dies (OOM,
   eviction) a round fails and requeues — same as any engine failure;
   needs the same "check first that it is not already done" hygiene.

## 7. Alternatives considered

- **Use more sonnet/opus (status quo).** No new infrastructure, strictly
  smarter, but paid per token from request one — no free tier. Modal is
  complementary.
- **Bigger local GPU / second box.** Capex + power + ops for what the free
  allotment already demos; revisit only if the paid Modal rate disappoints.
- **70B on the 128 GB RAM (CPU llama.cpp).** Runs at single-digit tok/s —
  unusable for agentic rounds.
- **Other serverless (RunPod, Baseten, Replicate, …).** Same shape; Modal
  was the ask, its per-second billing + free monthly allotment + Volumes
  fit best. The plan is provider-swappable by design (one driver, one app).

## 8. Phased plan — NOT scheduled (per Nathan: decide first)

- **P0 — Nathan decides** (§9). No work starts before this.
- **P1 — Spike (~half a day, one round).** Modal account + app + one
  70B-class GGUF on the cheapest GPU that fits. Measure: cold-start time,
  tok/s, 128K-ctx fit, cost of a real `agent-local check` round, free-tier
  GPU availability, payment-method requirement. Output: numbers for §3/§6.
- **P2 — Driver integration (one round).** `modal` driver + stage3 brief +
  secrets + `specs/modal.md` + `jarvis-stack-status` line + budget cap.
  Acceptance: a job with `driver: modal` runs end-to-end from the queue,
  verified live (the owner's path), and a failed Modal call requeues
  cleanly.
- **P3 — Optional.** Capability workers on Modal (swap-window-free photo
  generation first). Only after P2 proves the pattern.
- **P4 — Review gate (monthly).** Keep free / pay for more / retire. The
  token ledger + Modal dashboard give the spend data for the call.

## 9. Decision points for Nathan

1. **Go / no-go** on the spike (P1).
2. **Account + payment method**: sign up on his account? Add a card to
   enable the budget cap, or no payment method and accept a hard stop when
   the allotment runs out?
3. **Model choice for the spike**: 70B dense (Llama-3.3-70B-class) vs a
   30B-class MoE (faster, cheaper, maybe "good enough") — or let the spike
   benchmark both and pick.
4. **Privacy line**: which job classes may ever run on `driver: modal`
   (default proposal: no secrets, no personal notes).
5. **Spend policy**: cap at exactly $30 (pure free tier) or a small
   overage allowance (e.g. $50) if the value proves out?

---
Sources (checked 2026-11-21): [modal.com/pricing](https://modal.com/pricing)
(free allotment, per-second GPU/CPU/RAM rates, Volume/egress pricing,
Starter limits, budget caps), [modal.com/docs/guide/functions](https://modal.com/docs/guide/functions)
(function timeout 5 min default / 24 h max, scale-to-zero).
