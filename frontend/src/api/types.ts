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
export type WorkType = "video" | "image" | "daily";
/** works 列表/batch-ids 的 type 筛选参数:live = 含动图/实况片段的图集。 */
export type WorkTypeFilter = "video" | "image" | "live" | "daily";
/** works 列表/batch-ids 的 dl 筛选参数(按 dl_status 过滤,canceled 不在契约筛选枚举内)。 */
export type WorkDlFilter = Exclude<DLStatus, "canceled">;
export type AssetKind = "video" | "image" | "cover" | "metadata";

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
  /** 契约 v1.4:用户设置的显示别名;null = 显示默认昵称 nickname。 */
  alias: string | null;
  /** 契约 v1.4:分组名;null = 未分组。 */
  group: string | null;
  avatar_url: string;
  profile_url: string;
  reported_work_count: number;
  works_count: number;
  downloaded_count: number;
  /** 契约 v1.2:该博主全部已下载资产的字节总和(SUM assets.size_bytes,kind video+image)。 */
  download_bytes: number;
  /** 契约 v1.3:该博主独立下载根目录(绝对路径);null = 跟随全局 download_root。 */
  download_root: string | null;
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

/** 契约 v1.4b:POST /api/creators subscribe 对象(创建/刷新 creator 级订阅)。 */
export interface CreatorSubscribeInput {
  /** 监控间隔(分钟),必填 >= 1。 */
  interval_minutes: number;
  /** 画质;省略 = 全局默认画质。 */
  quality?: string;
  /** 自动下载新作品;省略 = true。 */
  auto_download?: boolean;
}

/** 契约 v1.4/1.4b:POST /api/creators 的可选参数(202 响应新增 creator 摘要)。 */
export interface CreateCreatorInput {
  profile_url: string;
  /** 独立下载根目录(绝对路径,后端校验并自动创建);省略 = 跟随全局。 */
  download_root?: string;
  /** 非空时同时创建 creator 级订阅(契约 v1.4b)。 */
  subscribe?: CreatorSubscribeInput;
  /** 初始分组名;空 = 未分组。 */
  group?: string;
  /** 显示别名;空 = 用默认昵称。 */
  alias?: string;
}

export interface CreateCreatorResult {
  creator_id: number;
  scan_id: number;
  /** 契约 v1.4:202 响应带回完整 creator 视图(含 alias/group)。 */
  creator?: Creator;
}

/** 契约 v1.4:PATCH /api/creators/{id} 请求与响应。alias/group null 或空串 = 清除。 */
export interface CreatorPatch {
  alias?: string | null;
  group?: string | null;
}

export interface CreatorPatchResult {
  ok: boolean;
  alias: string | null;
  group: string | null;
}

export interface RescanResult {
  scan_id: number;
}

/** 契约 v1.3:PATCH /api/creators/{id}/download-root 响应。 */
export interface BatchDeleteSkipped {
  work_id: number;
  reason: string;
}

export interface BatchDeleteWorksResult {
  deleted: number[];
  freed_bytes: number;
  skipped: BatchDeleteSkipped[];
}

export interface WorksRedownloadResult {
  enqueued: number[];
  skipped: { work_id: number; reason: string }[];
  freed_bytes: number;
}

export interface CreatorDownloadRootResult {
  ok: boolean;
  /** null = 已清除独立设置,跟随全局。 */
  download_root: string | null;
}

/** 契约 v1.3:move-downloads 失败明细元素。 */
export interface MoveFailedFile {
  path: string;
  error: string;
}

/** 契约 v1.3:POST /api/creators/{id}/move-downloads 响应。 */
export interface MoveDownloadsResult {
  moved_files: number;
  moved_bytes: number;
  /** 因目标已存在等原因跳过的文件数(后端可能省略)。 */
  skipped_files?: number;
  failed_files: MoveFailedFile[];
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
  /** 契约 v1.1:video=视频,image=图集/动图(此时 duration=0、image_count 为张数)。 */
  type: WorkType;
  image_count: number;
  dl_status: DLStatus;
  downloaded_quality: string | null;
  created_at: string;
}

export interface WorksQuery {
  page: number;
  page_size: number;
  q?: string;
  collection_id?: number;
  /** "none" = 只看单发作品(排除合集) */
  collection_none?: boolean;
  /** 契约 v1.4b:多选排除合集(与 collection_id 互斥,exclude 优先)。 */
  exclude_collection_ids?: number[];
  /** 契约 v1.2:type=live 表示含动图片段的图集。 */
  type?: WorkTypeFilter;
  dl?: WorkDlFilter;
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
  /** video=画质档;image=4 位序号("0001"…按原始顺序);live 片段="live0001"…;cover/metadata=null。 */
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
  /** 契约 v1.2:监控期间(订阅创建后)新收录的作品数,及其中已下载(succeeded)数。 */
  new_works: number;
  new_downloaded: number;
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

/** GET /api/subscriptions/{id}/new-works 响应:监控期间(订阅创建后)新增作品明细。 */
export interface SubscriptionNewWorks {
  items: Work[];
  /** 监控期间新增作品总数(与列表 new_works 同口径)。 */
  total: number;
  /** 其中已下载(succeeded)数(与列表 new_downloaded 同口径)。 */
  downloaded: number;
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

export interface MinioSettings {
  enabled: boolean;
  endpoint: string;
  bucket: string;
  access_key: string;
  /** 只写:回显仅 secret_set 布尔 */
  secret_key: string;
  secret_set: boolean;
  use_ssl: boolean;
  prefix: string;
  concurrency: number;
}

export interface MinioStatus {
  queued: number;
  uploaded_total: number;
  failed_total: number;
  dropped_total: number;
  last_error: string | null;
}

export interface Settings {
  provider_mode: "auto" | "sidecar" | "mock";
  cookie: string;
  /** 契约 v1.3:全局下载根目录(绝对路径,空 = 默认 <data_dir>/downloads);保存时后端自动建目录。 */
  download_root: string;
  download_concurrency: number;
  download_quality: string;
  scan_page_delay_ms: number;
  scan_max_empty_pages: number;
  scan_concurrency: number;
  incremental_stop_pages: number;
  completeness_gap_threshold: number;
  sidecar_idle_timeout_minutes: number;
  minio: MinioSettings;
  smtp: SmtpConfig;
  notify_on_new_work: boolean;
  notify_on_failure: boolean;
}

export interface SettingsPatch {
  provider_mode?: Settings["provider_mode"];
  cookie?: string;
  /** 契约 v1.3:空字符串 = 恢复默认 <data_dir>/downloads。 */
  download_root?: string;
  download_concurrency?: number;
  download_quality?: string;
  scan_page_delay_ms?: number;
  scan_max_empty_pages?: number;
  scan_concurrency?: number;
  incremental_stop_pages?: number;
  completeness_gap_threshold?: number;
  sidecar_idle_timeout_minutes?: number;
  minio?: Partial<MinioSettings>;
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
