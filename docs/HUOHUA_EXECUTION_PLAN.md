# 火花融合执行计划（dev-huohua 分支施工图纸）

> 本文档是交给执行模型（GLM-5.3-Flash）的**完整施工说明书**，自包含、可直接照做。
> 背景与方案分析见 `docs/SPARKFLOW_MERGE_PLAN.md`（先读其 §2.1 模块分级与 §3 方案 B）。
> 编写日期：2026-09-14。任务全部在 `dev-huohua` 分支完成。

---

## 0. 执行者须知（每个任务开始前重读）

**项目现状**

- 仓库根：`E:\opt\vide coding\douyin`（Windows 开发机；服务器为 Linux Docker）
- 主项目：Go 后端 `backend/`（模块名 `douyin/backend`，导入前缀 `douyin/backend/internal/...`）+ React 前端 `frontend/`（Vite + TS + Tailwind v4 + Radix UI + TanStack Query v5 + react-router 7）+ Python F2 签名侧车 `sidecar/`
- 参考项目（**只读**）：`douyin-sparkflow/DouYinSparkFlow/`（上游克隆，commit 7c7d9c2，PolyForm Noncommercial 许可）

**工作循环（每个任务严格执行）**

1. 先读本任务「参考文件」栏
2. 按步骤实现；有设计空白时选**最小实现**，并在 §12 执行日志记录
3. 跑「验收」命令，全过才进入下一任务
4. 提交：`git add -A && git commit -m "spark(T编号): 一句话摘要"`

**硬规则（违反任一即返工）**

1. **禁止修改 `douyin-sparkflow/` 目录下任何文件**。需要上游代码时：复制到 `spark-engine/` 后修改副本。
2. 从上游复制/改写的每个文件，文件头加注释：`# 来源: douyin-sparkflow(DouYinSparkFlow/<原路径>) · 许可: PolyForm Noncommercial 1.0.0 · 仅限非商业用途`
3. Go 侧改动后必须：`cd backend && go build ./... && go vet ./... && go test ./... -count=1` 全绿
4. 前端改动后必须：`cd frontend && npm run build` 零错误（含 tsc -b 类型检查）
5. 不动既有业务代码：scanner / downloader / subscriptions / provider / events 的既有行为不得改变；新代码集中在 `backend/internal/spark/`、`frontend/src/pages/spark/`、`spark-engine/`
6. **Go 是 SQLite 唯一写者**；Python 引擎不连数据库，状态全部通过 HTTP 返回
7. 引擎只监听 127.0.0.1（容器内为容器网络地址），所有端点校验 `X-Engine-Token`
8. 时区统一 `Asia/Shanghai`；Go 侧在 config 包所在二进制 `main.go` 加 `import _ "time/tzdata"`
9. 引擎忙时 `POST /send/run` 返回 `409 {"detail":"task already running"}`（同一时刻只允许一个发送任务）

**术语**

- 帧藏 = 主项目（douyin-archive v2）
- spark-engine = 本计划新建的 Python 火花引擎（FastAPI + Playwright）
- 登录桌面 = 上游 `login_desktop_server.py`（扫码登录环境，noVNC 可远程操作）
- 强确认 = 发送后在会话 DOM 里验证消息确实出现（上游核心防"假发送"机制）

---

## 1. 目标架构与 v1 范围决策

```
React SPA（新增 /spark 页面，Tabs: 总览/账号/控制台/记录/设置）
   │ /api/spark/*（帧藏既有 session 认证 authGuard）
Go 主后端 :8787
   ├ internal/spark/     新包：HTTP 客户端 + API handlers + DB 读写 + 发送调度编排
   ├ SQLite 新增 4 表（migrations/0006_spark.sql）
   ├ spark 调度器（30s tick → 选出到期目标 → 串行调引擎）
   └ /api/spark/login/* 反代引擎（含 noVNC WebSocket）
        │ http://127.0.0.1:18788 + X-Engine-Token（引擎是外部进程/容器）
        ▼
spark-engine/（新建目录，Python FastAPI + Playwright）
   ├ engine/             应用壳 + 端点 + 发送闭环
   ├ core/               上游平移：browser / friends / msg_builder / send_state + 发送工具函数
   ├ login_desktop_server.py   上游平移（扫码登录 + noVNC）
   └ state/browser-profiles/   持久化浏览器登录态（挂载卷，不进 git）

Cookie 闭环（统一登录态）：
  扫码登录成功 → 引擎导出 .douyin.com 域 Cookie → Go 写 data/.cookie
  → 帧藏 F2 侧车每次请求热加载该文件（sidecar/main.py:85 read_cookie）
  → 归档扫描复用同一登录态，免手动粘贴 Cookie
```

**已定的 v1 范围决策（不要重新设计）**

| 决策 | 内容 | 理由 |
|---|---|---|
| 引擎生命周期 | 外部进程/容器常驻（compose `--profile spark`），Go 只做 HTTP 客户端 + 健康检查，**不做进程拉起** | Playwright 冷启动慢；发送窗口集中在白天；避免复刻 sidecar.Manager 的复杂度 |
| 协议模式 | 不迁移 `protocol_sender.mjs` / `protocol_dispatch.py` | 可选项，后置 |
| 代理 | 不迁移 Mihomo；浏览器启动参数含 `--no-proxy-server` 直连 | 国内直连可达 |
| 节日模板 | 不迁移 `chinese_new_year_2026_mare.py`（932 行） | 过时且非核心 |
| 好友表刷新 | 整表替换；**新好友默认 selected=0（不发送）**，UI 提供全选 | 防止误发消息 |
| 引擎并发 | /send/run 全局单任务（409 拒绝并发）；好友刷新与发送互斥 | 上游同为单浏览器模型 |
| 账号级冷却 | 当日账号级错误 ≥3 次进入 60 分钟冷却（写 spark_accounts.cooldown_until） | 上游 pause 机制简化版 |
| 登录桌面 | 引擎容器 entrypoint 后台启动；本地 Windows 开发不可用（用 T0.2 的 headful 扫码脚本代替） | 上游依赖 X 环境 |

