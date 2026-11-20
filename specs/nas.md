---
id: nas
name: NAS (TempleOfTime)
summary: Synology NAS at 111.1.2.222 — hosts Plane and is the durable store for benchmark results and future media.
group: external
kind: external
status: external
ports: ["SMB, DSM 5000/5001"]
docs: [docs/reel/jellyfin-baseline.md]
depends_on: []
---

## Purpose

Durable storage off this box (Nathan, 2026-11-04: save benchmark results
to the NAS; the local 1 TB SSD is working space). Media source for Reel
later.

## Interfaces

- SMB share `Jarviss`; credentials in `/etc/jarvis-nas.credentials`
  (root 600).

## Configuration

- Mount target `/mnt/jarvis` when configured.

## Data

- Benchmark results and backups; future media library.

## Deploy

Not deployed from this repo.

## Verify live

    mount | grep /mnt/jarvis

## Failure modes

- The share named in the Reel plan did not exist; the agent guessed share
  names twice before Nathan clarified (2026-11-04).
