/**
 * 页码式分页器:页码窗口(首尾 + 当前±1,间隔省略号)+ 跳页 + 每页条数(契约:20|50|100)。
 */
import { useState, type FormEvent } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../components/ui/select";
import { cn } from "../../lib/utils";

function buildPageItems(current: number, totalPages: number): (number | "gap")[] {
  const pages = new Set<number>([1, totalPages, current]);
  if (current - 1 >= 1) pages.add(current - 1);
  if (current + 1 <= totalPages) pages.add(current + 1);
  const sorted = [...pages].filter((p) => p >= 1 && p <= totalPages).sort((a, b) => a - b);
  const items: (number | "gap")[] = [];
  let prev = 0;
  for (const p of sorted) {
    if (p - prev > 1) items.push("gap");
    items.push(p);
    prev = p;
  }
  return items;
}

const PAGE_SIZES = [20, 50, 100] as const;

interface PagerProps {
  page: number;
  pageSize: number;
  total: number;
  disabled?: boolean;
  onPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
}

export function Pager({ page, pageSize, total, disabled = false, onPageChange, onPageSizeChange }: PagerProps) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const [jump, setJump] = useState("");

  const submitJump = (e: FormEvent) => {
    e.preventDefault();
    const n = Number(jump);
    if (!Number.isInteger(n) || jump.trim() === "") return;
    onPageChange(Math.min(totalPages, Math.max(1, n)));
    setJump("");
  };

  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
      <p className="text-xs text-muted-foreground">
        共 {total} 条 · 第 {page} / {totalPages} 页
      </p>
      <div className="flex flex-wrap items-center gap-1.5">
        <Button
          variant="outline"
          size="icon-sm"
          aria-label="上一页"
          disabled={page <= 1 || disabled}
          onClick={() => onPageChange(page - 1)}
        >
          <ChevronLeft className="size-4" />
        </Button>
        {buildPageItems(page, totalPages).map((it, i) =>
          it === "gap" ? (
            <span key={`gap-${i}`} className="px-1 text-xs text-muted-foreground">
              …
            </span>
          ) : (
            <Button
              key={it}
              variant={it === page ? "default" : "outline"}
              size="icon-sm"
              disabled={disabled}
              className={cn("min-w-8 tabular-nums")}
              onClick={() => onPageChange(it)}
            >
              {it}
            </Button>
          ),
        )}
        <Button
          variant="outline"
          size="icon-sm"
          aria-label="下一页"
          disabled={page >= totalPages || disabled}
          onClick={() => onPageChange(page + 1)}
        >
          <ChevronRight className="size-4" />
        </Button>

        <form onSubmit={submitJump} className="ml-1 flex items-center gap-1">
          <Input
            aria-label="跳转到页"
            placeholder="页码"
            inputMode="numeric"
            className="h-8 w-16 text-xs"
            value={jump}
            onChange={(e) => setJump(e.target.value)}
          />
          <Button variant="outline" size="sm" type="submit" disabled={disabled}>
            跳页
          </Button>
        </form>

        <Select value={String(pageSize)} onValueChange={(v) => onPageSizeChange(Number(v))}>
          <SelectTrigger className="ml-1 h-8 w-[104px] text-xs" aria-label="每页条数">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZES.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n} 条/页
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  );
}