---

## 2. 最终目录结构

```
douyin/
├─ backend/
│  ├─ internal/
│  │  ├─ spark/                    ★新增
│  │  │  ├─ client.go              引擎 HTTP 客户端（token/超时/错误包装）
│  │  │  ├─ store.go                4 张表的 CRUD（Go 单写者）
│  │  │  ├─ api.go                  /api/spark/* handlers
│  │  │  ├─ scheduler.go            30s tick + 窗口/到期目标选择 + 串行发送
│  │  │  └─ spark_test.go           契约测试
│  │  └─ db/migrations/0006_spark.sql ★新增
│  └─ cmd/server/main.go           +2 行：注册 spark 路由、启动 spark 调度器
├─ frontend/src/
│  ├─ api/spark.ts                 ★新增 endpoints
│  ├─ api/spark-types.ts           ★新增 TS 类型
│  ├─ api/queries.ts               +spark query keys（模仿现有 qk）
│  ├─ api/useEvents.ts             +spark.* 事件缓存失效
│  ├─ components/layout.tsx        侧边栏 +1 项（火花/Flame 图标）
│  ├─ App.tsx                       +1 路由 /spark
│  └─ pages/spark/                 ★新增
│     ├─ spark-page.tsx            Tabs 容器
│     ├─ overview-tab.tsx / accounts-tab.tsx / console-tab.tsx
│     ├─ records-tab.tsx / settings-tab.tsx
│     └─ login-dialog.tsx          扫码登录（QR 轮询 + noVNC iframe）
├─ spark-engine/                   ★新增（Python）
│  ├─ main.py                      入口：--port --token（env 兜底）
│  ├─ requirements.txt
│  ├─ Dockerfile
│  ├─ docker/entrypoint.sh
│  ├─ engine/
│  │  ├─ app.py                    FastAPI 壳（token 中间件 + 全部端点）
│  │  ├─ send_loop.py              ★核心：单账号发送闭环（§6.3）
│  │  └─ login_bridge.py           登录桌面桥接（§6.7）
│  ├─ core/                        上游平移（§任务 T1.2 清单）
│  │  ├─ browser.py  friends.py  msg_builder.py  send_state.py
│  │  └─ send_tools.py             从 tasks.py 摘出的发送工具函数集
│  ├─ utils/                       上游平移：config 裁剪版 / logger / hitokoto
│  ├─ login_desktop_server.py       上游平移（基本原样）
│  ├─ scripts/start_login_desktop.sh 上游平移
│  ├─ spike/cookie_spike.py        M0 验证脚本（保留）
│  └─ state/                       运行时数据（gitignore：state/）
├─ docker-compose.yml              +spark-engine 服务（profile: spark）
└─ .gitignore                      +spark-engine/state/
```

---

## 3. 数据库设计

新建 `backend/internal/db/migrations/0006_spark.sql`（编号必须衔接现有 0005）：

```sql
-- v6: 火花（续火花）融合表。上游 douyin-sparkflow 的 JSON 存储改为 SQLite，Go 为唯一写者。

CREATE TABLE IF NOT EXISTS spark_accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    unique_id TEXT NOT NULL UNIQUE,          -- 抖音号（登录导出）
    username TEXT NOT NULL DEFAULT '',        -- 冗余显示名（上游字段）
    nickname TEXT NOT NULL DEFAULT '',
    profile_name TEXT NOT NULL,              -- 浏览器 profile 目录名（uid-{unique_id}）
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'idle',     -- idle|sending|login_required|cooldown|error
    last_error TEXT NOT NULL DEFAULT '',
    last_friends_refresh_at TEXT NOT NULL DEFAULT '',   -- RFC3339
    last_send_at TEXT NOT NULL DEFAULT '',
    cooldown_until TEXT NOT NULL DEFAULT '',            -- RFC3339，空=无冷却
    failure_count_today TEXT NOT NULL DEFAULT '',       -- JSON: {"2026-09-14": 3}
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS spark_friends (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES spark_accounts(id) ON DELETE CASCADE,
    friend_key TEXT NOT NULL,                -- 归一化名字（上游 _normalize_target_name）
    display_name TEXT NOT NULL,
    selected INTEGER NOT NULL DEFAULT 0,     -- 是否发送目标；新好友默认 0
    UNIQUE(account_id, friend_key)
);
CREATE INDEX IF NOT EXISTS idx_spark_friends_account ON spark_friends(account_id);

CREATE TABLE IF NOT EXISTS spark_send_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES spark_accounts(id) ON DELETE CASCADE,
    friend_key TEXT NOT NULL,
    message TEXT NOT NULL,                   -- 实际发送的文本（引擎返回）
    confirm_state TEXT NOT NULL,            -- strong|weak|failed
    category TEXT NOT NULL DEFAULT '',      -- target_not_found|send_failed|login_required|account_error
    detail TEXT NOT NULL DEFAULT '',
    run_mode TEXT NOT NULL,                 -- manual|manual_failed|manual_unsent|scheduled
    sent_at TEXT NOT NULL,                  -- RFC3339 UTC
    local_date TEXT NOT NULL                -- Asia/Shanghai 的 YYYY-MM-DD（"今日"判定用）
);
CREATE INDEX IF NOT EXISTS idx_spark_send_records_lookup
    ON spark_send_records(account_id, local_date);
CREATE INDEX IF NOT EXISTS idx_spark_send_records_time
    ON spark_send_records(sent_at DESC);

CREATE TABLE IF NOT EXISTS spark_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL                      -- JSON 字符串
);
```

`spark_settings` 初始插入一行 `key='send_config'`，value 为 §5.4 的默认 JSON。

