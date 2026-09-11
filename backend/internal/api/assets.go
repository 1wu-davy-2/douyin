package api

// Asset streaming endpoints — 由后续阶段实现。
//
// Planned routes (docs/api.md "资产与播放 Assets"):
//
//	GET /api/assets/{id}/content  -> http.ServeContent (Range 支持)
//	GET /api/works/{id}/assets    -> 资产列表（视频资产在前）
//
// Seams available now:
//   - s.deps.DB via db.WithTx for the assets table (work_id, kind, path,
//     size_bytes, quality)
//   - static.go's contentTypeByExt helper for file responses
