/**
 * 监控订阅页:
 * 表格(目标/类型/间隔/自动下载/画质/上次运行/监控期间统计/启用 Switch)+ 新建 Dialog + 删除 confirm-dialog。
 * 启用开关走乐观更新(PATCH /api/subscriptions/{id})。
 * 新建订阅(契约 v1.4):博主模式为多选(带头像 + 搜索,逐个创建);合集模式保持单选。
 * 博主名统一显示 alias ?? 昵称(与作品库一致)。
 */
import { useEffect, useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Radar, Search, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { createSubscription, deleteSubscription, updateSubscription } from "../api/endpoints";
import { qk, useCollections, useCreators, useSubscriptions } from "../api/queries";
import type { Creator, Quality, Subscription, TargetType } from "../api/types";
import { QUALITIES } from "../api/types";
import { ConfirmDialog } from "../components/confirm-dialog";
import { EmptyState } from "../components/empty-state";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Card } from "../components/ui/card";
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Skeleton } from "../components/ui/skeleton";
import { Switch } from "../components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";
import { cn } from "../lib/utils";
import { formatDateTime } from "../lib/format";

/** 博主显示名:alias ?? nickname(与作品库一致)。 */
function creatorDisplayName(c: Creator): string {
  return c.alias ?? c.nickname;
}

