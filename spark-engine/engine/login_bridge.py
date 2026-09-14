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
- noVNC 远程桌面反代（滑块验证等人工干预场景）留待后续版本（见执行计划 §12 待办）。
"""

from __future__ import annotations

import asyncio
import os
import shutil
import time
from pathlib import Path

import requests
from fastapi import APIRouter, HTTPException
from fastapi.responses import Response
from pydantic import BaseModel

from engine.endpoints_browser import _extract_douyin_cookie_header, profile_context
from engine.runtime import runtime
from utils.config import profile_root


router = APIRouter()

LD_BASE = os.getenv("SPARKFLOW_LOGIN_DESKTOP_API_URL", "http://127.0.0.1:18090").rstrip("/")
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
