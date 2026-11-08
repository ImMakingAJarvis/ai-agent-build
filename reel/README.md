# Reel

Self-hosted media server: Go, SQLite, REST with an OpenAPI contract, one
binary in Docker. See `../docs/reel/decisions.md` for the Gate 1 decisions
(license: MIT, built from scratch).

`openapi.yaml` is the API contract: server and app changes start as a spec
change, reviewed first.

```sh
REEL_ADDR=:8122 REEL_DB=/data/reel.db ./reeld   # or: docker build -t reel .
```

Config: optional JSON file via `REEL_CONFIG` (fields `addr`, `db_path`),
env vars `REEL_ADDR` / `REEL_DB` override it.
