/**
 * 应用装配:认证门 + 路由 + 全局 Toaster / PlayerDialog。
 * 认证门依据 GET /api/auth/status:未初始化 → Setup;未登录 → Login;已登录 → Layout + 路由。
 * 非 auth 接口收到 401 时(client.ts 广播)自动回到登录页。
 */
import { useEffect } from "react";
import { Navigate, Route, Routes } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Toaster } from "sonner";
import { Loader2 } from "lucide-react";
import { UNAUTHORIZED_EVENT } from "./api/client";
import { qk, useAuthStatus } from "./api/queries";
import { useEvents } from "./api/useEvents";
import { Layout } from "./components/layout";
import { PlayerDialog } from "./components/player/player-dialog";
import { SetupPage } from "./pages/setup";
import { LoginPage } from "./pages/login";
import { LibraryPage } from "./pages/library";
import { DownloadsPage } from "./pages/downloads";
import { SubscriptionsPage } from "./pages/subscriptions";
import { SettingsPage } from "./pages/settings";
import { SparkPage } from "./pages/spark/spark-page";

function Splash() {
  return (
    <div className="flex h-dvh flex-col items-center justify-center gap-3 text-muted-foreground">
      <Loader2 className="size-6 animate-spin" />
      <p className="text-sm">正在连接服务…</p>
    </div>
  );
}

function AuthGate() {
  const queryClient = useQueryClient();
  const { data, isPending } = useAuthStatus();

  useEffect(() => {
    const onUnauthorized = () => {
      void queryClient.invalidateQueries({ queryKey: qk.auth });
    };
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, [queryClient]);

  if (isPending) return <Splash />;
  if (!data?.initialized) return <SetupPage />;
  if (!data?.authenticated) return <LoginPage />;

  return (
    <>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<LibraryPage />} />
          <Route path="downloads" element={<DownloadsPage />} />
          <Route path="subscriptions" element={<SubscriptionsPage />} />
          <Route path="spark" element={<SparkPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
      {/* 播放器由 lib/player-store 驱动,全局挂载一次 */}
      <PlayerDialog />
      <EventBridge />
    </>
  );
}

/** 已登录后建立 SSE 连接(download/scan/provider 事件驱动缓存)。 */
function EventBridge() {
  useEvents(true);
  return null;
}

export default function App() {
  return (
    <>
      <AuthGate />
      <Toaster theme="light" position="top-center" richColors closeButton />
    </>
  );
}
