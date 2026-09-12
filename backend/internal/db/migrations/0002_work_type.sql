-- Migration v2 — stage 9: gallery/image works (docs/api.md v1.1).
-- works.type distinguishes plain videos from image galleries (multi-image /
-- live-photo posts). Existing rows are videos; the sidecar classifies new
-- rows ("aweme.images" non-empty -> "image").

ALTER TABLE works ADD COLUMN type TEXT NOT NULL DEFAULT 'video';
