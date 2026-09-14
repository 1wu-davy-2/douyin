"""T1.4: /cookies/export 与 /friends/refresh 端点。

两者都会打开浏览器 profile，与 /send/run 共用 engine.runtime.lock 串行。
契约: docs/HUOHUA_EXECUTION_PLAN.md §4。
"""

from __future__ import annotations

import asyncio
import time
from contextlib import asynccontextmanager
from typing import Optional

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from core.browser import get_persistent_browser_context
from core.friends import CHAT_PAGE_URL, collect_friend_names
from core.send_tools import _normalize_target_name as normalize_target_name
from engine.runtime import runtime


router = APIRouter()


class ProfileRequest(BaseModel):
    profile_name: str


class FriendsRefreshRequest(ProfileRequest):
    # v1 接受但忽略：上游 collect_friend_names 内置滚动/空闲停止逻辑。
    scan: Optional[dict] = None


@asynccontextmanager
async def profile_context(profile_name: str, GUI: bool = False):
    """打开账号的持久化浏览器 profile，退出时保证清理。"""
    playwright, context, profile_dir = await get_persistent_browser_context(profile_name, GUI=GUI)
    try:
        yield context
    finally:
        try:
            await context.close()
        except Exception:  # pragma: no cover - 清理尽力而为
            pass
        try:
            await playwright.stop()
        except Exception:  # pragma: no cover
            pass


def _extract_douyin_cookie_header(cookies: list[dict]) -> str:
    """Playwright cookie 列表 → Cookie 头字符串（只保留 douyin 域）。

    输出格式与帧藏 data/.cookie 一致（纯 "k=v; k2=v2"，sidecar/main.py:85 每次请求热加载）。
    """
    pairs = []
    for c in cookies:
        domain = str(c.get("domain") or "")
        name = str(c.get("name") or "")
        value = str(c.get("value") or "")
        if name and value and (".douyin.com" in domain or domain.endswith("douyin.com")):
            pairs.append(f"{name}={value}")
    return "; ".join(pairs)


@router.post("/cookies/export")
async def cookies_export(req: ProfileRequest):
    if runtime.lock.locked():
        raise HTTPException(status_code=409, detail="task already running")
    async with runtime.lock:
        runtime.mark_start("cookies")
        started = time.time()
        try:
            async with profile_context(req.profile_name) as context:
                page = await context.new_page()
                try:
                    # 先访问 www 域一次，补齐 ttwid 等 www.douyin.com Cookie
                    # （登录桌面身份收集本身也会访问，此处双保险）。
                    await page.goto("https://www.douyin.com/", wait_until="domcontentloaded", timeout=60_000)
                    await asyncio.sleep(2)
                finally:
                    try:
                        await page.close()
                    except Exception:
                        pass
                cookies = await context.cookies()
            cookie_header = _extract_douyin_cookie_header(cookies)
            if not cookie_header:
                raise HTTPException(status_code=502, detail="no douyin cookies in profile; login first")
            pair_count = len([p for p in cookie_header.split("; ") if "=" in p])
            return {
                "cookie": cookie_header,
                "cookie_count": pair_count,
                "exported_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "elapsed_seconds": round(time.time() - started, 2),
            }
        except HTTPException:
            raise
        except Exception as exc:
            raise HTTPException(status_code=502, detail=f"cookie export failed: {exc}")
        finally:
            runtime.mark_end()


@router.post("/friends/refresh")
async def friends_refresh(req: FriendsRefreshRequest):
    if runtime.lock.locked():
        raise HTTPException(status_code=409, detail="task already running")
    async with runtime.lock:
        runtime.mark_start("friends")
        started = time.time()
        try:
            async with profile_context(req.profile_name) as context:
                page = await context.new_page()
                try:
                    await page.goto(CHAT_PAGE_URL, wait_until="commit", timeout=120_000)
                    await asyncio.sleep(1)
                    names = await collect_friend_names(page)
                finally:
                    try:
                        await page.close()
                    except Exception:
                        pass
            friends = []
            seen = set()
            for name in names or []:
                display = str(name).strip()
                key = normalize_target_name(display)
                if not key or key in seen:
                    continue
                seen.add(key)
                friends.append({"key": key, "display_name": display})
            return {
                "friends": friends,
                "complete": True,
                "elapsed_seconds": round(time.time() - started, 2),
            }
        except RuntimeError as exc:
            # 上游 friends.py 的登录类错误原样上抛（含 "login/扫码" 字样）。
            raise HTTPException(status_code=502, detail=str(exc))
        except Exception as exc:
            raise HTTPException(status_code=502, detail=f"friend refresh failed: {exc}")
        finally:
            runtime.mark_end()
