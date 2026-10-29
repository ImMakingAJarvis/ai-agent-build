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
- 2026-10-29T01:20Z Nathan wanted to watch the worker's thought process and progress live in a page; suggested Redis as the transport.
