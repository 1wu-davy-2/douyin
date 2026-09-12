/**
 * 全局播放器 Dialog(双模式)。
 * - 视频作品(有 video 资产):/api/assets/{id}/content(后端原生支持 Range);MOCK 模式下 mock 后端
 *   不提供二进制,自动改用演示源
 * - 图集作品(有 kind=image 资产,quality 为 "0001" 4 位序号):幻灯片模式,默认每 5s 自动切换下一张,
 *   空格键暂停/恢复计时;到最后一张后:连播开 → 自动切下一个作品,连播关 → 停留
 * - video 与 image 资产并存时(契约上不应发生)video 优先
 * - 上一条/下一条按打开时的播放列表顺序;连播 Switch 持久化到 localStorage
 */
import { useEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight, Film, Image as ImageIcon, ImageOff, Loader2, Repeat2, VideoOff } from "lucide-react";
import { Dialog, DialogContent, DialogTitle } from "../ui/dialog";
import { Badge } from "../ui/badge";
import { Switch } from "../ui/switch";
import { Label } from "../ui/label";
import { usePlayer, usePlayerActions } from "../../lib/player-store";
import { useWorkAssets } from "../../api/queries";
import { assetContentUrl } from "../../api/client";
import { MOCK } from "../../lib/env";
import type { Asset } from "../../api/types";

const DEMO_VIDEO = "https://interactive-examples.mdn.mozilla.net/media/cc0-videos/flower.mp4";
const AUTO_NEXT_KEY = "douyin-archive:auto-next";
const SLIDE_INTERVAL_MS = 5000;

