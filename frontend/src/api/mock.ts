/**
 * 内存 mock API(VITE_MOCK=1 时由 vite.config.ts 挂载到 dev server 的 /api)。
 * 纯 TS 实现(无 DOM / Node 专属 API),完整覆盖 docs/api.md 契约语义:
 * 分页(页码式 + 游标式)、筛选/排序、任务状态流转(queued→downloading→succeeded/failed)、
 * SSE 事件(download.progress / download.status / scan.progress / scan.done / provider.status)。
 */
import type {
  Asset,
  AssetKind,
  BatchActionResult,
  Collection,
  CreateDownloadsResult,
  Creator,
  CreatorDetail,
  CursorPage,
  DLStatus,
  DownloadsSummary,
  Job,
  JobStatus,
  NotificationTestResult,
  Page,
  QualityOption,
  ScanRun,
  ScanStatus,
  Settings,
  Subscription,
  TargetType,
  Work,
  WorkDetail,
  WorkType,
  MinioSettings,
} from "./types";
import { JOB_STATUSES } from "./types";

// ---------------------------------------------------------------- 工具

function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const rand = mulberry32(0xd00df00d);
const ri = (min: number, max: number): number => min + Math.floor(rand() * (max - min + 1));
function pick<T>(arr: readonly T[]): T {
  return arr[Math.floor(rand() * arr.length)];
}

/** RFC3339 UTC(秒级)。 */
function iso(t: number): string {
  return new Date(t).toISOString().replace(/\.\d{3}Z$/, "Z");
}

const MB = 1024 * 1024;

