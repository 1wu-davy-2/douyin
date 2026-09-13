/**
 * 作品库页(核心):
 * 左列:添加博主"+"弹引导弹窗(别名/分组/独立下载根/监控参数,契约 v1.4/1.4b);
 *       博主按分组树形展示(组头可折叠,localStorage 记忆;未分组平铺排最后);
 *       博主卡片显示 别名 ?? 昵称(alias 存在时次要文字显示原昵称),SSE scan.progress 驱动扫描进度条。
 * 右列:选中博主详情(标题行显示别名 + ✏️ 重命名/分组弹窗),Tabs(作品/合集)。
 * 作品筛选:合集多选排除(契约 v1.4b),"按筛选全选"同步传 exclude_collection_ids。
 */
import { useEffect, useMemo, useState, useSyncExternalStore, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  BellOff,
  BellRing,
  ChevronDown,
  ChevronRight,
  Library as LibraryIcon,
  Pencil,
  Plus,
  RotateCcw,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";
import { ApiError } from "../api/client";
import {
  batchDeleteWorks,
  batchWorkIds,
  createCreator,
  createDownloads,
  createSubscription,
  deleteSubscription,
  moveCreatorDownloads,
  patchCreator,
  rescanCreator,
  setCreatorDownloadRoot,
  updateSubscription,
  worksRedownload,
  deleteCreator,
} from "../api/endpoints";
import { qk, useCollections, useCreators, useSettings, useSubscriptions, useWorks } from "../api/queries";
import type {
  CreateCreatorInput,
  Creator,
  MoveDownloadsResult,
  Quality,
  Subscription,
  WorkDlFilter,
  WorkSort,
  WorkTypeFilter,
} from "../api/types";
import { QUALITIES } from "../api/types";
import { ConfirmDialog } from "../components/confirm-dialog";
import { EmptyState } from "../components/empty-state";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Checkbox } from "../components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../components/ui/dialog";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";
import { Progress } from "../components/ui/progress";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Skeleton } from "../components/ui/skeleton";
import { Switch } from "../components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "../components/ui/tabs";
import { useDebouncedValue } from "../lib/use-debounced-value";
import { getScanSnapshot, subscribeScans, type LiveScan } from "../lib/scan-store";
import { formatBytes, truncateMiddle } from "../lib/format";
import { openPlayer } from "../lib/player-store";
import { cn, isAbsolutePath, removeFromSet, toggleInSet, unionIntoSet } from "../lib/utils";
import { CollectionsPanel } from "./library/collections-panel";
import { WorksPanel } from "./library/works-panel";

type TabKey = "works" | "collections";

// ---------------------------------------------------------------- 分组树 --

/** 左列折叠状态存 localStorage(仅展示层,不影响选中)。 */
const GROUPS_COLLAPSED_KEY = "douyin.creatorGroups.collapsed";

function loadCollapsedGroups(): Set<string> {
  try {
    const raw = localStorage.getItem(GROUPS_COLLAPSED_KEY);
    if (!raw) return new Set();
    const parsed: unknown = JSON.parse(raw);
    return new Set(
      Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === "string") : [],
    );
  } catch {
    return new Set();
  }
}

interface CreatorGroup {
  name: string;
  creators: Creator[];
}

/** 按 group 分组;组内保持接口顺序(created_at desc),组名按中文排序;未分组单独返回。 */
function buildCreatorGroups(list: Creator[]): { groups: CreatorGroup[]; ungrouped: Creator[] } {
  const map = new Map<string, Creator[]>();
  const ungrouped: Creator[] = [];
  for (const c of list) {
    if (c.group) {
      const arr = map.get(c.group) ?? [];
      arr.push(c);
      map.set(c.group, arr);
    } else {
      ungrouped.push(c);
    }
  }
  const groups = [...map.entries()]
    .map(([name, creators]) => ({ name, creators }))
    .sort((a, b) => a.name.localeCompare(b.name, "zh-Hans-CN"));
  return { groups, ungrouped };
}

function creatorDisplayName(c: Creator): string {
  return c.alias ?? c.nickname;
}

