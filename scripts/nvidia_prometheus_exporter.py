#!/usr/bin/env python3
"""Lightweight nvidia-smi Prometheus exporter for RTX GPU monitoring."""
import subprocess
import sys
import time
from http.server import HTTPServer, BaseHTTPRequestHandler

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 3001

Q = (
    "index,utilization.gpu,utilization.memory,memory.used,memory.total,"
    "memory.free,temperature.gpu,power.draw,clocks.gr,clocks.mem,"
    "fan.speed,pstate"
)

def fetch_metrics():
    try:
        out = subprocess.check_output(
            ["nvidia-smi", "--query-gpu=" + Q, "--format=csv,noheader"],
            timeout=10,
        ).decode()
        lines = [l.strip() for l in out.strip().split("\n") if l.strip()]
        return lines
    except Exception as e:
        return [f"error: {e}"]


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/metrics":
            lines = fetch_metrics()
            now = time.time()
            parts = [
                "# HELP nvidia_gpu_scrape_error 1 if scrape failed",
                "# TYPE nvidia_gpu_scrape_error gauge",
            ]
            ok = True
            for line in lines:
                if line.startswith("error:"):
                    parts.append(f"nvidia_gpu_scrape_error {{}} {now} 1")
                    ok = False
                    break
                # Split on comma, strip units
                raw = line.split(",")
                vals = []
                for v in raw:
                    v = v.strip()
                    for unit in (" %", " MiB", " W", " MHz"):
                        if v.endswith(unit):
                            v = v[: -len(unit)]
                            break
                    vals.append(v)

                gpu = vals[0]  # index
                # vals[1]=gpu_util, [2]=mem_util, [3]=mem_used, [4]=mem_total,
                # [5]=mem_free, [6]=temp, [7]=power, [8]=clock_gr,
                # [9]=clock_mem, [10]=fan, [11]=pstate
                parts.extend([
                    "# HELP nvidia_gpu_utilization GPU utilization percent",
                    "# TYPE nvidia_gpu_utilization gauge",
                    f'nvidia_gpu_utilization {{gpu="{gpu}"}} {vals[1]}',
                    "# HELP nvidia_gpu_mem_util GPU memory utilization percent",
                    "# TYPE nvidia_gpu_mem_util gauge",
                    f'nvidia_gpu_mem_util {{gpu="{gpu}"}} {vals[2]}',
                    "# HELP nvidia_gpu_mem_used_mb GPU memory used MB",
                    "# TYPE nvidia_gpu_mem_used_mb gauge",
                    f'nvidia_gpu_mem_used_mb {{gpu="{gpu}"}} {vals[3]}',
                    "# HELP nvidia_gpu_mem_total_mb GPU memory total MB",
                    "# TYPE nvidia_gpu_mem_total_mb gauge",
                    f'nvidia_gpu_mem_total_mb {{gpu="{gpu}"}} {vals[4]}',
                    "# HELP nvidia_gpu_temp GPU temperature C",
                    "# TYPE nvidia_gpu_temp gauge",
                    f'nvidia_gpu_temp {{gpu="{gpu}"}} {vals[6]}',
                    "# HELP nvidia_gpu_power_w GPU power draw watts",
                    "# TYPE nvidia_gpu_power_w gauge",
                    f'nvidia_gpu_power_w {{gpu="{gpu}"}} {vals[7]}',
                    "# HELP nvidia_gpu_clock_gr_mhz GPU graphics clock MHz",
                    "# TYPE nvidia_gpu_clock_gr_mhz gauge",
                    f'nvidia_gpu_clock_gr_mhz {{gpu="{gpu}"}} {vals[8]}',
                    "# HELP nvidia_gpu_fan_percent GPU fan speed percent",
                    "# TYPE nvidia_gpu_fan_percent gauge",
                    f'nvidia_gpu_fan_percent {{gpu="{gpu}"}} {vals[10]}',
                ])
            if ok:
                parts.append(f"nvidia_gpu_scrape_error {{}} {now} 0")
            body = "\n".join(parts) + "\n"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.end_headers()
            self.wfile.write(body.encode())
        elif self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt, *args):
        pass  # quiet


if __name__ == "__main__":
    server = HTTPServer(("0.0.0.0", PORT), Handler)
    print(f"NVIDIA exporter listening on :{PORT}")
    server.serve_forever()
