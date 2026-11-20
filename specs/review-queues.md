---
id: review-queues
name: PR review queues (reviewers + five-minute check)
summary: CPU-only cloud reviewers (sonnet, opus, fable) on the review.<model> queues, and the five-minute check that revives missing jobs.
group: agent
kind: container
status: active
compose: [review-sonnet, review-opus, review-fable, review-sweep]
binaries: [cmd/review-worker, cmd/review-sweep, cmd/rev-seed]
ports: ["— (no listener)"]
docs: [docs/pr-review-build-plan.md, docs/event-driven.md]
depends_on:
  - to: rabbitmq
    why: "consumes review.<model>; sweep publishes review and author jobs"
  - to: anthropic-api
    why: "the reviewers run the claude CLI against the API"
  - to: plane
    why: "review results and the sweep digest are posted as comments"
  - to: discord
    why: "the sweep's end of day digest (review-sweep only)"
  - to: github
    why: "worktrees, commits and pushes on the PR branch (reviewers only)"
---

## Purpose

The PR review queues build (docs/pr-review-build-plan.md). Three
CPU-only containers run the claude CLI against the Anthropic API and
review pull requests off `review.sonnet`, `review.opus`, `review.fable`
(prefetch 1, manual ack, nack without requeue on error). `review-sweep`
runs the five-minute check: it walks the review records, revives a
missing review or author job, and at 18:00 Los Angeles sends the end of
day digest. CPU-only on purpose — a reviewer must never load the 27B
onto the 4090; `TestComposeReviewNoGPU` in cmd/review-worker fails if a
review service block in docker-compose.yml gains a GPU.

## Interfaces

- No listeners. Reviewers consume their queue; the sweep publishes jobs
  (same publisher as the review request path) and appends revival log
  lines.
- `rev-seed` is the one-shot seeder for the REV Plane board; re-runs
  match the `plan-task: <id>` marker in a card's description and do not
  create second cards.
- Fable is the release gate: the worker refuses a held record until
  `!review-release` (chat side).

## Configuration

- `agent.env` (env_file): `ANTHROPIC_API_KEY`, the `PLANE_*` set.
  `review-sweep` also loads `discord.env` (`DISCORD_BOT_TOKEN`,
  `DISCORD_OWNER_ID`) — it reads the token from the environment.
- `RABBITMQ_URL`, `REVIEW_REPO=/repo`, `REVIEW_WORKTREE_BASE=/worktrees`
  (reviewers); `JARVIS_REVIEW_CFG=/config/review.yaml` (sweep).
- Per-model Plane keys live in the local secrets store
  (`reviewplane.KeyName`, `~/.local/share/jarvis/secrets`).
- The fable model id defaults to claude-fable-5-1; `AGENT_FABLE_MODEL`
  overrides it.

## Data

- Review records + revival log: `~/.local/share/jarvis/review/`
  (`pr-<n>.json`); a revival line in the log is a bug, not noise to
  delete.
- Worktrees: `/var/lib/jarvis/review-worktrees` (host dir, safe to clean
  between reviews) plus git worktree metadata under the repo's
  `.git/worktrees`.
- The sweep reads the agent job store `~/.local/share/jarvis/jobs`.

## Deploy

    docker compose build review-sonnet review-opus review-fable review-sweep
    docker compose up -d review-sonnet review-opus review-fable review-sweep

Confirm the running thing is the new one: fresh `Created` timestamps,
the log line `review-worker: consuming review.<model> as <model>
(prefetch=1, manual ack)`, and — per plan job 12a — `docker inspect`
shows an empty `HostConfig.DeviceRequests` on all four containers.

## Verify live

    docker logs --tail 50 jarvis-review-sonnet
    docker inspect jarvis-review-sonnet jarvis-review-opus jarvis-review-fable jarvis-review-sweep \
      --format '{{.Name}} {{json .HostConfig.DeviceRequests}}'   # all []

A quiet `review-sweep` logs nothing; a `review-sweep: pass:` line is an
error, and the 18:00 digest on Discord is the pass it was waiting for.

## Failure modes

- None yet: the services are defined but not brought up as of
  2026-11-19 (plan job 12 defers `docker compose up` to the merge).
