# Brain handoff — 2026-11-12 03:10 PST

<!-- Written by the brain before a swap window (docs/brain-swap.md). The
     next brain round reads this first, continues, then moves it to
     notes/archive/. Keep it short: what was in flight, the exact next step. -->

## In flight
- Job chat-1794474399-e223 (Nathan: make me a Discord avatar with the image pipeline). Concept: a nocturnal moth whose wings are a glowing constellation/circuit map of light. Two candidates in the window (job A carries the follow-up).

## First thing on return
- The follow-up round fu-avatar-moth-amber (priority 110, fullauto) runs itself: it VLM-describes both PNGs, judges them, picks or iterates, sets the Discord avatar, and closes both jobs. I only verify it happened and archive this file.

## Photo window
- window: opened by resourced for the photo batch
- photo jobs: avatar-moth-amber, avatar-luna-green (both priority 120)
- results check: two new PNGs in /var/lib/jarvis/art/out/ (seconds apart), both delivered to Nathan's DM via jarvis-discord image

## Verify on return
- [ ] jarvis-swap status: brain healthy, no maintenance window
- [ ] both candidate PNGs generated and delivered (fu-avatar-moth-amber done, Discord avatar set)
- [ ] conversation note + jarvis-notes done for the swap round

## Queue at handoff time
```
ready      plan-epic-planner-plan-with-m-1793259204-19 C2: escalation and Blocked state 
ready      plan-epic-planner-plan-with-m-1793259204-20 C3: epic PR to main 
ready      plan-epic-planner-plan-with-m-1793259204-21 C4: per-job metrics (Grafana) 
ready      plan-epic-planner-plan-with-m-1793259204-22 D1: pull + benchmark Flash Next MoE 
ready      plan-epic-planner-plan-with-m-1793259204-23 D2: docs/adding-a-model.md 
parked     chat-1793872182-6ee3                     plan, make plane entries, and schedule work for: running RENDER. Sign up for a w 
running    chat-1794474399-e223                     I want you to use our existing image generator to make yourself a discord avatar 
ready      plan-jarvis-model-downloader-1793259146-4 t2i bench: generation runner + metrics capture 
ready      plan-jarvis-model-downloader-1793259146-5 t2i bench: full run (20 prompts x all models) 
ready      plan-jarvis-build-plan-open-s-1793478730-5 Reel: TMDB metadata + artwork cache 
ready      plan-jarvis-model-downloader-1793259146-6 t2i bench: blind voting UI (Tinder-swipe, reveal, tally) 
ready      plan-jarvis-build-plan-open-s-1793478730-6 Reel: web UI — browse, search, admin 
ready      plan-jarvis-model-downloader-1793259146-7 t2i bench: metrics & results page 
ready      plan-jarvis-build-plan-open-s-1793478730-7 Reel: direct play streaming + resume 
ready      plan-jarvis-model-downloader-1793259146-8 t2i bench: docs, final tally, winner recommendation 
ready      plan-jarvis-build-plan-open-s-1793478730-8 Reel: NVENC HLS transcoding via queue 
ready      plan-jarvis-build-plan-open-s-1793478730-9 Reel: user profiles, watched state, auth 
ready      plan-jarvis-build-plan-open-s-1793478730-10 Reel Gate 2: parity report + replace/fork decision 
ready      plan-jarvis-build-plan-open-s-1793478730-11 Reel: Android app — scaffold + browse 
ready      plan-jarvis-build-plan-open-s-1793478730-12 Reel: Android app — playback (Media3) 
ready      plan-jarvis-build-plan-open-s-1793478730-13 Reel: Apple app — scaffold + macOS CI + browse 
ready      plan-jarvis-build-plan-open-s-1793478730-14 Reel: Apple app — playback (AVPlayer) 
ready      plan-jarvis-build-plan-open-s-1793478730-15 Reel: Tailscale remote access 
ready      plan-jarvis-build-plan-open-s-1793478730-16 Reel v1: field test, hardening, Gate 4 sign-off 
ready      voice-natural-tts                        Improve voice output: natural conversational TTS (currently sped up / weirdly modulated) 
```
