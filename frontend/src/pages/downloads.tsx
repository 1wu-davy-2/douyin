/**
 * 下载页:
 * - 统计卡行(排队/进行中/失败/已完成)+ 全局暂停/恢复 Switch + 并发显示(来自 /api/downloads/summary + SSE)
 * - Tabs:进行中(queued/downloading/paused_q:进度条 + MB/s + 取消)/ 失败(重试 + 批量重试 + 重试全部)/
 *   已完成(游标分页"加载更多" + 播放 + 删除记录 + 清空已完成)
 * - 行内多选 + 批量操作 DropdownMenu(重试/取消/删除 → POST /api/downloads/batch)
 * 实时进度由 SSE(download.progress)精准 patch 缓存,状态翻转与统计由 download.status 失效驱动,无轮询。
 */
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  CheckCircle2,
  ChevronDown,
  DownloadCloud,
  Inbox,
  Play,
  RotateCcw,
  Timer,
  Trash2,
  X,
} from "lucide-react";
import {
  batchDownloads,
  cancelDownload,
  clearCompletedDownloads,
  deleteDownload,
  pauseQueue,
  resumeQueue,
  retryDownload,
  retryFailedDownloads,
} from "../api/endpoints";
import { qk, useDownloads, useDownloadsSummary } from "../api/queries";
import type { DownloadsSummary, Job, JobStatus } from "../api/types";
import { ConfirmDialog } from "../components/confirm-dialog";
import { EmptyState } from "../components/empty-state";
import { JobStatusBadge } from "../components/status-badge";
import { Button } from "../components/ui/button";
import { Card } from "../components/ui/card";
import { Checkbox } from "../components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../components/ui/dropdown-menu";
import { Progress } from "../components/ui/progress";
import { Skeleton } from "../components/ui/skeleton";
import { Switch } from "../components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "../components/ui/tabs";
import { formatBytes, formatDateTime, formatPercent, formatSpeed } from "../lib/format";
import { openPlayer } from "../lib/player-store";
import { cn, toggleInSet, unionIntoSet } from "../lib/utils";

type TabKey = "active" | "failed" | "done";

const TAB_FILTERS: Record<TabKey, JobStatus[]> = {
  active: ["queued", "downloading", "paused_q"],
  failed: ["failed"],
  done: ["succeeded", "canceled"],
};

const TAB_LABELS: Record<TabKey, string> = {
  active: "进行中",
  failed: "失败",
  done: "已完成",
};

type ConfirmTarget = { kind: "delete-job"; job: Job } | { kind: "clear-completed" } | null;

