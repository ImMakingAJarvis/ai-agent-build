# Brain handoff — 2026-11-08 14:08 PST

<!-- Written by the brain before a swap window (docs/brain-swap.md). The
     next brain round reads this first, continues, then moves it to
     notes/archive/. Keep it short: what was in flight, the exact next step. -->

## In flight
- EVB-7 (job plan-plan-event-driven-brain-1794016330-7): zombie engine resolved per
  Nathan (option A, compose canonical — docs in PR #205), go build/vet/test green,
  all 13 unmerged branches audited + documented. Only gate left: this manual photo
  window. Stopped right after queueing the photo job + requeueing EVB-7 (fix/100).

## First thing on return
- Verify the photo: `jarvis-jobs show photo-evb7-1794175744` → status done, and
  `ls ~/.local/share/jarvis/photos/photo-evb7-1794175744.sent` exists. If done:
  finish EVB-7 — `jarvis-board done EVB-8 "done: ..."` (PR #205), `jarvis-discord
  notify` with the PR link, `jarvis-jobs finish plan-plan-event-driven-brain-1794016330-7 0`,
  `mkdir -p notes/archive && mv notes/brain-handoff.md notes/archive/`, jarvis-notes.
  If NOT done: `jarvis-discord notify` Nathan with the failure, requeue the photo job,
  keep EVB-7 open.

## Photo window
- window: opened automatically by the agent-run watcher on the photo pull
- photo jobs: photo-evb7-1794175744 (flipped to igpu, runs as jarvis-photo code, no model round)
- results check: the delivered Discord image + the .sent marker above

## Verify on return
- [ ] jarvis-swap status: brain healthy, no maintenance window
- [ ] photo-evb7-1794175744 done + .sent marker (image delivered to Discord)
- [ ] (conversation note + jarvis-notes done for the swap round)

## Queue at handoff time
```
ready      chat-1794173444-f261                     the image generator is only generating 1024x1024 images. I want to be able to do 
ready      plan-epic-planner-plan-with-m-1793259204-19 C2: escalation and Blocked state 
ready      plan-epic-planner-plan-with-m-1793259204-20 C3: epic PR to main 
ready      plan-epic-planner-plan-with-m-1793259204-21 C4: per-job metrics (Grafana) 
ready      plan-epic-planner-plan-with-m-1793259204-22 D1: pull + benchmark Flash Next MoE 
ready      plan-epic-planner-plan-with-m-1793259204-23 D2: docs/adding-a-model.md 
parked     chat-1793872182-6ee3                     plan, make plane entries, and schedule work for: running RENDER. Sign up for a w 
ready      plan-jarvis-model-downloader-1793259146-4 t2i bench: generation runner + metrics capture 
ready      plan-jarvis-model-downloader-1793259146-5 t2i bench: full run (20 prompts x all models) 
ready      plan-jarvis-build-plan-open-s-1793478730-5 Reel: TMDB metadata + artwork cache 
ready      plan-jarvis-model-downloader-1793259146-6 t2i bench: blind voting UI (Tinder-swipe, reveal, tally) 
ready      plan-jarvis-build-plan-open-s-1793478730-6 Reel: web UI — browse, search, admin 
ready      plan-jarvis-model-downloader-1793259146-7 t2i bench: metrics & results page 
running    plan-plan-event-driven-brain-1794016330-7 EVB: resolve zombie compose engine; Phase 0 stabilization gate 
ready      plan-jarvis-build-plan-open-s-1793478730-7 Reel: direct play streaming + resume 
ready      plan-plan-event-driven-brain-1794016330-8 EVB: message envelope v2 + Result type + v1-compat tests 
ready      plan-jarvis-model-downloader-1793259146-8 t2i bench: docs, final tally, winner recommendation 
ready      plan-plan-event-driven-brain-1794016330-9 EVB: events exchange + lifecycle event publisher 
ready      plan-jarvis-build-plan-open-s-1793478730-8 Reel: NVENC HLS transcoding via queue 
ready      plan-plan-event-driven-brain-1794016330-10 EVB: jarvis.dlx + dead queue + dashboard dead letters 
ready      plan-jarvis-build-plan-open-s-1793478730-9 Reel: user profiles, watched state, auth 
ready      plan-plan-event-driven-brain-1794016330-11 EVB: declare fix + image in definitions.json; document every queue 
ready      plan-jarvis-build-plan-open-s-1793478730-10 Reel Gate 2: parity report + replace/fork decision 
ready      plan-plan-event-driven-brain-1794016330-12 EVB: agentjob waiting status + Yield/Wake state machine 
ready      plan-jarvis-build-plan-open-s-1793478730-11 Reel: Android app — scaffold + browse 
```
