# -*- coding: utf-8 -*-
"""诊断：模拟发送流程的 open_chat_page 登录墙检测，验证不再瞬态误判。"""
import asyncio
import sys

sys.path.insert(0, ".")

from core.send_tools import ensure_not_login_required  # noqa: E402
from engine.endpoints_browser import get_persistent_browser_context  # noqa: E402

CHAT_URL = "https://creator.douyin.com/creator-micro/data/following/chat"


async def main(profile_name: str) -> None:
    playwright, context, pdir = await get_persistent_browser_context(profile_name)
    try:
        page = await context.new_page()
        await page.goto(CHAT_URL, wait_until="domcontentloaded", timeout=60000)
        # 复刻 send_tools 的实际时序：goto → sleep(3) → 检测
        await asyncio.sleep(3)
        try:
            await ensure_not_login_required(page, "diag", "open_chat_page")
            print("RESULT: PASS - 未误判 login_required")
        except RuntimeError as exc:
            print("RESULT: RAISED ->", str(exc)[:200])
        print("final url:", page.url)
    finally:
        try:
            await context.close()
        except Exception:
            pass
        try:
            await playwright.stop()
        except Exception:
            pass


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1]))
