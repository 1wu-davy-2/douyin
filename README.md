# Douyin Archive v2

本地运行的抖音公开作品订阅与增量归档工具。输入博主主页链接后扫描作品与合集,按账号 / 合集 / 单作品选择下载,支持定时监控新作品;内置播放器直接观看已下载的视频与图集。

> 仅用于归档本人拥有或已获授权下载的公开内容。请遵守平台规则、创作者版权与所在地法律法规。
>
> 旧版(Vue + Python FastAPI)位于 `E:\home\douyin-archive`,本项目为其全量重写,旧项目仅作参考。

## 功能

- **扫描**:全量 / 增量扫描博主主页与合集;坏页重试、游标回退、完整性对账,缺口超阈值自动补扫(修复旧版"主页 N 个只扫出 N-100"的漏扫)
- **作品类型**:视频与图集(图文)作品;图集下载为 `作品名/0001.jpg...` 聚合目录,含动图(实况)视频片段
- **下载**:Go worker 池真实并发(1-8 可配)、事件驱动派发、CDN 多候选地址按序尝试、请求节流防风控;进行中任务可取消,异常重启自动续跑
- **播放**:内置播放器,视频 Range 流式拖动;图集幻灯片模式每张 5 秒自动切换;连播
- **任务管理**:按状态筛选、游标分页历史、批量重试 / 取消 / 删除、清空已完成
- **作品库**:类型 / 下载状态 / 合集 / 关键词组合筛选,服务端分页,批量下载、删除与重新下载
- **监控订阅**:按间隔定时扫描并自动下载新作品,可设扫描并发(1-5);展示监控期间新增与已下载数
- **存储**:全局下载根目录 + 每博主独立路径覆盖;资产按绝对路径记录,搬迁不失效
- **通知**:SMTP 邮件(新作品 / 下载失败)

## 架构

```
React SPA ──HTTP/SSE──> Go 后端(单二进制,嵌入前端,常驻 ~30MB)──> SQLite + 磁盘
                              │ 按需拉起,空闲自动退出
                              ▼
                  Python 侧车(F2 签名,仅 3 个数据端点)──> 抖音 web API
Go 后端 ──直连 CDN 下载(不走 Python)
```

- **Go 主后端**:API、扫描器、下载 worker 池、SSE 推送、SQLite(modernc.org 纯 Go 驱动,免 CGO)
- **Python 侧车**:封装 F2 完成 a_bogus 签名与接口数据拉取;`/health` 带 token 校验、闲置自毁,孤儿进程不留隐患
- **媒体下载**由 Go 直连 CDN(Referer/UA/Cookie 头 + 多候选地址),不经过 Python

## 环境要求

- Windows 10/11(其他平台理论可用,未测试)
- Go 1.25+、Node.js 20+、Python 3.10+(侧车用,需可 `pip install f2`)
- Node 用于 F2 的 JS 签名

## 构建与运行

```powershell
# 1. 构建(前端产物嵌入 Go 二进制)
powershell -File scripts\build.ps1     # 产出 backend\douyin-server.exe

# 2. 运行
.\backend\douyin-server.exe            # http://127.0.0.1:8787
```

首次打开网页创建管理员账号;之后在 **设置 → Provider** 粘贴抖音网页版 Cookie(浏览器 F12 → Network → 任一 douyin.com 请求的 Cookie 请求头,完整复制),运行模式选 `sidecar`,即可真实扫描。

开发模式(mock 数据,无需 Cookie):

```powershell
powershell -File scripts\dev.ps1       # 后端 :8787 (DY_MOCK=1) + 前端 Vite :5173
```

Mock 模式内置假博主(502 个作品、含图集与动图)与本地假 CDN,可离线走通 扫描 → 下载 → 播放 全链路。

## 下载路径

- **全局**:设置 → 下载 → 下载根目录(留空 = `data\downloads`)
- **单博主**:作品库 → 选中博主 → 标题行路径旁 ✏️ → 设置独立路径,可勾选"同时移动已下载文件"
- 资产按绝对路径记录;历史文件不受路径修改影响;同盘移动为 rename,跨盘为复制+校验+删源

## 从旧版(v1 / Python 版)迁移

```powershell
.\backend\douyin-server.exe -mode import-legacy -legacy-db "E:\path\to\旧项目\data\app.db"
```

- 迁移 creators / collections / works / assets / 已完成任务;`raw_metadata` 巨型列不迁移
- 旧下载文件**零拷贝**接入:数据目录下创建 `downloads\legacy` 目录联接指向旧下载根
- 图文作品自动识别为图集(type=image),幂等可重复执行

## 配置

运行时设置(网页"设置"页)优先于环境变量;环境变量见 `backend/internal/config/config.go`,常用的:

| 变量 | 默认 | 说明 |
|---|---|---|
| `DY_PORT` | 8787 | HTTP 端口 |
| `DY_DATA_DIR` | ./data | 数据目录(数据库、下载、Cookie) |
| `DY_MOCK` | 0 | 1 = Mock 模式(内置假数据 + 假 CDN) |
| `DY_SIDECAR_PORT` | 18787 | 侧车端口 |
| `DY_SIDECAR_PYTHON` | 自动探测 | 侧车 Python 解释器 |
| `DY_SIDECAR_IDLE_TIMEOUT` | 10m | 侧车空闲退出时间 |

## 目录结构

```
douyin/
├─ backend/          Go 主后端(cmd/server、cmd/import-legacy、internal/*)
├─ sidecar/          Python 签名侧车(main.py + requirements.txt)
├─ frontend/         React SPA(Vite + Tailwind + Radix + TanStack Query)
├─ scripts/          dev.ps1 / build.ps1
└─ docs/             api.md(接口契约)、PROGRESS.md(开发进度)
```

## 测试

```powershell
cd backend
E:\path\to\go\bin\go.exe test ./... -count=1
```

覆盖:扫描器状态机(坏页重试 / 游标回退 / 增量早停 / 完整性对账)、下载器(并发 / 取消 / 恢复 / 节流 / 多候选)、图集聚合、迁移幂等、API 契约。

## 注意事项

- 抖音页面、接口参数与风控会变化;签名实现隔离在侧车,F2 更新后只需 `pip install -U f2`
- HTTP 200 但内容为空通常是 Cookie 失效或被风控;扫描结果会如实标记 `partial` 并给出缺口
- 不要把本工具直接暴露到公网;定位是本机单用户工具
- Cookie 保存在 `data/.cookie`,数据库在 `data/app.db`,均已被 git 忽略

## 第三方组件

- [F2](https://github.com/Johnserf-Seed/f2)(Apache-2.0)——抖音签名与接口
- mock 演示视频:Big Buck Bunny,(c) Blender Foundation,CC-BY 3.0,经 test-videos.co.uk 裁剪(`backend/internal/mockmedia/mockassets/`)

## 许可证

MIT
