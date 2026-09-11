# Douyin Archive (v2 · Go + React)

抖音公开作品订阅与增量归档工具(重构版)。Go 主后端 + React 前端 + 极薄 Python/F2 签名侧车。

> 旧版(Vue + Python FastAPI)位于 `E:\home\douyin-archive`,本项目为其全量重写,旧项目仅作参考。

## 架构

```
React SPA ──HTTP/SSE──> Go 后端(常驻,单二进制,嵌入前端) ──> SQLite(纯Go驱动) + 磁盘
                              │ 按需拉起,空闲自动退出
                              ▼
                     Python 侧车(F2 签名,仅3个数据端点) ──> 抖音 web API
Go 后端 ──直连 CDN 下载(带宽大头不经过 Python)
```

- **Go 常驻内存 ~20-40MB**;侧车只在扫描/取播放地址时运行,空闲 10 分钟自动退出
- 队列 = SQLite 持久化状态 + Go worker 池,事件驱动、可取消进行中的任务
- 扫描修复旧版漏扫:F2 吞错导致空页被当"到底"、游标回退只试一次、增量扫描永不回补 —— 全部重做(见 `docs/api.md` 顶部说明)

## 文档

- [API 契约](docs/api.md) — 前后端对接的唯一依据(含 SSE 事件格式、侧车契约、库表结构)

## 开发

环境要求:Go 1.27(`E:\tmp\tools\go`,未入 PATH)、Node 22、Python 3.12。**go.mod 在 backend/ 下,Go 命令须在 backend/ 目录执行**。

```powershell
# 一键开发(mock 后端 + Vite)
powershell -File scripts\dev.ps1

# 或手动:后端(开发模式,mock 数据,无需 cookie)
cd backend; $env:DY_MOCK=1; $env:DY_DATA_DIR="..\\data"; E:\tmp\tools\go\bin\go.exe run ./cmd/server

# 前端(dev server :5173,代理 /api -> :8787)
npm install --prefix frontend; npm run dev --prefix frontend

# 侧车自测
python sidecar/main.py --port 18787 --token dev --mock
```

## 生产运行

```powershell
powershell -File scripts\build.ps1        # 前端构建 -> 拷入 embed 目录 -> 单二进制
.\backend\douyin-server.exe               # http://127.0.0.1:8787
```

首次打开网页创建管理员;在"设置"粘贴抖音 Cookie 并把 provider_mode 切到 `sidecar` 即可真实扫描。

## 数据迁移

```powershell
.\douyin-server.exe -mode=import-legacy "E:\home\douyin-archive\data\app.db"
```
