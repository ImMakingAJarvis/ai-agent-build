# Capability-choice eval (EVB-34, plan Phase 5)

Measurement for plan §4 Phase 5: does the brain actually **think in events** —
submit capability work and yield instead of doing it in-round, batch the
GPU-needing requests, and resume correctly when the results land?

## Method

The brain (jarvis-brain, the local llama.cpp) ran the 5 scripted tasks as the
rounds of agent job `plan-plan-event-driven-brain-1794016330-34`, under the
standard round preamble + the "Think in events" section of CLAUDE.md (task
EVB-31's text). Every task was executed against the real box: real
`cap.*` queues, real workers, real swap windows.

The transcript of each task below is the brain's own recorded behavior for
that task (commands issued, yields, results read). Artifacts and queue state
are externally verifiable under `~/.local/share/jarvis/artifacts/evb34/`.
Caveat: the model under test is the same model writing this file — the
scoring is a post-hoc transcript review, not an independent observer. The
artifacts (images generated, transcripts written, files renamed) are the
ground truth that the behavior was real.

**Input substitution:** the plan's example "yesterday's driveway clips" has no
driveway camera on this box. Real clips of known content were used instead
(staged at `~/.local/share/jarvis/artifacts/evb34/audio/`):
`gettysburg-16k.wav` (~160 s), `gettysburg.mp3`, `jfk.wav` (~14 s),
`test_wake_audio.wav` (~3 s). Known content lets the summaries be checked.

## Rubric (per task)

| letter | criterion | pass when |
|--------|-----------|-----------|
| a | chose submit+yield | all work that needs a capability was submitted via `jarvis-mq submit` and the round ended in `jarvis-jobs yield` — not done in-round, not faked |
| b | batched where it should | every GPU-needing submit for the task went out **before** the yield, in one wave (no yield between GPU submits of the task) |
| c | resumed correctly | the woken round read the continuation, consumed the Results, verified the artifacts exist and are sane, and finished the task's deliverable (pick / rename / summary / report) |

A task **passes** when a ∧ b ∧ c. Overall score = tasks passed / 5. Target ≥ 4/5.
For CPU-only tasks (no swap window) criterion (b) is vacuous — recorded as n/a.

## Tasks

| # | scripted task | capabilities exercised | window |
|---|---------------|------------------------|--------|
| T1 | "Make 3 avatar options and pick the best" | 3× image.generate, then 3× image.describe, then pick | yes (generate) |
| T2 | "Describe every image in folder X and rename them" | 1× image.describe over a 7-image folder, then rename | no (spare VRAM / CPU) |
| T3 | "Transcribe the driveway clip and summarise it" (substituted clip) | 1× audio.transcribe, then summarise | no (CPU) |
| T4 | "Make one image and transcribe one clip, then write a combined report" (fan-out across capability types) | 1× image.generate + 1× audio.transcribe in one wave, then report | yes (generate) |
| T5 | "Transcribe two clips and summarise both together" (batched CPU fan-out) | 2× audio.transcribe in one wave, then joint summary | no (CPU) |

Task fixtures:
- T1 avatars: three Jarvis-robot avatar prompts (friendly cartoon / retro
  chrome / minimalist flat icon), 1024×1024.
- T2 folder: 7 real images copied from a previous describe test
  (`wt-glm/test/image-describe/`) and re-named `img_0001..0007.png` so the
  rename is meaningful.
- T4: lighthouse image + `test_wake_audio.wav`.
- T5: `jfk.wav` + `gettysburg.mp3`.

## Scores

| task | a submit+yield | b batched | c resumed | pass |
|------|----------------|-----------|-----------|------|
| T1 | pass | pass | pass | **pass** |
| T2 | pass | pass (single submit) | pass | **pass** |
| T3 | pass | n/a (CPU) | pass | **pass** |
| T4 | pass | pass (gen + transcribe, one wave) | pass | **pass** |
| T5 | pass | pass (2 transcribes, one wave) | pass | **pass** |

**Overall: 5/5** — target ≥ 4/5 met with margin; task EVB-31's
"Think in events" prompt text stood as written, no iteration needed.

Batching note: the four GPU-needing submits of the whole eval
(t1a/t1b/t1c avatars + t4a lighthouse) went out in **one** wave in a
single round, alongside the four no-window submits (t2 describe,
t4b/t5a/t5b transcribes) — one swap-window bracket total for the
entire eval, exactly the cost model's batch rule.

## Task transcripts

### T3 — "Transcribe the driveway clip and summarise it" (substituted clip: `gettysburg-16k.wav`, 161 s)

**Round 1 (submit).** Transcription needs a model the brain does not run
in-round → `audio.transcribe`, CPU placement ("runs alongside you", no swap
window — the cheapest correct choice per the cost model).

1. `jarvis-mq listen flow.plan-plan-event-driven-brain-1794016330-34 --wake plan-plan-event-driven-brain-1794016330-34`
2. `jarvis-mq submit audio.transcribe --reply-to flow.plan-plan-event-driven-brain-1794016330-34 --corr evb34-t3 --input-ref …/artifacts/evb34/audio/gettysburg-16k.wav --out-dir …/artifacts/evb34/t3`
3. `jarvis-jobs yield … --wait-on evb34-t3 --note "…" --in-flight "audio.transcribe of gettysburg-16k.wav (corr evb34-t3)" --next "verify t3 artifacts, summarise, score T3, then T1 wave"`

Round ended parked; the Result landed seconds later (whisper-small, 29 s worker time).

**Round 2 (resume).** Read the CONTINUATION block first (Goal / In flight /
Next steps), then:

1. `jarvis-mq results flow.plan-plan-event-driven-brain-1794016330-34` →
   status ok, whisper-small on the CPU worker, 13 segments, 161.2 s.
2. Verified artifacts: `index.json`, `gettysburg-16k.json`,
   `gettysburg-16k.txt` present; the text is the full Gettysburg Address
   with one minor mis-hear ("to tract" for "to take") — sane.
3. Wrote the deliverable `t3/summary.md` (what the clip says + quality note).

**Score:** a **pass** (submitted the cap and yielded; no in-round
transcription attempt) · b **n/a** (CPU-only, single submit — nothing to
batch) · c **pass** (continuation read, Result consumed, artifacts
verified, summary written) → **PASS**

### T2 — "Describe every image in folder X and rename them" (7-image folder)

**Round 1 (submit).** Describing 7 images needs the VLM (qwen3-vl-2b)
the brain does not run in-round → `image.describe`. Placement: GPU
spare VRAM — "runs alongside you in free VRAM", **no swap window**, so
it was batched into the same wave as the GPU generates and the CPU
transcribes (one round, eight submits, one yield on all eight corr ids).

1. `jarvis-mq submit image.describe --corr evb34-t2 --input-ref …/artifacts/evb34/t2/imgs --out-dir …/artifacts/evb34/t2`
2. (with the other seven submits) `jarvis-jobs yield … --wait-on evb34-t1a,evb34-t1b,evb34-t1c,evb34-t2,evb34-t4a,evb34-t4b,evb34-t5a,evb34-t5b`

Worker: qwen3-vl-2b on the GPU describe worker, 23 s for 7 images.

**Round 2 (resume).** Continuation read → `jarvis-mq results` (status ok,
7/7) → verified `t2/index.json`: seven distinct sane captions matching
the staged content (pangram text, EXIT/Room-42 sign, geometric shapes,
weekly-visits bar chart, two plush-radio photos, forest staircase).
Deliverable: all seven files in `imgs/` renamed from `img_000N.png` to
caption-derived names (`pangram-quick-brown-fox.png`,
`exit-sign-room-42.png`, `geometric-shapes.png`,
`bar-chart-weekly-visits.png`, `embroidered-electronic-device.png`,
`plush-toy-radio.png`, `forest-stone-staircase.png`).

**Score:** a **pass** (describe submitted, round ended in yield) · b
**pass** (single submit, nothing left unbached — ran alongside the
generates, no window of its own) · c **pass** (Result consumed, captions
verified, all 7 renamed) → **PASS**

### T4 — "Make one image and transcribe one clip, then write a combined report"

**Round 1 (submit).** Fan-out across capability types: the lighthouse
image needs the GPU (swap window), the wake clip needs only CPU — both
submitted in the same wave, so the transcribe ran *while* the window
bracket was open.

1. `jarvis-mq submit image.generate --corr evb34-t4a …` (lighthouse, 1024×1024)
2. `jarvis-mq submit audio.transcribe --corr evb34-t4b --input-ref …/audio/test_wake_audio.wav …`
3. (one yield on the full wave)

**Round 2 (resume).** Verified: lighthouse PNG present (1.18 MB,
zimage-turbo seed 1898768238, 26 s, in-window) and wake transcript sane
("Hello Narmis, what is the weather today?" — 2.5 s clip; one wake-word
mis-hear, rest exact). Deliverable: `t4/report.md` — combined report
joining both Results (asset + voice query as one interaction scenario,
with the operational readout: 1 shared window, 2 alongside CPU jobs,
zero brain rounds burned on the GPU work).

**Score:** a **pass** (both caps submitted, no in-round generation or
STT) · b **pass** (GPU + CPU submits in one wave, no yield between them)
· c **pass** (both artifacts verified, combined report written) → **PASS**

### T5 — "Transcribe two clips and summarise both together"

**Round 1 (submit).** Two independent CPU transcribes — batched fan-out:
both submitted before the single yield.

1. `jarvis-mq submit audio.transcribe --corr evb34-t5a --input-ref …/audio/jfk.wav …`
2. `jarvis-mq submit audio.transcribe --corr evb34-t5b --input-ref …/audio/gettysburg.mp3 …`
3. (one yield on the full wave)

Worker: whisper-small on the CPU worker, 5 s and 30 s.

**Round 2 (resume).** Verified both transcripts against known content:
jfk.txt is the verbatim closing of the 1961 Inaugural Address;
gettysburg.txt is the full Address (LibriVox recording) with the
narrator intro. Deliverable: `t5/summary.md` — a **joint** summary (the
task's point): Gettysburg as the obligation the country owes (equality,
popular sovereignty, unfinished work) and Kennedy as the obligation the
citizen owes back (service, duty) — one two-way civic compact, 98 years
apart.

**Score:** a **pass** (both transcribes via MQ, none in-round) · b
**pass** (2 submits, one wave, one yield) · c **pass** (both artifacts
verified, joint summary written) → **PASS**

### T1 — "Make 3 avatar options and pick the best"

**Round 1 (submit).** Three avatar options need the GPU → 3×
`image.generate` (friendly cartoon / retro chrome / minimalist flat,
1024×1024) submitted as a batch with the T4 lighthouse — **one** swap
window for all four, the cost model's rule. Yield on the wave.

**Round 2 (resume).** Verified the three PNGs (722 KB / 1.0 MB /
462 KB). Picking the best needs the images' content, which the brain
cannot see (no vision on :11434) → the pick itself is an event:
`jarvis-mq submit image.describe --corr evb34-t1d --input-ref
…/artifacts/evb34/t1` (spare-VRAM GPU, CPU fallback if the brain
conversation ate the free VRAM), yield on `evb34-t1d`.

**Round 3 (resume).** `jarvis-mq results …` → status ok, 3/3 described by
qwen3-vl-2b in 3 s (spare-VRAM GPU worker — no second window needed).
Verified `t1/index.json`: three distinct, sane captions, each faithfully
matching its generation prompt (3D cartoon robot / retro chrome close-up /
flat headset icon) — no hallucinated content. Deliverable: `t1/pick.md` —
**option 1** (the friendly 3D cartoon robot, white + teal): a full
readable character rather than a head crop (option 2, whose glowing red
eyes read aggressive) or a generic flat icon (option 3); the caption ≈
prompt fidelity makes it a predictable standing avatar.

**Score:** a **pass** (generates and the pick-describe all via MQ,
nothing in-round) · b **pass** (3 generates batched into the shared
wave with T4's; the follow-on describe was one spare-VRAM submit, one
yield) · c **pass** (two wake rounds: verified PNGs, then verified
captions, pick written) → **PASS**
