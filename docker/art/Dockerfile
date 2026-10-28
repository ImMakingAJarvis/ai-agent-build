# Art service runtime (photo generation). The code itself is the pinned
# deploy point /opt/jarvis-art (bind-mounted by docker-compose.yml),
# matching the voice service pattern.
#
# torch comes from the cu128 wheel index: it bundles its own CUDA runtime,
# so it works with the host driver (13.x) without the CUDA toolkit libs
# that the engine container bind-mounts.
FROM python:3.12-slim
RUN pip install --no-cache-dir --upgrade pip \
 && pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cu128 \
 && pip install --no-cache-dir \
    fastapi "uvicorn[standard]" diffusers transformers accelerate \
    huggingface_hub pillow
# uid 1001 is the host 'agent' user (the bind-mounted deploy point is
# agent-owned). Give it a passwd entry: huggingface_hub calls
# getpass.getuser(), which raises 'getpwuid(): uid not found' without one.
RUN useradd -u 1001 -U agent
USER 1001:1001
WORKDIR /opt/jarvis-art
# No ENTRYPOINT: docker-compose.yml passes the full command, so an entrypoint
# here would duplicate the script path as an extra argument.
