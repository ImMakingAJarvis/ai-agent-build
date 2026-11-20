---
cursor:
  subagentId: "bc-bfb08375-6148-5f0f-b51c-a5b5e6fca1e2"
---

# Build the PR review queues

You are the local model. This file is the spec. Do the jobs in order. Do not redesign them, do not add a job, and do not ask another model to plan this again.

## How this gets onto the build queue

Verified against `notes/agent/system.md` (2026-10-28) and `internal/dashboard/plans.go`. Nathan uploads this file on the queue website, Plans section, `POST /api/plans`. The form fields are `file`, `title`, `class`, and `priority`. The class menu defaults to **build**. That one upload creates one ready agent job:

- class `build` (the Build band)
- priority 10, unless he typed another number
- driver `local`
- source `plan`
- effort `high`
- id `plan-<slug>-<unix>`, same id as the directory the file was saved in

`plane-bridge` already watches Plane. It subscribes to issue and issue_comment webhooks, and on startup it rechecks every card. A comment does not schedule anything. The webhook handler in `internal/bridge/bridge.go` runs the trigger check only for an `issue` event. Comments are read later, and only when an epic is moved back to Planning for a re-plan.

What the watcher does schedule:

- An epic in Backlog with a `plan:<model>` label becomes a `plan_epic` job, class `background`, also published on the background queue.
- A card moved to Ready with a `model:<id>` label becomes a `run_task` on `jobs.gpu`, `jobs.cpu`, or `jobs.igpu`. The store row is class `background`. `task-runner` runs it. The agent-run build band does not.

Do not add a `plan:` label and do not move a card to Ready to get this work onto the build queue. That starts the other executor.

If your job id starts with `plan-` and the title starts with `Plan:`, you are the breakdown round. You do not write code and you do not open a pull request. You put the work on the build queue, then you stop.

The file is already on disk at the path in your job body. Read it. Then create these jobs, in this order. The id prefix is your own job id. Class is `build` on every one. Priority starts at 30 and drops by one, which is the recipe in `planJobInstructions`. Each job depends on the one before it, so they run one at a time. Paste the named section of this file into the body so the job stands on its own.

```bash
jarvis-jobs add -id "$ID-1"  -title "REV seed the Plane board"           -class build -priority 30 -effort high <<'EOF'
... Job 0 of the plan ...
EOF
jarvis-jobs add -id "$ID-2"  -title "REV review config"                  -class build -priority 29 -effort high -depends "$ID-1" <<'EOF'
... Job 1 ...
EOF
jarvis-jobs add -id "$ID-3"  -title "REV stamps and revival log"         -class build -priority 28 -effort high -depends "$ID-2" <<'EOF'
... Job 2 ...
EOF
jarvis-jobs add -id "$ID-4"  -title "REV review queues"                  -class build -priority 27 -effort high -depends "$ID-3" <<'EOF'
... Job 3 ...
EOF
jarvis-jobs add -id "$ID-5"  -title "REV request a review and hold Fable" -class build -priority 26 -effort high -depends "$ID-4" <<'EOF'
... Job 4 ...
EOF
jarvis-jobs add -id "$ID-6"  -title "REV Discord and full auto reviewer" -class build -priority 25 -effort high -depends "$ID-5" <<'EOF'
... Job 5 ...
EOF
jarvis-jobs add -id "$ID-7"  -title "REV review worker"                  -class build -priority 24 -effort high -depends "$ID-6" <<'EOF'
... Job 6 ...
EOF
jarvis-jobs add -id "$ID-8"  -title "REV commits on the pull request branch" -class build -priority 23 -effort high -depends "$ID-7" <<'EOF'
... Job 7 ...
EOF
jarvis-jobs add -id "$ID-9"  -title "REV merge gate"                     -class build -priority 22 -effort high -depends "$ID-8" <<'EOF'
... Job 8 ...
EOF
jarvis-jobs add -id "$ID-10" -title "REV full auto limits and risk"      -class build -priority 21 -effort high -depends "$ID-9" <<'EOF'
... Job 9 ...
EOF
jarvis-jobs add -id "$ID-11" -title "REV five minute check"              -class build -priority 20 -effort high -depends "$ID-10" <<'EOF'
... Job 10 ...
EOF
jarvis-jobs add -id "$ID-12" -title "REV Plane identity"                 -class build -priority 19 -effort high -depends "$ID-11" <<'EOF'
... Job 11 ...
EOF
jarvis-jobs add -id "$ID-13" -title "REV containers and the standing note" -class build -priority 18 -effort high -depends "$ID-12" <<'EOF'
... Job 12 ...
EOF
```

`$ID` is your job id. Do not use `-class background`, `-class fix`, or `-tags nightly`. A second add of the same id prints `exists`. If it does, do not delete the job and do not pick a new id.

Then do steps 4 and 5 of the upload recipe, which are already in your job body: one kanban epic for this plan, one kanban task per job you just added, and `jarvis-discord notify` with the ids. End the round. The child jobs do the code.

If you are one of those child jobs, you are not the breakdown round. Do your section. Do not create more jobs.

The decisions live in `docs/pr-review-queues.md` next to this file in the notes you were given. If that file and this one disagree, this one wins. The first code job copies this file into the repo at `docs/pr-review-build-plan.md`. Do not shorten it.

Work in `/home/agent/ai-agent-build`. Every code job uses the one branch `phase-pr-review-queues`, created by the first code job from current `origin/main`. Later jobs check that branch out and commit on it. Where a job below names a different branch, ignore that name. Do not open a pull request until the last job. The `-depends` chain is what orders the work.

Do not commit anything under `notes/`. Do not `docker start` or `docker stop` `jarvis-engine`. New processes go in Docker. Do not add a host systemd unit.

A job is done only when the acceptance command exits 0, `go build ./...`, `go vet ./...` and `go test ./...` pass, and `gh-pr-wait` reports the PR MERGEABLE. Then run `jarvis-board done <display-id> "<acceptance command> passed"`. If a live check needs Nathan, leave the card in **In progress** and say so. Do not mark it Done.

## Decisions already made, so you do not ask

These are the defaults. Do not change them unless Nathan edits `config/review.yaml` himself.

- A reviewer that changes code commits on the original pull request branch. There is no second pull request.
- Only the model holding the turn token pushes. A push sets that model's stamp to the new SHA and clears the other model's stamp.
- `main` updates only when both stamps name the current head, the merging turn did not push, and the merging model is listed in `merge_allow`. `merge_allow` starts empty, so no model merges to `main`. The turn asks Nathan instead.
- Fable does not start until Nathan releases that one job. Fable is never a fallback.
- Full auto names one reviewer, `!fullauto on sonnet`. Cloud review spend stops at $10 in a calendar month. At $10, leave the pull request waiting, comment on its Plane item, and send Nathan one Discord line. Do not switch reviewer.
- Two open pull requests is the cap while full auto is on. At the cap, do not start a job that would open another one.
- The author rates the pull request `note`, `medium-low`, `medium`, `high`, or `very high`. `note` may merge with no reviewer. Any other rating waits for review. A diff that touches a sensitive path below is `high` even if the author said `note`.
- Conversation is on Plane. GitHub is the diff, the commits, and the merge. Do not read or post GitHub comments.
- One local review queue. The Plane user is the model id from `config/models.yaml` that actually ran, never a user named Local.
- Three round-trips, then the token goes to Nathan. Ten open Plane points, then the same.
- A check every five minutes requeues a missing next job and writes a revival. A revival is a bug. At 6pm America/Los_Angeles, if the day's log is not empty, one Discord message and one Plane bug. The check never merges and never releases Fable.

