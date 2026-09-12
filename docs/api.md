# API 契约(v1 · 冻结)

> 本文件是前后端并行开发的唯一依据。字段名一旦发布不再更改;新增字段只能追加。
> 除非特别说明,所有请求/响应均为 JSON;认证方式为 HttpOnly cookie `dy_session`。
> 未认证访问业务接口返回 `401 {"detail":"unauthorized"}`;错误统一 `{"detail": "..."}`。

## 通用约定

- 分页:页码式 `?page=1&page_size=20`(page 从 1 起,page_size ∈ {20,50,100}),响应 `{"items":[...],"total":123,"page":1,"page_size":20}`
- 任务历史用游标式:`?cursor=<int64 上一页最后一条 id>&limit=50`,响应 `{"items":[...],"next_cursor":123|null}`
- 时间:UTC RFC3339 字符串,如 `2026-09-11T08:00:00Z`
- 枚举:
  - work 下载状态 `dl_status`: `none | queued | downloading | succeeded | failed | canceled`(来自该作品最新一条任务;多资产部分成功按 `succeeded` 处理前提是视频资产成功)
  - job 状态 `status`: `queued | downloading | paused_q | succeeded | failed | canceled`
  - 订阅 `target_type`: `creator | collection`;`schedule` 增量扫描参数
  - 扫描状态: `running | succeeded | partial | failed`

---

## 认证 Auth

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/auth/status` | → `{authenticated, initialized, username?}` |
| POST | `/api/auth/setup` | 首次初始化 `{username, password}`(仅未初始化时可用) |
| POST | `/api/auth/login` | `{username, password}` → `{ok, username}`;连续失败限流(内存按 IP) |
| POST | `/api/auth/logout` | 清会话 |
| POST | `/api/auth/password` | `{old_password, new_password}` |

## 健康

| GET | `/api/health` | → `{status:"ok", provider:"mock"|"sidecar", sidecar:"stopped"|"starting"|"running", real_scan_ready:bool}` |

## 博主与作品 Creators / Works

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/creators` | `{profile_url}` → **202** `{creator_id, scan_id}`(异步扫描,SSE 报进度)。URL 支持主页链接或 sec_uid。重复添加返回已有 creator 并同样触发扫描 |
| GET | `/api/creators` | → `[{id, sec_uid, nickname, avatar_url, profile_url, reported_work_count, works_count, downloaded_count, created_at}]`(按 created_at desc) |
| GET | `/api/creators/{id}` | 单个详情,字段同上 + `last_scan: {id, status, pages, new_count, updated_count, empty_pages, completeness, started_at, finished_at, last_error}` |
| DELETE | `/api/creators/{id}` | 删除博主及其作品/合集/任务记录(不删已下载文件) |
| POST | `/api/creators/{id}/rescan` | `{full?:bool}` → 202 `{scan_id}`;full=true 全量,默认增量 |
| GET | `/api/creators/{id}/collections` | → `[{id, mix_id, name, cover_url, works_count, downloaded_count}]` |
| GET | `/api/creators/{id}/works` | `?page=&page_size=&q=&collection_id=&sort=published_at_desc\|published_at_asc\|duration_desc` → 分页作品。item 字段见下 |
| GET | `/api/works/{id}` | 单作品详情(含 `mix_info`、`asset` 已下载资产列表、`last_job` 最新任务摘要) |
| POST | `/api/works/batch-ids` | `{creator_id, q?, collection_id?}` → `{ids:[int]}`(**仅 id**,服务"按筛选全选") |
| GET | `/api/works/{id}/qualities` | 实时解析清晰度 → `[{quality:"540p"\|"720p"\|"1080p", width, height, bitrate, size_bytes}]`(需侧车在线;离线 503) |

work item 字段(白名单,列表用):

```json
{
  "id": 123, "item_id": "v1a2b3...", "title": "标题", "cover_url": "https://...",
  "duration": 45, "published_at": "2026-01-01T00:00:00Z",
  "collection_id": 7, "collection_name": "合集名",
  "type": "video", "image_count": 0,
  "dl_status": "succeeded", "downloaded_quality": "1080p",
  "created_at": "..."
}
```

`type ∈ video|image`;image(图集/动图)作品 `duration=0`、`image_count` 为图集张数。
资产存储约定:图集作品按**作品名聚合**为目录 `downloads/{creator}/collections|singles/{safeName(标题)}/`,
每张图为资产 `kind="image"`、`quality` 为 4 位序号("0001"、"0002"... 按原始顺序);
动图视频片段 `kind="video"`、`quality="live0001"...`;封面与 metadata 同目录。
`GET /api/works/{id}/assets` 排序:video(最新在前)→ image(quality 序号升序)→ cover → metadata。

## 合集 Collections

