"""T1.6: POST /send/run 端点。

契约: docs/HUOHUA_EXECUTION_PLAN.md §4。
- 与 /cookies/export、/friends/refresh 共用 engine.runtime.lock（全局单任务）。
- v1 同步等待发送完成（Go 侧超时 10 分钟）；上游单账号一轮发送即为该耗时上限量级。
"""

from __future__ import annotations

import time
from typing import Optional

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from engine.runtime import runtime
from engine.send_loop import run_send


router = APIRouter()


class SendRunRequest(BaseModel):
    profile_name: str
    unique_id: str = ""
    account_name: str = ""
    targets: list[str] = Field(default_factory=list)
    # Playwright cookie dict 列表（可选；持久化 profile 已有登录态时可为空）
    cookies: list[dict] = Field(default_factory=list)
    # Go 下发的 send_config（§5.4 的 message* + sendStrategy + friendScan 子集）
    config: dict = Field(default_factory=dict)
    # 可选：每目标上一条已发消息（防重复文案）{"张三": "🤩今日火花+1"}
    previous_messages: Optional[dict[str, str]] = None


@router.post("/send/run")
async def send_run(req: SendRunRequest):
    if not req.targets:
        return {"results": [], "account_failure": {}, "started_at": _now(), "finished_at": _now()}
    if runtime.lock.locked():
        raise HTTPException(status_code=409, detail="task already running")

    async with runtime.lock:
        runtime.mark_start("send")
        started = time.time()
        try:
            out = await run_send(
                unique_id=req.unique_id,
                account_name=req.account_name or req.profile_name,
                targets=req.targets,
                cookies=req.cookies,
                config=req.config,
                previous_messages=req.previous_messages,
            )
            out["started_at"] = _now()
            out["finished_at"] = _now()
            out["elapsed_seconds"] = round(time.time() - started, 2)
            return out
        except HTTPException:
            raise
        except Exception as exc:
            raise HTTPException(status_code=502, detail=f"send run failed: {exc}")
        finally:
            runtime.mark_end()


def _now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
