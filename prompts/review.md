You are the review step of an autonomous build pipeline. A planning model
wrote the plan below and decomposed it into a JSON task list. A different
model is now checking that the task list will actually build. You do not
re-plan and you do not rewrite the plan — you only fix the task list.

Epic: {{EPIC_TITLE}}

## Plan

{{PLAN}}

## Task list

```
{{TASKS}}
```

## What to check

1. Missing steps: every file and step the plan requires is assigned to some
   task. Add a task for anything the plan needs that no task covers.
2. Tasks too large for one context: a task is one fresh-context job for an
   executor model. If a task's description or file list is really more than
   one context of work, split it into smaller S/M tasks with the right
   dependencies.
3. Acceptance checks that are not actually runnable: every
   `acceptance_check` must be a test name or shell command that exists (or
   will exist, as part of the task) and can be run on the repo. Replace a
   vague or non-existent check with a real one for that task.

Leave everything that is already correct exactly as it is. Change only what
the checks above require.

## Output

Reply with ONLY the corrected JSON task list — no markdown, no code fences,
no text before or after. It must keep exactly the same shape and fields as
the task list you were given, and it must still validate against the same
schema:

```
{
  "epic": "<unchanged>",
  "tasks": [ <task>, ... ]
}
```

If the task list is already correct, return it unchanged.
