/**
 * 设置 Tab:SparkSettings 表单(模板/变体/一言/窗口/策略/扫描/失败暂停)+
 * 「导出 Cookie 到归档」快捷入口。保存 PUT /api/spark/settings(后端 Normalize)。
 */
import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Loader2, Save, Upload } from "lucide-react";
import { toast } from "sonner";
import { exportSparkCookies, putSparkSettings } from "../../api/spark";
import type { SparkSettings } from "../../api/spark-types";
import { useSparkAccounts, useSparkSettings } from "../../api/queries";
import { Button } from "../../components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "../../components/ui/card";
import { Input, Textarea } from "../../components/ui/input";
import { Label } from "../../components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select";
import { Skeleton } from "../../components/ui/skeleton";
import { Switch } from "../../components/ui/switch";

type NumForm = Record<string, string>;

interface SettingsForm {
  messageTemplate: string;
  messageVariants: string; // 每行一条
  hitokotoTypes: string; // 逗号分隔
  nums: NumForm;
}

const NUM_FIELDS: { key: string; label: string; min: number; max: number; hint?: string }[] = [
  { key: "startHour", label: "窗口开始(时)", min: 0, max: 23 },
  { key: "endHour", label: "窗口结束(时)", min: 0, max: 23 },
  { key: "intervalMinutes", label: "间隔宽限(分)", min: 0, max: 1440 },
  { key: "messageIntervalSecondsMin", label: "消息间隔下限(秒)", min: 25, max: 3600, hint: "风控红线:不低于 25" },
  { key: "messageIntervalSecondsMax", label: "消息间隔上限(秒)", min: 25, max: 3600 },
  { key: "accountStartDelaySecondsMin", label: "账号起始延迟下限(秒)", min: 0, max: 600 },
  { key: "accountStartDelaySecondsMax", label: "账号起始延迟上限(秒)", min: 0, max: 600 },
  { key: "maxScanSeconds", label: "好友扫描上限(秒)", min: 5, max: 600 },
  { key: "idleScanSeconds", label: "扫描空闲(秒)", min: 0, max: 60 },
  { key: "scrollStepPx", label: "滚动步长(px)", min: 50, max: 2000 },
  { key: "scrollDelaySeconds", label: "滚动间隔(秒)", min: 0, max: 10 },
  { key: "attempts", label: "失败暂停阈值(次/日)", min: 1, max: 50 },
  { key: "cooldownMinutes", label: "失败冷却(分)", min: 1, max: 720 },
];

function toForm(s: SparkSettings): SettingsForm {
  return {
    messageTemplate: s.messageTemplate,
    messageVariants: s.messageVariants.join("\n"),
    hitokotoTypes: s.hitokotoTypes.join(","),
    nums: {
      startHour: String(s.sendWindow.startHour),
      endHour: String(s.sendWindow.endHour),
      intervalMinutes: String(s.sendWindow.intervalMinutes),
      messageIntervalSecondsMin: String(s.sendStrategy.messageIntervalSecondsMin),
      messageIntervalSecondsMax: String(s.sendStrategy.messageIntervalSecondsMax),
      accountStartDelaySecondsMin: String(s.sendStrategy.accountStartDelaySecondsMin),
      accountStartDelaySecondsMax: String(s.sendStrategy.accountStartDelaySecondsMax),
      maxScanSeconds: String(s.friendScan.maxScanSeconds),
      idleScanSeconds: String(s.friendScan.idleScanSeconds),
      scrollStepPx: String(s.friendScan.scrollStepPx),
      scrollDelaySeconds: String(s.friendScan.scrollDelaySeconds),
      attempts: String(s.accountFailurePause.attempts),
      cooldownMinutes: String(s.accountFailurePause.cooldownMinutes),
    },
  };
}

