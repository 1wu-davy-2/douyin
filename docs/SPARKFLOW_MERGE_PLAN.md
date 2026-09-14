# 抖音归档 v2（帧藏）× douyin-sparkflow 融合方案（合并计划）

> 日期：2026-09-14
> 范围：halfwaystudent/douyin-sparkflow（本地副本 `douyin-sparkflow/`，commit 7c7d9c2，已加入 .gitignore）与本仓库（douyin-archive v2）的对比与融合规划。
> 结论先行：**两项目功能互补、技术栈几乎零交集。推荐"方案 A 并行双栈"起步（1–2 天可用），中期按使用频率决定是否演进到"方案 B 深度整合"（spark 引擎侧车化 + React 统一页面）。**
>
> **【状态更新 2026-09-14】方案 B 已直接落地实施完毕**（dev-huohua 分支）：spark-engine 无状态引擎化（M1）→ Go 接入层 `/api/spark/*`（M2）→ React `/spark` 五 Tab（M3）→ Docker 部署与文档（M4）。执行明细见 `HUOHUA_EXECUTION_PLAN.md` §12；残留项：真实账号冒烟（T1.7）、Docker 构建实测。

---

## 1. 两项目结构对比

### 1.1 功能定位

| 维度 | 帧藏（本仓库 douyin-archive v2） | douyin-sparkflow |
|---|---|---|
| 一句话定位 | 抖音**公开作品**订阅与增量归档工具 | 抖音**好友火花标记**自动维护系统（多账号） |
| 数据方向 | 拉取入库（下载到本地/MinIO） | 外发操作（自动给好友发消息） |
| 账号要求 | 无需登录抖音账号，仅需网页版 Cookie（只读公开数据） | 需要**完整登录态**（扫码登录，操作 creator.douyin.com 私信） |
| 核心动作 | 扫描主页/合集 → 下载视频/图集 → 播放/归档 | 刷新好友列表 → 按时间窗口定时发消息 → 强确认/重试 |
| 账号数量 | 单 Web 管理员（本机单用户工具） | 多抖音账号 + 多 Web 用户（账号可分配） |
| 产物 | SQLite 数据库 + 磁盘媒体文件 | 发送记录（JSON）、好友列表快照 |
| 许可证 | MIT | **PolyForm Noncommercial 1.0.0（禁止商用，2026-08-27 从 MIT 改制）** |

**互补性判断**：两者面向同一个用户画像（抖音个人账号的自动化管理），但一个管"内容收"（归档）、一个管"消息发"（互动）。功能不重叠、不冲突，融合价值在于统一入口、统一部署、统一运维。

### 1.2 技术栈对比

| 层 | 帧藏 | douyin-sparkflow |
|---|---|---|
| 后端 | Go 1.25 单二进制（嵌入前端产物，常驻 ~30MB） | Python 3.9+ / FastAPI + uvicorn |
| 前端 | React 18 SPA（Vite + TS + Tailwind v4 + Radix UI + TanStack Query + react-router 7） | Jinja2 模板 + Vanilla JS（app.js 746 行，Lucide 图标内嵌） |
| 数据存储 | SQLite（modernc 纯 Go，免 CGO） | JSON 文件（usersData.json / config.json / webui_settings.json）+ 日志文件 |
| 抖音交互 | Go 直连 CDN 下载；F2 签名走 Python 侧车（按需拉起、空闲自毁） | Playwright 浏览器自动化（creator.douyin.com 聊天页 DOM 抓取/发送）+ 可选协议模式（Node 子进程 protocol_sender.mjs） |
| 调度 | Go internal/scheduler（30s tick + 抖动，进程内） | 独立 scheduler 容器跑 cron_runner.py 读 crontab 文件；另有 GitHub Actions 定时 |
| 认证 | Go session cookie + authGuard 中间件（单管理员，首访初始化） | FastAPI SessionMiddleware + CSRF + 多用户存储 + bootstrap 管理员 |
| 代理 | 无（直连） | Mihomo（Clash Meta）容器，direct 优先 / mihomo 回退 |
| 远程登录 | 不适用（贴 Cookie 即可） | noVNC + x11vnc + fluxbox 登录桌面容器（远程扫码） |
| 容器化 | 三阶段构建单容器（node → golang → python:3.12-slim 运行时） | 5 个服务：web / login-desktop / proxy / scheduler / task；镜像基于 playwright-python（~2GB+） |
| 端口 | 8787（可配 DY_PORT） | 8787(web) / 8788(noVNC) / 7890+9090(Mihomo) —— **与主项目 8787 冲突** |

