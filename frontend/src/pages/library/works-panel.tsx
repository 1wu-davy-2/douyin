/**
 * 作品 Tab:搜索(防抖)+ 合集/类型/下载状态筛选 + 排序 + 作品表格 + 服务端分页 + 底部下载操作栏。
 * 选中集合由 LibraryPage 持有(可跨页/跨筛选),此处只渲染与回调。
 */
import type { Page, Quality, Work, WorkDlFilter, WorkSort, WorkTypeFilter, Collection } from "../../api/types";
import { QUALITIES } from "../../api/types";
import { DlStatusBadge } from "../../components/status-badge";
import { EmptyState } from "../../components/empty-state";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Checkbox } from "../../components/ui/checkbox";
import { Input } from "../../components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../components/ui/select";
import { Skeleton } from "../../components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { formatDuration, formatDate } from "../../lib/format";
import { Link } from "react-router";
import { Download, FileVideo, Film, Image as ImageIcon, ListFilter, Play, RefreshCw, Search, Sunrise, Trash2, X } from "lucide-react";
import { Pager } from "./pager";

interface WorksPanelProps {
  works: Page<Work> | undefined;
  isPending: boolean;
  isFetching: boolean;
  collections: Collection[];
  /** 筛选/排序/分页(受控,状态在 LibraryPage,切 Tab 不丢) */
  q: string;
  onQChange: (q: string) => void;
  collectionId: number | null | "none";
  onCollectionChange: (id: number | null | "none") => void;
  /** 契约 v1.2:type 筛选(null=全部类型;live=含动图片段的图集) */
  type: WorkTypeFilter | null;
  onTypeChange: (type: WorkTypeFilter | null) => void;
  /** 契约 v1.2:dl_status 筛选(null=全部状态) */
  dl: WorkDlFilter | null;
  onDlChange: (dl: WorkDlFilter | null) => void;
  sort: WorkSort;
  onSortChange: (sort: WorkSort) => void;
  page: number;
  pageSize: number;
  onPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
  /** 选择集合 */
  selected: ReadonlySet<number>;
  pageItems: Work[];
  onToggleRow: (id: number) => void;
  onSelectPage: () => void;
  onClearSelection: () => void;
  onSelectAllFiltered: () => void;
  selectAllPending: boolean;
  /** 底部操作栏 */
  quality: Quality;
  onQualityChange: (q: Quality) => void;
  onDownload: () => void;
  downloadPending: boolean;
  /** 删除已选(含已下载文件) */
  onDelete: () => void;
  deletePending: boolean;
  /** 重新下载(清旧资产按新格式重下) */
  onRedownload: () => void;
  redownloadPending: boolean;
  /** 播放 */
  onPlay: (index: number) => void;
}

const SORT_OPTIONS: { value: WorkSort; label: string }[] = [
  { value: "published_at_desc", label: "最新发布" },
  { value: "published_at_asc", label: "最早发布" },
  { value: "duration_desc", label: "时长最长" },
];

const TYPE_OPTIONS: { value: WorkTypeFilter; label: string }[] = [
  { value: "video", label: "视频" },
  { value: "image", label: "图集" },
  { value: "live", label: "动图" },
  { value: "daily", label: "日常" },
];

const DL_OPTIONS: { value: WorkDlFilter; label: string }[] = [
  { value: "none", label: "未下载" },
  { value: "queued", label: "排队中" },
  { value: "downloading", label: "下载中" },
  { value: "succeeded", label: "已下载" },
  { value: "failed", label: "失败" },
];