export function LibraryPage() {
  const queryClient = useQueryClient();

  // ---------- 数据 ----------
  const creators = useCreators();
  const [selectedCreatorId, setSelectedCreatorId] = useState<number | null>(null);
  const collections = useCollections(selectedCreatorId);

  // 作品筛选/分页(受控状态,切 Tab 不丢)
  const [tab, setTab] = useState<TabKey>("works");
  const [qInput, setQInput] = useState("");
  const q = useDebouncedValue(qInput, 300);
  /** 契约 v1.4b:多选排除的合集 id;与单合集聚焦互斥(聚焦时置空)。 */
  const [excludeIds, setExcludeIds] = useState<number[]>([]);
  /** 单发作品开关(契约 v1.2:collection_id=none)。 */
  const [singlesOnly, setSinglesOnly] = useState(false);
  /** 单合集聚焦(合集面板"查看作品"入口)。 */
  const [collectionId, setCollectionId] = useState<number | null>(null);
  const [workType, setWorkType] = useState<WorkTypeFilter | null>(null);
  const [dlStatus, setDlStatus] = useState<WorkDlFilter | null>(null);
  const [sort, setSort] = useState<WorkSort>("published_at_desc");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const works = useWorks(selectedCreatorId, {
    page,
    page_size: pageSize,
    sort,
    q: q || undefined,
    collection_none: singlesOnly || undefined,
    collection_id: collectionId ?? undefined,
    exclude_collection_ids: excludeIds.length > 0 ? excludeIds : undefined,
    type: workType ?? undefined,
    dl: dlStatus ?? undefined,
  });

  // 选择集合与画质
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [confirmDeleteOpen, setConfirmDeleteOpen] = useState(false);
  const [confirmRedownloadOpen, setConfirmRedownloadOpen] = useState(false);
  const [quality, setQuality] = useState<Quality>("1080p");

  const creatorList = creators.data ?? [];
  const selectedCreator = creatorList.find((c) => c.id === selectedCreatorId) ?? null;
  const scans = useSyncExternalStore(subscribeScans, getScanSnapshot, getScanSnapshot);

  // 监控状态(契约:creator 级订阅;一个博主可能多条,启用中的优先展示)
  const subscriptions = useSubscriptions();
  const subByCreator = useMemo(() => {
    const map = new Map<number, Subscription>();
    for (const s of subscriptions.data ?? []) {
      if (s.target_type !== "creator") continue;
      const cur = map.get(s.creator_id);
      if (!cur || (!cur.enabled && s.enabled)) map.set(s.creator_id, s);
    }
    return map;
  }, [subscriptions.data]);

  // 分组树(无分组时保持平铺;有分组时未分组排最后)
  const grouped = useMemo(() => buildCreatorGroups(creatorList), [creatorList]);
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(loadCollapsedGroups);
  const toggleGroupCollapsed = (name: string) => {
    setCollapsedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      try {
        localStorage.setItem(GROUPS_COLLAPSED_KEY, JSON.stringify([...next]));
      } catch {
        /* localStorage 不可用时静默(仅影响记忆) */
      }
      return next;
    });
  };
  const groupSuggestions = useMemo(
    () => [...new Set(creatorList.map((c) => c.group).filter((g): g is string => Boolean(g)))].sort((a, b) => a.localeCompare(b, "zh-Hans-CN")),
    [creatorList],
  );

  // 默认选中第一个博主;博主被删除后回落
  useEffect(() => {
    if (creatorList.length === 0) return;
    if (selectedCreatorId === null || !creatorList.some((c) => c.id === selectedCreatorId)) {
      setSelectedCreatorId(creatorList[0].id);
    }
  }, [creatorList, selectedCreatorId]);

  // 切换博主:清空筛选与选择
  useEffect(() => {
    setQInput("");
    setExcludeIds([]);
    setSinglesOnly(false);
    setCollectionId(null);
    setWorkType(null);
    setDlStatus(null);
    setSelected(new Set());
    setPage(1);
  }, [selectedCreatorId]);

  // 筛选条件变化回到第 1 页
  useEffect(() => {
    setPage(1);
  }, [q, singlesOnly, excludeIds, collectionId, workType, dlStatus, sort, pageSize]);

  // 总数变少时收拢页码
  useEffect(() => {
    const total = works.data?.total;
    if (total !== undefined && total > 0) {
      const tp = Math.ceil(total / pageSize);
      if (page > tp) setPage(tp);
    }
  }, [works.data, pageSize, page]);

  const pageItems = useMemo(() => works.data?.items ?? [], [works.data]);

  // ---------- 操作 ----------
  const [profileUrl, setProfileUrl] = useState("");
  /** 点击"+"后先弹引导弹窗,确认才 POST(契约 v1.4/1.4b)。 */
  const [wizardOpen, setWizardOpen] = useState(false);
  const addCreatorMut = useMutation({
    mutationFn: (input: CreateCreatorInput) => createCreator(input),
    onSuccess: (res) => {
      toast.success("博主已加入,开始扫描作品");
      setProfileUrl("");
      setWizardOpen(false);
      setSelectedCreatorId(res.creator_id);
      void queryClient.invalidateQueries({ queryKey: qk.creators });
    },
    onError: (e) => toast.error("添加博主失败", { description: e.message }),
  });

  const selectAllFilteredMut = useMutation({
    mutationFn: () =>
      batchWorkIds(selectedCreatorId as number, {
        q: q || undefined,
        collection_none: singlesOnly || undefined,
        collection_id: collectionId ?? undefined,
        exclude_collection_ids: excludeIds.length > 0 ? excludeIds : undefined,
        type: workType ?? undefined,
        dl: dlStatus ?? undefined,
      }),
    onSuccess: (res) => {
      setSelected((prev) => unionIntoSet(prev, res.ids));
      toast.success(`已按筛选选中 ${res.ids.length} 条作品`);
    },
    onError: (e) => toast.error("按筛选全选失败", { description: e.message }),
  });

  const downloadMut = useMutation({
    mutationFn: (workIds: number[]) => createDownloads(workIds, quality),
    onSuccess: (res) => {
      const created = res.created.length;
      const skipped = res.skipped.length;
      if (created > 0) {
        toast.success(`已创建 ${created} 个下载任务`, {
          description: skipped > 0 ? `跳过 ${skipped} 条(已下载或已在队列)` : undefined,
        });
      } else {
        toast.info("没有新建下载任务", { description: `${skipped} 条被跳过(已下载或已在队列)` });
      }
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: qk.downloadsAll });
      void queryClient.invalidateQueries({ queryKey: qk.downloadsSummary });
      void queryClient.invalidateQueries({ queryKey: ["works"] });
      void queryClient.invalidateQueries({ queryKey: ["creators"] });
    },
    onError: (e) => toast.error("创建下载任务失败", { description: e.message }),
  });

  // ---------- 删除博主 ----------
  const deleteCreatorMut = useMutation({
    mutationFn: (v: { id: number; deleteFiles: boolean }) =>
      deleteCreator(v.id, v.deleteFiles),
    onSuccess: (_r, v) => {
      const name = creatorList.find((c) => c.id === v.id)?.alias
        ?? creatorList.find((c) => c.id === v.id)?.nickname ?? "博主";
      toast.success(`已删除 ${name}`, {
        description: v.deleteFiles ? "作品记录与已下载文件已一并删除" : "作品记录已删除,已下载文件保留",
      });
      setDeleteTarget(null);
      void queryClient.invalidateQueries({ queryKey: ["creators"] });
      void queryClient.invalidateQueries({ queryKey: qk.subscriptions });
    },
    onError: (e) => toast.error("删除博主失败", { description: e.message }),
  });

  // ---------- 重新扫描 ----------
  const rescanMut = useMutation({
    mutationFn: (id: number) => rescanCreator(id, true),
    onSuccess: (_r, id) => {
      const name = creatorList.find((c) => c.id === id)?.nickname ?? "博主";
      toast.info(`已开始重新扫描 ${name}`, { description: "进度见博主卡片,完成后自动刷新列表" });
      void queryClient.invalidateQueries({ queryKey: ["creators"] });
    },
    onError: (e) => toast.error("重新扫描失败", { description: e.message }),
  });

  // ---------- 重新下载 ----------
  const redownloadMut = useMutation({
    mutationFn: (v: { ids: number[]; quality?: string }) => worksRedownload(v.ids, v.quality),
    onSuccess: (res) => {
      const freed = formatBytes(res.freed_bytes);
      toast.success(`已重新排队 ${res.enqueued.length} 个作品`,
        { description: `旧资产已清理(释放 ${freed}),将按图集/视频新格式重新下载` });
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ["works"] });
      void queryClient.invalidateQueries({ queryKey: ["creators"] });
      void queryClient.invalidateQueries({ queryKey: qk.downloadsAll });
      void queryClient.invalidateQueries({ queryKey: qk.downloadsSummary });
    },
    onError: (e) => toast.error("重新下载失败", { description: e.message }),
  });

  // ---------- 删除已选 ----------
  const deleteMut = useMutation({
    mutationFn: (ids: number[]) => batchDeleteWorks(ids),
    onSuccess: (res) => {
      const freed = formatBytes(res.freed_bytes);
      const fail = res.skipped.length;
      toast.success(`已删除 ${res.deleted.length} 个作品(释放 ${freed})`,
        fail > 0 ? { description: `${fail} 个作品删除失败,记录已保留` } : undefined);
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ["works"] });
      void queryClient.invalidateQueries({ queryKey: ["creators"] });
      void queryClient.invalidateQueries({ queryKey: ["collections"] });
      void queryClient.invalidateQueries({ queryKey: qk.downloadsAll });
      void queryClient.invalidateQueries({ queryKey: qk.downloadsSummary });
    },
    onError: (e) => toast.error("删除失败", { description: e.message }),
  });

  // ---------- 重命名/分组 ----------
  const [renameOpen, setRenameOpen] = useState(false);
  /** 详情标题行"监控"快捷入口(添加/管理该博主的 creator 级订阅)。 */
  const [monitorOpen, setMonitorOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Creator | null>(null);
  const [deleteFiles, setDeleteFiles] = useState(false);
  const selectedCreatorSub = selectedCreator ? subByCreator.get(selectedCreator.id) ?? null : null;

  // ---------- 渲染 ----------
  return (
    <div className="flex gap-4 p-4">
      {/* 左列:添加博主 + 博主分组树 */}
      <aside className="w-72 shrink-0 space-y-3 self-start lg:sticky lg:top-4 lg:max-h-[calc(100dvh-2rem)] lg:overflow-y-auto">
        <form
          className="flex gap-2"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            if (!profileUrl.trim()) {
              toast.error("请输入博主主页链接或 sec_uid");
              return;
            }
            setWizardOpen(true);
          }}
        >
          <Input
            value={profileUrl}
            onChange={(e) => setProfileUrl(e.target.value)}
            placeholder="粘贴主页链接或 sec_uid"
            aria-label="添加博主"
          />
          <Button type="submit" size="icon" disabled={addCreatorMut.isPending} title="添加博主">
            <Plus className="size-4" />
          </Button>
        </form>

        {creators.isPending ? (
          Array.from({ length: 4 }, (_, i) => (
            <div key={i} className="space-y-2 rounded-xl border border-border p-3">
              <div className="flex items-center gap-2.5">
                <Skeleton className="size-9 rounded-full" />
                <div className="flex-1 space-y-1.5">
                  <Skeleton className="h-3.5 w-2/3" />
                  <Skeleton className="h-3 w-1/2" />
                </div>
              </div>
            </div>
          ))
        ) : creatorList.length === 0 ? (
          <EmptyState
            icon={LibraryIcon}
            title="还没有博主"
            description="在上方粘贴抖音主页链接或 sec_uid,添加第一个博主"
          />
        ) : grouped.groups.length === 0 ? (
          /* 无分组:保持平铺现状 */
          creatorList.map((c) => (
            <CreatorCard
              key={c.id}
              creator={c}
              active={c.id === selectedCreatorId}
              live={scans.byCreator.get(c.id)}
              monitor={subByCreator.get(c.id)}
              onClick={() => setSelectedCreatorId(c.id)}
              onRescan={() => rescanMut.mutate(c.id)}
              rescanPending={rescanMut.isPending && rescanMut.variables === c.id}
              onDelete={() => setDeleteTarget(c)}
              deletePending={deleteCreatorMut.isPending}
            />
          ))
        ) : (
          <>
            {/* 有分组:组头可折叠(状态存 localStorage),组内排序不变;未分组平铺排最后 */}
            {grouped.groups.map((g) => {
              const collapsed = collapsedGroups.has(g.name);
              return (
                <div key={g.name} className="space-y-2">
                  <button
                    type="button"
                    aria-expanded={!collapsed}
                    onClick={() => toggleGroupCollapsed(g.name)}
                    className="flex w-full items-center gap-1 rounded-lg px-1 py-0.5 text-xs text-muted-foreground transition-colors hover:text-foreground"
                    title={collapsed ? "展开分组" : "折叠分组"}
                  >
                    {collapsed ? <ChevronRight className="size-3.5" /> : <ChevronDown className="size-3.5" />}
                    <span className="truncate font-medium text-foreground/80">{g.name}</span>
                    <span className="tabular-nums">({g.creators.length})</span>
                  </button>
                  {!collapsed &&
                    g.creators.map((c) => (
                      <CreatorCard
                        key={c.id}
                        creator={c}
                        active={c.id === selectedCreatorId}
                        live={scans.byCreator.get(c.id)}
                        monitor={subByCreator.get(c.id)}
                        onClick={() => setSelectedCreatorId(c.id)}
                        onRescan={() => rescanMut.mutate(c.id)}
                        rescanPending={rescanMut.isPending && rescanMut.variables === c.id}
                        onDelete={() => setDeleteTarget(c)}
                        deletePending={deleteCreatorMut.isPending}
                      />
                    ))}
                </div>
              );
            })}
            {grouped.ungrouped.length > 0 ? (
              <div className="space-y-2 border-t border-border pt-2">
                {grouped.ungrouped.map((c) => (
                  <CreatorCard
                    key={c.id}
                    creator={c}
                    active={c.id === selectedCreatorId}
                    live={scans.byCreator.get(c.id)}
                    monitor={subByCreator.get(c.id)}
                    onClick={() => setSelectedCreatorId(c.id)}
                    onRescan={() => rescanMut.mutate(c.id)}
                    rescanPending={rescanMut.isPending && rescanMut.variables === c.id}
                    onDelete={() => setDeleteTarget(c)}
                    deletePending={deleteCreatorMut.isPending}
                  />
                ))}
              </div>
            ) : null}
          </>
        )}
      </aside>

      {/* 右列:选中博主详情 */}
      <section className="min-w-0 flex-1 space-y-3">
        {selectedCreator ? (
          <>
            <div className="flex items-center gap-3">
              <img src={selectedCreator.avatar_url} alt="" className="size-11 rounded-full object-cover" />
              <div className="min-w-0">
                <div className="flex items-center gap-1.5">
                  <h1 className="truncate text-base font-semibold" title={creatorDisplayName(selectedCreator)}>
                    {creatorDisplayName(selectedCreator)}
                  </h1>
                  {selectedCreator.alias ? (
                    <span className="shrink-0 text-xs text-muted-foreground" title={`原昵称:${selectedCreator.nickname}`}>
                      原昵称:{selectedCreator.nickname}
                    </span>
                  ) : null}
                  {selectedCreator.group ? (
                    <Badge variant="secondary" className="shrink-0 px-1.5 py-0 text-[10px] leading-4">
                      {selectedCreator.group}
                    </Badge>
                  ) : null}
                  {selectedCreatorSub ? (
                    selectedCreatorSub.enabled ? (
                      <Badge variant="info" className="shrink-0 gap-0.5 px-1.5 py-0 text-[10px] leading-4" title={`监控中 · 每 ${selectedCreatorSub.interval_minutes} 分钟检查一次`}>
                        <BellRing className="size-2.5" /> 监控中
                      </Badge>
                    ) : (
                      <Badge variant="muted" className="shrink-0 px-1.5 py-0 text-[10px] leading-4" title="订阅存在但已暂停">
                        已暂停
                      </Badge>
                    )
                  ) : null}
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    className="size-6 shrink-0 text-muted-foreground hover:text-foreground"
                    title="重命名 / 分组"
                    aria-label="重命名 / 分组"
                    onClick={() => setRenameOpen(true)}
                  >
                    <Pencil className="size-3.5" />
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="h-6 shrink-0 gap-1 px-1.5 text-[11px] text-muted-foreground hover:text-foreground"
                    title={selectedCreatorSub ? "管理监控(间隔/画质/自动下载)" : "为该博主添加监控订阅"}
                    onClick={() => setMonitorOpen(true)}
                  >
                    <BellRing className="size-3" />
                    {selectedCreatorSub ? "监控中" : "添加监控"}
                  </Button>
                </div>
                <p className="text-xs text-muted-foreground">
                  已收录 {selectedCreator.works_count} / 主页 {selectedCreator.reported_work_count} 个作品 · 已下载{" "}
                  {selectedCreator.downloaded_count} · 下载空间 {formatBytes(selectedCreator.download_bytes)}
                  <DownloadRootEditor creator={selectedCreator} />
                </p>
              </div>
            </div>

            <Tabs value={tab} onValueChange={(v) => setTab(v as TabKey)}>
              <TabsList>
                <TabsTrigger value="works">作品</TabsTrigger>
                <TabsTrigger value="collections">
                  合集{collections.data ? `(${collections.data.length})` : ""}
                </TabsTrigger>
              </TabsList>
            </Tabs>

            {tab === "works" ? (
              <WorksPanel
                works={works.data}
                isPending={works.isPending}
                isFetching={works.isFetching}
                collections={collections.data ?? []}
                q={qInput}
                onQChange={setQInput}
                excludeIds={excludeIds}
                onToggleExclude={(id) => {
                  setExcludeIds((prev) =>
                    prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id],
                  );
                  setCollectionId(null); // 排除与单合集聚焦互斥
                }}
                singlesOnly={singlesOnly}
                onSinglesOnlyChange={(v) => {
                  setSinglesOnly(v);
                  if (v) setCollectionId(null);
                }}
                collectionId={collectionId}
                onCollectionFocusChange={setCollectionId}
                type={workType}
                onTypeChange={setWorkType}
                dl={dlStatus}
                onDlChange={setDlStatus}
                sort={sort}
                onSortChange={setSort}
                page={page}
                pageSize={pageSize}
                onPageChange={setPage}
                onPageSizeChange={setPageSize}
                selected={selected}
                pageItems={pageItems}
                onToggleRow={(id) => setSelected((prev) => toggleInSet(prev, id))}
                onSelectPage={() =>
                  setSelected((prev) => {
                    const allSelected = pageItems.length > 0 && pageItems.every((w) => prev.has(w.id));
                    const ids = pageItems.map((w) => w.id);
                    return allSelected ? removeFromSet(prev, ids) : unionIntoSet(prev, ids);
                  })
                }
                onClearSelection={() => setSelected(new Set())}
                onSelectAllFiltered={() => selectAllFilteredMut.mutate()}
                selectAllPending={selectAllFilteredMut.isPending}
                quality={quality}
                onQualityChange={setQuality}
                onDownload={() => downloadMut.mutate([...selected])}
                downloadPending={downloadMut.isPending}
                onDelete={() => setConfirmDeleteOpen(true)}
                deletePending={deleteMut.isPending}
                onRedownload={() => setConfirmRedownloadOpen(true)}
                redownloadPending={redownloadMut.isPending}
                onPlay={(index) => {
                  const playable = pageItems
                    .filter((w) => w.dl_status === "succeeded")
                    .map((w) => ({ workId: w.id, title: w.title, creatorName: creatorDisplayName(selectedCreator) }));
                  if (playable.length > 0) openPlayer(playable, index);
                }}
              />
            ) : (
              <CollectionsPanel
                collections={collections.data}
                isPending={collections.isPending}
                onPick={(id) => {
                  setCollectionId(id); // 单合集聚焦(契约 v1.4b:与排除互斥,聚焦时清空排除)
                  setExcludeIds([]);
                  setSinglesOnly(false);
                  setTab("works");
                }}
              />
            )}
          </>
        ) : creators.isPending ? (
          <Skeleton className="h-64 rounded-xl" />
        ) : (
          <EmptyState
            icon={LibraryIcon}
            title="选择左侧博主查看作品"
            description="或添加一个新博主开始扫描"
          />
        )}
      </section>

      <AddCreatorDialog
        open={wizardOpen}
        onOpenChange={setWizardOpen}
        profileUrl={profileUrl.trim()}
        groupSuggestions={groupSuggestions}
        pending={addCreatorMut.isPending}
        onSubmit={(input) => addCreatorMut.mutate(input)}
      />

      {selectedCreator ? (
        <RenameGroupDialog
          creator={selectedCreator}
          groupSuggestions={groupSuggestions}
          open={renameOpen}
          onOpenChange={setRenameOpen}
        />
      ) : null}

      {selectedCreator ? (
        <MonitorDialog
          creator={selectedCreator}
          sub={selectedCreatorSub}
          open={monitorOpen}
          onOpenChange={setMonitorOpen}
        />
      ) : null}

      <ConfirmDialog
        open={confirmRedownloadOpen}
        onOpenChange={setConfirmRedownloadOpen}
        title={`重新下载 ${selected.size} 个作品?`}
        description="将清理这些作品的旧下载文件与记录,然后按当前格式(图集/视频)重新排队下载。"
        confirmLabel="重新下载"
        loading={redownloadMut.isPending}
        onConfirm={() => redownloadMut.mutate({ ids: [...selected], quality })}
      />
      <ConfirmDialog
        open={confirmDeleteOpen}
        onOpenChange={setConfirmDeleteOpen}
        title={`删除已选的 ${selected.size} 个作品?`}
        description="将同时删除这些作品已下载的视频/图片文件与下载记录,不可恢复。"
        confirmLabel="删除"
        destructive
        loading={deleteMut.isPending}
        onConfirm={() => deleteMut.mutate([...selected])}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(o) => (o ? undefined : setDeleteTarget(null))}
        title={`删除博主「${deleteTarget ? (deleteTarget.alias ?? deleteTarget.nickname) : ""}」?`}
        description="将删除该博主的作品记录、合集、下载任务与订阅。可在下方选择是否同时删除已下载的视频/图片文件。"
        confirmLabel="删除"
        destructive
        loading={deleteCreatorMut.isPending}
        onConfirm={() => {
          if (deleteTarget) deleteCreatorMut.mutate({ id: deleteTarget.id, deleteFiles });
        }}
      >
        <label className="flex items-center gap-2 rounded-md border border-border px-3 py-2 text-sm">
          <Checkbox
            checked={deleteFiles}
            onCheckedChange={(v) => setDeleteFiles(v === true)}
            aria-label="同时删除已下载文件"
          />
          同时删除已下载文件({formatBytes(deleteTarget?.download_bytes ?? 0)})
          <span className="text-xs text-muted-foreground">不勾选则文件保留在磁盘</span>
        </label>
      </ConfirmDialog>
    </div>
  );
}