export function DownloadsPage() {
  const queryClient = useQueryClient();
  const summary = useDownloadsSummary();

  const [tab, setTab] = useState<TabKey>("active");
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [confirmTarget, setConfirmTarget] = useState<ConfirmTarget>(null);

  const downloads = useDownloads({ status: TAB_FILTERS[tab], limit: 50 });
  const jobs = useMemo(() => downloads.data?.pages.flatMap((p) => p.items) ?? [], [downloads.data]);

  // 切 Tab 清空选择,避免跨状态批量误操作
  useEffect(() => {
    setSelected(new Set());
  }, [tab]);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: qk.downloadsAll });
    void queryClient.invalidateQueries({ queryKey: qk.downloadsSummary });
  };

  // ---------- 操作 ----------
  const cancelMut = useMutation({
    mutationFn: cancelDownload,
    onSuccess: () => invalidate(),
    onError: (e) => toast.error("取消失败", { description: e.message }),
  });
  const retryMut = useMutation({
    mutationFn: (id: number) => retryDownload(id),
    onSuccess: () => {
      toast.success("已重新排队");
      invalidate();
    },
    onError: (e) => toast.error("重试失败", { description: e.message }),
  });
  const deleteMut = useMutation({
    mutationFn: deleteDownload,
    onSuccess: () => {
      toast.success("已删除任务记录");
      invalidate();
    },
    onError: (e) => toast.error("删除失败", { description: e.message }),
  });
  const batchMut = useMutation({
    mutationFn: ({ action, ids }: { action: "retry" | "cancel" | "delete"; ids: number[] }) =>
      batchDownloads(action, ids),
    onSuccess: (res, vars) => {
      toast.success(`批量${{ retry: "重试", cancel: "取消", delete: "删除" }[vars.action]}完成`, {
        description: `已影响 ${res.affected} 条`,
      });
      setSelected(new Set());
      invalidate();
    },
    onError: (e) => toast.error("批量操作失败", { description: e.message }),
  });
  const retryFailedMut = useMutation({
    mutationFn: retryFailedDownloads,
    onSuccess: (res) => {
      toast.success(res.affected > 0 ? `已重试 ${res.affected} 个失败任务` : "没有可重试的失败任务");
      invalidate();
    },
    onError: (e) => toast.error("重试失败任务失败", { description: e.message }),
  });
  const clearCompletedMut = useMutation({
    mutationFn: clearCompletedDownloads,
    onSuccess: (res) => {
      toast.success(`已清空 ${res.affected} 条已完成记录`);
      setSelected(new Set());
      invalidate();
    },
    onError: (e) => toast.error("清空失败", { description: e.message }),
  });
  const pauseMut = useMutation({
    mutationFn: (paused: boolean) => (paused ? pauseQueue() : resumeQueue()),
    onSuccess: (res) => {
      toast.success(res.paused ? "已暂停派发(进行中任务不受影响)" : "已恢复派发");
      queryClient.setQueryData(qk.downloadsSummary, res);
    },
    onError: (e) => toast.error("操作失败", { description: e.message }),
  });

  // ---------- 选择 ----------
  const toggleRow = (id: number) => setSelected((prev) => toggleInSet(prev, id));
  const selectAll = () =>
    setSelected((prev) =>
      prev.size === jobs.length && jobs.every((j) => prev.has(j.id)) ? new Set() : unionIntoSet(prev, jobs.map((j) => j.id)),
    );

  const playable = jobs.filter((j) => j.status === "succeeded");
  const playIndex = (job: Job) => playable.findIndex((j) => j.id === job.id);
  const play = (job: Job) => {
    const idx = playIndex(job);
    if (idx >= 0) {
      openPlayer(
        playable.map((j) => ({ workId: j.work_id, title: j.work_title, creatorName: j.creator_nickname })),
        idx,
      );
    }
  };

  const tabCount = (s: DownloadsSummary | undefined, key: TabKey): number => {
    if (!s) return 0;
    if (key === "active") return s.queued + s.downloading;
    if (key === "failed") return s.failed;
    return s.succeeded;
  };

  const s = summary.data;

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      {/* 统计卡行 */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
        <StatCard label="排队" value={s?.queued} icon={Timer} active={tab === "active"} onClick={() => setTab("active")} />
        <StatCard label="进行中" value={s?.downloading} icon={DownloadCloud} active={tab === "active"} onClick={() => setTab("active")} />
        <StatCard label="失败" value={s?.failed} icon={X} tone="destructive" active={tab === "failed"} onClick={() => setTab("failed")} />
        <StatCard label="已完成" value={s?.succeeded} icon={CheckCircle2} tone="success" active={tab === "done"} onClick={() => setTab("done")} />

        <Card className="flex items-center justify-between gap-3 p-4">
          <div className="min-w-0">
            <p className="text-sm font-medium">{s?.paused ? "已暂停派发" : "全局暂停"}</p>
            <p className="text-[11px] text-muted-foreground">
              并发 {s?.concurrency ?? "—"} · 暂停不影响进行中
            </p>
          </div>
          <Switch
            aria-label="全局暂停/恢复"
            checked={s?.paused ?? false}
            disabled={pauseMut.isPending || summary.isPending}
            onCheckedChange={(paused) => pauseMut.mutate(paused)}
          />
        </Card>
      </div>

      {/* Tabs + 工具行 */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Tabs value={tab} onValueChange={(v) => setTab(v as TabKey)}>
          <TabsList>
            {(Object.keys(TAB_LABELS) as TabKey[]).map((key) => (
              <TabsTrigger key={key} value={key}>
                {TAB_LABELS[key]}
                {tabCount(s, key) > 0 ? `(${tabCount(s, key)})` : ""}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>

        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">
            已选 <span className="font-medium text-foreground tabular-nums">{selected.size}</span> 条
          </span>

          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="outline" size="sm" disabled={selected.size === 0}>
                批量操作 <ChevronDown className="size-3.5" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem disabled={batchMut.isPending} onClick={() => batchMut.mutate({ action: "retry", ids: [...selected] })}>
                <RotateCcw /> 重试选中
              </DropdownMenuItem>
              <DropdownMenuItem disabled={batchMut.isPending} onClick={() => batchMut.mutate({ action: "cancel", ids: [...selected] })}>
                <X /> 取消选中
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem destructive disabled={batchMut.isPending} onClick={() => batchMut.mutate({ action: "delete", ids: [...selected] })}>
                <Trash2 /> 删除选中
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>

          {tab === "failed" ? (
            <Button variant="outline" size="sm" disabled={retryFailedMut.isPending} onClick={() => retryFailedMut.mutate()}>
              <RotateCcw className="size-3.5" /> 重试全部
            </Button>
          ) : null}
          {tab === "done" ? (
            <Button variant="outline" size="sm" onClick={() => setConfirmTarget({ kind: "clear-completed" })}>
              <Trash2 className="size-3.5" /> 清空已完成
            </Button>
          ) : null}
        </div>
      </div>

      {/* 任务表 */}
      <Card className="overflow-hidden">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="w-10 pr-0">
                <Checkbox
                  aria-label="全选当前列表"
                  checked={jobs.length > 0 && selected.size >= jobs.length && jobs.every((j) => selected.has(j.id))}
                  disabled={jobs.length === 0}
                  onCheckedChange={selectAll}
                />
              </TableHead>
              <TableHead>作品</TableHead>
              <TableHead className="w-24">状态</TableHead>
              <TableHead className="w-56">{tab === "done" ? "大小" : "进度"}</TableHead>
              {tab === "done" ? <TableHead className="w-20">画质</TableHead> : <TableHead className="w-24">速度</TableHead>}
              <TableHead className="w-36">{tab === "done" ? "完成时间" : "排队时间"}</TableHead>
              <TableHead className="w-24 text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {downloads.isPending ? (
              Array.from({ length: 5 }, (_, i) => (
                <TableRow key={i}>
                  <TableCell className="pr-0"><Skeleton className="size-4" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-48" /></TableCell>
                  <TableCell><Skeleton className="h-5 w-14" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-full" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-14" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-24" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-10" /></TableCell>
                </TableRow>
              ))
            ) : jobs.length === 0 ? (
              <TableRow>
                <TableCell colSpan={7} className="p-0">
                  <EmptyState
                    className="border-0"
                    icon={tab === "active" ? DownloadCloud : tab === "failed" ? CheckCircle2 : Inbox}
                    title={
                      tab === "active" ? "队列空闲" : tab === "failed" ? "没有失败的任务" : "暂无已完成记录"
                    }
                    description={
                      tab === "active"
                        ? "从作品库选择作品开始下载"
                        : tab === "failed"
                          ? "所有任务都在正常运行"
                          : "下载完成的记录会出现在这里"
                    }
                    action={
                      tab === "active" ? (
                        <Button variant="outline" size="sm" asChild>
                          <Link to="/">去作品库</Link>
                        </Button>
                      ) : undefined
                    }
                  />
                </TableCell>
              </TableRow>
            ) : (
              jobs.map((job) => (
                <TableRow key={job.id} data-state={selected.has(job.id) ? "selected" : undefined}>
                  <TableCell className="pr-0">
                    <Checkbox aria-label={`选择 ${job.work_title}`} checked={selected.has(job.id)} onCheckedChange={() => toggleRow(job.id)} />
                  </TableCell>
                  <TableCell className="max-w-0">
                    <p className="max-w-[320px] truncate" title={job.work_title}>{job.work_title}</p>
                    <p className="text-xs text-muted-foreground">{job.creator_nickname}</p>
                  </TableCell>
                  <TableCell>
                    <JobStatusBadge status={job.status} />
                    {job.status === "failed" && job.attempts > 1 ? (
                      <p className="mt-0.5 text-[11px] text-muted-foreground">已尝试 {job.attempts} 次</p>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    {tab === "done" ? (
                      <span className="tabular-nums text-muted-foreground">{formatBytes(job.total_bytes)}</span>
                    ) : job.status === "downloading" ? (
                      <div className="space-y-1">
                        <Progress value={formatPercent(job.downloaded_bytes, job.total_bytes)} className="h-1.5" />
                        <p className="text-[11px] tabular-nums text-muted-foreground">
                          {formatBytes(job.downloaded_bytes)} / {formatBytes(job.total_bytes)}
                        </p>
                      </div>
                    ) : job.status === "queued" ? (
                      <span className="text-xs text-muted-foreground">等待派发…</span>
                    ) : (
                      <span className="text-xs text-muted-foreground">已暂停</span>
                    )}
                  </TableCell>
                  {tab === "done" ? (
                    <TableCell className="text-xs tabular-nums text-muted-foreground">{job.quality}</TableCell>
                  ) : (
                    <TableCell className="text-xs tabular-nums text-muted-foreground">
                      {job.status === "downloading" ? formatSpeed(job.speed_bps) : "—"}
                    </TableCell>
                  )}
                  <TableCell className="text-xs tabular-nums text-muted-foreground">
                    {tab === "done" ? formatDateTime(job.finished_at) : formatDateTime(job.queued_at)}
                    {job.status === "failed" && job.error ? (
                      <p className="mt-0.5 max-w-40 truncate text-[11px] text-destructive" title={job.error}>
                        {job.error}
                      </p>
                    ) : null}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-1">
                      {job.status === "downloading" || job.status === "queued" ? (
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title="取消"
                          disabled={cancelMut.isPending}
                          onClick={() => cancelMut.mutate(job.id)}
                        >
                          <X className="size-4" />
                        </Button>
                      ) : null}
                      {job.status === "paused_q" ? (
                        <span className="text-[11px] text-muted-foreground">等待恢复</span>
                      ) : null}
                      {job.status === "failed" ? (
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title="重试"
                          disabled={retryMut.isPending}
                          onClick={() => retryMut.mutate(job.id)}
                        >
                          <RotateCcw className="size-4" />
                        </Button>
                      ) : null}
                      {job.status === "succeeded" ? (
                        <Button variant="ghost" size="icon-sm" title="播放" onClick={() => play(job)}>
                          <Play className="size-4" />
                        </Button>
                      ) : null}
                      {job.status === "succeeded" || job.status === "canceled" || job.status === "failed" ? (
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title="删除记录"
                          onClick={() => setConfirmTarget({ kind: "delete-job", job })}
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      ) : null}
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      {/* 加载更多(游标分页) */}
      {downloads.hasNextPage ? (
        <div className="flex justify-center">
          <Button variant="outline" disabled={downloads.isFetchingNextPage} onClick={() => void downloads.fetchNextPage()}>
            {downloads.isFetchingNextPage ? "加载中…" : "加载更多"}
          </Button>
        </div>
      ) : null}

      <ConfirmDialog
        open={confirmTarget !== null}
        onOpenChange={(open) => (open ? undefined : setConfirmTarget(null))}
        title={confirmTarget?.kind === "delete-job" ? "删除任务记录" : "清空已完成记录"}
        description={
          confirmTarget?.kind === "delete-job"
            ? `将删除「${confirmTarget.job.work_title}」的任务记录(不删除已下载文件)。`
            : "将删除全部“已完成”与“已取消”的任务记录(不删除已下载文件),此操作不可撤销。"
        }
        confirmLabel={confirmTarget?.kind === "delete-job" ? "删除" : "清空"}
        destructive
        loading={deleteMut.isPending || clearCompletedMut.isPending}
        onConfirm={() => {
          if (!confirmTarget) return;
          if (confirmTarget.kind === "delete-job") {
            deleteMut.mutate(confirmTarget.job.id, { onSettled: () => setConfirmTarget(null) });
          } else {
            clearCompletedMut.mutate(undefined, { onSettled: () => setConfirmTarget(null) });
          }
        }}
      />
    </div>
  );
}

function StatCard({
  label,
  value,
  icon: Icon,
  tone = "default",
  active,
  onClick,
}: {
  label: string;
  value: number | undefined;
  icon: typeof Timer;
  tone?: "default" | "destructive" | "success";
  active: boolean;
  onClick: () => void;
}) {
  return (
    <Card
      onClick={onClick}
      className={cn(
        "cursor-pointer p-4 transition-colors hover:bg-accent/50",
        active && "border-primary/60 bg-accent/60",
      )}
    >
      <div className="flex items-center justify-between gap-2">
        <div>
          <p className="text-2xl font-semibold tabular-nums">{value ?? "—"}</p>
          <p className="text-xs text-muted-foreground">{label}</p>
        </div>
        <Icon
          className={cn(
            "size-4",
            tone === "destructive" ? "text-destructive" : tone === "success" ? "text-emerald-400" : "text-muted-foreground",
          )}
        />
      </div>
    </Card>
  );
}
