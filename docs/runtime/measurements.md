# Runtime measurements

## Save, restore, re-prefill

| prompt tokens | snapshot MB | save s | restore s | re-prefill tokens/s |
|---|---|---|---|---|
| 8001 | 589 | 0.13 | 0.16 | 3061 |
| 31996 | 1012 | 0.34 | 0.21 | 2811 |
| 100203 | 2212 | 3.24 | 0.36 | 2143 |

snapshot_min_tokens: 900

prefill time = tokens / rate; snapshot cost = save s + restore s; above snapshot_min_tokens a snapshot is cheaper than re-reading the prompt (linear interpolation between sizes).
tmpfs: skipped. The engine reads only its --slot-save-path (/var/lib/jarvis/slots, bind mount), so a snapshot elsewhere cannot be restored.

Measured 2026-11-25 on the live brain (slot 0, unified 128K KV, q4_0 cache) by `scripts/measure_slots.sh`; no other job was running during the run and none started. The smallest size measured (8000 tokens) already favours a snapshot (prefill 2.6 s vs 0.29 s save+restore), so 900 is extrapolated: save+restore is about 0.3 s at small sizes, which prefill matches at about 900 tokens. Save of the 100K snapshot (3.24 s) is the slow part; restore stays under 0.4 s.