## Plane rules, read twice

`plane-epic create` is the only way to add a project. Flags go before the title. The usage text printed by the tool is wrong about that.

Create the project and the cards in **Backlog**. Do not add a `plan:` label to the epic. That label queues `plan_epic`, and a second planner will replace these cards. Do not move the epic to **Plan approved**. Do not move a card to **Ready**. A card in Ready with no `model:` label is blocked by the bridge (`internal/bridge`, test `TestRunTaskNoModelLabelBlocks`). Leave the label off on purpose, so a mistake cannot start a second executor.

You do the work by reading this file. The cards are the checklist. Start a card with `jarvis-board state <id> "In progress" "starting <plan-task id>"`. Finish it with `jarvis-board done`.

Plane comments use `comment_html`. The display id `REV-4` selects the project. `jarvis-board` flags go before the positional args.

Sleep is built into the seeder. Do not write your own curl loop. Plane returns 429 if you create cards in a tight loop.

## Job 0 — Put the board on Plane

**Branch:** `phase-pr-review-queues`. Create it from `origin/main` if it does not exist.

**Do not:** add a `plan:` label, move any card to Ready, call the Plane API by hand, create a project with `CreateProject` from your own code.

### 0a. Copy this spec

Save this file, unchanged, at `docs/pr-review-build-plan.md`.

### 0b. Create the project

From the repo root:

```bash
go run ./cmd/plane-epic create \
  --identifier REV \
  --description "$(cat <<'EOF'
PR review queues. Spec: docs/pr-review-build-plan.md. Do not add a plan label. Do not move cards to Ready or the epic to Plan approved. The local model executes from that spec and marks cards Done itself.
EOF
)" \
  "PR review queues"
```

The title is the last argument. A re-run is a no-op if the project already exists. This restarts `plane-bridge`. That is expected. It is not the brain.

If the command fails because identifier `REV` is already a different project, stop and tell Nathan. Do not pick another identifier.

### 0c. Write the seeder

Create `cmd/rev-seed/main.go` with the program in the next section, byte for byte apart from formatting `gofmt` fixes. Then:

```bash
gofmt -w cmd/rev-seed/main.go
go run ./cmd/rev-seed
go run ./cmd/rev-seed
```

The first run prints `created:` greater than 0. The second prints `created: 0`. Both print `bad-state: 0` and `bad-label: 0` and exit 0.

`jarvis-board item REV-1` shows the epic in Backlog.

### 0d. Comment the defaults

```bash
jarvis-board comment REV-1 "Spec is docs/pr-review-build-plan.md. Cards stay in Backlog with no model label. merge_allow is empty. At 10 dollars of cloud review, wait for Nathan. Sensitive paths floor a note rating to high."
```

### 0e. Verify and stop

```bash
go build ./cmd/rev-seed/
go vet ./cmd/rev-seed/
```

Do not open a pull request in this job. Commit on `phase-pr-review-queues` and finish. The next job is already queued behind `-depends`. The spec file is on that branch for the later jobs. The last job opens the one pull request.

## The seeder