---

## 4. 引擎 API 契约（spark-engine，监听 127.0.0.1:18788）

所有请求带 `X-Engine-Token` 头；错误统一 `非2xx + {"detail":"..."}`。

| 方法/路径 | 请求体 | 响应 |
|---|---|---|
| GET `/health` | — | `{"status":"ok","version":1,"task_running":false}` |
| POST `/cookies/export` | `{"profile_name":"uid-123"}` | `{"cookie":"a=1; b=2","cookie_count":28,"exported_at":"RFC3339"}` |
| POST `/friends/refresh` | `{"profile_name":"uid-123","scan":{"maxScanSeconds":300,"idleScanSeconds":120,"scrollStepPx":400,"scrollDelaySeconds":0.8}}` | `{"friends":[{"key":"张三","display_name":"张三"}],"complete":true,"elapsed_seconds":12.3}` |
| POST `/send/run` | `{"profile_name":"uid-123","targets":["张三","李四"],"config":{§5.4 的 message*+sendStrategy 子集}}` | `{"results":[§6.3 结果项],"started_at":"...","finished_at":"..."}` |
| POST `/login/open` | `{}` | `{"status":"opened"}` |
| GET `/login/qr` | — | `image/png` |
| POST `/login/refresh-qr` | — | `{"status":"refreshed"}` |
| GET `/login/status` | — | `{"running":true,"logged_in":false,"unique_id":"","nickname":""}` |
| POST `/login/export` | `{}` | `{"unique_id":"...","nickname":"...","profile_name":"uid-...","cookie":"a=1; b=2"}` |
| POST `/login/close` | `{}` | `{"status":"closed"}` |
| GET `/login/vnc/*` | — | noVNC 反代（HTTP + WebSocket，见 §6.7） |

要点：
- `/send/run` 忙时 `409 {"detail":"task already running"}`；`targets` 为空直接返回空结果。
- `/cookies/export` 与 `/friends/refresh` 与 `/send/run` 共用同一把引擎级互斥锁（asyncio.Lock），防止同时开两个浏览器 profile。
- 引擎不落任何业务数据（除浏览器 profile 与日志）；一切状态通过响应返回给 Go 入库。

---

## 5. Go API 契约（/api/spark/*，全部在 authGuard 之后）

| 方法/路径 | 请求 | 响应（要点） |
|---|---|---|
| GET `/api/spark/overview` | — | `{"engine":{"ok":true,"version":1},"accounts":[{id,unique_id,nickname,enabled,status,cooldown_until,last_send_at}],"today":{"strong":12,"weak":2,"failed":1},"window":{"enabled":true,"in_window":true,"now":"RFC3339"}}` |
| GET `/api/spark/accounts` | — | 账号列表 |
| POST `/api/spark/accounts` | `{"unique_id":"...","nickname":"...","profile_name":"uid-..."}` | 新建账号（登录导出后调用） |
| PATCH `/api/spark/accounts/{id}` | `{"enabled":true}` 等 | 更新 |
| DELETE `/api/spark/accounts/{id}` | — | 删除（级联好友/记录） |
| POST `/api/spark/accounts/{id}/friends/refresh` | — | 调引擎 → 整表替换 spark_friends（保留既有 selected）→ 返回 `{"count":42,"new":3}` |
| GET `/api/spark/accounts/{id}/friends` | `?selected=` | 好友列表（含 selected、今日发送状态聚合） |
| PATCH `/api/spark/accounts/{id}/friends` | `{"updates":[{"key":"张三","selected":true}]}` | 批量勾选 |
| POST `/api/spark/send/run` | `{"mode":"now\|failed\|unsent","account_ids":[1,2]}`（ids 省略=全部启用账号） | `{"accepted":true}`（异步执行，进度走 SSE） |
| GET `/api/spark/records` | `?account_id=&cursor=&limit=20` | 游标分页 `{items:[...],next_cursor:""}` |
| GET/PUT `/api/spark/settings` | §5.4 JSON | 读/写 send_config |
| POST `/api/spark/cookies/export` | `{"account_id":1}` | 调引擎导出 → **Go 写 data/.cookie** → `{"cookie_count":28}` |
| GET `/api/spark/engine/health` | — | 透传引擎健康（引擎不可达时 `{"ok":false}`，前端显示"引擎未启动"） |
| `/api/spark/login/*` | — | httputil.ReverseProxy → 引擎 `/login/*`（含 WebSocket，§6.7） |

### 5.4 spark_settings.send_config 默认值（PUT 时整体替换，字段缺失回落此默认）

```json
{
  "messageTemplate": "✨今日火花+1",
  "messageVariants": ["🤩今日火花+1", "今天来补个火花", "给你续一下今天的火花", "路过给你加个小火花"],
  "hitokotoTypes": ["文学", "影视", "诗词", "哲学"],
  "sendWindow": { "enabled": true, "startHour": 10, "endHour": 18, "intervalMinutes": 20 },
  "sendStrategy": { "shuffleTargets": true, "accountStartDelaySecondsMin": 15, "accountStartDelaySecondsMax": 60, "messageIntervalSecondsMin": 25, "messageIntervalSecondsMax": 70 },
  "friendScan": { "maxScanSeconds": 300, "idleScanSeconds": 120, "scrollStepPx": 400, "scrollDelaySeconds": 0.8 },
  "accountFailurePause": { "attempts": 3, "cooldownMinutes": 60 }
}
```

---

## 6. 核心逻辑详细规格

### 6.1 Cookie 导出（引擎 `POST /cookies/export`）

