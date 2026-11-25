#!/usr/bin/env bash
# ELR-2 M0.1: time llama.cpp slot save / restore and prompt re-prefill on the
# live brain. Run from a sonnet round, never a local one, with no other job
# running. Usage: scripts/measure_slots.sh [out.md]   (default: stdout)
# Env: BRAIN_URL, SLOTS_DIR, SIZES (space-separated token counts), JOBS_API.
set -euo pipefail

BRAIN_URL=${BRAIN_URL:-http://127.0.0.1:11434}
SLOTS_DIR=${SLOTS_DIR:-/var/lib/jarvis/slots}
SIZES=${SIZES:-"8000 32000 100000"}
JOBS_API=${JOBS_API:-http://127.0.0.1:8080/api/jobs}
OUT=${1:-/dev/stdout}
WORK=$(mktemp -d)
trap 'rm -f "$SLOTS_DIR"/measure-*.bin; rm -rf "$WORK"' EXIT

# Other running jobs than this one (this round's own id is $JARVIS_JOB_ID).
others_running() {
  curl -s -m 20 "$JOBS_API" | python3 -c '
import json, os, sys
d = json.load(sys.stdin)
d = d if isinstance(d, list) else d.get("jobs", [])
me = os.environ.get("JARVIS_JOB_ID", "")
print(" ".join(j["id"] for j in d if j.get("status") == "running" and j.get("id") != me))'
}

# Wait up to 60 min (poll every 2 min) until nothing else runs.
wait_quiet() {
  for _ in $(seq 30); do
    [ -z "$(others_running)" ] && return 0
    echo "waiting: $(others_running) running" >&2
    sleep 120
  done
  echo "BLOCKED: other jobs kept running for 60 min" >&2
  exit 3
}

# gen_prompt <approx tokens> <file>: unique pseudo-random text, calibrated
# against the engine's own /tokenize so the size is real tokens.
gen_prompt() {
  python3 - "$1" "$2" "$BRAIN_URL" <<'PY'
import json, random, sys, urllib.request
n, path, url = int(sys.argv[1]), sys.argv[2], sys.argv[3]
rnd = random.Random()
words = ["alpha","river","matrix","orbit","lantern","copper","vector","harbor","signal","quartz",
         "meadow","ledger","falcon","prism","anchor","ember","cipher","summit","thread","glacier"]
def build(k): return " ".join(rnd.choice(words) + str(rnd.randint(0, 9999)) for _ in range(k))
def count(t):
    r = urllib.request.Request(url + "/tokenize", json.dumps({"content": t}).encode(),
                               {"Content-Type": "application/json"})
    return len(json.load(urllib.request.urlopen(r, timeout=120))["tokens"])
k = n // 3
for _ in range(3):
    t = build(k)
    c = count(t)
    k = max(1, int(k * n / c))
open(path, "w").write(build(k))
PY
}

post() { # post <path> <json> -> prints seconds
  curl -s -m 600 -o "$WORK/resp.json" -w '%{time_total}' -X POST \
    -H 'Content-Type: application/json' -d "$2" "$BRAIN_URL$1"
}

measure() { # measure <size>  -> one row; sets REPEAT=1 if a job appeared
  local size=$1 f="$WORK/p-$1.txt" fn="measure-$1.bin"
  gen_prompt "$size" "$f"
  post "/slots/0?action=erase" '{}' >/dev/null
  python3 -c '
import json,sys
print(json.dumps({"prompt":open(sys.argv[1]).read(),"id_slot":0,"cache_prompt":True,"n_predict":1,"temperature":0}))' "$f" >"$WORK/req.json"
  local t1; t1=$(curl -s -m 1200 -o "$WORK/resp.json" -w '%{time_total}' -X POST \
    -H 'Content-Type: application/json' --data-binary @"$WORK/req.json" "$BRAIN_URL/completion")
  local ptok; ptok=$(python3 -c 'import json;print(json.load(open("'"$WORK"'/resp.json"))["tokens_evaluated"])')
  local ts; ts=$(post "/slots/0?action=save" "{\"filename\":\"$fn\"}")
  local mb; mb=$(( $(stat -c %s "$SLOTS_DIR/$fn") / 1048576 ))
  post "/slots/0?action=erase" '{}' >/dev/null
  local tr; tr=$(post "/slots/0?action=restore" "{\"filename\":\"$fn\"}")
  local n; n=$(python3 -c 'import json;print(json.load(open("'"$WORK"'/resp.json")).get("n_restored"))')
  rm -f "$SLOTS_DIR/$fn"
  echo "$ptok $mb $ts $tr $t1 $n"
}

wait_quiet
: >"$WORK/rows"
for s in $SIZES; do
  while :; do
    wait_quiet
    row=$(measure "$s")
    if [ -n "$(others_running)" ]; then
      echo "a job started during the $s run; repeating" >&2
      echo "$s" >>"$WORK/repeated"
      continue
    fi
    echo "$row" >>"$WORK/rows"; break
  done
done
post "/slots/0?action=erase" '{}' >/dev/null

python3 - "$WORK/rows" "$WORK/repeated" >"$OUT" <<'PY'
import os, sys
rows = [list(map(float, l.split()[:5])) for l in open(sys.argv[1])]
rep = sorted(set(open(sys.argv[2]).read().split())) if os.path.exists(sys.argv[2]) else []
print("## Save, restore, re-prefill\n")
print("| prompt tokens | snapshot MB | save s | restore s | re-prefill tokens/s |")
print("|---|---|---|---|---|")
pts = []
for tok, mb, ts, tr, t1 in rows:
    rate = tok / t1
    print(f"| {int(tok)} | {int(mb)} | {ts:.2f} | {tr:.2f} | {rate:.0f} |")
    pts.append((tok, t1, ts + tr))
# smallest n where prefill time exceeds save+restore, linear between sizes
d = [(n, p - c) for n, p, c in pts]
if d[0][1] >= 0: m = d[0][0] if len(d) == 1 else 0
else:
    m = None
    for (n0, g0), (n1, g1) in zip(d, d[1:]):
        if g0 < 0 <= g1: m = n0 + (n1 - n0) * (-g0) / (g1 - g0); break
print()
print("snapshot_min_tokens: " + (str(int(m)) if m is not None else f">{int(d[-1][0])}"))
print("\nprefill time = tokens / rate; snapshot cost = save s + restore s; above snapshot_min_tokens a snapshot is cheaper than re-reading the prompt (linear interpolation between sizes).")
print("tmpfs: skipped. The engine reads only its --slot-save-path (/var/lib/jarvis/slots, bind mount), so a snapshot elsewhere cannot be restored.")
if rep: print("repeated (a job started during the run): " + ", ".join(rep))
PY
