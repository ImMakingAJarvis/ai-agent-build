# Reel

Self-hosted media server: Go, SQLite, REST with an OpenAPI contract, one
binary in Docker. See `../docs/reel/decisions.md` for the Gate 1 decisions
(license: MIT, built from scratch).

`openapi.yaml` is the API contract: server and app changes start as a spec
change, reviewed first.

```sh
REEL_ADDR=:8122 REEL_DB=/data/reel.db ./reeld   # or: docker build -t reel .
```

Config: optional JSON file via `REEL_CONFIG` (fields `addr`, `db_path`,
`amqp_url`, `resourced`), env vars `REEL_ADDR` / `REEL_DB` /
`RABBITMQ_URL` / `REEL_RESOURCED` override it. Defaults: `:8122`,
`reel.db`, `amqp://jarvis:jarvis-local@localhost:5672`,
`http://172.17.0.1:9104`.

## Scanning

Libraries are scanned by walking their root and upserting what is found
(`internal/scan`): movies are `"Name (Year)/<file>"`, shows are
`"Show/Season 01/<file>"` with `SxxEyy` episode names (dotted names and
trailing quality tags parse too). Items that are no longer seen are kept —
a partial NAS mount must not delete the library.

Scans are queue jobs, not API work: `POST /api/v1/libraries/{id}/scan`
creates a `scans` row and publishes a persistent `reel.scan` message on the
stack's `tasks` exchange; reeld's consumer (one queue, prefetch 1, like the
stack's location worker) runs it and closes the row out with its
added/updated/skipped counters or the error. `GET
/api/v1/libraries/{id}/scan` reports the latest scan
(queued → running → done|error). One scan per library at a time (409
otherwise). If the broker is down at startup, reeld retries every 2 s and
serves the API meanwhile (scans stay queued); a broker death mid-run is
recovered by resubscribing — the queue is durable.

The consumer pauses under the gaming watchdog (EVB-29): it follows the
resourced stream events on the `events` exchange (`stream.started` with
`game: true` pauses, any `stream.stopped` resumes; a Sunshine desktop
stream sets `game: false` and never pauses) and polls resourced's
`GET /state` once at startup so a reeld that boots mid-game begins paused.
While paused, an arriving scan delivery is held unacked and runs after the
stream ends; a scan already running finishes first.

## Metadata & artwork (TMDB)

Scans fetch metadata (title/year, overview, rating, air date, poster,
backdrop) from the TMDB v3 API (`internal/tmdb`) and store it on the item
(`item_meta`; exposed as the `metadata` field on the item endpoints).
Movies match on title + year, shows on the folder name — an exact
(case-folded) match wins, otherwise TMDB's own relevance ranking is the
fuzzy fallback.

- **Key:** `TMDB_API_KEY` or `REEL_TMDB_KEY` (env only — never in the
  repo). Without a key, scans run without metadata.
- **Cache:** on disk under `REEL_CACHE_DIR` (default `<db dir>/cache`) —
  the NAS media mounts are read-only, so the cache lives on the box next
  to the database, not on the NAS. Meta is one JSON file per lookup under
  `meta/`; poster (w500) and backdrop (w1280) images under `artwork/`,
  served by `GET /api/v1/items/{id}/artwork/{kind}` (episode artwork is
  the series' artwork).
- **Cache-first:** a second scan of an unchanged library makes zero
  network calls; an item whose file changed is refetched. A 429 is waited
  out via `X-RateLimit-Reset` / `Retry-After` and retried (≤3 waits).
- **Best effort:** a fetch failure leaves the item without metadata and
  is logged — it never fails the scan.

## Test library

`testdata/` is a permanent, tiny generated library (ffmpeg clips, both
layouts) that all automated tests use — never the real NAS media:

- `testdata/movies/` — 3 movie dirs (`Big Buck Bunny (2008)`, `Sintel
  (2010)`, `War for the Planet of the Apes (2017) [1080p]`).
- `testdata/shows/The Test Show/Season 0{1,2}/` — 4 episodes in three
  naming styles plus non-media files (`.txt`, a hidden `.mkv`) that the
  walker must skip.

`go test ./...` in this directory runs everything, including the queue E2E
(`internal/reelq`): a real broker on `RABBITMQ_URL` (default the local
stack broker) makes it run the full POST → queue → scan → items path and
the game-stream pause; without a reachable broker it skips.
