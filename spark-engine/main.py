#!/usr/bin/env python3
"""spark-engine —— 火花引擎入口。

    python main.py --port 18788 --token <t>
    环境变量兜底: SPARK_ENGINE_PORT / SPARK_ENGINE_HOST / SPARK_ENGINE_TOKEN

API 契约: docs/HUOHUA_EXECUTION_PLAN.md §4。
设计: 引擎是无状态 Python 服务(Playwright 浏览器动作闭环)，不连数据库；
      一切业务状态通过 HTTP 响应返回给 Go 主后端入库(Go 是 SQLite 唯一写者)。
"""

from __future__ import annotations

import argparse
import os

import uvicorn


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="spark-engine (huohua)")
    parser.add_argument("--host", default=os.getenv("SPARK_ENGINE_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.getenv("SPARK_ENGINE_PORT", "18788")))
    parser.add_argument("--token", default=os.getenv("SPARK_ENGINE_TOKEN", ""))
    return parser


def main() -> None:
    args = build_parser().parse_args()
    # 下发给 engine.app 的 token 校验中间件读取环境变量。
    os.environ["SPARK_ENGINE_TOKEN"] = args.token

    from engine.app import create_app

    app = create_app()
    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
