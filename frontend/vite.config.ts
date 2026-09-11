import { defineConfig, type Connect, type Plugin } from "vite";
import type { IncomingMessage, ServerResponse } from "node:http";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { mockRoute, subscribeMockEvents } from "./src/api/mock";

const mockEnabled = process.env.VITE_MOCK === "1";

function readBody(req: IncomingMessage): Promise<unknown> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      if (chunks.length === 0) return resolve(undefined);
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")) as unknown);
      } catch {
        resolve(undefined);
      }
    });
    req.on("error", () => resolve(undefined));
  });
}

function parseCookies(header: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of (header ?? "").split(";")) {
    const idx = part.indexOf("=");
    if (idx > 0) out[part.slice(0, idx).trim()] = decodeURIComponent(part.slice(idx + 1).trim());
  }
  return out;
}

async function handleMock(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? "/", "http://mock.local");
  if (url.pathname === "/events") {
    res.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    });
    res.write("retry: 3000\n\n");
    const unsubscribe = subscribeMockEvents((event) => {
      res.write(`event: ${event.type}\ndata: ${JSON.stringify(event.data)}\n\n`);
    });
    const ping = setInterval(() => res.write(": ping\n\n"), 15000);
    req.on("close", () => {
      clearInterval(ping);
      unsubscribe();
      res.end();
    });
    return;
  }
  const body = await readBody(req);
  const session = parseCookies(req.headers.cookie)["dy_session"] ?? null;
  const out = mockRoute({
    method: (req.method ?? "GET").toUpperCase(),
    path: url.pathname,
    query: url.searchParams,
    body,
    session,
  });
  if (out.setSession) {
    res.setHeader("Set-Cookie", `dy_session=${out.setSession}; Path=/; HttpOnly; SameSite=Lax; Max-Age=604800`);
  }
  if (out.clearSession) {
    res.setHeader("Set-Cookie", "dy_session=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0");
  }
  res.statusCode = out.status;
  res.setHeader("Content-Type", "application/json; charset=utf-8");
  res.end(JSON.stringify(out.json));
}

function mockPlugin(): Plugin {
  return {
    name: "douyin-mock-api",
    configureServer(server) {
      server.middlewares.use("/api", (_req: IncomingMessage, res: ServerResponse, next: Connect.NextFunction) => {
        void handleMock(_req, res).catch(() => {
          if (!res.headersSent) {
            res.statusCode = 500;
            res.setHeader("Content-Type", "application/json; charset=utf-8");
          }
          res.end(JSON.stringify({ detail: "mock internal error" }));
        });
        void next;
      });
    },
  };
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss(), ...(mockEnabled ? [mockPlugin()] : [])],
  server: {
    port: 5173,
    proxy: mockEnabled
      ? undefined
      : {
          "/api": {
            target: process.env.VITE_API_PROXY_TARGET ?? "http://127.0.0.1:8787",
            changeOrigin: true,
          },
        },
  },
});
