#!/bin/sh
# Noisy build: ~200 KB of log, one real error in the middle.
i=1
while [ $i -le 2800 ]; do
  echo "[build] step $i: compiling unit_$i ... ok (cache hit, 0 warnings, artifact sha=0000000000000000000000000000000000000000000000000000000000$i)"
  if [ $i -eq 1400 ]; then
    w=$(sed -n 's/^workers=//p' config.txt)
    if [ "$w" -lt 1 ]; then echo "ERROR: config.txt: workers must be >= 1 (got $w)"; exit 1; fi
  fi
  i=$((i+1))
done
echo "[build] done"
