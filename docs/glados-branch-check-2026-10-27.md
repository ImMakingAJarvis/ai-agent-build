# GLaDOS / Portal voice — branch & working-tree check

Job `chat-1792993647-5486`, 2026-10-27. Scope: all local and remote branches,
all commits (`git log --all`, subject + `-S` pickaxe), stashes, worktrees, and
the working tree of `ai-agent-build`.

## Branches

| Branch | Where | State |
|---|---|---|
| `phase-voice-glados` | local + `origin/` | **Merged** via PR #48 (Hermes scaffold) + PR #49 (Hermes `Chat()` routing), both merged 2026-10-24. Tip `4aac1d9`. |
| `phase-4-ha-voice-glados` | local + `origin/` | **Merged** via PR #53 "phase 4: GLaDOS voice + HA Voice Preview Edition connection (auth fix)", merged 2026-10-27. Tip `2f5b6cc` (the merge commit, on main). |

Both branches are fully merged into main; nothing GLaDOS-related is unmerged.
No other local branch, worktree, or remote branch matches glados/portal/aperture.

## Commits (all refs)

GLaDOS-specific:

- `478e581` feat(voice): add GLaDOS voice profile (2026-10-25) — **in main**.
  Files: `integrations/jarvis_voice/README.md`, `scripts/jarvis-voice.service`,
  `scripts/voice_prompts/glados.txt` (new), `scripts/voice_prompts/jarvis.txt`,
  `scripts/voice_service.py`.
- `2f5b6cc` Merge PR #53 (`phase-4-ha-voice-glados`)
- `1837b94` Merge PR #48 (`phase-voice-glados`)
- `07a67de` Merge PR #49 (`phase-voice-glados`)
- `1dba2f4`, `cb8fc78` — merge `origin/main` into `phase-4-ha-voice-glados` (housekeeping)

Content mentions (pickaxe `-S 'GLaDOS'`): `6546929` (containerize stack),
`9baa0e6` (notes), `a0bf8dd` (docs), `7c31113` (docs), `154fb9a`
(restore voices + hermes from WIP branch), `7cf0119` (WIP save),
`ae15dcd` (add missing voices + fast_classify packages), `4aac1d9` (Hermes routing).

Pickaxe for `Aperture` and `cake is a lie`: **no commits**.

## Stashes

- `stash@{0}` — "WIP on phase-voice-glados" (base `4aac1d9`):
  `cmd/jarvis-chat/main.go`, `internal/hermes/classify_prompt.go`,
  `internal/hermes/hermes.go`, `internal/hermes/result.go` (112+/55−).
  Hermes fast-classify WIP saved while on the GLaDOS branch — not GLaDOS content.
- `stash@{1}` — on `phase-qwen38-upgrade`, unrelated.

## File paths (any commit)

Only two paths in the entire history contain a keyword:
`internal/voices/glados_test.go` and `scripts/voice_prompts/glados.txt`.

## Working tree (checked at `39881e4`)

GLaDOS voice implementation (all on main, present in tree):

| File | Reference |
|---|---|
| `internal/voices/voices.go` | `Voice` def: Key `glados`, Name `GLaDOS`, system prompt (L95+); "do not force Portal lore" (L114) |
| `internal/voices/glados_test.go` | `TestGLaDOSVoiceContent` |
| `internal/voices/voices_test.go` | `TestGetGLaDOS` + key-list coverage |
| `scripts/voice_prompts/glados.txt` | Persona prompt ("You are GLaDOS…"); "Portal lore" (L26) |
| `scripts/voice_service.py` | `JARVIS_VOICE` doc + `glados` profile (L35, L113–115) |
| `scripts/jarvis-voice.service` | `Environment=JARVIS_VOICE=glados` (L17) — **the deployed voice service runs GLaDOS** |
| `cmd/jarvis-chat/main_test.go` | glados assertions (L396–401) |
| `integrations/jarvis_voice/README.md` | Voice table: "GLaDOS from Portal … `en-US-EmmaNeural` −10% rate, −2 Hz" (L137, L153) |

Mentions in docs/notes: `docs/runner-log-capture-2026-10-26.md` (L61),
`docs/phase4-voice-ha-log-capture.md` (L19, L85),
`notes/conversations/agent-2026-10-27.md` (L163).
`docs/jobs/d-discord.md` (L8) uses "portal" in the Discord Developer Portal
sense — not the game.

## Queue context

Related jobs in the jarvis-jobs store: `chat-1792745085-4d8f` (failed) and
`chat-1792819094-8e26` (failed) — "a GLADOS voice for use with Jarvis-chatbot";
`chat-1792993617-4223` (skipped) — "Check for GLaDOS voice remnants";
`chat-1792994365-9ba0` (ready) — "investigate-gladoss-voice-branch", a
follow-up investigation already queued behind this job.

## Bottom line

The GLaDOS voice is fully merged, present in main, and live: the `jarvis-voice`
systemd service is configured with `JARVIS_VOICE=glados`. The two GLaDOS
branches are merged with nothing pending; the only GLaDOS-adjacent stash is
unrelated Hermes WIP saved while sitting on `phase-voice-glados`.
