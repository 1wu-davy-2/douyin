#!/usr/bin/env bash
# Docker Compose 部署一键脚本(Linux,建议 root 执行)
# 用法: sudo bash scripts/deploy_docker.sh
set -euo pipefail
cd "$(dirname "$0")/.."

DOWNLOAD_ROOT="${DOWNLOAD_ROOT:-/opt/douyin/down}"

# 1. 宿主机下载目录(不存在自动创建;root 执行有权限)
mkdir -p "$DOWNLOAD_ROOT"
echo "[deploy] download root: $DOWNLOAD_ROOT"

# 2. 构建并启动
DOWNLOAD_ROOT="$DOWNLOAD_ROOT" docker compose up -d --build

# 3. 状态与提示
docker compose ps
echo
echo "[deploy] done. Web: http://<host>:8787"
echo "[deploy] download root on host: $DOWNLOAD_ROOT"
