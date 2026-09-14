/**
 * 火花(续火花)端点封装 — docs/HUOHUA_EXECUTION_PLAN.md §5。
 * 业务端点走 /api/spark/*;登录组经 /api/spark/login/* 反代到引擎。
 * Cookie 内容不出现在任何响应展示里(只回 cookie_count)。
 */
import { api } from "./client";
import type {
  SparkAccount,
  SparkFriendToday,
  SparkFriendUpdate,
  SparkLoginExport,
  SparkLoginStatus,
  SparkOverview,
  SparkRecordsPage,
  SparkSettings,
} from "./spark-types";

// ---------- 总览 / 引擎 ----------
export const getSparkOverview = () => api<SparkOverview>("/api/spark/overview");
export const getSparkEngineHealth = () =>
  api<{ ok: boolean; version?: number; task_running?: boolean; detail?: string }>(
    "/api/spark/engine/health",
  );

// ---------- 账号 ----------
export const listSparkAccounts = () => api<SparkAccount[]>("/api/spark/accounts");
export const createSparkAccount = (input: {
  unique_id: string;
  username?: string;
  nickname?: string;
  profile_name?: string;
}) => api<SparkAccount>("/api/spark/accounts", { method: "POST", json: input });
export const updateSparkAccount = (id: number, patch: { enabled?: boolean; nickname?: string }) =>
  api<SparkAccount>(`/api/spark/accounts/${id}`, { method: "PATCH", json: patch });
export const deleteSparkAccount = (id: number) =>
  api<{ status: string }>(`/api/spark/accounts/${id}`, { method: "DELETE" });

// ---------- 好友 ----------
export function listSparkFriends(
  accountId: number,
  opts: { selected?: boolean; withToday?: boolean } = {},
): Promise<SparkFriendToday[]> {
  const qs = new URLSearchParams();
  if (opts.selected !== undefined) qs.set("selected", String(opts.selected));
  if (opts.withToday) qs.set("with_today", "1");
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return api<SparkFriendToday[]>(`/api/spark/accounts/${accountId}/friends${suffix}`);
}
export const refreshSparkFriends = (accountId: number) =>
  api<{ count: number; new: number }>(`/api/spark/accounts/${accountId}/friends/refresh`, {
    method: "POST",
    json: {},
  });
export const patchSparkFriends = (accountId: number, updates: SparkFriendUpdate[]) =>
  api<{ status: string }>(`/api/spark/accounts/${accountId}/friends`, {
    method: "PATCH",
    json: { updates },
  });

// ---------- 发送 ----------
export const triggerSparkRun = (mode: "now" | "failed" | "unsent", accountIds?: number[]) =>
  api<{ accepted: boolean }>("/api/spark/send/run", {
    method: "POST",
    json: { mode, account_ids: accountIds },
  });
export function listSparkRecords(params: { accountId?: number; cursor?: number; limit?: number } = {}) {
  const qs = new URLSearchParams();
  if (params.accountId !== undefined) qs.set("account_id", String(params.accountId));
  if (params.cursor !== undefined) qs.set("cursor", String(params.cursor));
  if (params.limit !== undefined) qs.set("limit", String(params.limit));
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return api<SparkRecordsPage>(`/api/spark/records${suffix}`);
}

// ---------- 设置 / Cookie ----------
export const getSparkSettings = () => api<SparkSettings>("/api/spark/settings");
export const putSparkSettings = (settings: SparkSettings) =>
  api<SparkSettings>("/api/spark/settings", { method: "PUT", json: settings });
/** 引擎导出该账号 Cookie 并写入 <data_dir>/.cookie(归档侧车热加载)。 */
export const exportSparkCookies = (accountId: number) =>
  api<{ cookie_count: number }>("/api/spark/cookies/export", {
    method: "POST",
    json: { account_id: accountId },
  });

// ---------- 登录(反代引擎 /login/*) ----------
export const openSparkLogin = () =>
  api<{ status: string }>("/api/spark/login/open", { method: "POST", json: {} });
export const refreshSparkQr = () =>
  api<{ status: string }>("/api/spark/login/refresh-qr", { method: "POST", json: {} });
export const getSparkLoginStatus = () => api<SparkLoginStatus>("/api/spark/login/status");
/** 登录成功后导出身份 + Cookie(引擎会把登录 profile 复制为 uid-{unique_id})。 */
export const exportSparkLogin = () =>
  api<SparkLoginExport>("/api/spark/login/export", { method: "POST", json: {} });
export const closeSparkLogin = () =>
  api<{ status: string }>("/api/spark/login/close", { method: "POST", json: {} });
/** 二维码图片地址(t 为时间戳防缓存)。 */
export const sparkLoginQrUrl = (t: number) => `/api/spark/login/qr?t=${t}`;
/**
 * 轮询登录二维码:引擎 /login/qr 在二维码尚未渲染时返回 202(继续等)、
 * 过期返回 409(需调 refresh-qr)、就绪返回 PNG。走 blob URL 展示,
 * 避免 <img src> 直怼 202 JSON 渲染成裂图。
 */
export type SparkLoginQr = { state: "pending" } | { state: "expired" } | { state: "ready"; url: string };
export async function fetchSparkLoginQr(): Promise<SparkLoginQr> {
  const res = await fetch(`/api/spark/login/qr?t=${Date.now()}`, { cache: "no-store" });
  if (res.status === 202) return { state: "pending" };
  if (res.status === 409) return { state: "expired" };
  if (!res.ok) throw new Error(`二维码加载失败 (${res.status})`);
  const blob = await res.blob();
  if (!blob.type.startsWith("image/")) return { state: "pending" };
  return { state: "ready", url: URL.createObjectURL(blob) };
}
/** noVNC 远程桌面 iframe 地址(滑块验证等人工干预)。 */
export const sparkLoginVncUrl = () =>
  "/api/spark/login/vnc/vnc.html?autoconnect=1&resize=scale&path=api/spark/login/vnc/websockify";
