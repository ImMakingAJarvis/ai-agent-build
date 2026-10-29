# Kanban task dump — 2026-10-27

Job chat-1793162906-6c4d (`dump-kanban-details`). Source:
`~/.local/share/jarvis/kanban/tasks/*.json` (10 files) + epic names from
`~/.local/share/jarvis/kanban/epics/`. Task JSONs carry no comment field;
`content` is the description (null for the benchmark tasks). Subtasks
directory is empty.

All 10 tasks: status `backlog`, priority `5`.

## Epic: NoBrakes (`329068b7-0b48-403b-a520-86136a10f836`) — 6 tasks, model `local`

| ID | Title | Description (content) |
|---|---|---|
| 4546f229 | Redis Setup | Install/configure Redis for nobrakes service; define key structure: `nobrakes:auto_mode` (on/off), `nobrakes:nightly_flag` |
| 97248746 | Discord Daemon Trigger | Add command to other Discord daemon to set/clear auto_mode flag in Redis; example: `!automode on` / `!automode off` |
| 4ac55290 | DB Flag Integration | Worker loop checks Redis auto_mode flag on each iteration; if on, auto-enqueue jobs tagged nightly without manual trigger; if off, current behavior |
| 7e2972c4 | Kanban Nightly Tag | Add nightly tag to Kanban board; jobs tagged nightly are eligible for auto-execution when auto_mode=on |
| 029dc1d7 | Worker Loop Update | Modify worker to check Redis flag before enqueuing; only run nightly-tagged jobs when flag is on; log auto-executed jobs for audit |
| ad6f93fc | Safety Guardrails | Max concurrent jobs: 1; pause on error, notify via Discord; manual override always available |

Created 2026-10-27 22:04–22:05 UTC, updated 22:05–22:06 UTC.

## Epic: vram cache benchmarking of MoE models (`f4cf6031-19b0-4985-9444-a363dde6d905`) — 4 tasks, model `sonnet`, no description

| ID | Title |
|---|---|
| 066c8a01 | Benchmark with cache size = vram total - 100MB |
| 69b984cf | Benchmark with cache size = vram total - 150MB |
| ccb3a8f3 | Benchmark with cache size = vram total - 200MB |
| 5669329e | Benchmark with cache size = vram total - 250MB |

Created 2026-10-27 22:09–22:11 UTC. Note: Nathan's ask (asked.md) mentioned
100/150/200/250/300 MB — only the first four exist in the store.
