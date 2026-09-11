package api

// Creators/works endpoints — 由后续阶段实现（扫描器 agent）。
//
// Planned routes (docs/api.md "博主与作品 Creators / Works"):
//
//	POST   /api/creators              -> 202 {creator_id, scan_id}
//	GET    /api/creators              -> [{id, sec_uid, nickname, ...}]
//	GET    /api/creators/{id}         -> detail + last_scan
//	DELETE /api/creators/{id}
//	POST   /api/creators/{id}/rescan  -> 202 {scan_id}
//	GET    /api/creators/{id}/collections
//	GET    /api/creators/{id}/works   -> 分页作品
//	GET    /api/works/{id}            -> 详情(mix_info/asset/last_job)
//	POST   /api/works/batch-ids       -> {ids:[int]}
//	GET    /api/works/{id}/qualities  -> 实时清晰度（经 SidecarProvider）
//
// Seams available now:
//   - s.deps.Resolver.Resolve(ctx) -> provider.Provider (Profile/PostsPage/WorkDetail)
//   - s.deps.Bus.Publish(events.Event{...}) with events.ScanProgress / events.ScanDone
//   - s.deps.DB via db.WithTx for creators/works/collections/scan_runs tables
