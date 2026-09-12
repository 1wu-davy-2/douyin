/**
 * 全局播放器 Dialog。
 * - 视频源:/api/assets/{id}/content(后端原生支持 Range);MOCK 模式下 mock 后端不提供二进制,自动改用演示源
 * - 上一条/下一条按打开时的播放列表顺序;空格键播放/暂停;连播 Switch 持久化到 localStorage
 */
import { useEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight, Loader2, Repeat2, VideoOff } from "lucide-react";
import { Dialog, DialogContent, DialogTitle } from "../ui/dialog";
import { Switch } from "../ui/switch";
import { Label } from "../ui/label";
import { usePlayer, usePlayerActions } from "../../lib/player-store";
import { useWorkAssets } from "../../api/queries";
import { assetContentUrl } from "../../api/client";
import { MOCK } from "../../lib/env";

const DEMO_VIDEO = "https://interactive-examples.mdn.mozilla.net/media/cc0-videos/flower.mp4";
const AUTO_NEXT_KEY = "douyin-archive:auto-next";

export function PlayerDialog() {
  const player = usePlayer();
  const { next, prev, close } = usePlayerActions();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [autoNext, setAutoNext] = useState(() => localStorage.getItem(AUTO_NEXT_KEY) === "true");
  const [playError, setPlayError] = useState(false);

  const item = player.items[player.index] ?? null;
  const isLast = player.index >= player.items.length - 1;
  const isFirst = player.index <= 0;

  const assets = useWorkAssets(item ? (MOCK ? null : item.workId) : null);
  // 同一作品可能因换画质重下存在多个 video 资产,取最新一份
  const videoAsset = (assets.data ?? [])
    .filter((a) => a.kind === "video")
    .sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""))[0];
  const src = item ? (MOCK ? DEMO_VIDEO : videoAsset ? assetContentUrl(videoAsset.id) : null) : null;
  const loading = Boolean(item) && !MOCK && assets.isPending;
  const missing = Boolean(item) && !MOCK && assets.isSuccess && !videoAsset;

  // 切换条目时重置错误态并尝试自动播放
  useEffect(() => {
    setPlayError(false);
    const el = videoRef.current;
    if (!el) return;
    el.load();
    el.play().catch(() => {
      // 自动播放被浏览器策略拦截时静默,保留用户手动播放
    });
  }, [src, item?.workId]);

  // 空格键播放/暂停(表单元素聚焦时不拦截)
  useEffect(() => {
    if (!player.open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.code !== "Space") return;
      const target = e.target as HTMLElement | null;
      if (
        target &&
        (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.tagName === "SELECT" || target.isContentEditable)
      ) {
        return;
      }
      e.preventDefault();
      const el = videoRef.current;
      if (!el) return;
      if (el.paused) void el.play().catch(() => {});
      else el.pause();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [player.open]);

  const handleEnded = () => {
    if (autoNext && !isLast) next();
  };

  return (
    <Dialog open={player.open} onOpenChange={(open) => (open ? undefined : close())}>
      <DialogContent className="max-w-3xl gap-0 overflow-hidden p-0" onOpenAutoFocus={(e) => e.preventDefault()}>
        {item ? <DialogTitle className="px-4 pt-3 text-sm font-medium">{item.title}</DialogTitle> : null}
        <div className="relative aspect-video w-full bg-black">
          {loading ? (
            <div className="absolute inset-0 flex items-center justify-center text-muted-foreground">
              <Loader2 className="size-7 animate-spin" />
            </div>
          ) : src && !playError ? (
            <video
              ref={videoRef}
              key={src}
              src={src}
              className="size-full"
              controls
              playsInline
              onError={() => setPlayError(true)}
              onEnded={handleEnded}
            />
          ) : (
            <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 text-muted-foreground">
              <VideoOff className="size-8" />
              <p className="text-sm">{missing ? "该作品没有可播放的视频资产" : "视频加载失败,请稍后重试"}</p>
              {MOCK ? <p className="text-xs text-muted-foreground/70">(演示模式使用在线示例视频,需联网)</p> : null}
            </div>
          )}
        </div>

        <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={prev}
              disabled={isFirst}
              className="inline-flex h-8 items-center gap-1 rounded-md border border-border px-2.5 text-xs transition-colors hover:bg-accent disabled:pointer-events-none disabled:opacity-40"
            >
              <ChevronLeft className="size-4" /> 上一条
            </button>
            <span className="min-w-14 text-center text-xs tabular-nums text-muted-foreground">
              {player.items.length > 0 ? `${player.index + 1} / ${player.items.length}` : "—"}
            </span>
            <button
              type="button"
              onClick={next}
              disabled={isLast}
              className="inline-flex h-8 items-center gap-1 rounded-md border border-border px-2.5 text-xs transition-colors hover:bg-accent disabled:pointer-events-none disabled:opacity-40"
            >
              下一条 <ChevronRight className="size-4" />
            </button>
          </div>

          <div className="flex items-center gap-3">
            {item?.creatorName ? (
              <span className="text-xs text-muted-foreground">{item.creatorName}</span>
            ) : null}
            {player.items.length > 1 ? (
              <div className="flex items-center gap-1.5">
                <Repeat2 className="size-3.5 text-muted-foreground" />
                <Label htmlFor="player-auto-next" className="text-xs text-muted-foreground">
                  连播
                </Label>
                <Switch
                  id="player-auto-next"
                  checked={autoNext}
                  onCheckedChange={(v) => {
                    setAutoNext(v);
                    localStorage.setItem(AUTO_NEXT_KEY, String(v));
                  }}
                />
              </div>
            ) : null}
          </div>
        </div>

        <div className="border-t border-border/60 px-4 py-2 text-[11px] text-muted-foreground/70">
          提示:空格键播放 / 暂停;视频源支持 HTTP Range,可任意拖动进度条
        </div>
      </DialogContent>
    </Dialog>
  );
}