| GET | `/api/collections/{id}/works` | 同 creator works,按合集过滤(参数一致) |

## 下载任务 Downloads

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/downloads` | `{work_ids:[int], quality?: "540p"\|"720p"\|"1080p"}` → 202 `{created:[job_id], skipped:[{work_id, reason}]}`;已 succeeded 且画质相同的跳过 |
| GET | `/api/downloads` | `?status=&cursor=&limit=` → 游标分页 job 列表(status 可多值逗号分隔,如 `queued,downloading`) |
| GET | `/api/downloads/summary` | → `{queued, downloading, failed, succeeded, canceled, paused:bool, concurrency:int}` |
| POST | `/api/downloads/{id}/retry` | `{quality?}` 排队重试(failed/canceled/succeeded 均可) |
| POST | `/api/downloads/{id}/cancel` | **queued 或 downloading 均可取消**;downloading 走 context 取消 |
| DELETE | `/api/downloads/{id}` | 删除任务记录(进行中的先拒绝) |
| POST | `/api/downloads/batch` | `{action:"retry"\|"cancel"\|"delete", ids:[job_id], quality?}` → `{affected:int}` |
| POST | `/api/downloads/retry-failed` | 重试全部 failed(attempts<5)→ `{affected}` |
| POST | `/api/downloads/clear-completed` | 清空 succeeded+canceled 记录 → `{affected}` |
| POST | `/api/downloads/queue/pause` | 暂停派发(进行中不中断) |
| POST | `/api/downloads/queue/resume` | 恢复派发 `{retry_failed?:bool}` |

job 字段:

```json
{
  "id": 99, "work_id": 123, "creator_id": 1, "work_title": "标题", "creator_nickname": "昵称",
  "status": "downloading", "quality": "1080p", "attempts": 1,
  "total_bytes": 52428800, "downloaded_bytes": 10485760, "speed_bps": 2621440,
  "error": null, "queued_at": "...", "started_at": "...", "finished_at": null
}
```

## 资产与播放 Assets

| GET | `/api/assets/{id}/content` | 流式返回文件,**支持 Range**(Go http.ServeContent);asset 字段 `{id, work_id, kind:"video"\|"cover"\|"metadata", path, size_bytes, quality?}` |
| GET | `/api/works/{id}/assets` | 该作品资产列表(视频资产在前) |

## 订阅监控 Subscriptions

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/subscriptions` | → `[{id, target_type, creator_id, collection_id?, creator_nickname, target_name, interval_minutes, auto_download, quality, last_run_at, next_run_at, enabled}]` |
| POST | `/api/subscriptions` | `{target_type:"creator"\|"collection", creator_id, collection_id?, interval_minutes, auto_download:bool, quality?}` |
| PATCH | `/api/subscriptions/{id}` | 部分更新(同上字段均可选) |
| DELETE | `/api/subscriptions/{id}` | |

新作品在 `auto_download=true` 时自动按 `quality` 入队(默认 `1080p`,没有则最高档)。

## 设置 Settings

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/settings` | → 全部运行时设置(敏感值打码:`cookie` 只回前 8 字符 + `...`) |
| PATCH | `/api/settings` | 部分更新;字段见下;`cookie` 变更后自动重载 provider |
| POST | `/api/notifications/test` | 发测试邮件 → `{ok, error?}` |

设置字段:`provider_mode(auto|sidecar|mock)`, `cookie(string)`, `download_concurrency(1-8)`, `download_quality(默认画质)`, `scan_page_delay_ms(1000-10000)`, `scan_max_empty_pages(1-10)`, `incremental_stop_pages(1-10)`, `completeness_gap_threshold(1-50)`, `sidecar_idle_timeout_minutes`, `smtp(host,port,username,password,from,to)`, `notify_on_new_work(bool)`, `notify_on_failure(bool)`。

## SSE 事件流(替代轮询)

`GET /api/events`(需认证)→ `text/event-stream`。事件格式:`event: <type>\ndata: <json>\n\n`

```
event: download.progress
data: {"job_id":99,"downloaded_bytes":10485760,"total_bytes":52428800,"speed_bps":2621440}

event: download.status
data: {"job_id":99,"status":"succeeded","error":null,"work_id":123}

event: scan.progress
data: {"scan_id":5,"creator_id":1,"page":12,"new_count":180,"updated_count":2,"status":"running"}

event: scan.done
data: {"scan_id":5,"creator_id":1,"status":"succeeded"|"partial"|"failed","pages":26,"new_count":502,"completeness":0,"last_error":null}