`cmd/rev-seed/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// One card. ParentID empty means the epic is the parent. DependsOn is
// another plan-task id. The seeder is the only creator. Re-runs match
// the plan-task marker and do not create a second card.
type card struct {
	ID, Name, What, Files, Accept, ParentID, DependsOn string
}

func cards() []card {
	return []card{
		{ID: "rev-1", Name: "1. Review config", What: "Tracker only. Mark Done after rev-1a and rev-1b are Done.", Files: "config/review.yaml, internal/reviewcfg", Accept: "go test ./internal/reviewcfg/", ParentID: "", DependsOn: ""},
		{ID: "rev-1a", Name: "1a. Write config/review.yaml", What: "Write the file in Job 1a of docs/pr-review-build-plan.md. Do not add a model to merge_allow.", Files: "config/review.yaml", Accept: "test -f config/review.yaml", ParentID: "rev-1", DependsOn: "rev-1"},
		{ID: "rev-1b", Name: "1b. Load the review config", What: "Job 1b of docs/pr-review-build-plan.md.", Files: "internal/reviewcfg", Accept: "go test ./internal/reviewcfg/", ParentID: "rev-1", DependsOn: "rev-1a"},

		{ID: "rev-2", Name: "2. Stamps and revival log", What: "Tracker only. Mark Done after rev-2a, rev-2b, and rev-2c are Done.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/", ParentID: "", DependsOn: "rev-1b"},
		{ID: "rev-2a", Name: "2a. File store for one pull request", What: "Job 2a of docs/pr-review-build-plan.md.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/ -run TestStoreRoundTrip", ParentID: "rev-2", DependsOn: "rev-2"},
		{ID: "rev-2b", Name: "2b. Stamp rules", What: "Job 2b of docs/pr-review-build-plan.md. Copy the test table. Do not invent cases.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/ -run TestStamp", ParentID: "rev-2", DependsOn: "rev-2a"},
		{ID: "rev-2c", Name: "2c. Revival log", What: "Job 2c of docs/pr-review-build-plan.md.", Files: "internal/reviewstate", Accept: "go test ./internal/reviewstate/ -run TestRevival", ParentID: "rev-2", DependsOn: "rev-2b"},

		{ID: "rev-3", Name: "3. Review queues", What: "Tracker only. Mark Done after rev-3a, rev-3b, and rev-3c are Done.", Files: "rabbitmq/definitions.json, scripts/jarvis-review.sh, internal/reviewroute", Accept: "go test ./internal/reviewroute/", ParentID: "", DependsOn: "rev-2c"},
		{ID: "rev-3a", Name: "3a. Declare the four queues", What: "Job 3a of docs/pr-review-build-plan.md.", Files: "rabbitmq/definitions.json, internal/queue", Accept: "go test ./internal/queue/ -count=1", ParentID: "rev-3", DependsOn: "rev-3"},
		{ID: "rev-3b", Name: "3b. Stop using priority 100", What: "Job 3b of docs/pr-review-build-plan.md.", Files: "scripts/jarvis-review.sh", Accept: "go test ./internal/reviewroute/", ParentID: "rev-3", DependsOn: "rev-3a"},
		{ID: "rev-3c", Name: "3c. Queue name tests", What: "Job 3c of docs/pr-review-build-plan.md. Copy the test rows.", Files: "internal/reviewroute", Accept: "go test ./internal/reviewroute/ -run TestQueue", ParentID: "rev-3", DependsOn: "rev-3b"},

		{ID: "rev-4", Name: "4. Request a review, hold Fable", What: "Tracker only. Mark Done after rev-4a and rev-4b are Done.", Files: "internal/reviewrequest", Accept: "go test ./internal/reviewrequest/", ParentID: "", DependsOn: "rev-3c"},
		{ID: "rev-4a", Name: "4a. RequestReview", What: "Job 4a of docs/pr-review-build-plan.md.", Files: "internal/reviewrequest", Accept: "go test ./internal/reviewrequest/ -run TestRequest", ParentID: "rev-4", DependsOn: "rev-4"},
		{ID: "rev-4b", Name: "4b. Fable stays held", What: "Job 4b of docs/pr-review-build-plan.md.", Files: "internal/reviewrequest", Accept: "go test ./internal/reviewrequest/ -run TestFable", ParentID: "rev-4", DependsOn: "rev-4a"},

		{ID: "rev-5", Name: "5. Discord and full auto reviewer", What: "Tracker only. Mark Done after rev-5a and rev-5b are Done.", Files: "cmd/jarvis-chat", Accept: "go test ./cmd/jarvis-chat/ -count=1", ParentID: "", DependsOn: "rev-4b"},
		{ID: "rev-5a", Name: "5a. !review", What: "Job 5a of docs/pr-review-build-plan.md.", Files: "cmd/jarvis-chat/cmdparse.go", Accept: "go test ./cmd/jarvis-chat/ -run TestReviewCmd -count=1", ParentID: "rev-5", DependsOn: "rev-5"},
		{ID: "rev-5b", Name: "5b. !fullauto on names the reviewer", What: "Job 5b of docs/pr-review-build-plan.md.", Files: "cmd/jarvis-chat/fullauto.go", Accept: "go test ./cmd/jarvis-chat/ -run TestFullAuto -count=1", ParentID: "rev-5", DependsOn: "rev-5a"},

		{ID: "rev-6", Name: "6. Review worker", What: "Tracker only. Mark Done after rev-6a, rev-6b, and rev-6c are Done.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/", ParentID: "", DependsOn: "rev-5b"},
		{ID: "rev-6a", Name: "6a. Consume one queue", What: "Job 6a of docs/pr-review-build-plan.md.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/ -run TestConsume", ParentID: "rev-6", DependsOn: "rev-6"},
		{ID: "rev-6b", Name: "6b. Worktree and the Go gate", What: "Job 6b of docs/pr-review-build-plan.md.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/ -run TestGate", ParentID: "rev-6", DependsOn: "rev-6a"},
		{ID: "rev-6c", Name: "6c. Claude, and no GPU", What: "Job 6c of docs/pr-review-build-plan.md.", Files: "cmd/review-worker", Accept: "go test ./cmd/review-worker/ -run TestNoGPU", ParentID: "rev-6", DependsOn: "rev-6b"},

		{ID: "rev-7", Name: "7. Commits on the pull request branch", What: "Tracker only. Mark Done after rev-7a, rev-7b, and rev-7c are Done.", Files: "internal/reviewstate, internal/reviewturn", Accept: "go test ./internal/reviewturn/", ParentID: "", DependsOn: "rev-6c"},
		{ID: "rev-7a", Name: "7a. Only the token holder pushes", What: "Job 7a of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestToken", ParentID: "rev-7", DependsOn: "rev-7"},
		{ID: "rev-7b", Name: "7b. A push updates stamps", What: "Job 7b of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestPush", ParentID: "rev-7", DependsOn: "rev-7a"},
		{ID: "rev-7c", Name: "7c. Author response job", What: "Job 7c of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestAuthorJob", ParentID: "rev-7", DependsOn: "rev-7b"},

		{ID: "rev-8", Name: "8. Merge gate", What: "Tracker only. Mark Done after rev-8a and rev-8b are Done.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestMerge", ParentID: "", DependsOn: "rev-7c"},
		{ID: "rev-8a", Name: "8a. Both stamps, and this turn did not push", What: "Job 8a of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestMerge -count=1", ParentID: "rev-8", DependsOn: "rev-8"},
		{ID: "rev-8b", Name: "8b. Empty allow list asks Nathan", What: "Job 8b of docs/pr-review-build-plan.md.", Files: "internal/reviewturn", Accept: "go test ./internal/reviewturn/ -run TestMergeAllow -count=1", ParentID: "rev-8", DependsOn: "rev-8a"},

		{ID: "rev-9", Name: "9. Full auto limits and the risk rating", What: "Tracker only. Mark Done after rev-9a, rev-9b, and rev-9c are Done.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/", ParentID: "", DependsOn: "rev-8b"},
		{ID: "rev-9a", Name: "9a. Two open pull requests", What: "Job 9a of docs/pr-review-build-plan.md.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/ -run TestOpenCap", ParentID: "rev-9", DependsOn: "rev-9"},
		{ID: "rev-9b", Name: "9b. Ten dollar month", What: "Job 9b of docs/pr-review-build-plan.md.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/ -run TestSpend", ParentID: "rev-9", DependsOn: "rev-9a"},
		{ID: "rev-9c", Name: "9c. Risk rating and the sensitive floor", What: "Job 9c of docs/pr-review-build-plan.md. Copy the path list.", Files: "internal/reviewauto", Accept: "go test ./internal/reviewauto/ -run TestRisk", ParentID: "rev-9", DependsOn: "rev-9b"},

		{ID: "rev-10", Name: "10. Five minute check", What: "Tracker only. Mark Done after rev-10a, rev-10b, and rev-10c are Done.", Files: "internal/reviewsweep, cmd/review-sweep", Accept: "go test ./internal/reviewsweep/", ParentID: "", DependsOn: "rev-9c"},
		{ID: "rev-10a", Name: "10a. Next action table", What: "Job 10a of docs/pr-review-build-plan.md. Copy the table into tests.", Files: "internal/reviewsweep", Accept: "go test ./internal/reviewsweep/ -run TestNext", ParentID: "rev-10", DependsOn: "rev-10"},
		{ID: "rev-10b", Name: "10b. The loop", What: "Job 10b of docs/pr-review-build-plan.md.", Files: "cmd/review-sweep", Accept: "go test ./internal/reviewsweep/ -run TestSweep", ParentID: "rev-10", DependsOn: "rev-10a"},
		{ID: "rev-10c", Name: "10c. End of day", What: "Job 10c of docs/pr-review-build-plan.md.", Files: "internal/reviewsweep", Accept: "go test ./internal/reviewsweep/ -run TestDigest", ParentID: "rev-10", DependsOn: "rev-10b"},

		{ID: "rev-11", Name: "11. Plane identity", What: "Tracker only. Mark Done after rev-11a and rev-11b are Done. The Plane keys are already in the secret manager.", Files: "internal/reviewplane", Accept: "go test ./internal/reviewplane/", ParentID: "", DependsOn: "rev-10c"},
		{ID: "rev-11a", Name: "11a. One key per model", What: "Job 11a of docs/pr-review-build-plan.md. The keys are already stored. List names only. Never print a value.", Files: "internal/reviewplane", Accept: "go test ./internal/reviewplane/ -run TestKeys", ParentID: "rev-11", DependsOn: "rev-11"},
		{ID: "rev-11b", Name: "11b. Route Nathan's replies", What: "Job 11b of docs/pr-review-build-plan.md.", Files: "internal/reviewplane", Accept: "go test ./internal/reviewplane/ -run TestRoute", ParentID: "rev-11", DependsOn: "rev-11a"},

		{ID: "rev-12", Name: "12. Containers and the standing note", What: "Tracker only. Mark Done after rev-12a and rev-12b are Done.", Files: "docker-compose.yml, docs/agent-claude.md", Accept: "go test ./cmd/review-worker/ ./cmd/review-sweep/", ParentID: "", DependsOn: "rev-11b"},
		{ID: "rev-12a", Name: "12a. Compose services with no GPU", What: "Job 12a of docs/pr-review-build-plan.md. Do not start jarvis-engine.", Files: "docker-compose.yml", Accept: "go test ./cmd/review-worker/ -run TestNoGPU", ParentID: "rev-12", DependsOn: "rev-12"},
		{ID: "rev-12b", Name: "12b. Definition of done", What: "Job 12b of docs/pr-review-build-plan.md.", Files: "docs/agent-claude.md", Accept: "test -f docs/agent-claude.md", ParentID: "rev-12", DependsOn: "rev-12a"},
	}
}

func main() {
	c, err := plane.FromEnv()
	if err != nil {
		die(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	projects, err := c.ListProjects(ctx)
	if err != nil {
		die(err)
	}
	var project *plane.Project
	for i := range projects {
		if strings.EqualFold(projects[i].Identifier, "REV") {
			project = &projects[i]
			break
		}
	}
	if project == nil {
		die(fmt.Errorf("no Plane project REV; run plane-epic first"))
	}
	items, err := c.ListWorkItems(ctx, project.ID)
	if err != nil {
		die(err)
	}
	var epic *plane.WorkItem
	for i := range items {
		if items[i].Parent == "" && items[i].Name == "PR review queues" {
			epic = &items[i]
			break
		}
	}
	if epic == nil {
		die(fmt.Errorf("REV has no epic named %q", "PR review queues"))
	}
	backlog, err := c.StateByName(ctx, project.ID, "Backlog")
	if err != nil {
		die(err)
	}

	byMarker := map[string]plane.WorkItem{}
	for _, it := range items {
		if id, ok := marker(it.DescriptionHTML); ok {
			byMarker[id] = it
		}
	}

	created, existed := 0, 0
	for _, cd := range cards() {
		if _, ok := byMarker[cd.ID]; ok {
			existed++
			continue
		}
		parent := epic.ID
		if cd.ParentID != "" {
			p, ok := byMarker[cd.ParentID]
			if !ok {
				die(fmt.Errorf("card %s: parent %s not created yet", cd.ID, cd.ParentID))
			}
			parent = p.ID
		}
		wi, err := c.CreateWorkItem(ctx, project.ID, plane.WorkItemCreate{
			Name:            cd.Name,
			DescriptionHTML: cardHTML(cd),
			Parent:          parent,
			State:           backlog.ID,
		})
		if err != nil {
			die(fmt.Errorf("create %s: %w", cd.ID, err))
		}
		byMarker[cd.ID] = *wi
		created++
		fmt.Printf("created %s-%d %s\n", project.Identifier, wi.SequenceID, cd.ID)
		time.Sleep(4 * time.Second)
	}

	relations := 0
	for _, cd := range cards() {
		if cd.DependsOn == "" {
			continue
		}
		self := byMarker[cd.ID]
		dep := byMarker[cd.DependsOn]
		rel, err := c.Relations(ctx, project.ID, self.ID)
		if err != nil {
			die(err)
		}
		if hasBlock(rel, dep.ID) {
			continue
		}
		if err := c.AddBlockedBy(ctx, project.ID, self.ID, dep.ID); err != nil {
			die(fmt.Errorf("blocked-by %s: %w", cd.ID, err))
		}
		relations++
		time.Sleep(4 * time.Second)
	}

	// Re-list so a card created earlier in this run is checked too.
	items, err = c.ListWorkItems(ctx, project.ID)
	if err != nil {
		die(err)
	}
	badState, badLabel := 0, 0
	for _, it := range items {
		id, ok := marker(it.DescriptionHTML)
		if !ok {
			continue
		}
		if it.State != backlog.ID {
			fmt.Fprintf(os.Stderr, "bad state %s\n", id)
			badState++
		}
		for _, lid := range it.Labels {
			// Labels are IDs. Any label on a seeded card is wrong:
			// no plan: and no model:.
			fmt.Fprintf(os.Stderr, "label on %s: %s\n", id, lid)
			badLabel++
		}
	}
	fmt.Printf("created: %d existed: %d relations: %d bad-state: %d bad-label: %d\n",
		created, existed, relations, badState, badLabel)
	if badState > 0 || badLabel > 0 {
		os.Exit(1)
	}
}

func cardHTML(cd card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>plan-task: %s</p>\n", cd.ID)
	fmt.Fprintf(&b, "<p><strong>What to build:</strong> %s</p>\n", html.EscapeString(cd.What))
	fmt.Fprintf(&b, "<p><strong>Files:</strong> %s</p>\n", html.EscapeString(cd.Files))
	fmt.Fprintf(&b, "<p><strong>Acceptance check:</strong> %s</p>\n", html.EscapeString(cd.Accept))
	b.WriteString("<p><strong>Size:</strong> S · <strong>Executor:</strong> follow docs/pr-review-build-plan.md</p>")
	return b.String()
}

func marker(desc string) (string, bool) {
	const p = "plan-task: "
	i := strings.Index(desc, p)
	if i < 0 {
		return "", false
	}
	j := i + len(p)
	for j < len(desc) {
		c := desc[j]
		if c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			j++
			continue
		}
		break
	}
	if j == i+len(p) {
		return "", false
	}
	return desc[i+len(p) : j], true
}

func hasBlock(r *plane.Relations, id string) bool {
	if r == nil {
		return false
	}
	for _, b := range r.BlockedBy {
		if b.IssueID == id {
			return true
		}
	}
	return false
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "rev-seed:", err)
	os.Exit(1)
}
```

