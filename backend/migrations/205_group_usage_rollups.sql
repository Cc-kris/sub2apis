-- Group usage daily rollups.  The migration is intentionally idempotent.
CREATE TABLE IF NOT EXISTS usage_group_daily_rollups (
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    timezone TEXT NOT NULL,
    bucket_date DATE NOT NULL,
    request_count BIGINT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens BIGINT NOT NULL DEFAULT 0,
    total_cost NUMERIC(20,10) NOT NULL DEFAULT 0,
    actual_cost NUMERIC(20,10) NOT NULL DEFAULT 0,
    source_max_created_at TIMESTAMPTZ,
    recalculated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revision BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (group_id, timezone, bucket_date)
);
CREATE INDEX IF NOT EXISTS idx_usage_group_daily_rollups_day
    ON usage_group_daily_rollups (timezone, bucket_date, group_id);

CREATE TABLE IF NOT EXISTS usage_group_model_daily_rollups (
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    timezone TEXT NOT NULL,
    bucket_date DATE NOT NULL,
    request_count BIGINT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens BIGINT NOT NULL DEFAULT 0,
    total_cost NUMERIC(20,10) NOT NULL DEFAULT 0,
    actual_cost NUMERIC(20,10) NOT NULL DEFAULT 0,
    source_max_created_at TIMESTAMPTZ,
    recalculated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revision BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (group_id, model, timezone, bucket_date)
);
CREATE INDEX IF NOT EXISTS idx_usage_group_model_daily_rollups_day
    ON usage_group_model_daily_rollups (timezone, bucket_date, group_id, model);

CREATE TABLE IF NOT EXISTS usage_group_rollup_invalidations (
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    timezone TEXT NOT NULL,
    bucket_date DATE NOT NULL,
    invalidated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reason TEXT NOT NULL DEFAULT 'usage_log_changed',
    revision BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (group_id, timezone, bucket_date)
);

CREATE TABLE IF NOT EXISTS usage_group_rollup_leases (
    timezone TEXT PRIMARY KEY,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    revision BIGINT NOT NULL DEFAULT 0,
    closed_before DATE,
    last_error TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS usage_group_rollup_timezones (
    timezone TEXT PRIMARY KEY,
    bootstrap_state TEXT NOT NULL DEFAULT 'pending',
    bootstrap_from TIMESTAMPTZ,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ready_at TIMESTAMPTZ,
    CONSTRAINT usage_group_rollup_timezones_state_chk
      CHECK (bootstrap_state IN ('pending','rebuilding','ready','degraded'))
);

CREATE OR REPLACE FUNCTION usage_group_rollup_invalidate() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  tz RECORD;
  gid BIGINT;
  ts TIMESTAMPTZ;
BEGIN
  FOR tz IN SELECT timezone FROM usage_group_rollup_timezones LOOP
    IF TG_OP <> 'DELETE' THEN gid := NEW.group_id; ts := NEW.created_at;
    ELSE gid := OLD.group_id; ts := OLD.created_at;
    END IF;
    IF gid IS NOT NULL THEN
      INSERT INTO usage_group_rollup_invalidations(group_id, timezone, bucket_date)
      VALUES (gid, tz.timezone, (ts AT TIME ZONE tz.timezone)::date)
      ON CONFLICT (group_id, timezone, bucket_date) DO UPDATE
        SET invalidated_at = NOW(), revision = usage_group_rollup_invalidations.revision + 1;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.group_id IS NOT NULL AND OLD.group_id IS DISTINCT FROM NEW.group_id THEN
      INSERT INTO usage_group_rollup_invalidations(group_id, timezone, bucket_date)
      VALUES (OLD.group_id, tz.timezone, (OLD.created_at AT TIME ZONE tz.timezone)::date)
      ON CONFLICT (group_id, timezone, bucket_date) DO UPDATE
        SET invalidated_at = NOW(), revision = usage_group_rollup_invalidations.revision + 1;
    END IF;
  END LOOP;
  IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS trg_usage_group_rollup_invalidate ON usage_logs;
CREATE TRIGGER trg_usage_group_rollup_invalidate
AFTER INSERT OR UPDATE OR DELETE ON usage_logs
FOR EACH ROW EXECUTE FUNCTION usage_group_rollup_invalidate();

-- Existing partitions need their own trigger when the parent trigger was not
-- cloned by the PostgreSQL version used by the deployment.
DO $$ DECLARE p RECORD; BEGIN
  FOR p IN SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
           JOIN pg_class parent ON parent.oid=i.inhparent WHERE parent.relname='usage_logs' LOOP
    EXECUTE format('DROP TRIGGER IF EXISTS trg_usage_group_rollup_invalidate ON %I', p.relname);
    EXECUTE format('CREATE TRIGGER trg_usage_group_rollup_invalidate AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION usage_group_rollup_invalidate()', p.relname);
  END LOOP;
END $$;
