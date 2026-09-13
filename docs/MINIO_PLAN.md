# MinIO 同步方案(评估稿 v1)

> 状态:**方案评估,未实现**。设置页先落配置项与依赖,上传逻辑后续补全。

## 目标

下载完成的视频/图集资产**同步上传**到 MinIO(本地 NAS / 对象存储),本地保留原文件;MinIO 作为备份或媒体库(可供其他工具如 Jellyfin 直接读)。

## 方案选型

| 方案 | 说明 | 取舍 |
|---|---|---|
| A. 下载完成后同步上传(推荐) | 下载器 succeeded 收尾钩子里异步上传,失败仅记日志/重试队列,不阻塞本地流程 | 首选:实现简单、零侵入、失败可独立重试 |
| B. 双写存储后端 | 旧版 v1 的 storage 抽象(local/minio 二选一) | 放弃:切换后历史资产读不到了,单机工具没必要 |
| C. 定时对账同步 | 定时扫描 assets 表,对比远端对象列表补差 | 作为 A 的兜底补充(低频跑),不单独用 |

**结论:A 为主、C 兜底**。assets 表已有绝对路径与 size_bytes,天然是同步清单。

## 配置项(设置页 → 存储)

```
minio_enabled        bool    总开关(默认关)
minio_endpoint       string  例如 127.0.0.1:9000(不含 scheme)
minio_bucket         string  例如 douyin
minio_access_key     string
minio_secret_key     string  (打码回显,同 SMTP password 模式)
minio_use_ssl        bool
minio_prefix         string  对象前缀,默认 "douyin/" → 对象键 douyin/{博主}/{singles|collections}/{作品}/{文件}
minio_concurrency    int     并发上传数(默认 2)
```

优先级:网页设置(runtime settings 表)> `.env`(Docker Secret 同理)。`docker-compose.yml` 已附可选的本地 MinIO 服务(注释态)。

## 上传语义

- **触发**:下载器 `succeeded` 落库后,投递异步任务(minio 队列,有界);图集=逐张图片+live 片段+cover+metadata
- **对象键**:`{prefix}{博主}/{相对结构}`,即与磁盘目录一致,便于直接挂给媒体服务器
- **幂等**:上传前 `StatObject`,size 一致则跳断点续传逻辑直接跳过;不一致则覆盖(PUT)
- **失败**:重试 3 次(指数退避);仍失败记入日志与设置页"同步状态"(failed 计数),提供"重试失败上传"按钮;**绝不影响本地下载的成功状态**
- **删除/移动联动**:作品删除时可选同步删除远端对象(默认**不删**,防止误删备份;设置项 `minio_sync_delete` 默认 false);move-downloads 不迁移远端对象(对象键含路径,重算即可)——方案细节:对象键以 item_id 为锚更稳?**决定:对象键用 `douyin/{sec_uid}/{item_id}/{序号文件名}`**,避免标题改名导致键漂移,媒体服务器侧靠 metadata.json 呈现标题

## 依赖与代码落点

- Go 依赖:`github.com/minio/minio-go/v7`(纯 Go,无 CGO,与现有构建兼容)
- `internal/settings`:新增 minio_* 字段(打码 secret_key)
- 新包 `internal/uploader`:MinIO client 封装 + 队列 + 重试 + "测试连接"(设置页按钮,同 SMTP test 模式)
- `internal/downloader`:succeeded 收尾处调用 `uploader.Enqueue(asset)`(接口注入,未启用时 no-op,测试可替换)
- `internal/api`:设置页读写 + `POST /api/settings/minio/test` + 同步状态查询(`GET /api/settings/minio/status`)
- 前端:设置页"对象存储(MinIO)"分组(端点/bucket/密钥/开关/测试连接/同步状态);不新增页面

## 工作量评估

- 设置+打码+测试连接:~0.5 天
- 上传队列+重试+状态:~1 天
- 下载钩子+图集多文件:~0.5 天
- 前端分组+状态展示:~0.5 天
- 合计 ~2.5 天(含测试);依赖引入 1 行 go.mod

## 风险

- 大文件上传占用带宽,与下载并发互相挤占 → 独立低并发(默认 2)+ 深夜可加
- MinIO 不可用时不能拖垮下载 → 队列有界丢弃+计数告警,主流程完全解耦
- 对象键含 sec_uid 不含中文标题,人工浏览体验差 → metadata.json 同步上传,媒体服务器/脚本可读