## Job 1 — Config

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `config/models.yaml` (the field comments at the top), `internal/registry`.

### 1a. Write `config/review.yaml`

Create the file with exactly these keys. Values:

```yaml
# Review queues. Loaded by internal/reviewcfg.
month_cap_usd: 10
max_open_prs: 2
max_rounds: 3
max_open_points: 10
# Empty until Nathan adds a model id. No model merges to main while this is empty.
merge_allow: []
# Author response jobs use class build at this priority (higher runs first
# inside the build band, and every fix still runs before any build).
author_job_class: build
author_job_priority: 500
prices_usd_per_mtok:
  sonnet: 3
  opus: 15
  fable: 10
reviewers:
  sonnet:
    queue: review.sonnet
    kind: cloud
    needs_release: false
  opus:
    queue: review.opus
    kind: cloud
    needs_release: false
  fable:
    queue: review.fable
    kind: cloud
    needs_release: true
  qwen3.8-27b:
    queue: review.local
    kind: local
    needs_release: false
sensitive_paths:
  - cmd/resourced/
  - internal/streamwatcher/
  - scripts/jarvis-watchdog
  - scripts/voice_service.py
  - scripts/jarvis-voice.service
  - internal/dashboard/
  - cmd/dashboard/
  - internal/secrets/
  - scripts/jarvis-secrets
  - docker-compose.yml
  - scripts/agent-run
```