function hashString(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

// ---------------------------------------------------------------- 内部模型

interface MockCreator {
  id: number;
  sec_uid: string;
  nickname: string;
  /** 契约 v1.4:显示别名;null = 用昵称。 */
  alias: string | null;
  /** 契约 v1.4:分组名;null = 未分组。 */
  group_name: string | null;
  avatar_url: string;
  profile_url: string;
  reported_work_count: number;
  /** 契约 v1.3:独立下载根目录;null = 跟随全局。 */
  download_root: string | null;
  created_at: string;
}

interface MockCollection {
  id: number;
  creator_id: number;
  mix_id: string;
  name: string;
  cover_url: string;
}

interface MockWork {
  id: number;
  creator_id: number;
  item_id: string;
  title: string;
  cover_url: string;
  duration: number;
  published_at: string;
  collection_id: number | null;
  created_at: string;
  /** 契约 v1.1:image=图集(duration 恒为 0)。 */
  type: WorkType;
  /** image 作品的图集张数。 */
  image_count: number;
  /** image 作品附带的动图/实况视频片段数(0 或 1)。 */
  live_count: number;
  /** 内部字段:每张图的 data URI(序列化时剔除,仅注入 image 资产 path)。 */
  image_urls: string[];
}

interface MockJob {
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
  /** 内部字段:本次尝试的“预谋失败”进度点(序列化时剔除)。 */
  fail_plan: { at_pct: number; error: string } | null;
  /** 内部字段:上次 progress 事件时间戳。 */
  last_emit: number;
}

interface MockScanRun {
  id: number;
  creator_id: number;
  trigger: string;
  full: boolean;
  status: ScanStatus;
  pages: number;
  new_count: number;
  updated_count: number;
  empty_pages: number;
  completeness: number;
  last_error: string | null;
  started_at: string;
  finished_at: string | null;
}

interface MockAsset {
  id: number;
  work_id: number;
  kind: AssetKind;
  path: string;
  size_bytes: number;
  quality: string | null;
}

interface MockSubscription {
  id: number;
  target_type: TargetType;
  creator_id: number;
  collection_id: number | null;
  interval_minutes: number;
  auto_download: boolean;
  quality: string;
  enabled: boolean;
  last_run_at: string | null;
  created_at: string;
}

interface MockSettings {
  provider_mode: Settings["provider_mode"];
  cookie: string;
  /** 契约 v1.3:全局下载根目录(绝对路径,空 = 默认 <data_dir>/downloads)。 */
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
  smtp: Settings["smtp"];
  notify_on_new_work: boolean;
  notify_on_failure: boolean;
}

// ---------------------------------------------------------------- 生成素材

const TITLE_A = [
  "巷口", "夜航", "山雾", "旧钟", "速写", "长焦", "雨季", "候车", "胶片", "一瞬",
  "深巷", "回声", "日暮", "晚风", "街角", "拾光", "远山", "浮桥", "灯火", "潮汐",
];
const TITLE_B = ["手记", "随拍", "光影", "碎片", "片刻", "杂记", "小品", "拾遗"];
const ERROR_POOL = [
  "CDN 下载超时,已重试 3 次",
  "403: 疑似风控拦截,请更换 Cookie",
  "网络连接中断",
  "磁盘写入失败",
  "视频地址解析失败(无可用清晰度)",
];
const NICK_POOL = ["柠檬气泡", "汽水漫记", "山茶看世界", "北岛影像", "白杨胡同", "陈皮日常", "路灯下", "南方车站"];

function coverSvg(seed: number): string {
  const h1 = seed % 360;
  const h2 = (h1 + 40 + (seed % 90)) % 360;
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' width='320' height='180'>` +
    `<defs><linearGradient id='g' x1='0' y1='0' x2='1' y2='1'>` +
    `<stop offset='0%' stop-color='hsl(${h1},42%,32%)'/>` +
    `<stop offset='100%' stop-color='hsl(${h2},50%,15%)'/>` +
    `</linearGradient></defs>` +
    `<rect width='320' height='180' fill='url(#g)'/>` +
    `<circle cx='${60 + (seed % 200)}' cy='${60 + ((seed * 7) % 80)}' r='38' fill='rgba(255,255,255,0.08)'/>` +
    `<circle cx='${40 + ((seed * 3) % 240)}' cy='${120 - ((seed * 5) % 60)}' r='16' fill='rgba(255,255,255,0.12)'/>` +
    `</svg>`;
  return "data:image/svg+xml;charset=utf-8," + encodeURIComponent(svg);
}

function avatarSvg(letter: string, hue: number): string {
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' width='96' height='96'>` +
    `<rect width='96' height='96' rx='48' fill='hsl(${hue},38%,30%)'/>` +
    `<text x='50%' y='55%' font-size='42' fill='rgba(255,255,255,0.9)' font-family='sans-serif' ` +
    `text-anchor='middle' dominant-baseline='middle'>${letter}</text>` +
    `</svg>`;
  return "data:image/svg+xml;charset=utf-8," + encodeURIComponent(svg);
}

/** 图集第 n 张:竖版 SVG data URI,带大号序号文字方便肉眼核对轮播顺序。 */
function imageSvg(seed: number, n: number, total: number): string {
  const h1 = (seed * 37 + n * 53) % 360;
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' width='400' height='700'>` +
    `<defs><linearGradient id='g' x1='0' y1='0' x2='1' y2='1'>` +
    `<stop offset='0%' stop-color='hsl(${h1},45%,30%)'/>` +
    `<stop offset='100%' stop-color='hsl(${(h1 + 60) % 360},50%,16%)'/>` +
    `</linearGradient></defs>` +
    `<rect width='400' height='700' fill='url(#g)'/>` +
    `<circle cx='${80 + ((seed + n * 31) % 240)}' cy='${140 + ((seed * 7 + n * 17) % 420)}' r='110' fill='rgba(255,255,255,0.06)'/>` +
    `<text x='50%' y='44%' font-size='170' font-weight='bold' fill='rgba(255,255,255,0.88)' font-family='sans-serif' ` +
    `text-anchor='middle' dominant-baseline='middle'>${n}</text>` +
    `<text x='50%' y='60%' font-size='34' fill='rgba(255,255,255,0.6)' font-family='sans-serif' ` +
    `text-anchor='middle' dominant-baseline='middle'>第 ${n} 张 / 共 ${total} 张</text>` +
    `</svg>`;
  return "data:image/svg+xml;charset=utf-8," + encodeURIComponent(svg);
}

/**
 * 图集覆盖规则(按作品序号确定性判定):
 * - index % 7 == 6 → image 作品,3-6 张图(3 + index % 4)
 * - index % 14 == 13 → 另带 1 段动图/实况视频片段(live)
 */
function imageRuleFor(index: number): { isImage: boolean; imageCount: number; liveCount: number; isDaily: boolean } {
  const isImage = index % 7 === 6;
  const isDaily = index % 9 === 8;
  if (!isImage) return { isImage: false, imageCount: 0, liveCount: 0, isDaily };
  const imageCount = 3 + (index % 4);
  const liveCount = index % 14 === 13 ? 1 : 0;
  return { isImage, imageCount, liveCount, isDaily };
}

function extractSecUid(url: string): string {
  const q = url.match(/sec_uid=([^&]+)/);
  if (q) return decodeURIComponent(q[1]);
  const token = url.match(/(MS4wLjABAAAA[\w-]+)/);
  if (token) return token[1];
  return "sec_" + hashString(url).toString(36);
}

// ---------------------------------------------------------------- 全局状态

interface MockState {
  ids: { creator: number; collection: number; work: number; job: number; asset: number; scan: number; sub: number };
  creators: MockCreator[];
  collections: MockCollection[];
  works: MockWork[];
  jobs: MockJob[];
  assets: MockAsset[];
  scans: MockScanRun[];
  subs: MockSubscription[];
  settings: MockSettings;
  sessions: Map<string, string>;
  username: string | null;
  password: string | null;
  initialized: boolean;
  paused: boolean;
  loginFails: number;
}

const state: MockState = {
  ids: { creator: 0, collection: 0, work: 0, job: 0, asset: 0, scan: 0, sub: 0 },
  creators: [],
  collections: [],
  works: [],
  jobs: [],
  assets: [],
  scans: [],
  subs: [],
  settings: {
    provider_mode: "auto",
    cookie: "",
    download_root: "",
    download_concurrency: 3,
    download_quality: "1080p",
    scan_page_delay_ms: 2000,
    scan_max_empty_pages: 3,
    scan_concurrency: 3,
    incremental_stop_pages: 2,
    completeness_gap_threshold: 10,
    sidecar_idle_timeout_minutes: 10,
    minio: { enabled: false, endpoint: "", bucket: "", access_key: "", secret_key: "", secret_set: false, use_ssl: false, prefix: "douyin/", concurrency: 2 },
    smtp: { host: "", port: 465, username: "", password: "", from: "", to: "" },
    notify_on_new_work: true,
    notify_on_failure: true,
  },
  sessions: new Map(),
  username: null,
  password: null,
  initialized: false,
  paused: false,
  loginFails: 0,
};

// ---------------------------------------------------------------- 数据播种

function makeWorks(creator: MockCreator, count: number, baseTime: number, cols: MockCollection[]): MockWork[] {
  const works: MockWork[] = [];
  let t = baseTime;
  for (let i = 0; i < count; i++) {
    t -= ri(3, 26) * 3600_000;
    const col = cols.length > 0 && rand() < 0.45 ? pick(cols) : null;
    const rule = imageRuleFor(i);
    const imageUrls = rule.isImage
      ? Array.from({ length: rule.imageCount }, (_, n) => imageSvg(creator.id * 1000 + i, n + 1, rule.imageCount))
      : [];
    works.push({
      id: ++state.ids.work,
      creator_id: creator.id,
      item_id: `v${(hashString(`${creator.sec_uid}:${i}`) >>> 0).toString(36)}${(rand() * 1e6).toFixed(0)}`,
      title: `${pick(TITLE_A)}${pick(TITLE_B)} #${i + 1}`,
      cover_url: rule.isImage ? imageUrls[0] : coverSvg(creator.id * 1000 + i),
      duration: rule.isImage ? 0 : Math.round(8 + rand() * rand() * 700),
      published_at: iso(t),
      collection_id: col ? col.id : null,
      // created_at 随发布时间递减铺开:订阅的监控窗口(.created_at >= 订阅创建点)
      // 才能截出真实的"新增"数量,与后端口径一致
      created_at: iso(t),
      type: rule.isDaily ? "daily" : rule.isImage ? "image" : "video",
      image_count: rule.imageCount,
      live_count: rule.liveCount,
      image_urls: imageUrls,
    });
  }
  return works;
}

function seed(): void {
  const now = Date.now();
  const seedCreator = (nickname: string, reported: number): MockCreator => {
    const c: MockCreator = {
      id: ++state.ids.creator,
      sec_uid: `MS4wLjABAAAA_seed_${state.ids.creator}_${hashString(nickname).toString(36)}`,
      nickname,
      alias: null,
      group_name: null,
      avatar_url: avatarSvg(nickname[0], (hashString(nickname) % 300) + 20),
      profile_url: `https://www.douyin.com/user/MS4wLjABAAAA_seed_${state.ids.creator}`,
      reported_work_count: reported,
      download_root: null,
      created_at: iso(now - (4 - state.ids.creator) * 86400_000),
    };
    state.creators.push(c);
    return c;
  };
  const seedCollections = (creator: MockCreator, names: string[]): MockCollection[] => {
    return names.map((name, i) => {
      const col: MockCollection = {
        id: ++state.ids.collection,
        creator_id: creator.id,
        mix_id: `mix_${creator.id}_${i + 1}`,
        name,
        cover_url: coverSvg(9000 + creator.id * 50 + i),
      };
      state.collections.push(col);
      return col;
    });
  };

  // 博主 1:川流不息(128 作品,状态分布均衡,含 1 个合集;契约 v1.4 分组示例)
  const c1 = seedCreator("川流不息", 128);
  c1.group_name = "摄影";
  const [col1] = seedCollections(c1, ["城市夜行手记"]);
  const w1 = makeWorks(c1, 128, now - 86400_000, [col1]);
  state.works.push(...w1);

  // 博主 2:山城影像志(502 作品,部分已下载)
  const c2 = seedCreator("山城影像志", 502);
  c2.group_name = "日常";
  seedCollections(c2, ["两江夜色", "老城阶梯", "雾都十二时辰", "轻轨穿楼", "方言故事"]);
  const w2 = makeWorks(c2, 502, now - 2 * 86400_000, []);
  state.works.push(...w2);

  // 博主 3:夜晚放映厅(36 作品 + 3 合集,大多已下载;契约 v1.4 别名示例)
  const c3 = seedCreator("夜晚放映厅", 36);
  c3.alias = "夜放";
  c3.group_name = "摄影";
  const cols3 = seedCollections(c3, ["默片修复所", "午夜短片", "配乐实验"]);
  const w3 = makeWorks(c3, 36, now - 3 * 86400_000, cols3);
  state.works.push(...w3);
  // 契约 v1.3 示例:该博主设置了独立下载根目录(非空示例,便于前端区分"独立/全局")
  c3.download_root = "E:\\Media\\Douyin\\night-screening";

  // 初始下载任务(与作品 dl_status 一致)
  const thresholds: Record<number, { succ: number; fail: number; cancel: number }> = {
    [c1.id]: { succ: 0.55, fail: 0.6, cancel: 0.63 },
    [c2.id]: { succ: 0.26, fail: 0.28, cancel: 0.29 },
    [c3.id]: { succ: 0.55, fail: 0.62, cancel: 0.65 },
  };
  const byCreator = new Map<number, MockWork[]>();
  for (const w of state.works) {
    const list = byCreator.get(w.creator_id) ?? [];
    list.push(w);
    byCreator.set(w.creator_id, list);
  }
  for (const [creatorId, list] of byCreator) {
    const th = thresholds[creatorId] ?? { succ: 0.4, fail: 0.45, cancel: 0.47 };
    const creator = state.creators.find((c) => c.id === creatorId)!;
    for (const w of list) {
      const r = rand();
      let status: JobStatus | null = null;
      if (r < th.succ) status = "succeeded";
      else if (r < th.fail) status = "failed";
      else if (r < th.cancel) status = "canceled";
      if (!status) continue;
      const quality = rand() < 0.7 ? "1080p" : rand() < 0.6 ? "720p" : "540p";
      const total = ri(12, 88) * MB;
      const finished = now - ri(1, 420) * 3600_000;
      state.jobs.push({
        id: ++state.ids.job,
        work_id: w.id,
        creator_id: creatorId,
        work_title: w.title,
        creator_nickname: creator.nickname,
        status,
        quality,
        attempts: status === "failed" ? ri(1, 2) : 1,
        total_bytes: total,
        downloaded_bytes: status === "succeeded" ? total : Math.round(total * (0.2 + rand() * 0.5)),
        speed_bps: 0,
        error: status === "failed" ? pick(ERROR_POOL) : null,
        queued_at: iso(finished - 3600_000),
        started_at: iso(finished - 1800_000),
        finished_at: iso(finished),
        fail_plan: null,
        last_emit: 0,
      });
      if (status === "succeeded") createAssets(w, quality, total);
    }
  }

  // 额外制造“正在下载 3 个 + 排队 1 个”的活跃现场
  const idle = state.works.filter((w) => !state.jobs.some((j) => j.work_id === w.id));
  const live = [idle[0], idle[1], idle[2], idle[3]].filter((w): w is MockWork => Boolean(w));
  live.forEach((w, i) => {
    const creator = state.creators.find((c) => c.id === w.creator_id)!;
    const total = ri(24, 90) * MB;
    const downloading = i < 3;
    state.jobs.push({
      id: ++state.ids.job,
      work_id: w.id,
      creator_id: w.creator_id,
      work_title: w.title,
      creator_nickname: creator.nickname,
      status: downloading ? "downloading" : "queued",
      quality: "1080p",
      attempts: 1,
      total_bytes: total,
      downloaded_bytes: downloading ? Math.round(total * (0.1 + rand() * 0.5)) : 0,
      speed_bps: downloading ? ri(2, 9) * MB : 0,
      error: null,
      queued_at: iso(now - 60_000),
      started_at: downloading ? iso(now - 30_000) : null,
      finished_at: null,
      fail_plan: null,
      last_emit: 0,
    });
  });

  // 初始订阅(监控期间统计按真实口径从作品表推导,见 monitorStatsOf)
  state.subs.push({
    id: ++state.ids.sub,
    target_type: "creator",
    creator_id: c2.id,
    collection_id: null,
    interval_minutes: 30,
    auto_download: true,
    quality: "1080p",
    enabled: true,
    last_run_at: iso(now - 2 * 3600_000),
    created_at: iso(now - 6 * 86400_000),
  });
  state.subs.push({
    id: ++state.ids.sub,
    target_type: "collection",
    creator_id: c3.id,
    collection_id: cols3[0].id,
    interval_minutes: 60,
    auto_download: false,
    quality: "720p",
    enabled: true,
    last_run_at: iso(now - 5 * 3600_000),
    created_at: iso(now - 9 * 86400_000),
  });

  // 初始扫描记录
  for (const c of state.creators) {
    const count = state.works.filter((w) => w.creator_id === c.id).length;
    state.scans.push({
      id: ++state.ids.scan,
      creator_id: c.id,
      trigger: "manual",
      full: true,
      status: "succeeded",
      pages: Math.max(1, Math.ceil(count / 40)),
      new_count: count,
      updated_count: 0,
      empty_pages: 0,
      completeness: Math.min(100, Math.round((count / c.reported_work_count) * 100)),
      last_error: null,
      started_at: iso(now - 86400_000),
      finished_at: iso(now - 86400_000 + 600_000),
    });
  }
}

function createAssets(work: MockWork, quality: string, sizeBytes: number): void {
  // 图集作品:每张图一个 kind=image 资产(quality 为 4 位序号,path 直接放 data URI,
  // mock 不提供二进制 content 端点,播放器在 mock 模式取 path 当图片地址);live 片段 kind=video/live0001。
  if (work.type === "image") {
    for (let n = 1; n <= work.image_count; n++) {
      const q = String(n).padStart(4, "0");
      if (state.assets.some((a) => a.work_id === work.id && a.kind === "image" && a.quality === q)) continue;
      state.assets.push({
        id: ++state.ids.asset,
        work_id: work.id,
        kind: "image",
        path: work.image_urls[n - 1] ?? imageSvg(work.id, n, work.image_count),
        size_bytes: ri(120, 900) * 1024,
        quality: q,
      });
    }
    if (work.live_count > 0 && !state.assets.some((a) => a.work_id === work.id && a.kind === "video")) {
      state.assets.push({
        id: ++state.ids.asset,
        work_id: work.id,
        kind: "video",
        path: `downloads/${work.creator_id}/singles/${work.item_id}/live0001.mp4`,
        size_bytes: ri(2, 8) * MB,
        quality: "live0001",
      });
    }
  } else {
    const hasVideo = state.assets.some((a) => a.work_id === work.id && a.kind === "video");
    if (!hasVideo) {
      state.assets.push({
        id: ++state.ids.asset,
        work_id: work.id,
        kind: "video",
        path: `downloads/${work.creator_id}/${work.item_id}_${quality}.mp4`,
        size_bytes: sizeBytes,
        quality,
      });
    }
  }
  const hasCover = state.assets.some((a) => a.work_id === work.id && a.kind === "cover");
  if (!hasCover) {
    state.assets.push({
      id: ++state.ids.asset,
      work_id: work.id,
      kind: "cover",
      path: `downloads/${work.creator_id}/${work.item_id}.webp`,
      size_bytes: ri(30, 200) * 1024,
      quality: null,
    });
  }
  // 图集作品再补一份 metadata(契约排序 video→image→cover→metadata)
  if (work.type === "image" && !state.assets.some((a) => a.work_id === work.id && a.kind === "metadata")) {
    state.assets.push({
      id: ++state.ids.asset,
      work_id: work.id,
      kind: "metadata",
      path: `downloads/${work.creator_id}/singles/${work.item_id}/metadata.json`,
      size_bytes: ri(1, 20) * 1024,
      quality: null,
    });
  }
}

// ---------------------------------------------------------------- SSE 总线

export interface MockSseEvent {
  type: string;
  data: unknown;
}

const listeners = new Set<(e: MockSseEvent) => void>();

export function subscribeMockEvents(cb: (e: MockSseEvent) => void): () => void {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
}

function emit(type: string, data: unknown): void {
  for (const l of listeners) l({ type, data });
}

// ---------------------------------------------------------------- 调度器(下载 worker 池)

function sizeForQuality(quality: string): number {
  if (quality === "540p") return ri(6, 22) * MB;
  if (quality === "720p") return ri(12, 45) * MB;
  return ri(24, 90) * MB;
}

function jobJson(j: MockJob): Job {
  return {
    id: j.id,
    work_id: j.work_id,
    creator_id: j.creator_id,
    work_title: j.work_title,
    creator_nickname: j.creator_nickname,
    status: j.status,
    quality: j.quality,
    attempts: j.attempts,
    total_bytes: j.total_bytes,
    downloaded_bytes: j.downloaded_bytes,
    speed_bps: j.speed_bps,
    error: j.error,
    queued_at: j.queued_at,
    started_at: j.started_at,
    finished_at: j.finished_at,
  };
}

function emitStatus(j: MockJob): void {
  emit("download.status", { job_id: j.id, status: j.status, error: j.error, work_id: j.work_id });
}

const TICK_MS = 400;

function dispatcherTick(): void {
  const now = Date.now();
  const concurrency = state.settings.download_concurrency;

  if (!state.paused) {
    let active = state.jobs.filter((j) => j.status === "downloading").length;
    if (active < concurrency) {
      const queued = state.jobs.filter((j) => j.status === "queued").sort((a, b) => a.id - b.id);
      for (const j of queued) {
        if (active >= concurrency) break;
        j.status = "downloading";
        j.started_at = iso(now);
        j.speed_bps = ri(2, 9) * MB;
        j.fail_plan =
          j.attempts > 0 && rand() < 0.15
            ? { at_pct: ri(25, 85), error: pick(ERROR_POOL) }
            : null;
        active += 1;
        emitStatus(j);
      }
    }
  }

  for (const j of state.jobs) {
    if (j.status !== "downloading") continue;
    j.downloaded_bytes = Math.min(j.total_bytes, j.downloaded_bytes + Math.round(j.speed_bps * (TICK_MS / 1000)));
    if (now - j.last_emit >= 700 && j.total_bytes > 0) {
      j.last_emit = now;
      emit("download.progress", {
        job_id: j.id,
        downloaded_bytes: j.downloaded_bytes,
        total_bytes: j.total_bytes,
        speed_bps: j.speed_bps,
      });
    }
    const plan = j.fail_plan;
    const pct = j.total_bytes > 0 ? (j.downloaded_bytes / j.total_bytes) * 100 : 0;
    if (plan && pct >= plan.at_pct) {
      j.status = "failed";
      j.error = plan.error;
      j.finished_at = iso(now);
      j.speed_bps = 0;
      j.fail_plan = null;
      emitStatus(j);
    } else if (j.downloaded_bytes >= j.total_bytes) {
      j.status = "succeeded";
      j.error = null;
      j.finished_at = iso(now);
      j.downloaded_bytes = j.total_bytes;
      j.speed_bps = 0;
      const work = state.works.find((w) => w.id === j.work_id);
      if (work) createAssets(work, j.quality, j.total_bytes);
      emitStatus(j);
    }
  }
}

// ---------------------------------------------------------------- 扫描模拟

function worksCountOf(creatorId: number): number {
  return state.works.filter((w) => w.creator_id === creatorId).length;
}

function addWorks(creator: MockCreator, count: number): number {
  if (count <= 0) return 0;
  const baseIndex = worksCountOf(creator.id); // 图集规则按该博主作品序号判定
  const latest = state.works
    .filter((w) => w.creator_id === creator.id)
    .reduce<MockWork | null>((acc, w) => (!acc || w.published_at > acc.published_at ? w : acc), null);
  let t = latest ? new Date(latest.published_at).getTime() : Date.now();
  const now = Date.now();
  let added = 0;
  for (let i = 0; i < count; i++) {
    t += ri(2, 20) * 3600_000;
    if (t > now) t = now - ri(1, 40) * 60_000;
    const creatorCols = state.collections.filter((c) => c.creator_id === creator.id);
    const col = creatorCols.length > 0 && rand() < 0.4 ? pick(creatorCols) : null;
    const rule = imageRuleFor(baseIndex + i);
    const imageUrls = rule.isImage
      ? Array.from({ length: rule.imageCount }, (_, n) => imageSvg(creator.id * 1000 + baseIndex + i, n + 1, rule.imageCount))
      : [];
    state.works.push({
      id: ++state.ids.work,
      creator_id: creator.id,
      item_id: `v${(hashString(`${creator.sec_uid}:new:${now}:${i}`) >>> 0).toString(36)}${i}`,
      title: `${pick(TITLE_A)}${pick(TITLE_B)} · 新${added + 1}`,
      cover_url: rule.isImage ? imageUrls[0] : coverSvg((now % 9000) + i * 13),
      duration: rule.isImage ? 0 : Math.round(8 + rand() * rand() * 700),
      published_at: iso(t),
      collection_id: col ? col.id : null,
      created_at: iso(now),
      type: rule.isDaily ? "daily" : rule.isImage ? "image" : "video",
      image_count: rule.imageCount,
      live_count: rule.liveCount,
      image_urls: imageUrls,
    });
    added += 1;
  }
  return added;
}

function startScan(creatorId: number, full: boolean): number {
  const run: MockScanRun = {
    id: ++state.ids.scan,
    creator_id: creatorId,
    trigger: "manual",
    full,
    status: "running",
    pages: 0,
    new_count: 0,
    updated_count: 0,
    empty_pages: 0,
    completeness: 0,
    last_error: null,
    started_at: iso(Date.now()),
    finished_at: null,
  };
  state.scans.push(run);
  const creator = state.creators.find((c) => c.id === creatorId);
  if (!creator) return run.id;

  const existing = worksCountOf(creatorId);
  const pending = Math.max(0, creator.reported_work_count + (full ? ri(0, 4) : ri(0, 6)) - existing);
  const pagesBudget = Math.max(2, Math.ceil(pending / 22) + (full ? ri(1, 2) : state.settings.incremental_stop_pages));
  const perPage = Math.max(1, Math.ceil(pending / pagesBudget));
  let emittedNew = 0;

  const timer = setInterval(() => {
    if (run.status !== "running") {
      clearInterval(timer);
      return;
    }
    run.pages += 1;
    if (emittedNew < pending) {
      const add = Math.min(perPage, pending - emittedNew);
      const got = addWorks(creator, add);
      emittedNew += got;
      run.new_count += got;
      if (rand() < 0.3) run.updated_count += ri(1, 3);
    } else {
      run.empty_pages += 1;
    }
    emit("scan.progress", {
      scan_id: run.id,
      creator_id: creatorId,
      page: run.pages,
      new_count: run.new_count,
      updated_count: run.updated_count,
      status: "running",
    });
    const doneByPending = emittedNew >= pending && run.pages >= pagesBudget;
    const doneByEmpty = run.empty_pages >= state.settings.incremental_stop_pages && emittedNew >= pending;
    if (doneByPending || doneByEmpty || run.pages >= 30) {
      run.status = rand() < 0.08 ? "partial" : "succeeded";
      run.completeness = creator.reported_work_count
        ? Math.min(100, Math.round((worksCountOf(creatorId) / creator.reported_work_count) * 100))
        : 0;
      run.finished_at = iso(Date.now());
      clearInterval(timer);
      emit("scan.done", {
        scan_id: run.id,
        creator_id: creatorId,
        status: run.status,
        pages: run.pages,
        new_count: run.new_count,
        completeness: run.completeness,
        last_error: run.status === "partial" ? "部分页面返回异常,已跳过" : null,
      });
    }
  }, 800);
  return run.id;
}

// ---------------------------------------------------------------- 序列化

function mask(v: string): string {
  return v ? v.slice(0, 8) + "..." : "";
}

function settingsJson(): Settings {
  const s = state.settings;
  return {
    ...s,
    cookie: mask(s.cookie),
    minio: { ...s.minio, secret_key: s.minio.secret_key ? mask(s.minio.secret_key) : "", secret_set: Boolean(s.minio.secret_key) },
    smtp: { ...s.smtp, password: mask(s.smtp.password) },
  };
}

function latestJobByWork(): Map<number, MockJob> {
  const map = new Map<number, MockJob>();
  for (const j of state.jobs) {
    const cur = map.get(j.work_id);
    if (!cur || j.id > cur.id) map.set(j.work_id, j);
  }
  return map;
}

function dlStatusOf(j: MockJob | undefined): DLStatus {
  if (!j) return "none";
  if (j.status === "paused_q") return "queued";
  return j.status;
}

function workJson(w: MockWork, latest: Map<number, MockJob>): Work {
  const j = latest.get(w.id);
  const col = w.collection_id ? state.collections.find((c) => c.id === w.collection_id) : undefined;
  return {
    id: w.id,
    item_id: w.item_id,
    title: w.title,
    cover_url: w.cover_url,
    duration: w.duration,
    published_at: w.published_at,
    collection_id: w.collection_id,
    collection_name: col ? col.name : null,
    type: w.type,
    image_count: w.image_count,
    dl_status: dlStatusOf(j),
    downloaded_quality: j && j.status === "succeeded" ? j.quality : null,
    created_at: w.created_at,
  };
}

/** 该博主全部已下载资产的字节总和(契约 v1.2:SUM assets.size_bytes,kind video+image)。 */
function downloadBytesOf(creatorId: number): number {
  const workIds = new Set(state.works.filter((w) => w.creator_id === creatorId).map((w) => w.id));
  let sum = 0;
  for (const a of state.assets) {
    if (workIds.has(a.work_id) && (a.kind === "video" || a.kind === "image")) sum += a.size_bytes;
  }
  return sum;
}

function creatorJson(c: MockCreator): Creator {
  const works = state.works.filter((w) => w.creator_id === c.id);
  const latest = latestJobByWork();
  const downloaded = works.filter((w) => dlStatusOf(latest.get(w.id)) === "succeeded").length;
  return {
    id: c.id,
    sec_uid: c.sec_uid,
    nickname: c.nickname,
    alias: c.alias,
    group: c.group_name,
    avatar_url: c.avatar_url,
    profile_url: c.profile_url,
    reported_work_count: c.reported_work_count,
    works_count: works.length,
    downloaded_count: downloaded,
    download_bytes: downloadBytesOf(c.id),
    download_root: c.download_root,
    created_at: c.created_at,
  };
}

function scanRunJson(s: MockScanRun): ScanRun {
  return {
    id: s.id,
    status: s.status,
    pages: s.pages,
    new_count: s.new_count,
    updated_count: s.updated_count,
    empty_pages: s.empty_pages,
    completeness: s.completeness,
    started_at: s.started_at,
    finished_at: s.finished_at,
    last_error: s.last_error,
  };
}

function collectionJson(c: MockCollection): Collection {
  const works = state.works.filter((w) => w.collection_id === c.id);
  const latest = latestJobByWork();
  return {
    id: c.id,
    mix_id: c.mix_id,
    name: c.name,
    cover_url: c.cover_url,
    works_count: works.length,
    downloaded_count: works.filter((w) => dlStatusOf(latest.get(w.id)) === "succeeded").length,
  };
}

/**
 * 监控期间统计(与后端 subscriptionListQuery 同口径):
 * created_at >= 订阅创建点的作品,creator target 统计整主页、collection target 限定合集;
 * 已下载 = 其中最新任务 succeeded(dl_status 推导一致)。
 */
function monitorStatsOf(s: MockSubscription): { newWorks: number; newDownloaded: number } {
  const latest = latestJobByWork();
  let newWorks = 0;
  let newDownloaded = 0;
  for (const w of state.works) {
    if (w.creator_id !== s.creator_id) continue;
    if (w.created_at < s.created_at) continue;
    if (s.collection_id !== null && w.collection_id !== s.collection_id) continue;
    newWorks += 1;
    if (dlStatusOf(latest.get(w.id)) === "succeeded") newDownloaded += 1;
  }
  return { newWorks, newDownloaded };
}

/** 监控期间新增作品明细(订阅 new-works 端点):与 monitorStatsOf 同一窗口,按发布时间倒序。 */
function monitorWorksOf(s: MockSubscription): Work[] {
  const latest = latestJobByWork();
  return state.works
    .filter(
      (w) =>
        w.creator_id === s.creator_id &&
        w.created_at >= s.created_at &&
        (s.collection_id === null || w.collection_id === s.collection_id),
    )
    .slice()
    .sort((a, b2) => b2.published_at.localeCompare(a.published_at))
    .map((w) => workJson(w, latest));
}

function subscriptionJson(s: MockSubscription): Subscription {
  const creator = state.creators.find((c) => c.id === s.creator_id);
  const collection = s.collection_id ? state.collections.find((c) => c.id === s.collection_id) : undefined;
  const lastRun = s.last_run_at ? new Date(s.last_run_at).getTime() : null;
  const stats = monitorStatsOf(s);
  return {
    id: s.id,
    target_type: s.target_type,
    creator_id: s.creator_id,
    collection_id: s.collection_id,
    creator_nickname: creator ? creator.nickname : `博主 ${s.creator_id}`,
    target_name: collection ? collection.name : creator ? creator.nickname : `目标 ${s.id}`,
    interval_minutes: s.interval_minutes,
    auto_download: s.auto_download,
    quality: s.quality,
    last_run_at: s.last_run_at,
    next_run_at: s.enabled && lastRun ? iso(lastRun + s.interval_minutes * 60_000) : null,
    enabled: s.enabled,
    new_works: stats.newWorks,
    new_downloaded: stats.newDownloaded,
  };
}

function assetJson(a: MockAsset): Asset {
  return { id: a.id, work_id: a.work_id, kind: a.kind, path: a.path, size_bytes: a.size_bytes, quality: a.quality };
}

function summaryJson(): DownloadsSummary {
  const count = (s: JobStatus) => state.jobs.filter((j) => j.status === s).length;
  return {
    queued: count("queued"),
    downloading: count("downloading"),
    failed: count("failed"),
    succeeded: count("succeeded"),
    canceled: count("canceled"),
    paused: state.paused,
    concurrency: state.settings.download_concurrency,
  };
}

// ---------------------------------------------------------------- 路由

export interface MockRequest {
  method: string;
  path: string; // 已去掉 /api 前缀,如 "/creators/3/works"
  query: URLSearchParams;
  body: unknown;
  session: string | null;
}

export interface MockResponse {
  status: number;
  json: unknown;
  setSession?: string;
  clearSession?: boolean;
}

function asRecord(v: unknown): Record<string, unknown> {
  return (v && typeof v === "object" ? v : {}) as Record<string, unknown>;
}
function num(v: unknown, fallback: number): number {
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : fallback;
}
function optNum(v: unknown): number | undefined {
  if (v === undefined || v === null || v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}
function str(v: unknown, fallback = ""): string {
  return typeof v === "string" ? v : fallback;
}
function bool(v: unknown, fallback = false): boolean {
  return typeof v === "boolean" ? v : fallback;
}
function numArray(v: unknown): number[] {
  return Array.isArray(v) ? v.map((x) => Number(x)).filter((x) => Number.isFinite(x)) : [];
}

/** 契约 v1.3:下载根目录须为绝对路径(Windows 盘符或 POSIX 根)。 */
function isAbsolutePath(p: string): boolean {
  return /^[a-zA-Z]:[\\/]/.test(p) || p.startsWith("/");
}

const ok = (json: unknown): MockResponse => ({ status: 200, json });
const err = (status: number, detail: string): MockResponse => ({ status, json: { detail } });

/**
 * works 列表契约筛选(契约 v1.2):
 * - type:video=视频,image=图集,live=含动图/实况片段的图集(EXISTS live 资产 → live_count>0)
 * - dl:按 dl_status(最新一条任务推导)
 */
function matchesTypeDl(w: MockWork, type: string, dl: string, latest: Map<number, MockJob>): boolean {
  if (type === "video" && w.type !== "video") return false;
  if (type === "image" && w.type !== "image") return false;
  if (type === "live" && !(w.type === "image" && w.live_count > 0)) return false;
  if (dl && dlStatusOf(latest.get(w.id)) !== dl) return false;
  return true;
}

function listWorks(creatorId: number, query: URLSearchParams, forceCollectionId?: number): Page<Work> {
  const page = Math.max(1, optNum(query.get("page")) ?? 1);
  const pageSize = Math.min(100, Math.max(1, optNum(query.get("page_size")) ?? 20));
  const q = (query.get("q") ?? "").trim().toLowerCase();
  const cid = forceCollectionId ?? optNum(query.get("collection_id"));
  // 契约 v1.4b:多选排除合集(逗号分隔);仅博主作品列表支持,exclude 优先于 cid
  const excludeRaw = forceCollectionId === undefined ? (query.get("exclude_collection_ids") ?? "") : "";
  const excludeIds = excludeRaw
    .split(",")
    .map((s) => optNum(s.trim()))
    .filter((n): n is number => n !== undefined && n > 0);
  const type = query.get("type") ?? "";
  const dl = query.get("dl") ?? "";
  const sort = query.get("sort") ?? "published_at_desc";
  const latest = latestJobByWork();

  let items = state.works.filter((w) => w.creator_id === creatorId);
  if (excludeIds.length > 0) {
    items = items.filter((w) => w.collection_id === null || !excludeIds.includes(w.collection_id));
  } else if (cid !== undefined) {
    items = items.filter((w) => w.collection_id === cid);
  }
  if (q) items = items.filter((w) => w.title.toLowerCase().includes(q) || w.item_id.toLowerCase().includes(q));
  if (type || dl) items = items.filter((w) => matchesTypeDl(w, type, dl, latest));
  items = items.slice().sort((a, b) => {
    if (sort === "published_at_asc") return a.published_at.localeCompare(b.published_at);
    if (sort === "duration_desc") return b.duration - a.duration;
    return b.published_at.localeCompare(a.published_at);
  });

  const total = items.length;
  const start = (page - 1) * pageSize;
  return {
    items: items.slice(start, start + pageSize).map((w) => workJson(w, latest)),
    total,
    page,
    page_size: pageSize,
  };
}

function retryJob(job: MockJob, quality: string | undefined): void {
  if (quality) job.quality = quality;
  job.status = "queued";
  job.attempts += 1;
  job.downloaded_bytes = 0;
  job.speed_bps = 0;
  job.error = null;
  job.started_at = null;
  job.finished_at = null;
  job.queued_at = iso(Date.now());
  job.fail_plan = null;
  emitStatus(job);
}

/** 契约 v1.4:可选文本字段 — null/空串 = 清除(null),有值 = 设置。 */
function optionalTextOf(v: unknown): string | null {
  if (typeof v !== "string") return null;
  const s = v.trim();
  return s === "" ? null : s;
}

/** 契约 v1.4b:创建/刷新 creator 级订阅(subscribe 对象语义)。 */
function upsertCreatorSubscription(creatorId: number, intervalMinutes: number, quality: string, autoDownload: boolean): void {
  const existing = state.subs.find(
    (s) => s.target_type === "creator" && s.creator_id === creatorId && s.collection_id === null,
  );
  if (existing) {
    existing.interval_minutes = intervalMinutes;
    existing.quality = quality;
    existing.auto_download = autoDownload;
    existing.enabled = true;
    return;
  }
  state.subs.push({
    id: ++state.ids.sub,
    target_type: "creator",
    creator_id: creatorId,
    collection_id: null,
    interval_minutes: intervalMinutes,
    auto_download: autoDownload,
    quality,
    enabled: true,
    last_run_at: null,
    created_at: iso(Date.now()),
  });
}

export function mockRoute(req: MockRequest): MockResponse {
  const seg = req.path.replace(/^\/+|\/+$/g, "").split("/").filter(Boolean);
  const method = req.method;
  const b = asRecord(req.body);
  const authed = req.session !== null && state.sessions.has(req.session);
  const now = Date.now();

  // ----- 公开端点 -----
  if (seg[0] === "health") return ok({ status: "ok", provider: "mock", sidecar: "running", real_scan_ready: false, cookie_blocked: false, blocked_reason: null, blocked_since: null });

  if (seg[0] === "auth") {
    if (seg[1] === "status" && method === "GET") {
      return ok({
        authenticated: authed,
        initialized: state.initialized,
        ...(authed ? { username: state.sessions.get(req.session!) } : {}),
      });
    }
    if (seg[1] === "setup" && method === "POST") {
      if (state.initialized) return err(400, "管理员已初始化,请直接登录");
      const username = str(b.username).trim();
      const password = str(b.password);
      if (username.length < 2) return err(400, "用户名至少 2 个字符");
      if (password.length < 8) return err(400, "密码至少 8 位");
      state.username = username;
      state.password = password;
      state.initialized = true;
      const token = `mock_${Math.random().toString(36).slice(2)}${now.toString(36)}`;
      state.sessions.set(token, username);
      return { status: 200, json: { ok: true, username }, setSession: token };
    }
    if (seg[1] === "login" && method === "POST") {
      const username = str(b.username).trim();
      const password = str(b.password);
      if (!state.initialized) return err(400, "请先完成初始化");
      if (username !== state.username || password !== state.password) {
        state.loginFails += 1;
        return err(401, state.loginFails >= 3 ? "用户名或密码错误(连续失败已限流)" : "用户名或密码错误");
      }
      state.loginFails = 0;
      const token = `mock_${Math.random().toString(36).slice(2)}${now.toString(36)}`;
      state.sessions.set(token, username);
      return { status: 200, json: { ok: true, username }, setSession: token };
    }
    if (seg[1] === "logout" && method === "POST") {
      if (req.session) state.sessions.delete(req.session);
      return { status: 200, json: { ok: true }, clearSession: true };
    }
    if (seg[1] === "password" && method === "POST") {
      if (!authed) return err(401, "unauthorized");
      const oldPassword = str(b.old_password);
      const newPassword = str(b.new_password);
      if (oldPassword !== state.password) return err(400, "当前密码不正确");
      if (newPassword.length < 8) return err(400, "新密码至少 8 位");
      state.password = newPassword;
      return ok({ ok: true });
    }
    return err(404, "not found");
  }

  // ----- 认证门 -----
  if (!authed) return err(401, "unauthorized");

  if (seg[0] === "creators") {
    if (seg.length === 1) {
      if (method === "GET") {
        const list = state.creators
          .slice()
          .sort((a, b2) => b2.created_at.localeCompare(a.created_at))
          .map(creatorJson);
        return ok(list);
      }
      if (method === "POST") {
        const profileUrl = str(b.profile_url).trim();
        if (!profileUrl) return err(400, "profile_url 不能为空");
        const secUid = extractSecUid(profileUrl);
        // 契约 v1.4/1.4b 可选参数(重复添加同样生效;缺省字段保持原值)
        const alias = b.alias === undefined ? undefined : optionalTextOf(b.alias);
        const group = b.group === undefined ? undefined : optionalTextOf(b.group);
        let downloadRoot: string | null | undefined;
        if (b.download_root !== undefined && b.download_root !== null) {
          const p = str(b.download_root).trim();
          if (p) {
            if (!isAbsolutePath(p)) return err(400, "download_root 必须为绝对路径,如 D:\\Media\\Douyin");
            downloadRoot = p;
          } else {
            downloadRoot = null;
          }
        }
        const subscribe = asRecord(b.subscribe);
        let applySubscribe: { interval: number; quality: string; autoDownload: boolean } | null = null;
        if (b.subscribe !== undefined && b.subscribe !== null) {
          const interval = Math.max(1, num(subscribe.interval_minutes, 0));
          if (interval < 1) return err(400, "subscribe.interval_minutes 必填且 >= 1");
          const quality = str(subscribe.quality, state.settings.download_quality) || "1080p";
          const autoDownload = subscribe.auto_download === undefined ? true : bool(subscribe.auto_download, true);
          applySubscribe = { interval, quality, autoDownload };
        }
        const existing = state.creators.find((c) => c.sec_uid === secUid || c.profile_url === profileUrl);
        if (existing) {
          if (alias !== undefined) existing.alias = alias;
          if (group !== undefined) existing.group_name = group;
          if (downloadRoot !== undefined) existing.download_root = downloadRoot;
          if (applySubscribe) upsertCreatorSubscription(existing.id, applySubscribe.interval, applySubscribe.quality, applySubscribe.autoDownload);
          const scanId = startScan(existing.id, false);
          return { status: 202, json: { creator_id: existing.id, scan_id: scanId, creator: creatorJson(existing) } };
        }
        const nickname = `新博主·${pick(NICK_POOL)}`;
        const creator: MockCreator = {
          id: ++state.ids.creator,
          sec_uid: secUid,
          nickname,
          alias: alias ?? null,
          group_name: group ?? null,
          avatar_url: avatarSvg(nickname[0], (hashString(nickname) % 300) + 20),
          profile_url: profileUrl,
          reported_work_count: ri(60, 420),
          download_root: downloadRoot ?? null,
          created_at: iso(now),
        };
        state.creators.push(creator);
        if (applySubscribe) upsertCreatorSubscription(creator.id, applySubscribe.interval, applySubscribe.quality, applySubscribe.autoDownload);
        const scanId = startScan(creator.id, true);
        return { status: 202, json: { creator_id: creator.id, scan_id: scanId, creator: creatorJson(creator) } };
      }
      return err(405, "method not allowed");
    }

    const creatorId = Number(seg[1]);
    const creator = state.creators.find((c) => c.id === creatorId);
    if (!creator) return err(404, "博主不存在");

    if (seg.length === 2) {
      if (method === "GET") {
        const last = state.scans
          .filter((s) => s.creator_id === creatorId)
          .sort((a, b2) => b2.id - a.id)[0];
        const detail: CreatorDetail = { ...creatorJson(creator), last_scan: last ? scanRunJson(last) : null };
        return ok(detail);
      }
      if (method === "PATCH") {
        // 契约 v1.4:{alias?, group?};null/空串 = 清除 → {ok, alias, group}
        if (b.alias !== undefined) creator.alias = optionalTextOf(b.alias);
        if (b.group !== undefined) creator.group_name = optionalTextOf(b.group);
        return ok({ ok: true, alias: creator.alias, group: creator.group_name });
      }
      if (method === "DELETE") {
        const removedWorkIds = new Set(
          state.works.filter((w) => w.creator_id === creatorId).map((w) => w.id),
        );
        state.works = state.works.filter((w) => w.creator_id !== creatorId);
        state.jobs = state.jobs.filter((j) => j.creator_id !== creatorId);
        const colIds = state.collections.filter((c) => c.creator_id === creatorId).map((c) => c.id);
        state.collections = state.collections.filter((c) => c.creator_id !== creatorId);
        state.assets = state.assets.filter((a) => !removedWorkIds.has(a.work_id));
        state.scans = state.scans.filter((s) => s.creator_id !== creatorId);
        state.subs = state.subs.filter(
          (s) => s.creator_id !== creatorId && !(s.collection_id && colIds.includes(s.collection_id)),
        );
        state.creators = state.creators.filter((c) => c.id !== creatorId);
        return ok({ ok: true });
      }
      return err(405, "method not allowed");
    }

    if (seg[2] === "download-root" && method === "PATCH") {
      // 契约 v1.3:{path: string|null};null = 清除独立设置跟随全局;须为绝对路径,后端自动建目录(mock 仅改内存)
      if (b.path === null || b.path === undefined) {
        creator.download_root = null;
      } else {
        const p = str(b.path).trim();
        if (!p) return err(400, "path 不能为空(清除独立设置请传 null)");
        if (!isAbsolutePath(p)) return err(400, "path 必须为绝对路径,如 D:\\Media\\Douyin");
        creator.download_root = p;
      }
      return ok({ ok: true, download_root: creator.download_root });
    }
    if (seg[2] === "move-downloads" && method === "POST") {
      // 契约 v1.3:{target_root} 把已下载文件整体搬到新根;存在 downloading 任务时 409。
      // mock 不触达文件系统:改写内存中的资产路径前缀并返回统计。
      const targetRoot = str(b.target_root).trim();
      if (!targetRoot) return err(400, "target_root 不能为空");
      if (!isAbsolutePath(targetRoot)) return err(400, "target_root 必须为绝对路径,如 D:\\Media\\Douyin");
      // 排队中/下载中的任务都会往旧目录写文件,均视为忙碌(契约对 downloading 明确 409)
      const hasDownloading = state.jobs.some(
        (j) => j.creator_id === creatorId && (j.status === "downloading" || j.status === "queued"),
      );
      if (hasDownloading) return err(409, "该博主存在下载中的任务,请稍后再试");
      const workIds = new Set(state.works.filter((w) => w.creator_id === creatorId).map((w) => w.id));
      let movedFiles = 0;
      let movedBytes = 0;
      for (const a of state.assets) {
        if (!workIds.has(a.work_id)) continue;
        movedFiles += 1;
        movedBytes += a.size_bytes;
        if (a.path.startsWith("downloads/")) {
          a.path = targetRoot.replace(/[\\/]+$/, "") + a.path.slice("downloads".length);
        }
      }
      return ok({ moved_files: movedFiles, moved_bytes: movedBytes, skipped_files: 0, failed_files: [] });
    }
    if (seg[2] === "rescan" && method === "POST") {
      const scanId = startScan(creatorId, bool(b.full, false));
      return { status: 202, json: { scan_id: scanId } };
    }
    if (seg[2] === "collections" && method === "GET") {
      return ok(
        state.collections
          .filter((c) => c.creator_id === creatorId)
          .sort((a, b2) => a.id - b2.id)
          .map(collectionJson),
      );
    }
    if (seg[2] === "works" && method === "GET") {
      return ok(listWorks(creatorId, req.query));
    }
    return err(404, "not found");
  }

  if (seg[0] === "works") {
    // /works/batch-ids → seg = ["works","batch-ids"]
    if (seg[1] === "batch-ids" && method === "POST") {
      const creatorId = num(b.creator_id, 0);
      const q = str(b.q).trim().toLowerCase();
      const cid = optNum(b.collection_id);
      // 契约 v1.4b:exclude 为 JSON 数组,与列表语义一致(exclude 优先)
      const excludeIds = Array.isArray(b.exclude_collection_ids)
        ? b.exclude_collection_ids.map((x) => Number(x)).filter((x) => Number.isFinite(x) && x > 0)
        : [];
      const type = str(b.type);
      const dl = str(b.dl);
      const latest = latestJobByWork();
      let pool = state.works.filter((w) => w.creator_id === creatorId);
      if (excludeIds.length > 0) {
        pool = pool.filter((w) => w.collection_id === null || !excludeIds.includes(w.collection_id));
      }
      const ids = pool
        .filter((w) => (cid !== undefined && excludeIds.length === 0 ? w.collection_id === cid : true))
        .filter((w) => (q ? w.title.toLowerCase().includes(q) || w.item_id.toLowerCase().includes(q) : true))
        .filter((w) => matchesTypeDl(w, type, dl, latest))
        .map((w) => w.id);
      return ok({ ids });
    }
    if (seg[2] === "qualities" && method === "GET") {
      const workId = Number(seg[1]);
      const work = state.works.find((w) => w.id === workId);
      if (!work) return err(404, "作品不存在");
      const base = ri(30, 120) * MB;
      const options: QualityOption[] = [
        { quality: "1080p", width: 1920, height: 1080, bitrate: 2_600_000, size_bytes: base },
        { quality: "720p", width: 1280, height: 720, bitrate: 1_200_000, size_bytes: Math.round(base * 0.46) },
        { quality: "540p", width: 960, height: 540, bitrate: 700_000, size_bytes: Math.round(base * 0.24) },
      ];
      return ok(options);
    }
    if (seg[2] === "assets" && method === "GET") {
      const workId = Number(seg[1]);
      // 契约 v1.1 排序:video(最新在前)→ image(quality 4 位序号升序)→ cover → metadata
      const kindRank: Record<AssetKind, number> = { video: 0, image: 1, cover: 2, metadata: 3 };
      const list = state.assets
        .filter((a) => a.work_id === workId)
        .sort((a, b2) => {
          const r = kindRank[a.kind] - kindRank[b2.kind];
          if (r !== 0) return r;
          if (a.kind === "video") return b2.id - a.id;
          if (a.kind === "image") return (a.quality ?? "").localeCompare(b2.quality ?? "");
          return a.id - b2.id;
        })
        .map(assetJson);
      return ok(list);
    }
    if (seg.length === 2 && method === "GET") {
      const workId = Number(seg[1]);
      const work = state.works.find((w) => w.id === workId);
      if (!work) return err(404, "作品不存在");
      const latest = latestJobByWork();
      const col = work.collection_id ? state.collections.find((c) => c.id === work.collection_id) : undefined;
      const detail: WorkDetail = {
        ...workJson(work, latest),
        mix_info: col
          ? { collection_id: col.id, mix_id: col.mix_id, name: col.name }
          : null,
        asset: state.assets.filter((a) => a.work_id === workId).map(assetJson),
        last_job: latest.get(workId) ? jobJson(latest.get(workId)!) : null,
      };
      return ok(detail);
    }
    return err(404, "not found");
  }

  if (seg[0] === "collections" && seg[2] === "works" && method === "GET") {
    const col = state.collections.find((c) => c.id === Number(seg[1]));
    if (!col) return err(404, "合集不存在");
    return ok(listWorks(col.creator_id, req.query, col.id));
  }

  if (seg[0] === "assets" && seg[2] === "content") {
    return err(404, "mock 模式不提供资产二进制内容(播放器使用演示视频源)");
  }

  if (seg[0] === "downloads") {
    if (seg.length === 1) {
      if (method === "GET") {
        const statusParam = req.query.get("status");
        const statuses = statusParam
          ? (statusParam.split(",").filter((s) => (JOB_STATUSES as readonly string[]).includes(s)) as JobStatus[])
          : null;
        let jobs = state.jobs.slice().sort((a, b2) => b2.id - a.id);
        if (statuses && statuses.length) jobs = jobs.filter((j) => statuses.includes(j.status));
        const cursor = optNum(req.query.get("cursor"));
        if (cursor !== undefined) jobs = jobs.filter((j) => j.id < cursor);
        const limit = Math.min(100, Math.max(1, optNum(req.query.get("limit")) ?? 50));
        const items = jobs.slice(0, limit).map(jobJson);
        const page: CursorPage<Job> = {
          items,
          next_cursor: jobs.length > limit && items.length > 0 ? items[items.length - 1].id : null,
        };
        return ok(page);
      }
      if (method === "POST") {
        const workIds = numArray(b.work_ids);
        const quality = str(b.quality, state.settings.download_quality);
        const created: number[] = [];
        const skipped: { work_id: number; reason: string }[] = [];
        const latest = latestJobByWork();
        for (const workId of workIds) {
          const work = state.works.find((w) => w.id === workId);
          if (!work) {
            skipped.push({ work_id: workId, reason: "作品不存在" });
            continue;
          }
          const active = state.jobs.find(
            (j) => j.work_id === workId && (j.status === "queued" || j.status === "downloading"),
          );
          if (active) {
            skipped.push({ work_id: workId, reason: "已在下载队列中" });
            continue;
          }
          const last = latest.get(workId);
          if (last && last.status === "succeeded" && last.quality === quality) {
            skipped.push({ work_id: workId, reason: `已以 ${quality} 下载` });
            continue;
          }
          const job: MockJob = {
            id: ++state.ids.job,
            work_id: workId,
            creator_id: work.creator_id,
            work_title: work.title,
            creator_nickname: state.creators.find((c) => c.id === work.creator_id)?.nickname ?? "",
            status: "queued",
            quality,
            attempts: 0,
            total_bytes: sizeForQuality(quality),
            downloaded_bytes: 0,
            speed_bps: 0,
            error: null,
            queued_at: iso(now),
            started_at: null,
            finished_at: null,
            fail_plan: null,
            last_emit: 0,
          };
          state.jobs.push(job);
          created.push(job.id);
          emitStatus(job);
        }
        return { status: 202, json: { created, skipped } satisfies CreateDownloadsResult };
      }
      return err(405, "method not allowed");
    }

    if (seg[1] === "summary" && method === "GET") return ok(summaryJson());

    if (seg[1] === "batch" && method === "POST") {
      const action = str(b.action);
      const ids = numArray(b.ids);
      const quality = str(b.quality) || undefined;
      let affected = 0;
      for (const id of ids) {
        const job = state.jobs.find((j) => j.id === id);
        if (!job) continue;
        if (action === "retry" && !["queued", "downloading"].includes(job.status)) {
          retryJob(job, quality);
          affected += 1;
        } else if (action === "cancel" && ["queued", "downloading"].includes(job.status)) {
          job.status = "canceled";
          job.finished_at = iso(now);
          job.fail_plan = null;
          emitStatus(job);
          affected += 1;
        } else if (action === "delete" && !["queued", "downloading"].includes(job.status)) {
          state.jobs = state.jobs.filter((j) => j.id !== id);
          affected += 1;
        }
      }
      const result: BatchActionResult = { affected };
      return ok(result);
    }

    if (seg[1] === "retry-failed" && method === "POST") {
      let affected = 0;
      for (const job of state.jobs.filter((j) => j.status === "failed" && j.attempts < 5)) {
        retryJob(job, undefined);
        affected += 1;
      }
      return ok({ affected });
    }

    if (seg[1] === "clear-completed" && method === "POST") {
      const before = state.jobs.length;
      state.jobs = state.jobs.filter((j) => j.status !== "succeeded" && j.status !== "canceled");
      return ok({ affected: before - state.jobs.length });
    }

    if (seg[1] === "queue" && method === "POST") {
      if (seg[2] === "pause") {
        state.paused = true;
        return ok(summaryJson());
      }
      if (seg[2] === "resume") {
        state.paused = false;
        if (bool(b.retry_failed, false)) {
          for (const job of state.jobs.filter((j) => j.status === "failed" && j.attempts < 5)) retryJob(job, undefined);
        }
        return ok(summaryJson());
      }
      return err(404, "not found");
    }

    const jobId = Number(seg[1]);
    const job = state.jobs.find((j) => j.id === jobId);
    if (!job) return err(404, "任务不存在");
    if (seg[2] === "retry" && method === "POST") {
      if (["queued", "downloading"].includes(job.status)) return err(409, "任务正在进行中,无需重试");
      retryJob(job, str(b.quality) || undefined);
      return ok(jobJson(job));
    }
    if (seg[2] === "cancel" && method === "POST") {
      if (!["queued", "downloading"].includes(job.status)) return err(409, "任务已结束,无法取消");
      job.status = "canceled";
      job.finished_at = iso(now);
      job.fail_plan = null;
      emitStatus(job);
      return ok(jobJson(job));
    }
    if (seg.length === 2 && method === "DELETE") {
      if (["queued", "downloading"].includes(job.status)) return err(409, "进行中的任务不能删除,请先取消");
      state.jobs = state.jobs.filter((j) => j.id !== jobId);
      return ok({ ok: true });
    }
    return err(404, "not found");
  }

  if (seg[0] === "subscriptions") {
    if (seg.length === 1) {
      if (method === "GET") {
        return ok(
          state.subs
            .slice()
            .sort((a, b2) => b2.id - a.id)
            .map(subscriptionJson),
        );
      }
      if (method === "POST") {
        const targetType = str(b.target_type) as TargetType;
        const creatorId = num(b.creator_id, 0);
        const collectionId = optNum(b.collection_id);
        const creator = state.creators.find((c) => c.id === creatorId);
        if (!creator) return err(400, "博主不存在");
        if (targetType === "collection") {
          const col = state.collections.find((c) => c.id === collectionId && c.creator_id === creatorId);
          if (!col) return err(400, "合集不存在或不属于该博主");
        }
        const sub: MockSubscription = {
          id: ++state.ids.sub,
          target_type: targetType,
          creator_id: creatorId,
          collection_id: targetType === "collection" ? collectionId! : null,
          interval_minutes: Math.min(10080, Math.max(1, num(b.interval_minutes, 20))),
          auto_download: bool(b.auto_download, true),
          quality: str(b.quality, state.settings.download_quality),
          enabled: true,
          last_run_at: null,
          created_at: iso(now),
        };
        state.subs.push(sub);
        return { status: 201, json: subscriptionJson(sub) };
      }
      return err(405, "method not allowed");
    }
    const sub = state.subs.find((s) => s.id === Number(seg[1]));
    if (!sub) return err(404, "订阅不存在");
    if (seg[2] === "new-works" && method === "GET") {
      // 监控期间新增作品明细(与列表统计同口径,含分页外全量)
      const items = monitorWorksOf(sub);
      return ok({
        items,
        total: items.length,
        downloaded: items.filter((w) => w.dl_status === "succeeded").length,
      });
    }
    if (method === "PATCH") {
      if (b.interval_minutes !== undefined) sub.interval_minutes = Math.min(10080, Math.max(1, num(b.interval_minutes, sub.interval_minutes)));
      if (b.auto_download !== undefined) sub.auto_download = bool(b.auto_download, sub.auto_download);
      if (b.enabled !== undefined) sub.enabled = bool(b.enabled, sub.enabled);
      if (b.quality !== undefined) sub.quality = str(b.quality, sub.quality);
      return ok(subscriptionJson(sub));
    }
    if (method === "DELETE") {
      state.subs = state.subs.filter((s) => s.id !== sub.id);
      return ok({ ok: true });
    }
    return err(405, "method not allowed");
  }

  if (seg[0] === "settings") {
    if (method === "GET") return ok(settingsJson());
    if (method === "PATCH") {
      const s = state.settings;
      if (b.provider_mode !== undefined) s.provider_mode = str(b.provider_mode, s.provider_mode) as MockSettings["provider_mode"];
      if (typeof b.cookie === "string" && b.cookie.length > 0) s.cookie = b.cookie;
      if (typeof b.download_root === "string") {
        // 契约 v1.3:绝对路径,空 = 默认 <data_dir>/downloads;后端保存时自动建目录(mock 仅存内存)
        const root = b.download_root.trim();
        if (root && !isAbsolutePath(root)) return err(400, "download_root 必须为绝对路径,如 D:\\Media\\Douyin");
        s.download_root = root;
      }
      if (b.download_concurrency !== undefined) s.download_concurrency = Math.min(8, Math.max(1, num(b.download_concurrency, s.download_concurrency)));
      if (b.download_quality !== undefined) s.download_quality = str(b.download_quality, s.download_quality);
      if (b.scan_page_delay_ms !== undefined) s.scan_page_delay_ms = Math.min(10000, Math.max(1000, num(b.scan_page_delay_ms, s.scan_page_delay_ms)));
      if (b.scan_max_empty_pages !== undefined) s.scan_max_empty_pages = Math.min(10, Math.max(1, num(b.scan_max_empty_pages, s.scan_max_empty_pages)));
      if (b.scan_concurrency !== undefined) s.scan_concurrency = Math.min(5, Math.max(1, num(b.scan_concurrency, s.scan_concurrency)));
      if (b.incremental_stop_pages !== undefined) s.incremental_stop_pages = Math.min(10, Math.max(1, num(b.incremental_stop_pages, s.incremental_stop_pages)));
      if (b.completeness_gap_threshold !== undefined) s.completeness_gap_threshold = Math.min(50, Math.max(1, num(b.completeness_gap_threshold, s.completeness_gap_threshold)));
      if (b.sidecar_idle_timeout_minutes !== undefined) s.sidecar_idle_timeout_minutes = Math.max(1, num(b.sidecar_idle_timeout_minutes, s.sidecar_idle_timeout_minutes));
      if (b.notify_on_new_work !== undefined) s.notify_on_new_work = bool(b.notify_on_new_work, s.notify_on_new_work);
      if (b.notify_on_failure !== undefined) s.notify_on_failure = bool(b.notify_on_failure, s.notify_on_failure);
      if (b.minio !== undefined && b.minio !== null && typeof b.minio === "object") {
        const m = b.minio as Record<string, unknown>;
        s.minio = {
          ...s.minio, ...m,
          secret_key:
            m.secret_key !== undefined && m.secret_key !== "" ? String(m.secret_key) : s.minio.secret_key,
        };
      }
      if (b.smtp && typeof b.smtp === "object") {
        const smtp = asRecord(b.smtp);
        if (smtp.host !== undefined) s.smtp.host = str(smtp.host, s.smtp.host);
        if (smtp.port !== undefined) s.smtp.port = Math.min(65535, Math.max(1, num(smtp.port, s.smtp.port)));
        if (smtp.username !== undefined) s.smtp.username = str(smtp.username, s.smtp.username);
        if (typeof smtp.password === "string" && smtp.password.length > 0) s.smtp.password = smtp.password;
        if (smtp.from !== undefined) s.smtp.from = str(smtp.from, s.smtp.from);
        if (smtp.to !== undefined) s.smtp.to = str(smtp.to, s.smtp.to);
      }
      return ok(settingsJson());
    }
    return err(405, "method not allowed");
  }

  if (seg[0] === "notifications" && seg[1] === "test" && method === "POST") {
    const smtp = state.settings.smtp;
    if (!smtp.host) {
      const result: NotificationTestResult = { ok: false, error: "SMTP 服务器未配置" };
      return ok(result);
    }
    return ok({ ok: true } satisfies NotificationTestResult);
  }

  return err(404, "not found");
}

// 启动:播种数据 + 下载调度器。
// 注意:setInterval 会让进程事件循环常驻——vite build 时 vite.config.ts 会 import 本模块,
// 不 unref 会导致构建进程在产物写完后挂起不退出(服务器上表现为前端阶段永远不结束)。
seed();
const dispatcherTimer = setInterval(dispatcherTick, TICK_MS);
if (typeof dispatcherTimer === "object" && typeof dispatcherTimer.unref === "function") {
  dispatcherTimer.unref();
}
