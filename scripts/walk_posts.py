#!/usr/bin/env python3
"""逐页走一遍侧车 /posts,打印每页 (cursor, 条数, has_more, next_cursor)。

用法: python scripts/walk_posts.py <sec_uid>
需要: 本地侧车已在 18787 运行(服务起着就行),TOKEN 自动从进程命令行抓取。
"""
import json
import subprocess
import sys
import urllib.request

SEC = sys.argv[1] if len(sys.argv) > 1 else ""
BASE = "http://127.0.0.1:18787"


def sidecar_token() -> str:
    out = subprocess.run(
        ["wmic", "process", "where", "name='python.exe'", "get", "commandline"],
        capture_output=True, text=True,
    ).stdout
    for line in out.splitlines():
        if "main.py" in line and "--token" in line:
            return line.split("--token")[1].strip().split()[0]
    raise SystemExit("no sidecar process found")


def get(path: str):
    req = urllib.request.Request(BASE + path, headers={"X-Sidecar-Token": TOKEN})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")[:200]
        return e.code, {"error": body}


TOKEN = sidecar_token()
print(f"token ok, walking sec_uid={SEC[:20]}...")

cursor, seen, page = "0", set(), 0
while page < 15:
    page += 1
    status, data = get(f"/posts?sec_uid={SEC}&cursor={cursor}&count=20")
    if status != 200:
        print(f"page {page}: HTTP {status} cursor={cursor} error={data.get('error')}")
        break
    items = data.get("items") or []
    new = [i["item_id"] for i in items if i["item_id"] not in seen]
    seen.update(i["item_id"] for i in items)
    nc = data.get("next_cursor")
    print(f"page {page}: items={len(items)} new={len(new)} has_more={data.get('has_more')} "
          f"next_cursor={nc} first_ts={items[0]['published_at'] if items else '-'} "
          f"last_ts={items[-1]['published_at'] if items else '-'}")
    if not data.get("has_more"):
        print(f"== has_more=false after page {page}; total unique items seen: {len(seen)}")
        break
    if nc is None or nc in seen and False:
        print("== next_cursor empty")
        break
    if nc and nc == cursor:
        print("== next_cursor REPEATED -> douyin is stalling pagination")
        break
    cursor = nc if nc is not None else ""
else:
    print("== hit 15-page guard")
