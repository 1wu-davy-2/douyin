/**
 * 登录页:居中卡片,Enter 提交;错误行内提示 + toast。
 */
import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Clapperboard, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { login } from "../api/endpoints";
import { qk } from "../api/queries";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";

export function LoginPage() {
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);

  const loginMut = useMutation({
    mutationFn: () => login(username.trim(), password),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: qk.auth });
    },
    onError: (e) => {
      setError(e.message);
      toast.error("登录失败", { description: e.message });
    },
  });

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!username.trim() || !password) {
      setError("请输入用户名和密码");
      return;
    }
    loginMut.mutate();
  };

  return (
    <div className="flex h-dvh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          <span className="mx-auto mb-1 flex size-11 items-center justify-center rounded-xl bg-primary/10 text-primary">
            <Clapperboard className="size-5" />
          </span>
          <CardTitle className="brand-serif tracking-wide">帧藏</CardTitle>
          <CardDescription>登录以管理本地作品归档</CardDescription>
        </CardHeader>
        <CardContent>
          <form className="space-y-4" onSubmit={handleSubmit}>
            <div className="space-y-1.5">
              <Label htmlFor="login-username">用户名</Label>
              <Input
                id="login-username"
                autoComplete="username"
                autoFocus
                placeholder="管理员用户名"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="login-password">密码</Label>
              <Input
                id="login-password"
                type="password"
                autoComplete="current-password"
                placeholder="密码"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            {error ? <p className="text-xs text-destructive">{error}</p> : null}
            <Button type="submit" className="w-full" disabled={loginMut.isPending}>
              {loginMut.isPending ? <Loader2 className="size-4 animate-spin" /> : null}
              登录
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
