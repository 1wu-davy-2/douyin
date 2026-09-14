/**
 * 扫码登录弹窗:
 * 打开 → 引擎 open 登录会话 → 5s 轮询 status → 展示二维码(/api/spark/login/qr 反代);
 * logged_in → export 拿身份 → 建档账号 → 刷好友 + 导出 Cookie → toast 收尾关闭。
 * noVNC 远程桌面放在 <details> 里按需展开(滑块验证等人工干预)。
 */
import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import {
  closeSparkLogin,
  createSparkAccount,
  exportSparkCookies,
  exportSparkLogin,
  fetchSparkLoginQr,
  getSparkLoginStatus,
  openSparkLogin,
  refreshSparkFriends,
  refreshSparkQr,
  sparkLoginVncUrl,
} from "../../api/spark";
import { Button } from "../../components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../../components/ui/dialog";

interface LoginDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 完成建档后的回调(切换选中账号等)。 */
  onCreated?: (accountId: number) => void;
}

export function LoginDialog({ open, onOpenChange, onCreated }: LoginDialogProps) {
  const queryClient = useQueryClient();
  const [vncOpen, setVncOpen] = useState(false);
  // 每次打开会话只跑一次导出+建档
  const doneRef = useRef(false);
  // 记录本次会话是否已成功 open(失败时禁用轮询报错展示)
  const openFailedRef = useRef(false);
  // 已创建的 blob URL,替换/卸载时 revoke 防泄漏
  const lastQrUrlRef = useRef<string | null>(null);

  // 打开时启动登录会话
  useEffect(() => {
    if (!open) return;
    doneRef.current = false;
    openFailedRef.current = false;
    openSparkLogin()
      .then(() => queryClient.invalidateQueries({ queryKey: ["spark", "login-qr"] }))
      .catch((e) => {
        openFailedRef.current = true;
        toast.error("启动登录会话失败", { description: e.message });
      });
  }, [open, queryClient]);

  const status = useQuery({
    queryKey: ["spark", "login-status"],
    queryFn: getSparkLoginStatus,
    enabled: open && !openFailedRef.current,
    refetchInterval: 5000,
    retry: false,
  });

  const s0 = status.data;
  const showQr = Boolean(open && s0?.running && !s0.logged_in);

  // 二维码轮询:202(生成中)2.5s 重试;就绪后 15s 换新截图保持有效
  const qr = useQuery({
    queryKey: ["spark", "login-qr"],
    queryFn: fetchSparkLoginQr,
    enabled: showQr,
    refetchInterval: (q) => (q.state.data?.state === "ready" ? 15000 : 2500),
    retry: false,
  });

  // 旧的 blob URL 及时回收
  const qrUrl = qr.data?.state === "ready" ? qr.data.url : null;
  useEffect(() => {
    if (qrUrl && qrUrl !== lastQrUrlRef.current) {
      const prev = lastQrUrlRef.current;
      lastQrUrlRef.current = qrUrl;
      if (prev) URL.revokeObjectURL(prev);
    }
  }, [qrUrl]);
  useEffect(
    () => () => {
      if (lastQrUrlRef.current) URL.revokeObjectURL(lastQrUrlRef.current);
    },
    [],
  );

  // 登录成功 → 导出 + 建档(一次性)
  useEffect(() => {
    const s = status.data;
    if (!open || !s?.logged_in || doneRef.current) return;
    doneRef.current = true;
    (async () => {
      try {
        const identity = await exportSparkLogin();
        const account = await createSparkAccount({
          unique_id: identity.unique_id,
          username: identity.unique_id,
          nickname: identity.nickname,
          profile_name: identity.profile_name,
        });
        toast.success("账号已添加", { description: `${identity.nickname}(@${identity.unique_id})` });
        // 好友刷新与 Cookie 导出后台执行,不阻塞关闭
        void refreshSparkFriends(account.id).catch(() =>
          toast.warning("好友列表刷新失败", { description: "可稍后在账号页手动刷新" }),
        );
        void exportSparkCookies(account.id)
          .then((r) => toast.info("Cookie 已同步到归档", { description: `共 ${r.cookie_count} 条` }))
          .catch(() => toast.warning("Cookie 导出失败", { description: "可稍后在账号页手动导出" }));
        void queryClient.invalidateQueries({ queryKey: ["spark"] });
        onCreated?.(account.id);
        onOpenChange(false);
      } catch (e) {
        toast.error("登录收尾失败", { description: (e as Error).message });
      }
    })();
  }, [open, status.data, queryClient, onCreated, onOpenChange]);

  const refreshMut = useMutation({
    mutationFn: refreshSparkQr,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["spark", "login-qr"] }),
    onError: (e) => toast.error("刷新二维码失败", { description: e.message }),
  });

  // 关闭弹窗时顺带结束引擎登录会话(失败不提示)
  const handleOpenChange = (next: boolean) => {
    if (!next && open) void closeSparkLogin().catch(() => {});
    onOpenChange(next);
  };

  const s = status.data;

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>扫码添加账号</DialogTitle>
          <DialogDescription>
            用抖音 App 扫描二维码完成登录;登录信息仅保存在本机引擎中。
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col items-center gap-3 py-2">
          {status.isPending || (!s && !openFailedRef.current) ? (
            <div className="flex h-56 w-56 items-center justify-center rounded-lg border border-dashed border-border">
              <Loader2 className="size-6 animate-spin text-muted-foreground" />
            </div>
          ) : s?.logged_in ? (
            <div className="flex h-56 w-56 flex-col items-center justify-center gap-2 rounded-lg border border-emerald-200 bg-emerald-50">
              <span className="text-4xl">✅</span>
              <p className="text-sm font-medium text-emerald-700">登录成功,正在建档…</p>
              <p className="text-xs text-emerald-600">
                {s.nickname}
                {s.unique_id ? `(@${s.unique_id})` : ""}
              </p>
            </div>
          ) : showQr ? (
            <>
              {qr.data?.state === "ready" ? (
                <img
                  src={qr.data.url}
                  alt="抖音登录二维码"
                  className="h-56 w-56 rounded-lg border border-border bg-white object-contain p-2"
                />
              ) : qr.data?.state === "expired" ? (
                <div className="flex h-56 w-56 items-center justify-center rounded-lg border border-dashed border-border text-sm text-muted-foreground">
                  二维码已过期,点击下方刷新
                </div>
              ) : (
                <div className="flex h-56 w-56 flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border">
                  <Loader2 className="size-6 animate-spin text-muted-foreground" />
                  <p className="text-xs text-muted-foreground">二维码生成中…</p>
                </div>
              )}
              <Button
                variant="outline"
                size="sm"
                disabled={refreshMut.isPending}
                onClick={() => refreshMut.mutate()}
              >
                {refreshMut.isPending ? (
                  <Loader2 className="size-4 animate-spin" />
                ) : (
                  <RefreshCw className="size-4" />
                )}
                二维码过期?点击刷新
              </Button>
            </>
          ) : (
            <div className="flex h-56 w-56 items-center justify-center rounded-lg border border-dashed border-border text-sm text-muted-foreground">
              会话未运行,重新打开弹窗重试
            </div>
          )}
          {showQr ? (
            <p className="text-xs text-muted-foreground">状态每 5 秒自动刷新,请保持页面打开</p>
          ) : null}
        </div>

        <details
          className="rounded-md border border-border/70"
          onToggle={(e) => setVncOpen((e.target as HTMLDetailsElement).open)}
        >
          <summary className="cursor-pointer select-none px-3 py-2 text-xs text-muted-foreground">
            遇到滑块验证?打开远程桌面手动处理(仅 Docker 部署;本机登录会弹出本地浏览器窗口)
          </summary>
          {/* 懒加载:details 折叠时浏览器也会请求 iframe,这里展开才挂载,避免引擎无 noVNC 时报 502 */}
          {vncOpen ? (
            <iframe
              src={sparkLoginVncUrl()}
              title="noVNC 远程桌面"
              className="h-80 w-full rounded-b-md border-t border-border/70 bg-black"
            />
          ) : null}
        </details>

        <p className="text-center text-[11px] text-muted-foreground">
          引擎状态:{s?.running ? "运行中" : "空闲"} · 关闭弹窗会结束登录会话
        </p>
      </DialogContent>
    </Dialog>
  );
}
