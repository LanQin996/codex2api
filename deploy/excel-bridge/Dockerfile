FROM python:3.12-slim AS source
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Pin implementation, never run an automatically changing main branch.
RUN git init . \
    && git remote add origin https://github.com/Nonary/ghcp_proxy.git \
    && git fetch --depth 1 origin ad23ce2db3b5212c0355762d981c3877322fb160 \
    && git checkout --detach FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = ad23ce2db3b5212c0355762d981c3877322fb160

COPY upstream-compaction.patch /tmp/upstream-compaction.patch
RUN git apply --check /tmp/upstream-compaction.patch && git apply /tmp/upstream-compaction.patch

FROM python:3.12-slim
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    PYTHONPATH=/opt/ghcp \
    GHCP_CONFIG_DIR=/tmp/ghcp/config \
    GHCP_STATE_DIR=/tmp/ghcp/state \
    GHCP_CACHE_DIR=/tmp/ghcp/cache \
    GHCP_RESPONSES_UPSTREAM=rest \
    GHCP_UPSTREAM_TLS_VERIFY=1
WORKDIR /app
COPY --from=source /src/*.py /src/requirements.txt /src/LICENSE /opt/ghcp/
RUN pip install --no-cache-dir -r /opt/ghcp/requirements.txt
COPY bridge.py /app/bridge.py
RUN mkdir -p /var/lib/excel-bridge && chown 10001:10001 /var/lib/excel-bridge \
    && chmod 700 /var/lib/excel-bridge
USER 10001:10001
EXPOSE 8000
CMD ["uvicorn", "bridge:production_app", "--factory", "--host", "0.0.0.0", "--port", "8000", "--workers", "1", "--no-access-log"]
