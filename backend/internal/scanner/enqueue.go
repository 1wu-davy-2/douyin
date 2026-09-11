package scanner

// Subscription auto-download matching. After every finished scan the scanner
// looks up enabled auto_download subscriptions for the creator and hands the
// newly found works to the Enqueuer:
//
//   - target_type=creator    -> all new works of the scan
//   - target_type=collection -> only new works belonging to that collection
//     (collections update naturally as part of the creator scan)
//
// While enqueuer is nil (stage 4: the downloader does not exist yet) matching
// is skipped with a log line; stage 5 injects the real queue via SetEnqueuer.

import (
	"context"
	"database/sql"
	"log"
	"strings"
)

// enqueueNewWorks routes newWorkIDs to every matching subscription. Best
// effort: failures are logged, never propagated to the scan result.
func (s *Scanner) enqueueNewWorks(ctx context.Context, creatorID int64, newWorkIDs []int64) {
	if len(newWorkIDs) == 0 {
		return
	}
	if s.enqueuer == nil {
		log.Printf("[scanner] creator %d: %d new work(s), enqueuer not wired yet - auto download skipped",
			creatorID, len(newWorkIDs))
		return
	}

	type sub struct {
		id           int64
		targetType   string
		collectionID sql.NullInt64
		quality      string
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_type, collection_id, quality
		FROM subscriptions
		WHERE creator_id = ? AND enabled = 1 AND auto_download = 1`, creatorID)
	if err != nil {
		log.Printf("[scanner] creator %d: load auto-download subscriptions: %v", creatorID, err)
		return
	}
	var subs []sub
	for rows.Next() {
		var r sub
		if err := rows.Scan(&r.id, &r.targetType, &r.collectionID, &r.quality); err == nil {
			subs = append(subs, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[scanner] creator %d: scan auto-download subscriptions: %v", creatorID, err)
		return
	}
	if len(subs) == 0 {
		return
	}

	// Collection membership of the new works (only queried when needed).
	var collByWork map[int64]sql.NullInt64
	needCollections := false
	for _, r := range subs {
		if r.targetType == "collection" {
			needCollections = true
			break
		}
	}
	if needCollections {
		collByWork = make(map[int64]sql.NullInt64, len(newWorkIDs))
		q := `SELECT id, collection_id FROM works WHERE id IN (` + placeholders(len(newWorkIDs)) + `)`
		args := make([]any, 0, len(newWorkIDs))
		for _, id := range newWorkIDs {
			args = append(args, id)
		}
		r2, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			log.Printf("[scanner] creator %d: load new work collections: %v", creatorID, err)
			return
		}
		for r2.Next() {
			var id int64
			var cid sql.NullInt64
			if err := r2.Scan(&id, &cid); err == nil {
				collByWork[id] = cid
			}
		}
		r2.Close()
	}

	for _, r := range subs {
		ids := newWorkIDs
		if strings.EqualFold(r.targetType, "collection") {
			if !r.collectionID.Valid {
				log.Printf("[scanner] subscription %d: collection target without collection_id, skipped", r.id)
				continue
			}
			ids = nil // filter fresh for collection targets
			for _, id := range newWorkIDs {
				if c, ok := collByWork[id]; ok && c.Valid && c.Int64 == r.collectionID.Int64 {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			continue
		}
		quality := strings.TrimSpace(r.quality)
		if quality == "" {
			quality = "1080p"
		}
		if err := s.enqueuer.Enqueue(ctx, ids, quality); err != nil {
			log.Printf("[scanner] subscription %d: enqueue %d work(s): %v", r.id, len(ids), err)
			continue
		}
		log.Printf("[scanner] subscription %d: enqueued %d new work(s) at %s", r.id, len(ids), quality)
	}
}