event: provider.status
data: {"sidecar":"running","risk_paused":false,"paused_until":null}
```

规则:每 15s 发 `: ping` 保活;客户端重连指数退避;`download.progress` 仅对 downloading 任务、节流 1s 一条;事件总线缓冲 256,慢消费者丢弃 progress 类事件(状态类必达)。

## 静态前端

非 `/api/*` 路径全部回落到嵌入的 SPA `index.html`(go:embed `frontend/dist`)。

---

# 侧车契约(Go ↔ Python,127.0.0.1:18787)

- 鉴权:每次请求带 `X-Sidecar-Token`(启动时由 Go 生成并通过 argv 传入侧车,防本机其他进程误用)
- 模式:环境变量 `DY_MOCK=1` 时返回确定性假数据(开发/测试,不触网)
- Go 端职责:按需拉起 `python sidecar/main.py --port 18787 --token <t>`、健康检查、空闲超时杀死

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | → `{status:"ok", mock:bool, cookie_loaded:bool}` |
| GET | `/profile?sec_uid=` | → `{sec_uid, nickname, avatar_url, aweme_count, signature}` |
| GET | `/posts?sec_uid=&cursor=&count=20` | → `{items:[{item_id,title,cover_url,duration,published_at,mix_id,mix_name,type,image_count}], has_more, next_cursor}`;**任何失败返回非 200 + `{"error":"..."}`,绝不吞错返回空 items**;`type ∈ video\|image`(`aweme.images` 非空即 image),image 时 `image_count` 为图集张数、duration 为 0 |
| GET | `/work?item_id=` | → `{item_id, title, type, variants:[{quality,width,height,bitrate,size_bytes,url,urls?:[...]}], images?:[{url,urls,width,height}], live_videos?:[{url,urls}], cover_url, duration}`;`type ∈ video\|image`(aweme.images 非空即 image)。video 作品:variants 非空(每档 `urls` 为同资产多个 CDN 候选,下载方按序尝试,`url` 恒等于首个候选),无 images;image 作品:反之(variants 为空数组),`images` 为图集逐张地址(按原始顺序),`live_videos` 为图集中动图/实况的视频片段(可为空) |

约定:
1. `published_at` 侧车负责从 create_time 转成 RFC3339 UTC
2. `next_cursor` 直接透传抖音返回的 max_cursor;`has_more=false` 时为 null
3. 侧车只做"签名 + 拉取 + 结构裁剪",不做重试、不入库(Go 管重试,修漏扫)
4. Cookie 从 `data/.cookie` 文件读取(Go 在设置变更时写入),侧车热加载每次请求读文件,无需重启

# 数据库表(供后端实现参照,字段名即列名)

```sql
creators(id INTEGER PK, sec_uid TEXT UNIQUE, nickname, avatar_url, profile_url,
         reported_work_count INTEGER DEFAULT 0, created_at, last_scan_at)
collections(id INTEGER PK, creator_id INT REFERENCES creators, mix_id TEXT,
            name, cover_url, created_at, UNIQUE(creator_id, mix_id))
works(id INTEGER PK, creator_id INT REFERENCES creators, collection_id INT NULL REFERENCES collections,
      item_id TEXT, title, cover_url, duration INTEGER, published_at,
      deleted_at TEXT NULL, created_at, updated_at,
      UNIQUE(creator_id, item_id))          -- 每创作者唯一,非全局
-- 下载状态不存 works(由最新 download_jobs 推导),asset 如下
assets(id INTEGER PK, work_id INT REFERENCES works, kind TEXT, path TEXT,
       size_bytes INTEGER, quality TEXT NULL, created_at, UNIQUE(work_id, kind, quality))
download_jobs(id INTEGER PK, work_id INT REFERENCES works, creator_id INT,
              quality TEXT, status TEXT, attempts INTEGER DEFAULT 0,
              total_bytes INTEGER DEFAULT 0, downloaded_bytes INTEGER DEFAULT 0,
              error TEXT NULL, queued_at, started_at NULL, finished_at NULL)
              -- 无 UNIQUE(work_id,status) 陷阱;索引 (status,id)、(work_id)
subscriptions(id INTEGER PK, target_type TEXT, creator_id INT, collection_id INT NULL,
              interval_minutes INTEGER, auto_download INTEGER, quality TEXT,
              enabled INTEGER DEFAULT 1, last_run_at NULL, created_at)
scan_runs(id INTEGER PK, creator_id INT, trigger TEXT, full INTEGER,
          status TEXT, pages INTEGER DEFAULT 0, new_count INTEGER DEFAULT 0,
          updated_count INTEGER DEFAULT 0, empty_pages INTEGER DEFAULT 0,
          completeness INTEGER DEFAULT 0, last_error TEXT NULL,
          started_at, finished_at NULL)
sessions(token TEXT PK, username, expires_at)
settings(key TEXT PK, value TEXT)          -- 运行时设置,覆盖 .env
provider_state(id INTEGER CHECK(id=1), risk_paused INTEGER, paused_until NULL,
               consecutive_failures INTEGER, updated_at)
```
