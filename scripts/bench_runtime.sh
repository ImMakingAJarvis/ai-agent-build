#!/usr/bin/env bash
# bench_runtime.sh <driver> <scenario> <out.json>
#
# Runs the fixed benchmark (docs/runtime/bench/suite.json) on one driver
# and one scenario (S1, S2, S3) and writes a result file. Read
# docs/runtime/bench/README.md for what each number means.
#
#   driver    local | igpu | sonnet | opus | fake
#             fake = no model: jobs are created in a throwaway job store
#             and marked done at once (proves collection + JSON shape).
#   scenario  S1 each job alone, one after another
#             S2 all six queued at the same moment
#             S3 S2 plus five chat messages 60 s apart while it runs
#
# Env (all optional):
#   BENCH_PROM        Prometheus base URL            (http://localhost:9090)
#   BENCH_ENGINE_URL  llama.cpp engine for tokens + chat replies
#                     (default by driver: local 11434, igpu 11435; none for sonnet/opus/fake)
#   BENCH_TEMP        sampling temperature for chat messages (0.7)
#   BENCH_SEED        seed for chat messages and recorded in the result (42)
#   BENCH_TIMEOUT     seconds to wait per job (3600)
#   BENCH_POLL        poll interval seconds (2)
#   BENCH_CHAT_INTERVAL  seconds between chat messages (suite.json value)
#   BENCH_KEEP        1 = keep the temp workspace
#   BENCH_CHECK_TIMEOUT  seconds a check may run (900); a timeout counts as failed
set -euo pipefail

driver=${1:?usage: bench_runtime.sh <driver> <scenario> <out.json>}
scenario=${2:?usage: bench_runtime.sh <driver> <scenario> <out.json>}
out=${3:?usage: bench_runtime.sh <driver> <scenario> <out.json>}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
bench=${BENCH_DIR:-$here/../docs/runtime/bench}
suite=$bench/suite.json
[ -f "$suite" ] || { echo "no $suite" >&2; exit 2; }

PROM=${BENCH_PROM:-http://localhost:9090}
TEMP=${BENCH_TEMP:-0.7}
SEED=${BENCH_SEED:-42}
TIMEOUT=${BENCH_TIMEOUT:-3600}
POLL=${BENCH_POLL:-2}
JJ=${JARVIS_JOBS_BIN:-jarvis-jobs}

case $driver in
  local) engine=${BENCH_ENGINE_URL:-http://127.0.0.1:11434} ;;
  igpu) engine=${BENCH_ENGINE_URL:-http://127.0.0.1:11435} ;;
  sonnet | opus | fake) engine=${BENCH_ENGINE_URL:-} ;;
  *) echo "unknown driver $driver (local|igpu|sonnet|opus|fake)" >&2; exit 2 ;;
esac

# scenario field lookup: sfield <field> <default>
sfield() {
  python3 -I -c "
import json,sys
d=json.load(open(sys.argv[1])); s=[x for x in d['scenarios'] if x['id']==sys.argv[2]]
if not s: sys.exit(1)
v=s[0].get(sys.argv[3], sys.argv[4])
print(' '.join(v) if isinstance(v,list) else v)" "$suite" "$scenario" "$1" "$2"
}
job_ids=$(sfield jobs "") || { echo "unknown scenario $scenario" >&2; exit 2; }
chat_n=$(sfield chat_messages 0)
chat_gap=${BENCH_CHAT_INTERVAL:-$(sfield chat_interval_s 60)}
mode=$(sfield mode parallel)

jobfield() { # jobfield <id> <field>
  python3 -I -c "
import json,sys
d=json.load(open(sys.argv[1]))
print([j for j in d['jobs'] if j['id']==sys.argv[2]][0][sys.argv[3]])" "$suite" "$1" "$2"
}

ws=$(mktemp -d /tmp/bench-ws.XXXXXX)
run=$(date +%s)-$$
state=$ws/.state
mkdir -p "$state"
fake_store=
cleanup() {
  [ "${BENCH_KEEP:-0}" = 1 ] || rm -rf "$ws"
  [ -z "$fake_store" ] || rm -rf "$fake_store"
}
trap cleanup EXIT

if [ "$driver" = fake ]; then
  fake_store=$(mktemp -d /tmp/bench-store.XXXXXX)
  export JARVIS_DATA_DIR=$fake_store
fi

# --- fresh workspace ---------------------------------------------------
for id in $job_ids; do
  mkdir -p "$ws/$id"
  cp -a "$bench/fixtures/$(jobfield "$id" fixture)/." "$ws/$id/"
done