```python
# engine/app.py 内实现（伪代码 → 实码）
async def export_cookies(profile_name: str) -> dict:
    async with engine_browser_context(profile_name) as ctx:     # §T1.2 平移的 browser 封装
        page = await ctx.new_page()
        # 1. 先访问 www.douyin.com 一次：补齐 ttwid 等 www 域 Cookie
        #    （上游登录流程本身会访问 www.douyin.com/user/self 收集身份，
        #     login_desktop_server.py:18,622-625；这里再确保一次，双保险）
        await page.goto("https://www.douyin.com/", wait_until="domcontentloaded", timeout=60_000)
        cookies = await ctx.cookies()                             # Playwright API
        pairs = [f"{c['name']}={c['value']}" for c in cookies
                 if ".douyin.com" in c.get("domain", "")]          # 只留 douyin 域
        return {"cookie": "; ".join(pairs), "cookie_count": len(pairs), "exported_at": now_rfc3339()}
```

Go 侧 `POST /api/spark/cookies/export`：调上端点 → 校验 `cookie` 非空且包含 `=` → 原样写入 `<DY_DATA_DIR>/data/.cookie`（文件就是纯 Cookie 头字符串，帧藏 F2 侧车每次请求热加载，见 `sidecar/main.py:85-98`）。写入成功后返回计数。**前端在登录成功后与设置页手动按钮两处触发。**

### 6.2 好友刷新（引擎 `POST /friends/refresh`）

平移上游 `core/friends.py` 全文（374 行，基本原样），引擎端点包装：

1. 用账号 profile 启动浏览器 → 打开 `CREATOR_CHAT_URL`（friends.py:10）
2. `await fetch_account_friends(account)`（friends.py:342，内部含：好友 tab 点击、虚拟列表滚动、`no more` 判定、登录遮罩检测、弹窗关闭）
3. 返回 `[{key: normalize(name), display_name: name}]`（key 用上游 `_normalize_target_name`，tasks.py:52）
4. Go 收到后**整表替换**该账号的 spark_friends（DELETE+批量 INSERT，事务内），已存在的 friend_key 保留 selected 值；返回 `{"count":N,"new":M}`

### 6.3 单账号发送闭环 + 强确认（引擎 `POST /send/run`）★全项目最核心

新写 `engine/send_loop.py`，并从上游 `core/tasks.py` 摘出以下工具函数**原样平移**到 `core/send_tools.py`（行号为上游位置，供核对）：

| 函数 | 上游行号 | 作用 |
|---|---|---|
| `locate_chat_input` | 395 | 定位聊天输入框（多选择器） |
| `read_chat_input_text` | 415 | 读输入框当前文本 |
| `snapshot_last_own_message` | 711 | 快照"最后一条自己的消息"（文本+位置签名） |
| `count_today_own_message_matches` | 771 | 当日会话中指定文本出现次数 |
| `count_visible_message_matches` | 809 | 可见区域文本匹配计数 |
| `confirm_message_sent` | 855 | **强确认主函数** |
| `_detect_send_failure_indicator` | 443 | 红色感叹/重试按钮检测 |
| `scroll_and_select_user` | 1353 | 在好友列表定位并点击目标进入会话 |
| `ensure_not_login_required` | 265 | 登录遮罩检测（失败→账号级错误） |
| `_dismiss_non_login_dialogs` | 271 | 关闭无关弹窗 |
| `save_debug_artifacts` | 381 | 失败截图留证 |

**send_loop 主流程**：

```python
async def send_to_targets(profile_name: str, targets: list[str], config: dict) -> list[dict]:
    results = []
    strategy = config["sendStrategy"]
    async with engine_browser_context(profile_name) as ctx:      # headless；DEBUG 时 SPARK_ENGINE_HEADFUL=1
        page = await ctx.new_page()
        await page.goto(CREATOR_CHAT_URL, wait_until="domcontentloaded", timeout=60_000)
        await ensure_not_login_required(page, account_name, "open_chat")   # 失败 → 所有目标记 login_required，返回
        ordered = random.shuffle(targets) if strategy["shuffleTargets"] else list(targets)
        for target in ordered:
            delay = random.randint(strategy["messageIntervalSecondsMin"], strategy["messageIntervalSecondsMax"])
            await asyncio.sleep(delay)                                       # 目标间隔
            message = pick_message(config)                                   # 复用 msg_builder.build_message_candidates 后随机取 1
            before = await snapshot_last_own_message(page)
            try:
                await scroll_and_select_user(page, account_name, target)     # 定位目标会话；找不到 → skipped/target_not_found
                chat_input = await locate_chat_input(page)
                await fill_and_send(chat_input, message)                      # 填入文本+回车/发送按钮
                ok, detail = await confirm_message_sent(page, chat_input, message, before)
                state = "strong" if ok else "weak"                            # 详见下表
                category = "" if ok else "send_failed"
                results.append({"target": target, "message": message, "state": state, "category": category, "detail": detail})
            except TargetNotFound:
                results.append({"target": target, "message": message, "state": "failed", "category": "target_not_found", "detail": "friend row not found"})
            except LoginRequired:
                results.append({... "category": "login_required"})            # 剩余目标全部补 login_required 后 break
            except Exception as exc:                                          # classify_browser_failure 平移（967 行）
                results.append({... "state": "failed", "category": classify_browser_failure(stage, exc), "detail": str(exc)})
        await save_debug_artifacts_if_failed(...)
    return results
```

**强确认规则（必须逐条照抄上游 confirm_message_sent，tasks.py:855-931）**：

```
轮询：每 1 秒一次，总时限 8 秒
每轮：
  1. 若页面出现发送失败指示（红感叹号/重试按钮/失败文案，443 行）→ 立即判 failed
  2. 主判定：输入框已清空 && 最后一条自己消息的文本 == 发送文本
             && 其(文本,centerY,right)签名 != 发送前快照签名 → strong
  3. 计数兜底：输入框清空且无快照时，当日该文本出现次数 > 发送前次数 → strong
超时后：
  - 输入框仍有残留文本 → failed("chat input still contains ...")
  - 最终计数兜底再判一次 → strong / 否则 failed
state 映射：判定 strong=确认送达；failed=确认失败；weak=超时但无明确失败指示（消息可能已发出）
```

