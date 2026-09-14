-- Migration v6 — spark (续火花) integration tables.
--
-- Upstream douyin-sparkflow (PolyForm Noncommercial 1.0.0, noncommercial use
-- only) kept accounts/friends/send state as JSON files. In the merged
-- architecture SQLite is the single source of truth and the Go backend is the
-- only writer; the Python engine (spark-engine/) stays stateless and returns
-- everything over HTTP.
--
-- confirm_state: strong (DOM-confirmed) | failed (engine failure queue).
--   The upstream "weak" outcome is never persisted (failure queue semantics),
--   the column keeps the value space for future engine revisions.
-- category: target_not_found | friend_not_found | send_failed | login_required
--   | account_error | "" (empty on success).
-- run_mode: scheduled | manual | manual_failed | manual_unsent.
-- failure_count_today: JSON map {"2006-01-02": n} (Asia/Shanghai day).

CREATE TABLE IF NOT EXISTS spark_accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    unique_id TEXT NOT NULL UNIQUE,          -- 抖音号（登录导出）
    username TEXT NOT NULL DEFAULT '',        -- 冗余显示名（上游字段）
    nickname TEXT NOT NULL DEFAULT '',
    profile_name TEXT NOT NULL,               -- 浏览器 profile 目录名（uid-{unique_id}）
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'idle',      -- idle|sending|login_required|cooldown|error
    last_error TEXT NOT NULL DEFAULT '',
    last_friends_refresh_at TEXT NOT NULL DEFAULT '',   -- RFC3339
    last_send_at TEXT NOT NULL DEFAULT '',              -- RFC3339
    cooldown_until TEXT NOT NULL DEFAULT '',            -- RFC3339，空=无冷却
    failure_count_today TEXT NOT NULL DEFAULT '',       -- JSON: {"2026-09-14": 3}
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS spark_friends (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES spark_accounts(id) ON DELETE CASCADE,
    friend_key TEXT NOT NULL,                 -- 归一化名字（上游 _normalize_target_name）
    display_name TEXT NOT NULL,
    selected INTEGER NOT NULL DEFAULT 0,      -- 是否发送目标；新好友默认 0（防误发）
    UNIQUE(account_id, friend_key)
);
CREATE INDEX IF NOT EXISTS idx_spark_friends_account ON spark_friends(account_id);

CREATE TABLE IF NOT EXISTS spark_send_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES spark_accounts(id) ON DELETE CASCADE,
    friend_key TEXT NOT NULL,
    message TEXT NOT NULL,                    -- 实际发送的文本（引擎返回）
    confirm_state TEXT NOT NULL,              -- strong|failed
    category TEXT NOT NULL DEFAULT '',        -- 失败分类，成功为空
    detail TEXT NOT NULL DEFAULT '',
    run_mode TEXT NOT NULL,                   -- manual|manual_failed|manual_unsent|scheduled
    sent_at TEXT NOT NULL,                    -- RFC3339 UTC
    local_date TEXT NOT NULL                  -- Asia/Shanghai 的 YYYY-MM-DD（"今日"判定用）
);
CREATE INDEX IF NOT EXISTS idx_spark_send_records_lookup
    ON spark_send_records(account_id, local_date);
CREATE INDEX IF NOT EXISTS idx_spark_send_records_time
    ON spark_send_records(sent_at DESC);

CREATE TABLE IF NOT EXISTS spark_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL                       -- JSON 字符串
);

-- Default send config (docs/HUOHUA_EXECUTION_PLAN.md §5.4). Rate-limit floors
-- (messageIntervalSecondsMin 25) must not be lowered — see §10.3.
INSERT OR IGNORE INTO spark_settings(key, value) VALUES('send_config', '{"messageTemplate":"✨今日火花+1","messageVariants":["🤩今日火花+1","今天来补个火花","给你续一下今天的火花","路过给你加个小火花"],"hitokotoTypes":["文学","影视","诗词","哲学"],"sendWindow":{"enabled":true,"startHour":10,"endHour":18,"intervalMinutes":20},"sendStrategy":{"shuffleTargets":true,"accountStartDelaySecondsMin":15,"accountStartDelaySecondsMax":60,"messageIntervalSecondsMin":25,"messageIntervalSecondsMax":70},"friendScan":{"maxScanSeconds":300,"idleScanSeconds":120,"scrollStepPx":400,"scrollDelaySeconds":0.8},"accountFailurePause":{"attempts":3,"cooldownMinutes":60}}');