export function PlayerDialog() {
  const player = usePlayer();
  const { next, prev, close } = usePlayerActions();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [autoNext, setAutoNext] = useState(() => localStorage.getItem(AUTO_NEXT_KEY) === "true");
  const [playError, setPlayError] = useState(false);
  const [slideIndex, setSlideIndex] = useState(0);
  const [slidePaused, setSlidePaused] = useState(false);
  const [imgError, setImgError] = useState(false);

  const item = player.items[player.index] ?? null;
  const isLast = player.index >= player.items.length - 1;
  const isFirst = player.index <= 0;

  const assets = useWorkAssets(item ? item.workId : null);
  const assetList = assets.data ?? [];
  // 同一作品可能因换画质重下存在多个 video 资产,取最新一份(id 最大)
  const videoAsset = assetList.filter((a) => a.kind === "video").sort((a, b) => b.id - a.id)[0];
  // 图集资产按 quality 4 位序号升序(后端契约已排序,这里防御性再排一次)
  const imageAssets = assetList
    .filter((a) => a.kind === "image")
    .slice()
    .sort((a, b) => (a.quality ?? "").localeCompare(b.quality ?? ""));
  // video 与 image 并存时(契约上不应发生)video 优先
  const slideshow = !videoAsset && imageAssets.length > 0;
  const slidePos = Math.min(slideIndex, imageAssets.length - 1);
  const currentImage = slideshow ? imageAssets[slidePos] : undefined;

  const src = item ? (slideshow ? null : MOCK ? DEMO_VIDEO : videoAsset ? assetContentUrl(videoAsset.id) : null) : null;
  const loading = Boolean(item) && assets.isPending;
  const missing = Boolean(item) && assets.isSuccess && !videoAsset && imageAssets.length === 0;

  // 打开/切换作品时:幻灯片回到第 1 张、恢复轮播并清除错误态(手动上一条/下一条由此重置 5s 计时)
  useEffect(() => {
    if (!player.open) return;
    setSlideIndex(0);
    setSlidePaused(false);
    setImgError(false);
  }, [item?.workId, player.open]);

  // 切换视频源时重置错误态并尝试自动播放(幻灯片模式下 videoRef 为空,自动跳过)
  useEffect(() => {
    setPlayError(false);
    const el = videoRef.current;
    if (!el) return;
    el.load();
    el.play().catch(() => {
      // 自动播放被浏览器策略拦截时静默,保留用户手动播放
    });
  }, [src, item?.workId]);

  // 当前图地址变化时清除图片错误态
  useEffect(() => {
    setImgError(false);
  }, [currentImage?.id]);

  // 幻灯片:每 5s 自动切下一张;最后一张时连播开且还有下一条 → 切下一个作品,否则停留
  useEffect(() => {
    if (!player.open || !slideshow || slidePaused) return;
    const atLastSlide = slidePos >= imageAssets.length - 1;
    if (atLastSlide && (isLast || !autoNext)) return;
    const timer = setTimeout(() => {
      if (slidePos < imageAssets.length - 1) {
        setSlideIndex(slidePos + 1);
      } else if (autoNext && !isLast) {
        next();
      }
    }, SLIDE_INTERVAL_MS);
    return () => clearTimeout(timer);
  }, [player.open, slideshow, slidePaused, slidePos, imageAssets.length, autoNext, isLast, next]);

  // 空格键:视频模式播放/暂停;幻灯片模式暂停/恢复 5s 计时(表单元素聚焦时不拦截)
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
      if (slideshow) {
        setSlidePaused((p) => !p);
        return;
      }
      const el = videoRef.current;
      if (!el) return;
      if (el.paused) void el.play().catch(() => {});
      else el.pause();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [player.open, slideshow]);

  const handleEnded = () => {
    if (autoNext && !isLast) next();
  };

  /** mock 模式图片资产 path 即 SVG data URI(无二进制 content 端点);真实模式走支持 Range 的 content 端点。 */
  const imageSrc = (a: Asset): string => (MOCK && a.path.startsWith("data:") ? a.path : assetContentUrl(a.id));

  return (
    <Dialog open={player.open} onOpenChange={(open) => (open ? undefined : close())}>
      <DialogContent className="max-w-3xl gap-0 overflow-hidden p-0" onOpenAutoFocus={(e) => e.preventDefault()}>
        {item ? (
          <DialogTitle className="flex items-center gap-2 px-4 pt-3 text-sm font-medium">
            <span className="min-w-0 flex-1 truncate" title={item.title}>{item.title}</span>
            {assets.isSuccess ? (
              slideshow ? (
                <Badge variant="secondary" className="shrink-0">
                  <ImageIcon /> 图集 {imageAssets.length} 张
                </Badge>
              ) : (
                <Badge variant="muted" className="shrink-0">
                  <Film /> 视频
                </Badge>
              )
            ) : null}
          </DialogTitle>
        ) : null}
        <div className="relative aspect-video w-full bg-black">
          {loading ? (
            <div className="absolute inset-0 flex items-center justify-center text-muted-foreground">
              <Loader2 className="size-7 animate-spin" />
            </div>
          ) : slideshow && currentImage ? (
            imgError ? (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 text-muted-foreground">
                <ImageOff className="size-8" />
                <p className="text-sm">图片加载失败,请稍后重试</p>
              </div>
            ) : (
              <img
                key={currentImage.id}
                src={imageSrc(currentImage)}
                alt={`${item.title} 第 ${slidePos + 1} 张`}
                className="size-full object-contain"
                draggable={false}
                onError={() => setImgError(true)}
              />
            )
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
              <p className="text-sm">{missing ? "该作品没有可播放的资产" : "视频加载失败,请稍后重试"}</p>
              {MOCK ? <p className="text-xs text-muted-foreground/70">(演示模式使用在线示例视频,需联网)</p> : null}
            </div>
          )}
          {slideshow && slidePaused ? (
            <div className="absolute left-3 top-3 rounded-md bg-black/60 px-2 py-1 text-xs text-white/90">轮播已暂停</div>
          ) : null}
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
              {slideshow
                ? `${slidePos + 1} / ${imageAssets.length}`
                : player.items.length > 0
                  ? `${player.index + 1} / ${player.items.length}`
                  : "—"}
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
          {slideshow
            ? "提示:空格键 暂停 / 继续轮播;每 5 秒自动切换下一张,最后一张后按连播设置切下一个作品"
            : "提示:空格键播放 / 暂停;视频源支持 HTTP Range,可任意拖动进度条"}
        </div>
      </DialogContent>
    </Dialog>
  );
}
