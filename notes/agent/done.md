# done — what the agent has shipped
# One line per finished job or PR: `- <UTC ts> [<job>] <what> (PR #n)`.
# Keep it curated; the full detail lives in the job transcript in the vector graph.
# (Seeded 2026-10-28 with recent merges; each round appends one line.)

- 2026-10-28T03:23Z Conversation notes: agent notes + Stop-hook enforcement (PR #57)
- 2026-10-28T03:23Z Worker: interrupt a running job from the queue site (owner messages, no restart) (PR #58)
- 2026-10-28T03:23Z Worker: priority order jobs > fix > build > background (PR #64)
- 2026-10-28T03:23Z Queue: drag to reorder & reclassify jobs, mouse + touch (PR #65)
- 2026-10-28T03:23Z Voice chat: HTTPS mic (secure context) + IP-based link (PR #66)
- 2026-10-28T03:23Z Kanban: fix empty board render (list APIs answer [] not null) (PR #70)
- 2026-10-28T03:23Z Voice chat: schedule jobs + search job history via tool calling (PR #71)
- 2026-10-28T03:23Z Queue: runtime re-prioritization of band order (PR #75)
- 2026-10-28T03:23Z Mobile chat: Jarvis Chat PWA, iOS + Android (PR #76)
- 2026-10-28T03:23Z PWA mic: decode webm/opus uploads via ffmpeg (PR #78)
- 2026-10-28T03:23Z Voice: fix broken multi-line replies (newlines in reply headers) (PR #80)
- 2026-10-28T03:23Z Dashboard: /logs page with engine status + log tails (PR #81)
- 2026-10-28T03:23Z Dashboard: in-progress / recently-finished job lists (PR #82)
- 2026-10-28T03:23Z Stack: containerize all self-built services (compose + Dockerfiles + boot) (PR #83)
- 2026-10-28T03:23Z Chatbot: memory of the work logs (vector DB wiring, RAG + search tool) (PR #85)
- 2026-10-28T03:29Z [chat-1792998605-81b4] Agent notes: three-bucket done/system/asked notes + jarvis-notes CLI, wired into the vector graph (PR #87)
- 2026-10-28T03:57Z [chat-1793040169-43b4] FreeToken runner hardened: clone at /home/agent/freetoken (v0.2.1), editable venv /home/agent/ftvenv verified (PR #89)
- 2026-10-28T04:21Z [chat-1793001320-4d10] Voice chat mic: HTTPS 8443 listener restored in the containerized stack — compose publishes 8443 + bind-mounts ~/.config/jarvis/tls ro; plain-HTTP-only origin was why the browser never prompted for the mic (PR #90)
- 2026-10-28T04:39Z [chat-1793161590-d967] Discord chatbot 'stumbling' fixed — models.NewClient now reads ENGINE_URL (was OLLAMA_URL/localhost only), so brain calls work in the containerized stack; dashboard given ENGINE_URL too (PR #92)
- 2026-10-28T04:43Z [chat-1793161502-641f] Scheduled voice-natural-tts job (build/medium): fix sped-up/modulated voice output, natural conversational persona
- 2026-10-28T05:06Z [chat-1793008053-3f25] Diagnosed VPE 'wake word fires then pulses and stops': root cause is dead HA STT stt.speech_to_phrase (unknown since ~Oct 23); wake word + VPE->HA + local brain/voice all healthy, pipeline stalls at listening until STT timeout. Fix = load installed Whisper STT on HA + point 'Jarvis' pipeline at it (PR #93)
- 2026-10-28T05:36Z [chat-1793164047-6316] PR #93: corrected HA-access wording — this box CAN see HA via MCP/REST/WS (re-verified dead STT live); only the pipeline/STT config is not settable from here (job chat-1793164047-6316)
- 2026-10-28T05:41Z [chat-1793162906-6c4d] [chat-1793162906-6c4d] Dumped details of the 10 kanban task JSONs (6 NoBrakes + 4 VRAM-benchmark tasks, all backlog/p5) to notes/kanban/kanban-dump-2026-10-27.md (PR #94)
- 2026-10-28T06:26Z [chat-1793163063-9121] CLAUDE.md: read the kanban board entry (task+epic+comments, ID from job title) before building; comment system absent so queued job kanban-comments (PR #95)
- 2026-10-28T06:50Z [chat-1793164192-8d46] Voice channel epic 'camera mics + direct Sonos/Google speakers (no HA)' on the kanban: 12 tasks in dependency order + design doc (job chat-1793164192-8d46, PR #96)
- 2026-10-28T10:21Z [chat-1793177028-8f95] Plan upload: dashboard Plans section (POST /api/plans) stores plan files under ~/.local/share/jarvis/plans/<id>/ and enqueues a ready agent job with the same id whose instructions are the worker's breakdown recipe; jarvis-jobs add gains -depends (PR #98)
- 2026-10-28T20:11Z [chat-1793046157-6cbc] Fix-queue priority job chat-1793046157-6cbc closed as already done: PR #64 (agentjob class ordering) + PR #74 (worker SetDepths), both merged; evidence comment on PR #74
- 2026-10-28T20:30Z [chat-1793091300-027f] Discord !photo command (jarvis-chat → art service, image attachment DM) + photo gallery site at :8120 with per-image prompt metadata (PR #99)
