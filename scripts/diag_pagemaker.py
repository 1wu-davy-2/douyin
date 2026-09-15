# -*- coding: utf-8 -*-
"""诊断脚本:用 sidecar 同款 F2 代码逐页拉取博主作品,复现扫描分页行为。

只读诊断,不改库、不下载。按 Go scanner 的分页规则走:
cursor=0 起步 -> next_cursor -> has_more=false 停;打印每页关键信息。
"""
import sys, time, json
sys.path.insert(0, "sidecar")
import main as sidecar

SEC_UID = "MS4wLjABAAAAkyV1b4N1iOiSA0DzSEzcBEHFWeMgeDEXW6dSLlQvApILYBd0ggm20AvH6uXtAjB6"

cookie = sidecar.read_cookie()
print("cookie loaded:", bool(cookie), "len:", len(cookie))

prof = sidecar.f2_fetch_profile(SEC_UID, cookie)
print("profile:", json.dumps({k: prof[k] for k in ("nickname", "aweme_count")}, ensure_ascii=False))

cursor = 0
seen_ids = []
page = 0
while page < 15:
    page += 1
    try:
        data = sidecar.f2_fetch_posts(SEC_UID, cursor, 20, cookie)
    except Exception as e:
        print(f"page {page} cursor={cursor} ERROR: {e}")
        break
    items = data["items"]
    ids = [it["item_id"] for it in items]
    dup = len(ids) - len(set(ids))
    dup_vs_prev = sum(1 for i in ids if i in seen_ids)
    times = [it["published_at"] for it in items if it.get("published_at")]
    print(
        f"page {page} cursor={cursor} items={len(ids)} has_more={data['has_more']} "
        f"next_cursor={data['next_cursor']!r} dup_in_page={dup} dup_vs_prev={dup_vs_prev} "
        f"newest={times[0] if times else None} oldest={times[-1] if times else None}"
    )
    seen_ids.extend(ids)
    if not data["has_more"]:
        print("=> has_more=false, natural end. total seen:", len(set(seen_ids)))
        break
    nc = data.get("next_cursor")
    if nc is None:
        print("=> has_more=true but next_cursor=None (Go scanner would fallback)")
        break
    cursor = int(nc)
    time.sleep(2.0)
else:
    print("=> hit 15-page safety cap")

print("unique item ids collected:", len(set(seen_ids)))
