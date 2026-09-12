/**
 * 作品库页(核心):
 * 左列:博主卡片(头像/昵称/作品数/已下载数/下载空间),SSE scan.progress 驱动扫描进度条;
 *       "添加博主"输入框 → POST /api/creators(202)→ scan-store 记录 → scan.done 刷新并 toast。
 * 右列:选中博主详情,Tabs(作品/合集)。
 */
import { useEffect, useMemo, useState, useSyncExternalStore, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Library as LibraryIcon, Pencil, Plus, RotateCcw } from "lucide-react";
import { toast } from "sonner";
import { ApiError } from "../api/client";
import { createCreator, moveCreatorDownloads, rescanCreator, setCreatorDownloadRoot, batchDeleteWorks, worksRedownload } from "../api/endpoints";
import { ConfirmDialog } from "../components/confirm-dialog";
import { qk, useCollections, useCreators, useSettings, useWorks } from "../api/queries";
import type { Creator, MoveDownloadsResult, Quality, WorkDlFilter, WorkSort, WorkTypeFilter} from "../api/types";
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
import { Skeleton } from "../components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "../components/ui/tabs";
import { useDebouncedValue } from "../lib/use-debounced-value";
import { getScanSnapshot, subscribeScans, type LiveScan } from "../lib/scan-store";
import { formatBytes, truncateMiddle } from "../lib/format";
import { openPlayer } from "../lib/player-store";
import { batchWorkIds, createDownloads } from "../api/endpoints";
import { cn, isAbsolutePath, removeFromSet, toggleInSet, unionIntoSet } from "../lib/utils";
import { CollectionsPanel } from "./library/collections-panel";
import { WorksPanel } from "./library/works-panel";

type TabKey = "works" | "collections";

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
    collection_id: collectionId ?? undefined,
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
    setCollectionId(null);
    setWorkType(null);
    setDlStatus(null);
    setSelected(new Set());
    setPage(1);
  }, [selectedCreatorId]);

  // 筛选条件变化回到第 1 页
  useEffect(() => {
    setPage(1);
  }, [q, collectionId, workType, dlStatus, sort, pageSize]);

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
  const addCreatorMut = useMutation({
    mutationFn: () => createCreator(profileUrl.trim()),
    onSuccess: (res) => {
      toast.success("博主已加入,开始扫描作品");
      setProfileUrl("");
      setSelectedCreatorId(res.creator_id);
      void queryClient.invalidateQueries({ queryKey: qk.creators });
    },
    onError: (e) => toast.error("添加博主失败", { description: e.message }),
  });

  const selectAllFilteredMut = useMutation({
    mutationFn: () =>
      batchWorkIds(selectedCreatorId as number, {
        q: q || undefined,
        collection_id: collectionId ?? undefined,
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

  // ---------- 渲染 ----------
  return (
    <div className="flex gap-4 p-4">
      {/* 左列:添加博主 + 博主卡片 */}
      <aside className="w-72 shrink-0 space-y-3 self-start lg:sticky lg:top-4 lg:max-h-[calc(100dvh-2rem)] lg:overflow-y-auto">
        <form
          className="flex gap-2"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            if (!profileUrl.trim()) {
              toast.error("请输入博主主页链接或 sec_uid");
              return;
            }
            addCreatorMut.mutate();
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
        ) : (
          creatorList.map((c) => (
            <CreatorCard
              key={c.id}
              creator={c}
              active={c.id === selectedCreatorId}
              live={scans.byCreator.get(c.id)}
              onClick={() => setSelectedCreatorId(c.id)}
              onRescan={() => rescanMut.mutate(c.id)}
              rescanPending={rescanMut.isPending && rescanMut.variables === c.id}
            />
          ))
        )}
      </aside>

      {/* 右列:选中博主详情 */}
      <section className="min-w-0 flex-1 space-y-3">
        {selectedCreator ? (
          <>
            <div className="flex items-center gap-3">
              <img src={selectedCreator.avatar_url} alt="" className="size-11 rounded-full object-cover" />
              <div className="min-w-0">
                <h1 className="truncate text-base font-semibold">{selectedCreator.nickname}</h1>
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
                collectionId={collectionId}
                onCollectionChange={setCollectionId}
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
                    .map((w) => ({ workId: w.id, title: w.title, creatorName: selectedCreator.nickname }));
                  if (playable.length > 0) openPlayer(playable, index);
                }}
              />
            ) : (
              <CollectionsPanel
                collections={collections.data}
                isPending={collections.isPending}
                onPick={(id) => {
                  setCollectionId(id);
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
    </div>
  );
}

/** 博主卡片:基础信息 + 已下载数 + SSE 扫描实时进度(scan-store)。 */
function CreatorCard({
  creator,
  active,
  live,
  onClick,
  onRescan,
  rescanPending,
}: {
  creator: { id: number; nickname: string; avatar_url: string; works_count: number; downloaded_count: number; download_bytes: number };
  active: boolean;
  live: LiveScan | undefined;
  onClick: () => void;
  onRescan: () => void;
  rescanPending: boolean;
}) {
  const scanning = Boolean(live?.running);
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
    >
      <div className="flex items-center gap-2.5">
        <img src={creator.avatar_url} alt="" className="size-9 shrink-0 rounded-full object-cover" />
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium" title={creator.nickname}>{creator.nickname}</p>
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
      </div>

      {live?.running ? (
        <div className="mt-2.5 space-y-1">
          <Progress value={Math.min(92, live.page * 11)} className="h-1" />
          <p className="text-[11px] text-sky-400">
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