# --- engine counters ---------------------------------------------------
counter() { # counter <metric>  (empty when no engine)
  [ -n "$engine" ] || return 0
  curl -fsS --max-time 5 "$engine/metrics" 2>/dev/null | awk -v m="$1" '$1==m {print $2}' | head -1
}
tok_before_p=$(counter 'llamacpp:prompt_tokens_total' || true)
tok_before_g=$(counter 'llamacpp:tokens_predicted_total' || true)

# --- jobs --------------------------------------------------------------
now() { date +%s.%N; }
jid() { echo "bench-$run-$1"; }
driver_arg=$driver
[ "$driver" != fake ] || driver_arg=local

submit() { # submit <id>
  local id=$1 instr
  instr=$(jobfield "$id" instructions)
  instr=${instr//\{dir\}/$ws/$id}
  now >"$state/$id.start"
  "$JJ" add -id "$(jid "$id")" -title "bench $id ($scenario)" -driver "$driver_arg" \
    -tags bench -source bench -class background -instructions "$instr" >/dev/null
}

jstatus() { "$JJ" show "$(jid "$1")" 2>/dev/null | awk '/^status:/ {print $2; exit}'; }
finished() { case $1 in done | failed | parked | skipped) return 0 ;; *) return 1 ;; esac; }

wait_job() { # wait_job <id>  (records .end and .status)
  local id=$1 t0 st
  t0=$(date +%s)
  while :; do
    st=$(jstatus "$id" || true)
    if finished "$st"; then now >"$state/$id.end"; echo "$st" >"$state/$id.status"; return 0; fi
    if [ $(($(date +%s) - t0)) -gt "$TIMEOUT" ]; then
      now >"$state/$id.end"; echo timeout >"$state/$id.status"; return 0
    fi
    sleep "$POLL"
  done
}

fake_finish() { # model-free completion
  "$JJ" mark-running "$(jid "$1")" >/dev/null 2>&1
  "$JJ" finish "$(jid "$1")" 0 >/dev/null 2>&1
}