### 6.4 到期目标选择（Go，`internal/spark/scheduler.go`）

从上游 `_select_due_targets`（tasks.py:1830-1888）翻译为 Go，数据源换成 SQLite：

```
func selectDueTargets(acct, cfg, now) (due, pending []string):
  targets = spark_friends WHERE account_id=? AND selected=1
  for t in targets:
     今日已强确认（spark_send_records 当日 confirm_state='strong' 存在）→ 跳过
     今日已失败                                                → 跳过（等手动重试或次日）
     account 在冷却（cooldown_until > now）                    → pending
     now >= scheduledTime(t)                                   → due
     否则                                                      → pending

手动模式（mode=now）    ：返回全部 selected 目标（无视窗口与已发）
手动模式（mode=failed） ：返回今日 confirm_state='failed' 的目标
手动模式（mode=unsent） ：返回今日无任何记录的目标
scheduledTime(t)（确定性散布，照抄 tasks.py:1816-1827）：
  windowMinutes = (endHour - startHour) * 60
  seed  = sha256(localDate + "|" + accountUniqueID + "|" + target)
  offsetMinutes = int(seed[0:8]) % windowMinutes
  return 当日 windowStart + offsetMinutes
窗口外（now < start 或 now > end + intervalMinutes 宽限）→ 不选任何目标
```

### 6.5 发送调度（Go，`internal/spark/scheduler.go`）

```
Run(ctx): ticker 30s，每 tick:
  1. 读 send_config；sendWindow.enabled=false → 跳过
  2. 不在窗口内（含宽限）→ 跳过
  3. 遍历 enabled 且 status 不在 {sending, login_required, cooldown} 的账号：
       due = selectDueTargets(...)
       if len(due)==0 → continue
       账号间随机延迟 accountStartDelaySecondsMin..Max
       置 status=sending（SSE spark.account.status）
       调引擎 POST /send/run {profile_name, targets: due, config}
       逐条结果写 spark_send_records（local_date=Asia/Shanghai 当日）+ SSE spark.send.progress
       汇总写回 account.last_send_at / status / failure_count_today
       账号级错误（login_required / account_error 类）→ failure_count_today+1，
         达到 accountFailurePause.attempts(3) → cooldown_until = now + cooldownMinutes(60)
  4. 与手动触发互斥：包级 mutex（内存锁）；正在跑时手动 API 返回 409 {"detail":"send already running"}
```

在 `cmd/server/main.go` 里 `go sparkScheduler.Run(ctx)`（与现有订阅调度器并列，互不影响）。

### 6.6 SSE 事件（复用 internal/events 总线，前端 useEvents 消费）

| type | data |
|---|---|
| `spark.account.status` | `{"account_id":1,"status":"sending","detail":""}` |
| `spark.send.progress` | `{"account_id":1,"target":"张三","state":"strong","detail":"..."}` |
| `spark.send.finished` | `{"account_id":1,"strong":4,"weak":1,"failed":0}` |
| `spark.friends.updated` | `{"account_id":1,"count":42}` |
| `spark.login.status` | `{"logged_in":true,"unique_id":"..."}` |

实现前先读 `backend/internal/events/events.go` 与 `frontend/src/api/useEvents.ts`，模仿既有事件的发布与缓存失效写法。

### 6.7 登录桌面集成（引擎）

1. **平移**：复制上游 `login_desktop_server.py`（引擎根目录）与 `scripts/start_login_desktop.sh`（原样；它负责 Xvfb+fluxbox+x11vnc+websockify+启动 login_desktop_server 18090 端口）。
2. **平移代理段**：把上游 `webui/app.py` 第 1269-1600 行的 `/login-desktop/*` 路由组（HTTP+WebSocket 反代 noVNC、`/qr`、`/qr/refresh`、`/status`、`/open`、`/close`、`/reset`、`/save`、`/heartbeat`、`/focus`、`/workspace-status`）平移到 `engine/login_bridge.py`，挂到引擎 FastAPI，路径前缀改为 `/login/`。环境变量：`SPARKFLOW_LOGIN_DESKTOP_API_URL=http://127.0.0.1:18090`、noVNC ws `http://127.0.0.1:6080`。
3. **引擎新增桥接**：`POST /login/export` —— 调 login_desktop 的 `POST /export`（login_desktop_server.py:734）拿身份（unique_id/nickname；其内部会访问 `www.douyin.com/user/self`，见 18 行 `WWW_SELF_URL` 与 622 行 `collect_www_login_result`，所以 www 域 Cookie 天然在 profile 里）→ 用登录 cookie 在 `state/browser-profiles/` 下播种 `uid-{unique_id}` 账号 profile（平移上游 seed 逻辑，tasks.py:333 `apply_stored_cookies_to_profile`）→ 再走 §6.1 导出 cookie → 一并返回。
4. **容器启动**：`spark-engine/docker/entrypoint.sh` 先后台 `bash scripts/start_login_desktop.sh`，再 `exec python main.py --port ${SPARK_ENGINE_PORT} --token ${SPARK_ENGINE_TOKEN}`。
5. **Go 反代**：`/api/spark/login/` → 引擎 `/login/`，`httputil.NewSingleHostReverseProxy` + WebSocket 支持（升级检查 `Connection: Upgrade` 时透传 Header）。挂 authGuard 之后（noVNC 不再裸露）。

---

## 7. 前端规格

**路由与导航**：`App.tsx` 增 `<Route path="spark" element={<SparkPage />} />`；`layout.tsx` 的 NAV_ITEMS 在"监控"后加 `{ to: "/spark", label: "火花", icon: Flame }`（lucide-react 已有 Flame）。

**`api/spark-types.ts`**：按 §5 契约定义 Account/Friend/SendRecord/SparkSettings/Overview 类型。

