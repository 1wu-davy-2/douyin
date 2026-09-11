/**
 * SSE 实时事件流 hook(替代轮询)。
 *
 * - EventSource("/api/events", { withCredentials: true });断线指数退避重连(1s→30s 封顶)
 * - download.progress:精准 patch 查询缓存中对应 job(不触发请求)
 * - download.status:invalidate ["downloads"] 全部 key 与 ["downloadsSummary"](并同步作品/博主侧的 dl_status)
 * - scan.progress / scan.done:写入 scan-store(Library 卡片进度条)并 invalidate ["creator", id] 与 ["creators"]
 * - provider.status:轻量 patch ["health"] 缓存
 * - mock 模式:VITE_MOCK=1 时 dev server 的 /api/events 由 mock.ts 提供假事件流,本 hook 无感
 */
import { useEffect, useRef } from "react";
import { useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { toast } from "sonner";
import type { CursorPage, Health, Job } from "./types";
import { qk } from "./queries";
import { applyScanDone, applyScanProgress } from "../lib/scan-store";

const MAX_BACKOFF_MS = 30_000;

function patchJobInCache(
  client: ReturnType<typeof useQueryClient>,
  jobId: number,
  patch: Partial<Job>,
): void {
  client.setQueriesData<InfiniteData<CursorPage<Job>>>({ queryKey: qk.downloadsAll }, (data) => {
    if (!data) return data;
    let changed = false;
    const pages = data.pages.map((page) => {
      let pageChanged = false;
      const items = page.items.map((job) => {
        if (job.id !== jobId) return job;
        pageChanged = true;
        return { ...job, ...patch };
      });
      if (!pageChanged) return page;
      changed = true;
      return { ...page, items };
    });
    return changed ? { ...data, pages } : data;
  });
}

export function useEvents(enabled: boolean): void {
  const client = useQueryClient();
  const backoffRef = useRef(0);

  useEffect(() => {
    if (!enabled) return;

    let source: EventSource | null = null;
    let reconnectTimer = 0;
    let disposed = false;

    const connect = () => {
      if (disposed) return;
      source = new EventSource("/api/events", { withCredentials: true });

      source.onopen = () => {
        backoffRef.current = 0;
      };

      source.onerror = () => {
        // 浏览器原生重连不可控,这里统一接管:关闭后按指数退避重连
        source?.close();
        source = null;
        if (disposed) return;
        const delay = Math.min(MAX_BACKOFF_MS, 1000 * 2 ** backoffRef.current);
        backoffRef.current = Math.min(backoffRef.current + 1, 6);
        reconnectTimer = window.setTimeout(connect, delay);
      };

      source.addEventListener("download.progress", (e) => {
        const data = JSON.parse((e as MessageEvent<string>).data) as {
          job_id: number;
          downloaded_bytes: number;
          total_bytes: number;
          speed_bps: number;
        };
        patchJobInCache(client, data.job_id, {
          downloaded_bytes: data.downloaded_bytes,
          total_bytes: data.total_bytes,
          speed_bps: data.speed_bps,
        });
      });

      source.addEventListener("download.status", (e) => {
        const data = JSON.parse((e as MessageEvent<string>).data) as {
          job_id: number;
          status: Job["status"];
          error: string | null;
          work_id: number;
        };
        // 进度缓存里的任务状态也要翻面(进度条行可能停在 downloading)
        patchJobInCache(client, data.job_id, { status: data.status, error: data.error, speed_bps: 0 });
        void client.invalidateQueries({ queryKey: qk.downloadsAll });
        void client.invalidateQueries({ queryKey: qk.downloadsSummary });
        // dl_status 由最新任务推导,同步作品与博主列表
        void client.invalidateQueries({ queryKey: ["works"] });
        void client.invalidateQueries({ queryKey: ["creators"] });
        void client.invalidateQueries({ queryKey: ["creator"] });
      });

      source.addEventListener("scan.progress", (e) => {
        const data = JSON.parse((e as MessageEvent<string>).data) as Parameters<typeof applyScanProgress>[0];
        applyScanProgress(data);
        void client.invalidateQueries({ queryKey: qk.creator(data.creator_id) });
        void client.invalidateQueries({ queryKey: qk.creators });
        void client.invalidateQueries({ queryKey: ["works"] });
      });

      source.addEventListener("scan.done", (e) => {
        const data = JSON.parse((e as MessageEvent<string>).data) as Parameters<typeof applyScanDone>[0];
        applyScanDone(data);
        void client.invalidateQueries({ queryKey: qk.creator(data.creator_id) });
        void client.invalidateQueries({ queryKey: qk.creators });
        void client.invalidateQueries({ queryKey: ["works"] });
        if (data.status === "failed") {
          toast.error("扫描失败", { description: data.last_error ?? "请检查 Cookie 或网络后重试" });
        } else {
          const label = data.status === "partial" ? "扫描部分完成" : "扫描完成";
          toast.success(label, {
            description: `新增 ${data.new_count} 个作品 · 共 ${data.pages} 页 · 完整度 ${data.completeness}%`,
          });
        }
      });

      source.addEventListener("provider.status", (e) => {
        const data = JSON.parse((e as MessageEvent<string>).data) as {
          sidecar: Health["sidecar"];
          risk_paused: boolean;
          paused_until: string | null;
        };
        client.setQueryData<Health>(qk.health, (prev) =>
          prev ? { ...prev, sidecar: data.sidecar } : prev,
        );
      });
    };

    connect();

    // 网络恢复时立即重连
    const onOnline = () => {
      if (source || disposed) return;
      window.clearTimeout(reconnectTimer);
      backoffRef.current = 0;
      connect();
    };
    window.addEventListener("online", onOnline);

    return () => {
      disposed = true;
      window.clearTimeout(reconnectTimer);
      window.removeEventListener("online", onOnline);
      source?.close();
    };
  }, [client, enabled]);
}
