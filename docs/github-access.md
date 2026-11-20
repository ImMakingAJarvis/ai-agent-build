# GitHub access model

Decision 2026-11-18 (Nathan, job `chat-1795025589-376f`): the repo stays
**public with forking allowed** — anyone may fork and open a PR. What is
locked down is **approval and merging**: only the owner (Nathan) and the
GitHub App `thisappismakingajarvis` may approve or merge PRs.

## Why external PRs cannot land

- The org has **no members** and the repo has **no collaborators or
  teams** (checked 2026-11-18). The collaborators list includes any team
  granted repo access (it appears as a `Team` entry), and the empty org
  members list rules out org-wide repo access. So the only accounts with
  **write access** to `ImMakingAJarvis/ai-agent-build` are the org owner
  and the app installation.
- GitHub only shows the approve button to users with write access, and a PR
  author **cannot approve their own PR**. Merging likewise requires write
  access. An external fork author has neither.

Therefore no external code can reach `main` without an approval or merge by
one of us.

## Invariants (do not break these)

- Do **not** add collaborators, teams, or org members with write access
  without an explicit decision; the write set must stay {Nathan, app}.
- Never approve or merge a PR made by anyone other than the two of us.
- The app's permission set (see "Verifying" below) deliberately has **no
  Administration** — the agent cannot change repo settings, visibility, or
  protection rules on its own.

## Pending: branch protection on `main` (Nathan, one-time)

Enforcement is currently de facto (no one else has write access). To make it
structural — surviving a future collaborator and blocking force pushes and
deletions — add a branch-protection rule in the repo UI:

Settings → Branches → **Add rule** (branch name `main`):

- **Require pull request reviews before merging**: 1 approval
  (leave "Require review from Code Owners", "Dismiss stale approvals", and
  "Require branches to be up to date before merging" off)
- **Restrict force pushes**: on
- **Restrict deletions**: on
- **Include administrators**: off (owner keeps bypass)

Alternative: grant the app the **Administration** repository permission
(org → app settings → Repository permissions → Administration: Read and
write → Save, then Grant new permissions on the installation
`settings/installations/173893356`) so the agent can create and manage the
rule via API instead.

## Verifying the access state

    GH_TOKEN=$(ghapp-token) gh api orgs/ImMakingAJarvis/members
    # expect [] (owner is never listed)
    GH_TOKEN=$(ghapp-token) gh api repos/ImMakingAJarvis/ai-agent-build/collaborators
    # expect [] (teams with repo access appear here as Team entries)
    GH_TOKEN=$(ghapp-token) gh api repos/ImMakingAJarvis/ai-agent-build
    # private:false, allow_forking:true, pull_request_creation_policy:"all"

The app's granted permissions (no `administration` expected) come from the
installation endpoint, which needs an app JWT rather than the installation
token — the `ghapp-token` script builds one internally.
