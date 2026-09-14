/**
 * 控制台 Tab:三种触发按钮(立即/补发失败/补发未发)+ SSE 实时日志流。
 * 409 → toast「已有任务在跑」;日志来自 spark-log-store(useSyncExternalStore)。
 */
import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Eraser, Play, RotateCcw, Send } from "lucide-react";
import { toast } from "sonner";
import { triggerSparkRun } from "../../api/spark";
import { ApiError } from "../../api/client";
import { Button } from "../../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../../components/ui/card";
import {
  clearSparkLog,
  getSparkLogSnapshot,
  subscribeSparkLog,
  type SparkLogLine,
} from "../../lib/spark-log-store";
import { cn } from "../../lib/utils";

function lineColor(line: SparkLogLine): string {
  if (line.kind === "finished") return "text-blue-600";
  if (line.state === "failed" || (line.kind === "account" && line.state === "error")) return "text-red-600";
  if (line.state === "cooldown") return "text-orange-600";
  return "text-foreground/80";
}

export function ConsoleTab() {
  const queryClient = useQueryClient();
  const [lines, setLines] = useState<SparkLogLine[]>(getSparkLogSnapshot);
  const bottomRef = useRef<HTMLDivElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);

  useEffect(() => subscribeSparkLog(() => setLines(getSparkLogSnapshot())), []);

  useEffect(() => {
    if (autoScroll) bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [lines, autoScroll]);

  const runMut = useMutation({
    mutationFn: (mode: "now" | "failed" | "unsent") => triggerSparkRun(mode),
    onSuccess: () => {
      toast.success("发送任务已受理", { description: "引擎将按策略执行,日志见下方" });
      void queryClient.invalidateQueries({ queryKey: ["spark"] });
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409) {
        toast.warning("已有任务在跑", { description: "等当前批次结束再触发" });
      } else {
        toast.error("触发失败", { description: e.message });
      }
    },
  });

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-base">触发发送</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          <Button disabled={runMut.isPending} onClick={() => runMut.mutate("now")}>
            <Play className="size-4" />
            立即发送(全部已勾选)
          </Button>
          <Button variant="outline" disabled={runMut.isPending} onClick={() => runMut.mutate("failed")}>
            <RotateCcw className="size-4" />
            补发今日失败
          </Button>
          <Button variant="outline" disabled={runMut.isPending} onClick={() => runMut.mutate("unsent")}>
            <Send className="size-4" />
            补发选中年未发
          </Button>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0 pb-3">
          <CardTitle className="text-base">实时日志(SSE)</CardTitle>
          <span className="flex items-center gap-2">
            <label className="flex cursor-pointer items-center gap-1.5 text-xs text-muted-foreground">
              <input
                type="checkbox"
                checked={autoScroll}
                onChange={(e) => setAutoScroll(e.target.checked)}
                className="accent-primary"
              />
              自动滚动
            </label>
            <Button variant="ghost" size="icon-sm" title="清空日志" onClick={() => clearSparkLog()}>
              <Eraser className="size-4" />
            </Button>
          </span>
        </CardHeader>
        <CardContent>
          <div className="h-80 overflow-y-auto rounded-md bg-muted/50 p-3 font-mono text-xs leading-relaxed">
            {lines.length === 0 ? (
              <p className="text-muted-foreground">
                暂无日志。触发发送或等待调度器执行后,事件会实时出现在这里。
              </p>
            ) : (
              lines.map((l) => (
                <p key={l.id} className={cn("whitespace-pre-wrap", lineColor(l))}>
                  <span className="mr-2 text-muted-foreground/70">{l.at}</span>
                  {l.text}
                </p>
              ))
            )}
            <div ref={bottomRef} />
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