**`api/spark.ts`**（模仿 `endpoints.ts` 现有函数写法，走 `client.ts` 的 api()）：
`getSparkOverview / listSparkAccounts / createSparkAccount / updateSparkAccount / deleteSparkAccount / refreshSparkFriends(accountId) / listSparkFriends(accountId) / patchSparkFriends / triggerSparkRun(mode, accountIds?) / listSparkRecords(params) / getSparkSettings / putSparkSettings / exportSparkCookies(accountId)` + 登录组 `openLogin / loginQrUrl / refreshQr / loginStatus / exportLogin / closeLogin`。

**query keys**（加进 queries.ts 的 qk）：`spark.overview / spark.accounts / spark.friends(id) / spark.records(params) / spark.settings`。

**页面结构**（`pages/spark/spark-page.tsx`，Radix Tabs，风格对照 settings.tsx 分组卡片）：

| Tab | 内容 | 数据流 |
|---|---|---|
| 总览 | 引擎健康灯、账号状态卡列表、今日 strong/weak/failed 计数、窗口状态 | `qk.spark.overview` 轮询 30s + SSE 失效 |
| 账号 | 账号列表（启停 Switch、删除、刷新好友按钮）+ 每账号好友表格（勾选 selected、今日状态徽标）+ "扫码添加账号"按钮开 LoginDialog | accounts + friends |
| 控制台 | 手动触发三按钮（立即发送/重试失败/补发未发）+ 实时日志流（SSE spark.send.progress 逐条追加，最多保留 500 行） | useEvents |
| 记录 | 发送记录表：账号/目标/消息/状态徽标(strong 绿·weak 黄·failed 红)/时间，游标分页 | records |
| 设置 | §5.4 JSON 的表单化（模板与变体多行文本、窗口三数字、限速四数字、失败暂停两数字）+ "导出 Cookie 到归档"按钮 + 保存 | settings PUT |

**LoginDialog**：打开时 `POST /api/spark/login/open` → `<img src={/api/spark/login/qr?t=...}>` 每 5s 轮询 `loginStatus`（logged_in 后自动调 `exportLogin` → `POST /api/spark/accounts` 建号 → 触发 `refreshSparkFriends` + `exportSparkCookies`，toast 成功后关闭）+ "远程桌面"折叠区嵌 `<iframe src="/api/spark/login/vnc/vnc.html?autoconnect=1&resize=scale&path=api/spark/login/vnc/websockify">`（滑块验证等人工干预用）。QR 过期显示"点击刷新"（调 refresh-qr）。

---

## 8. 部署

### spark-engine/Dockerfile（全文模板）

```dockerfile
ARG PLAYWRIGHT_BASE_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/mcr.microsoft.com/playwright/python:v1.56.0-jammy
FROM ${PLAYWRIGHT_BASE_IMAGE}
WORKDIR /app
ENV TZ=Asia/Shanghai PYTHONUNBUFFERED=1
COPY requirements.txt .
RUN pip install --no-cache-dir -i https://pypi.tuna.tsinghua.edu.cn/simple -r requirements.txt
RUN sed -i 's/archive.ubuntu.com/mirrors.aliyun.com/g; s/security.ubuntu.com/mirrors.aliyun.com/g' /etc/apt/sources.list \
 && apt-get update && apt-get install -y --no-install-recommends \
      fluxbox fonts-wqy-zenhei novnc websockify x11vnc xvfb \
 && rm -rf /var/lib/apt/lists/*
COPY . .
ENV SPARK_ENGINE_PORT=18788 SPARKFLOW_BROWSER_PROFILE_ROOT=/app/state/browser-profiles
EXPOSE 18788
CMD ["bash", "docker/entrypoint.sh"]
```

`spark-engine/requirements.txt`（对齐上游版本）：
```
fastapi==0.117.1
uvicorn==0.34.0
playwright==1.56.0
requests==2.32.5
websockets==15.0.1
tzdata==2025.2
```

### docker-compose.yml 增量

```yaml
  spark-engine:
    build: { context: ./spark-engine, dockerfile: Dockerfile }
    profiles: ["spark"]
    restart: unless-stopped
    environment:
      TZ: Asia/Shanghai
      SPARK_ENGINE_PORT: "18788"
      SPARK_ENGINE_TOKEN: ${DY_SPARK_TOKEN:?set_in_.env}
    volumes:
      - spark-state:/app/state
  # 主 douyin 服务 environment 增：
  #   DY_SPARK_URL: http://spark-engine:18788
  #   DY_SPARK_TOKEN: ${DY_SPARK_TOKEN}
  # 并在顶层 volumes: 加 spark-state:
```

