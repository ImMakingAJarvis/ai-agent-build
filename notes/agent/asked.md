# asked — what Nathan has asked about or decided
# One line per topic: `- <UTC ts> <topic / decision>`. Group by theme as they recur.
# These are the *themes*; the specific words and history live in the vector
# graph (job transcripts + Discord logs), searchable via the memory service.

- 2026-10-28T03:23Z Local brain / model choice — which model runs the agent; decision: Qwen3.8 27B as the brain, no separate coder model for now (revisit qwen3-coder-next only if the brain's coding proves insufficient).
- 2026-10-28T03:23Z Voice — wake-word detection, STT/TTS, and the mic path (PWA/HTTPS secure context, client-side VAD auto-stop, webm/opus decode); plus voice-chat tool calling to schedule jobs and search job history.
- 2026-10-28T03:23Z Queue & worker priority — the ordering jobs > fix > build > background, runtime re-prioritization of band order, and keeping the highest non-empty queue unpaused so fixes take priority.
- 2026-10-28T03:23Z Dashboard — a /logs page (engine status + log tails), in-progress / recently-finished job lists, and the drag&drop queue linked from the status bar.
- 2026-10-28T03:23Z Cost — measuring power cost: kWh and $ from GPU power draw for local jobs.
- 2026-10-28T03:23Z Mobile — the Jarvis Chat PWA (iOS + Android).
- 2026-10-28T03:23Z Conversation notes — keep notes from all conversations (agent rounds in git; Discord/voice verbatim, gitignored).
- 2026-10-28T03:23Z Memory — a local vector graph over the work logs so the agent/chatbot can recall what was done and what is queued next; this round: start keeping structured notes (done/system/asked) fed into it.
- 2026-10-28T03:23Z Recurring "is this clean?" checks — e.g. GLaDOS/Portal: is the branch present and the working tree clean. Expect similar verify-a-state checks; answer with evidence, not a guess.
- 2026-10-28T03:22Z [chat-1792998605-81b4] Nathan wants the agent to keep notes (done/system/asked) wired into the vector graph
- 2026-10-28T03:57Z Nathan wants FreeToken as second runner: install+verify now, then VRAM-spare benchmark (100/150/200/250/300 MB free) of qwen3.8-flash-next (job chat-1792976974-392b)
- 2026-10-28T04:21Z Voice chat app mic: 'permission denied, never asked, still denied with everything allowed' (chat-1793001320-4d10) — classic non-secure-context signature, not a settings issue
- 2026-10-28T04:39Z Nathan reported the Discord chatbot was 'stumbling' (replying 'Sorry, my brain stumbled'); wanted it fixed so it actually answers.
- 2026-10-28T05:36Z Nathan on PR #93: 'you have MCP access to HA — is it not in your tools?' — confirmed it is and works; corrected PR; Whisper-STT fix still to be applied on the HA box
- 2026-10-28T06:26Z Nathan: worker must check the kanban entry (kanban ID in job title) and read task+epic+comments before building; worker must be able to comment on the kanban with questions — CLAUDE.md gets that line once the comment system ships
- 2026-10-28T06:50Z Own voice channel bypassing Home Assistant: UniFi Protect camera mics (RTSP audio) as input, Sonos (S1 RCP announce) / Google Nest (Cast) / camera speakers (Protect talk) as direct output; future mics expected — built as kanban epic ed9ecd12, 12 tasks, design doc PR #96
- 2026-10-28T07:19Z Nathan asked (job chat-1793010774-0126) whether the Ryzen iGPU could serve as another model handler — investigated, PR #97, recommendation: keep disabled
- 2026-10-28T10:21Z Nathan asked (job chat-1793177028-8f95): a way to upload a md or other file with a plan, and a worker takes it and creates all the jobs around it — decided: dashboard upload enqueues the breakdown job; the actual job creation happens in the worker's round.
- 2026-10-28T20:11Z Nathan's repeated theme (Oct 26-28): fix jobs must always run before build, build before background — resolved by PR #64 + #74, original job 6cbc closed on evidence
- 2026-10-28T20:30Z Nathan (job chat-1793091300-027f): discord daemon should be able to trigger photo gen, and there should be a site displaying all generated images
- 2026-10-28T20:38Z Nathan: fix PR #97 (iGPU investigation) — it was CONFLICTING with main; wanted it mergeable for review
- 2026-10-28T22:37Z !photo posted no image — Nathan asked to debug the art/Discord posting path; also: he'll check jarvis-brain status next job if local chat feels off
- 2026-10-28T23:01Z Nathan (job chat-1793219773-c61e): service restart must be triggerable from jarvis-jobs interface or Discord even when the daemon is down
- 2026-10-28T23:26Z Nathan: always-on small model (CPU, or iGPU if accessible) as a helper for setup changes and self-restart — iGPU route adopted
- 2026-10-29T01:20Z Nathan wanted to watch the worker's thought process and progress live in a page; suggested Redis as the transport.
- 2026-10-29T02:05Z Live worker view re-queued as duplicate (chat-1793228762-fd2a); confirmed live on main via merged PR #104, Nathan said close it
- 2026-10-29T03:46Z Nathan wants worker logs human readable, not raw stream-json; he confirmed he was on the raw log page when he saw the json
- 2026-10-29T04:53Z Nathan asked for an output of running containers + currently loaded models; he suspected a qwen2.5 chatbot on the iGPU — confirmed: Qwen2.5-3B Q4_K_M on :11435 is the jarvis-helper model
- 2026-10-29T07:31Z Nathan (job chat-1793252684-1127): open PRs have merge conflicts — check and fix all open PRs' mergeability
- 2026-10-29T18:22Z Two helpers (CPU + iGPU)? other models for the box? — answered: deep helper = 30B-A3B on iGPU on-demand, CPU stays free (job chat-1793249102-d283)
- 2026-10-29T19:51Z Photo workflow: wanted the thinking mind to write much better image-generator prompts from casual requests, with special modes — photoassist for refining prompts with context, photocheck for iterating with the generator (job chat-1793252974-720f)
- 2026-10-29T21:11Z Nathan: schedule all of the kanban tasks for MOE benchmarking — the four vram-cache points of epic f4cf6031 (100/150/200/250 MB free VRAM, Qwen3.8-Flash-Next on FreeToken)
- 2026-10-29T22:29Z Nathan wants the smaller iGPU model as an option for running rounds/jobs (e.g. MOE benchmarks) that need the 4090
- 2026-10-30T00:33Z Nathan: which image gen model is active and does it support reference images (img2img)? — answer: Z-Image-Turbo, t2i only (job chat-1793307525-a7df)
