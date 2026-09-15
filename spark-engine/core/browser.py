# 来源: douyin-sparkflow (DouYinSparkFlow/core/browser.py) @ 7c7d9c2 —— 火花融合改造版
# 许可: PolyForm Noncommercial 1.0.0 —— 仅限非商业用途。
# 改造说明(相对上游):
#   1. 去除 rich 依赖，改用标准 logging；
#   2. 网络模式改为环境变量 SPARK_ENGINE_NETWORK_MODE（默认 direct，仅 direct/mihomo 二选一），
#      不再读 webui_settings.json；
#   3. Playwright 浏览器缺失时抛 RuntimeError（引擎常驻进程不应 sys.exit）；
#   4. headless 开关改为 SPARK_ENGINE_HEADFUL=1（本地调试用）。

import logging
import os
import re
import sys
import traceback
from pathlib import Path

from playwright.async_api import async_playwright

from utils.config import DEBUG, Environment, get_environment, profile_root as config_profile_root


logger = logging.getLogger(__name__)

PLAYWRIGHT_BROWSERS_PATH = "../chrome"
# 改造说明(火花融合): 上游 DEFAULT_PROFILE_ROOT 是 Docker 绝对路径
# "/opt/douyin-sparkflow/state/browser-profiles"。融合版统一走 utils.config.profile_root()
# (repo_root()/state/browser-profiles,env SPARKFLOW_BROWSER_PROFILE_ROOT 优先)——
# 否则未设 env 时 core 与 config/login_bridge 两套默认值分叉成两个 profile 根,
# 表现为 export 复制到 A 目录、profile_context 去 B 目录读 → cookie_count=0。


def configure_playwright_environment():
    if os.getenv("PLAYWRIGHT_BROWSERS_PATH"):
        return

    env = get_environment()
    if env == Environment.PACKED:
        bundle_path = Path(sys.executable).resolve().parent / PLAYWRIGHT_BROWSERS_PATH
    else:
        bundle_path = Path(__file__).resolve().parent / PLAYWRIGHT_BROWSERS_PATH

    if bundle_path.exists():
        os.environ["PLAYWRIGHT_BROWSERS_PATH"] = str(bundle_path.resolve())


def _headless_for(GUI=False):
    headful_env = str(os.getenv("SPARK_ENGINE_HEADFUL") or "").strip().lower()
    if headful_env in {"1", "true", "yes", "on"}:
        return False
    headless = not GUI
    if get_environment() == Environment.LOCAL and DEBUG:
        headless = False
    return headless


def _browser_args():
    return [
        "--disable-dev-shm-usage",
        "--no-sandbox",
    ]


def _douyin_network_mode():
    return str(os.getenv("SPARK_ENGINE_NETWORK_MODE") or "direct").strip().lower()


def douyin_network_modes():
    # direct 是默认；mihomo 仅在显式指定时启用（v1 不迁移 Mihomo 容器）。
    mode = _douyin_network_mode()
    if mode == "mihomo":
        return ("mihomo",)
    return ("direct",)


def _douyin_browser_proxy(network_mode=None):
    mode = str(network_mode or _douyin_network_mode()).strip().lower()
    if mode != "mihomo":
        return None
    return str(
        os.getenv("SPARK_ENGINE_DOUYIN_PROXY_URL") or "http://127.0.0.1:7890"
    ).strip() or None


def _browser_launch_options(GUI=False, network_mode=None):
    args = _browser_args()
    proxy = _douyin_browser_proxy(network_mode=network_mode)
    if proxy:
        return {
            "headless": _headless_for(GUI),
            "args": args,
            "proxy": {"server": proxy},
        }
    args.append("--no-proxy-server")
    return {
        "headless": _headless_for(GUI),
        "args": args,
    }


async def select_douyin_network_mode(target_url):
    # 任务开始前探测可用出口（v1 仅 direct，等于一次可达性预检）。
    failures = []
    for network_mode in douyin_network_modes():
        playwright = browser = page = None
        try:
            playwright, browser = await get_browser(network_mode=network_mode)
            page = await browser.new_page()
            response = await page.goto(target_url, wait_until="commit", timeout=30000)
            status = response.status if response is not None else None
            if status is not None and status < 500:
                return network_mode
            failures.append(f"{network_mode}: HTTP {status}")
        except Exception as exc:
            failures.append(f"{network_mode}: {exc}")
        finally:
            if page:
                await page.close()
            if browser:
                await browser.close()
            if playwright:
                await playwright.stop()
    raise RuntimeError(f"Douyin network preflight failed: {'; '.join(failures)}")


def sanitize_profile_name(value):
    raw = str(value or "").strip()
    if not raw:
        raw = "unknown"
    safe = re.sub(r"[^0-9A-Za-z._-]+", "_", raw)
    safe = safe.strip("._-") or "unknown"
    return safe[:80]


def browser_profile_root(root=None):
    configured = (
        root
        or os.getenv("SPARKFLOW_BROWSER_PROFILE_ROOT")
    )
    if configured:
        return Path(configured)
    # 未设 env/参数 → 与 login_bridge 的复制目标保持同一默认(单一事实来源)。
    return config_profile_root()


async def get_browser(GUI=False, network_mode=None):
    configure_playwright_environment()

    try:
        playwright = await async_playwright().start()
        browser = await playwright.chromium.launch(**_browser_launch_options(GUI, network_mode=network_mode))
        return playwright, browser
    except Exception as exc:
        if "Executable doesn't exist" in str(exc):
            logger.error("Playwright browser is missing; run: python -m playwright install chromium")
            raise RuntimeError("playwright chromium not installed") from exc
        traceback.print_exc()
        raise


async def get_persistent_browser_context(profile_name, GUI=False, root=None, network_mode=None):
    configure_playwright_environment()

    profile_dir = browser_profile_root(root) / sanitize_profile_name(profile_name)
    profile_dir.mkdir(parents=True, exist_ok=True)

    try:
        playwright = await async_playwright().start()
        launch_options = _browser_launch_options(GUI, network_mode=network_mode)
        launch_options["viewport"] = {"width": 1600, "height": 1000}
        context = await playwright.chromium.launch_persistent_context(
            str(profile_dir),
            **launch_options,
        )
        return playwright, context, profile_dir
    except Exception as exc:
        if "Executable doesn't exist" in str(exc):
            logger.error("Playwright browser is missing; run: python -m playwright install chromium")
            raise RuntimeError("playwright chromium not installed") from exc
        traceback.print_exc()
        raise
