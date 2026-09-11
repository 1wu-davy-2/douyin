import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}

export function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value));
}

/** 在 Set 中切换某个 id 的选中态(返回新 Set)。 */
export function toggleInSet(set: ReadonlySet<number>, id: number): Set<number> {
  const next = new Set(set);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  return next;
}

/** 把一批 id 并入 Set(返回新 Set)。 */
export function unionIntoSet(set: ReadonlySet<number>, ids: Iterable<number>): Set<number> {
  const next = new Set(set);
  for (const id of ids) next.add(id);
  return next;
}

/** 从 Set 中移除一批 id(返回新 Set)。 */
export function removeFromSet(set: ReadonlySet<number>, ids: Iterable<number>): Set<number> {
  const next = new Set(set);
  for (const id of ids) next.delete(id);
  return next;
}
