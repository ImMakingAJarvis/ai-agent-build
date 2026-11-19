# Render network — earning the power draw

Nathan's ask (voice, 2026-11-18): "we should still work at getting render
going, getting connected to the render network so that you can actually make
me money instead of just wasting power." This is the decision document. The
`render-deep-dive` think job (first entry in `config/think-backlog.md`)
refreshes the numbers below with fresh context before anything is connected.

## The constraint that shapes everything

This is **not** a free box. A 27B Q4_K_M brain (llama.cpp, port 11434) holds
~18.5 GB of the 4090's 24.5 GB VRAM at idle, and it serves the live
conversation — it can never be stopped on demand. Two consequences:

1. **A network worker can only use the ~6 GB the brain leaves free** —
   enough for small models, small batches, CPU-side tasks. Anything that
   needs 12+ GB (photo generation, big inference) already has its own
   swap-window machinery (`docs/brain-swap.md`); a render worker would have
   to plug into the same bracket, which most network workers do not support.
2. **Latency is sacred while the brain is in a conversation** — the render
   worker must yield to it or the voice/chat latency dies. On one GPU that
   means either strict time-slicing or, more honestly, *don't run render
   during conversation hours*.

So the realistic posture is: **paid idle in the gaps** (nights, no
conversation), with the same swap-window discipline the photo system uses.
A network whose worker assumes exclusive GPU ownership all day is a bad fit
regardless of its pay rate.

## Options (numbers to verify — the deep-dive job owns freshness)

| Network | Model | Fit for a 24/7 Jarvis host | Notes |
|---|---|---|---|
| **Salad** | Run "apps" (games, small GPU apps) on your GPU; pays in BTC/credits | **Best first fit** — apps are small (fits the ~6 GB gap), short jobs, soft pause on disconnect | Account + wallet required. Earnings for a single 4090 are modest (tens of $/month class — verify). App availability varies a lot. |
| **Vast.ai** | *Sell* the GPU as a hosted instance | Possible, but you are the host: the instance must be reachable (port/TLS), and Vast's typical workloads are large-model training/inference that want the **whole** GPU — conflicts with the brain. Fits only inside a swap window, which is not how Vast workers are built. | Higher revenue ceiling, much more integration work. Defer until the swap-window controller can arbitrate a "no brain" window on demand. |
| **Golem** | Decentralized compute market, task-based | CPU+GPU tasks exist, but payout is small and the network has a trust/uptime reputation system; a box that is also a dev host is a mismatch. | Low priority. |
| **BOINC** | Volunteer compute (science) | **Good citizen fit**: pauses/resumes per task, runs in the background, no wallet. Pays nothing — it answers the "wasting power" objection with utility instead of money, and it can run in the 6 GB gap or on CPU. | Zero revenue, zero crypto, trivially safe. A fine default while the paid options are being decided. |
| **RNDR / Flux** | Render-node marketplace | Aimed at dedicated render nodes with exclusive GPUs and high network throughput. Wrong shape for this box today. | Skip for now. |

## What "connected" actually looks like (per option)

- **Wallet/crypto** — Salad (and most paid networks) need a crypto wallet
  for payouts. Whether that is acceptable is **Nathan's call**, not the
  agent's: it is his machine, his money, his tax record.
- **Exposure** — a render worker is a service listening on the network.
  This box is also the house's AI; a render daemon should run as a
  dedicated unit (or container), with the same care as any other service,
  and it must never touch the brain's ports or the agent store.
- **Yield rule** — the worker runs only when (a) no agent job is
  running/waiting and (b) the brain is idle, i.e. it is another consumer
  of the same "is the GPU free" signal the photo window uses. Implementation
  sketch: a small systemd unit gated by a check script (queue depth == 0,
  no swap window open, no active conversation per the voice service state),
  started/stopped by a timer or by the window controller.
- **Power accounting** — the box already measures per-job kWh
  (`docs/agent-jobs.md`, `gpu-power-measurements.md`). Render hours should
  get the same treatment (a unit that logs its own draw) so "money made
  vs power spent" is answerable — that is exactly the `power-economics`
  backlog entry.

## Decision list for Nathan

1. **Which network?** Salad (paid, small apps, wallet) / BOINC (free,
   useful, no wallet) / both (BOINC as the always-on floor, Salad in the
   paid gaps) / none yet.
2. **Crypto wallet OK?** (payouts + what gets recorded)
3. **When may it run?** Proposal: only in idle gaps — never while the
   brain is in a conversation, never while an agent job is active.
4. **Revenue expectation** — for one 4090 these are small numbers
   (roughly electricity-cost-covering, not income). The deep-dive job will
   put current per-GPU figures in before you choose.

The first think job (`render-deep-dive`) verifies all of the "to verify"
numbers against the live sites and updates this file; the choice above
stays Nathan's.
