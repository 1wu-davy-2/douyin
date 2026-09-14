"""T1.5: 登录桌面桥接（engine ←→ login_desktop_server.py）。

设计：
- 引擎不重实现登录。上游 login_desktop_server.py（原样平移，独立 FastAPI 进程，
  监听 127.0.0.1:18090，env LOGIN_PROFILE_DIR 指定登录 profile 目录）持有登录浏览器；
  本模块把引擎的 /login/* 请求转发给它。
- 导出流程（POST /login/export）：
    1. 转发 POST {LD}/export 取登录身份（best-effort 解析 unique_id/nickname）
    2. 转发 POST {LD}/close 关闭登录浏览器（释放 profile 文件句柄）
    3. 把登录 profile 目录整体复制为账号 profile（uid-{unique_id}）
    4. 从账号 profile 读 Cookie → 拼成 Cookie 头字符串返回
      （Go 收到后写 data/.cookie，帧藏 F2 侧车热加载 → 归档复用登录态）
- noVNC 远程桌面反代（/login/vnc/*，平移上游 webui/app.py 的 login-desktop/proxy 段）：
  HTTP 资产转发到 websockify 的静态 noVNC 目录；WebSocket 双向中继到 websockify。
  引擎的 token 中间件只覆盖 HTTP，WS 端点自行校验 X-Engine-Token（Go 反代会带上）。
"""

from __future__ import annotations

import asyncio
import os
import shutil
import time
import urllib.error
import urllib.request
from pathlib import Path
from urllib.parse import quote

import requests
import websockets
from fastapi import APIRouter, HTTPException, Request, WebSocket, WebSocketDisconnect
from fastapi.responses import PlainTextResponse, RedirectResponse, Response
from pydantic import BaseModel
from websockets.exceptions import ConnectionClosed

from engine.endpoints_browser import _extract_douyin_cookie_header, profile_context
from engine.runtime import runtime
from utils.config import profile_root


router = APIRouter()

LD_BASE = os.getenv("SPARKFLOW_LOGIN_DESKTOP_API_URL", "http://127.0.0.1:18090").rstrip("/")
NOVNC_HTTP_URL = os.getenv(
    "SPARKFLOW_LOGIN_DESKTOP_NOVNC_URL", "http://127.0.0.1:8788"
).rstrip("/")
NOVNC_WS_URL = os.getenv(
    "SPARKFLOW_LOGIN_DESKTOP_NOVNC_WS_URL", "ws://127.0.0.1:8788/websockify"
)
LOGIN_PROFILE_DIR = Path(
    os.getenv("LOGIN_PROFILE_DIR")
    or str(Path(profile_root()).parent / "login-profile")
)


class ExportResult(BaseModel):
    unique_id: str
    nickname: str
    profile_name: str
    cookie: str
    cookie_count: int


def _ld_request(method: str, path: str, timeout: int = 30) -> requests.Response:
    try:
        return requests.request(method, f"{LD_BASE}{path}", timeout=timeout)
    except requests.RequestException as exc:
        raise HTTPException(
            status_code=502,
            detail=f"login desktop not reachable at {LD_BASE}: {exc}",
        )


_IDENTITY_KEY_CANDIDATES = ("unique_id", "douyin_id", "uid", "sec_uid")
_NICKNAME_KEY_CANDIDATES = ("nickname", "display_name", "name", "screen_name")


def _find_first(result: dict, keys: tuple[str, ...]) -> str:
    """在导出结果（可能嵌套）里按候选键名做 best-effort 查找。"""
    def walk(node) -> str:
        if isinstance(node, dict):
            for key in keys:
                value = node.get(key)
                if isinstance(value, str) and value.strip():
                    return value.strip()
            for value in node.values():
                found = walk(value)
                if found:
                    return found
        elif isinstance(node, list):
            for item in node:
                found = walk(item)
                if found:
                    return found
        return ""

    return walk(result or {})


@router.get("/login/health")
async def login_health():
    resp = _ld_request("GET", "/health", timeout=5)
    return {"ok": resp.status_code == 200, "status_code": resp.status_code}


@router.post("/login/open")
async def login_open():
    resp = _ld_request("POST", "/open-login", timeout=60)
    if resp.status_code >= 400:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    return {"status": "opened", "upstream": resp.json() if resp.content else {}}


@router.get("/login/qr")
async def login_qr():
    resp = _ld_request("GET", "/qr", timeout=30)
    if resp.status_code == 200:
        return Response(content=resp.content, media_type="image/png",
                        headers={"Cache-Control": "no-store, max-age=0"})
    raise HTTPException(status_code=resp.status_code, detail=resp.text or "QR not ready")


@router.post("/login/refresh-qr")
async def login_refresh_qr():
    resp = _ld_request("POST", "/refresh-qr", timeout=30)
    if resp.status_code >= 400:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    return {"status": "refreshed"}


@router.get("/login/status")
async def login_status():
    resp = _ld_request("GET", "/status", timeout=10)
    if resp.status_code >= 400:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    try:
        return resp.json()
    except ValueError:
        return {"raw": resp.text}


@router.post("/login/close")
async def login_close():
    resp = _ld_request("POST", "/close", timeout=30)
    if resp.status_code >= 400:
        raise HTTPException(status_code=resp.status_code, detail=resp.text)
    return {"status": "closed"}