`opus` at 15 is a placeholder input price so the cap cannot be bypassed by a zero. When you find the real figure in `config/models.yaml` or the Anthropic price Nathan already wrote down, use that number and say what you used in the PR. Do not leave opus at 0.

### 1b. Load it

Package `internal/reviewcfg`. `Load(path string) (Config, error)`.

Reject, with the key name in the error:

- missing file
- `month_cap_usd` less than or equal to 0
- `max_open_prs` less than 1
- `max_rounds` less than 1
- `max_open_points` less than 1
- a reviewer with an empty queue
- two reviewers with the same queue
- `author_job_class` not `build`
- a sensitive path that is empty

Tests use `t.TempDir()` and a written yaml file. No network. Table:

| file contents | want |
|---|---|
| the Job 1a file | nil error, cap 10, four reviewers, fable `needs_release` true, `merge_allow` length 0 |
| `month_cap_usd: 0` | error contains `month_cap_usd` |
| two reviewers both `queue: review.sonnet` | error contains `queue` |
| `merge_allow: [sonnet]` | nil error, allow list is sonnet only |

Mark card `rev-1a` Done after the file exists. Mark `rev-1b` and `rev-1` Done after `go test ./internal/reviewcfg/` exits 0.

## Job 2 — Stamps

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `internal/agentjob` only far enough to see that jobs are JSON files on disk. Do the same for review state: one JSON file per pull request number under a directory the test sets with `t.TempDir()`. Default directory `~/.local/share/jarvis/review/`. Do not use the job directory.

### 2a. Store

`internal/reviewstate`. Type `Record`:

- `PR` int
- `Branch` string
- `Head` string, the current SHA
- `AuthorModel` string
- `ReviewerModel` string
- `Token` string, either the author model id or the reviewer model id
- `AuthorStamp` string, a SHA or empty
- `ReviewerStamp` string, a SHA or empty
- `Rounds` int
- `OpenPoints` int
- `PlaneItem` string, a display id like `REV-4`
- `Held` bool, Fable not released
- `TurnPushed` bool, the current turn already pushed

`Save` and `Load`. A missing file is not an error from `Load`; it returns a zero record and `os.ErrNotExist`, which the caller can see with `errors.Is`. `Save` writes atomically (temp file in the same directory, then rename) and the file mode is `0600`.

`TestStoreRoundTrip` saves a record, loads it, and checks every field.

### 2b. Stamp rules

Function `ApplyPush(r Record, pusher, newSHA string) (Record, error)`.

- If `pusher` is not `r.Token`, return the record unchanged and an error that contains `token`.
- If `pusher` is the author, set `AuthorStamp` to `newSHA`, clear `ReviewerStamp`, set `Head` to `newSHA`, set `TurnPushed` true.
- If `pusher` is the reviewer, set `ReviewerStamp` to `newSHA`, clear `AuthorStamp`, set `Head` to `newSHA`, set `TurnPushed` true.
- Any other pusher is an error that contains `pusher`.

Function `ApplyStamp(r Record, who, sha string) (Record, error)`. This is the no-push LGTM.

- If `who` is not `r.Token`, error contains `token`.
- If `r.TurnPushed` is true, error contains `pushed`. A turn that already pushed cannot also stamp.
- If `sha` is not `r.Head`, error contains `head`.
- Author sets `AuthorStamp`. Reviewer sets `ReviewerStamp`.

Function `PassToken(r Record) Record` clears `TurnPushed` and swaps `Token` to the other of the two model ids.

Copy this test table into `TestStamp`. Do not drop a row.

| start | call | result |
|---|---|---|
| token author, head `aaa`, both stamps `aaa` | `ApplyPush` by author to `bbb` | author stamp `bbb`, reviewer stamp empty, head `bbb`, TurnPushed true |
| token reviewer, head `aaa` | `ApplyPush` by reviewer to `ccc` | reviewer stamp `ccc`, author stamp empty, head `ccc` |
| token author | `ApplyPush` by reviewer | error contains `token`, record unchanged |
| token author, TurnPushed false, head `bbb`, author stamp empty | `ApplyStamp` author `bbb` | author stamp `bbb` |
| token reviewer, TurnPushed true | `ApplyStamp` reviewer `bbb` | error contains `pushed` |
| token author, head `bbb` | `ApplyStamp` author `aaa` | error contains `head` |
| token author, TurnPushed true | `PassToken` | token is the reviewer, TurnPushed false |

Opening a pull request is not `ApplyPush`. The caller sets `AuthorStamp` and `Head` to the same SHA when the record is first saved, because the author wrote that commit.

### 2c. Revival log

`AppendRevival(dir, Revival) error` appends one JSON object per line to `dir/revivals.jsonl`, mode `0600`. `Revival` has `At` (`time.Time`, UTC), `PR` int, `State` string, `Enqueued` string.

`RevivalsOn(dir string, day time.Time) ([]Revival, error)` returns lines whose `At` falls on that calendar day in `America/Los_Angeles`. A line from 07:30 UTC is the previous evening in Los Angeles during standard time (UTC-8). Test that: a stamp at `2026-11-20T07:30:00Z` is included in the Los Angeles day `2026-11-19`, and not in `2026-11-20`.

`TestRevival` covers append, a second append, and that day split.

## Job 3 — Queues

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `rabbitmq/definitions.json`, `internal/queue/message.go` around the queue name constants, `scripts/jarvis-review.sh` lines that enqueue with `-class fix -priority 100`.

### 3a. Declare the queues

Add four queues next to the existing ones in `rabbitmq/definitions.json`: `review.sonnet`, `review.opus`, `review.fable`, `review.local`. Copy the durability, the dead-letter settings, and the binding style of the `fix` queue. Do not give them priority 100. Do not bind them to the agent-run consumer. Nothing should consume them yet. Job 6 does that.

Add matching constants in `internal/queue`. A test in that package fails if any of the four names is missing from the definitions file. Parse the JSON. Do not shell out to a broker.

### 3b. Get reviews off the fix band