### 1.3 页面结构对比

**帧藏（React SPA，5 个路由 + 2 个门页）**

| 路由 | 页面 | 内容 |
|---|---|---|
| （无路由） | SetupPage | 首次初始化（创建管理员） |
| （无路由） | LoginPage | 登录 |
| `/` | LibraryPage 作品库 | 博主列表 + 作品/合集双面板（1528 行，最重页面）+ 全局播放器（视频流式 / 图集幻灯片） |
| `/downloads` | DownloadsPage | 下载任务管理（状态筛选/分页/批量操作） |
| `/subscriptions` | SubscriptionsPage | 监控订阅管理（783 行） |
| `/settings` | SettingsPage | 分组卡片：Provider / 扫描 / 下载 / 通知(SMTP) / MinIO / 安全（664 行） |

后端 API 面（Go，`/api/*` 前缀，SSE 事件流）：auth / health / settings / events / creators(+works/collections/roots) / subscriptions / downloads / assets / minio / mockcdn。

**douyin-sparkflow（Jinja2 多页 + 锚点单页）**

| URL | 页面 | 内容 |
|---|---|---|
| `/login` | login.html | Web 控制台登录 |
| `/` | dashboard.html | 首页总览，**单页锚点聚合**：账号管理（#account-management）、登录工作区（#interactive-login-section，扫码/QR）、运行配置（#config-panel）、运维操作（#ops-panel）、系统设置（#settings-panel） |
| `/ops/send-console` | send_console.html | 发送控制台（实时任务日志） |
| `/ops/logs` | logs.html | 发送记录（仅管理员） |
| `/login-desktop/proxy/*` | — | noVNC HTTP/WS 反代（登录桌面） |

