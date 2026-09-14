/**
 * 火花(续火花)页:总览 / 账号 / 控制台 / 记录 / 设置 五个 Tab。
 */
import { useState } from "react";
import { Flame } from "lucide-react";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../../components/ui/tabs";
import { OverviewTab } from "./overview-tab";
import { AccountsTab } from "./accounts-tab";
import { ConsoleTab } from "./console-tab";
import { RecordsTab } from "./records-tab";
import { SettingsTab } from "./settings-tab";

export function SparkPage() {
  const [tab, setTab] = useState("overview");

  return (
    <div className="mx-auto max-w-6xl space-y-6 p-6">
      <header className="flex items-center gap-3">
        <span className="flex size-9 items-center justify-center rounded-lg bg-orange-100 text-orange-600">
          <Flame className="size-5" />
        </span>
        <div>
          <h1 className="text-lg font-semibold">火花</h1>
          <p className="text-xs text-muted-foreground">
            抖音好友自动续火花:扫码登录、勾选好友、定时/手动发送,引擎在本机沙箱内执行。
          </p>
        </div>
      </header>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="overview">总览</TabsTrigger>
          <TabsTrigger value="accounts">账号</TabsTrigger>
          <TabsTrigger value="console">控制台</TabsTrigger>
          <TabsTrigger value="records">记录</TabsTrigger>
          <TabsTrigger value="settings">设置</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="mt-4">
          <OverviewTab />
        </TabsContent>
        <TabsContent value="accounts" className="mt-4">
          <AccountsTab />
        </TabsContent>
        <TabsContent value="console" className="mt-4">
          <ConsoleTab />
        </TabsContent>
        <TabsContent value="records" className="mt-4">
          <RecordsTab />
        </TabsContent>
        <TabsContent value="settings" className="mt-4">
          <SettingsTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}
