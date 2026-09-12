-- works.image_count: douyin-reported gallery size from the latest scan
-- (0 for video works). Keeps the badge truthful before download;
-- assets remain the source of truth for actually downloaded images.
ALTER TABLE works ADD COLUMN image_count INTEGER NOT NULL DEFAULT 0;
