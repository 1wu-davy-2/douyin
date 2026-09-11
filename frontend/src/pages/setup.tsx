/**
 * 首次初始化页:设置管理员用户名与密码(仅未初始化时可达,由认证门控制)。
 */
import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Clapperboard, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { setup } from "../api/endpoints";
import { qk } from "../api/queries";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";

export function SetupPage() {
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string | null>(null);

  const setupMut = useMutation({
    mutationFn: () => setup(username.trim(), password),
    onSuccess: () => {
      toast.success("初始化完成", { description: "已自动登录" });
      queryClient.invalidateQueries({ queryKey: qk.auth });
    },
    onError: (e) => {
      setError(e.message);
      toast.error("初始化失败", { description: e.message });
    },
  });

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (username.trim().length < 2) {
      setError("用户名至少 2 个字符");
      return;
    }
    if (password.length < 8) {
      setError("密码至少 8 位");
      return;
    }
    if (password !== confirm) {
      setError("两次输入的密码不一致");
      return;
    }
    setupMut.mutate();
  };

  return (
    <div className="flex h-dvh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          <span className="mx-auto mb-1 flex size-11 items-center justify-center rounded-xl bg-primary/10 text-primary">
            <Clapperboard className="size-5" />
          </span>
          <CardTitle>初始化管理员</CardTitle>
          <CardDescription>首次使用需创建管理员账号</CardDescription>
        </CardHeader>
        <CardContent>
          <form className="space-y-4" onSubmit={handleSubmit}>
            <div className="space-y-1.5">
              <Label htmlFor="setup-username">用户名</Label>
              <Input
                id="setup-username"
                autoComplete="username"
                autoFocus
                placeholder="至少 2 个字符"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="setup-password">密码</Label>
              <Input
                id="setup-password"
                type="password"
                autoComplete="new-password"
                placeholder="至少 8 位"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="setup-confirm">确认密码</Label>
              <Input
                id="setup-confirm"
                type="password"
                autoComplete="new-password"
                placeholder="再次输入密码"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
              />
            </div>
            {error ? <p className="text-xs text-destructive">{error}</p> : null}
            <Button type="submit" className="w-full" disabled={setupMut.isPending}>
              {setupMut.isPending ? <Loader2 className="size-4 animate-spin" /> : null}
              完成初始化
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
