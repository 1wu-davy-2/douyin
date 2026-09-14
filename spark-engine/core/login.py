# 来源: douyin-sparkflow (DouYinSparkFlow) @ 7c7d9c2
# 许可: PolyForm Noncommercial 1.0.0 —— 仅限非商业用途。本副本为火花融合(spark-engine)平移版。

import asyncio
import logging

from core.browser import get_browser
from utils.config import normalize_unique_id


logger = logging.getLogger(__name__)

READY_SELECTOR = (
    'xpath=//*[contains(@id, "garfish_app_for_douyin_creator_pc_home")]'
    '/div/div[2]/div/div[2]/div[1]'
)
XPATHS = {
    "unique_id": (
        'xpath=//*[contains(@id, "garfish_app_for_douyin_creator_pc_home")]'
        '/div/div[2]/div/div[2]/div[1]/div[2]/div[1]/div[3]'
    ),
    "name": (
        'xpath=//*[contains(@id, "garfish_app_for_douyin_creator_pc_home")]'
        '/div/div[2]/div/div[2]/div[1]/div[2]/div[1]/div[1]/div[1]'
    ),
}


async def wait_for_logged_in_identity(page, timeout_ms=300000):
    await page.wait_for_selector(READY_SELECTOR, timeout=timeout_ms)

    unique_id_element = await page.wait_for_selector(XPATHS["unique_id"], timeout=timeout_ms)
    name_element = await page.wait_for_selector(XPATHS["name"], timeout=timeout_ms)

    unique_id_text = await unique_id_element.inner_text()
    username = (await name_element.inner_text()).strip()
    unique_id = normalize_unique_id(unique_id_text)
    return unique_id, username


async def collect_login_result(page, context, timeout_ms=300000):
    unique_id, username = await wait_for_logged_in_identity(page, timeout_ms=timeout_ms)
    cookies = await context.cookies()
    return {
        "unique_id": unique_id,
        "username": username,
        "cookies": cookies,
    }


async def userLogin(targets=None):
    """上游 CLI 登录流程(融合版不使用:持久化由 Go 侧负责,登录走 login_desktop_server)。
    仅保留作参考;依赖的 upsert_user_account 已随 usersData 持久化层裁剪,故在函数内懒导入。
    """
    from utils.config import upsert_user_account  # 裁剪版 config 保留该函数时才可用

    playwright, browser = await get_browser(GUI=True)
    try:
        context = await browser.new_context()
        page = await context.new_page()

        await page.goto("https://creator.douyin.com/")
        logger.info("Please scan the QR code and finish logging into Douyin Creator Center.")

        login_result = await collect_login_result(page, context)
        logger.info("Unique ID: %s", login_result['unique_id'])
        logger.info("Name: %s", login_result['username'])
        logger.info("Cookies: found %d cookies", len(login_result['cookies']))

        if targets is None:
            raw_targets = input(
                "Open Creator Center -> 互动管理 -> 私信管理 -> 朋友私信, then enter friend display names separated by spaces: "
            )
            targets = [target.strip() for target in raw_targets.split(" ") if target.strip()]

        account = upsert_user_account(
            login_result["unique_id"],
            login_result["username"],
            login_result["cookies"],
            targets,
        )
        logger.info("Login complete. Updated account %s.", account['username'])
        return account
    finally:
        await playwright.stop()
        await browser.close()


if __name__ == "__main__":
    asyncio.run(userLogin())
