# 发送闭环工具函数集 —— 从上游 core/tasks.py 摘取并参数化。
# 来源: douyin-sparkflow (DouYinSparkFlow/core/tasks.py) @ 7c7d9c2
# 许可: PolyForm Noncommercial 1.0.0 —— 仅限非商业用途。
# 状态: T1.3 进行中 —— 目前仅包含文本归一化；聊天 DOM 工具函数
#       (locate_chat_input / snapshot_last_own_message / confirm_message_sent /
#        scroll_and_select_user 等，上游行号见 docs/HUOHUA_EXECUTION_PLAN.md §6.3 表)
#       在 T1.3 中补齐。

from __future__ import annotations

import unicodedata


def normalize_target_name(value) -> str:
    """好友名归一化（上游 tasks.py:_normalize_target_name 原样）。"""
    raw = unicodedata.normalize("NFKC", str(value or ""))
    for token in ("\u200b", "\u200c", "\u200d", "\ufeff"):
        raw = raw.replace(token, "")
    raw = raw.replace("\xa0", " ")
    return " ".join(raw.split()).strip()
