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
