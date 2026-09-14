/**
 * 火花发送实时日志外部存储(useSyncExternalStore)。
 * useEvents 收到 spark.* 事件时写入;Console Tab 订阅渲染滚动日志。
 * 行数上限 500(防长跑内存膨胀),先进先出裁剪。
 */

export type SparkLogKind = "progress" | "finished" | "account" | "friends" | "login";

export interface SparkLogLine {
  id: number;
  /** 收到事件时的本地时间(HH:MM:SS)。 */
  at: string;
  kind: SparkLogKind;
  accountId?: number;
  target?: string;
  state?: string;
  category?: string;
  detail?: string;
  /** 已渲染成人类可读的一行文本。 */
  text: string;
}

const MAX_LINES = 500;

let lines: SparkLogLine[] = [];
let nextId = 1;
const listeners = new Set<() => void>();

function emit(): void {
  lines = [...lines];
  for (const l of listeners) l();
}

function nowHMS(): string {
  const d = new Date();
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function pushSparkLog(line: Omit<SparkLogLine, "id" | "at">): void {
  const trimmed = [...lines, { ...line, id: nextId++, at: nowHMS() }];
  lines = trimmed.length > MAX_LINES ? trimmed.slice(trimmed.length - MAX_LINES) : trimmed;
  emit();
}

export function clearSparkLog(): void {
  lines = [];
  emit();
}

export function subscribeSparkLog(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function getSparkLogSnapshot(): SparkLogLine[] {
  return lines;
}