@router.post("/login/export")
async def login_export():
    """导出登录身份并把登录态落成账号 profile + Cookie 头（见模块 docstring）。"""
    if runtime.lock.locked():
        raise HTTPException(status_code=409, detail="task already running")
    async with runtime.lock:
        runtime.mark_start("login")
        try:
            resp = _ld_request("POST", "/export", timeout=120)
            if resp.status_code >= 400:
                raise HTTPException(status_code=resp.status_code, detail=resp.text)
            try:
                payload = resp.json()
            except ValueError:
                raise HTTPException(status_code=502, detail="login export returned non-JSON")
            result = payload.get("result") if isinstance(payload, dict) else payload

            unique_id = _find_first(result if isinstance(result, dict) else {}, _IDENTITY_KEY_CANDIDATES)
            nickname = _find_first(result if isinstance(result, dict) else {}, _NICKNAME_KEY_CANDIDATES)
            if not unique_id:
                raise HTTPException(
                    status_code=502,
                    detail=f"identity parse failed; raw keys={sorted(result.keys()) if isinstance(result, dict) else type(result).__name__}",
                )

            # 关闭登录浏览器，释放 login-profile 目录句柄。
            try:
                _ld_request("POST", "/close", timeout=30)
            except HTTPException:
                pass
            await asyncio.sleep(2)

            src = LOGIN_PROFILE_DIR
            if not src.exists():
                raise HTTPException(status_code=502, detail=f"login profile dir missing: {src}")
            profile_name = f"uid-{unique_id}"
            dst = Path(profile_root()) / profile_name
            dst.mkdir(parents=True, exist_ok=True)
            shutil.copytree(src, dst, dirs_exist_ok=True,
                            ignore=shutil.ignore_patterns("LOCK*", "*.tmp", "Singleton*"))

            # 从账号 profile 读 Cookie（无需访问页面，profile 已带登录态）。
            async with profile_context(profile_name) as context:
                cookies = await context.cookies()
            cookie_header = _extract_douyin_cookie_header(cookies)

            return {
                "unique_id": unique_id,
                "nickname": nickname,
                "profile_name": profile_name,
                "cookie": cookie_header,
                "cookie_count": len([p for p in cookie_header.split("; ") if "=" in p]),
                "exported_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            }
        finally:
            runtime.mark_end()


# ---------- noVNC 远程桌面反代(/login/vnc/*,平移上游 webui/app.py proxy 段) ----------


def _fetch_novnc_asset(asset_path: str, query: str = ""):
    safe_path = quote(str(asset_path or "vnc.html").lstrip("/"), safe="/._-")
    url = f"{NOVNC_HTTP_URL}/{safe_path}"
    if query:
        url = f"{url}?{query}"
    request = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(request, timeout=20) as upstream:
            headers = {
                key: value
                for key, value in upstream.headers.items()
                if key.lower()
                in {"content-type", "content-encoding", "cache-control", "etag", "last-modified"}
            }
            return upstream.status, headers, upstream.read()
    except (urllib.error.URLError, TimeoutError) as exc:
        raise RuntimeError(f"noVNC proxy failed: {exc}") from exc


@router.get("/login/vnc")
async def login_vnc_root():
    """根路径重定向到 noVNC 页面(path 参数指向引擎侧 WS 反代)。"""
    return RedirectResponse(
        "/login/vnc/vnc.html?autoconnect=1&resize=scale&view_only=0&path=login/vnc/websockify",
        status_code=307,
    )


@router.get("/login/vnc/{asset_path:path}")
async def login_vnc_asset(asset_path: str, request: Request):
    try:
        status, headers, content = await asyncio.to_thread(
            _fetch_novnc_asset, asset_path, request.url.query
        )
        return Response(content=content, status_code=status, headers=headers)
    except RuntimeError as exc:
        return PlainTextResponse(str(exc), status_code=502)


async def _run_ws_relays(*coroutines):
    tasks = {asyncio.create_task(coro) for coro in coroutines}
    try:
        _, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        for task in pending:
            task.cancel()
        results = await asyncio.gather(*tasks, return_exceptions=True)
        for result in results:
            if isinstance(result, (ConnectionClosed, WebSocketDisconnect, asyncio.CancelledError)):
                continue
            if isinstance(result, BaseException):
                raise result
    finally:
        for task in tasks:
            if not task.done():
                task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)


@router.websocket("/login/vnc/websockify")
async def login_vnc_websockify(websocket: WebSocket):
    """noVNC WebSocket 双向中继。token 中间件不覆盖 WS,这里自行校验。"""
    expected = str(os.getenv("SPARK_ENGINE_TOKEN", "")).strip()
    provided = websocket.headers.get("x-engine-token", "") or str(
        websocket.query_params.get("token", "")
    )
    if not expected or provided != expected:
        await websocket.close(code=4401)
        return

    requested_protocols = [
        item.strip()
        for item in websocket.headers.get("sec-websocket-protocol", "").split(",")
        if item.strip()
    ]
    accepted = False
    try:
        async with websockets.connect(
            NOVNC_WS_URL,
            subprotocols=requested_protocols or None,
            open_timeout=10,
            close_timeout=5,
        ) as upstream:
            await websocket.accept(subprotocol=upstream.subprotocol)
            accepted = True

            async def client_to_upstream():
                while True:
                    message = await websocket.receive()
                    if message["type"] == "websocket.disconnect":
                        return
                    if message.get("bytes") is not None:
                        await upstream.send(message["bytes"])
                    elif message.get("text") is not None:
                        await upstream.send(message["text"])

            async def upstream_to_client():
                async for message in upstream:
                    if isinstance(message, bytes):
                        await websocket.send_bytes(message)
                    else:
                        await websocket.send_text(message)

            await _run_ws_relays(client_to_upstream(), upstream_to_client())
    except (ConnectionClosed, WebSocketDisconnect):
        pass
    except Exception:
        if not accepted:
            await websocket.close(code=1011)