后端路由（FastAPI，40+ 端点，非 REST 风格、表单提交为主）：认证（/login /bootstrap /logout /admin/users/*）、账号（/accounts/{id}/update|toggle-enabled|friends/refresh|delete|retry-target|mark-target-unconfirmed）、运维（/ops/overview|run-now|run-failed|run-unsent|reset-today-unconfirmed|proxy/*|schedule）、配置（/config /settings）、登录桌面（/login-desktop/qr|status|open|close|reset|save|heartbeat|focus|workspace-status + noVNC 代理）。

### 1.4 部署形态对比

```
帧藏（单容器）                         sparkflow（5 容器组）
┌─────────────────────┐              ┌────────────────────────────┐
│ douyin-archive      │              │ douyin-web (FastAPI:8787)  │
│ Go 单二进制 :8787    │              │ login-desktop (noVNC:8788) │
│  ├ 嵌入 React SPA   │              │ proxy (Mihomo:7890/9090)   │
│  ├ SQLite + 磁盘     │              │ scheduler (cron_runner)    │
│  └ F2 侧车(按需拉起) │              │ task (一次性 --doTask)      │
└─────────────────────┘              └────────────────────────────┘
常驻 ~30MB                           常驻 Go 无 —— Python+Chromium+VNC，
构建 ~60s（缓存后）                    镜像 2GB+，挂载 docker.sock*
```
*sparkflow 的 web 容器挂载了宿主 docker.sock（历史遗留的容器运维功能，新版 scheduler 已改直接运行，可视为可选挂载）。

### 1.5 目录结构对照

```
douyin/（本仓库）                          douyin-sparkflow/DouYinSparkFlow/
├─ backend/        Go 主后端              ├─ core/
│  ├ cmd/server     入口(main.go)          │  ├ tasks.py        2833 行  任务调度核心
│  └ internal/                            │  ├ browser.py       187 行  Playwright 管理/网络模式
│     ├ api/        路由+处理器             │  ├ friends.py      374 行  好友列表抓取(DOM选择器)
│     ├ scanner/   扫描器状态机            │  ├ login.py         83 行  扫码登录
│     ├ downloader/ 下载 worker 池          │  ├ msg_builder.py  129 行  消息模板(一言/节日/变体)
│     ├ scheduler/  订阅调度                │  ├ protocol_dispatch.py 406 行 协议发送调度
│     ├ provider/   数据源抽象              │  ├ protocol_sender.mjs   766 行 Node 协议客户端
│     ├ sidecar/    F2 侧车管理            │  └ send_state.py    51 行  发送确认状态
│     ├ settings/   设置存储               ├─ webui/
│     ├ events/     SSE 事件总线            │  ├ app.py         1643 行  FastAPI 主应用
│     ├ auth/       认证                   │  ├ ops.py         1184 行  运维操作
│     └ db/         SQLite 迁移            │  ├ users.py        278 行  多用户
├─ frontend/        React SPA              │  ├ auth.py         116 行  会话/CSRF
├─ sidecar/         Python F2 签名(724行)   │  └ login_lock.py   346 行  登录工作区锁
├─ scripts/         dev/build ps1          ├─ utils/            config/logger/hitokoto
├─ docs/            api/PROGRESS/DOCKER    ├─ login_desktop_server.py  登录桌面
└─ docker-compose.yml (单服务)            ├─ scripts/cron_runner.py   定时执行
                                          ├─ webui/static/multiPagePlugins/  22 个注册/邮箱辅助插件 js
                                          └─ vendor/webdeps/*.whl     内嵌 Python 依赖
```

### 1.6 代码规模

| | 帧藏 | sparkflow |
|---|---|---|
| 后端 | Go ~70 文件（api 层非测试约 2400 行 + scanner/downloader/scheduler 等） | Python ~10.6k 行（含测试） |
| 前端 | 32 个 tsx 约 6k 行（library 1528 / subscriptions 783 / settings 664 / downloads 488） | 模板 + app.js 746 行 + multiPagePlugins 22 个 js |
| Python 侧 | sidecar 724 行（纯标准库） | core+webui+utils 全量 |

---

## 2. 可复用模块识别

### 2.1 sparkflow 侧资产分级

**A 级：低耦合，可直接搬运（无 Web 依赖，纯逻辑/纯进程）**
| 模块 | 行数 | 说明 |
|---|---|---|
| core/msg_builder.py | 129 | 消息模板渲染（一言 API、节日祝福、多变体），仅依赖 utils/config |
| utils/hitokoto.py | 48 | 一言 API 客户端 |
| core/send_state.py | 51 | 发送强确认状态判定 |
| login_desktop_server.py | — | 登录桌面独立服务（API + noVNC），方案 B 中可整体保留 |

**B 级：中耦合，需接口化改造后复用（核心能力所在）**
| 模块 | 行数 | 复用要点 |
|---|---|---|
| core/tasks.py | 2833 | 发送任务全流程（账号遍历/窗口/限速/重试/冷却/确认）。**最大最有价值也最难搬**：与 JSON 存储、日志、浏览器层交织，深度整合时建议按"行为规格"重写而非逐行搬运 |
| core/browser.py | 187 | Playwright 启动/持久化 Profile/代理选择，改造量小 |
| core/friends.py | 374 | 好友列表 DOM 抓取（多选择器兜底 + 虚拟滚动），**对抖音页面改版敏感，需跟进上游** |
| core/login.py | 83 | 扫码登录流程 |
| core/protocol_dispatch.py + protocol_sender.mjs | 406+766 | 协议模式发送（Node 子进程），与浏览器模式二选一，可后置 |

**C 级：高耦合，不建议复用（将被主项目等价物替代）**
- webui/ 全部（app.py 1643 + ops.py 1184 + users.py + auth.py + login_lock.py）→ 方案 B 中被 React 页面 + Go API 替代
- scripts/cron_runner.py 调度 → 被 Go scheduler 替代
- usersData.json / config.json / webui_settings.json 存储 → 被 SQLite + settings 表替代
- webui/static/multiPagePlugins/（22 个注册/邮箱自动化插件）→ 与火花功能无关的辅助资产，默认弃置
- Mihomo 代理容器 → 主项目本就直连可用；仅服务器网络环境特殊时保留

### 2.2 帧藏侧可供融合复用的基建

| 基建 | 融合时的用途 |
|---|---|
| sidecar.Manager 模式（按需拉起/token 校验/健康检查/空闲退出） | 方案 B 直接套用到 spark 引擎的进程管理 |
| Provider 抽象 + Resolver | 证明本项目已有"数据源可插拔"的成熟先例，spark 引擎照此模式接入 |
| internal/scheduler（tick + 抖动） | 替代 sparkflow 的 cron 容器，驱动火花发送窗口 |
| events.SSE 总线 | 火花任务进度实时推送到 React 页（替代 sparkflow 的控制台轮询） |
| settings.Store + SettingsPage 分组卡片 | 火花配置纳入统一设置页 |
| auth 单管理员体系 | 替代 sparkflow 多用户（个人场景够用） |
| Docker 三阶段构建 + 国内镜像加速经验 | sparkflow 镜像同样需要加速（其 compose 已内置华为云 playwright 镜像源） |

### 2.3 冲突点清单

| 冲突 | 严重度 | 处置 |
|---|---|---|
| 端口 8787 双方默认相同 | 高 | sparkflow 改 8790（web）/8791（noVNC） |
| 两套独立认证（各自登录页/会话） | 中 | 方案 A 接受双登录；方案 B 统一为帧藏认证 |
| 数据层（SQLite vs JSON 文件） | 中 | 方案 A 各管各的；方案 B 时 spark 数据入库 |
| 调度重复（Go scheduler vs cron 容器） | 低 | 方案 A 保留 sparkflow 自带；方案 B 收编 |
| 运行时资源（sparkflow Playwright+VNC 常驻较重） | 中 | compose profile 按需启停；login-desktop 已有 CPU/内存限制 |
| 前端栈不一致（React vs Jinja2+原生 JS） | 低（A）/高（B） | A 不处理；B 重写页面 |
| docker.sock 挂载（sparkflow web 容器） | 中（安全面） | 新版 scheduler 已不依赖，可移除该挂载 |
| Cookie/登录态敏感性（sparkflow 持有完整账号凭据） | 高 | state/ 目录权限控制、不入 git、不暴露公网 |

### 2.4 License 合规红线（必读）

- sparkflow 自有代码为 **PolyForm Noncommercial 1.0.0**：允许个人非商业使用/修改/再分发，但**禁止任何商业用途**，且再分发需保留许可与声明。
- 本仓库为 **MIT** 且已推送至公开 GitHub（1wu-davy-2/douyin）。
- 推论：
  1. **不能把 sparkflow 源码以 MIT 名义合入本仓库公开分发**（许可不兼容）。
  2. 当前 .gitignore 已排除 `douyin-sparkflow/`（第 39 行）——**维持现状，外部参考项目不提交**，这是最干净的边界。
  3. 方案 B 若"参考行为规格、不复制代码"地重写核心逻辑，新代码归本仓库 MIT 所有，无传染；若直接搬运/改写 sparkflow 源码片段，该部分文件须保留 PolyForm 声明并单独标注，且整仓不得用于商业用途。
  4. 两方案都要求：仅操作本人拥有/已授权的账号，注意自动化发送的平台风控风险（上游 README 明确警告可能导致封号）。

---

## 3. 融合路径（三选一 + 演进推荐）

### 方案 A：并行双栈（保留两套独立页面）—— 推荐起步

**思路**：两套系统零代码融合，只做"部署编排统一 + 入口互通"。sparkflow 保持原样（独立目录、独立容器组、自带 UI），帧藏作为主入口提供跳转。

```
浏览器
 ├─ http://host:8787  帧藏（作品库/下载/监控/设置）
 └─ http://host:8790  sparkflow 控制台（总览/发送/记录）
                      └─ :8791 noVNC 扫码（SSH 隧道访问）
服务器：一个 compose 项目编两套服务（或两个 compose 文件共用网络）
```

- 改动点：sparkflow `.env` 端口改 8790/8791；帧藏前端"设置"或侧边栏加一个"火花维护"外链（新标签页打开）；可选——帧藏侧边栏/README 登记入口。
- 优点：1–2 天可上线；上游 sparkflow 更新可直接 `git pull` 跟进（对抖音页面改版敏感的 friends.py 尤其重要）；许可边界天然清晰；风险最低。
- 缺点：双登录、双 UI 风格、双数据目录；运维两套；Playwright 镜像 2GB+ 占磁盘。

### 方案 B：深度整合（spark 引擎侧车化 + React 统一页面）—— 已确认为目标方向（2026-09-14 用户决策）

**思路**：复刻本项目已有的 F2 侧车架构——把 sparkflow 的 core 打包成"火花引擎侧车"（纯内网 FastAPI 微服务），Go 主后端成为唯一入口与状态持有者，前端长出统一的"火花"页面。

```
React SPA（新增 /spark 路由：概览/账号/控制台/记录/设置）
   │ HTTP/SSE
Go 主后端 :8787（唯一入口）
   ├─ /api/spark/*        新增路由组（账号/好友/发送/记录/配置）
   ├─ internal/spark       新增包：SparkManager（仿 sidecar.Manager：
   │                       按需拉起、token、健康检查、空闲退出）
   ├─ SQLite 新表：spark_accounts / spark_friends / spark_send_records / spark_config
   └─ scheduler 扩展：火花发送窗口任务（替代 cron 容器）
        │ HTTP (127.0.0.1:18788 + token)
        ▼
spark-engine 侧车（Python/Playwright，剥离 webui）
   ├─ core/browser+friends+login+tasks（保留 A/B 级模块）
   ├─ login_desktop_server（登录桌面，整体保留）
   └─ Mihomo 可选（直连优先策略保留）
```

- 关键设计决策：
  1. **引擎无状态化**：sparkflow 的 JSON 存储改为侧车不落盘、状态回报给 Go 入库（避免双数据源）。
  2. **浏览器 Profile 目录仍归侧车管**（挂载卷），这是 Chromium 的事，SQLite 管不了。
  3. **扫码登录链路**：Go 反代 `/api/spark/login-desktop/*` 到侧车的 noVNC，React 页面内嵌 QR 状态轮询（上游已有 `/login-desktop/qr` 端点可复用）。
  4. **发送确认语义**（send_state 的"强确认"）必须完整迁移，这是上游踩坑最多的部分，规格照抄、代码参考重写。
  5. **统一登录态（2026-09-14 新增）**：扫码登录成功后，引擎导出 `.douyin.com` 域 Cookie，Go 写入 `data/.cookie` —— 帧藏 F2 侧车设计上**每次请求热加载该文件**，写入即生效；归档扫描从此免手动粘贴 Cookie，并可借持久化 Profile 定期重导出刷新。前置依赖 M0 验证（§5.2）。
  6. **tasks.py 拆分原则**：账号遍历 / 发送窗口 / 重试冷却等**编排逻辑移到 Go**（复用 scheduler + 事件总线）；Python 引擎只保留"单账号：刷好友 → 逐目标发送 → 强确认"的**浏览器动作闭环**。这样 2833 行中真正需要搬运/改写的只有发送+确认核心。
- 优点：单入口单认证、统一 UI/数据/SSE 进度、复用全部帧藏基建；个人使用下许可风险可控（参考重写部分 MIT）。
- 缺点：工作量约 2–4 周业余时间（见 §5.2）；脱离上游后 friends.py 的页面适配需自己跟进（可用"保留上游 git remote、定期比对新版"缓解）；license 上搬运与重写需严格分界。

### 方案 C：统一入口反代（A 的增强补丁，可单独实施）

**思路**：方案 A 基础上，Go 主后端加 `httputil.ReverseProxy`，把 `/sparkflow/*` 反代到 sparkflow web 容器（同 compose 网络内 `http://sparkflow-web:8790`）。
- 效果：单域名/单端口暴露两套页面；可在 Go 层加统一认证护栏（替代/前置 sparkflow 登录）；noVNC WebSocket 反代也可由 Go 承接。
- 代价：路径前缀重写（FastAPI 应用挂在子路径需要处理静态资源/表单 action，**上游未内置 base-path 支持，改造有坑**）；CSP/Cookie SameSite 需要调。
- 定位：不作为独立目标，而是方案 A 完成后的可选增强。

### 对比矩阵与推荐

| 维度 | A 并行双栈 | B 深度整合 | C 反代入口 |
|---|---|---|---|
| 工作量 | 1–2 天 | 2–4 周 | +2–3 天（在 A 之上） |
| 用户体验 | 双登录/双 UI | 完全统一 | 单入口、双 UI |
| 上游跟进能力 | 直接 pull | 需手动比对 | 随 A |
| 维护面 | 两套 | 一套 | 两套 |
| 数据一致性 | 各自独立 | SQLite 统一 | 各自独立 |
| 许可风险 | 最低（隔离） | 需分界管理 | 最低 |
| 资源占用 | 高（常驻） | 中（侧车按需拉起） | 高 |

**推荐**：**A 起步 → 用 1–2 个月 → 若火花功能成为日常高频使用且双登录确实烦人，再投入 B**。C 视公网暴露需求决定是否加做。判定信号：发送成功率是否稳定（上游 friends.py 对页面改版敏感）、是否需要把火花记录与归档数据联动（如"给作品收藏的博主发消息"这类跨功能，只有 B 能做）。

> **更新（2026-09-14）**：经逐点评估，确认直接以方案 B 为目标方向——前端 React 重写并入帧藏 / 共用 SQLite（Go 单写者）/ 帧藏认证为主 / 仅复用引擎逻辑（tasks.py 按第 6 条拆分）。方案 A 降级为迁移期的并行与回退通道；**先做 M0（统一登录态验证）再全面开工**。

---

## 4. 目录与依赖调整建议

### 4.1 方案 A（阶段一落地形态）

```
douyin/
├─ backend/  frontend/  sidecar/  scripts/  docs/     # 本仓库，不动
├─ docker-compose.yml                                  # 帧藏编排（不动）
├─ docker-compose.spark.yml                            # 新增：sparkflow 编排（从上游复制改造）
│                                                       #   端口 8790/8791，可选 profile "spark"
├─ .env.spark                                          # 新增：sparkflow 环境变量（端口/资源限制）
└─ douyin-sparkflow/                                   # 上游克隆（保持 gitignore，不提交）
    └─（上游原样，仅本地 .env 与 deploy 脚本参数改动）
```

依赖与端口规划：
| 项 | 值 | 说明 |
|---|---|---|
| sparkflow web | 8790 | 原 8787，避免与帧藏冲突 |
| noVNC 登录桌面 | 127.0.0.1:8791 | 原 8788，保持仅本地/SSH 隧道 |
| Mihomo | 127.0.0.1:7890/9090 | 不动（不与帧藏冲突） |
| 镜像 | douyin-sparkflow:local | 基于 playwright-python 镜像，磁盘 ~2GB+，构建走华为云源 |
| 网络 | 独立 compose 网络 | 与帧藏网络隔离，避免容器名冲突（上游容器名 douyin-web 与本仓库无冲突但语义易混） |

### 4.2 方案 B（阶段二目标形态）

```
douyin/
├─ backend/internal/
│  ├─ spark/                 # 新增：SparkManager（拉起/健康/空闲退出）+ API 处理器
│  └─ db/migrate.go          # 扩表：spark_accounts/spark_friends/spark_send_records/spark_config
├─ frontend/src/pages/spark/ # 新增：overview.tsx / accounts.tsx / console.tsx / records.tsx / settings.tsx
├─ spark-engine/             # 新增目录（取代 vendor 上游）：
│  ├─ engine/                #   由上游 core/* 参考改写：browser/friends/login/tasks/msg/send_state
│  ├─ login_desktop/         #   登录桌面（上游 login_desktop_server 平移）
│  ├─ requirements.txt       #   fastapi/playwright 等（收敛上游双 requirements）
│  └─ Dockerfile.engine       #   基于 playwright-python 镜像（独立于主镜像，按需构建）
├─ docker-compose.yml        # 扩展：spark-engine 服务（127.0.0.1:18788）+ 可选 login-desktop
└─ Dockerfile                # 主镜像不变（spark 引擎独立镜像，避免主镜像膨胀 2GB）
```

依赖调整要点：
1. **主镜像不吸收 Playwright**：保持 python:3.12-slim 运行时轻量，spark-engine 独立镜像、独立生命周期（按需启动）。
2. Go 侧新增依赖为零（ReverseProxy/SSE/SQLite 复用现有）。
3. spark-engine 依赖收敛：合并上游 requirements.txt + requirements-web.txt，剔除 qrcode/rich 等仅 CLI 用的包。
4. `protocol_sender.mjs` 需要 Node 运行时——主镜像已有 node 二进制（侧车同款做法），spark-engine 镜像亦内置 node22。
5. 数据库迁移遵循现有 db/migrate.go 模式，火花表与归档表同库不同前缀，事务边界分离。

---

## 5. 分步实施步骤

### 5.1 阶段一：方案 A（并行双栈上线）

**Step 1 — 端口与配置改造（0.5h）**
- `douyin-sparkflow/.env`（本地）：`WEB_PORT=8790`、`LOGIN_DESKTOP_WEB_PORT=8791`
- 帧藏不动；确认两者 `PROXY_*` 端口无冲突（保持 127.0.0.1 绑定）

**Step 2 — 部署编排统一（1h）**
- 服务器 `/opt/douyin` 下克隆上游到 `/opt/douyin-sparkflow`（与帧藏并列，或仓库内 gitignored 目录）
- 复制上游 docker-compose.yml 为 `docker-compose.spark.yml`，按 Step 1 调整端口；评估移除 web 容器的 `/var/run/docker.sock` 挂载（新版 scheduler 不再需要）
- 首次启动前执行上游 `deploy/install-local.sh`（生成 proxy/config.yaml，避免 Docker 把缺失文件创建成目录）

**Step 3 — 服务验证（0.5h）**
- `http://服务器:8790` 打开 sparkflow 控制台，完成 bootstrap 管理员初始化
- 验证 noVNC：`ssh -L 8791:127.0.0.1:8791` 后打开 `http://127.0.0.1:8791/vnc.html?...`，确认能扫码
- 验证 scheduler 容器按 `DEFAULT_SCHEDULE=10:00-18:00/20m` 生成发送任务

**Step 4 — 帧藏入口互通（1h）**
- 前端 layout.tsx 侧边栏（或设置页）加"火花维护"外链：`target=_blank` 指向 `:8790`（URL 做成设置项，默认 8790）
- README 补一节"联动工具"说明端口与 SSH 隧道访问法

**Step 5 — 安全加固（0.5h）**
- 公网 VM：8790 建议不直接暴露（参照帧藏 8787 的教训，用安全组限制来源 IP 或套反代+认证）
- `state/`（浏览器 Profile/Cookie）目录权限确认；`.env` 不入 git

**Step 6 — 验收与记录（0.5h）**
- 验收清单：两服务并存互不干扰（同时 `docker stats` 观察内存）；sparkflow 完成 1 次真实账号登录 + 1 次手动发送（`/ops/run-now`）；帧藏扫描/下载回归正常
- 更新 docs/PROGRESS.md 与本计划的状态标记

### 5.2 阶段二：方案 B（深度整合，按里程碑推进）

**M0 — 统一登录态验证 spike（~0.5–1 天，先行）**
0. 本地跑通：sparkflow 登录桌面扫码 → 引擎导出 `.douyin.com` Cookie（若缺 `ttwid` 等字段，先用 Playwright 访问一次 `www.douyin.com` 补齐）→ 写入帧藏 `data/.cookie` → 触发一次真实扫描。通过 = 方案 B 最大不确定项解除；失败 = 归档退回手动贴 Cookie，其余融合不受影响。

**M1 — 引擎侧车化（核心，~1 周）**
1. 新建 `spark-engine/`，从上游平移 A/B 级模块，剥离 webui/JSON 存储；**tasks.py 拆分原则：账号遍历 / 发送窗口 / 重试冷却等编排逻辑移到 Go，Python 只保留"单账号：刷好友 → 逐目标发送 → 强确认"的浏览器动作闭环**
2. 写薄 FastAPI 壳（仅监听 127.0.0.1:18788 + token，仿 F2 侧车契约）：`POST /run`（执行一轮发送，传配置/账号）、`GET /friends?account=` 、`POST /login/*`（对接登录桌面）、`GET /health`
3. 任务内状态改为返回值/回调上报（供 Go 入库），浏览器 Profile 继续走挂载卷
4. 单测：send_state 语义、账号遍历窗口逻辑（参考上游 tests/ 目录的用例）

**M2 — Go 接入层（~4 天）**
5. `internal/spark/SparkManager`：按需拉起引擎、token、健康检查、空闲退出（对照 internal/sidecar/manager.go 实现风格）
6. DB 迁移：spark_accounts / spark_friends / spark_send_records / spark_config 四表
7. `/api/spark/*` 路由：账号 CRUD、好友刷新、发送触发（run-now/run-failed/run-unsent）、记录分页、配置读写、login-desktop 反代
8. scheduler 扩展：发送窗口 tick（复用订阅调度框架）

**M3 — React 页面（~1 周）**
9. `/spark` 路由 + 侧边栏"火花"入口（Flame 图标）
10. 页面：概览（账号状态/今日进度）、账号管理（列表/启停/好友）、发送控制台（SSE 实时日志）、发送记录（分页/重试）、设置（模板/窗口/限速——并入统一设置风格）
11. 扫码登录组件：轮询 QR 状态 + 内嵌 noVNC iframe（远程场景）

**M4 — 部署与联调（~3 天）**
12. Dockerfile.engine（playwright-python + node22）；主 docker-compose.yml 增 spark-engine 服务（127.0.0.1:18788，profile 按需）
13. 端到端联调：登录 → 刷好友 → 配模板 → 定时发送 → 记录入库 → SSE 推送
14. 服务器灰度：保留方案 A 的 8790 入口作为回退，稳定一周后下线

**M5 — 收尾（~2 天）**
15. 文档：docs/api.md 补 spark 契约；README 架构图更新；PROGRESS.md 记录
16. 测试补齐：Go API 契约测试 + 引擎单测纳入 CI（`go test ./...`）
17. 上游同步策略：保留 douyin-sparkflow 克隆目录作只读参考，每月 diff 上游 friends.py/tasks.py 变更

---

## 6. 风险与注意事项

| 风险 | 影响 | 缓解 |
|---|---|---|
| 自动发送的平台风控（封号/限制） | 高 | 保留上游限速/窗口/变体模板默认值；仅操作本人账号；不做规模化 |
| 抖音页面改版导致 friends.py 选择器失效 | 高 | 方案 A 可直接跟上游；方案 B 建立每月 diff 上游机制 |
| PolyForm Noncommercial 许可 | 中 | sparkflow 目录保持 gitignore 不入库；方案 B 搬运代码处逐文件标注许可；整仓避免商业用途 |
| Playwright 镜像体积与资源（2GB+/1.2GB 内存上限） | 中 | compose profile 按需启动；login-desktop 已限 CPU 0.8/内存 1200m/PID 256 |
| 公网暴露 8790/noVNC | 中 | 默认仅 loopback + SSH 隧道；公网必须反代+认证+HTTPS |
| 双系统数据无法联动（仅方案 A） | 低 | 记录在案；联动需求出现即是启动方案 B 的信号 |
| docker.sock 挂载面 | 低 | 移除（新版 scheduler 不依赖） |

---

## 附：本计划的决策状态

- [x] 两项目对比与可复用模块识别（本文档 §1–§2）
- [x] 融合路径三方案与推荐（§3）
- [ ] 阶段一（方案 A）实施 —— 降级为迁移期并行 / 回退通道，按需执行
- [x] 阶段二（方案 B）方向确认（2026-09-14）—— 前端 React 重写并入帧藏 / 共用 SQLite / 帧藏认证为主 / 仅复用引擎逻辑；扫码登录 Cookie 反哺归档列为 M0 先行验证项
- [ ] 阶段二（方案 B）实施 —— M0 验证通过后启动
