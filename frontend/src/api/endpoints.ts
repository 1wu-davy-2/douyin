/**
 * 与 docs/api.md(冻结契约)一一对应的端点封装。
 * 只做 path 拼接与类型标注,不持有状态;状态全部走 TanStack Query。
 */
import { api } from "./client";
import type {
  Asset,
  AuthStatus,
  BatchActionResult,
  BatchIdsResult,
  Collection,
  CreateCreatorInput,
  CreateCreatorResult,
  CreateDownloadsResult,
  Creator,
  CreatorDetail,
  CreatorDownloadRootResult,
  CreatorPatch,
  CreatorPatchResult,
  CursorPage,
  DownloadsSummary,
  Health,
  Job,
  JobStatus,
  MoveDownloadsResult,
  NotificationTestResult,
  Page,
  Quality,
  QualityOption,
  RescanResult,
  Settings,
  SettingsPatch,
  Subscription,
  SubscriptionInput,
  SubscriptionNewWorks,
  SubscriptionPatch,
  Work,
  WorkDetail,
  WorkDlFilter,
  WorkSort,
  WorkTypeFilter,
  BatchDeleteWorksResult,
  WorksRedownloadResult,
  MinioStatus,
} from "./types";

// ---------- 认证 ----------
export const getAuthStatus = () => api<AuthStatus>("/api/auth/status");
export const login = (username: string, password: string) =>
  api<{ ok: boolean; username?: string }>("/api/auth/login", {
    method: "POST",
    json: { username, password },
  });
export const setup = (username: string, password: string) =>
  api<{ ok: boolean; username?: string }>("/api/auth/setup", {
    method: "POST",
    json: { username, password },
  });
export const logout = () => api<{ ok: boolean }>("/api/auth/logout", { method: "POST" });
export const changePassword = (oldPassword: string, newPassword: string) =>
  api<{ ok: boolean }>("/api/auth/password", {
    method: "POST",
    json: { old_password: oldPassword, new_password: newPassword },
  });

// ---------- 健康 ----------
export const getHealth = () => api<Health>("/api/health");

// ---------- 博主与作品 ----------
export const listCreators = () => api<Creator[]>("/api/creators");
/** 契约 v1.4:创建时可带可选参数(别名/分组/独立下载根/自动下载+画质);202 后 SSE 报扫描进度。 */
export const createCreator = (input: CreateCreatorInput) =>
  api<CreateCreatorResult>("/api/creators", { method: "POST", json: input });
export const getCreator = (id: number) => api<CreatorDetail>(`/api/creators/${id}`);
/** 契约 v1.4:改别名/分组;null 或空串 = 清除(别名恢复默认昵称,分组移出)。 */
export const patchCreator = (id: number, patch: CreatorPatch) =>
  api<CreatorPatchResult>(`/api/creators/${id}`, { method: "PATCH", json: patch });
export const deleteCreator = (id: number, deleteFiles = false) =>
  api<{ ok: boolean }>(`/api/creators/${id}${deleteFiles ? "?delete_files=true" : ""}`, { method: "DELETE" });
/** 契约 v1.3:设置博主独立下载根目录;path=null 表示清除,跟随全局。后端自动建目录。 */
export const setCreatorDownloadRoot = (id: number, path: string | null) =>
  api<CreatorDownloadRootResult>(`/api/creators/${id}/download-root`, { method: "PATCH", json: { path } });
/** 契约 v1.3:把该博主已下载文件整体搬到新根;存在 downloading 任务时后端 409。 */
export const moveCreatorDownloads = (id: number, targetRoot: string) =>
  api<MoveDownloadsResult>(`/api/creators/${id}/move-downloads`, {
    method: "POST",
    json: { target_root: targetRoot },
  });
export const batchDeleteWorks = (ids: number[]) =>
  api<BatchDeleteWorksResult>("/api/works/batch-delete", { method: "POST", json: { ids } });
export const testMinio = () =>
  api<{ ok: boolean; error?: string }>("/api/settings/minio/test", { method: "POST", json: {} });
export const minioStatus = () => api<MinioStatus>("/api/settings/minio/status");
export const worksRedownload = (ids: number[], quality?: string) =>
  api<WorksRedownloadResult>("/api/works/redownload", { method: "POST", json: { ids, quality } });
export const rescanCreator = (id: number, full = false) =>
  api<RescanResult>(`/api/creators/${id}/rescan`, { method: "POST", json: { full } });
export const listCollections = (creatorId: number) =>
  api<Collection[]>(`/api/creators/${creatorId}/collections`);

export interface WorksListParams {
  page: number;
  page_size: number;
  q?: string;
  collection_id?: number;
  collection_none?: boolean;
  /** 契约 v1.4b:多选排除合集(与 collection_id 互斥,后端以 exclude 为准)。 */
  exclude_collection_ids?: number[];
  /** 契约 v1.2:type=live 表示含动图片段的图集。 */
  type?: WorkTypeFilter;
  /** 契约 v1.2:按 dl_status 过滤。 */
  dl?: WorkDlFilter;
  sort: WorkSort;
}