export function WorksPanel(props: WorksPanelProps) {
  const {
    works, isPending, isFetching, collections,
    q, onQChange, collectionId, onCollectionChange, type, onTypeChange, dl, onDlChange, sort, onSortChange,
    page, pageSize, onPageChange, onPageSizeChange,
    selected, pageItems, onToggleRow, onSelectPage, onClearSelection, onSelectAllFiltered, selectAllPending,
    quality, onQualityChange, onDownload, downloadPending, onPlay,
    onDelete, deletePending, onRedownload, redownloadPending,
  } = props;

  const total = works?.total ?? 0;
  const pageSelected = pageItems.filter((w) => selected.has(w.id)).length;
  const allPageSelected = pageItems.length > 0 && pageSelected === pageItems.length;
  const somePageSelected = pageSelected > 0 && !allPageSelected;
  const hasFilter = q !== "" || collectionId !== null || type !== null || dl !== null;

  // 可播放列表 = 当前页已下载作品(播放器内保持列表顺序)
  const playable = pageItems.filter((w) => w.dl_status === "succeeded");
  const playIndex = (work: Work) => playable.findIndex((w) => w.id === work.id);

  return (
    <div className="space-y-3">
      {/* 工具栏:搜索 / 合集筛选 / 排序 */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative w-64 max-w-full">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={q}
            onChange={(e) => onQChange(e.target.value)}
            placeholder="搜索标题 / item_id"
            className="pl-8"
            aria-label="搜索作品"
          />
        </div>
        <Select
          value={collectionId === null ? "all" : collectionId === "none" ? "none" : String(collectionId)}
          onValueChange={(v) =>
            onCollectionChange(v === "all" ? null : v === "none" ? "none" : Number(v))
          }
        >
          <SelectTrigger className="w-44" aria-label="按合集筛选">
            <ListFilter className="size-3.5 text-muted-foreground" />
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部合集</SelectItem>
            <SelectItem value="none">单发作品(排除合集)</SelectItem>
            {collections.map((c) => (
              <SelectItem key={c.id} value={String(c.id)}>
                {c.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={type === null ? "all" : type}
          onValueChange={(v) => onTypeChange(v === "all" ? null : (v as WorkTypeFilter))}
        >
          <SelectTrigger className="w-32" aria-label="按类型筛选">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部类型</SelectItem>
            {TYPE_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={dl === null ? "all" : dl}
          onValueChange={(v) => onDlChange(v === "all" ? null : (v as WorkDlFilter))}
        >
          <SelectTrigger className="w-32" aria-label="按下载状态筛选">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部状态</SelectItem>
            {DL_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={sort} onValueChange={(v) => onSortChange(v as WorkSort)}>
          <SelectTrigger className="w-36" aria-label="排序">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {SORT_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {/* 表格 */}
      <div className="overflow-hidden rounded-xl border border-border bg-card" data-fetching={isFetching || undefined}>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="w-10 pr-0">
                <Checkbox
                  aria-label="全选当前页"
                  checked={allPageSelected ? true : somePageSelected ? "indeterminate" : false}
                  disabled={pageItems.length === 0}
                  onCheckedChange={() => onSelectPage()}
                />
              </TableHead>
              <TableHead className="w-24">封面</TableHead>
              <TableHead>标题</TableHead>
              <TableHead className="w-20">时长</TableHead>
              <TableHead className="w-28">发布日期</TableHead>
              <TableHead className="w-28">下载状态</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isPending ? (
              Array.from({ length: 8 }, (_, i) => (
                <TableRow key={i}>
                  <TableCell className="pr-0"><Skeleton className="size-4" /></TableCell>
                  <TableCell><Skeleton className="h-11 w-20 rounded-md" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-2/3" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-10" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-20" /></TableCell>
                  <TableCell><Skeleton className="h-5 w-16" /></TableCell>
                </TableRow>
              ))
            ) : pageItems.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="p-0">
                  <EmptyState
                    icon={FileVideo}
                    className="border-0"
                    title={hasFilter ? "没有匹配的作品" : "还没有作品"}
                    description={
                      hasFilter
                        ? "试试调整搜索关键词或筛选条件"
                        : "该博主还没有扫描到作品,可在左侧重新添加或等待扫描完成"
                    }
                  />
                </TableCell>
              </TableRow>
            ) : (
              pageItems.map((w) => (
                <TableRow key={w.id} data-state={selected.has(w.id) ? "selected" : undefined}>
                  <TableCell className="pr-0">
                    <Checkbox
                      aria-label={`选择 ${w.title}`}
                      checked={selected.has(w.id)}
                      onCheckedChange={() => onToggleRow(w.id)}
                    />
                  </TableCell>
                  <TableCell>
                    {w.dl_status === "succeeded" ? (
                      <button
                        type="button"
                        title="播放"
                        onClick={() => onPlay(playIndex(w))}
                        className="group relative block w-20 overflow-hidden rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring"
                      >
                        <img src={w.cover_url} alt="" className="aspect-video w-20 object-cover" loading="lazy" />
                        <span className="absolute inset-0 hidden items-center justify-center bg-black/45 text-white group-hover:flex">
                          <Play className="size-5" />
                        </span>
                      </button>
                    ) : (
                      <img src={w.cover_url} alt="" className="aspect-video w-20 rounded-md object-cover opacity-80" loading="lazy" />
                    )}
                  </TableCell>
                  <TableCell className="max-w-0">
                    <div className="flex items-center gap-1.5">
                      <p className="max-w-[380px] truncate" title={w.title}>{w.title}</p>
                      {w.type === "daily" ? (
                        <Badge variant="secondary" className="shrink-0">
                          <Sunrise /> 日常
                        </Badge>
                      ) : w.type === "image" ? (
                        <Badge variant="secondary" className="shrink-0">
                          <ImageIcon /> 图集 {w.image_count} 张
                        </Badge>
                      ) : (
                        <Badge variant="muted" className="shrink-0">
                          <Film /> 视频
                        </Badge>
                      )}
                    </div>
                    {w.collection_name ? (
                      <p className="text-xs text-muted-foreground">{w.collection_name}</p>
                    ) : null}
                  </TableCell>
                  <TableCell className="tabular-nums text-muted-foreground">
                    {w.type === "image" ? "—" : formatDuration(w.duration)}
                  </TableCell>
                  <TableCell className="tabular-nums text-muted-foreground">{formatDate(w.published_at)}</TableCell>
                  <TableCell><DlStatusBadge status={w.dl_status} quality={w.downloaded_quality} /></TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>

      {/* 选择工具行 */}
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <span>
          已选 <span className="font-medium text-foreground tabular-nums">{selected.size}</span> 条
          {selected.size > 0 ? <span className="ml-1">(可能包含未在当前页展示的条目)</span> : null}
        </span>
        <Button variant="outline" size="sm" disabled={pageItems.length === 0} onClick={onSelectPage}>
          全选当前页
        </Button>
        <Button variant="ghost" size="sm" disabled={selected.size === 0} onClick={onClearSelection}>
          <X className="size-3.5" /> 清除
        </Button>
        <Button variant="ghost" size="sm" disabled={selectAllPending || total === 0} onClick={onSelectAllFiltered}>
          按筛选全选({total})
        </Button>
        <Button variant="outline" size="sm" disabled={redownloadPending || selected.size === 0} onClick={onRedownload}>
          <RefreshCw className="size-3.5" /> 重新下载
        </Button>
        <Button variant="destructive" size="sm" disabled={deletePending || selected.size === 0} onClick={onDelete}>
          <Trash2 className="size-3.5" /> 删除已选
        </Button>
      </div>

      <Pager page={page} pageSize={pageSize} total={total} onPageChange={onPageChange} onPageSizeChange={onPageSizeChange} />

      {/* 底部操作栏 */}
      <div className="sticky bottom-0 flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card/95 px-4 py-3 shadow-sm backdrop-blur">
        <span className="text-sm text-muted-foreground">
          画质
          <Select value={quality} onValueChange={(v) => onQualityChange(v as Quality)}>
            <SelectTrigger className="ml-2 inline-flex h-8 w-28" aria-label="下载画质">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {QUALITIES.map((q2) => (
                <SelectItem key={q2} value={q2}>{q2}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        </span>
        <Button onClick={onDownload} disabled={downloadPending || selected.size === 0} className="ml-auto">
          <Download className="size-4" />
          {downloadPending ? "创建中…" : `下载已选 ${selected.size} 条`}
        </Button>
        <Link to="/downloads" className="text-sm text-sky-400 underline-offset-4 hover:underline">
          查看队列
        </Link>
      </div>
    </div>
  );
}
