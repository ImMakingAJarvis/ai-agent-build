# Agent jobs: the queue as the source of build work

- A job is a RabbitMQ message with `kind: "agent"`, `title`, `instructions`, `branch`, `depends_on`, `driver` (`local` default, or `sonnet`), and optional `reasoning_effort` (`off`, `low`, `medium`, `high`/`xhigh`/`max`). Coding defaults to medium; voice defaults to off. They go on the `background` queue.
- The worker only **ingests** them: writes `~/.local/share/jarvis/jobs/<id>/job.json` + `state.json` (`ready`) and acks at once. Redelivery is harmless (ingest is idempotent).
- `agent-run` asks `jarvis-jobs next` (highest priority, then oldest, all `depends_on` done), names the job at the Discord checkpoint, and runs it with `jarvis-jobs run <id> -driver local|sonnet`. States: ready, claimed, running, done, failed (or skipped).
- Results (status, duration, model, PR URL, summary) go to the job's `state.json` and `jobs.jsonl`; the full output is `transcript.txt` in the job dir. The dashboard lists them under "Agent Jobs".
- A claimed/running job whose owner process is gone (crash, reboot) is ready again on the next `jarvis-jobs next`. A live owner is never preempted.
- Text jobs (`voice`, chat, benchmarks) keep the fast path; their output is stored as `output.txt`. API-routed jobs are recorded `failed` ("API dispatch not implemented") until that exists.

## Putting work on the queue

    scripts/migrate_tasks_to_queue -dry-run   # show the plan
    scripts/migrate_tasks_to_queue            # publish (idempotent; needs the NEW worker running)

Run it only after the worker binary from this change is running: an older worker treats agent messages as text jobs.
Other CLI: `jarvis-jobs list [status] | show <id> | skip <id> | requeue <id>`. `list` takes an optional status (e.g. `jarvis-jobs list failed`) and ends with a `# N jobs: …` line giving per-status totals for the unfiltered set, so a `head`-truncated view still shows how much was cut off.

Set thinking depth when enqueueing or running:

```
jarvis-jobs add -id J1 -title "…" -effort medium
jarvis-jobs run J1 -driver local -effort high
enqueue --type=coding --effort=low --prompt="…"
enqueue --type=voice --effort=off --prompt="…"   # voice default is already off
```

## Power cost (local jobs)

While a local agent job runs, `jarvis-jobs` samples GPU power draw
(`nvidia-smi power.draw`) every 10 s and integrates it into watt-hours
(`internal/power`). At the end of the round the total is converted to
kWh at the configured $/kWh rate and stored on the job (`kwh`,
`power_cost` in `state.json`) and in the `jobs.jsonl` history record.
Sampling is cumulative across attempts of the same job: the running
total persists in `power.json` in the job dir, and a round resumed
after a reboot picks it back up (at most one sampling interval is lost).

| Variable | Default | Meaning |
| --- | --- | --- |
| `JARVIS_POWER_RATE` | `0` | $/kWh; 0 tracks kWh at $0 |
| `JARVIS_POWER_SYSTEM_W` | `0` | flat non-GPU overhead in W (the machine has no system power sensor, so the total is GPU draw + this) |
| `JARVIS_POWER_SAMPLE_SECS` | `10` | sampling interval |
| `JARVIS_POWER_GPU_CMD` | `nvidia-smi` | override for tests |

Where the numbers show up: the Phase 6 dashboard (Cost Tracking cards
"Power Cost (local)" / "Energy (local)", a ⚡ cell per recent job, and a
Power column on the Agent Jobs table), the `jarvis-ha` sensors
`sensor.jarvis_power_cost_today` / `sensor.jarvis_energy_today`, and
the job log. Reference draw for this box: see
`docs/gpu-power-measurements.md` (idle ~58 W, loaded idle ~71 W,
generating ~412 W).
