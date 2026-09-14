"""spark-engine FastAPI 应用壳。

API 契约: docs/HUOHUA_EXECUTION_PLAN.md §4。
- 所有端点要求请求头 X-Engine-Token（与启动参数 --token 一致）；token 未配置时整体拒绝(503)。
- 错误统一 非2xx + {"detail": "..."}，与帧藏 F2 侧车风格一致。
- 打开浏览器的三类操作(/send/run /friends/refresh /cookies/export)共用 engine.runtime.lock 串行。
"""

from __future__ import annotations

import os

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from engine.runtime import runtime

ENGINE_VERSION = 1


def _token() -> str:
    return str(os.getenv("SPARK_ENGINE_TOKEN", "")).strip()


def create_app() -> FastAPI:
    app = FastAPI(title="spark-engine", version=str(ENGINE_VERSION))

    @app.middleware("http")
    async def token_guard(request: Request, call_next):
        token = _token()
        if not token:
            return JSONResponse({"detail": "engine token not configured"}, status_code=503)
        if request.headers.get("X-Engine-Token") != token:
            return JSONResponse({"detail": "unauthorized"}, status_code=401)
        return await call_next(request)

    @app.get("/health")
    async def health():
        return {
            "status": "ok",
            "version": ENGINE_VERSION,
            "task_running": runtime.busy(),
            "task_kind": runtime.task_kind,
        }

    # ---- 业务端点在各阶段任务中逐个挂载 -------------------------------------
    from engine import endpoints_browser  # noqa: E402  (T1.4: cookies/friends)

    app.include_router(endpoints_browser.router)

    try:
        from engine import endpoints_send  # noqa: E402  (T1.6: send/run)

        app.include_router(endpoints_send.router)
    except ImportError:  # pragma: no cover - send_loop 未就绪时不阻塞 /health
        pass

    try:
        from engine import login_bridge  # noqa: E402  (T1.5: login desktop)

        login_bridge.mount(app)
    except ImportError:  # pragma: no cover - 登录桌面未就绪时不阻塞 /health
        pass

    return app
