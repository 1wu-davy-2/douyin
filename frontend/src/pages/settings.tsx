/**
 * 设置页:分组卡片(Provider / 扫描 / 下载 / 通知 / 安全)。
 * GET /api/settings 填充(敏感值打码),PATCH 保存 + toast;
 * Cookie / SMTP 密码仅在用户修改后随保存提交(打码值原样不发送)。
 */
import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { MailCheck, Save } from "lucide-react";
import { toast } from "sonner";
import { changePassword, patchSettings, testNotification } from "../api/endpoints";
import { qk, useSettings } from "../api/queries";
import type { Quality, Settings, SettingsPatch } from "../api/types";
import { QUALITIES } from "../api/types";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "../components/ui/card";
import { Input, Textarea } from "../components/ui/input";
import { Label } from "../components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Skeleton } from "../components/ui/skeleton";
import { Switch } from "../components/ui/switch";
import { isAbsolutePath } from "../lib/utils";

interface SettingsForm {
  provider_mode: Settings["provider_mode"];
  cookie: string;
  download_root: string;
  download_concurrency: string;
  download_quality: string;
  scan_page_delay_ms: string;
  incremental_stop_pages: string;
  scan_concurrency: string;
  completeness_gap_threshold: string;
  smtp: { host: string; port: string; username: string; password: string; from: string; to: string };
  notify_on_new_work: boolean;
  notify_on_failure: boolean;
}

function toForm(s: Settings): SettingsForm {
  return {
    provider_mode: s.provider_mode,
    cookie: s.cookie,
    // 契约 v1.3:绝对路径,空 = 默认 <data_dir>/downloads(后端未返回时按空串容错)
    download_root: s.download_root ?? "",
    download_concurrency: String(s.download_concurrency),
    download_quality: s.download_quality,
    scan_page_delay_ms: String(s.scan_page_delay_ms),
    incremental_stop_pages: String(s.incremental_stop_pages),
    scan_concurrency: String(s.scan_concurrency),
    completeness_gap_threshold: String(s.completeness_gap_threshold),
    smtp: { ...s.smtp, port: String(s.smtp.port) },
    notify_on_new_work: s.notify_on_new_work,
    notify_on_failure: s.notify_on_failure,
  };
}

function toInt(v: string, min: number, max: number): number | null {
  const n = Number(v);
  if (!Number.isInteger(n) || n < min || n > max) return null;
  return n;
}

