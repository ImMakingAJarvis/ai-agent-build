# render-deep-dive — Paid GPU sharing is a no-go on this box; BOINC is the go

Verified against live sites/APIs 2026-11-19 (Salad site, Vast.ai public
API, Golem GitHub/DNS). Updated `docs/render-network.md` with the verdicts;
this note is the evidence trail.

## What I found

**The binding constraint is not pay rate — it's the brain.** The 27B Q4_K_M
brain holds ~19.6 GB of the 4090's 24 GB 24/7 and serves live conversation.
Only ~4 GB VRAM and a latency budget remain. Every paid network I checked
either needs the whole GPU, or a dedicated machine, or another OS:

- **Salad — Windows-only (disqualifier).** `salad.com/download` states
  "Salad doesn't run on Mac, Linux, or Chromebook"; the support portal lists
  Windows 10/11, RTX 3060+, 32 GB RAM, and says "other graphics-card
  activity pauses jobs, so the PC should be left idle." Notably, payouts
  have changed: "Salad Balance" redeemable to **PayPal**/gift cards — the
  old crypto-wallet objection is gone, the OS objection is not. (Salad
  Cloud's "$0.015/GPU-hr" is the customer price, not the Chef payout; no
  per-GPU Chef rate is published — they show a demand-based projection.)
- **Vast.ai — whole GPU + dedicated machine.** Live `console.vast.ai`
  bundles sample (64 offers, 2026-11-19): single-4090 boxes ask
  **$0.11–0.54/hr, median ≈$0.35** (US single-GPU asks ≈$0.38–0.40; cheap
  ones are 2/4-GPU CN boxes). $0.35/hr × 730h ≈ $256/mo gross at 100%
  uptime; a lone 4090 host realistically captures a fraction (single-GPU
  residential hosts lose to datacenter and multi-GPU listings). Vast
  tenants get exclusive CUDA; co-running the brain's 19.6 GB is not a
  thing, and the worker expects the machine dedicated to it.
- **Golem — dead.** `golemfactory/golem` last pushed **2021-03** (GitHub
  API), `forum.golem.network` no longer resolves (DNS ENOTFOUND), and the
  token was rebranded GNT→GLM with no live activity metrics on the site.
  Don't spend setup time here.
- **BOINC — works, pays nothing, needs no wallet.** Volunteer science
  (SETI/Asteroid/LHC etc.). Coexistence is configuration: cap share
  (`max_ncpus`, `max_gpu_usage` in `cc_config`), and gate CUDA tasks to
  brain-idle windows with `boinccmd` — the same "is the GPU free" signal
  the photo swap window uses. CPU tasks can run at a low share
  continuously (note: host load was ~34 on 8 cores this round, so keep the
  CPU share small).
- **Also ruled out:** RNDR/Flux, io.net/Aethir/Akash — all assume
  dedicated exclusive-GPU nodes with stable public IP. Mining (Kryptex,
  solo) — a 4090 grossed roughly $1–2.50/day in 2026 vs ≈$1/day of
  *marginal* power just to break even, and it wants the whole GPU plus a
  crypto payout.

**Power math.** Idle box ≈0.13 kW (GPU 58–71 W with brain resident,
`docs/gpu-power-measurements.md`, + system); render ≈0.45–0.5 kW.
Marginal ≈0.35 kW ≈ 8–9 kWh/day ≈ **≈$1/day at $0.13/kWh** → a paid
option needs ~$30/mo net just to tie the extra electricity, before any
convenience value. (Exact modeling is the `power-economics` backlog job.)

## Options / next steps (Nathan's call)

1. **GO now: BOINC as the always-on floor.** One setup session: install
   client, low CPU share, CUDA gated to brain-idle windows, a systemd unit
   that logs its own kWh. Zero risk, zero crypto, answers "wasting power"
   with utility. This is my recommendation.
2. **Second used GPU (3090/4090 class, ~$800–1500) dedicated to a paid
   network** — the brain stays untouched on GPU 0; the new GPU runs Salad
   (needs a small Windows box anyway, so pair it with option 3) or a Vast
   instance on GPU 1. Bonus: ~12 GB of extra VRAM for photo generation.
   The `jarvis-engine-igpu` container proves a second accelerator lives
   happily on this box.
3. **Cheap dedicated mini-box** on the LAN (Windows, RTX 3060–4090) for
   Salad's PayPal payouts — no wallet, no risk to the brain, and it
   earns even while this box is in a conversation.
4. **Revisit Vast on GPU 1 or the mini-box** once either exists — it has
   the highest ceiling of the verified options (≈$250/mo gross at full
   uptime on a 4090) but also the most integration (Docker worker,
   reliability score, public reachability).
5. **Skip** Golem, RNDR/Flux, io.net/Aethir/Akash, mining — verified
   no-gos above.

## Recommendation

Connect **BOINC only, this box, gated to idle gaps** (option 1) — it is
the only network whose shape matches a 24/7 brain, and it needs no wallet
and no new hardware. Treat "making money" as a **hardware decision**
(options 2/3), not a network decision: none of the paid networks fits a
single 4090 already committed to the brain, and the ~$1/day marginal
power cost means the first paid setup must clear ~$30/mo net to matter.
If Nathan wants to pursue it, the mini-box (option 3) is the lowest-risk
first paid step because Salad now pays out to PayPal.

## Sources

- https://salad.com/ , https://salad.com/download/ , https://support.salad.com/
- https://console.vast.ai/api/v0/bundles/ (live offer sample, 2026-11-19)
- https://api.github.com/repos/golemfactory/golem (pushed_at 2021-03-09)
- https://golem.network (site; forum.golem.network → NXDOMAIN)
- https://boinc.berkeley.edu (client manual / options)
- `docs/gpu-power-measurements.md`, `docs/brain-swap.md` (local power +
  window machinery)
