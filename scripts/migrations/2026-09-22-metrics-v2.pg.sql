-- 先审核 LOG_SQL_DSN 目标库；仅由一个迁移任务运行。
-- 本文件不得放入总事务：CREATE INDEX CONCURRENTLY 必须在事务外。
SET lock_timeout = '2s';
ALTER TABLE logs ADD COLUMN IF NOT EXISTS metrics_version bigint DEFAULT 0;
ALTER TABLE logs ADD COLUMN IF NOT EXISTS metrics_source_key varchar(200);
ALTER TABLE logs ADD COLUMN IF NOT EXISTS metrics_model_name varchar(200);
ALTER TABLE logs ADD COLUMN IF NOT EXISTS metrics_attempts text;

-- 首期采用有界JSON文本，与Go/SQLite测试保持一致；不为payload建立GIN索引。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_metrics_v2_scan
ON logs (created_at, id) WHERE type IN (2,5) AND metrics_version = 2;

CREATE TABLE IF NOT EXISTS metrics_v2_state (
 id bigint PRIMARY KEY, coverage_start bigint, next_bucket bigint, finalized_before bigint,
 owner varchar(64), lease_until bigint, generation bigint, heartbeat bigint, paused boolean
);
CREATE TABLE IF NOT EXISTS metrics_v2_jobs (
 start bigint PRIMARY KEY, cursor_time bigint, cursor_id bigint, pass bigint,
 next_run bigint, scanning boolean, published boolean, finalized boolean, error text
);
CREATE INDEX IF NOT EXISTS idx_mv2_job_due ON metrics_v2_jobs(next_run);
CREATE TABLE IF NOT EXISTS metrics_v2_stage (
 id bigserial PRIMARY KEY, start bigint, model varchar(200), source varchar(200), channel bigint, payload text
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mv2_stage ON metrics_v2_stage(start,model,source,channel);
CREATE TABLE IF NOT EXISTS metrics_v2_buckets (
 id bigserial PRIMARY KEY, resolution bigint, start bigint, model varchar(200), source varchar(200), channel bigint, payload text
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mv2_bucket ON metrics_v2_buckets(resolution,start,model,source,channel);
CREATE INDEX IF NOT EXISTS idx_mv2_source ON metrics_v2_buckets(model,source,channel,start);
CREATE INDEX IF NOT EXISTS idx_mv2_retention ON metrics_v2_buckets(start);
CREATE TABLE IF NOT EXISTS metrics_v2_snapshots (
 id bigserial PRIMARY KEY, model varchar(200), source varchar(200), version bigint, as_of bigint,
 payload text, mini text, dirty boolean
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mv2_snapshot ON metrics_v2_snapshots(model,source);
CREATE INDEX IF NOT EXISTS idx_mv2_snapshot_dirty ON metrics_v2_snapshots(dirty);

-- 检查有效性和定义，不要把无效同名索引当成已完成。
SELECT c.relname,i.indisvalid,i.indisready,pg_get_indexdef(i.indexrelid)
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
WHERE c.relname='idx_logs_metrics_v2_scan';
RESET lock_timeout;