function Field({ label, hint, htmlFor, children }: { label: string; hint?: string; htmlFor?: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint ? <p className="text-[11px] text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

export function SettingsPage() {
  const queryClient = useQueryClient();
  const settings = useSettings();
  const [form, setForm] = useState<SettingsForm | null>(null);
  // 首次加载时的打码值,用于判断用户是否真的修改了 Cookie / SMTP 密码
  const masked = useRef<{ cookie: string; smtpPassword: string } | null>(null);

  useEffect(() => {
    if (settings.data && form === null) {
      setForm(toForm(settings.data));
      masked.current = { cookie: settings.data.cookie, smtpPassword: settings.data.smtp.password };
    }
  }, [settings.data, form]);

  const saveMut = useMutation({
    mutationFn: (patch: SettingsPatch) => patchSettings(patch),
    onSuccess: () => {
      toast.success("已保存", { description: "设置已生效" });
      queryClient.invalidateQueries({ queryKey: qk.settings });
    },
    onError: (e) => toast.error("保存失败", { description: e.message }),
  });

  const testMailMut = useMutation({
    mutationFn: testNotification,
    onSuccess: (res) => {
      if (res.ok) toast.success("测试邮件已发送");
      else toast.error("测试邮件发送失败", { description: res.error ?? "请检查 SMTP 配置" });
    },
    onError: (e) => toast.error("测试邮件发送失败", { description: e.message }),
  });

  if (settings.isPending || !form) {
    return (
      <div className="mx-auto max-w-3xl space-y-4 p-4">
        {Array.from({ length: 3 }, (_, i) => (
          <Card key={i} className="p-5">
            <Skeleton className="h-4 w-28" />
            <div className="mt-4 space-y-3">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </div>
          </Card>
        ))}
      </div>
    );
  }

  const update = (patch: Partial<SettingsForm>) => setForm((f) => (f ? { ...f, ...patch } : f));
  const updateSmtp = (patch: Partial<SettingsForm["smtp"]>) =>
    setForm((f) => (f ? { ...f, smtp: { ...f.smtp, ...patch } } : f));

  const saveProvider = () => {
    const patch: SettingsPatch = {
      provider_mode: form.provider_mode,
      ...(form.cookie !== masked.current?.cookie ? { cookie: form.cookie } : {}),
    };
    saveMut.mutate(patch);
  };

  const saveScan = () => {
    const delay = toInt(form.scan_page_delay_ms, 1000, 10000);
    const stopPages = toInt(form.incremental_stop_pages, 1, 10);
    const concurrency = toInt(form.scan_concurrency, 1, 5);
    const gap = toInt(form.completeness_gap_threshold, 1, 50);
    if (delay === null || stopPages === null || concurrency === null || gap === null) {
      toast.error("扫描参数不合法", { description: "请检查输入范围:翻页间隔 1000~10000,空页停止 1~10,扫描并发 1~5,缺口阈值 1~50" });
      return;
    }
    saveMut.mutate({
      scan_page_delay_ms: delay,
      incremental_stop_pages: stopPages,
      scan_concurrency: concurrency,
      completeness_gap_threshold: gap,
    });
  };

  const saveDownload = () => {
    const concurrency = toInt(form.download_concurrency, 1, 8);
    if (concurrency === null) {
      toast.error("下载并发不合法", { description: "并发数需为 1 ~ 8 的整数" });
      return;
    }
    // 契约 v1.3:绝对路径,空 = 默认 <data_dir>/downloads
    const root = form.download_root.trim();
    if (root && !isAbsolutePath(root)) {
      toast.error("下载根目录不合法", { description: "需为绝对路径,如 D:\\Media\\Douyin" });
      return;
    }
    saveMut.mutate({ download_root: root, download_concurrency: concurrency, download_quality: form.download_quality });
  };

  const saveNotify = () => {
    const port = toInt(form.smtp.port, 1, 65535);
    if (port === null) {
      toast.error("SMTP 端口不合法", { description: "端口需为 1 ~ 65535 的整数" });
      return;
    }
    saveMut.mutate({
      smtp: {
        host: form.smtp.host,
        port,
        username: form.smtp.username,
        ...(form.smtp.password !== masked.current?.smtpPassword ? { password: form.smtp.password } : {}),
        from: form.smtp.from,
        to: form.smtp.to,
      },
      notify_on_new_work: form.notify_on_new_work,
      notify_on_failure: form.notify_on_failure,
    });
  };

  return (
    <div className="mx-auto max-w-3xl space-y-4 p-4">
      {/* Provider */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Provider</CardTitle>
          <CardDescription>数据源与登录态</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <Field label="运行模式" htmlFor="set-provider-mode">
            <Select
              value={form.provider_mode}
              onValueChange={(v) => update({ provider_mode: v as SettingsForm["provider_mode"] })}
            >
              <SelectTrigger id="set-provider-mode">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="auto">auto · 自动(按需拉起侧车)</SelectItem>
                <SelectItem value="sidecar">sidecar · 始终使用侧车</SelectItem>
                <SelectItem value="mock">mock · 演示数据</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          <Field
            label="Cookie"
            htmlFor="set-cookie"
            hint="网页版 Cookie,已配置时打码显示;保持原样表示不修改"
          >
            <Textarea
              id="set-cookie"
              rows={4}
              className="font-mono text-xs"
              placeholder="粘贴抖音网页版 Cookie…"
              value={form.cookie}
              onChange={(e) => update({ cookie: e.target.value })}
            />
          </Field>
        </CardContent>
        <CardFooter>
          <Button onClick={saveProvider} disabled={saveMut.isPending}>
            <Save className="size-4" /> {saveMut.isPending ? "保存中…" : "保存"}
          </Button>
          <p className="ml-3 text-[11px] text-muted-foreground">保存后自动生效(修改 Cookie 会立即重载 Provider)</p>
        </CardFooter>
      </Card>

      {/* 扫描 */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">扫描</CardTitle>
          <CardDescription>增量扫描节奏与完整性对账</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-4">
          <Field label="扫描并发(博主数)" htmlFor="set-scanconc" hint="1 ~ 5,同时扫描的博主数">
            <Input
              id="set-scanconc"
              inputMode="numeric"
              value={form.scan_concurrency}
              onChange={(e) => update({ scan_concurrency: e.target.value })}
            />
          </Field>
          <Field label="翻页间隔(毫秒)" htmlFor="set-delay" hint="1000 ~ 10000">
            <Input
              id="set-delay"
              inputMode="numeric"
              value={form.scan_page_delay_ms}
              onChange={(e) => update({ scan_page_delay_ms: e.target.value })}
            />
          </Field>
          <Field label="增量空页停止页数" htmlFor="set-stop" hint="1 ~ 10,连续空页即停止">
            <Input
              id="set-stop"
              inputMode="numeric"
              value={form.incremental_stop_pages}
              onChange={(e) => update({ incremental_stop_pages: e.target.value })}
            />
          </Field>
          <Field label="完整度缺口阈值(%)" htmlFor="set-gap" hint="1 ~ 50,超过则自动补扫">
            <Input
              id="set-gap"
              inputMode="numeric"
              value={form.completeness_gap_threshold}
              onChange={(e) => update({ completeness_gap_threshold: e.target.value })}
            />
          </Field>
        </CardContent>
        <CardFooter>
          <Button onClick={saveScan} disabled={saveMut.isPending}>
            <Save className="size-4" /> {saveMut.isPending ? "保存中…" : "保存"}
          </Button>
        </CardFooter>
      </Card>

      {/* 下载 */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">下载</CardTitle>
          <CardDescription>并发、默认画质与存储位置</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <Field label="并发数" htmlFor="set-concurrency" hint="1 ~ 8">
            <Input
              id="set-concurrency"
              inputMode="numeric"
              value={form.download_concurrency}
              onChange={(e) => update({ download_concurrency: e.target.value })}
            />
          </Field>
          <Field label="默认画质" htmlFor="set-quality">
            <Select value={form.download_quality} onValueChange={(v) => update({ download_quality: v })}>
              <SelectTrigger id="set-quality">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {QUALITIES.map((q: Quality) => (
                  <SelectItem key={q} value={q}>
                    {q}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <div className="md:col-span-2">
            <Field
              label="下载根目录"
              htmlFor="set-download-root"
              hint="修改后仅新下载落入新目录,历史文件位置不变(资产按绝对路径记录);保存时自动创建目录"
            >
              <Input
                id="set-download-root"
                className="font-mono text-xs"
                placeholder={"默认 data\\downloads(留空 = 跟随默认)"}
                value={form.download_root}
                onChange={(e) => update({ download_root: e.target.value })}
              />
            </Field>
          </div>
        </CardContent>
        <CardFooter>
          <Button onClick={saveDownload} disabled={saveMut.isPending}>
            <Save className="size-4" /> {saveMut.isPending ? "保存中…" : "保存"}
          </Button>
        </CardFooter>
      </Card>

      {/* 通知 */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">通知</CardTitle>
          <CardDescription>SMTP 邮件通知</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-4 md:grid-cols-2">
            <Field label="SMTP 服务器" htmlFor="set-smtp-host">
              <Input
                id="set-smtp-host"
                placeholder="smtp.example.com"
                value={form.smtp.host}
                onChange={(e) => updateSmtp({ host: e.target.value })}
              />
            </Field>
            <Field label="端口" htmlFor="set-smtp-port" hint="常用:465(SSL)/ 587(STARTTLS)">
              <Input
                id="set-smtp-port"
                inputMode="numeric"
                value={form.smtp.port}
                onChange={(e) => updateSmtp({ port: e.target.value })}
              />
            </Field>
            <Field label="用户名" htmlFor="set-smtp-username">
              <Input
                id="set-smtp-username"
                autoComplete="off"
                value={form.smtp.username}
                onChange={(e) => updateSmtp({ username: e.target.value })}
              />
            </Field>
            <Field label="密码 / 授权码" htmlFor="set-smtp-password" hint="已配置时打码显示;保持原样表示不修改">
              <Input
                id="set-smtp-password"
                type="password"
                autoComplete="new-password"
                value={form.smtp.password}
                onChange={(e) => updateSmtp({ password: e.target.value })}
              />
            </Field>
            <Field label="发件人" htmlFor="set-smtp-from">
              <Input
                id="set-smtp-from"
                placeholder="archive@example.com"
                value={form.smtp.from}
                onChange={(e) => updateSmtp({ from: e.target.value })}
              />
            </Field>
            <Field label="收件人" htmlFor="set-smtp-to">
              <Input
                id="set-smtp-to"
                placeholder="you@example.com"
                value={form.smtp.to}
                onChange={(e) => updateSmtp({ to: e.target.value })}
              />
            </Field>
          </div>

          <div className="flex flex-col gap-3 rounded-lg border border-border px-3 py-3 sm:flex-row sm:items-center sm:justify-between">
            <SwitchRow
              id="set-notify-new"
              label="新作品通知"
              description="扫描发现新作品时发送邮件"
              checked={form.notify_on_new_work}
              onChange={(notify_on_new_work) => update({ notify_on_new_work })}
            />
            <SwitchRow
              id="set-notify-failure"
              label="失败通知"
              description="下载任务最终失败时发送邮件"
              checked={form.notify_on_failure}
              onChange={(notify_on_failure) => update({ notify_on_failure })}
            />
          </div>
        </CardContent>
        <CardFooter className="gap-2">
          <Button onClick={saveNotify} disabled={saveMut.isPending}>
            <Save className="size-4" /> {saveMut.isPending ? "保存中…" : "保存"}
          </Button>
          <Button variant="outline" disabled={testMailMut.isPending} onClick={() => testMailMut.mutate()}>
            <MailCheck className="size-4" /> {testMailMut.isPending ? "发送中…" : "发送测试邮件"}
          </Button>
        </CardFooter>
      </Card>

      <SecurityCard />
    </div>
  );
}

function SwitchRow({
  id,
  label,
  description,
  checked,
  onChange,
}: {
  id: string;
  label: string;
  description: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <div>
        <Label htmlFor={id} className="text-sm">{label}</Label>
        <p className="mt-0.5 text-[11px] text-muted-foreground">{description}</p>
      </div>
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
    </div>
  );
}

/** 安全:修改管理员密码(POST /api/auth/password)。 */
function SecurityCard() {
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirm, setConfirm] = useState("");

  const changeMut = useMutation({
    mutationFn: () => changePassword(oldPassword, newPassword),
    onSuccess: () => {
      toast.success("密码已修改");
      setOldPassword("");
      setNewPassword("");
      setConfirm("");
    },
    onError: (e) => toast.error("修改密码失败", { description: e.message }),
  });

  const handleSubmit = () => {
    if (!oldPassword || !newPassword) {
      toast.error("请填写当前密码与新密码");
      return;
    }
    if (newPassword.length < 8) {
      toast.error("新密码至少 8 位");
      return;
    }
    if (newPassword !== confirm) {
      toast.error("两次输入的新密码不一致");
      return;
    }
    changeMut.mutate();
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">安全</CardTitle>
        <CardDescription>修改管理员密码</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4 md:grid-cols-3">
        <Field label="当前密码" htmlFor="set-pwd-old">
          <Input
            id="set-pwd-old"
            type="password"
            autoComplete="current-password"
            value={oldPassword}
            onChange={(e) => setOldPassword(e.target.value)}
          />
        </Field>
        <Field label="新密码" htmlFor="set-pwd-new" hint="至少 8 位">
          <Input
            id="set-pwd-new"
            type="password"
            autoComplete="new-password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
        </Field>
        <Field label="确认新密码" htmlFor="set-pwd-confirm">
          <Input
            id="set-pwd-confirm"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
        </Field>
      </CardContent>
      <CardFooter>
        <Button onClick={handleSubmit} disabled={changeMut.isPending}>
          {changeMut.isPending ? "提交中…" : "修改密码"}
        </Button>
      </CardFooter>
    </Card>
  );
}
