ALTER TABLE usage_upstream_attempts
    ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0;

UPDATE usage_upstream_attempts AS attempt
SET cache_creation_tokens = usage_log.cache_creation_tokens
FROM usage_logs AS usage_log
WHERE attempt.usage_log_id = usage_log.id
  AND attempt.cache_creation_tokens = 0
  AND usage_log.cache_creation_tokens > 0;

ALTER TABLE usage_upstream_attempts
    DROP CONSTRAINT IF EXISTS usage_upstream_attempts_usage_check;

ALTER TABLE usage_upstream_attempts
    ADD CONSTRAINT usage_upstream_attempts_usage_check CHECK (
        input_tokens >= 0 AND output_tokens >= 0 AND cache_read_tokens >= 0
        AND cache_creation_tokens >= 0
        AND cache_creation_5m_tokens >= 0 AND cache_creation_1h_tokens >= 0
        AND request_count >= 0 AND image_count >= 0 AND video_seconds >= 0
    );

COMMENT ON COLUMN usage_upstream_attempts.cache_creation_tokens IS
    'Generic cache-write tokens reported without a cache TTL breakdown.';
