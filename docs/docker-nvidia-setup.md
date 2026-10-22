# Docker + NVIDIA Container Toolkit

## What was installed
- **NVIDIA Container Toolkit** `nvidia-container-toolkit` 1.20.1-1 (from the CUDA Ubuntu 26.04 repo already present on this system).
- NVIDIA runtime added to `/etc/docker/daemon.json` via `nvidia-ctk runtime configure`.

## Configuration
`/etc/docker/daemon.json`:
```json
{
    "runtimes": {
        "nvidia": {
            "args": [],
            "path": "nvidia-container-runtime"
        }
    }
}
```

## Verification
```bash
docker run --rm --gpus all nvidia/cuda:12.6.0-base-ubuntu24.04 nvidia-smi
```
Shows the RTX 4090 inside the container — GPU access confirmed.

## Reinstall
```bash
scripts/install_nvidia_container_toolkit.sh
```

## Undo
```bash
sudo apt-get remove nvidia-container-toolkit nvidia-container-toolkit-base libnvidia-container-tools libnvidia-container1
# Remove the nvidia runtime section from /etc/docker/daemon.json
sudo systemctl restart docker
```
