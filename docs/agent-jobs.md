# Agent jobs: the queue as the source of build work

- A job is a RabbitMQ message with `kind: "agent"`, `title`, `instructions`, `branch`, `depends_on`, `driver` (`local` default, or `sonnet`). They go on the `background` queue.
- The worker only **ingests** them: writes `~/.local/share/jarvis/jobs/<id>/job.json` + `state.json` (`ready`) and acks at once. Redelivery is harmless (ingest is idempotent).
- `agent-run` asks `jarvis-jobs next` (highest priority, then oldest, all `depends_on` done), names the job at the Discord checkpoint, and runs it with `jarvis-jobs run <id> -driver local|sonnet`. States: ready, claimed, running, done, failed (or skipped).
- Results (status, duration, model, PR URL, summary) go to the job's `state.json` and `jobs.jsonl`; the full output is `transcript.txt` in the job dir. The dashboard lists them under "Agent Jobs".
- A claimed/running job whose owner process is gone (crash, reboot) is ready again on the next `jarvis-jobs next`. A live owner is never preempted.
- Text jobs (`voice`, chat, benchmarks) keep the fast path; their output is stored as `output.txt`. API-routed jobs are recorded `failed` ("API dispatch not implemented") until that exists.

## Putting work on the queue

    scripts/migrate_tasks_to_queue -dry-run   # show the plan
    scripts/migrate_tasks_to_queue            # publish (idempotent; needs the NEW worker running)

Run it only after the worker binary from this change is running: an older worker treats agent messages as text jobs.
Other CLI: `jarvis-jobs list | show <id> | skip <id> | requeue <id>`.
