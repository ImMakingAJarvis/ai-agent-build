# NVIDIA GPU Prometheus exporter (nvidia-smi wrapper, no pynvml).
# nvidia-smi is bind-mounted from the host; the nvidia runtime (set in
# docker-compose.yml) mounts the driver libs and /dev/nvidia* devices.
FROM python:3.14-slim
COPY scripts/nvidia_prometheus_exporter.py /usr/local/bin/nvidia_prometheus_exporter.py
RUN chmod +x /usr/local/bin/nvidia_prometheus_exporter.py
ENTRYPOINT ["python3", "/usr/local/bin/nvidia_prometheus_exporter.py"]
