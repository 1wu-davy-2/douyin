# 重构进度看板

> 自动化改造进行中。计划全文见会话;API 契约见 `docs/api.md`(冻结)。

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 脚手架 + 冻结契约 + README | ✅ 17ec5aa |
| ① | Go 基础框架(config/db/auth/events/sidecar/provider/settings/SSE/静态) | ✅ 92c6b03 |
| ② | Python F2 侧车(profile/posts/work + mock,严格错误语义) | ✅ 9d1ced3 |
| ③ | React 前端全部页面(构建零错误,mock 端到端通过) | ✅ 9e5cbaa |
| ④ | Go 扫描器 + 订阅调度(测试全绿,mock 502 作品端到端通过) | ✅ 92695cf |
| ⑤ | Go 下载器 + 任务 REST + SSE(14 行为测试全绿,Range 206 验证) | ✅ 540ead0 |
| 6 | 主线联调:浏览器 E2E 全流程通过(初始化/扫描502对账/分页/下载/SSE进度/播放器Range/订阅调度/设置);修复 mock CDN 真实 MP4 + 播放器取最新资产 | ✅ ccf982b |
| 7 | import-legacy 旧库迁移工具(1734 作品/2299 资产零拷贝接入,junction,幂等,206 播放验证) | ✅ 72a5d7e |
| 8 | 真实 cookie 实测:@吖头与妈妈🌹 101/101 作品扫描完整性 0 缺口、全部下载成功;实测修复 safeName 中文截断 panic、CDN 403(浏览器头 + 多候选地址 + 任务节流)、侧车排他绑定 | ✅ 33ac171 |
| 9 | 图集/图片作品全链:识别(type)→ 下载(按作品名聚合目录 `标题/0001.jpg...` + 动图 live 片段)→ 播放器幻灯片(每张 5 秒自动切换);真实数据验证:酒梨窝涡 30 个图集已识别入队;博主卡片重扫按钮;扫描并发 1-5 可配(设置页);侧车 /health token 校验 + 自闲置退出(孤儿侧车防御) | ✅ 34f3199 |

## 全部完成。日常使用

```powershell
powershell -File scripts\build.ps1     # 构建单二进制(已构建: backend\douyin-server.exe)
.\backend\douyin-server.exe            # http://127.0.0.1:8787
```

- 首次使用:网页创建管理员 → 设置页粘贴抖音 Cookie(旧项目 .env 的 F2_COOKIE 已迁移)→ 运行模式选 sidecar → 作品库添加博主
- 旧数据已通过 import-legacy 接入(7 博主/1734 作品/928 已下载,`data\downloads\legacy` 为指向旧下载根的 junction,零拷贝)
- 迁移命令:`.\backend\douyin-server.exe -mode import-legacy -legacy-db "E:\home\douyin-archive\data\app.db"`

## 旧版四大问题的修复落点

1. **下载页卡顿** → SSE 增量推送替代 2s 全量轮询;服务端分页;TanStack Query 精准失效(阶段③⑤)
2. **扫描漏扫(502→400+)** → 侧车不吞错(空响应=502 显式失败);Go 扫描器坏页重试+游标回退+完整性对账+自动补扫(阶段②④)
3. **启动内存** → Go 常驻(~20-40MB)替代 Python 双进程;侧车按需启停;无 raw_metadata 死重(阶段①②)
4. **任务管理** → worker 池并发真实生效;可取消进行中任务;批量操作/历史分页/清空(阶段⑤)