`scripts/jarvis-review.sh` today enqueues `jarvis-jobs add -driver ... -class fix -priority 100`. That line is the thrash. Delete it.

The script calls a small Go helper or `go run` is too heavy. Put the mapping in `internal/reviewroute`:

```text
Queue(reviewer string) (string, error)
```

| reviewer | queue |
|---|---|
| `sonnet` | `review.sonnet` |
| `opus` | `review.opus` |
| `planner` | `review.opus` |
| `fable` | `review.fable` |
| `local` | `review.local` |
| `qwen3.8-27b` | `review.local` |
| `qwen3.8-flash-next` | `review.local` |
| `glm5.3-flash` | `review.local` |
| anything else | error contains `reviewer` |

`planner` stays as an alias because `jarvis-review.sh` already accepts it. New local models are `review.local` when their id is in `config/review.yaml` under `kind: local`. The function takes the loaded config, not a hard-coded list, so the table above is the test fixture's config. The three local ids are in the fixture. An unknown id errors.

The shell script publishes to that queue with `jarvis-mq`. Read `jarvis-mq publish` in `cmd/jarvis-mq/main.go` and use the flags it already has. Do not invent a flag. The message names the pull request number, the branch, and the reviewer. It does not contain a prompt from a GitHub comment.

Grep the script after the edit. These strings must be absent: `-priority 100`, `-class fix`.

### 3c. Tests

`TestQueue` is the table in 3b, plus one row that a local id not in the config returns an error.

## Job 4 — Request and hold

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `scripts/jarvis-review.sh` from the top through the enqueue, `internal/reviewstate`, `internal/reviewroute`.

### 4a. `RequestReview`

`internal/reviewrequest`.

```text
RequestReview(ctx, pr int, branch, reviewer, planeItem string) (jobID string, held bool, err error)
```

Behavior, with a fake publisher in the test (a function field, no broker):

- Unknown reviewer returns the `reviewroute` error and publishes nothing.
- Known cloud reviewer other than Fable publishes exactly one message to that reviewer's queue and returns `held false`.
- The message body has the PR number and the branch. Assert both in the test.
- Calling it twice for the same PR and reviewer does not publish a second message when a record already exists with that reviewer and `Held` false and the head is unchanged. Test this.

Save the `reviewstate` record. Token starts as the reviewer. `AuthorStamp` stays as it was if the record existed; on a new record leave stamps empty until the author job sets them. Do not guess the SHA in this package. The caller passes `head` and you store it.

### 4b. Fable

If the reviewer's `needs_release` is true:

- Do not publish.
- Save the record with `Held` true.
- Return `held true` and a job id you generate, stable for that PR: `review-held-<pr>-fable`.
- `Release(pr int) error` publishes the one message and sets `Held` false.
- `Release` on a record that is not held returns an error that contains `held` and publishes nothing.
- A second `Release` publishes nothing. Test that.

Nothing in this package starts Fable because Sonnet is busy. There is no fallback function. Do not write one.

## Job 5 — Discord

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `cmd/jarvis-chat/fullauto.go` and `cmd/jarvis-chat/cmdparse.go`. Match the style of `cutFullAuto`. Owner-only is already true because the chat hook only runs for Nathan's DM. Do not add a second permission check.

### 5a. `!review`

`cutReview(content string) (pr int, reviewer string, valid, isCommand bool)`.

| message | isCommand | valid | pr | reviewer |
|---|---|---|---|---|
| `!review 268 sonnet` | true | true | 268 | sonnet |
| `!review 268 SONNET` | true | true | 268 | sonnet |
| `!review` | true | false |  |  |
| `!review 268` | true | false |  |  |
| `!review abc sonnet` | true | false |  |  |
| `hello !review 268 sonnet` | false | false |  |  |
| `!reviewx 268 sonnet` | false | false |  |  |

Wire the valid command to `RequestReview`. On `held true`, the reply to Nathan is `Fable review for PR 268 is held. Release it with !review-release 268`. On success, the reply names the queue. On error, the reply is the error text and nothing is published.

`cutReviewRelease` accepts only `!review-release <number>`. Wire it to `Release`.

### 5b. Full auto names the reviewer

Today `cutFullAuto` accepts only `on` and `off`. Change it so:

| message | result |
|---|---|
| `!fullauto on sonnet` | on, reviewer `sonnet`, valid |
| `!fullauto on` | isCommand true, valid false |
| `!fullauto off` | off, valid, reviewer empty |
| `!fullauto on extra extra` | isCommand true, valid false |
| `!fullauto maybe` | isCommand true, valid false |

Store the reviewer next to the existing full-auto flag. Read `internal/nobrakes` before you add a key. Use a new key `full_auto_reviewer`. Do not overload the on/off flag with the model name.

`!fullauto off` clears the reviewer key too.

Reject a reviewer the config does not know. The reply says the name is unknown and does not turn full auto on.

Update `fullauto_test.go`. The old rows that expect `!fullauto on` to be valid must change: bare `on` is now invalid. Do that on purpose. Say so in the PR.

## Job 6 — The worker

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `internal/agentjob/run.go` function `defaultCommand` and `ModelFor`. Sonnet and Opus already run `claude --model <ModelFor(driver)>`. Use that. Do not invent a model string. Fable uses `claude-fable-5-1` unless `AGENT_FABLE_MODEL` is set.

### 6a. Consume

`cmd/review-worker` flags: `-queue` (required), `-model` (required). It consumes one queue, prefetch 1, manual ack. A test uses a fake consumer that delivers one message and records the ack. A second message is not taken until the first is acked. A panic or a returned error nacks without requeue. Test the nack.

The message from Job 4 is the only body it understands. A body without a PR number is acked and logged, not retried. Test that, so a poison message cannot loop.

### 6b. Worktree and the gate

Check out the pull request branch in a new worktree under `t.TempDir()` in tests. Use a fake `git` only if the real `git` worktree needs a repo: for the unit test, put a stub script named `git` on `PATH` via `t.Setenv`. The stub records its args. Assert it was called with `worktree add`.

The gate command, in the worktree, is `go test ./...` and `go vet ./...`. The worker runs them. A non-zero exit is a failed review, not a crash: ack the message, and the result struct says `gate` failed and includes the combined output trimmed to 4000 bytes. Test the trim with a 5000 byte fake output.

Do not run the gate against the agent's live checkout.

### 6c. Claude, and no GPU

The cloud path builds the command the same way `defaultCommand` does for `sonnet` and `opus`: binary `claude`, `--model` from `ModelFor`, `--dangerously-skip-permissions`, and `-p` with a prompt you assemble from the pull request number, the branch, and the Plane item id. The prompt tells the model to comment on Plane and, if it changes code, to commit on that branch and not to open a new pull request. The prompt does not tell it to merge.

