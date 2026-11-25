# Runtime measurements

## Save, restore, re-prefill

| prompt tokens | snapshot MB | save s | restore s | re-prefill tokens/s |
|---|---|---|---|---|
| 8001 | 589 | 0.13 | 0.16 | 3061 |
| 31996 | 1012 | 0.34 | 0.21 | 2811 |
| 100203 | 2212 | 3.24 | 0.36 | 2143 |

snapshot_min_tokens: 900

prefill time = tokens / rate; snapshot cost = save s + restore s; above snapshot_min_tokens a snapshot is cheaper than re-reading the prompt (linear interpolation between sizes).
tmpfs: skipped. The engine reads only its --slot-save-path (/var/lib/jarvis/slots, bind mount), so a snapshot elsewhere cannot be restored.

Measured 2026-11-25 on the live brain (slot 0, unified 128K KV, q4_0 cache) by `scripts/measure_slots.sh`; no other job was running during the run and none started. The smallest size measured (8000 tokens) already favours a snapshot (prefill 2.6 s vs 0.29 s save+restore), so 900 is extrapolated: save+restore is about 0.3 s at small sizes, which prefill matches at about 900 tokens. Save of the 100K snapshot (3.24 s) is the slow part; restore stays under 0.4 s.

## Engine behaviours

Measured 2026-11-25 on the live brain (llama.cpp `b1-7170cad4d`, `--parallel 2 --ctx-size 131072 --kv-unified --jinja`, q4_0 KV). Scripts were throwaway Python over `localhost:11434`; no other job (no `plane-run_task-*`) was running before, during or after, so nothing was repeated. `jarvis-engine` was restarted once for (c) inside a maintenance window and is healthy.

| | Question | Answer | Command / request |
|---|---|---|---|
| (a) | How is the 131072 context shared between slots with `--kv-unified`? | One pool, no per-slot split. A single slot can use nearly all of it: a 122,223-token prompt ran on slot 0 (61 s prefill, then a repeat was 4 tokens evaluated). A prompt over 131072 is rejected with HTTP 400 `exceed_context_size_error` (236,909 tokens tried). When both slots together exceed the pool the newer request still succeeds but the other slot's cache is dropped: slot 1 took a 59,975-token prompt while slot 0 held 122K, then repeating slot 0's prompt re-evaluated all 122,223 tokens (cache_n 0) and repeating slot 1's re-evaluated all 59,975. No error, just lost reuse. | `POST /completion {"prompt":P,"id_slot":0,"cache_prompt":true,"n_predict":4}` with P of 122K tokens, then P2 (60K) with `id_slot:1`, then each again; read `timings.cache_n` / `prompt_n`. |
| (b) | Request with no `id_slot`: which slot? | Slot 0 cached, slot 1 idle: an unrelated prompt landed on slot 1 (the empty one), cache_n 0. Both cached, unrelated prompt: it landed on slot 0, the least recently used (filled first); the other cache stays. A prompt that shares a prefix goes to the slot holding that prefix (1389 of 1394 tokens cached, either slot). | Erase both (`POST /slots/N?action=erase`), seed slot 0 with `id_slot:0`, then `POST /completion` with no `id_slot`; read `id_slot` in the reply. Repeat with both seeded. |
| (c) | Does a slot-0 snapshot restore after `docker restart jarvis-engine`, and into slot 1? | Yes to both. A 5,830-token snapshot (578 MB) saved from slot 0 (122 ms) restored after the restart into slot 1 (161 ms) and into slot 0 (166 ms); in each case the same prompt then evaluated 4 tokens with 5,823 from cache. The file lives in the `--slot-save-path` bind mount. | `POST /slots/0?action=save {"filename":"elr3-test.bin"}`; `docker restart jarvis-engine`; `POST /slots/1?action=restore` and `/slots/0?action=restore`; resend the prompt with the same `id_slot`. File removed afterwards. |
| (d) | After a client disconnect mid-generation, is the prompt still cached? | Yes. A 5,837-token prompt streamed on slot 0 with `n_predict 2000` and the connection closed after 5 chunks; the slot was idle at once, and the same prompt with `cache_prompt:true` evaluated 4 tokens (cache_n 5,833). | `POST /completion {"stream":true,"id_slot":0,"n_predict":2000,"cache_prompt":true}`, close after 5 `data:` lines, poll `GET /slots` for idle, resend the prompt. |
| (e) | Is earlier reasoning kept in the prompt on turn two, and is the cache reused? | Yes to both, when the client sends it back as `reasoning_content` on the assistant message. The rendered prompt (`POST /apply-template`) contains the `<think>…</think>` block of turn one (626 chars); without the field it is dropped (442 chars). Turn two evaluated 16 prompt tokens with 142 cached (`prompt_tokens_details.cached_tokens` 142 of 158). | `POST /v1/chat/completions` turn 1 (the reply has `message.reasoning_content`), then turn 2 with the assistant message carrying `content` and `reasoning_content`; compare `timings`. |
| (f) | Two tools, a prompt needing both: two `tool_calls` in one message? | Yes. One assistant message with two `tool_calls` (`get_weather {"city":"Paris"}`, `get_time {"city":"Tokyo"}`), `finish_reason` `tool_calls`; `reasoning_content` is a separate field and `content` is empty. The streamed version carries both calls as deltas and ends with `finish_reason:"tool_calls"` and `[DONE]`. Raw replies: `fixtures/toolcalls.json`, `fixtures/toolcalls.sse`. | `POST /v1/chat/completions` with the two tools, `temperature 0`, once with and once with `"stream":true`. |

Decision 4 is not affected: (c) restore-after-restart and (d) are both yes.

## Reload time

Seconds from `docker start jarvis-engine` until `GET 127.0.0.1:11434/health` returns 200, after `docker stop jarvis-engine` and a 10 s wait (weights in the OS page cache):

| run | seconds |
|---|---|
| 1 | 3.85 |
| 2 | 3.81 |
| 3 | 3.83 |

mean: 3.83 s

mlock flag: yes

`llama-server --help` lists `--mlock` (force the model to stay in RAM) and `--mmap`/`--no-mmap`; the brain does not pass `--mlock`. Measured 2026-11-25 inside a maintenance window; no `plane-run_task-*` job was running before or after the three runs, so none was repeated.

## Decisions

Decision: snapshot_min_tokens = 900
Decision: slots share context = yes
Decision: unpinned requests are unsafe = yes
Decision: snapshots survive restart = yes
Decision: cancel keeps prompt cache = yes
Decision: keep reasoning in history = yes
Decision: min_residency_seconds = 80
Plan status: continue

Basis: 900 tokens is the measured break-even (save+restore about 0.3 s against 2100-3000 tokens/s prefill). Slots share one KV pool (a) and an unpinned request lands on the least recently used slot and can evict its cache (b), so every caller pins a slot. Restore works after a restart and into either slot (c); a cancelled request keeps its prompt cache (d); sending `reasoning_content` back keeps the cache hit (e). Mean reload is 3.83 s, 20 times that is 76.6 s, rounded up to 80.
