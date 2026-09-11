/**
 * 已登录区域外壳:左侧栏(作品库/下载/监控/设置)+ 内容区。
 * 底部显示登录用户名,提供登出。
 */
import { NavLink, Outlet } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Clapperboard, Download, LogOut, Radar, Settings, Library } from "lucide-react";
import { toast } from "sonner";
import { logout } from "../api/endpoints";
import { useAuthStatus } from "../api/queries";
import { cn } from "../lib/utils";
import { Button } from "./ui/button";

const NAV_ITEMS = [
  { to: "/", label: "作品库", icon: Library, end: true },
  { to: "/downloads", label: "下载", icon: Download, end: false },
  { to: "/subscriptions", label: "监控", icon: Radar, end: false },
  { to: "/settings", label: "设置", icon: Settings, end: false },
] as const;

export function Layout() {
  const queryClient = useQueryClient();
  const { data: auth } = useAuthStatus();

  const logoutMut = useMutation({
    mutationFn: logout,
    onSettled: () => {
      // 清空全部缓存回到登录页(auth 查询由门组件重新拉取)
      queryClient.clear();
    },
    onError: (e) => toast.error("登出失败", { description: e.message }),
  });

  return (
    <div className="flex h-dvh">
      <aside className="flex w-56 shrink-0 flex-col border-r border-border/70 bg-card/40">
        <div className="flex items-center gap-2.5 border-b border-border/60 px-4 py-4">
          <span className="flex size-8 items-center justify-center rounded-lg bg-primary/10 text-primary">
            <Clapperboard className="size-4.5" />
          </span>
          <div className="leading-tight">
            <p className="text-sm font-semibold">抖音归档工具</p>
            <p className="text-[11px] text-muted-foreground">v2 · 本地归档</p>
          </div>
        </div>

        <nav className="flex-1 space-y-1 overflow-y-auto p-2">
          {NAV_ITEMS.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                cn(
                  "flex items-center gap-2.5 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                  "hover:bg-accent hover:text-accent-foreground",
                  isActive
                    ? "bg-accent text-accent-foreground"
                    : "text-muted-foreground",
                )
              }
            >
              <Icon className="size-4" />
              {label}
            </NavLink>
          ))}
        </nav>

        <div className="flex items-center gap-2 border-t border-border/60 p-3">
          <span className="flex size-7 shrink-0 items-center justify-center rounded-full bg-secondary text-xs font-semibold text-secondary-foreground">
            {(auth?.username ?? "?").slice(0, 1).toUpperCase()}
          </span>
          <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground" title={auth?.username}>
            {auth?.username ?? "未知用户"}
          </span>
          <Button
            variant="ghost"
            size="icon-sm"
            title="登出"
            disabled={logoutMut.isPending}
            onClick={() => logoutMut.mutate()}
          >
            <LogOut className="size-4" />
          </Button>
        </div>
      </aside>

      <main className="min-w-0 flex-1 overflow-y-auto">
        <Outlet />
      </main>
    </div>
  );
}
