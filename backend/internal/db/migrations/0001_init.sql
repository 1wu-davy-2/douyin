-- Migration v1 — full schema from docs/api.md ("数据库表" section).
-- Column names are exactly the contract names; works is UNIQUE(creator_id, item_id)
-- (per-creator uniqueness, NOT globally unique).

CREATE TABLE IF NOT EXISTS creators (
    id                  INTEGER PRIMARY KEY,
    sec_uid             TEXT NOT NULL UNIQUE,
    nickname            TEXT NOT NULL DEFAULT '',
    avatar_url          TEXT NOT NULL DEFAULT '',
    profile_url         TEXT NOT NULL DEFAULT '',
    reported_work_count INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,
    last_scan_at        TEXT
);

CREATE TABLE IF NOT EXISTS collections (
    id         INTEGER PRIMARY KEY,
    creator_id INTEGER NOT NULL REFERENCES creators(id),
    mix_id     TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    cover_url  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE(creator_id, mix_id)
);

CREATE TABLE IF NOT EXISTS works (
    id            INTEGER PRIMARY KEY,
    creator_id    INTEGER NOT NULL REFERENCES creators(id),
    collection_id INTEGER REFERENCES collections(id),
    item_id       TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    cover_url     TEXT NOT NULL DEFAULT '',
    duration      INTEGER NOT NULL DEFAULT 0,
    published_at  TEXT,
    deleted_at    TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE(creator_id, item_id)
);

-- Download state is NOT stored on works (derived from the latest download_jobs
-- row); assets live in their own table.
CREATE TABLE IF NOT EXISTS assets (
    id         INTEGER PRIMARY KEY,
    work_id    INTEGER NOT NULL REFERENCES works(id),
    kind       TEXT NOT NULL,
    path       TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    quality    TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(work_id, kind, quality)
);

CREATE TABLE IF NOT EXISTS download_jobs (
    id               INTEGER PRIMARY KEY,
    work_id          INTEGER NOT NULL REFERENCES works(id),
    creator_id       INTEGER NOT NULL,
    quality          TEXT NOT NULL,
    status           TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    total_bytes      INTEGER NOT NULL DEFAULT 0,
    downloaded_bytes INTEGER NOT NULL DEFAULT 0,
    error            TEXT,
    queued_at        TEXT NOT NULL,
    started_at       TEXT,
    finished_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_download_jobs_status ON download_jobs(status, id);
CREATE INDEX IF NOT EXISTS idx_download_jobs_work ON download_jobs(work_id);

CREATE TABLE IF NOT EXISTS subscriptions (
    id               INTEGER PRIMARY KEY,
    target_type      TEXT NOT NULL,
    creator_id       INTEGER NOT NULL,
    collection_id    INTEGER,
    interval_minutes INTEGER NOT NULL,
    auto_download    INTEGER NOT NULL DEFAULT 0,
    quality          TEXT NOT NULL DEFAULT '1080p',
    enabled          INTEGER NOT NULL DEFAULT 1,
    last_run_at      TEXT,
    created_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS scan_runs (
    id             INTEGER PRIMARY KEY,
    creator_id     INTEGER NOT NULL,
    "trigger"      TEXT NOT NULL DEFAULT 'manual',
    full           INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL,
    pages          INTEGER NOT NULL DEFAULT 0,
    new_count      INTEGER NOT NULL DEFAULT 0,
    updated_count  INTEGER NOT NULL DEFAULT 0,
    empty_pages    INTEGER NOT NULL DEFAULT 0,
    completeness   INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    started_at     TEXT NOT NULL,
    finished_at    TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    username   TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_state (
    id                   INTEGER PRIMARY KEY CHECK(id = 1),
    risk_paused          INTEGER NOT NULL DEFAULT 0,
    paused_until         TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT NOT NULL
);
