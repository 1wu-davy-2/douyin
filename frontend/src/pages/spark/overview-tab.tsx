/**
 * 总览 Tab:引擎健康灯 / 账号状态卡 / 今日计数 / 发送窗口状态。
 * 数据来自 useSparkOverview(30s 兜底轮询 + SSE invalidate)。
 */
import { Activity, Clock, Flame, HeartHandshake, Users } from "lucide-react";
import { useSparkOverview } from "../../api/queries";
import { Card, CardContent, CardHeader, CardTitle } from "../../components/ui/card";
import { Skeleton } from "../../components/ui/skeleton";
import { SparkStatusBadge, fmtDateTime } from "./shared";

function StatCard({
  icon: Icon,
  label,
  value,
  hint,
}: {
  icon: typeof Flame;
  label: string;
  value: string | number;
  hint?: string;
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between space-y-0 pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">{label}</CardTitle>
        <Icon className="size-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        <p className="text-2xl font-semibold tabular-nums">{value}</p>
        {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
      </CardContent>
    </Card>
  );
}

export function OverviewTab() {
  const overview = useSparkOverview();

  if (overview.isPending) {
    return (
      <div className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      </div>
    );
  }

  if (overview.isError || !overview.data) {
    return (
      <Card>
        <CardContent className="pt-6 text-sm text-muted-foreground">
          总览加载失败:{overview.error?.message ?? "未知错误"}
        </CardContent>
      </Card>
    );
  }

  const { engine, accounts, today, window: win } = overview.data;

  return (
    <div className="space-y-6">
      {/* 引擎 + 窗口状态条 */}
      <div className="flex flex-wrap items-center gap-3 rounded-lg border border-border/70 bg-card/60 px-4 py-3 text-sm">
        <span className="flex items-center gap-2">
          <Activity className="size-4 text-muted-foreground" />
          引擎:
          {engine.ok ? (
            <span className="flex items-center gap-1.5 font-medium text-emerald-600">
              <span className="size-2 rounded-full bg-emerald-500" />
              运行中(v{engine.version ?? "?"})
            </span>
          ) : (
            <span className="flex items-center gap-1.5 font-medium text-red-600">
              <span className="size-2 rounded-full bg-red-500" />
              不可用{engine.detail ? ` · ${engine.detail}` : ""}
            </span>
          )}
        </span>
        <span className="text-border">|</span>
        <span className="flex items-center gap-2">
          <Clock className="size-4 text-muted-foreground" />
          发送窗口:
          {win.enabled ? (
            win.in_window ? (
              <span className="font-medium text-emerald-600">窗口内(当前 {win.now})</span>
            ) : (
              <span className="font-medium text-muted-foreground">窗口外(当前 {win.now})</span>
            )
          ) : (
            <span className="font-medium text-muted-foreground">未启用(全天生效)</span>
          )}
        </span>
        {engine.task_running ? (
          <>
            <span className="text-border">|</span>
            <span className="font-medium text-blue-600">引擎任务执行中…</span>
          </>
        ) : null}
      </div>

      {/* 今日计数 */}
      <div className="grid gap-4 sm:grid-cols-3">
        <StatCard icon={HeartHandshake} label="今日已确认" value={today.strong} hint="收到对方强确认" />
        <StatCard icon={Flame} label="今日弱确认" value={today.weak} hint="已发送但未强确认" />
        <StatCard icon={Users} label="今日失败" value={today.failed} hint={'可用「补发失败」重试'} />
      </div>

      {/* 账号状态 */}
      <div className="space-y-2">
        <h3 className="text-sm font-medium text-muted-foreground">账号状态({accounts.length})</h3>
        {accounts.length === 0 ? (
          <Card>
            <CardContent className="pt-6 text-sm text-muted-foreground">
              还没有账号。到「账号」标签页扫码添加。
            </CardContent>
          </Card>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {accounts.map((a) => (
              <Card key={a.id}>
                <CardContent className="space-y-2 pt-5">
                  <div className="flex items-center justify-between gap-2">
                    <p className="min-w-0 truncate font-medium">{a.nickname || a.username}</p>
                    <SparkStatusBadge status={a.status} />
                  </div>
                  <p className="truncate text-xs text-muted-foreground">@{a.unique_id}</p>
                  {a.status === "cooldown" && a.cooldown_until ? (
                    <p className="text-xs text-orange-600">冷却至 {fmtDateTime(a.cooldown_until)}</p>
                  ) : null}
                  {a.status === "error" && a.last_error ? (
                    <p className="line-clamp-2 text-xs text-red-600">{a.last_error}</p>
                  ) : null}
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
