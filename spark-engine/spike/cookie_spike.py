"""M0 验证脚本：扫码登录 → 导出 .douyin.com Cookie → 写入帧藏 data/.cookie。

用法（开发机 Windows，headful 需要人操作）：
    cd spark-engine
    python -m playwright install chromium        # 首次
    python spike/cookie_spike.py --cookie-out ../data/.cookie

流程：
    1. 打开 https://www.douyin.com/ （headful，人工扫码登录）
    2. 每 3s 轮询 Cookie，出现 SESSIONID（登录态）即认为登录成功（最长 5 分钟）
    3. 访问 https://www.douyin.com/user/self 确认登录并补齐 Cookie
       （对齐上游 login_desktop_server.py 的 WWW_SELF_URL 身份收集）
    4. 导出 .douyin.com 域 Cookie 拼成 Cookie 头字符串写入目标文件
       （帧藏 F2 侧车每次请求热加载该文件，sidecar/main.py:85）
"""

from __future__ import annotations

import argparse
import asyncio
from pathlib import Path

from playwright.async_api import async_playwright

WWW_URL = "https://www.douyin.com/"
WWW_SELF_URL = "https://www.douyin.com/user/self"
LOGIN_COOKIE_MARKER = "SESSIONID"
POLL_SECONDS = 3
MAX_WAIT_SECONDS = 300


def _douyin_cookie_header(cookies: list[dict]) -> tuple[str, int]:
    pairs = []
    for c in cookies:
        domain = str(c.get("domain") or "")
        name = str(c.get("name") or "")
        value = str(c.get("value") or "")
        if name and value and ".douyin.com" in domain:
            pairs.append(f"{name}={value}")
    return "; ".join(pairs), len(pairs)


async def main() -> None:
    parser = argparse.ArgumentParser(description="douyin cookie export spike")
    parser.add_argument("--cookie-out", default="../data/.cookie")
    parser.add_argument("--headless", action="store_true", help="无头模式（无法人工扫码，仅调试）")
    args = parser.parse_args()

    playwright = await async_playwright().start()
    try:
        browser = await playwright.chromium.launch(
            headless=args.headless,
            args=["--disable-dev-shm-usage", "--no-sandbox", "--no-proxy-server"],
        )
        context = await browser.new_context(viewport={"width": 1400, "height": 900})
        page = await context.new_page()
        await page.goto(WWW_URL, wait_until="domcontentloaded", timeout=60_000)

        print(">>> 请在弹出的浏览器窗口中扫码登录抖音（最长等待 5 分钟）...")
        waited = 0
        logged_in = False
        while waited < MAX_WAIT_SECONDS:
            await asyncio.sleep(POLL_SECONDS)
            waited += POLL_SECONDS
            cookies = await context.cookies()
            names = {c.get("name") for c in cookies if ".douyin.com" in str(c.get("domain") or "")}
            if LOGIN_COOKIE_MARKER in names:
                logged_in = True
                break
            print(f">>> 等待登录中... {waited}s")
        if not logged_in:
            raise SystemExit("等待登录超时，未检测到 SESSIONID Cookie")

        # 访问个人主页确认登录态并补齐 Cookie（对齐上游身份收集行为）。
        await page.goto(WWW_SELF_URL, wait_until="domcontentloaded", timeout=60_000)
        await asyncio.sleep(3)

        cookie_header, count = _douyin_cookie_header(await context.cookies())
        if not cookie_header:
            raise SystemExit("导出失败：未取到 .douyin.com 域 Cookie")

        out = Path(args.cookie_out)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(cookie_header + "\n", encoding="utf-8")
        print(f">>> 已导出 {count} 个 Cookie 到 {out.resolve()}")
        print(">>> 下一步：启动帧藏（非 mock），对任一博主触发扫描验证归档可用。")
        await browser.close()
    finally:
        await playwright.stop()


if __name__ == "__main__":
    asyncio.run(main())
