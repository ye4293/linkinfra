-- PostgreSQL：供部署前审核，在日志库执行。本文件不会自动执行。
-- 大表 ALTER 会短暂取锁，建议先在维护窗口执行，再发布应用。
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE logs ADD COLUMN IF NOT EXISTS ranking_model_name varchar(180);
ALTER TABLE logs ADD COLUMN IF NOT EXISTS ranking_tokens bigint;

CREATE TABLE IF NOT EXISTS ranking_daily (
    day_start bigint NOT NULL,
    model_name varchar(180) NOT NULL,
    provider varchar(200) NOT NULL,
    tokens bigint NOT NULL DEFAULT 0,
    requests bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (day_start, model_name, provider)
);
CREATE TABLE IF NOT EXISTS ranking_states (
    id bigint PRIMARY KEY,
    coverage_start bigint NOT NULL DEFAULT 0,
    next_day bigint NOT NULL DEFAULT 0,
    lease_owner varchar(64) NOT NULL DEFAULT '',
    lease_until bigint NOT NULL DEFAULT 0,
    generation bigint NOT NULL DEFAULT 0,
    heartbeat bigint NOT NULL DEFAULT 0,
    snapshot_dirty boolean NOT NULL DEFAULT false,
    log_purged_before bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ranking_jobs (
    day_start bigint PRIMARY KEY,
    completed_at bigint NOT NULL DEFAULT 0,
    finalized boolean NOT NULL DEFAULT false,
    excluded_requests bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ranking_snapshots (
    id bigint PRIMARY KEY,
    payload text NOT NULL,
    version varchar(64) NOT NULL
);
COMMIT;
