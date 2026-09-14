/**
 * 记录 Tab:发送记录表(strong 绿 / failed 红)+ 账号过滤 + 游标翻页。
 */
import { useState } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useSparkAccounts, useSparkRecordsPager } from "../../api/queries";
import { Button } from "../../components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select";
import { Skeleton } from "../../components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table";
import { fmtDateTime } from "./shared";

const MODE_LABEL: Record<string, string> = {
  manual: "手动",
  manual_failed: "补发失败",
  manual_unsent: "补发未发",
  scheduled: "定时",
};

function StateBadge({ state, category }: { state: "strong" | "failed"; category: string }) {
  if (state === "strong") {
    return (
      <span className="inline-flex items-center rounded-md border border-emerald-200 bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700">
        已确认
      </span>
    );
  }
  return (
    <span className="inline-flex items-center rounded-md border border-red-200 bg-red-100 px-2 py-0.5 text-xs font-medium text-red-700">
      失败{category ? `·${category}` : ""}
    </span>
  );
}

export function RecordsTab() {
  const accounts = useSparkAccounts();
  const [accountFilter, setAccountFilter] = useState("all");
  const accountId = accountFilter === "all" ? undefined : Number(accountFilter);
  const pager = useSparkRecordsPager(accountId);
  const rows = pager.page.data?.items ?? [];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <span className="text-sm text-muted-foreground">账号</span>
        <Select
          value={accountFilter}
          onValueChange={(v) => {
            setAccountFilter(v);
            pager.reset();
          }}
        >
          <SelectTrigger className="w-56">
            <SelectValue placeholder="全部账号" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部账号</SelectItem>
            {(accounts.data ?? []).map((a) => (
              <SelectItem key={a.id} value={String(a.id)}>
                {a.nickname || a.username}(@{a.unique_id})
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {pager.page.isPending ? (
        <Skeleton className="h-64" />
      ) : pager.page.isError ? (
        <p className="text-sm text-red-600">记录加载失败:{pager.page.error.message}</p>
      ) : rows.length === 0 ? (
        <p className="rounded-lg border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
          还没有发送记录。
        </p>
      ) : (
        <div className="rounded-lg border border-border/70">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-32">时间</TableHead>
                <TableHead className="w-28">账号</TableHead>
                <TableHead className="w-36">好友</TableHead>
                <TableHead>消息</TableHead>
                <TableHead className="w-24">状态</TableHead>
                <TableHead className="w-24">方式</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                    {fmtDateTime(r.sent_at)}
                  </TableCell>
                  <TableCell className="max-w-28 truncate">{r.account_label}</TableCell>
                  <TableCell className="max-w-36 truncate">{r.friend_key}</TableCell>
                  <TableCell className="max-w-80">
                    <p className="truncate" title={r.message}>
                      {r.message}
                    </p>
                    {r.detail ? (
                      <p className="truncate text-xs text-muted-foreground" title={r.detail}>
                        {r.detail}
                      </p>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    <StateBadge state={r.confirm_state} category={r.category} />
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {MODE_LABEL[r.run_mode] ?? r.run_mode}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <div className="flex items-center justify-end gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={!pager.hasHistory || pager.page.isFetching}
          onClick={pager.goBack}
        >
          <ChevronLeft className="size-4" />
          上一页
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={!pager.canLoadMore || pager.page.isFetching}
          onClick={pager.loadMore}
        >
          下一页
          <ChevronRight className="size-4" />
        </Button>
      </div>
    </div>
  );
}