`TestNoGPU` asserts the command's environment has `CUDA_VISIBLE_DEVICES` empty or unset, and that the args do not contain a GPU index. Local reviews are not this process. `review.local` is rejected by this binary with an error that contains `local`. The local queue is consumed by the existing agent-run loop in a later job, in batches. This binary must refuse it so a container cannot load the 27B.

Fable: if the record is `Held`, the worker does not call `claude`. It acks and returns. Test with a held record. Release happens only through `!review-release`.

## Job 7 — Commits

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** Job 2b, `internal/agentjob/store.go` around priority (higher number runs first inside a class), `internal/agentjob/order.go` (`DefaultBandOrder` is fix, then build, then background).

### 7a. Token

`internal/reviewturn`. `AllowPush(r reviewstate.Record, pusher string) error`.

- Pusher is the token: nil error.
- Pusher is the other model: error contains `token`.
- Token empty: error contains `token`.

The worker calls this before `git push`. A failed check does not push. Test that the push function is not called: pass a `push func() error` and assert it was not invoked.

### 7b. Push updates stamps

`NotePush(r, pusher, sha string) (reviewstate.Record, error)` calls `ApplyPush` and then `PassToken`. After a successful push the token is the other model and `TurnPushed` is false, because the turn is over. Test: author holds the token, push, result token is the reviewer, author stamp is the new SHA, reviewer stamp is empty.

### 7c. Author job

`AuthorJob(r reviewstate.Record) agentjob.Spec` (use the real `Spec` type; read its fields and set the ones that exist).

- Class is `build`. Not `fix`.
- Priority is 500.
- The title contains the PR number.
- The instructions tell the author to read the commits added since its last stamp, reply on the Plane item, and either stamp the head with no push or push a correction. They tell it not to merge to `main`.

Test those four. Do not add a new job class. A new class would have to be threaded through `DefaultBandOrder`, `SetBandOrder`, and the dashboard, and a 500-priority build already sits above ordinary builds and below every fix.

## Job 8 — Merge

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

### 8a. When a merge is allowed

`Decide(r reviewstate.Record, actor string, allow []string, turnPushed bool) Decision`.

`Decision` is one of `merge`, `ask`, `refuse`, plus a `Reason` string.

Copy these rows into `TestMerge`. `allow` is `[]string{"sonnet"}` unless the row says otherwise.

| record | actor | turnPushed | allow | decision | reason contains |
|---|---|---|---|---|---|
| head `abc`, both stamps `abc`, token reviewer, reviewer is sonnet | sonnet | false | sonnet | merge | |
| head `abc`, author stamp `abc`, reviewer stamp empty | sonnet | false | sonnet | refuse | `stamp` |
| head `abc`, both stamps `abc` | sonnet | true | sonnet | refuse | `pushed` |
| head `abc`, both stamps `abd` | sonnet | false | sonnet | refuse | `head` |
| head `abc`, both stamps `abc`, token author | sonnet | false | sonnet | refuse | `token` |
| head `abc`, both stamps `abc` | sonnet | false | empty | ask | `allow` |
| head `abc`, both stamps `abc`, reviewer is fable | fable | false | fable | ask | `release` when the record is Held, otherwise merge only if not Held |

The Held row: `Held` true always returns `ask` with reason containing `held`, even if fable is on the allow list. Test that as its own row.

`refuse` means the turn does not merge and does not ask Nathan, because the stamps are simply not ready. The next model still gets the token from the caller. `ask` means a Plane comment to Nathan and no merge.

### 8b. Empty allow list

`TestMergeAllow` loads `config/review.yaml` from the repo (skip the test if the file is absent, with `t.Skip`) and asserts `merge_allow` is empty. Also unit-test `Decide` with `allow` nil and expect `ask`.

Do not add `sonnet` to the yaml in this job.

## Job 9 — Full auto limits

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `cmd/jarvis-chat/fullauto.go` after Job 5, and `config/review.yaml`.

### 9a. Two open pull requests

`internal/reviewauto`. `CanOpen(openPRs int, cap int, fullAuto bool) bool`.

| open | cap | fullAuto | result |
|---|---|---|---|
| 0 | 2 | true | true |
| 2 | 2 | true | false |
| 3 | 2 | true | false |
| 5 | 2 | false | true |

When full auto is off, the cap does not apply. When it is on and the cap is hit, the caller does not start a job whose result is a new pull request. A job that opens nothing still runs. Encode that as `CanStart(opensPR bool, openPRs, cap int, fullAuto bool) bool`, and test `opensPR false` at the cap returns true.

### 9b. Ten dollars

`AddSpend(prev float64, tokens int, usdPerMTok float64) float64` adds `tokens / 1e6 * usdPerMTok`. Test 1_000_000 tokens at $3 is $3. Test 0 tokens adds 0.

`OverCap(spent, cap float64) bool` is true when `spent >= cap`. Test 10 and 10 is true. Test 9.99 and 10 is false.

`MonthKey(t time.Time) string` is `YYYY-MM` in America/Los_Angeles. Test `2026-01-01T07:30:00Z` is `2025-12`, because that instant is still December 31 in Los Angeles.

When `OverCap` is true, `RequestReview` is not called. The function `SpendBlocked(planeItem string) Notice` returns a notice whose Plane text contains the pull request's item and the words `10` and whose Discord text is one line. Test the strings. Do not call Discord in the test. The caller sends it.

### 9c. Risk

`Rate(authorRating string, files []string, sensitive []string) string`.

| author | files | result |
|---|---|---|
| `note` | `docs/readme.md` | `note` |
| `note` | `cmd/resourced/stream.go` | `high` |
| `note` | `internal/secrets/store.go` | `high` |
| `note` | `docker-compose.yml` | `high` |
| `medium-low` | `docs/readme.md` | `medium-low` |
| `` | `docs/readme.md` | `high` |
| `banana` | `docs/readme.md` | `high` |

A sensitive path matches if the file equals it or is under it when the entry ends with `/`. `docker-compose.yml` matches only that file. `cmd/resourced/stream.go` matches `cmd/resourced/`.

`NeedsReview(rating string) bool` is false only for `note`.

Use the path list from `config/review.yaml`. Do not type a second list in the code. The test passes the list in.

## Job 10 — The five minute check

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

This process is not a model. It does not call `claude`, `agent-local`, or Ollama. `TestSweep` fails if the command list contains any of those strings.

### 10a. Next action

`Next(in Input) Action`. `Action` is `none`, `enqueue-reviewer`, `enqueue-author`, `ask-nathan`, or `release-never`.

`Input` has: record, a bool `jobQueued`, a bool `jobRunning`, `waiting` `time.Duration`, `openPoints` int.

Copy this table. `waiting` is 5 minutes unless the row says 1 minute.

| situation | action |
|---|---|
| job queued | none |
| job running | none |
| waiting 1 minute, nothing queued | none |
| token is the reviewer, not held, nothing queued, waiting 5 minutes | enqueue-reviewer |
| token is the author, nothing queued, waiting 5 minutes | enqueue-author |
| rounds >= 3, nothing queued | ask-nathan |
| open points > 10 | ask-nathan |
| Held true, waiting 5 minutes | ask-nathan |
| Held true | never `enqueue-reviewer`. Add a row that would otherwise enqueue the reviewer and assert the action is `ask-nathan` |

