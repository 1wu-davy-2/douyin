# spark-engine 裁剪版配置工具。
# 来源: douyin-sparkflow (DouYinSparkFlow/utils/config.py) @ 7c7d9c2 —— 裁剪重写
# 许可: 上游为 PolyForm Noncommercial 1.0.0，仅限非商业用途。
# 改造说明: 引擎不落业务数据(JSON 存储全部移除，Go 是唯一写者)；
#          仅保留环境判定 / repo_root / normalize_unique_id 供 core 模块引用。

from __future__ import annotations

import os
import sys
from enum import Enum
from pathlib import Path


DEBUG = False


class Environment(Enum):
    GITHUBACTION = "GITHUB_ACTION"
    LOCAL = "LOCAL"
    PACKED = "PACKED"


def get_environment() -> Environment:
    if getattr(sys, "frozen", False) and hasattr(sys, "_MEIPASS"):
        return Environment.PACKED
    if os.getenv("GITHUB_ACTIONS") == "true":
        return Environment.GITHUBACTION
    return Environment.LOCAL


def repo_root() -> Path:
    """spark-engine/ 目录（本文件位于 utils/ 下）。"""
    return Path(__file__).resolve().parents[1]


def profile_root() -> Path:
    """浏览器持久化 profile 根目录（挂载卷），env 优先。"""
    return Path(
        os.getenv("SPARKFLOW_BROWSER_PROFILE_ROOT")
        or str(repo_root() / "state" / "browser-profiles")
    )


def normalize_unique_id(unique_id) -> str:
    if not unique_id:
        return ""
    digits = "".join(ch for ch in str(unique_id) if ch.isdigit())
    return digits or str(unique_id).strip()


def engine_profile_name(unique_id: str, username: str = "") -> str:
    """账号 → 浏览器 profile 目录名（对齐上游 _account_profile_name）。"""
    from core.browser import sanitize_profile_name  # 延迟导入避免环

    normalized = normalize_unique_id(unique_id)
    if normalized:
        return f"uid-{normalized}"
    if str(username or "").strip():
        return f"user-{sanitize_profile_name(username)}"
    return "unknown"
