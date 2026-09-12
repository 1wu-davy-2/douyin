-- Migration v3 — contract v1.3: configurable download roots.
--
-- creators.download_root holds the per-creator download root override
-- (NULL = follow the global settings.download_root, empty string is treated
-- like NULL). The companion Go step for this version absolutizes the
-- existing relative assets.path values (see migrate.go).

ALTER TABLE creators ADD COLUMN download_root TEXT NULL;
