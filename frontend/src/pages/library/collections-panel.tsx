/**
 * 合集 Tab:封面卡片网格;点击卡片切换到作品 Tab 并按该合集过滤。
 */
import type { Collection } from "../../api/types";
import { EmptyState } from "../../components/empty-state";
import { Skeleton } from "../../components/ui/skeleton";
import { FolderOpen } from "lucide-react";

interface CollectionsPanelProps {
  collections: Collection[] | undefined;
  isPending: boolean;
  onPick: (collectionId: number) => void;
}

export function CollectionsPanel({ collections, isPending, onPick }: CollectionsPanelProps) {
  if (isPending) {
    return (
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4">
        {Array.from({ length: 8 }, (_, i) => (
          <div key={i} className="overflow-hidden rounded-xl border border-border">
            <Skeleton className="aspect-video w-full rounded-none" />
            <div className="space-y-1.5 p-3">
              <Skeleton className="h-4 w-2/3" />
              <Skeleton className="h-3 w-1/2" />
            </div>
          </div>
        ))}
      </div>
    );
  }

  if (!collections || collections.length === 0) {
    return (
      <EmptyState
        icon={FolderOpen}
        title="该博主暂无合集"
        description="扫描完成后,合集会出现在这里"
      />
    );
  }

  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4">
      {collections.map((col) => (
        <button
          key={col.id}
          type="button"
          onClick={() => onPick(col.id)}
          title={`查看合集「${col.name}」的作品`}
          className="group overflow-hidden rounded-xl border border-border bg-card text-left transition-colors hover:border-primary/60"
        >
          <img src={col.cover_url} alt="" className="aspect-video w-full object-cover" loading="lazy" />
          <div className="space-y-0.5 p-3">
            <p className="truncate text-sm font-medium" title={col.name}>{col.name}</p>
            <p className="text-xs text-muted-foreground">
              {col.works_count} 作品 · 已下载 {col.downloaded_count}
            </p>
          </div>
        </button>
      ))}
    </div>
  );
}
