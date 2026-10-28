# Memory service runtime. Code is the pinned deploy point /opt/jarvis-memory
# (bind-mounted read-only by docker-compose.yml); DB + ONNX model live in
# ~/.local/share/jarvis (mounted).
FROM python:3.14-slim
RUN pip install --no-cache-dir \
    fastapi "uvicorn[standard]" onnxruntime tokenizers numpy pydantic
USER 1001:1001
# No ENTRYPOINT: docker-compose.yml passes the full command (memory and
# memory-reindex differ), so an entrypoint here would duplicate the script
# path as an extra argument and crash argparse.
