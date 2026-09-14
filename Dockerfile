# syntax=docker/dockerfile:1

# 国内网络友好:Go 模块走 goproxy.cn,pip 走清华源,npm 走 npmmirror。
# 运行时的 node 直接从构建阶段拷贝二进制(不引 nodesource apt 源)。

# ---- 阶段 1:前端构建 ----
FROM node:22-bookworm-slim AS frontend
WORKDIR /fe
COPY frontend/package.json frontend/package-lock.json ./
# BuildKit 缓存挂载:重复构建时 npm 依赖不再全量重新下载
RUN --mount=type=cache,target=/root/.npm \
    npm ci --registry=https://registry.npmmirror.com
COPY frontend/ ./
# 某些环境下 vite build 完成后进程因残留句柄不退出(产物已写完),
# 用 timeout 兜底强杀;随后校验 dist 产物完整性,构建真失败时在此失败。
RUN timeout 900 node node_modules/vite/bin/vite.js build || true; \
    test -f dist/index.html && grep -q "assets/index-" dist/index.html \
 && echo "[frontend] dist verified"

# ---- 阶段 2:后端编译(CGON=0 纯静态;嵌入前端产物)----
FROM golang:1.27-bookworm AS backend
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    mkdir -p internal/api/dist \
 && printf '<!doctype html><html><body>placeholder</body></html>' > internal/api/dist/index.html \
 && go mod download
COPY backend/ ./
COPY --from=frontend /fe/dist ./internal/api/dist/
# 模块缓存 + 编译缓存挂载:依赖不变时 go build 秒级(否则 modernc sqlite 全量重编)
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/douyin-server ./cmd/server \
 && CGO_ENABLED=0 go build -o /out/import-legacy ./cmd/import-legacy

# ---- 阶段 3:运行时(python3 + node:Go 程序按需拉起侧车执行 F2 签名)----
# node 二进制直接取自前端构建阶段(官方 node 镜像),不再引入 nodesource apt 源。
FROM python:3.12-slim-bookworm
COPY --from=frontend /usr/local/bin/node /usr/local/bin/node
# apt 换清华源(bookworm 为 deb822 格式的 debian.sources;兼容旧 sources.list)
RUN set -eux; \
    sed -i 's|deb.debian.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    sed -i 's|deb.debian.org|mirrors.tuna.tsinghua.edu.cn|g' /etc/apt/sources.list 2>/dev/null || true; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates; \
    rm -rf /var/lib/apt/lists/*
# F2 依赖预装到系统 python(容器内直接用 python3,无需 venv);清华 pip 镜像 + BuildKit 缓存
COPY sidecar/requirements.txt /opt/sidecar/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install -i https://pypi.tuna.tsinghua.edu.cn/simple -r /opt/sidecar/requirements.txt

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
