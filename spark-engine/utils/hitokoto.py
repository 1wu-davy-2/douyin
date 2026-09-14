# 来源: douyin-sparkflow (DouYinSparkFlow/utils/hitokoto.py) @ 7c7d9c2 —— 火花融合改造版
# 许可: PolyForm Noncommercial 1.0.0 —— 仅限非商业用途。
# 改造说明: 一言类型由调用方传入（引擎从 Go 下发的 send_config 读取），
#          不再读 config.json。

import logging

import requests

logger = logging.getLogger(__name__)

hitokotoApi = "https://v1.hitokoto.cn/"

allHitokotoTypes = {
    "动画": "a",
    "漫画": "b",
    "游戏": "c",
    "文学": "d",
    "原创": "e",
    "来自网络": "f",
    "其他": "g",
    "影视": "h",
    "诗词": "i",
    "哲学": "k",
    "抖机灵": "l",
}


def request_hitokoto(types=None) -> str:
    """请求一言 API 获取一句话；types 为中文类型名列表（如 ["文学","诗词"]）。"""
    wanted = [str(t).strip() for t in (types or [])]

    api_url = hitokotoApi
    for t in allHitokotoTypes.keys():
        if t in wanted:
            if "?" not in api_url:
                api_url += "?"
            if "c=" in api_url:
                api_url += f"&c={allHitokotoTypes[t]}"
            else:
                api_url += f"c={allHitokotoTypes[t]}"

    try:
        response = requests.get(api_url, timeout=10)
        response.raise_for_status()
        data = response.json()
        theFrom = data.get("from")
        if theFrom is None or theFrom.strip() == "":
            theFrom = "未知来源"
        theFromWho = data.get("from_who")
        if theFromWho is None or theFromWho.strip() == "":
            theFromWho = "未知作者"
        return f"{data['hitokoto']} —— {theFrom} ({theFromWho})"
    except Exception as exc:
        logger.warning("hitokoto request failed: %s", exc)
        return "[error] 无法获取一言内容"
