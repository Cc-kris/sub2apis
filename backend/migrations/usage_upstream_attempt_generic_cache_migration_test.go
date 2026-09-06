package migrations

import (
	"strings"
	"testing"
)

func TestUsageUpstreamAttemptGenericCacheMigration(t *testing.T) {
	content, err := FS.ReadFile("212_usage_upstream_attempt_generic_cache.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := strings.ToLower(string(content))
	for _, required := range []string{
		"add column if not exists cache_creation_tokens",
		"set cache_creation_tokens = usage_log.cache_creation_tokens",
		"drop constraint if exists usage_upstream_attempts_usage_check",
		"cache_creation_tokens >= 0",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration missing generic cache contract %q", required)
		}
	}
}