`release-never` is not returned for Fable. The check does not release. The ask is so Nathan sees a held job that nobody mentioned. If a Plane comment asking him already exists, the caller passes `alreadyAsked true` and the action is `none`. Test that.

### 10b. The loop

`cmd/review-sweep` runs `Next` for each record in the review directory, and on `enqueue-*` calls the same publisher as Job 4, then `AppendRevival`. A test uses a temp dir with one record and a fake publisher. Assert one publish and one JSON line. Run it again with `jobQueued true` and assert no second line.

The production loop sleeps five minutes between passes. The test calls `Once`, not the sleep. Do not sleep in a test.

### 10c. End of day

`Digest(revs []reviewstate.Revival, now time.Time) (string, bool)`. `now` at 18:00 America/Los_Angeles or later, and `revs` non-empty for that Los Angeles day: the string lists each PR number and `ok` is true. Empty revs: `ok` false and the string is empty. `now` at 17:59 Los Angeles: `ok` false even if revs is non-empty, so a noon run does not send the day's bug early.

The sender, behind an interface, posts one Discord message and creates one Plane issue. Test the sender with fakes: one call each when `ok` is true, zero calls when false. The Plane issue title contains `review sweeper`. The body contains every PR number from the log.

Do not send a second digest the same Los Angeles day. Store `last-digest` as that `YYYY-MM-DD` in the review directory. Test a second call returns without sending.

## Job 11 — Plane identity

**Branch:** `phase-pr-review-queues`. Check it out. Do not open a pull request.

**Read first:** `internal/secrets`, `scripts/jarvis-secrets`, `internal/plane/plane.go` `Comment.CreatedBy`.

### 11a. Keys

Nathan stores one Plane API key per model in the queue site secret manager. The secrets page on 2026-11-19 at 5:21pm Pacific shows exactly the five names in the table below. Use those. Do not ask him to add them again. Do not rename them. Do not run `jarvis-secrets get`, and do not put a value in a comment, a log, a test, or a commit.

| Model | Secret name |
|---|---|
| Sonnet (`claude-sonnet-5-5`, driver `sonnet`) | `plane-sonnet-bot` |
| Opus (driver `opus`) | `plane-opus-bot` |
| Fable (`claude-fable-5-1`, driver `fable`) | `plane-fable-bot` |
| Haiku | `plane-haiku-bot` |
| The local brain (`qwen3.8-27b`) | `plane-jarvis-big-brain` |

`KeyName(modelID string) string` returns those. Test all five. Haiku has a key and no review queue in this plan. Posting a comment as Haiku uses `plane-haiku-bot`. Do not invent `review.haiku`.

Do not look for `sonnet-bot` or `plane-key-sonnet`. The name is `plane-sonnet-bot`, and the same pattern for the other four.

`HasKey` uses the secrets store interface (`List` or `Stat`, whichever exists; read `internal/secrets/store.go`). A missing key returns false. The test uses a temp store. Do not call the real `~/.local/share/jarvis/secrets` in the test.

On the live machine, after the tests pass, run `jarvis-secrets list`. That prints names only. The five names in the table must be in the list. If they are, mark `rev-11a` Done. A name that is absent is the only reason to leave the card **In progress**, and the comment names the missing secret, not a value.

Posting a comment takes the key for that model. If the key is missing, return an error that contains the secret name and do not fall back to `PLANE_API_KEY`. Test the fallback is not used: the fake client records which key was passed, and a missing key means the client function was not called.

### 11b. Routing

`Route(comment Comment, authorModel, namedModel string) (target string, guessed bool)`.

`Comment` in this test is a small struct: `AuthorID` string, `ParentAuthorID` string (empty if it is not a reply), `Body` string. You already know the map from Plane user id to model id. Pass the map in.

| comment | target | guessed |
|---|---|---|
| reply to Opus's user id | opus | false |
| body `sonnet please look` and no reply parent, and sonnet is not the author model | sonnet | false |
| body `looks fine` and no reply parent | the author model | true |
| reply to Nathan's user id, body empty | the author model | true |

Nathan is not in the model map. A reply to Nathan goes to the author model, which is the "was this for me" reader from the design. `guessed` true means the author must say, in its Plane reply, who it decided you were talking to.

Test all four rows. Do not call Plane.

## Job 12 — Containers and the note

**Branch:** `phase-pr-review-queues`. Check it out. This is the last job. Open the one pull request from this branch when the acceptance commands pass.

**Read first:** `docker-compose.yml` service `jarvis-chat` (it has no GPU). Copy that shape. Read `docs/agent-claude.md` far enough to see where "before you call a PR done" is, and add a short section after it. Do not rewrite the file.

### 12a. Services

Add four services: `review-sonnet`, `review-opus`, `review-fable`, `review-sweep`.

Each review service runs `review-worker -queue <its queue> -model <sonnet|opus|fable>`. `review-sweep` runs `review-sweep`.

They have the repo mounted, `gh`, and the Anthropic key the chat container already uses. They do not have a `deploy.resources.reservations.devices` entry. They do not set `NVIDIA_VISIBLE_DEVICES`. They do not depend on `jarvis-engine`.

`TestNoGPU` from Job 6 still passes. Add a test that reads `docker-compose.yml` and fails if any of those four service names appears in the same service block as `devices:` or `nvidia`. Parse carefully: a `devices:` under a different service must not fail the test. If a YAML parse is more than you can do safely, split the file by top-level service keys with a small scanner and test that scanner on a fixture in the test, not on a guess. The fixture has two services, one with a GPU and one without, and only the GPU one fails.

Do not `docker compose up` until the pull request from this branch is merged. After it is merged, bring up only those four services. `docker inspect` each one and confirm `HostConfig.DeviceRequests` is empty. If you cannot run Docker, leave `rev-12a` In progress and say so.

Do not recreate `jarvis-engine`, `jarvis-chat`, or `resourced`.

### 12b. The standing note

Add one section to `docs/agent-claude.md`, under twenty lines, titled `PR review`. It says:

- Reviews go to `review.sonnet`, `review.opus`, `review.fable`, or `review.local`. They do not use class `fix` or priority 100.
- Fable runs only after `!review-release <pr>`.
- The reviewer may commit on the pull request branch when it holds the token. It does not open a second pull request. It does not merge to `main` unless its id is in `config/review.yaml` `merge_allow`.
- Comments go on the Plane item, posted with that model's secret. They do not go on GitHub.
- A revival line in the review log is a bug. Do not delete it to make the evening digest quiet.

Then mark `rev-12b` and `rev-12` Done.

## When you are stuck

Stop the card in **In progress** and comment the command you ran and the last twenty lines of output. Do not open a different design. Do not move the card to Ready to "let the runner try." Do not lower `merge_allow` into the config to make a test pass.
