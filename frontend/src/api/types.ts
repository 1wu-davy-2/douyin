/**
 * 与 docs/api.md(冻结契约)逐字对齐的类型定义。
 */

// ---------- 枚举 ----------
export type DLStatus = "none" | "queued" | "downloading" | "succeeded" | "failed" | "canceled";
export type JobStatus = "queued" | "downloading" | "paused_q" | "succeeded" | "failed" | "canceled";
export type TargetType = "creator" | "collection";
export type ScanStatus = "running" | "succeeded" | "partial" | "failed";
export type Quality = "540p" | "720p" | "1080p";
export type WorkSort = "published_at_desc" | "published_at_asc" | "duration_desc";
export type AssetKind = "video" | "cover" | "metadata";

export const JOB_STATUSES: readonly JobStatus[] = [
  "queued",
  "downloading",
  "paused_q",
  "succeeded",
  "failed",
  "canceled",
];

export const QUALITIES: readonly Quality[] = ["540p", "720p", "1080p"];

// ---------- 通用 ----------
export interface Page<T> {
  items: T[];
  total: number;
  page: number;
  page_size: number;
}

export interface CursorPage<T> {
  items: T[];
  next_cursor: number | null;
}

// ---------- 认证 ----------
export interface AuthStatus {
  authenticated: boolean;
  initialized: boolean;
  username?: string;
}

export interface Health {
  status: string;
  provider: "mock" | "sidecar";
  sidecar: "stopped" | "starting" | "running";
  real_scan_ready: boolean;
}

// ---------- 博主与作品 ----------
export interface Creator {
  id: number;
  sec_uid: string;
  nickname: string;
  avatar_url: string;
  profile_url: string;
  reported_work_count: number;
  works_count: number;
  downloaded_count: number;
  created_at: string;
}

export interface ScanRun {
  id: number;
  status: ScanStatus;
  pages: number;
  new_count: number;
  updated_count: number;
  empty_pages: number;
  completeness: number;
  started_at: string;
  finished_at: string | null;
  last_error: string | null;
}

export interface CreatorDetail extends Creator {
  last_scan: ScanRun | null;
}

export interface CreateCreatorResult {
  creator_id: number;
  scan_id: number;
}

export interface RescanResult {
  scan_id: number;
}

export interface Collection {
  id: number;
  mix_id: string;
  name: string;
  cover_url: string;
  works_count: number;
  downloaded_count: number;
}

export interface Work {
  id: number;
  item_id: string;
  title: string;
  cover_url: string;
  duration: number;
  published_at: string;
  collection_id: number | null;
  collection_name: string | null;
  dl_status: DLStatus;
  downloaded_quality: string | null;
  created_at: string;
}

export interface WorksQuery {
  page: number;
  page_size: number;
  q?: string;
  collection_id?: number;
  sort: WorkSort;
}

export interface MixInfo {
  collection_id: number;
  mix_id: string;
  name: string;
}

export interface Asset {
  id: number;
  work_id: number;
  kind: AssetKind;
  path: string;
  size_bytes: number;
  quality?: string | null;
}

export interface WorkDetail extends Work {
  mix_info: MixInfo | null;
  asset: Asset[];
  last_job: Job | null;
}

export interface BatchIdsResult {
  ids: number[];
}

export interface QualityOption {
  quality: Quality;
  width: number;
  height: number;
  bitrate: number;
  size_bytes: number;
}

// ---------- 下载任务 ----------
export interface Job {
  id: number;
  work_id: number;
  creator_id: number;
  work_title: string;
  creator_nickname: string;
  status: JobStatus;
  quality: string;
  attempts: number;
  total_bytes: number;
  downloaded_bytes: number;
  speed_bps: number;
  error: string | null;
  queued_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface CreateDownloadsResult {
  created: number[];
  skipped: { work_id: number; reason: string }[];
}

export interface DownloadsSummary {
  queued: number;
  downloading: number;
  failed: number;
  succeeded: number;
  canceled: number;
  paused: boolean;
  concurrency: number;
}

export interface BatchAction {
  action: "retry" | "cancel" | "delete";
  ids: number[];
  quality?: string;
}

export interface BatchActionResult {
  affected: number;
}

// ---------- 订阅 ----------
export interface Subscription {
  id: number;
  target_type: TargetType;
  creator_id: number;
  collection_id?: number | null;
  creator_nickname: string;
  target_name: string;
  interval_minutes: number;
  auto_download: boolean;
  quality: string;
  last_run_at: string | null;
  next_run_at: string | null;
  enabled: boolean;
}

export interface SubscriptionInput {
  target_type: TargetType;
  creator_id: number;
  collection_id?: number;
  interval_minutes: number;
  auto_download: boolean;
  quality?: string;
}

/** PATCH /api/subscriptions/{id} 的部分更新载荷(契约:同上字段均可选,另含 enabled)。 */
export interface SubscriptionPatch {
  interval_minutes?: number;
  auto_download?: boolean;
  quality?: string;
  enabled?: boolean;
}

// ---------- 设置 ----------
export interface SmtpConfig {
  host: string;
  port: number;
  username: string;
  password: string;
  from: string;
  to: string;
}

export interface Settings {
  provider_mode: "auto" | "sidecar" | "mock";
  cookie: string;
  download_concurrency: number;
  download_quality: string;
  scan_page_delay_ms: number;
  scan_max_empty_pages: number;
  incremental_stop_pages: number;
  completeness_gap_threshold: number;
  sidecar_idle_timeout_minutes: number;
  smtp: SmtpConfig;
  notify_on_new_work: boolean;
  notify_on_failure: boolean;
}

export interface SettingsPatch {
  provider_mode?: Settings["provider_mode"];
  cookie?: string;
  download_concurrency?: number;
  download_quality?: string;
  scan_page_delay_ms?: number;
  scan_max_empty_pages?: number;
  incremental_stop_pages?: number;
  completeness_gap_threshold?: number;
  sidecar_idle_timeout_minutes?: number;
  smtp?: Partial<SmtpConfig>;
  notify_on_new_work?: boolean;
  notify_on_failure?: boolean;
}

export interface NotificationTestResult {
  ok: boolean;
  error?: string;
}

// ---------- SSE 事件 ----------
export interface DownloadProgressEvent {
  job_id: number;
  downloaded_bytes: number;
  total_bytes: number;
  speed_bps: number;
}

export interface DownloadStatusEvent {
  job_id: number;
  status: JobStatus;
  error: string | null;
  work_id: number;
}

export interface ScanProgressEvent {
  scan_id: number;
  creator_id: number;
  page: number;
  new_count: number;
  updated_count: number;
  status: ScanStatus;
}

export interface ScanDoneEvent {
  scan_id: number;
  creator_id: number;
  status: "succeeded" | "partial" | "failed";
  pages: number;
  new_count: number;
  completeness: number;
  last_error: string | null;
}

export interface ProviderStatusEvent {
  sidecar: "stopped" | "starting" | "running";
  risk_paused: boolean;
  paused_until: string | null;
}
