# power-economics — the box costs ~$22/mo as it runs; a night of thinking is ~50¢ and in winter its heat pays for itself

The `render-deep-dive` note (2026-11-19) deferred the exact modeling here. All
numbers below are built from measurements on this machine: GPU draws from
`docs/gpu-power-measurements.md` (2026-10-21), 24 h of Prometheus
(`node_hwmon_power_watt` = CPU socket, `nvidia_gpu_power_w`), and the job
store's own per-job GPU integration (`internal/power`, 263 jobs,
2026-10-27 → 11-20: **60.5 kWh = 2.5 kWh/day of real work**, one finished
think round: 72.9 min for 0.423 kWh → **348 W avg, GPU-only**).

## Whole-box power by mode

| Mode | GPU | CPU | rest (RAM/3×NVMe/mobo) | Whole box |
|---|---|---|---|---|
| Idle, brain resident (default 24/7 state) | 71 W (measured) | ~20 W | ~40 W (estimate) | **~130 W** |
| Real work (build/chat/plan) | ~245 W (measured fleet avg) | ~45 W | ~60 W | **~350 W** |
| Think round | ~348 W (measured, one sample) | ~40 W | ~55 W | **~440 W** |
| Render / photo (GPU at limit) | ~410–450 W | ~50 W | ~60 W | **~550 W** |

The 130 W idle converges independently with the render note's 0.13 kW.
"Rest" is the only unmeasured part (no wall sensor; `node_hwmon_power_watt`
is the CPU socket only — verified via chip label). Plausible band for idle:
110–150 W.

## $/month per mode (730 h; rate $0.13/kWh baseline, range $0.10–0.30)

| Mode | kWh/mo | @ $0.13 | @ $0.10 | @ $0.30 |
|---|---|---|---|---|
| Idle 24/7 | 94.9 | **$12.3** | $9.5 | $28.5 |
| Month as it actually runs (idle base + 76 kWh work) | ~190 | **~$22–25** | ~$19 | ~$57 |
| Think every night, 9 h (floor=3 on empty nights) | +119 (marginal +84) | **+$15.4** (+$10.9) | +$11.9 | +$35.7 |
| Render 24/7 | 365 | **$47.5** (marginal $35.1) | $36.5 | $109.5 |

Sanity: the last 24 h on Prometheus is a busy day (box avg ≈ 215 W →
5.2 kWh/day → ~$20/mo), consistent with the job-store figure. The render
note's break-even ("paid option needs ~$30/mo net") checks out: render
marginal over idle is $35/mo.

## A night of idle work: cost vs heat

Night = 22:00–07:00, ~7–8 think rounds back to back (one measured round =
73 min, so the rounds fit with slack):

- **Energy: ~4.0 kWh → ~$0.51 @ $0.13** ($0.49 @ 0.12, $1.17 @ 0.30).
  The box would have eaten ~1.2 kWh idling anyway → **marginal cost of
  thinking ≈ 2.8 kWh ≈ $0.36**. Per round ≈ 0.55 kWh whole-box ≈ **7¢**.
- **Heat: 100% of draw becomes heat** — no efficiency loss, so the box is a
  perfectly efficient electric heater, just a small one. The night delivers
  ~4 kWh ≈ 13,500 BTU (≈ 430 W average). The *extra* from thinking is
  ~2.8 kWh ≈ 9,500 BTU ≈ **310 W for the night ≈ 1.9 h of a 1500 W space
  heater**, bought for $0.36.
- The idle box alone (130 W) is a nightlight as a heater — a rounding error
  for a room. The machine only becomes a *real* heater when it works: a
  think night is a ~400 W heater, a render night ~550 W.
- **Winter**: electric heat displaces grid heat 1:1, so the extra $0.36 of
  a think night is worth exactly $0.36 in warmth — a night of thinking is
  effectively free (the notes are the bonus). **Summer**: $0.36 of pure
  waste-heat cost, per night.

## Options / next steps

1. **Set the real rate in the job store (5 min, no code).**
   `JARVIS_POWER_RATE` and `JARVIS_POWER_SYSTEM_W` are unset (defaults 0),
   so every job's `power_cost` is $0 and the dashboard "Power Cost (local)"
   card and HA sensors report zero. Put `JARVIS_POWER_RATE=0.13` (or the
   real bill rate — unconfirmed anywhere in the repo) and
   `JARVIS_POWER_SYSTEM_W≈60` in the agent-run env and restart; all 263
   future jobs' kWh become dollars.
2. **Integrate the CPU sensor into per-job kWh (small Go change).**
   `internal/power` currently integrates GPU draw only; the CPU socket is
   already in Prometheus (`node_hwmon_power_watt`). Adding it (plus the flat
   rest term) raises job energy from ~245 W to ~350 W avg — ~30% more
   honest, and makes the think-vs-work cost split real.
3. **Winter policy, no code: keep the think floor up in cold months.** A
   think night buys ~9,500 BTU of real heat for ~$0.36 marginal; in winter
   that heat is worth its face value, so the marginal value of a night of
   thinking is heat + notes. In summer it costs $0.36/night — still trivial.
4. **Meter the wall if the idle number matters ($20).** One night of plug-
   meter sampling (or the mom's-place bill delta) replaces the 40 W "rest"
   estimate with a measurement. Optional until we care about ±$3/mo.
5. **Paid GPU networks stay a no-go on this box — now with exact numbers.**
   Render 24/7 = $47.5/mo vs idle $12.3/mo; the marginal $35/mo is the
   floor any paid option must clear net. Vast's median $0.35/hr ask only
   clears it above ~30% sustained utilization on a single residential 4090.
   Confirms render-deep-dive's verdict: this is a hardware decision (2nd
   GPU / mini-box), not a network decision.

## Recommendation

Do **option 1 now** — one env var turns the kWh tracking that already runs
into real money, and every later economic question (this one included) stops
being an estimate. Queue **option 2** as a small code job when convenient.
Treat **option 3** as the standing answer to "the box is a heater": the
idle box isn't (130 W is a nightlight), but a night of thinking is a real
~400 W night-heater whose heat, in winter, pays for its own electricity.
Don't spend on paid GPU sharing for this box (option 5) — the break-even
math is now exact and unforgiving.

**Caveats**: rate $0.13 is an assumption (repo uses $0.12 in Oct, $0.13 in
Nov; never confirmed against the bill); the think-round wattage is one
sample; and the `sensor.jarvis_power_cost_today` / `jarvis_energy_today`
HA sensors named in `docs/agent-jobs.md` do not appear in HA today — the
jarvis-ha publish path for them may be broken, worth a look with option 1.
