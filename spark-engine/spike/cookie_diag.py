# -*- coding: utf-8 -*-
"""诊断：账号 profile 里的 douyin cookie 到底有多少。"""
import asyncio
import sys

sys.path.insert(0, ".")

from engine.endpoints_browser import get_persistent_browser_context  # noqa: E402


async def main(profile_name: str) -> None:
    playwright, context, pdir = await get_persistent_browser_context(profile_name)
    try:
        cookies = await context.cookies()
        print("profile_dir:", pdir)
        print("total cookies:", len(cookies))
        douyin = [c for c in cookies if "douyin.com" in str(c.get("domain") or "")]
        print("douyin cookies:", len(douyin))
        for c in douyin[:25]:
            print(f"  {c['domain']:24s} {c['name']:28s} len={len(c.get('value') or '')} exp={c.get('expires')}")
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