/** 博主卡片:显示 别名 ?? 昵称(alias 存在时次要文字显示原昵称)+ 监控状态 Badge + SSE 扫描实时进度(scan-store)。 */
function CreatorCard({
  creator,
  active,
  live,
  monitor,
  onClick,
  onRescan,
  rescanPending,
  onDelete,
  deletePending,
}: {
  creator: { id: number; nickname: string; alias: string | null; group: string | null; avatar_url: string; works_count: number; downloaded_count: number; download_bytes: number };
  active: boolean;
  live: LiveScan | undefined;
  /** 该博主的 creator 级订阅(无则不显示监控徽标;enabled=false 显示"已暂停")。 */
  monitor?: Subscription;
  onClick: () => void;
  onRescan: () => void;
  rescanPending: boolean;
  onDelete: () => void;
  deletePending: boolean;
}) {
  const scanning = Boolean(live?.running);
  const displayName = creator.alias ?? creator.nickname;
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }}
      className={cn(
        "relative w-full cursor-pointer rounded-xl border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60",
        active ? "border-primary/60 bg-accent" : "border-border hover:bg-accent/50",
      )}
      title={creator.alias ? `别名:${creator.alias} · 原昵称:${creator.nickname}` : creator.nickname}
    >
      <div className="flex items-center gap-2.5">
        <img src={creator.avatar_url} alt="" className="size-9 shrink-0 rounded-full object-cover" />
        <div className="min-w-0 flex-1">
          <p className="flex min-w-0 items-center gap-1 text-sm font-medium">
            <span className="truncate">{displayName}</span>
            {monitor ? (
              monitor.enabled ? (
                <Badge
                  variant="info"
                  className="shrink-0 gap-0.5 px-1 py-0 text-[10px] leading-4"
                  title={`监控中 · 每 ${monitor.interval_minutes} 分钟检查一次`}
                >
                  <BellRing className="size-2.5" /> 监控中
                </Badge>
              ) : (
                <Badge
                  variant="muted"
                  className="shrink-0 px-1 py-0 text-[10px] leading-4"
                  title="订阅存在但已暂停,可在监控页或详情页恢复"
                >
                  已暂停
                </Badge>
              )
            ) : null}
          </p>
          {creator.alias ? (
            <p className="truncate text-[11px] text-muted-foreground">{creator.nickname}</p>
          ) : null}
          <p className="text-[11px] text-muted-foreground">
            {creator.works_count} 作品 · 已下载 {creator.downloaded_count} · {formatBytes(creator.download_bytes)}
          </p>
        </div>
        <Button
          type="button"
          size="icon"
          variant="ghost"
          className="size-7 shrink-0 text-muted-foreground hover:text-foreground"
          title={scanning ? "扫描进行中" : "重新扫描(全量,用于补齐漏扫/无监控时拉新作品)"}
          disabled={scanning || rescanPending}
          onClick={(e) => {
            e.stopPropagation();
            onRescan();
          }}
        >
          <RotateCcw className={cn("size-3.5", (scanning || rescanPending) && "animate-spin")} />
        </Button>
        <Button
          type="button"
          size="icon"
          variant="ghost"
          className="size-7 shrink-0 text-muted-foreground hover:text-destructive"
          title="删除该博主(可选择是否同时删除已下载文件)"
          disabled={deletePending}
          onClick={(e) => {
            e.stopPropagation();
            onDelete();
          }}
        >
          <Trash2 className="size-3.5" />
        </Button>
      </div>

      {live?.running ? (
        <div className="mt-2.5 space-y-1">
          <Progress value={Math.min(92, live.page * 11)} className="h-1" />
          <p className="text-[11px] text-info">
            扫描中 · 第 {live.page} 页 · 新增 {live.newCount}
          </p>
        </div>
      ) : null}

      {live && !live.running && live.result ? (
        <div className="mt-2 flex items-center gap-1.5">
          {live.result.status === "succeeded" ? (
            <Badge variant="success">扫描完成 +{live.newCount}</Badge>
          ) : live.result.status === "partial" ? (
            <Badge variant="warning">
              部分完成 +{live.newCount} · 完整度 {live.result.completeness}%
            </Badge>
          ) : (
            <Badge variant="destructive">扫描失败</Badge>
          )}
        </div>
      ) : null}
    </div>
  );
}

