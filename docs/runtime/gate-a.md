# Gate A — two jobs, one slot, a slow command (ELR-35 / M3.6b)

Run 2026-11-26 on the real brain (jarvis-engine, :11434), from sonnet's round,
no local-driver job running. loopctl built from origin/epics/ELR-1.

## Command

```
go build -o /tmp/loopctl ./cmd/loopctl
/tmp/loopctl run --slot 0 "Run the shell command 'sleep 30 && echo first-done' and then tell me what it printed." "Create a file named hello.txt containing the word hi, then read it back and tell me its content."
```

## Full output (exit 0)

```
loopctl-1795712793-1  status=done  ticks=2  summary=The command printed `first-done`.
loopctl-1795712793-2  status=done  ticks=3  summary=Done. I created `hello.txt` and read it back — its content is: **hi** (a single line).
idle_while_ready_seconds: 0
wall_seconds: 37
```

## Numbers

| | job 1 (sleep 30) | job 2 (hello.txt) |
|---|---|---|
| status | done | done |
| ticks | 2 | 3 |

- idle_while_ready_seconds: 0 (criterion: under 1)
- wall_seconds: 37

## Prompt tokens evaluated (`curl -s 127.0.0.1:11434/metrics`, `llamacpp:prompt_tokens_total`, excludes cached tokens)

- before: 3.59915e+06
- after: 3.60154e+06
- delta: about 2390 tokens for 5 ticks across both jobs. That is far less than five full prompt re-evaluations would cost, so the second ticks mostly reused cache. The counter is engine-wide, not per job, and the brain was otherwise idle, so this is indicative, not a per-job proof.

## Caveat

loopctl prints no per-tick timestamps, so "job 2 finished while job 1's sleep was still running" is inferred, not observed: wall time was 37 s (sleep 30 s plus about 7 s for the five model ticks), and idle_while_ready_seconds was 0, which a blocked slot would not give. A later card could add tick timestamps to loopctl to make this direct.

Gate A: PASS
