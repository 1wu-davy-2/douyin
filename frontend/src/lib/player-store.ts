/**
 * 全局播放器外部存储(useSyncExternalStore)。
 * Library 已下载行 / Downloads 已完成行调用 openPlayer 打开;Dialog 挂在 Layout。
 * 播放列表保持调用方传入的顺序(“当前列表顺序”),上一条/下一条在列表内移动。
 */
import { useCallback, useSyncExternalStore } from "react";

export interface PlayerItem {
  workId: number;
  title: string;
  creatorName?: string;
}

interface PlayerState {
  open: boolean;
  items: PlayerItem[];
  index: number;
}

let state: PlayerState = { open: false, items: [], index: 0 };
const listeners = new Set<() => void>();

function emit(): void {
  state = { ...state };
  for (const l of listeners) l();
}

export function openPlayer(items: PlayerItem[], index: number): void {
  if (items.length === 0) return;
  state = { open: true, items, index: Math.min(Math.max(index, 0), items.length - 1) };
  emit();
}

export function closePlayer(): void {
  state = { ...state, open: false };
  emit();
}

export function setPlayerIndex(index: number): void {
  if (index < 0 || index >= state.items.length) return;
  state = { ...state, index };
  emit();
}

export function playerNext(): boolean {
  if (state.index >= state.items.length - 1) return false;
  setPlayerIndex(state.index + 1);
  return true;
}

export function playerPrev(): boolean {
  if (state.index <= 0) return false;
  setPlayerIndex(state.index - 1);
  return true;
}

export function subscribePlayer(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function getPlayerSnapshot(): PlayerState {
  return state;
}

/** 组件侧订阅 hook。 */
export function usePlayer(): PlayerState {
  return useSyncExternalStore(subscribePlayer, getPlayerSnapshot, getPlayerSnapshot);
}

/** 供 PlayerDialog 使用的便捷动作集。 */
export function usePlayerActions() {
  const next = useCallback(() => playerNext(), []);
  const prev = useCallback(() => playerPrev(), []);
  const close = useCallback(() => closePlayer(), []);
  const jump = useCallback((index: number) => setPlayerIndex(index), []);
  return { next, prev, close, jump };
}