export function SubscriptionsPage() {
  const queryClient = useQueryClient();
  const subscriptions = useSubscriptions();
  const creators = useCreators();
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Subscription | null>(null);

  // 表格里把订阅的博主名统一解析为 alias ?? 昵称
  const creatorById = useMemo(() => {
    const map = new Map<number, Creator>();
    for (const c of creators.data ?? []) map.set(c.id, c);
    return map;
  }, [creators.data]);
  const creatorNameOf = (sub: Subscription): string => {
    const c = creatorById.get(sub.creator_id);
    return c ? creatorDisplayName(c) : sub.target_name || sub.creator_nickname;
  };

  const enableMut = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) => updateSubscription(id, { enabled }),
    // 乐观更新:开关即时生效,失败回滚
    onMutate: async ({ id, enabled }) => {
      await queryClient.cancelQueries({ queryKey: qk.subscriptions });
      const prev = queryClient.getQueryData<Subscription[]>(qk.subscriptions);
      queryClient.setQueryData<Subscription[]>(qk.subscriptions, (old) =>
        old?.map((s) => (s.id === id ? { ...s, enabled } : s)),
      );
      return { prev };
    },
    onError: (e, _vars, ctx) => {
      if (ctx?.prev) queryClient.setQueryData(qk.subscriptions, ctx.prev);
      toast.error("更新订阅失败", { description: e.message });
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.subscriptions }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteSubscription(id),
    onSuccess: () => {
      toast.success("订阅已删除");
      setDeleteTarget(null);
      queryClient.invalidateQueries({ queryKey: qk.subscriptions });
    },
    onError: (e) => toast.error("删除订阅失败", { description: e.message }),
  });

  const subs = subscriptions.data ?? [];

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-base font-semibold">监控订阅</h1>
          <p className="text-xs text-muted-foreground">按间隔自动扫描博主/合集,可自动下载新作品</p>
        </div>
        <Button onClick={() => setCreateOpen(true)}>
          <Radar className="size-4" /> 新建订阅
        </Button>
      </div>

      <Card className="overflow-hidden">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>目标</TableHead>
              <TableHead className="w-20">类型</TableHead>
              <TableHead className="w-24">间隔</TableHead>
              <TableHead className="w-24">自动下载</TableHead>
              <TableHead className="w-20">画质</TableHead>
              <TableHead className="w-36">上次运行</TableHead>
              <TableHead
                className="w-32"
                title="自该订阅创建起,监控扫描新收录的作品数,及其中已下载(成功)的作品数。每 30 秒自动刷新。"
              >
                监控期间
              </TableHead>
              <TableHead className="w-16">启用</TableHead>
              <TableHead className="w-16 text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {subscriptions.isPending ? (
              Array.from({ length: 3 }, (_, i) => (
                <TableRow key={i}>
                  <TableCell><Skeleton className="h-4 w-40" /></TableCell>
                  <TableCell><Skeleton className="h-5 w-12" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-14" /></TableCell>
                  <TableCell><Skeleton className="h-5 w-10" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-12" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-24" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-20" /></TableCell>
                  <TableCell><Skeleton className="h-5 w-9" /></TableCell>
                  <TableCell><Skeleton className="h-4 w-8" /></TableCell>
                </TableRow>
              ))
            ) : subs.length === 0 ? (
              <TableRow>
                <TableCell colSpan={9} className="p-0">
                  <EmptyState
                    className="border-0"
                    icon={Radar}
                    title="还没有订阅"
                    description="创建订阅后,工具会按设定间隔自动扫描目标并按需下载新作品"
                    action={
                      <Button size="sm" onClick={() => setCreateOpen(true)}>
                        新建订阅
                      </Button>
                    }
                  />
                </TableCell>
              </TableRow>
            ) : (
              subs.map((sub) => (
                <TableRow key={sub.id}>
                  <TableCell>
                    <p className="max-w-56 truncate font-medium" title={creatorNameOf(sub)}>{creatorNameOf(sub)}</p>
                    <p className="text-xs text-muted-foreground">
                      {sub.target_type === "collection" ? `博主:${creatorNameOf(sub)}` : "整主页"}
                    </p>
                  </TableCell>
                  <TableCell>
                    <Badge variant={sub.target_type === "creator" ? "outline" : "secondary"}>
                      {sub.target_type === "creator" ? "博主" : "合集"}
                    </Badge>
                  </TableCell>
                  <TableCell className="tabular-nums text-muted-foreground">{sub.interval_minutes} 分钟</TableCell>
                  <TableCell>
                    <Badge variant={sub.auto_download ? "success" : "muted"}>{sub.auto_download ? "开" : "关"}</Badge>
                  </TableCell>
                  <TableCell className="text-muted-foreground">{sub.quality}</TableCell>
                  <TableCell className="text-xs tabular-nums text-muted-foreground">{formatDateTime(sub.last_run_at)}</TableCell>
                  <TableCell className="text-xs tabular-nums text-muted-foreground">
                    {sub.new_works > 0
                      ? `新增 ${sub.new_works} · 已下载 ${sub.new_downloaded}`
                      : "-"}
                  </TableCell>
                  <TableCell>
                    <Switch
                      aria-label={`启用 ${creatorNameOf(sub)}`}
                      checked={sub.enabled}
                      disabled={enableMut.isPending}
                      onCheckedChange={(enabled) => enableMut.mutate({ id: sub.id, enabled })}
                    />
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      title="删除订阅"
                      onClick={() => setDeleteTarget(sub)}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <CreateSubscriptionDialog open={createOpen} onOpenChange={setCreateOpen} />

      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => (open ? undefined : setDeleteTarget(null))}
        title="删除订阅"
        description={`将删除对「${deleteTarget ? creatorNameOf(deleteTarget) : ""}」的监控订阅,已下载文件不受影响。`}
        confirmLabel="删除"
        destructive
        loading={deleteMut.isPending}
        onConfirm={() => {
          if (deleteTarget) deleteMut.mutate(deleteTarget.id);
        }}
      />
    </div>
  );
}

interface CreateFormState {
  targetType: TargetType;
  /** 博主模式:多选的博主 id 列表(每位各建一条订阅)。 */
  creatorIds: number[];
  /** 合集模式:单选所属博主。 */
  creatorId: string;
  collectionId: string;
  intervalMinutes: string;
  autoDownload: boolean;
  quality: Quality;
}

const INITIAL_FORM: CreateFormState = {
  targetType: "creator",
  creatorIds: [],
  creatorId: "",
  collectionId: "",
  intervalMinutes: "60",
  autoDownload: true,
  quality: "1080p",
};

function CreateSubscriptionDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const queryClient = useQueryClient();
  const creators = useCreators();
  const [form, setForm] = useState<CreateFormState>(INITIAL_FORM);
  const [error, setError] = useState<string | null>(null);
  const [creatorSearch, setCreatorSearch] = useState("");

  // 打开时重置表单
  useEffect(() => {
    if (open) {
      setForm(INITIAL_FORM);
      setError(null);
      setCreatorSearch("");
    }
  }, [open]);

  const creatorIdNum = Number(form.creatorId) || 0;
  const collections = useCollections(form.targetType === "collection" && creatorIdNum > 0 ? creatorIdNum : null);

  // 多选列表:按 别名/昵称 过滤
  const creatorItems = useMemo(() => {
    const kw = creatorSearch.trim().toLowerCase();
    return (creators.data ?? []).filter(
      (c) =>
        kw === "" ||
        c.nickname.toLowerCase().includes(kw) ||
        (c.alias ?? "").toLowerCase().includes(kw),
    );
  }, [creators.data, creatorSearch]);

  const createMut = useMutation({
    mutationFn: async (ids: number[]) => {
      // 多选博主:每位各创建一条订阅;单条失败不影响其他
      return Promise.allSettled(
        ids.map((id) =>
          createSubscription({
            target_type: form.targetType,
            creator_id: id,
            ...(form.targetType === "collection" ? { collection_id: Number(form.collectionId) } : {}),
            interval_minutes: Number(form.intervalMinutes),
            auto_download: form.autoDownload,
            quality: form.quality,
          }),
        ),
      );
    },
    onSuccess: (results) => {
      const okCount = results.filter((r) => r.status === "fulfilled").length;
      const failCount = results.length - okCount;
      if (failCount === 0) {
        toast.success(`已创建 ${okCount} 条订阅`, { description: `每 ${form.intervalMinutes} 分钟检查一次` });
      } else {
        const firstError = results.find((r): r is PromiseRejectedResult => r.status === "rejected");
        toast.warning(`已创建 ${okCount} 条订阅,${failCount} 条失败`, {
          description: firstError?.reason instanceof Error ? firstError.reason.message : undefined,
        });
      }
      onOpenChange(false);
      queryClient.invalidateQueries({ queryKey: qk.subscriptions });
    },
    onError: (e) => {
      setError(e.message);
      toast.error("创建订阅失败", { description: e.message });
    },
  });

  const toggleCreator = (id: number) => {
    setForm((f) => ({
      ...f,
      creatorIds: f.creatorIds.includes(id) ? f.creatorIds.filter((x) => x !== id) : [...f.creatorIds, id],
    }));
  };

  const handleSubmit = () => {
    setError(null);
    const interval = Number(form.intervalMinutes);
    if (!Number.isInteger(interval) || interval < 1 || interval > 10080) {
      setError("间隔需为 1 ~ 10080 之间的整数分钟");
      return;
    }
    if (form.targetType === "creator") {
      if (form.creatorIds.length === 0) {
        setError("请至少勾选一位博主");
        return;
      }
      createMut.mutate(form.creatorIds);
      return;
    }
    if (!creatorIdNum) {
      setError("请选择博主");
      return;
    }
    if (!form.collectionId) {
      setError("请选择合集");
      return;
    }
    createMut.mutate([creatorIdNum]);
  };

  const pending = createMut.isPending;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>新建订阅</DialogTitle>
          <DialogDescription>按设定间隔自动扫描目标,新作品可自动下载;博主可多选批量创建</DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label>目标类型</Label>
            <Select
              value={form.targetType}
              onValueChange={(v) => setForm((f) => ({ ...f, targetType: v as TargetType, collectionId: "" }))}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="creator">博主(整主页)</SelectItem>
                <SelectItem value="collection">合集</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {form.targetType === "creator" ? (
            <div className="space-y-1.5">
              <Label>博主(可多选,已选 {form.creatorIds.length} 位)</Label>
              <div className="relative">
                <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  value={creatorSearch}
                  onChange={(e) => setCreatorSearch(e.target.value)}
                  placeholder="搜索别名 / 昵称"
                  className="h-8 pl-8"
                  aria-label="搜索博主"
                />
              </div>
              <div className="max-h-56 space-y-0.5 overflow-y-auto rounded-lg border border-border p-1">
                {creators.isPending ? (
                  <div className="px-2 py-1.5 text-xs text-muted-foreground">加载中…</div>
                ) : creatorItems.length === 0 ? (
                  <div className="px-2 py-1.5 text-xs text-muted-foreground">
                    {(creators.data ?? []).length === 0 ? "还没有博主,请先在作品库添加" : "没有匹配的博主"}
                  </div>
                ) : (
                  creatorItems.map((c) => {
                    const checked = form.creatorIds.includes(c.id);
                    return (
                      <label
                        key={c.id}
                        className={cn(
                          "flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 transition-colors hover:bg-accent/60",
                          checked && "bg-accent/80",
                        )}
                      >
                        <Checkbox checked={checked} onCheckedChange={() => toggleCreator(c.id)} />
                        <img src={c.avatar_url} alt="" className="size-7 shrink-0 rounded-full object-cover" />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-sm">{creatorDisplayName(c)}</span>
                          {c.alias ? (
                            <span className="block truncate text-[11px] text-muted-foreground">昵称:{c.nickname}</span>
                          ) : null}
                        </span>
                        <span className="shrink-0 text-[11px] text-muted-foreground tabular-nums">{c.works_count} 作品</span>
                      </label>
                    );
                  })
                )}
              </div>
            </div>
          ) : (
            <div className="space-y-1.5">
              <Label>博主</Label>
              <Select
                value={form.creatorId}
                onValueChange={(v) => setForm((f) => ({ ...f, creatorId: v, collectionId: "" }))}
              >
                <SelectTrigger>
                  <SelectValue placeholder="选择博主" />
                </SelectTrigger>
                <SelectContent>
                  {(creators.data ?? []).map((c) => (
                    <SelectItem key={c.id} value={String(c.id)}>
                      {creatorDisplayName(c)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}

          {form.targetType === "collection" ? (
            <div className="space-y-1.5">
              <Label>合集</Label>
              <Select
                value={form.collectionId}
                onValueChange={(v) => setForm((f) => ({ ...f, collectionId: v }))}
                disabled={!creatorIdNum}
              >
                <SelectTrigger>
                  <SelectValue placeholder={creatorIdNum ? "选择合集" : "先选择博主"} />
                </SelectTrigger>
                <SelectContent>
                  {(collections.data ?? []).map((col) => (
                    <SelectItem key={col.id} value={String(col.id)}>
                      {col.name}({col.works_count})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          ) : null}

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="sub-interval">间隔(分钟)</Label>
              <Input
                id="sub-interval"
                inputMode="numeric"
                value={form.intervalMinutes}
                onChange={(e) => setForm((f) => ({ ...f, intervalMinutes: e.target.value }))}
              />
            </div>
            <div className="space-y-1.5">
              <Label>画质</Label>
              <Select value={form.quality} onValueChange={(v) => setForm((f) => ({ ...f, quality: v as Quality }))}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {QUALITIES.map((q) => (
                    <SelectItem key={q} value={q}>
                      {q}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
            <div>
              <Label htmlFor="sub-auto-download" className="text-sm">自动下载新作品</Label>
              <p className="mt-0.5 text-[11px] text-muted-foreground">关闭时仅记录,需手动下载</p>
            </div>
            <Switch
              id="sub-auto-download"
              checked={form.autoDownload}
              onCheckedChange={(autoDownload) => setForm((f) => ({ ...f, autoDownload }))}
            />
          </div>

          {error ? <p className="text-xs text-destructive">{error}</p> : null}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>
            取消
          </Button>
          <Button onClick={handleSubmit} disabled={pending}>
            {pending
              ? "创建中…"
              : form.targetType === "creator" && form.creatorIds.length > 1
                ? `创建 ${form.creatorIds.length} 条订阅`
                : "创建订阅"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