function toPayload(form: SettingsForm, windowEnabled: boolean, shuffle: boolean): SparkSettings | null {
  const n: Record<string, number> = {};
  for (const f of NUM_FIELDS) {
    const v = Number(form.nums[f.key]);
    if (!Number.isInteger(v) || v < f.min || v > f.max) {
      toast.error(`${f.label} 需为 ${f.min}-${f.max} 的整数`);
      return null;
    }
    n[f.key] = v;
  }
  return {
    messageTemplate: form.messageTemplate,
    messageVariants: form.messageVariants
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean),
    hitokotoTypes: form.hitokotoTypes
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
    sendWindow: {
      enabled: windowEnabled,
      startHour: n.startHour,
      endHour: n.endHour,
      intervalMinutes: n.intervalMinutes,
    },
    sendStrategy: {
      shuffleTargets: shuffle,
      accountStartDelaySecondsMin: n.accountStartDelaySecondsMin,
      accountStartDelaySecondsMax: n.accountStartDelaySecondsMax,
      messageIntervalSecondsMin: n.messageIntervalSecondsMin,
      messageIntervalSecondsMax: n.messageIntervalSecondsMax,
    },
    friendScan: {
      maxScanSeconds: n.maxScanSeconds,
      idleScanSeconds: n.idleScanSeconds,
      scrollStepPx: n.scrollStepPx,
      scrollDelaySeconds: n.scrollDelaySeconds,
    },
    accountFailurePause: { attempts: n.attempts, cooldownMinutes: n.cooldownMinutes },
  };
}

