/**
 * 全局风控弹窗(契约 v1.4)。
 * 后端 risk tracker 连续探测到抖音 403 / Cookie 失效后,通过 /api/health
 * 与 SSE provider.status 推送 cookie_blocked=true;本组件挂载一次、全局弹出,
 * 引导用户去设置页更新 Cookie。"稍后处理"只屏蔽当前这次阻断,解除后再次
 * 触发会重新弹出。
 */
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { ShieldAlert } from "lucide-react";
import { useHealth } from "../api/queries";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { Button } from "./ui/button";

export function CookieAlertDialog() {
  const { data: health } = useHealth();
  const navigate = useNavigate();
  const [dismissedFor, setDismissedFor] = useState<string | null>(null);

  const blocked = health?.cookie_blocked ?? false;
  const since = health?.blocked_since ?? null;
  // 每次新的阻断都独立计:同一 since 内"稍后处理"有效,since 变化(解除后
  // 再次触发)会重新弹出。
  const blockKey = blocked ? (since ?? "blocked") : null;
  const open = blockKey !== null && dismissedFor !== blockKey;

  // 解除风控时同步清除屏蔽记录,避免状态滞留
  const prevBlockedRef = useRef(false);
  useEffect(() => {
    if (prevBlockedRef.current && !blocked) setDismissedFor(null);
    prevBlockedRef.current = blocked;
  }, [blocked]);

  const sinceText = since
    ? new Date(since).toLocaleString("zh-CN", { hour12: false })
    : null;

  return (
    <Dialog open={open} onOpenChange={(v) => { if (!v && blockKey) setDismissedFor(blockKey); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <div className="flex items-center gap-3">
            <span className="flex size-10 shrink-0 items-center justify-center rounded-full bg-destructive/10 text-destructive">
              <ShieldAlert className="size-5" />
            </span>
            <div>
              <DialogTitle>Cookie 已失效 / 触发抖音风控</DialogTitle>
              <DialogDescription className="mt-1">
                扫描与下载暂时无法获取数据
              </DialogDescription>
            </div>
          </div>
        </DialogHeader>

        <div className="space-y-2 text-sm text-muted-foreground">
          <p className="rounded-md bg-destructive/5 px-3 py-2 text-destructive/90">
            {health?.blocked_reason ?? "抖音接口持续返回 403（风控拦截）"}
          </p>
          <p>
            抖音 Cookie 有效期有限，高频请求也会触发风控。请前往
            <span className="font-medium text-foreground"> 设置 → Provider </span>
            重新粘贴浏览器中的 Cookie（F12 → Network → 任一 douyin.com
            请求的 Cookie 请求头，完整复制），保存后自动恢复。
          </p>
          {sinceText && <p className="text-xs">首次触发：{sinceText}</p>}
        </div>

        <DialogFooter className="gap-2 sm:gap-2">
          <Button variant="ghost" onClick={() => blockKey && setDismissedFor(blockKey)}>
            稍后处理
          </Button>
          <Button onClick={() => navigate("/settings")}>
            去设置更新 Cookie
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
