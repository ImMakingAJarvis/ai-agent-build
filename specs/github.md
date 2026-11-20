---
id: github
name: GitHub
summary: The repo (ImMakingAJarvis/ai-agent-build) — branches, PRs, the only CI (reel).
group: external
kind: external
status: external
ports: ["— (cloud)"]
docs: [docs/agent-claude.md]
depends_on: []
---

## Purpose

Where every change lands as a PR. The agent authenticates as a GitHub App
(`ghapp-token`, hourly tokens) and runs `gh` with `GH_TOKEN`.

## Interfaces

- `git push`, `gh pr create/view/merge`, `gh-pr-wait <n>`.

## Configuration

- App credentials on the box (never printed).

## Data

- Remote branches; notes-only branches `notes-*`.

## Deploy

Not deployed from this repo. CI: only `.github/workflows/reel-ci.yml`
(for `reel/**`) — the root module has no CI.

## Verify live

    GH_TOKEN=$(ghapp-token) gh pr list --state open

## Failure modes

- Pushing a workflow file needs the App's workflows permission at both the
  app and the installation level (2026-11-06).
- `gh pr edit` is broken here; PR bodies are patched through `gh api`.
