# -*- coding: utf-8 -*-
"""诊断：账号 profile 的登录态是否对 creator 私信页有效。"""
import asyncio
import sys

sys.path.insert(0, ".")

from engine.endpoints_browser import get_persistent_browser_context  # noqa: E402

CHAT_URL = "https://creator.douyin.com/creator-micro/data/following/chat"


async def main(profile_name: str) -> None:
    playwright, context, pdir = await get_persistent_browser_context(profile_name)
    try:
        cookies = await context.cookies()
        douyin = [c for c in cookies if "douyin.com" in str(c.get("domain") or "")]
        print("profile_dir :", pdir)
        print("cookie total:", len(cookies), "| douyin:", len(douyin))
        for name in ("sessionid", "sid_guard", "uid_tt", "passport_csrf_token"):
            hit = next((c for c in cookies if c.get("name") == name), None)
            if hit:
                print(f"  {name:20s} {str(hit.get('value'))[:16]}... exp={hit.get('expires')}")
            else:
                print(f"  {name:20s} MISSING")
        page = await context.new_page()
        await page.goto(CHAT_URL, wait_until="domcontentloaded", timeout=60000)
        await asyncio.sleep(6)
        body = await page.inner_text("body")
        print("page url    :", page.url)
        print("login_wall  :", "扫码登录" in body)
        print("body head   :", body.replace("\n", " | ")[:220])
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
