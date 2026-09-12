#!/usr/bin/env python3
"""抖音归档工具 v2 —— 签名侧车(thin F2 signing service)。

职责边界(见 docs/api.md "侧车契约"):
    只做「签名 + 拉取抖音接口 + 结构裁剪」,不做重试、不入库。
    任何失败一律 非200 + {"error":"..."},绝不返回 200 + 空数据假装成功。

启动:
    python sidecar/main.py --port 18787 --token <t> [--mock]

设计:
    - 纯标准库 http.server.ThreadingHTTPServer,无框架。
    - F2 延迟到首次真实请求才 import(--mock 零 F2 开销)。
    - F2 为异步库,在线程内用 asyncio.run 驱动;全局锁串行化所有 F2 调用。
    - Cookie 每次请求时从 data/.cookie 重新读取(热加载,Go 写文件即生效)。
    - F2 底层 BaseCrawler._fetch_get_json 吞错返回 {}(旧版漏扫祸根),
      侧车层对「关键字段缺失」一律视为错误上抛。
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import secrets
import socket
import sys
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable
from urllib.parse import parse_qs, urlparse

# ---------------------------------------------------------------------------
# 常量与全局状态
# ---------------------------------------------------------------------------

DEFAULT_PORT = 18787
MAX_COUNT = 20          # 抖音单页上限,传入更大值截断
F2_TIMEOUT = 15         # F2 httpx 超时(秒)

# F2 是同步阻塞式调用(asyncio.run 在线程内跑),用全局锁串行化,单用户工具足够
F2_LOCK = threading.Lock()

MOCK_TOTAL = 502        # mock:共 502 个作品,26 页(25*20 + 2)
MOCK_AWEME_COUNT = 502
MOCK_BASE_TS = 1735689600  # 2025-01-01T00:00:00Z,第 1 个(最新)作品的发布时间


class SidecarError(Exception):
    """带 HTTP 状态码的业务错误,message 即 {"error": ...} 内容。"""

    def __init__(self, status: int, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.message = message


def data_root() -> Path:
    """数据根目录:DY_DATA_DIR 环境变量优先,否则 main.py 上级目录。"""
    env = os.environ.get("DY_DATA_DIR", "").strip()
    if env:
        return Path(env)
    return Path(__file__).resolve().parent.parent


def cookie_file_path() -> Path:
    """data/.cookie 的路径。

    约定:cookie 文件为 <数据目录>/data/.cookie;默认数据目录 = main.py 上级目录。
    若 DY_DATA_DIR 指向的目录下没有 data/ 子目录,则视为 DY_DATA_DIR 本身即
    数据目录(即直接读 $DY_DATA_DIR/.cookie),两种用法都兼容。
    """
    root = data_root()
    if root.name == "data":
        return root / ".cookie"
    if (root / "data").is_dir():
        return root / "data" / ".cookie"
    return root / ".cookie"


def read_cookie() -> str:
    """每次请求时读 cookie 文件(热加载,无需重启)。空/不存在返回 ""。"""
    path = cookie_file_path()
    try:
        return path.read_text(encoding="utf-8", errors="replace").strip()
    except OSError:
        return ""


def require_cookie() -> str:
    cookie = read_cookie()
    if not cookie:
        raise SidecarError(503, "cookie not configured")
    return cookie


# ---------------------------------------------------------------------------
# Mock 模式(与 Go 侧 MockProvider 行为一致,用于侧车自测/联调)
# ---------------------------------------------------------------------------


def mock_mix_for(pos: int) -> tuple[str, str] | tuple[None, None]:
    """pos 为 1 起的全局序号:每第 10 个 → 合集A,第 15 个 → 合集B。"""
    if pos % 10 == 0:
        return "mock_mix_1", "Mock合集A"
    if pos % 15 == 0:
        return "mock_mix_2", "Mock合集B"
    return None, None


def rfc3339(ts: int) -> str:
    """Unix 秒 → RFC3339 UTC,如 2025-01-01T00:00:00Z。"""
    return datetime.fromtimestamp(int(ts), timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def mock_profile(sec_uid: str) -> dict[str, Any]:
    return {
        "sec_uid": sec_uid,
        "nickname": "Mock博主",
        "avatar_url": "",
        "aweme_count": MOCK_AWEME_COUNT,
        "signature": "Mock 模式签名",
    }


def mock_posts(sec_uid: str, cursor: int, count: int) -> dict[str, Any]:
    start = max(0, cursor)
    end = min(start + count, MOCK_TOTAL)
    items = []
    for idx in range(start, end):
        pos = idx + 1
        mix_id, mix_name = mock_mix_for(pos)
        kind, image_count = mock_kind_for(idx)
        mock_type = "daily" if idx % 9 == 8 else kind
        items.append(
            {
                "item_id": f"mock_{pos:04d}",
                "title": f"Mock作品 #{pos}",
                "cover_url": f"/mockcdn/mock_gallery_{pos}/cover.jpg",
                "duration": 0 if kind == "image" else 30 + (pos % 25),
                "published_at": rfc3339(MOCK_BASE_TS - (pos - 1) * 86400),
                "mix_id": mix_id,
                "mix_name": mix_name,
                "type": mock_type,
                "image_count": image_count,
            }
        )
    has_more = end < MOCK_TOTAL
    return {
        "items": items,
        "has_more": has_more,
        "next_cursor": str(end) if has_more else None,
    }


MOCK_WORK_VARIANTS = [
    {"quality": "1080p", "width": 1080, "height": 1920, "bitrate": 3000000, "size_bytes": 16875000},
    {"quality": "720p", "width": 720, "height": 1280, "bitrate": 1500000, "size_bytes": 8437500},
    {"quality": "540p", "width": 540, "height": 960, "bitrate": 800000, "size_bytes": 4500000},
]

# Mock 图集尺寸(竖屏 3:4);与 Go MockProvider 保持一致
MOCK_IMAGE_WIDTH = 1080
MOCK_IMAGE_HEIGHT = 1440

# 契约要求 video 档位携带 url(恒等于首个候选)与 urls(候选列表)。
# mock 档位用 /mockcdn/{item}/{quality}.mp4 相对地址,与 Go MockProvider 对齐。
def mock_variants(item_id: str) -> list[dict[str, Any]]:
    out = []
    for v in MOCK_WORK_VARIANTS:
        entry = dict(v)
        url = f"/mockcdn/{item_id}/{v['quality']}.mp4"
        entry["url"] = url
        entry["urls"] = [url]
        out.append(entry)
    return out


def mock_kind_for(idx: int) -> tuple[str, int]:
    """0 起全局序号 -> (type, image_count)。

    每第 7 个作品(index%7==6)为图集,3-6 张图(数量=3+index%4);
    其中 index%14==13 的再带 1 段实况(/work 层面体现,/posts 不携带)。
    与 Go MockProvider 的 mockKindFor 逐字对齐。
    """
    if idx % 7 == 6:
        return "image", 3 + idx % 4
    return "video", 0


def _mock_pos(item_id: str) -> int | None:
    """mock_0007 -> 7;非 mock_NNNN 形态返回 None(视为 video 作品)。"""
    s = str(item_id)
    if not s.startswith("mock_"):
        return None
    try:
        pos = int(s[len("mock_"):])
    except ValueError:
        return None
    return pos if pos > 0 else None


def mock_work(item_id: str) -> dict[str, Any]:
    pos = _mock_pos(item_id)
    idx = pos - 1 if pos is not None else -1
    kind, image_count = mock_kind_for(idx) if idx >= 0 else ("video", 0)
    if kind == "image":
        images = [
            {
                "url": f"/mockcdn/{item_id}/img{n}.jpg",
                "urls": [f"/mockcdn/{item_id}/img{n}.jpg"],
                "width": MOCK_IMAGE_WIDTH,
                "height": MOCK_IMAGE_HEIGHT,
            }
            for n in range(1, image_count + 1)
        ]
        live_videos: list[dict[str, Any]] = []
        if idx % 14 == 13:
            live_videos = [
                {"url": f"/mockcdn/{item_id}/live1.mp4", "urls": [f"/mockcdn/{item_id}/live1.mp4"]}
            ]
        return {
            "item_id": item_id,
            "title": f"Mock作品 {item_id}",
            "type": "image",
            "cover_url": f"/mockcdn/{item_id}/cover.jpg",
            "duration": 0,
            "variants": [],
            "images": images,
            "live_videos": live_videos,
        }
    return {
        "item_id": item_id,
        "title": f"Mock作品 {item_id}",
        "type": "video",
        "cover_url": f"/mockcdn/{item_id}/cover.jpg",
        "duration": 45,
        "variants": mock_variants(item_id),
    }


# ---------------------------------------------------------------------------
# F2 集成(延迟 import;全局锁串行化;缺失字段一律视为错误)
# ---------------------------------------------------------------------------


def f2_kwargs(cookie: str) -> dict[str, Any]:
    return {
        "headers": {
            "User-Agent": (
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                "Chrome/130.0 Safari/537.36"
            ),
            "Referer": "https://www.douyin.com/",
        },
        "proxies": {"http://": None, "https://": None},
        "timeout": F2_TIMEOUT,
        "cookie": cookie,
    }


def _first_url(url_list: Any) -> str:
    if isinstance(url_list, list):
        for url in url_list:
            if url:
                return str(url)
    return ""


def _url_candidates(url_list: Any) -> list[str]:
    """全部去重后的候选地址:douyin 每个 play_addr 常带多个 CDN 节点,
    主节点可能对部分请求 403,调用方(Go 下载器)应按序尝试。"""
    if not isinstance(url_list, list):
        return []
    out: list[str] = []
    for url in url_list:
        if url:
            s = str(url)
            if s not in out:
                out.append(s)
    return out


def _image_entry(img: Any) -> dict[str, Any] | None:
    """aweme.images[] 单项 -> {url, urls, width, height};无有效地址返回 None。"""
    if not isinstance(img, dict):
        return None
    candidates = _url_candidates(img.get("url_list"))
    if not candidates:
        return None
    try:
        width = int(img.get("width") or 0)
    except (TypeError, ValueError):
        width = 0
    try:
        height = int(img.get("height") or 0)
    except (TypeError, ValueError):
        height = 0
    return {"url": candidates[0], "urls": candidates, "width": width, "height": height}


def _live_entry(img: Any) -> dict[str, Any] | None:
    """图集图片上的实况/动图视频片段 -> {url, urls};来自
    images[].video.play_addr.url_list,退化为 images[].video.url_list。"""
    if not isinstance(img, dict):
        return None
    video = img.get("video")
    if not isinstance(video, dict):
        return None
    candidates = _url_candidates((video.get("play_addr") or {}).get("url_list"))
    if not candidates:
        candidates = _url_candidates(video.get("url_list"))
    if not candidates:
        return None
    return {"url": candidates[0], "urls": candidates}


def _quiet_f2_logs() -> None:
    """F2 库会往 stdout 打多行 ERROR 堆栈,压掉以维持侧车一行一条日志的约定。"""
    import logging

    logging.getLogger("f2").setLevel(logging.CRITICAL)
    for name in list(logging.root.manager.loggerDict):
        if name == "f2" or name.startswith("f2."):
            logging.getLogger(name).setLevel(logging.CRITICAL)


def run_f2(coro_factory: Callable[[], Any]) -> Any:
    """在全局锁内以独立事件循环执行一个 F2 异步调用工厂。"""
    _quiet_f2_logs()
    with F2_LOCK:
        return asyncio.run(coro_factory())


def f2_fetch_profile(sec_uid: str, cookie: str) -> dict[str, Any]:
    from f2.apps.douyin.crawler import DouyinCrawler
    from f2.apps.douyin.model import UserProfile

    async def _run() -> dict[str, Any]:
        async with DouyinCrawler(f2_kwargs(cookie)) as crawler:
            return await crawler.fetch_user_profile(UserProfile(sec_user_id=sec_uid))

    raw = run_f2(_run)
    user = raw.get("user") if isinstance(raw, dict) else None
    if not isinstance(user, dict) or not user.get("nickname"):
        # F2 _fetch_get_json 吞错返回 {} 或风控空壳 —— 必须显式报错
        raise SidecarError(502, "profile: upstream returned no user data (risk control or expired cookie?)")
    avatar = _first_url(((user.get("avatar_larger") or {}).get("url_list")) or [])
    aweme_count = user.get("aweme_count")
    if aweme_count is None:
        raise SidecarError(502, "profile: upstream missing aweme_count")
    return {
        "sec_uid": sec_uid,
        "nickname": str(user.get("nickname") or ""),
        "avatar_url": avatar,
        "aweme_count": int(aweme_count or 0),
        "signature": str(user.get("signature") or ""),
    }


def f2_fetch_posts(sec_uid: str, cursor: int, count: int, cookie: str) -> dict[str, Any]:
    from f2.apps.douyin.crawler import DouyinCrawler
    from f2.apps.douyin.model import UserPost

    async def _run() -> dict[str, Any]:
        async with DouyinCrawler(f2_kwargs(cookie)) as crawler:
            return await crawler.fetch_user_post(
                UserPost(max_cursor=cursor, count=count, sec_user_id=sec_uid)
            )

    raw = run_f2(_run)
    aweme_list = raw.get("aweme_list") if isinstance(raw, dict) else None
    if not isinstance(aweme_list, list):
        # F2 _fetch_get_json 吞错返回 {} —— 旧版漏扫祸根,此处视为错误
        raise SidecarError(502, "posts: upstream returned no aweme_list (risk control or expired cookie?)")

    items: list[dict[str, Any]] = []
    for aweme in aweme_list:
        if not isinstance(aweme, dict):
            continue
        video = aweme.get("video") or {}
        mix = aweme.get("mix_info") or {}
        create_time = aweme.get("create_time")
        raw_images = aweme.get("images") or []
        images = [e for e in (_image_entry(i) for i in raw_images) if e]
        is_image = bool(raw_images)  # aweme.images 非空即图集作品
        cover_url = _first_url((video.get("origin_cover") or {}).get("url_list"))
        if not cover_url and images:
            cover_url = images[0]["url"]  # 图集作品退化为首图
        items.append(
            {
                "item_id": str(aweme.get("aweme_id") or ""),
                "title": str(aweme.get("desc") or aweme.get("caption") or ""),
                "cover_url": cover_url,
                "duration": 0 if is_image else int(video.get("duration") or 0),
                "published_at": rfc3339(create_time) if create_time else None,
                "mix_id": str(mix.get("mix_id")) if mix.get("mix_id") is not None else None,
                "mix_name": str(mix.get("mix_name")) if mix.get("mix_name") is not None else None,
                "type": "daily" if aweme.get("aweme_type") == 150 else ("image" if is_image else "video"),
                "image_count": len(images) if is_image else 0,
            }
        )

    has_more = bool(raw.get("has_more"))
    max_cursor = raw.get("max_cursor")
    next_cursor: str | None = None
    if has_more and max_cursor is not None:
        try:
            next_cursor = str(int(max_cursor))
        except (TypeError, ValueError):
            next_cursor = str(max_cursor)
    return {"items": items, "has_more": has_more, "next_cursor": next_cursor}


def classify_quality(height: int) -> str:
    """按 height(竖屏即短边)分类;低于最低门槛归入最接近的更低档(540p)。"""
    if height >= 1920:
        return "1080p"
    if height >= 1280:
        return "720p"
    return "540p"


def work_variants(video: dict[str, Any]) -> list[dict[str, Any]]:
    """从 video.bit_rate 提取清晰度档位:同档保 bitrate 最高,按 quality 降序。"""
    best: dict[str, dict[str, Any]] = {}
    sources = list(video.get("bit_rate") or [])
    if not sources and video.get("play_addr"):
        sources = [{"play_addr": video.get("play_addr"), "bit_rate": 0}]

    for entry in sources:
        if not isinstance(entry, dict):
            continue
        play_addr = entry.get("play_addr") or {}
        candidates = _url_candidates(play_addr.get("url_list"))
        if not candidates:
            continue
        width = int(play_addr.get("width") or video.get("width") or 0)
        height = int(play_addr.get("height") or video.get("height") or 0)
        # height 缺失时退化为 width(避免全部跌入最低档)
        edge = height or width
        quality = classify_quality(edge)
        variant = {
            "quality": quality,
            "width": width,
            "height": height,
            "bitrate": int(entry.get("bit_rate") or 0),
            "size_bytes": int(play_addr.get("data_size") or 0),
            "url": candidates[0],
            "urls": candidates,
        }
        current = best.get(quality)
        if current is None or variant["bitrate"] > current["bitrate"]:
            best[quality] = variant

    order = {"1080p": 3, "720p": 2, "540p": 1}
    return sorted(best.values(), key=lambda v: order.get(v["quality"], 0), reverse=True)


def f2_fetch_work(item_id: str, cookie: str) -> dict[str, Any]:
    from f2.apps.douyin.crawler import DouyinCrawler
    from f2.apps.douyin.model import PostDetail

    async def _run() -> dict[str, Any]:
        async with DouyinCrawler(f2_kwargs(cookie)) as crawler:
            return await crawler.fetch_post_detail(PostDetail(aweme_id=item_id))

    raw = run_f2(_run)
    aweme = raw.get("aweme_detail") if isinstance(raw, dict) else None
    if not isinstance(aweme, dict) or not aweme.get("aweme_id"):
        # F2 _fetch_get_json 吞错返回 {} 或风控空壳 —— 必须显式报错
        raise SidecarError(502, "work: upstream returned no aweme_detail (risk control or expired cookie?)")
    video = aweme.get("video") or {}
    raw_images = aweme.get("images") or []
    images = [e for e in (_image_entry(i) for i in raw_images) if e]
    live_videos = [e for e in (_live_entry(i) for i in raw_images) if e]
    cover_url = _first_url((video.get("origin_cover") or {}).get("url_list"))
    if raw_images:  # 图集/动图作品:无清晰度档位,逐张图片地址(按原始顺序)
        if not cover_url and images:
            cover_url = images[0]["url"]
        return {
            "item_id": str(aweme.get("aweme_id") or item_id),
            "title": str(aweme.get("desc") or aweme.get("caption") or ""),
            "type": "image",
            "cover_url": cover_url,
            "duration": 0,
            "variants": [],
            "images": images,
            "live_videos": live_videos,
        }
    return {
        "item_id": str(aweme.get("aweme_id") or item_id),
        "title": str(aweme.get("desc") or aweme.get("caption") or ""),
        "type": "video",
        "cover_url": cover_url,
        "duration": int(video.get("duration") or 0),
        "variants": work_variants(video),
    }


# ---------------------------------------------------------------------------
# HTTP 层
# ---------------------------------------------------------------------------


def _clean_error(exc: BaseException) -> str:
    """把异常压成一行简洁原因(去换行、截断)。"""
    text = " ".join(str(exc).split()) or exc.__class__.__name__
    return text[:300]


class Handler(BaseHTTPRequestHandler):
    server_version = "dy-sidecar/2.0"
    protocol_version = "HTTP/1.1"
    token = ""  # 由 main() 注入

    # -- 基础设施 ----------------------------------------------------------

    def log_message(self, fmt: str, *args: Any) -> None:  # 关闭默认 stderr 日志
        pass

    def _send_json(self, status: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        try:
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _dispatch(self, method: str) -> None:
        started = time.monotonic()
        parsed = urlparse(self.path)
        path = parsed.path
        status = 500
        try:
            handler = _ROUTES.get(path)
            if handler is None:
                raise SidecarError(404, "not found")
            supplied = self.headers.get("X-Sidecar-Token", "")
            # encode 成 bytes 再比较:compare_digest 对非 ASCII str 会抛 TypeError
            matched = supplied and secrets.compare_digest(
                supplied.encode("utf-8"), self.token.encode("utf-8")
            )
            if path == "/health":
                # /health 本就允许无 token 探活;但若调用方"带了"token 却不匹配,
                # 说明端口上挂着另一个(孤儿)侧车 —— 必须 403,让 Go 管理器的
                # 就绪检查立即识破,而不是误把孤儿当自己(随后业务请求全部 403)。
                if supplied and not matched:
                    raise SidecarError(403, "invalid sidecar token (foreign sidecar on this port?)")
            elif not matched:
                raise SidecarError(403, "invalid or missing sidecar token")
            if matched:
                _SERVER_STATE["last_seen"] = time.monotonic()
            params = {k: v[-1] for k, v in parse_qs(parsed.query).items()}
            payload = handler(self, params)
            status = 200
            self._send_json(200, payload)
        except SidecarError as exc:
            status = exc.status
            self._send_json(exc.status, {"error": exc.message})
        except (BrokenPipeError, ConnectionResetError):
            status = 499  # client gone
        except Exception as exc:  # 网络/风控/解析等任何意外 → 502,绝不吞错
            status = 502
            self._send_json(502, {"error": _clean_error(exc)})
        finally:
            elapsed_ms = int((time.monotonic() - started) * 1000)
            print(
                f"{datetime.now().strftime('%Y-%m-%d %H:%M:%S')} {method} "
                f"{self.path[:160]} {status} {elapsed_ms}ms",
                flush=True,
            )

    def do_GET(self) -> None:
        self._dispatch("GET")

    def _method_not_allowed(self) -> None:
        # 侧车契约只有 GET
        self._send_json(405, {"error": "method not allowed"})

    do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = _method_not_allowed


# ---------------------------------------------------------------------------
# 路由实现
# ---------------------------------------------------------------------------


def _need(params: dict[str, str], key: str) -> str:
    value = (params.get(key) or "").strip()
    if not value:
        raise SidecarError(400, f"missing query param: {key}")
    return value


def _parse_int(params: dict[str, str], key: str, default: int) -> int:
    raw = (params.get(key) or "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        raise SidecarError(400, f"invalid {key}: {raw[:50]}")


def handle_health(_handler: Handler, _params: dict[str, str]) -> dict[str, Any]:
    return {
        "status": "ok",
        "mock": bool(_handler.mock),
        "cookie_loaded": bool(read_cookie()),
    }


def handle_profile(handler: Handler, params: dict[str, str]) -> dict[str, Any]:
    sec_uid = _need(params, "sec_uid")
    if handler.mock:
        return mock_profile(sec_uid)
    cookie = require_cookie()
    return f2_fetch_profile(sec_uid, cookie)


def handle_posts(handler: Handler, params: dict[str, str]) -> dict[str, Any]:
    sec_uid = _need(params, "sec_uid")
    cursor = _parse_int(params, "cursor", 0)
    count = max(1, min(MAX_COUNT, _parse_int(params, "count", MAX_COUNT)))
    if handler.mock:
        return mock_posts(sec_uid, cursor, count)
    cookie = require_cookie()
    return f2_fetch_posts(sec_uid, cursor, count, cookie)


def handle_work(handler: Handler, params: dict[str, str]) -> dict[str, Any]:
    item_id = _need(params, "item_id")
    if handler.mock:
        return mock_work(item_id)
    cookie = require_cookie()
    return f2_fetch_work(item_id, cookie)


_ROUTES: dict[str, Callable[[Handler, dict[str, str]], dict[str, Any]]] = {
    "/health": handle_health,
    "/profile": handle_profile,
    "/posts": handle_posts,
    "/work": handle_work,
}


# ---------------------------------------------------------------------------
# 入口
# ---------------------------------------------------------------------------


class ExclusiveThreadingHTTPServer(ThreadingHTTPServer):
    """拒绝与本机其他侧车进程双绑定同一端口。

    ThreadingHTTPServer 默认 allow_reuse_address=1,在 Windows 上映射为
    SO_REUSEADDR,会让两个进程同时 LISTEN 同一端口(连接被随机分配),
    造成 Go 侧 token 校验 403 这类难以排查的错乱。这里显式关掉并在
    Windows 上加 SO_EXCLUSIVEADDRUSE,让端口冲突变成启动期可见的失败。
    """

    allow_reuse_address = False

    def server_bind(self) -> None:
        if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
            self.socket.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
        super().server_bind()


_SERVER_STATE = {"last_seen": time.monotonic()}


def _self_idle_watchdog() -> None:
    """兜底自毁:若超过 DY_SIDECAR_SELF_IDLE_SECONDS(默认 1200s)没有任何
    带正确 token 的请求,说明宿主 Go 进程已死或被替换 —— 孤儿侧车自行退出,
    释放端口,避免占住 18787 让新实例的侧车无法绑定。"""
    limit = float(os.environ.get("DY_SIDECAR_SELF_IDLE_SECONDS", "1200"))
    while True:
        time.sleep(15)
        if time.monotonic() - _SERVER_STATE["last_seen"] > limit:
            print(f"sidecar self-exit: idle > {limit:.0f}s (orphan backstop)", flush=True)
            os._exit(0)


def main() -> None:
    parser = argparse.ArgumentParser(description="抖音归档工具 v2 签名侧车")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    parser.add_argument("--token", required=True, help="Go 主后端生成的共享令牌")
    parser.add_argument("--mock", action="store_true", help="返回确定性假数据,不触网、不加载 F2")
    args = parser.parse_args()

    Handler.token = args.token
    Handler.mock = args.mock

    server = ExclusiveThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    server.daemon_threads = True
    mode = "mock" if args.mock else "f2"
    print(
        f"{datetime.now().strftime('%Y-%m-%d %H:%M:%S')} sidecar listening on "
        f"127.0.0.1:{args.port} mode={mode} cookie_file={cookie_file_path()}",
        flush=True,
    )
    threading.Thread(target=_self_idle_watchdog, daemon=True).start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:  # 启动失败也要可见,绝不静默
        print(f"sidecar fatal: {_clean_error(exc)}", file=sys.stderr, flush=True)
        sys.exit(1)
