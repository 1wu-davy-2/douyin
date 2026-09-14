/**
 * 火花页面共享小件:状态徽标 / 今日状态徽标 / 时间格式化。
 */
import { Badge } from "../../components/ui/badge";
import { cn } from "../../lib/utils";
import type { SparkAccountStatus } from "../../api/spark-types";

export const SPARK_STATUS_META: Record<SparkAccountStatus, { label: string; className: string }> = {
  idle: { label: "空闲", className: "bg-emerald-100 text-emerald-700 border-emerald-200" },
  sending: { label: "发送中", className: "bg-blue-100 text-blue-700 border-blue-200" },
  login_required: { label: "需登录", className: "bg-amber-100 text-amber-700 border-amber-200" },
  cooldown: { label: "冷却中", className: "bg-orange-100 text-orange-700 border-orange-200" },
  error: { label: "异常", className: "bg-red-100 text-red-700 border-red-200" },
};

export function SparkStatusBadge({ status }: { status: SparkAccountStatus }) {
  const meta = SPARK_STATUS_META[status] ?? SPARK_STATUS_META.idle;
  return (
    <Badge variant="outline" className={cn(meta.className)}>
      {meta.label}
    </Badge>
  );
}

/** 今日状态:strong 确认 / failed 失败 / 空 = 未发。 */
export function TodayStateBadge({ state }: { state: string }) {
  if (!state) {
    return (
      <Badge variant="outline" className="bg-muted text-muted-foreground border-transparent">
        未发
      </Badge>
    );
  }
  if (state === "strong") {
    return (
      <Badge variant="outline" className="bg-emerald-100 text-emerald-700 border-emerald-200">
        已确认
      </Badge>
    );
  }
  // failed 或 failed:category
  const category = state.startsWith("failed:") ? state.slice(7) : "";
  return (
    <Badge variant="outline" className="bg-red-100 text-red-700 border-red-200">
      失败{category ? `·${category}` : ""}
    </Badge>
  );
}

export function fmtDateTime(s: string): string {
  if (!s) return "—";
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getMonth() + 1}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