启动：`docker compose --profile spark up -d`（不启用 profile 时主项目照常跑，/api/spark/* 返回引擎不可达状态，前端显示空态+提示）。

### Go 配置增量（backend/internal/config/config.go）

`DY_SPARK_URL`（默认 `http://127.0.0.1:18788`）、`DY_SPARK_TOKEN`（默认空=禁用 spark API，返回 503 `{"detail":"spark not configured"}`）。

### 本地开发运行

```powershell
# 终端1：引擎（Windows 下登录桌面不可用，仅业务端点）
cd spark-engine
python -m pip install -r requirements.txt && python -m playwright install chromium
python main.py --port 18788 --token devtoken
# 终端2：帧藏（既有 dev.ps1），环境变量 DY_SPARK_URL/DY_SPARK_TOKEN
```

---

## 9. 任务清单（按序执行，每项完成即 commit）

> 说明：`[前置]` 列为必须先完成的任务。M0 需要真人扫码，执行模型只负责把脚本与页面准备好并给出操作指引。

### M0 — Cookie 闭环验证

**T0.1 引擎骨架冒烟**（前置：无）
- 建 `spark-engine/`：`main.py`（argparse：--port/--token，env 兜底 SPARK_ENGINE_PORT/TOKEN）、`engine/app.py`（FastAPI + token 校验中间件 + GET /health）、`requirements.txt`、`.gitignore`（`state/`、`__pycache__/`）
- 验收：`python main.py --port 18788 --token t1` 后 `curl -H "X-Engine-Token: t1" http://127.0.0.1:18788/health` 返回 200；错 token 返回 401
- 同时根目录 `.gitignore` 加一行 `spark-engine/state/`

**T0.2 Cookie 导出 spike**（前置 T0.1；需用户扫码 1 次）
- 写 `spark-engine/spike/cookie_spike.py`：headful Playwright 打开 `https://www.douyin.com/` → 手动扫码登录 → 登录成功判定（出现用户头像/跳转）→ `context.cookies()` 过滤 `.douyin.com` → 拼接写入帧藏 `<repo>/data/.cookie`（纯字符串，格式见 §6.1）
- 跑法：`python spike/cookie_spike.py --cookie-out ../data/.cookie`
- 验收：启动帧藏（非 mock）后对任一博主触发扫描，`GET /api/creators/{id}` 的 last_scan 无 cookie/风控错误且取到作品数
- **若验证失败**（Cookie 字段不足）：在 spike 里先 `page.goto("https://www.douyin.com/user/self")` 等待 3s 再导出；仍失败则在 §12 记录，M2 的 cookies/export 仍实现，但 README 注明归档可选手动贴 Cookie

### M1 — 引擎实现

**T1.1 引擎应用壳补全**（前置 T0.1）：补齐 §4 表中除 send/friends/cookies/login 外的通用件：互斥 asyncio.Lock、统一错误 `{"detail":...}`、uvicorn 启动、结构化日志（复用 utils/logger.py 平移版）。验收：/health 带 `task_running` 字段。

**T1.2 core 模块平移**（前置 T0.1）
- 复制上游 → 引擎（逐文件）：
  - `core/browser.py`：删 `from rich.console import Console`（改 logging）；保留 `get_persistent_browser_context / sanitize_profile_name / select_douyin_network_mode`（direct 模式固定）；profile root 读 `SPARKFLOW_BROWSER_PROFILE_ROOT`（上游已有此 env 逻辑）
  - `core/friends.py`：原样；`utils/logger.py`：原样；`utils/hitokoto.py`：原样
  - `core/msg_builder.py`：原样（去掉 happyNewYear 分支及其 import）
  - `core/send_state.py`：原样
  - `utils/config.py`：**重写裁剪版**：删除 usersData/config.json/webui_settings 读写，仅保留 `normalize_unique_id / DEFAULT_* 常量 / repo_root`
- 验收：`python -c "import core.browser, core.friends, core.msg_builder, core.send_state"` 零报错

**T1.3 发送工具函数平移 + send_loop**（前置 T1.2）★核心
- 按 §6.3 表格把 tasks.py 的 11 个函数摘到 `core/send_tools.py`（保留原实现，仅去掉其中对 usersData/config 的引用，参数化）
- 新写 `engine/send_loop.py`（§6.3 主流程）+ `fill_and_send`（输入框填文本→Enter 发送，上游模式）
- 验收：`python -m pytest`（或最小脚本）对 send_tools 的 normalize/snapshot 逻辑做导入冒烟；send_loop 通过 T1.7 冒烟

**T1.4 /friends/refresh + /cookies/export**（前置 T1.2）
- §6.1、§6.2 实现；好友刷新走引擎互斥锁
- 验收：本地用 spike 登录过的 profile 调 `/friends/refresh` 返回好友 JSON；`/cookies/export` 返回非空 cookie

**T1.5 登录桌面集成**（前置 T1.2；本地仅做导入级验证，容器内全功能）
- §6.7 全部 5 点：平移 login_desktop_server.py + start_login_desktop.sh + 代理段（上游 app.py 1269-1600 → engine/login_bridge.py，前缀 /login/）+ `/login/export` 桥接
- 验收：`python -c "import engine.login_bridge"` 零报错；Docker 构建（T4.1 提前跑一次）后 `/login/open` → `/login/qr` 返回 PNG

**T1.6 /send/run 端点**（前置 T1.3）：请求校验 → 锁 → 后台任务（FastAPI BackgroundTasks）→ 完成后结果整体返回（同步等待也行，v1 简化：**同步等待**，Go 侧设 10 分钟超时）。验收：mock targets（无浏览器时返回 account_error）路径通。

**T1.7 引擎本地冒烟**（前置全部 M1）：真实账号小范围发送 1-2 个目标（需用户配合），核对 spark_send_records 三种状态与 SSE。若用户暂不测，记录到 §12 并继续 M2。

### M2 — Go 接入层

**T2.1 0006_spark.sql**（前置无）：§3 全文 + `spark_settings` 初始行。验收：`go test ./internal/db/...` 通过；用临时库手跑 `Migrate` 两遍（幂等）。

**T2.2 internal/spark 骨架**：`client.go`（BaseURL/Token 来自 config；`do(ctx, method, path, body, out)` 封装：X-Engine-Token 头、30s 普通超时、10min send 超时、非 2xx 解析 detail）、`store.go`（§3 四表 CRUD + `TodayStats(accountID)` 聚合）。验收：go build + 针对临时 SQLite 的 store 单测。

**T2.3 API handlers**（前置 T2.1/T2.2）：`api.go` 实现 §5 全部路由；`server.go` 加 `s.registerSparkRoutes(mux)`（模仿现有 register* 风格，endpoints 切片）；`/api/spark/login/*` 反代（§6.7.5）。验收：go vet/test；curl 冒烟（引擎关闭时 overview 返回 engine.ok=false 而非 500）。

**T2.4 调度器**（前置 T2.2）：§6.4+§6.5；`cmd/server/main.go` 接线。验收：单测覆盖 selectDueTargets 四分支（已发/已失败/冷却/到期）与 scheduledTime 确定性（同 seed 同值）。

**T2.5 SSE + cookies/export**：§6.6 事件发布点接入发送调度与好友刷新；`POST /api/spark/cookies/export` 写 data/.cookie（§6.1 Go 侧）。验收：联调时前端能看到事件；data/.cookie 内容更新。

**T2.6 Go 测试补齐**：api 契约测试（模仿 subscriptions_test.go 的 httptest 风格）。验收：`go test ./... -count=1` 全绿。

### M3 — 前端

**T3.1 types+endpoints+queries**（前置无，可与 M2 并行）：§7 的 spark-types.ts / spark.ts / qk 扩展。验收：tsc -b 零错误。

**T3.2 页面骨架**：spark-page.tsx（Tabs 五枚）+ App.tsx 路由 + layout.tsx 导航项。验收：npm run dev 后 /spark 可达，五 Tab 空态渲染。

**T3.3 账号 Tab**：账号卡片（启停/删除/刷新好友）+ 好友表（勾选/全选/今日状态）+ 添加账号按钮（先 toast "T3.6 接入"）。验收：与 T2.3 联调 CRUD 全通。

**T3.4 控制台 + 记录 Tab**：三触发按钮（409 时 toast "已有任务在跑"）+ SSE 日志流；记录表分页。验收：手动触发后日志实时滚动。

**T3.5 总览 + 设置 Tab**：引擎健康灯/账号卡/今日计数/窗口状态；设置表单 PUT；"导出 Cookie"按钮成功 toast 显示 cookie 数。验收：改动设置后调度行为随之下一次 tick 生效（日志可证）。

**T3.6 LoginDialog**：§7 登录组件全流程。验收：容器内端到端（或本地以 spike 登录的 profile 手动建号兜底）。

**T3.7 构建验证**：`npm run build` 零错误；`npm run preview` 走查五 Tab。

### M4 — 部署与收尾

**T4.1 Dockerfile.engine + entrypoint.sh**（前置 T1.5）：§8 模板。验收：`docker build` 成功，容器起后 /health 200。

**T4.2 compose 集成**：§8 增量 + `.env.example` 加 DY_SPARK_TOKEN。验收：`docker compose --profile spark up -d` 全绿；主服务读 DY_SPARK_URL 正常。

**T4.3 服务器部署 + 文档**：`scripts/deploy_docker.sh` 兼容 profile（读 DOCS/DOCKER.md 后最小改动）；README 架构图加 spark-engine 一行；docs/api.md 追加 §spark 契约（引用本文档 §4/§5 表格）。验收：服务器 `docker compose --profile spark up -d` 后浏览器完成一次「登录→建号→刷好友→手动发送→记录/SSE 可见」。

**T4.4 回归与收尾**：go test 全绿、npm build 通过、主项目 mock 模式（DY_MOCK=1）不受影响；更新 docs/PROGRESS.md 与 SPARKFLOW_MERGE_PLAN.md 决策状态。

---

## 10. 禁止事项与风险（执行者必读）

1. **许可红线**：spark-engine/ 中所有源自上游的文件必须保留 §0.2 的来源与 PolyForm 许可注释；不得把它们以 MIT 名义声明；不得删除。
2. `douyin-sparkflow/` 只读；上游更新时的比对方法见 SPARKFLOW_MERGE_PLAN §2.1。
3. 自动发消息存在平台风控（限流/封号）风险：默认限速值（25-70s 间隔）不得调低；仅操作本人账号。
4. data/.cookie 含完整登录凭据：不得打日志、不得进 git、不得出现在错误响应里。
5. 前端"记录/控制台"不得展示 cookie 或完整 profile 路径。
6. 不要顺手重构既有代码（哪怕看到可优化点）——记录到 §12 待办即可。

---

## 11. 里程碑验收总表

| 里程碑 | 完成标志 | 预估 |
|---|---|---|
| M0 | spike 扫码导出 Cookie 后帧藏真实扫描成功（或记录失败原因） | 0.5-1 天 |
| M1 | 引擎 5 组端点本地可调，发送闭环冒烟 | ~1 周 |
| M2 | go test 全绿，/api/spark/* 联调通过，调度器单测覆盖 | ~1 周 |
| M3 | 前端五 Tab + 登录组件可用 | ~1 周 |
| M4 | 服务器 profile 部署端到端成功 | 3-4 天 |

---

## 12. 执行日志（执行模型填写：勾选 + 决策 + 问题）

- [ ] T0.1 …（逐项勾选）
- 决策记录：（格式 `日期 | T编号 | 决策 | 原因`）
- 问题与偏差：
- 待办（不扩scope，仅记录）：

## 13. 参考文件速查

| 要看什么 | 路径 |
|---|---|
| 上游发送/确认实现 | douyin-sparkflow/DouYinSparkFlow/core/tasks.py（函数行号见 §6.3 表） |
| 好友抓取 | 同目录 core/friends.py |
| 登录桌面服务 | 同目录 login_desktop_server.py（端点行号见 §6.7） |
| 登录桌面代理参考 | 同目录 webui/app.py:1269-1600 |
| 帧藏 F2 侧车（Cookie 热加载） | sidecar/main.py:85-98 |
| Go 侧车管理（参考风格，v1 不实现） | backend/internal/sidecar/manager.go |
| Go 迁移模式 | backend/internal/db/migrate.go + migrations/ |
| Go 路由注册风格 | backend/internal/api/server.go（register/endpoints helper） |
| Go 调度器参考 | backend/internal/scheduler/scheduler.go |
| SSE 总线 | backend/internal/events/events.go；frontend/src/api/useEvents.ts |
| 前端 API 封装 | frontend/src/api/client.ts / endpoints.ts / queries.ts |
| 设置页 UI 风格 | frontend/src/pages/settings.tsx |
