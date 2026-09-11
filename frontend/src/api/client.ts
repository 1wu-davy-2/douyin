import { MOCK } from "../lib/env";

/** 401 时广播的事件名(AuthGate 监听后回到登录页)。 */
export const UNAUTHORIZED_EVENT = "douyin-archive:unauthorized";

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, detail: string) {
    super(detail);
    this.name = "ApiError";
    this.status = status;
  }
}

type RequestOptions = Omit<RequestInit, "body"> & {
  /** 以 JSON 序列化并发送的请求体(与原始 body 二选一)。 */
  json?: unknown;
  body?: BodyInit;
};

/**
 * fetch 封装:同源相对路径 + credentials include;
 * 非 /api/auth/* 路径收到 401 时广播登出事件;错误统一提取 `detail` 字段。
 */
export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { json, headers, ...rest } = options;

  const init: RequestInit = {
    credentials: "include",
    ...rest,
    headers: {
      ...(json !== undefined ? { "Content-Type": "application/json" } : {}),
      ...headers,
    },
  };
  if (json !== undefined) init.body = JSON.stringify(json);

  let res: Response;
  try {
    res = await fetch(path, init);
  } catch (err) {
    throw new ApiError(0, err instanceof Error ? `网络请求失败:${err.message}` : "网络请求失败");
  }

  if (res.status === 401 && !path.startsWith("/api/auth/")) {
    window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT));
  }

  if (!res.ok) {
    let detail = `请求失败(${res.status})`;
    try {
      const data = (await res.json()) as { detail?: unknown };
      if (typeof data.detail === "string" && data.detail) {
        detail = data.detail;
      } else if (data.detail && typeof data.detail === "object" && "message" in data.detail) {
        detail = String((data.detail as { message: unknown }).message);
      }
    } catch {
      // 保留默认错误文案
    }
    throw new ApiError(res.status, detail);
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** 播放器视频地址(后端原生支持 Range)。mock 模式由播放器自行替换演示源。 */
export function assetContentUrl(assetId: number): string {
  return `/api/assets/${assetId}/content`;
}

export { MOCK };
