"""spark-engine 引擎运行时状态（进程内单例）。

- engine_lock: 全局互斥锁。/send/run、/friends/refresh、/cookies/export 三类
  会打开浏览器 profile 的操作全部串行，避免同一 profile 被两个浏览器实例打开。
- task_running: 当前是否有一个发送/刷新任务在执行（/health 暴露给 Go 判断）。
"""

from __future__ import annotations

import asyncio
import time


class Runtime:
    def __init__(self) -> None:
        self.lock = asyncio.Lock()
        self.task_kind = ""       # "" | "send" | "friends" | "cookies" | "login"
        self.task_started_at = 0.0

    def mark_start(self, kind: str) -> None:
        self.task_kind = kind
        self.task_started_at = time.time()

    def mark_end(self) -> None:
        self.task_kind = ""
        self.task_started_at = 0.0

    def busy(self) -> bool:
        return self.task_kind != ""


runtime = Runtime()
