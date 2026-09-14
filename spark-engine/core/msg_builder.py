# 来源: douyin-sparkflow (DouYinSparkFlow/core/msg_builder.py) @ 7c7d9c2 —— 火花融合改造版
# 许可: PolyForm Noncommercial 1.0.0 —— 仅限非商业用途。
# 改造说明: 配置由调用方(引擎端点)注入，不再读 config.json；
#          移除节日祝福(happyNewYear)分支及其上游依赖。

import random
from typing import Dict, List, Optional

from utils.hitokoto import request_hitokoto


def _get_message_templates(active_config: dict) -> List[str]:
    strategy = active_config.get("sendStrategy", {}) or {}
    variants = [str(item).strip() for item in strategy.get("messageVariants", []) if str(item).strip()]
    if variants:
        return variants
    return [str(active_config.get("messageTemplate", "续火花")).strip()]


def _render_regular_message(template: str, hitokoto_types: Optional[List[str]] = None) -> str:
    message = template
    if "[API]" in message:
        message = message.replace("[API]", request_hitokoto(types=hitokoto_types))
    return message.strip()


def build_message_candidates(config: Optional[dict] = None) -> List[str]:
    active_config = config or {}
    candidates: List[str] = []
    hitokoto_types = [str(t).strip() for t in (active_config.get("hitokotoTypes") or []) if str(t).strip()]

    for template in _get_message_templates(active_config):
        message = _render_regular_message(template, hitokoto_types)
        if message and message not in candidates:
            candidates.append(message)

    if candidates:
        return candidates
    return ["续火花"]


def _extract_previous_message(previous_messages: Optional[dict], target: str) -> str:
    if not previous_messages:
        return ""

    previous = previous_messages.get(target, "")
    if isinstance(previous, dict):
        return str(previous.get("message", "")).strip()
    return str(previous).strip()


def _choose_message(candidates: List[str], previous_message: str, last_message: str) -> str:
    filtered = [message for message in candidates if message != previous_message and message != last_message]
    if filtered:
        return random.choice(filtered)

    filtered = [message for message in candidates if message != previous_message]
    if filtered:
        return random.choice(filtered)

    filtered = [message for message in candidates if message != last_message]
    if filtered:
        return random.choice(filtered)

    return random.choice(candidates)


def build_message(previous_message: str = "", config: Optional[dict] = None, last_message: str = "") -> str:
    candidates = build_message_candidates(config)
    return _choose_message(candidates, previous_message.strip(), last_message.strip()).strip()


def build_messages_for_targets(
    targets: List[str],
    previous_messages: Optional[dict] = None,
    config: Optional[dict] = None,
) -> Dict[str, str]:
    active_config = config or {}
    strategy = active_config.get("sendStrategy", {}) or {}

    ordered_targets = []
    seen_targets = set()
    for target in targets:
        normalized = str(target).strip()
        if not normalized or normalized in seen_targets:
            continue
        seen_targets.add(normalized)
        ordered_targets.append(normalized)

    if strategy.get("shuffleTargets", True):
        random.shuffle(ordered_targets)

    planned_messages: Dict[str, str] = {}
    last_message = ""
    for target in ordered_targets:
        previous_message = _extract_previous_message(previous_messages, target)
        message = build_message(previous_message=previous_message, config=active_config, last_message=last_message)
        planned_messages[target] = message
        last_message = message

    return planned_messages
