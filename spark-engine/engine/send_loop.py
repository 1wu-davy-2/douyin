"""T1.3/T1.6: /send/run 执行体 —— 包装 core.send_tools._do_user_task_locked。

上游 _do_user_task_locked 就是完整的"单账号发送闭环"（打开 creator 聊天页 →
滚动好友列表定位目标 → 逐目标输入/发送 → IM 观察器 + DOM 双重强确认 →
失败分类入队），本文件只做三件事：
  1. 把 Go 下发的 send_config 注入引擎（set_engine_config）
  2. 组装上游期望的 user dict（cookies/targets/message_history）
  3. 任务结束后从内存 user dict 读取结果，映射为 API 契约响应
"""

from __future__ import annotations

from core import send_tools
from core.send_tools import (
    _do_user_task_locked,
    _normalize_friend_list_scan_config,
    _normalize_send_strategy,
)
from utils.config import profile_root


def build_profile_config() -> dict:
    """持久化 profile 配置（上游 persistentBrowserProfiles 的引擎固定版）。"""
    return {
        "enabled": True,
        "root": str(profile_root()),
        "seedCookiesWhenEmpty": True,
        "syncStoredCookiesBeforeRun": True,
        "refreshStoredCookiesAfterLogin": True,
    }


async def run_send(
    unique_id: str,
    account_name: str,
    targets: list[str],
    cookies: list[dict],
    config: dict,
    previous_messages: dict[str, str] | None = None,
) -> dict:
    send_tools.set_engine_config(config)
    send_strategy = _normalize_send_strategy(config)
    friend_scan = _normalize_friend_list_scan_config(config)

    user: dict = {
        "unique_id": unique_id,
        "username": account_name,
        "cookies": list(cookies or []),
        "targets": list(targets),
        "message_history": {},
    }
    for target, message in (previous_messages or {}).items():
        user["message_history"][target] = {"message": message}

    # browser 参数在持久化 profile 模式下不使用（函数内部自行 launch）。
    await _do_user_task_locked(
        None,
        user,
        send_strategy,
        build_profile_config(),
        friend_scan,
        account_name,
        "direct",
    )

    history = dict(user.get("message_history") or {})
    failures = dict(user.get("failure_queue") or {})

    results = []
    for target in targets:
        entry = history.get(target)
        if entry:
            results.append(
                {
                    "target": target,
                    "message": str(entry.get("message") or ""),
                    "state": "strong",
                    "category": "",
                    "detail": str(entry.get("confirmationDetail") or ""),
                    "sent_at": str(entry.get("sentAt") or ""),
                    "server_receipt": entry.get("serverReceipt") or {},
                }
            )
            continue
        fail = failures.get(target)
        if fail:
            results.append(
                {
                    "target": target,
                    "message": str(fail.get("message") or ""),
                    "state": "failed",
                    "category": str(fail.get("category") or "send_failed"),
                    "detail": str(fail.get("reason") or ""),
                }
            )
            continue
        results.append(
            {
                "target": target,
                "message": "",
                "state": "failed",
                "category": "friend_not_found",
                "detail": "target not reached in friend list",
            }
        )

    return {
        "results": results,
        "account_failure": dict(user.get("account_failure") or {}),
    }