/** move-downloads 的失败数:契约 v1.3 返回失败明细数组([{path,error}]),兼容数值计数。 */
function failedCountOf(res: MoveDownloadsResult): number {
  const failed: unknown = res.failed_files;
  if (Array.isArray(failed)) return failed.length;
  return typeof failed === "number" ? failed : 0;
}

/**
 * 添加博主引导弹窗(契约 v1.4/1.4b):
 * 展示解析出的链接;可选 别名 / 分组(已有分组建议 + 可输入新名)/ 独立下载根;
 * "加入监控"开启时显示 间隔(分钟)+ 画质 + "自动下载新作品" 开关;
 * 确认把全部字段 POST 给 /api/creators;202 后的 SSE 扫描进度逻辑不变。
 */
function AddCreatorDialog({
  open,
  onOpenChange,
  profileUrl,
  groupSuggestions,
  pending,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  profileUrl: string;
  groupSuggestions: string[];
  pending: boolean;
  onSubmit: (input: CreateCreatorInput) => void;
}) {
  const { data: settings } = useSettings();
  const [alias, setAlias] = useState("");
  const [group, setGroup] = useState("");
  const [downloadRoot, setDownloadRoot] = useState("");
  const [monitor, setMonitor] = useState(true);
  const [intervalInput, setIntervalInput] = useState("60");
  const [quality, setQuality] = useState<Quality>("1080p");
  const [autoDownload, setAutoDownload] = useState(true);

  // 打开瞬间重置表单;画质默认跟随全局设置
  useEffect(() => {
    if (open) {
      setAlias("");
      setGroup("");
      setDownloadRoot("");
      setMonitor(true);
      setIntervalInput("60");
      setAutoDownload(true);
      const gq = settings?.download_quality;
      setQuality(gq === "540p" || gq === "720p" || gq === "1080p" ? gq : "1080p");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const globalRoot = settings?.download_root?.trim() ?? "";
  const rootPlaceholder = globalRoot
    ? `留空 = 跟随全局(${truncateMiddle(globalRoot, 26)})`
    : "留空 = 使用默认下载目录";

  const handleSubmit = () => {
    const interval = Number(intervalInput);
    if (monitor && (!Number.isInteger(interval) || interval < 1)) {
      toast.error("监控间隔不合法", { description: "间隔需为 >= 1 的整数(分钟)" });
      return;
    }
    if (downloadRoot.trim() && !isAbsolutePath(downloadRoot.trim())) {
      toast.error("下载根目录不合法", { description: "需为绝对路径,如 D:\\Media\\Douyin" });
      return;
    }
    onSubmit({
      profile_url: profileUrl,
      alias: alias.trim() || undefined,
      group: group.trim() || undefined,
      download_root: downloadRoot.trim() || undefined,
      subscribe: monitor
        ? { interval_minutes: interval, quality, auto_download: autoDownload }
        : undefined,
    });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle className="text-base">添加博主</DialogTitle>
          <DialogDescription>确认后立即开始首次扫描,可在扫描完成前继续操作其他博主</DialogDescription>
        </DialogHeader>
        <div className="space-y-3.5">
          <div className="space-y-1.5">
            <Label>博主链接</Label>
            <p className="max-h-16 overflow-y-auto break-all rounded-md border border-border bg-muted/40 px-2.5 py-1.5 font-mono text-xs">
              {profileUrl}
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="add-creator-alias">别名(可选)</Label>
            <Input
              id="add-creator-alias"
              value={alias}
              onChange={(e) => setAlias(e.target.value)}
              placeholder="默认使用博主昵称"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="add-creator-group">分组(可选)</Label>
            <Input
              id="add-creator-group"
              value={group}
              onChange={(e) => setGroup(e.target.value)}
              placeholder="输入新分组名,或点选下方建议"
            />
            {groupSuggestions.length > 0 ? (
              <div className="flex flex-wrap gap-1.5">
                {groupSuggestions.map((g) => (
                  <button
                    key={g}
                    type="button"
                    onClick={() => setGroup(g)}
                    className={cn(
                      "rounded-full border px-2 py-0.5 text-[11px] transition-colors",
                      group === g
                        ? "border-primary/60 bg-accent text-foreground"
                        : "border-border text-muted-foreground hover:bg-accent/60 hover:text-foreground",
                    )}
                  >
                    {g}
                  </button>
                ))}
              </div>
            ) : null}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="add-creator-root">独立下载根目录(可选)</Label>
            <Input
              id="add-creator-root"
              className="font-mono text-xs"
              value={downloadRoot}
              onChange={(e) => setDownloadRoot(e.target.value)}
              placeholder={rootPlaceholder}
            />
          </div>
          <label
            htmlFor="add-creator-monitor"
            className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2.5"
          >
            <span className="min-w-0">
              <span className="block text-sm">加入监控</span>
              <span className="block text-[11px] text-muted-foreground">
                定期扫描该博主主页,及时发现新作品
              </span>
            </span>
            <Switch
              id="add-creator-monitor"
              checked={monitor}
              onCheckedChange={setMonitor}
              disabled={pending}
            />
          </label>
          {monitor ? (
            <div className="space-y-3 rounded-lg border border-border px-3 py-2.5">
              <div className="flex items-center gap-2">
                <Label htmlFor="add-creator-interval" className="w-24 shrink-0 text-sm">
                  间隔(分钟)
                </Label>
                <Input
                  id="add-creator-interval"
                  type="number"
                  min={1}
                  value={intervalInput}
                  onChange={(e) => setIntervalInput(e.target.value)}
                  className="h-8 w-24"
                />
              </div>
              <div className="flex items-center gap-2">
                <Label htmlFor="add-creator-quality" className="w-24 shrink-0 text-sm">
                  下载画质
                </Label>
                <Select value={quality} onValueChange={(v) => setQuality(v as Quality)}>
                  <SelectTrigger id="add-creator-quality" className="h-8 w-28">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {QUALITIES.map((q2) => (
                      <SelectItem key={q2} value={q2}>
                        {q2}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="flex items-center justify-between gap-3">
                <span className="text-sm">自动下载新作品</span>
                <Switch
                  checked={autoDownload}
                  onCheckedChange={setAutoDownload}
                  disabled={pending}
                  aria-label="自动下载新作品"
                />
              </div>
            </div>
          ) : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>
            取消
          </Button>
          <Button onClick={handleSubmit} disabled={pending}>
            {pending ? "添加中…" : "确认添加"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * 重命名 / 分组弹窗(契约 v1.4):编辑 别名(空 = 恢复默认昵称)与 分组(空 = 未分组),
 * PATCH /api/creators/{id};已有分组作为点选建议。
 */
function RenameGroupDialog({
  creator,
  groupSuggestions,
  open,
  onOpenChange,
}: {
  creator: Creator;
  groupSuggestions: string[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [aliasInput, setAliasInput] = useState("");
  const [groupInput, setGroupInput] = useState("");

  useEffect(() => {
    if (open) {
      setAliasInput(creator.alias ?? "");
      setGroupInput(creator.group ?? "");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const patchMut = useMutation({
    mutationFn: (vars: { id: number; alias: string | null; group: string | null }) =>
      patchCreator(vars.id, { alias: vars.alias, group: vars.group }),
    onSuccess: (res) => {
      toast.success("已保存", {
        description: `显示名:${res.alias ?? "默认昵称"} · 分组:${res.group ?? "未分组"}`,
      });
      void queryClient.invalidateQueries({ queryKey: qk.creators });
      onOpenChange(false);
    },
    onError: (e) => toast.error("保存失败", { description: e.message }),
  });

  const suggestions = groupSuggestions.filter((g) => g !== creator.group);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle className="text-base">重命名 / 分组 · {creator.nickname}</DialogTitle>
          <DialogDescription>别名与分组仅影响显示与左列分组树,不影响下载</DialogDescription>
        </DialogHeader>
        <div className="space-y-3.5">
          <div className="space-y-1.5">
            <Label htmlFor="rename-creator-alias">别名</Label>
            <Input
              id="rename-creator-alias"
              value={aliasInput}
              onChange={(e) => setAliasInput(e.target.value)}
              placeholder="留空 = 恢复默认昵称"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="rename-creator-group">分组</Label>
            <Input
              id="rename-creator-group"
              value={groupInput}
              onChange={(e) => setGroupInput(e.target.value)}
              placeholder="留空 = 未分组"
            />
            {suggestions.length > 0 ? (
              <div className="flex flex-wrap gap-1.5">
                {suggestions.map((g) => (
                  <button
                    key={g}
                    type="button"
                    onClick={() => setGroupInput(g)}
                    className={cn(
                      "rounded-full border px-2 py-0.5 text-[11px] transition-colors",
                      groupInput === g
                        ? "border-primary/60 bg-accent text-foreground"
                        : "border-border text-muted-foreground hover:bg-accent/60 hover:text-foreground",
                    )}
                  >
                    {g}
                  </button>
                ))}
              </div>
            ) : null}
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={patchMut.isPending}>
            取消
          </Button>
          <Button
            onClick={() =>
              patchMut.mutate({
                id: creator.id,
                alias: aliasInput.trim() === "" ? null : aliasInput.trim(),
                group: groupInput.trim() === "" ? null : groupInput.trim(),
              })
            }
            disabled={patchMut.isPending}
          >
            {patchMut.isPending ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * 监控快捷弹窗(详情标题行"添加监控 / 监控中"入口):
 * 无订阅 → POST /api/subscriptions(creator 级);
 * 已有订阅 → 预填参数,PATCH /api/subscriptions/{id};另提供"移除监控"(DELETE,带确认)。
 */
function MonitorDialog({
  creator,
  sub,
  open,
  onOpenChange,
}: {
  creator: Creator;
  sub: Subscription | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const { data: settings } = useSettings();
  const [intervalInput, setIntervalInput] = useState("60");
  const [quality, setQuality] = useState<Quality>("1080p");
  const [autoDownload, setAutoDownload] = useState(true);
  const [confirmRemoveOpen, setConfirmRemoveOpen] = useState(false);

  useEffect(() => {
    if (open) {
      const gq = settings?.download_quality;
      setQuality(
        sub && isQuality(sub.quality)
          ? sub.quality
          : isQuality(gq) ? gq : "1080p",
      );
      setIntervalInput(String(sub?.interval_minutes ?? 60));
      setAutoDownload(sub?.auto_download ?? true);
      setConfirmRemoveOpen(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const invalidate = () => void queryClient.invalidateQueries({ queryKey: qk.subscriptions });

  const createMut = useMutation({
    mutationFn: () =>
      createSubscription({
        target_type: "creator",
        creator_id: creator.id,
        interval_minutes: Number(intervalInput),
        auto_download: autoDownload,
        quality,
      }),
    onSuccess: (s) => {
      toast.success("已加入监控", {
        description: `将每 ${s.interval_minutes} 分钟检查「${creatorDisplayName(creator)}」`,
      });
      invalidate();
      onOpenChange(false);
    },
    onError: (e) => toast.error("添加监控失败", { description: e.message }),
  });

  const patchMut = useMutation({
    mutationFn: () =>
      updateSubscription(sub!.id, {
        interval_minutes: Number(intervalInput),
        auto_download: autoDownload,
        quality,
      }),
    onSuccess: () => {
      toast.success("监控设置已保存");
      invalidate();
      onOpenChange(false);
    },
    onError: (e) => toast.error("保存监控设置失败", { description: e.message }),
  });

  const removeMut = useMutation({
    mutationFn: () => deleteSubscription(sub!.id),
    onSuccess: () => {
      toast.success("已移除监控", { description: `不再自动检查「${creatorDisplayName(creator)}」` });
      invalidate();
      onOpenChange(false);
    },
    onError: (e) => toast.error("移除监控失败", { description: e.message }),
  });

  const handleSave = () => {
    const interval = Number(intervalInput);
    if (!Number.isInteger(interval) || interval < 1 || interval > 10080) {
      toast.error("间隔不合法", { description: "间隔需为 1 ~ 10080 之间的整数分钟" });
      return;
    }
    if (sub) patchMut.mutate();
    else createMut.mutate();
  };

  const pending = createMut.isPending || patchMut.isPending || removeMut.isPending;

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle className="text-base">
              {sub ? "管理监控" : "添加监控"} · {creatorDisplayName(creator)}
            </DialogTitle>
            <DialogDescription>
              {sub
                ? "调整检查间隔 / 画质 / 自动下载;移除后不再自动扫描该博主"
                : "按设定间隔自动扫描该博主主页,新作品可自动下载"}
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="flex items-center gap-2">
              <Label htmlFor="monitor-interval" className="w-24 shrink-0 text-sm">
                间隔(分钟)
              </Label>
              <Input
                id="monitor-interval"
                type="number"
                min={1}
                value={intervalInput}
                onChange={(e) => setIntervalInput(e.target.value)}
                className="h-8 w-24"
              />
            </div>
            <div className="flex items-center gap-2">
              <Label htmlFor="monitor-quality" className="w-24 shrink-0 text-sm">
                下载画质
              </Label>
              <Select value={quality} onValueChange={(v) => setQuality(v as Quality)}>
                <SelectTrigger id="monitor-quality" className="h-8 w-28">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {QUALITIES.map((q2) => (
                    <SelectItem key={q2} value={q2}>
                      {q2}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
              <span>
                <span className="block text-sm">自动下载新作品</span>
                <span className="block text-[11px] text-muted-foreground">关闭时仅记录,需手动下载</span>
              </span>
              <Switch checked={autoDownload} onCheckedChange={setAutoDownload} disabled={pending} aria-label="自动下载新作品" />
            </div>
          </div>
          <DialogFooter className="sm:justify-between">
            {sub ? (
              <Button
                type="button"
                variant="ghost"
                className="text-destructive hover:text-destructive"
                onClick={() => setConfirmRemoveOpen(true)}
                disabled={pending}
              >
                <BellOff className="size-3.5" /> 移除监控
              </Button>
            ) : <span />}
            <span className="flex gap-2">
              <Button variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>
                取消
              </Button>
              <Button onClick={handleSave} disabled={pending}>
                {pending ? "保存中…" : sub ? "保存" : "添加监控"}
              </Button>
            </span>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={confirmRemoveOpen}
        onOpenChange={setConfirmRemoveOpen}
        title={`移除对「${creatorDisplayName(creator)}」的监控?`}
        description="移除后将不再自动扫描该博主;已下载文件与订阅统计不受影响。"
        confirmLabel="移除监控"
        destructive
        loading={removeMut.isPending}
        onConfirm={() => removeMut.mutate()}
      />
    </>
  );
}

function isQuality(v: unknown): v is Quality {
  return v === "540p" || v === "720p" || v === "1080p";
}

/**
 * 博主详情标题行的下载路径指示 + 编辑入口(契约 v1.3):
 * 显示当前生效路径(独立 ?? 全局 ?? 默认)并用徽标区分来源;铅笔按钮弹出编辑小窗。
 */
function DownloadRootEditor({ creator }: { creator: Creator }) {
  const [open, setOpen] = useState(false);
  const { data: settings } = useSettings();
  const own = creator.download_root ?? null;
  const globalRoot = settings?.download_root ?? "";
  const displayPath = own ?? globalRoot ?? "";
  const source = own ? "独立" : globalRoot ? "全局" : "默认";
  const titleText = own
    ? `独立下载根目录:${own}`
    : globalRoot
      ? `跟随全局下载根目录:${globalRoot}`
      : "使用默认下载根目录:<data_dir>/downloads";

  return (
    <span className="inline-flex items-center gap-1 whitespace-nowrap align-baseline">
      <span aria-hidden="true">·</span>
      <Badge variant={own ? "info" : "muted"} className="px-1.5 py-0 text-[10px] leading-4">
        {source}
      </Badge>
      <span className="font-mono text-[11px]" title={titleText}>
        {displayPath ? truncateMiddle(displayPath, 34) : "data\\downloads"}
      </span>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="size-5 text-muted-foreground hover:text-foreground"
        title="编辑下载根目录"
        aria-label="编辑下载根目录"
        onClick={() => setOpen(true)}
      >
        <Pencil className="size-3" />
      </Button>
      <DownloadRootDialog creator={creator} open={open} onOpenChange={setOpen} />
    </span>
  );
}

/**
 * 独立下载根目录编辑小窗(契约 v1.3):
 * 空 = 跟随全局;保存 PATCH /creators/{id}/download-root;
 * 可选同时 POST /creators/{id}/move-downloads 搬移已下载文件(409 = 有下载中任务)。
 */
function DownloadRootDialog({
  creator,
  open,
  onOpenChange,
}: {
  creator: Creator;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [pathInput, setPathInput] = useState("");
  const [moveFiles, setMoveFiles] = useState(false);

  // 打开瞬间同步当前值;仅依赖 open,避免后台 refetch(creator 引用变化)重置编辑中的内容
  useEffect(() => {
    if (open) {
      setPathInput(creator.download_root ?? "");
      setMoveFiles(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const moveMut = useMutation({
    mutationFn: (vars: { creatorId: number; targetRoot: string }) =>
      moveCreatorDownloads(vars.creatorId, vars.targetRoot),
    onSuccess: (res) => {
      const failed = failedCountOf(res);
      const skipped = res.skipped_files ?? 0;
      const description = skipped > 0 ? `跳过 ${skipped} 个文件` : undefined;
      if (failed > 0) {
        toast.warning(`已移动 ${res.moved_files} 个文件,${failed} 个失败`, { description });
      } else {
        toast.success(`已移动 ${res.moved_files} 个文件(${formatBytes(res.moved_bytes)})`, { description });
      }
      // 文件已搬到新位置,资产路径缓存全部失效
      void queryClient.invalidateQueries({ queryKey: ["assets"] });
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409) {
        toast.error("有下载中的任务,稍后再试", { description: e.message });
      } else {
        toast.error("移动已下载文件失败", { description: e.message });
      }
    },
  });

  const saveMut = useMutation({
    mutationFn: (vars: { creatorId: number; path: string | null; move: boolean }) =>
      setCreatorDownloadRoot(vars.creatorId, vars.path),
    onSuccess: (_res, vars) => {
      toast.success(vars.path ? "已设置独立下载根目录" : "已改为跟随全局下载目录", {
        description: vars.path ? "仅新下载落入新目录,历史文件位置不变" : undefined,
      });
      void queryClient.invalidateQueries({ queryKey: qk.creators });
      onOpenChange(false);
      if (vars.move && vars.path) {
        moveMut.mutate({ creatorId: vars.creatorId, targetRoot: vars.path });
      }
    },
    onError: (e) => toast.error("保存下载根目录失败", { description: e.message }),
  });

  const handleSave = () => {
    const next = pathInput.trim();
    if (next && !isAbsolutePath(next)) {
      toast.error("路径不合法", { description: "需为绝对路径,如 D:\\Media\\Douyin" });
      return;
    }
    saveMut.mutate({ creatorId: creator.id, path: next === "" ? null : next, move: moveFiles });
  };

  const canMove = pathInput.trim() !== "";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle className="text-base">下载根目录 · {creator.nickname}</DialogTitle>
          <DialogDescription>为该博主设置独立下载目录,不影响其他博主</DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="creator-download-root">下载根目录</Label>
            <Input
              id="creator-download-root"
              className="font-mono text-xs"
              placeholder="留空 = 跟随全局下载根目录"
              value={pathInput}
              onChange={(e) => setPathInput(e.target.value)}
            />
            <p className="text-[11px] text-muted-foreground">
              绝对路径,保存时自动创建目录;修改后仅新下载落入新目录,历史文件位置不变(资产按绝对路径记录)。
            </p>
          </div>
          <label
            htmlFor="creator-move-downloads"
            className="flex items-start gap-2 rounded-lg border border-border px-3 py-2.5"
          >
            <Checkbox
              id="creator-move-downloads"
              className="mt-0.5"
              checked={moveFiles}
              disabled={!canMove || saveMut.isPending}
              onCheckedChange={(v) => setMoveFiles(v === true)}
            />
            <span className="min-w-0">
              <span className="block text-sm">同时移动已下载文件到新路径</span>
              <span className="block text-[11px] text-muted-foreground">
                {canMove
                  ? "把该博主已下载文件整体搬到新目录;有下载中的任务时无法执行"
                  : "填写新路径后可用"}
              </span>
            </span>
          </label>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={saveMut.isPending}>
            取消
          </Button>
          <Button onClick={handleSave} disabled={saveMut.isPending}>
            {saveMut.isPending ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
