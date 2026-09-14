/**
 * 账号 Tab:账号卡片(启停/刷新好友/导出 Cookie/删除)+ 选中账号的好友列表勾选。
 * "扫码添加账号"打开 LoginDialog;好友今日状态徽标来自 with_today=1。
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Loader2, QrCode, RefreshCw, Send, Trash2, Upload } from "lucide-react";
import { toast } from "sonner";
import {
  deleteSparkAccount,
  exportSparkCookies,
  patchSparkFriends,
  refreshSparkFriends,
  updateSparkAccount,
} from "../../api/spark";
import type { SparkAccount, SparkFriendToday } from "../../api/spark-types";
import { useSparkAccounts, useSparkFriends } from "../../api/queries";
import { Button } from "../../components/ui/button";
import { Card, CardContent } from "../../components/ui/card";
import { Checkbox } from "../../components/ui/checkbox";
import { Label } from "../../components/ui/label";
import { Skeleton } from "../../components/ui/skeleton";
import { Switch } from "../../components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table";
import { ConfirmDialog } from "../../components/confirm-dialog";
import { SparkStatusBadge, TodayStateBadge, fmtDateTime } from "./shared";
import { LoginDialog } from "./login-dialog";

function AccountCard({
  account,
  active,
  onActivate,
  onDelete,
}: {
  account: SparkAccount;
  active: boolean;
  onActivate: () => void;
  onDelete: () => void;
}) {
  const queryClient = useQueryClient();
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: ["spark"] });

  const toggleMut = useMutation({
    mutationFn: (enabled: boolean) => updateSparkAccount(account.id, { enabled }),
    onSuccess: invalidate,
    onError: (e) => toast.error("更新失败", { description: e.message }),
  });

  const refreshMut = useMutation({
    mutationFn: () => refreshSparkFriends(account.id),
    onSuccess: (r) => {
      toast.success("好友已刷新", { description: `共 ${r.count} 人 · 新增 ${r.new}` });
      invalidate();
    },
    onError: (e) => toast.error("刷新好友失败", { description: e.message }),
  });

  const exportMut = useMutation({
    mutationFn: () => exportSparkCookies(account.id),
    onSuccess: (r) =>
      toast.success("Cookie 已同步到归档", { description: `共 ${r.cookie_count} 条,侧车将热加载` }),
    onError: (e) => toast.error("Cookie 导出失败", { description: e.message }),
  });

  const busy = toggleMut.isPending || refreshMut.isPending || exportMut.isPending;

  return (
    <Card
      className={active ? "border-primary/60 ring-1 ring-primary/30" : "cursor-pointer hover:border-border"}
      onClick={onActivate}
    >
      <CardContent className="space-y-2.5 pt-5">
        <div className="flex items-center justify-between gap-2">
          <p className="min-w-0 truncate font-medium">{account.nickname || account.username}</p>
          <SparkStatusBadge status={account.status} />
        </div>
        <p className="truncate text-xs text-muted-foreground">
          @{account.unique_id} · 好友刷新 {fmtDateTime(account.last_friends_refresh_at)}
        </p>
        {account.status === "cooldown" && account.cooldown_until ? (
          <p className="text-xs text-orange-600">冷却至 {fmtDateTime(account.cooldown_until)}</p>
        ) : null}
        {account.status === "error" && account.last_error ? (
          <p className="line-clamp-2 text-xs text-red-600">{account.last_error}</p>
        ) : null}
        <div className="flex items-center justify-between gap-2 pt-1" onClick={(e) => e.stopPropagation()}>
          <span className="flex items-center gap-2 text-xs text-muted-foreground">
            <Switch
              checked={account.enabled}
              disabled={busy}
              onCheckedChange={(v) => toggleMut.mutate(v)}
            />
            {account.enabled ? "已启用" : "已停用"}
          </span>
          <span className="flex items-center gap-1">
            <Button
              variant="ghost"
              size="icon-sm"
              title="刷新好友"
              disabled={busy}
              onClick={() => refreshMut.mutate()}
            >
              {refreshMut.isPending ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <RefreshCw className="size-4" />
              )}
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              title="导出 Cookie 到归档"
              disabled={busy}
              onClick={() => exportMut.mutate()}
            >
              {exportMut.isPending ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Upload className="size-4" />
              )}
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              title="删除账号"
              className="text-red-600 hover:text-red-700"
              disabled={busy}
              onClick={onDelete}
            >
              <Trash2 className="size-4" />
            </Button>
          </span>
        </div>
      </CardContent>
    </Card>
  );
}

function FriendTable({ accountId }: { accountId: number }) {
  const queryClient = useQueryClient();
  const friends = useSparkFriends(accountId);
  // 本地勾选态直接以服务端 selected 为初值,批量 PATCH 提交
  const [draft, setDraft] = useState<Map<string, boolean>>(new Map());
  const listRef = useRef<SparkFriendToday[]>([]);

  useEffect(() => {
    if (friends.data) {
      listRef.current = friends.data;
      setDraft(new Map(friends.data.map((f) => [f.friend_key, f.selected])));
    }
  }, [friends.data]);

  const saveMut = useMutation({
    mutationFn: (updates: { key: string; selected: boolean }[]) => patchSparkFriends(accountId, updates),
    onSuccess: () => {
      toast.success("勾选已保存");
      void queryClient.invalidateQueries({ queryKey: ["spark"] });
    },
    onError: (e) => toast.error("保存失败", { description: e.message }),
  });

  const rows = friends.data ?? [];
  const checkedCount = useMemo(() => [...draft.values()].filter(Boolean).length, [draft]);
  const dirty = useMemo(
    () =>
      rows.some((f) => draft.get(f.friend_key) !== undefined && draft.get(f.friend_key) !== f.selected),
    [rows, draft],
  );

  const toggle = (key: string, v: boolean | string) => {
    setDraft((prev) => new Map(prev).set(key, v === true));
  };
  const setAll = (v: boolean) => {
    setDraft(new Map(rows.map((f) => [f.friend_key, v])));
  };

  if (friends.isPending) return <Skeleton className="h-64" />;
  if (friends.isError)
    return <p className="text-sm text-red-600">好友列表加载失败:{friends.error.message}</p>;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm text-muted-foreground">
          共 {rows.length} 人 · 已勾选 <span className="font-medium text-foreground">{checkedCount}</span>
        </span>
        <span className="flex-1" />
        <Button variant="outline" size="sm" onClick={() => setAll(true)}>
          全选
        </Button>
        <Button variant="outline" size="sm" onClick={() => setAll(false)}>
          清空
        </Button>
        <Button size="sm" disabled={!dirty || saveMut.isPending} onClick={() => {
          const updates = rows
            .filter((f) => draft.get(f.friend_key) !== f.selected)
            .map((f) => ({ key: f.friend_key, selected: draft.get(f.friend_key) ?? f.selected }));
          if (updates.length > 0) saveMut.mutate(updates);
        }}>
          {saveMut.isPending ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4" />}
          保存勾选
        </Button>
      </div>

      {rows.length === 0 ? (
        <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
          还没有好友数据,先点账号卡片上的「刷新好友」。
        </p>
      ) : (
        <div className="max-h-[420px] overflow-y-auto rounded-lg border border-border/70">
          <Table>
            <TableHeader className="sticky top-0 bg-card">
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox
                    checked={rows.length > 0 && rows.every((f) => draft.get(f.friend_key) === true)}
                    onCheckedChange={(v) => setAll(v === true)}
                  />
                </TableHead>
                <TableHead>好友</TableHead>
                <TableHead>标识</TableHead>
                <TableHead className="w-28">今日状态</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((f) => (
                <TableRow key={f.friend_key}>
                  <TableCell>
                    <Checkbox
                      checked={draft.get(f.friend_key) ?? f.selected}
                      onCheckedChange={(v) => toggle(f.friend_key, v)}
                    />
                  </TableCell>
                  <TableCell className="max-w-52 truncate">{f.display_name || f.friend_key}</TableCell>
                  <TableCell className="max-w-40 truncate text-xs text-muted-foreground">
                    {f.friend_key}
                  </TableCell>
                  <TableCell>
                    <TodayStateBadge state={f.today_state} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

export function AccountsTab() {
  const accounts = useSparkAccounts();
  const queryClient = useQueryClient();
  const [activeId, setActiveId] = useState<number | null>(null);
  const [loginOpen, setLoginOpen] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<SparkAccount | null>(null);

  useEffect(() => {
    if (accounts.data && (activeId === null || !accounts.data.some((a) => a.id === activeId))) {
      setActiveId(accounts.data[0]?.id ?? null);
    }
  }, [accounts.data, activeId]);

  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteSparkAccount(id),
    onSuccess: () => {
      toast.success("账号已删除");
      setPendingDelete(null);
      void queryClient.invalidateQueries({ queryKey: ["spark"] });
    },
    onError: (e) => {
      toast.error("删除失败", { description: e.message });
      setPendingDelete(null);
    },
  });

  if (accounts.isPending) return <Skeleton className="h-64" />;
  if (accounts.isError)
    return <p className="text-sm text-red-600">账号加载失败:{accounts.error.message}</p>;

  const list = accounts.data ?? [];

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={() => setLoginOpen(true)}>
          <QrCode className="size-4" />
          扫码添加账号
        </Button>
        {list.length === 0 ? (
          <p className="text-sm text-muted-foreground">还没有账号,扫码登录后自动建档。</p>
        ) : null}
      </div>

      {list.length > 0 ? (
        <>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {list.map((a) => (
              <AccountCard
                key={a.id}
                account={a}
                active={a.id === activeId}
                onActivate={() => setActiveId(a.id)}
                onDelete={() => setPendingDelete(a)}
              />
            ))}
          </div>

          <div className="space-y-3">
            <Label className="text-sm font-medium">好友列表(点击上方卡片切换账号)</Label>
            {activeId !== null ? <FriendTable accountId={activeId} /> : null}
          </div>
        </>
      ) : null}

      <LoginDialog open={loginOpen} onOpenChange={setLoginOpen} onCreated={(id) => setActiveId(id)} />

      <ConfirmDialog
        open={pendingDelete !== null}
        onOpenChange={(v) => (v ? null : setPendingDelete(null))}
        title={`删除账号 ${pendingDelete?.nickname || (pendingDelete?.unique_id ?? "")}?`}
        description="将同时删除其好友列表与选中状态;发送记录保留用于审计。"
        confirmLabel="删除"
        destructive
        loading={deleteMut.isPending}
        onConfirm={() => pendingDelete && deleteMut.mutate(pendingDelete.id)}
      />
    </div>
  );
}
