import { Badge } from "./ui/badge";
import type { DLStatus, JobStatus } from "../api/types";

/** work.dl_status 徽章(docs/api.md 枚举)。 */
export function DlStatusBadge({ status, quality }: { status: DLStatus; quality?: string | null }) {
  const map: Record<DLStatus, { label: string; variant: "muted" | "info" | "success" | "destructive" | "warning" }> = {
    none: { label: "未下载", variant: "muted" },
    queued: { label: "排队中", variant: "info" },
    downloading: { label: "下载中", variant: "info" },
    succeeded: { label: "已下载", variant: "success" },
    failed: { label: "失败", variant: "destructive" },
    canceled: { label: "已取消", variant: "warning" },
  };
  const it = map[status];
  return (
    <Badge variant={it.variant}>
      {it.label}
      {status === "succeeded" && quality ? <span className="font-normal opacity-80">{quality}</span> : null}
    </Badge>
  );
}

/** job.status 徽章。 */
export function JobStatusBadge({ status }: { status: JobStatus }) {
  const map: Record<JobStatus, { label: string; variant: "muted" | "info" | "success" | "destructive" | "warning" }> = {
    queued: { label: "排队中", variant: "info" },
    downloading: { label: "下载中", variant: "info" },
    paused_q: { label: "已暂停", variant: "warning" },
    succeeded: { label: "已完成", variant: "success" },
    failed: { label: "失败", variant: "destructive" },
    canceled: { label: "已取消", variant: "muted" },
  };
  const it = map[status];
  return <Badge variant={it.variant}>{it.label}</Badge>;
}
