# syntax=docker/dockerfile:1

# ---- 阶段 1:前端构建 ----
FROM node:22-slim-bookworm AS frontend
WORKDIR /fe
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN node node_modules/vite/bin/vite.js build

# ---- 阶段 2:后端编译(CGON=0 纯静态;嵌入前端产物)----
FROM golang:1.27-bookworm AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN mkdir -p internal/api/dist \
 && printf '<!doctype html><html><body>placeholder</body></html>' > internal/api/dist/index.html \
 && go mod download
COPY backend/ ./
COPY --from=frontend /fe/dist ./internal/api/dist/
RUN CGO_ENABLED=0 go build -o /out/douyin-server ./cmd/server \
 && CGO_ENABLED=0 go build -o /out/import-legacy ./cmd/import-legacy

# ---- 阶段 3:运行时(python3 + node:Go 程序按需拉起侧车执行 F2 签名)----
FROM python:3.12-slim-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*
# F2 依赖预装到系统 python(容器内直接用 python3,无需 venv)
COPY sidecar/requirements.txt /opt/sidecar/requirements.txt
RUN pip install --no-cache-dir -r /opt/sidecar/requirements.txt

WORKDIR /app
COPY --from=backend /out/douyin-server ./douyin-server
COPY --from=backend /out/import-legacy ./import-legacy
COPY sidecar/main.py ./sidecar/main.py

ENV DY_PORT=8787 \
    DY_DATA_DIR=/app/data \
    DY_SIDECAR_PYTHON=python3 \
    DY_SIDECAR_PORT=18787
VOLUME ["/app/data"]
EXPOSE 8787

CMD ["/app/douyin-server"]
