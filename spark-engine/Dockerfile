# syntax=docker/dockerfile:1

# 火花引擎(续火花):FastAPI + Playwright + Xvfb/noVNC 登录桌面,单容器全家桶。
# 国内网络友好:Playwright 基础镜像走华为云镜像仓,pip 走清华源,apt 走阿里云。

ARG PLAYWRIGHT_BASE_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/mcr.microsoft.com/playwright/python:v1.56.0-jammy
FROM ${PLAYWRIGHT_BASE_IMAGE}

WORKDIR /app

ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1 \
    DEBIAN_FRONTEND=noninteractive

COPY requirements.txt ./
RUN pip install --no-cache-dir -i https://pypi.tuna.tsinghua.edu.cn/simple -r requirements.txt

# 登录桌面全家桶:Xvfb(虚拟显示)+ fluxbox(窗口管理器)+ x11vnc(VNC 服务)
# + websockify/noVNC(浏览器远程桌面)。中文字体保证二维码页与滑块验证可读。
RUN sed -i 's/archive.ubuntu.com/mirrors.aliyun.com/g; s/security.ubuntu.com/mirrors.aliyun.com/g' /etc/apt/sources.list \
 && apt-get update && apt-get install -y --no-install-recommends \
      bash curl procps \
      fluxbox fonts-wqy-zenhei fonts-noto-cjk \
      novnc websockify x11vnc xvfb \
 && rm -rf /var/lib/apt/lists/*

COPY . .

ENV SPARK_ENGINE_PORT=18788 \
    SPARKFLOW_BROWSER_PROFILE_ROOT=/app/state/browser-profiles
VOLUME ["/app/state"]
EXPOSE 18788

# entrypoint:后台拉起登录桌面(Xvfb+x11vnc+websockify+login_desktop_server:18090),
# 前台 exec 引擎主进程(uvicorn:18788)。
CMD ["bash", "docker/entrypoint.sh"]
