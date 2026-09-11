/**
 * 扫描实时进度外部存储(useSyncExternalStore)。
 * useEvents 收到 scan.progress / scan.done 时写入;Library 的博主卡片订阅渲染进度条。
 */
import type { ScanDoneEvent, ScanProgressEvent } from "../api/types";

export interface LiveScan {
  scanId: number;
  creatorId: number;
  running: boolean;
  page: number;
  newCount: number;
  updatedCount: number;
  /** scan.done 后的终态;running 时为 null。 */
  result: { status: "succeeded" | "partial" | "failed"; pages: number; completeness: number; lastError: string | null } | null;
}

interface ScanStoreState {
  byCreator: ReadonlyMap<number, LiveScan>;
}

let state: ScanStoreState = { byCreator: new Map() };
const listeners = new Set<() => void>();

function emit(): void {
  state = { byCreator: new Map(state.byCreator) };
  for (const l of listeners) l();
}

function setEntry(scan: LiveScan): void {
  const next = new Map(state.byCreator);
  next.set(scan.creatorId, scan);
  state = { byCreator: next };
  emit();
}

export function applyScanProgress(event: ScanProgressEvent): void {
  setEntry({
    scanId: event.scan_id,
    creatorId: event.creator_id,
    running: event.status === "running",
    page: event.page,
    newCount: event.new_count,
    updatedCount: event.updated_count,
    result: null,
  });
}

export function applyScanDone(event: ScanDoneEvent): void {
  setEntry({
    scanId: event.scan_id,
    creatorId: event.creator_id,
    running: false,
    page: 0,
    newCount: event.new_count,
    updatedCount: 0,
    result: { status: event.status, pages: event.pages, completeness: event.completeness, lastError: event.last_error },
  });
  // 终态展示数秒后自动清除,避免残留条目
  window.setTimeout(() => {
    const cur = state.byCreator.get(event.creator_id);
    if (cur && cur.scanId === event.scan_id && !cur.running) {
      const next = new Map(state.byCreator);
      next.delete(event.creator_id);
      state = { byCreator: next };
      emit();
    }
  }, 6000);
}

export function clearScan(creatorId: number): void {
  if (!state.byCreator.has(creatorId)) return;
  const next = new Map(state.byCreator);
  next.delete(creatorId);
  state = { byCreator: next };
  emit();
}

export function subscribeScans(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function getScanSnapshot(): ScanStoreState {
  return state;
}
