#!/usr/bin/env bash
# Proves bench_runtime.sh's collection and JSON shape with the 'fake' driver
# (no model; throwaway job store). Runs S1 and S3.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

export BENCH_PROM=http://127.0.0.1:9 # unreachable on purpose: gpu numbers must degrade to null
export BENCH_POLL=0.2 BENCH_CHAT_INTERVAL=1 BENCH_CHECK_TIMEOUT=5 # the slow-test fixture would take 60 s

for sc in S1 S3; do
  bash "$here/bench_runtime.sh" fake "$sc" "$tmp/$sc.json" >/dev/null
  python3 -I - "$tmp/$sc.json" "$sc" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
sc = sys.argv[2]
assert d["driver"] == "fake" and d["scenario"] == sc and d["fake"] is True
assert set(d["sampling"]) >= {"temperature", "seed", "model_file"}
r = d["run"]
assert r["jobs_total"] == 6 and isinstance(r["wall_s"], (int, float))
assert set(r["gpu"]) >= {"util_avg_pct", "vram_peak_mb", "idle_while_unfinished_s"}
assert set(r["tokens"]) >= {"prompt", "generated"}
assert len(d["jobs"]) == 6
for j in d["jobs"]:
    assert set(j) >= {"id", "status", "check_passed", "wall_s", "kwh"}, j
    assert j["status"] == "done", j          # marked done without a model
    assert j["check_passed"] is False, j     # nothing was fixed, so the check must fail
    assert isinstance(j["wall_s"], (int, float))
if sc == "S3":
    assert d["chat"], "S3 must record chat messages"
    assert all("reply_s" in c and "sent_offset_s" in c for c in d["chat"])
else:
    assert d["chat"] is None
EOF
done
echo "bench_runtime_test: ok"
