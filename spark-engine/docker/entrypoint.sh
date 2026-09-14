# 来源: docs/HUOHUA_EXECUTION_PLAN.md §8(引擎容器入口)。
# 后台拉起登录桌面全家桶(脚本自带 X 等待与进程守护循环),
# 前台 exec 引擎主进程 —— 引擎退出即容器退出,restart: unless-stopped 兜底。

#!/usr/bin/env bash
set -euo pipefail

export SPARK_ENGINE_HOST="${SPARK_ENGINE_HOST:-0.0.0.0}"
# 登录 profile 与浏览器 profile 都落在 /app/state 卷内,容器重建不丢登录态
export LOGIN_PROFILE_DIR="${LOGIN_PROFILE_DIR:-/app/state/login-profile}"
export DISPLAY="${DISPLAY:-:99}"
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-/ms-playwright}"

mkdir -p "$LOGIN_PROFILE_DIR" /app/logs/login_desktop /app/state/browser-profiles

# scripts/start_login_desktop.sh 以 exec 结尾,子 shell 里整脚本后台化即可
bash scripts/start_login_desktop.sh &

exec python main.py --host "$SPARK_ENGINE_HOST" \
  --port "${SPARK_ENGINE_PORT:-18788}" \
  --token "${SPARK_ENGINE_TOKEN:-}"
