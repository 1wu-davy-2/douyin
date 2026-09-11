package api

// Subscription monitor endpoints — 由后续阶段实现。
//
// Planned routes (docs/api.md "订阅监控 Subscriptions"):
//
//	GET    /api/subscriptions        -> [{id, target_type, ...}]
//	POST   /api/subscriptions        -> create (creator | collection)
//	PATCH  /api/subscriptions/{id}   -> partial update
//	DELETE /api/subscriptions/{id}
//
// Seams available now:
//   - s.deps.DB via db.WithTx for the subscriptions table
//   - schedule/tick logic can reuse s.deps.Resolver.Resolve(ctx) + scanner
