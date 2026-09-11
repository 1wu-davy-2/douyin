package api

// Download queue endpoints — 由后续阶段实现（下载器 agent）。
//
// Planned routes (docs/api.md "下载任务 Downloads"):
//
//	POST   /api/downloads              -> 202 {created, skipped}
//	GET    /api/downloads              -> 游标分页 job 列表
//	GET    /api/downloads/summary      -> {queued, downloading, ...}
//	POST   /api/downloads/{id}/retry
//	POST   /api/downloads/{id}/cancel
//	DELETE /api/downloads/{id}
//	POST   /api/downloads/batch        -> {affected}
//	POST   /api/downloads/retry-failed -> {affected}
//	POST   /api/downloads/clear-completed -> {affected}
//	POST   /api/downloads/queue/pause
//	POST   /api/downloads/queue/resume
//
// Seams available now:
//   - s.deps.DB via db.WithTx for download_jobs rows (queue = SQLite state)
//   - s.deps.Bus.Publish with events.DownloadProgress / events.DownloadStatus
//     (progress-class events are dropped on full buffers; status-class block)
//   - provider.WorkDetail(ctx, itemID) -> CDN variant URLs for the chosen quality