# --- chat messages (S3) ------------------------------------------------
chat_loop() {
  local i=0 text start end code body slept
  local texts
  mapfile -t texts < <(python3 -I -c "
import json,sys
d=json.load(open(sys.argv[1])); s=[x for x in d['scenarios'] if x['id']==sys.argv[2]][0]
print('\n'.join(s.get('chat_text',['ping'])))" "$suite" "$scenario")
  while [ "$i" -lt "$chat_n" ] && [ ! -e "$state/jobs.finished" ]; do
    text=${texts[$((i % ${#texts[@]}))]}
    start=$(now)
    if [ "$driver" = fake ]; then
      code=ok
    elif [ -n "$engine" ]; then
      body=$(python3 -I -c "
import json,sys
print(json.dumps({'messages':[{'role':'user','content':sys.argv[1]}],'max_tokens':120,'temperature':float(sys.argv[2]),'seed':int(sys.argv[3])}))" "$text" "$TEMP" "$SEED")
      if curl -fsS --max-time 300 -H 'Content-Type: application/json' -d "$body" "$engine/v1/chat/completions" >/dev/null 2>&1; then code=ok; else code=error; fi
    else
      code=no-engine
    fi
    end=$(now)
    echo "$i $start $end $code" >>"$state/chat.log"
    i=$((i + 1))
    slept=0 # sleep the gap, but wake early when the jobs are finished
    while [ "$slept" -lt "$chat_gap" ] && [ ! -e "$state/jobs.finished" ]; do sleep 1; slept=$((slept + 1)); done
  done
}

t_start=$(now)
chat_pid=
if [ "$chat_n" -gt 0 ]; then chat_loop & chat_pid=$!; fi

if [ "$mode" = sequential ]; then
  for id in $job_ids; do
    submit "$id"
    [ "$driver" != fake ] || fake_finish "$id"
    wait_job "$id"
  done
else
  for id in $job_ids; do submit "$id"; done
  if [ "$driver" = fake ]; then for id in $job_ids; do fake_finish "$id"; done; fi
  for id in $job_ids; do wait_job "$id"; done
fi
t_end=$(now)
touch "$state/jobs.finished"
[ -z "$chat_pid" ] || wait "$chat_pid" 2>/dev/null || true

tok_after_p=$(counter 'llamacpp:prompt_tokens_total' || true)
tok_after_g=$(counter 'llamacpp:tokens_predicted_total' || true)

# --- checks + per-job store data --------------------------------------
data_dir=${JARVIS_DATA_DIR:-$HOME/.local/share/jarvis}
for id in $job_ids; do
  chk=$(jobfield "$id" check)
  if (cd "$ws/$id" && timeout "${BENCH_CHECK_TIMEOUT:-900}" bash -c "$chk") >"$state/$id.check.log" 2>&1; then echo true >"$state/$id.check"; else echo false >"$state/$id.check"; fi
  sf=$data_dir/jobs/$(jid "$id")/state.json
  if [ -f "$sf" ]; then cp "$sf" "$state/$id.state.json"; else echo '{}' >"$state/$id.state.json"; fi
done

model_file=
if [ -n "$engine" ]; then
  model_file=$(curl -fsS --max-time 5 "$engine/props" 2>/dev/null | python3 -I -c "
import json,sys
try:
    d=json.load(sys.stdin); print(d.get('model_path') or d.get('model_alias') or '')
except Exception: pass" 2>/dev/null || true)
fi

# --- assemble ----------------------------------------------------------
python3 -I - "$suite" "$scenario" "$driver" "$out" "$state" "$PROM" "$t_start" "$t_end" "$job_ids" \
  "${tok_before_p:-}" "${tok_after_p:-}" "${tok_before_g:-}" "${tok_after_g:-}" \
  "$TEMP" "$SEED" "$model_file" "${engine:-}" "$chat_n" <<'EOF'
import json, os, sys, urllib.parse, urllib.request

(suite, scenario, driver, out, state, prom, t0, t1, ids,
 bp, ap, bg, ag, temp, seed, model_file, engine, chat_n) = sys.argv[1:19]
t0, t1 = float(t0), float(t1)
ids = ids.split()
suite_d = json.load(open(suite))


def rd(name, default=""):
    try:
        return open(os.path.join(state, name)).read().strip()
    except OSError:
        return default


def num(s):
    try:
        return float(s)
    except ValueError:
        return None


jobs, intervals = [], []
for i in ids:
    s, e = num(rd(i + ".start")), num(rd(i + ".end"))
    try:
        sj = json.load(open(os.path.join(state, i + ".state.json")))
    except Exception:
        sj = {}
    jobs.append({
        "id": i,
        "status": rd(i + ".status", "unknown"),
        "check_passed": rd(i + ".check") == "true",
        "wall_s": round(e - s, 2) if s is not None and e is not None else None,
        "kwh": sj.get("kwh"),
        "model": sj.get("model"),
    })
    if s is not None and e is not None:
        intervals.append((s, e))

STEP = 5


def prom_range(q):
    url = prom.rstrip("/") + "/api/v1/query_range?" + urllib.parse.urlencode(
        {"query": q, "start": t0, "end": t1 + 1, "step": STEP})
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            res = json.load(r)["data"]["result"]
        return [(float(t), float(v)) for t, v in res[0]["values"]] if res else []
    except Exception:
        return None


util = prom_range("nvidia_gpu_utilization")
vram = prom_range("nvidia_gpu_mem_used_mb")
gpu = {"util_avg_pct": None, "vram_peak_mb": None, "idle_while_unfinished_s": None,
       "source": prom + " (nvidia_gpu_utilization, nvidia_gpu_mem_used_mb)"}
if util:
    gpu["util_avg_pct"] = round(sum(v for _, v in util) / len(util), 2)
    gpu["idle_while_unfinished_s"] = sum(
        STEP for t, v in util if v < 10 and any(s <= t <= e for s, e in intervals))
if vram:
    gpu["vram_peak_mb"] = max(v for _, v in vram)

tokens = {"prompt": None, "generated": None, "source": (engine + "/metrics") if engine else None}
if bp and ap:
    tokens["prompt"] = int(float(ap) - float(bp))
if bg and ag:
    tokens["generated"] = int(float(ag) - float(bg))

chat = []
try:
    for line in open(os.path.join(state, "chat.log")):
        n, s, e, code = line.split()
        chat.append({"n": int(n) + 1, "sent_offset_s": round(float(s) - t0, 1),
                     "reply_s": round(float(e) - float(s), 2) if code == "ok" else None,
                     "result": code})
except OSError:
    pass

kwhs = [j["kwh"] for j in jobs if j["kwh"]]
res = {
    "suite_version": suite_d.get("version"),
    "driver": driver,
    "scenario": scenario,
    "fake": driver == "fake",
    "sampling": {
        "temperature": float(temp),
        "seed": int(seed),
        "model_file": model_file or None,
        "note": "temperature/seed are applied to the chat messages (direct engine calls). "
                "Job rounds use the driver's own sampling; the seed is requested only. "
                "Repeat each setup and compare against the spread (README: noise rule).",
    },
    "run": {
        "wall_s": round(t1 - t0, 2),
        "jobs_total": len(jobs),
        "jobs_passed": sum(1 for j in jobs if j["check_passed"]),
        "gpu": gpu,
        "tokens": tokens,
        "total_kwh": round(sum(kwhs), 6) if kwhs else None,
    },
    "jobs": jobs,
    "chat": chat if int(chat_n) > 0 else None,
}
json.dump(res, open(out, "w"), indent=1)
print("wrote %s jobs_passed=%d/%d wall=%.1fs" % (out, res["run"]["jobs_passed"], len(jobs), res["run"]["wall_s"]))
EOF
