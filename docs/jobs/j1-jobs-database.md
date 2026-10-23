# J1 — Job records in a database Nathan can query

Read only the section for your job. Background: job history lives in `/home/agent/.local/share/jarvis/jobs.jsonl`, and J0 adds a file-backed job store. Only the `agent` user can read either one. Everything moves into one PostgreSQL database that Nathan can query with `psql jarvis`. J0.3's `jobstore.Store` interface stays, and J1 adds a PostgreSQL implementation of it.

## J1.1 — Install PostgreSQL with roles

- `apt install postgresql`, with `postgresql.service` enabled and listening on localhost only. Create database `jarvis`. Role `agent` owns it and uses peer auth. Role `nathan` uses peer auth and gets `SELECT` only, including on future tables (`ALTER DEFAULT PRIVILEGES`).
- Done when: as `nathan`, `psql jarvis -c 'select 1'` works without a password; as `agent`, creating a table works; as `nathan`, an insert into it is refused. Write the commands into `scripts/setup_postgres.sh`, which is idempotent.

## J1.2 — Schema migrations

- New `internal/store/migrations/*.sql`, embedded and applied on startup by a tiny runner (a `schema_version` table).
- `jobs` has: id (primary key), kind, queue, title, source, priority, status, driver, model, use_local, created_at, claimed_at, started_at, finished_at, duration_ms, error, est_cost, actual_cost, input_tokens, output_tokens, branch, pr_url, depends_on `text[]`, doc, payload `jsonb`, output `text`, summary.
- `job_events` has: job_id, at, event, detail.
- Index status, started_at and queue.
- Done when: tests against a throwaway `jarvis_test` database migrate from empty, and a second run is a no-op.

## J1.3 — PostgreSQL job store

- `internal/store` implements `jobstore.Store`. `Claim` uses `SELECT … FOR UPDATE SKIP LOCKED`. Every state change also writes a `job_events` row. The connection comes from `DATABASE_URL` (default `postgres:///jarvis?host=/var/run/postgresql`).
- Done when: the J0.3 store tests pass against PostgreSQL too, including two concurrent claimers never getting the same job.

## J1.4 — Worker, enqueue and `jarvis-jobs` use the database

- Point them at the PostgreSQL store and stop writing `jobs.jsonl`.
- Done when: a job enqueued with `jarvis-enqueue` appears in `jobs` within 5s, and history survives a worker restart.

## J1.5 — Dashboard reads the database

- Done when: the dashboard shows full history no matter which user runs it.

## J1.6 — Backfill old records

- An idempotent import of `jobs.jsonl` (the last record per ID wins) and J0's `jobstore.json`. Rename each file to `*.imported` afterwards.
- Done when: a fixture import gives one row per unique ID with the latest status, and a second run adds nothing.

## J1.7 — Nightly backups

- A systemd timer runs `pg_dump jarvis` nightly to `/mnt/jarvis/backups/jarvis-db/` if it is mounted, else `~/backups/jarvis-db/`, keeping 14 dumps.
- Done when: `systemctl list-timers` shows it, and one manual run produced a dump that restores into `jarvis_test`.

## J1.8 — `docs/database.md`

- Cover how to connect, the tables, example queries (last 20 jobs, today's failures, cost per day, average duration per model), and backup and restore.
- Done when: every example query in the doc runs as `nathan`.
