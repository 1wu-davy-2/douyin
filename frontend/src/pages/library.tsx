/**
 * 作品库页(核心):
 * 左列:博主卡片(头像/昵称/作品数/已下载数),SSE scan.progress 驱动扫描进度条;
 *       "添加博主"输入框 → POST /api/creators(202)→ scan-store 记录 → scan.done 刷新并 toast。
 * 右列:选中博主详情,Tabs(作品/合集)。
 */
import { useEffect, useMemo, useState, useSyncExternalStore, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Library as LibraryIcon, Plus } from "lucide-react";
import { toast } from "sonner";
import { createCreator } from "../api/endpoints";
import { qk, useCollections, useCreators, useWorks } from "../api/queries";
import type { Quality, WorkSort } from "../api/types";
import { EmptyState } from "../components/empty-state";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Progress } from "../components/ui/progress";
import { Skeleton } from "../components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "../components/ui/tabs";
import { useDebouncedValue } from "../lib/use-debounced-value";
import { getScanSnapshot, subscribeScans, type LiveScan } from "../lib/scan-store";
import { openPlayer } from "../lib/player-store";
import { batchWorkIds, createDownloads } from "../api/endpoints";
import { cn, removeFromSet, toggleInSet, unionIntoSet } from "../lib/utils";
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
  const [sort, setSort] = useState<WorkSort>("published_at_desc");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const works = useWorks(selectedCreatorId, {
    page,
    page_size: pageSize,
    sort,
    q: q || undefined,
    collection_id: collectionId ?? undefined,
  });

  // 选择集合与画质
  const [selected, setSelected] = useState<Set<number>>(new Set());
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
    setSelected(new Set());
    setPage(1);
  }, [selectedCreatorId]);

  // 筛选条件变化回到第 1 页
  useEffect(() => {
    setPage(1);
  }, [q, collectionId, sort, pageSize]);

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
                  {selectedCreator.downloaded_count}
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
    </div>
  );
}

/** 博主卡片:基础信息 + 已下载数 + SSE 扫描实时进度(scan-store)。 */
function CreatorCard({
  creator,
  active,
  live,
  onClick,
}: {
  creator: { id: number; nickname: string; avatar_url: string; works_count: number; downloaded_count: number };
  active: boolean;
  live: LiveScan | undefined;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "w-full rounded-xl border p-3 text-left transition-colors",
        active ? "border-primary/60 bg-accent" : "border-border hover:bg-accent/50",
      )}
    >
      <div className="flex items-center gap-2.5">
        <img src={creator.avatar_url} alt="" className="size-9 shrink-0 rounded-full object-cover" />
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium" title={creator.nickname}>{creator.nickname}</p>
          <p className="text-[11px] text-muted-foreground">
            {creator.works_count} 作品 · 已下载 {creator.downloaded_count}
          </p>
        </div>
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
    </button>
  );
}
