# Linux / Docker 部署指南

## 兼容性说明

代码已兼容 Linux(为 Docker 部署准备):

- **路径**:全部使用 `filepath`(平台感知分隔符);数据库存正斜杠形式,回放/删除按平台解析;下载根目录校验接受 POSIX 绝对路径(`/mnt/...`)
- **大小写**:目录清理的前缀匹配仅在 Windows 上忽略大小写(ext4 严格区分)
- **侧车 Python**:自动探测 `sidecar/.venv/bin/python3`(POSIX)或 `Scripts/python.exe`(Windows);都没有时 POSIX 优先 `python3`;可用 `DY_SIDECAR_PYTHON` 显式指定
- **进程清理**:Windows 用 `taskkill /T /F`,POSIX 用 `kill`(进程组)
- **下载根目录**:支持 `DY_DOWNLOAD_ROOT` 环境变量作为默认(Docker 挂载场景最方便;运行时设置仍优先)
- SQLite 驱动为纯 Go(modernc.org/sqlite),CGO_ENABLED=0 静态编译,无 libc 依赖

## Docker 部署

```bash
# 构建并启动(三阶段构建:前端 → Go 静态编译 → python3+node 运行时)
docker compose up -d --build

# 查看日志
docker compose logs -f douyin
```

- 服务:http://<host>:8787 (首次打开创建管理员)
- 数据持久化:compose 默认挂载 `./data` → `/app/data`(数据库/下载/Cookie)
- Cookie 配置:网页"设置 → Provider"粘贴(容器内无需重启)
- 大体积下载建议挂载独立卷并在 compose 中设 `DY_DOWNLOAD_ROOT` 指向它

镜像内置 `python3 + node`(F2 签名需要),Go 主程序按需拉起/回收侧车,空闲不占资源。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `DY_PORT` | 8787 | HTTP 端口 |
| `DY_DATA_DIR` | /app/data | 数据目录 |
| `DY_DOWNLOAD_ROOT` | `<data>/downloads` | 默认下载根(env 层;网页设置的 download_root 优先) |
| `DY_SIDECAR_PYTHON` | 自动探测 | 容器内已设为 `python3` |
| `DY_MOCK` | 0 | 调试模式 |

## MinIO 同步(方案已评估,尚未实现)

设计已预留,见 `docs/MINIO_PLAN.md`。
