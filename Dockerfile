# syntax=docker/dockerfile:1

# ============================================================
# Stage 1: 构建前端 (React + Vite)
# 前端产物是纯静态文件，只需构建一次，与目标平台无关
# ============================================================
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY frontend/ .
ARG BUILD_VERSION=dev
RUN VITE_APP_VERSION=${BUILD_VERSION} npm run build

# ============================================================
# Stage 2: 构建 Go 后端
# 使用 BUILDPLATFORM 原生运行 + TARGETARCH 交叉编译
# ============================================================
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS go-builder

# 国内构建走 goproxy.cn，避免直连 proxy.golang.org 断流（unexpected EOF）
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /app
COPY go.mod go.sum ./
# Keep modules in the image layer so external layer caches restore them.
RUN go mod download

COPY . .
COPY --from=frontend-builder /frontend/dist ./frontend/dist

ARG TARGETARCH
ARG BUILD_VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/codex2api/internal/version.Version=${BUILD_VERSION}" -o /codex2api .

# ============================================================
# Stage 3: 最终运行镜像
# ============================================================
FROM node:20-bookworm-slim

ARG TOSUB2_REF=v1.7.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata python3 python3-venv git \
    && rm -rf /var/lib/apt/lists/*

# Keep the protocol dependency layer independent from the Go binary layer. In
# BuildKit, npm and pip caches survive ordinary application-only rebuilds.
RUN --mount=type=cache,target=/root/.npm \
    git clone --depth 1 --branch "${TOSUB2_REF}" https://github.com/poxiao33/toSub2.git /opt/tosub2 \
    && npm --prefix /opt/tosub2 ci --omit=dev --ignore-scripts --no-audit --no-fund \
    && rm -rf /opt/tosub2/.git

RUN python3 -m venv /opt/tosub2/.venv

RUN --mount=type=cache,target=/root/.cache/pip \
    /opt/tosub2/.venv/bin/pip install -r /opt/tosub2/requirements.txt

COPY --from=go-builder /codex2api /usr/local/bin/codex2api

ENV TOSUB2_ROOT=/opt/tosub2 \
    TOSUB2_PYTHON=/opt/tosub2/.venv/bin/python \
    CREDENTIAL_OPS_NODE=node

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/codex2api"]