export function SettingsTab() {
  const queryClient = useQueryClient();
  const settings = useSparkSettings();
  const accounts = useSparkAccounts();
  const [form, setForm] = useState<SettingsForm | null>(null);
  const [windowEnabled, setWindowEnabled] = useState(true);
  const [shuffle, setShuffle] = useState(true);
  const [exportId, setExportId] = useState("0");

  useEffect(() => {
    if (settings.data && form === null) {
      setForm(toForm(settings.data));
      setWindowEnabled(settings.data.sendWindow.enabled);
      setShuffle(settings.data.sendStrategy.shuffleTargets);
    }
  }, [settings.data, form]);

  useEffect(() => {
    if (accounts.data?.length && exportId === "0") setExportId(String(accounts.data[0].id));
  }, [accounts.data, exportId]);

  const saveMut = useMutation({
    mutationFn: (payload: SparkSettings) => putSparkSettings(payload),
    onSuccess: (s) => {
      toast.success("已保存", {
        description:
          s.sendStrategy.messageIntervalSecondsMin < 25
            ? "消息间隔已被后端钳制到 25s(风控红线)"
            : "设置已生效",
      });
      void queryClient.invalidateQueries({ queryKey: ["spark"] });
    },
    onError: (e) => toast.error("保存失败", { description: e.message }),
  });

  const exportMut = useMutation({
    mutationFn: () => exportSparkCookies(Number(exportId)),
    onSuccess: (r) =>
      toast.success("Cookie 已同步到归档", { description: `共 ${r.cookie_count} 条,侧车将热加载` }),
    onError: (e) => toast.error("导出失败", { description: e.message }),
  });

  if (settings.isPending || !form) return <Skeleton className="h-96" />;
  if (settings.isError)
    return <p className="text-sm text-red-600">设置加载失败:{settings.error.message}</p>;

  const setNum = (key: string, v: string) =>
    setForm((prev) => (prev ? { ...prev, nums: { ...prev.nums, [key]: v } } : prev));

  const save = () => {
    const payload = toPayload(form, windowEnabled, shuffle);
    if (payload) saveMut.mutate(payload);
  };

  return (
    <div className="max-w-3xl space-y-6">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">消息内容</CardTitle>
          <CardDescription>
            模板可用占位符 {"{friend}"}(好友昵称);变体每行一条,发送时随机挑选或走一言。
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-1.5">
            <Label>消息模板</Label>
            <Textarea
              rows={3}
              value={form.messageTemplate}
              onChange={(e) => setForm({ ...form, messageTemplate: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label>文案变体(每行一条,留空则只用模板)</Label>
            <Textarea
              rows={4}
              value={form.messageVariants}
              onChange={(e) => setForm({ ...form, messageVariants: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label>一言类型(逗号分隔,如 a,b,c;留空关闭一言)</Label>
            <Input
              value={form.hitokotoTypes}
              onChange={(e) => setForm({ ...form, hitokotoTypes: e.target.value })}
            />
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle className="text-base">发送窗口</CardTitle>
            <CardDescription>窗口外调度器不派发;手动触发不受限。</CardDescription>
          </div>
          <Switch checked={windowEnabled} onCheckedChange={setWindowEnabled} />
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-3">
          {["startHour", "endHour", "intervalMinutes"].map((key) => {
            const f = NUM_FIELDS.find((x) => x.key === key)!;
            return (
              <div key={key} className="space-y-1.5">
                <Label>{f.label}</Label>
                <Input
                  inputMode="numeric"
                  value={form.nums[key] ?? ""}
                  onChange={(e) => setNum(key, e.target.value)}
                />
              </div>
            );
          })}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle className="text-base">发送策略</CardTitle>
            <CardDescription>目标乱序 + 账号/消息随机间隔(下限 25s 为风控红线)。</CardDescription>
          </div>
          <Switch checked={shuffle} onCheckedChange={setShuffle} />
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          {[
            "messageIntervalSecondsMin",
            "messageIntervalSecondsMax",
            "accountStartDelaySecondsMin",
            "accountStartDelaySecondsMax",
          ].map((key) => {
            const f = NUM_FIELDS.find((x) => x.key === key)!;
            return (
              <div key={key} className="space-y-1.5">
                <Label>{f.label}</Label>
                <Input
                  inputMode="numeric"
                  value={form.nums[key] ?? ""}
                  onChange={(e) => setNum(key, e.target.value)}
                />
                {f.hint ? <p className="text-[11px] text-muted-foreground">{f.hint}</p> : null}
              </div>
            );
          })}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">好友扫描参数</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          {["maxScanSeconds", "idleScanSeconds", "scrollStepPx", "scrollDelaySeconds"].map((key) => {
            const f = NUM_FIELDS.find((x) => x.key === key)!;
            return (
              <div key={key} className="space-y-1.5">
                <Label>{f.label}</Label>
                <Input
                  inputMode="numeric"
                  value={form.nums[key] ?? ""}
                  onChange={(e) => setNum(key, e.target.value)}
                />
              </div>
            );
          })}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">失败暂停</CardTitle>
          <CardDescription>单账号当日失败达到阈值后冷却,冷却期调度器跳过。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          {["attempts", "cooldownMinutes"].map((key) => {
            const f = NUM_FIELDS.find((x) => x.key === key)!;
            return (
              <div key={key} className="space-y-1.5">
                <Label>{f.label}</Label>
                <Input
                  inputMode="numeric"
                  value={form.nums[key] ?? ""}
                  onChange={(e) => setNum(key, e.target.value)}
                />
              </div>
            );
          })}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Cookie 同步</CardTitle>
          <CardDescription>把火花账号的登录态写入帧藏 data/.cookie,归档侧车热加载。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-center gap-3">
          <Select value={exportId} onValueChange={setExportId}>
            <SelectTrigger className="w-56">
              <SelectValue placeholder="选择账号" />
            </SelectTrigger>
            <SelectContent>
              {(accounts.data ?? []).map((a) => (
                <SelectItem key={a.id} value={String(a.id)}>
                  {a.nickname || a.username}(@{a.unique_id})
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
            variant="outline"
            disabled={exportId === "0" || exportMut.isPending}
            onClick={() => exportMut.mutate()}
          >
            {exportMut.isPending ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Upload className="size-4" />
            )}
            导出 Cookie 到归档
          </Button>
        </CardContent>
      </Card>

      <div className="sticky bottom-4 flex justify-end">
        <Button size="lg" disabled={saveMut.isPending} onClick={save}>
          {saveMut.isPending ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <Save className="size-4" />
          )}
          保存设置
        </Button>
      </div>
    </div>
  );
}
