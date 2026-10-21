# D0 — Dashboard: raw JSON over the UI, and keeping it running

Read only the section for your job.

Root cause, shared by all D0 jobs (main at b5e96d7):
- In `internal/dashboard/templates/index.html`, line 300, `<div id="dashboard">` polls `hx-get="/api/state" hx-trigger="every 5s" hx-swap="outerHTML"`.
- `/api/state` is `handleState` (`internal/dashboard/dashboard.go`, line 198; route on line 163). It returns `application/json` through `respondJSON` (line 560).
- htmx swaps any 2xx body in as-is, so the first poll replaces the dashboard with JSON text. That also wipes the nested approvals panel (line 393). The JSON has no `hx-*` attributes, so polling stops.
- `<body>` on line 298 polls the same URL with `hx-swap="none"`, which only doubles the load.

Stopgap until D0 is merged: Nathan runs a proxy on :8081 (`~/.local/share/jarvis-dashboard-shim/shim.py` in his home) that answers htmx requests for `/api/state` with HTML. Tell him on Discord when D0.1 is merged so he can remove it.

## D0.1 — HTML partial for the poll

- Wrap the dashboard markup in `{{define "dashboard"}}…{{end}}`. Add `GET /partials/dashboard` (`text/html`) that renders just that block from `gatherState()`. The returned `<div id="dashboard" hx-get="/partials/dashboard" hx-trigger="every 5s" hx-swap="outerHTML">` must carry its own poll attributes. Remove the poll from `<body>`. `/api/state` stays JSON.
- Done when: `httptest` tests show `/partials/dashboard` returns `text/html`, starts with `<div id="dashboard"`, contains `hx-get="/partials/dashboard"` and no `{"ollama_healthy"`; `/api/state` is still JSON; `GET /` has exactly one dashboard poll. Manually, the page shows no JSON after 60s, and a new job appears without a reload.

## D0.2 — Page cut off mid-render

- `/tmp/dashboard.log` (15:36 on Oct 21) shows `superfluous response.WriteHeader call from …handleIndex (dashboard.go:194)`. `tmpl.Execute` failed after it had started writing. Find the template error. Parse templates once at startup, and render into a buffer before writing, so an error gives a clean 500.
- Done when: a template test renders with fixture state (empty history, failed jobs, approvals, costs) without error.

## D0.3 — Dashboard and worker as services

- On Oct 21 they were started by hand from agent sessions (`/tmp/jarvis-dashboard`, `/tmp/jarvis-worker`, two `~/.local/bin/worker`). So the dashboard died when a round ended, and three workers consumed at once.
- Add `jarvis-dashboard.service` and `jarvis-worker.service` (`User=agent`, binaries outside `/tmp`, `Restart=on-failure`, enabled at boot). Stop the stray copies. From then on use `systemctl restart`, never a background process from a session.
- Done when: `systemctl is-active` shows both active, `pgrep -c -f 'jarvis-worker|bin/worker'` is 1, and the dashboard is still up after the round ends and after a reboot.
