/**
 * 统一 queryKey 约定(与开发规约一致):
 *   ["creators"] / ["creator", id] / ["works", creatorId, filters]
 *   ["downloads", filters] / ["downloadsSummary"] / ["subscriptions"] / ["settings"] / ["auth"]
 * SSE 事件按前缀 invalidate 或精准 patch,页面不得自造轮询定时器。
 */
import { keepPreviousData, useInfiniteQuery, useQuery } from "@tanstack/react-query";
import * as ep from "./endpoints";
import type { WorksListParams } from "./endpoints";
import type { DownloadsQuery } from "./endpoints";
import type { CursorPage, Job } from "./types";

export const qk = {
  auth: ["auth"] as const,
  health: ["health"] as const,
  creators: ["creators"] as const,
  creator: (id: number) => ["creator", id] as const,
  works: (creatorId: number, filters: WorksListParams) => ["works", creatorId, filters] as const,
  collections: (creatorId: number) => ["collections", creatorId] as const,
  downloads: (filters: DownloadsQuery) => ["downloads", filters] as const,
  downloadsAll: ["downloads"] as const,
  downloadsSummary: ["downloadsSummary"] as const,
  subscriptions: ["subscriptions"] as const,
  /** 前缀含 ["subscriptions"],订阅列表 invalidate 时打开中的抽屉一并刷新。 */
  subscriptionNewWorks: (id: number) => ["subscriptions", id, "new-works"] as const,
  settings: ["settings"] as const,
  assets: (workId: number) => ["assets", workId] as const,
};

// ---------- 认证 / 健康 ----------
export function useAuthStatus() {
  return useQuery({
    queryKey: qk.auth,
    queryFn: ep.getAuthStatus,
    retry: false,
    staleTime: 30_000,
  });
}

export function useHealth() {
  return useQuery({ queryKey: qk.health, queryFn: ep.getHealth, staleTime: 60_000, retry: 1 });
}

// ---------- 博主 / 作品 / 合集 ----------
export function useCreators() {
  return useQuery({ queryKey: qk.creators, queryFn: ep.listCreators });
}

export function useCreator(id: number | null) {
  return useQuery({
    queryKey: qk.creator(id ?? 0),
    queryFn: () => ep.getCreator(id as number),
    enabled: id !== null,
  });
}

export function useCollections(creatorId: number | null) {
  return useQuery({
    queryKey: qk.collections(creatorId ?? 0),
    queryFn: () => ep.listCollections(creatorId as number),
    enabled: creatorId !== null,
  });
}

export function useWorks(creatorId: number | null, params: WorksListParams) {
  return useQuery({
    queryKey: qk.works(creatorId ?? 0, params),
    queryFn: () => ep.listWorks(creatorId as number, params),
    enabled: creatorId !== null,
    placeholderData: keepPreviousData,
  });
}

// ---------- 下载(游标分页,SSE 精准 patch) ----------
export function useDownloads(filters: DownloadsQuery) {
  return useInfiniteQuery({
    queryKey: qk.downloads(filters),
    queryFn: ({ pageParam }) => ep.listDownloads({ ...filters, cursor: pageParam }),
    initialPageParam: null as number | null,
    getNextPageParam: (last: CursorPage<Job>) => last.next_cursor,
  });
}

export function useDownloadsSummary() {
  return useQuery({
    queryKey: qk.downloadsSummary,
    queryFn: ep.downloadsSummary,
    // 进度事件不触发 summary,状态事件会 invalidate;这里兜底窗口同步
    refetchInterval: false,
  });
}

// ---------- 订阅 / 设置 ----------
export function useSubscriptions() {
  return useQuery({
    queryKey: qk.subscriptions,
    queryFn: ep.listSubscriptions,
    // 监控统计(新收录/已下载)无 SSE 事件,30s 轮询兜底;页面不可见时 TanStack 自动暂停
    refetchInterval: 30_000,
  });
}

/** 监控期间新增作品明细(抽屉);id=null 时禁用(抽屉未打开)。 */
export function useSubscriptionNewWorks(id: number | null) {
  return useQuery({
    queryKey: qk.subscriptionNewWorks(id ?? 0),
    queryFn: () => ep.getSubscriptionNewWorks(id as number),
    enabled: id !== null,
  });
}

export function useSettings() {
  return useQuery({ queryKey: qk.settings, queryFn: ep.getSettings });
}

// ---------- 播放器资产 ----------
export function useWorkAssets(workId: number | null) {
  return useQuery({
    queryKey: qk.assets(workId ?? 0),
    queryFn: () => ep.listAssets(workId as number),
    enabled: workId !== null,
    staleTime: 5 * 60_000,
    retry: 1,
  });
}