export function listWorks(creatorId: number, params: WorksListParams): Promise<Page<Work>> {
  const sp = new URLSearchParams({
    page: String(params.page),
    page_size: String(params.page_size),
    sort: params.sort,
  });
  if (params.q) sp.set("q", params.q);
  if (params.exclude_collection_ids?.length) {
    // 契约 v1.4b:exclude 优先,不再发送 collection_id/collection_none。
    sp.set("exclude_collection_ids", params.exclude_collection_ids.join(","));
  } else if (params.collection_none) {
    sp.set("collection_id", "none");
  } else if (params.collection_id !== undefined) {
    sp.set("collection_id", String(params.collection_id));
  }
  if (params.type) sp.set("type", params.type);
  if (params.dl) sp.set("dl", params.dl);
  return api<Page<Work>>(`/api/creators/${creatorId}/works?${sp.toString()}`);
}

export function getWork(workId: number): Promise<WorkDetail> {
  return api<WorkDetail>(`/api/works/${workId}`);
}

/** batch-ids 的筛选条件(与 works 列表完全一致,服务"按筛选全选")。 */
export interface BatchIdsFilters {
  collection_none?: boolean;
  q?: string;
  collection_id?: number;
  /** 契约 v1.4b:多选排除合集(JSON 数组,与列表语义一致)。 */
  exclude_collection_ids?: number[];
  type?: WorkTypeFilter;
  dl?: WorkDlFilter;
}

export const batchWorkIds = (creatorId: number, filters?: BatchIdsFilters) =>
  api<BatchIdsResult>("/api/works/batch-ids", {
    method: "POST",
    json: {
      creator_id: creatorId,
      ...(filters?.q ? { q: filters.q } : {}),
      ...(filters?.exclude_collection_ids?.length
        ? { exclude_collection_ids: filters.exclude_collection_ids }
        : {}),
      ...(filters?.exclude_collection_ids?.length
        ? {}
        : filters?.collection_id !== undefined
          ? { collection_id: filters.collection_id }
          : {}),
      ...(filters?.type ? { type: filters.type } : {}),
      ...(filters?.dl ? { dl: filters.dl } : {}),
    },
  });

export const listWorkQualities = (workId: number) =>
  api<QualityOption[]>(`/api/works/${workId}/qualities`);

// ---------- 资产 ----------
export function listAssets(workId: number): Promise<Asset[]> {
  return api<Asset[]>(`/api/works/${workId}/assets`);
}

// ---------- 下载 ----------
export interface DownloadsQuery {
  status?: JobStatus[];
  cursor?: number | null;
  limit?: number;
}

export function listDownloads(params: DownloadsQuery): Promise<CursorPage<Job>> {
  const sp = new URLSearchParams();
  if (params.status?.length) sp.set("status", params.status.join(","));
  if (params.cursor !== undefined && params.cursor !== null) sp.set("cursor", String(params.cursor));
  sp.set("limit", String(params.limit ?? 50));
  return api<CursorPage<Job>>(`/api/downloads?${sp.toString()}`);
}

export const downloadsSummary = () => api<DownloadsSummary>("/api/downloads/summary");
export const createDownloads = (workIds: number[], quality: Quality | string) =>
  api<CreateDownloadsResult>("/api/downloads", { method: "POST", json: { work_ids: workIds, quality } });
export const retryDownload = (id: number, quality?: string) =>
  api<Job>(`/api/downloads/${id}/retry`, { method: "POST", json: quality ? { quality } : {} });
export const cancelDownload = (id: number) => api<Job>(`/api/downloads/${id}/cancel`, { method: "POST" });
export const deleteDownload = (id: number) => api<{ ok: boolean }>(`/api/downloads/${id}`, { method: "DELETE" });
export const batchDownloads = (action: "retry" | "cancel" | "delete", ids: number[], quality?: string) =>
  api<BatchActionResult>("/api/downloads/batch", { method: "POST", json: { action, ids, ...(quality ? { quality } : {}) } });
export const retryFailedDownloads = () => api<BatchActionResult>("/api/downloads/retry-failed", { method: "POST" });
export const clearCompletedDownloads = () => api<BatchActionResult>("/api/downloads/clear-completed", { method: "POST" });
export const pauseQueue = () => api<DownloadsSummary>("/api/downloads/queue/pause", { method: "POST", json: {} });
export const resumeQueue = (retryFailed = false) =>
  api<DownloadsSummary>("/api/downloads/queue/resume", { method: "POST", json: { retry_failed: retryFailed } });

// ---------- 订阅 ----------
export const listSubscriptions = () => api<Subscription[]>("/api/subscriptions");
export const createSubscription = (input: SubscriptionInput) =>
  api<Subscription>("/api/subscriptions", { method: "POST", json: input });
export const updateSubscription = (id: number, patch: SubscriptionPatch) =>
  api<Subscription>(`/api/subscriptions/${id}`, { method: "PATCH", json: patch });
export const deleteSubscription = (id: number) => api<{ ok: boolean }>(`/api/subscriptions/${id}`, { method: "DELETE" });
/** 监控期间新增作品明细(表格"新增 X · 已下载 Y"链接背后的抽屉数据)。 */
export const getSubscriptionNewWorks = (id: number) =>
  api<SubscriptionNewWorks>(`/api/subscriptions/${id}/new-works`);

// ---------- 设置 ----------
export const getSettings = () => api<Settings>("/api/settings");
export const patchSettings = (patch: SettingsPatch) =>
  api<Settings>("/api/settings", { method: "PATCH", json: patch });
export const testNotification = () => api<NotificationTestResult>("/api/notifications/test", { method: "POST" });
