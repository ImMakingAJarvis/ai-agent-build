# GLM 5.3 flash VRAM sweep — point 1/6: ~100 MB free (GLM-2)

## Model

Provenance recorded by prerequisite job `plan-glm53-flash-vram-sweep-p-1793859678-1`
(kanban `fe3ebc1f`, Plane GLM-1), 2026-11-10.

- **Source:** Hugging Face `unsloth/GLM-5.3-Flash-GGUF` (base `zai-org/GLM-5.3-Flash`,
  320B total / 18B active, arch `glm5_next`), revision `a38483c8cd5df544f53d70fb281afe97369d5ab6`.
- **Quant:** `UD-IQ2_XXS` (Unsloth Dynamic; expert tensors mix IQ2_XXS / IQ2_S gate_up with IQ3_XXS / IQ4_XS down).
- **On disk:** `/home/agent/models/glm5.3-flash/UD-IQ2_XXS/` — pass the first shard as `--model`.

| Shard | Bytes | Size check vs HF |
|---|---|---|
| `GLM-5.3-Flash-UD-IQ2_XXS-00001-of-00004.gguf` | 9,429,984 | match |
| `GLM-5.3-Flash-UD-IQ2_XXS-00002-of-00004.gguf` | 49,896,298,080 | match |
| `GLM-5.3-Flash-UD-IQ2_XXS-00003-of-00004.gguf` | 49,248,507,840 | match |
| `GLM-5.3-Flash-UD-IQ2_XXS-00004-of-00004.gguf` | 2,690,716,000 | match |

Total ≈ 101.8 GB (94.8 GiB). All four sizes match the HF API byte-for-byte (sha256
from the repo: 337870ce…, d51a3d9c…, 89180f13…, 9e0ef47d…; not re-hashed locally).

### Why this quant (why not UD-IQ4_XS)

The Qwen sweep used `UD-IQ4_XS` (88 GB on disk). For GLM 5.3 flash the same quant is
~146 GiB (5 shards) — more than 24 GiB VRAM + 122.7 GiB host RAM can hold even with
the brain stopped, so it cannot be weights-in-RAM / experts-hot-on-GPU. Candidates
(GiB, from the HF file list): IQ1_S 87, IQ1_M 91, **IQ2_XXS 95**, IQ3_XXS 112,
Q2_K_XL 101, Q3_K_XL 137, IQ4_XS 146, Q4_K_XL 186+. IQ3_XXS (112) leaves only ~14 GiB
RAM headroom with the brain stopped (MemAvailable ≈ 92 GiB with it up + ~11 GiB) and
would page under load; IQ2_XXS keeps ~70 GiB resident in RAM with ~23 GiB on the GPU
and comfortable headroom. Note: the Qwen points used a 4-bit quant, so GLM numbers are
not bit-width-comparable. `glm5_next` needs a llama.cpp/ft build with arch support;
`ft bench bw` read the GGUF fine (below); serving support is for the point jobs to confirm.

### `ft bench bw` output

Run with the brain up (3.7 GiB VRAM free; no engine stop needed), `PATH` including
`/home/agent/ftvenv/bin` (otherwise `ft` fails with `No such file or directory: 'ninja'`).

```
  host HomeAI   gpu cuda:0 (NVIDIA GeForce RTX 4090)   cpu AMD Ryzen 7 7800X3D 8-Core Processor (8c, 1 NUMA)
  ceilings: CPU STREAM read 44.8  |  PCIe linear H2D 26.8  D2H 26.3  GB/s

  gguf(gate_up=IQ2_XXS, down=IQ3_XXS)  H=4096 I=2048 top_k=8 threads=7  (GLM-5.3-Flash-UD-IQ2_XXS-00001-of-00004.gguf; threshold 2.0x)
    format      expert       CPU-MoE   PCIe-gather  CPU/PCIe  backend
    gguf       7.19 MB     28.6 GB/s     25.3 GB/s     1.13x  offload
       overlapped: CPU-MoE 19.9 + PCIe 22.4 GB/s -> hybrid fetches 52.9% of misses

  gguf(gate_up=IQ2_S, down=IQ4_XS)  H=4096 I=2048 top_k=8 threads=7  (GLM-5.3-Flash-UD-IQ2_XXS-00001-of-00004.gguf; threshold 2.0x)
    format      expert       CPU-MoE   PCIe-gather  CPU/PCIe  backend
    gguf       9.38 MB     38.8 GB/s     25.6 GB/s     1.51x  offload
       overlapped: CPU-MoE 23.5 + PCIe 19.7 GB/s -> hybrid fetches 45.7% of misses

  gguf(gate_up=IQ2_XXS, down=IQ4_XS)  H=4096 I=2048 top_k=8 threads=7  (GLM-5.3-Flash-UD-IQ2_XXS-00001-of-00004.gguf; threshold 2.0x)
    format      expert       CPU-MoE   PCIe-gather  CPU/PCIe  backend
    gguf       8.38 MB     34.8 GB/s     25.6 GB/s     1.36x  offload
       overlapped: CPU-MoE 22.0 + PCIe 21.0 GB/s -> hybrid fetches 48.8% of misses

  saved: /home/agent/.cache/freetoken/benchbw/GPU-53c09353-4b85-a06f-4e54-c29df1b1ca33.json (3 rows benched, 3 in the profile)

```

Resolved cleanly: three expert-format groups benched, backend pick `offload` for each
(CPU/PCIe ratio 1.13–1.51× < 2.0× threshold); overlapped numbers say hybrid would fetch
~46–53% of misses over PCIe.

## Benchmark

_To be filled in by the 100 MB point job (`plan-glm53-flash-vram-sweep-p-1793859678-2`)._
