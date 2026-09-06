package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestAccountFromServiceShallow_RedactsSensitiveCredentials(t *testing.T) {
	src := &service.Account{
		ID:       42,
		Name:     "demo",
		Platform: "anthropic",
		Type:     "oauth",
		Credentials: map[string]any{
			"access_token":  "at-secret",
			"refresh_token": "rt-secret",
			"id_token":      "id-secret",
			"api_key":       "sk-secret",
			"base_url":      "https://api.example.com",
			"model_mapping": map[string]any{"foo": "bar"},
		},
	}

	got := AccountFromServiceShallow(src)
	require.NotNil(t, got)

	// 敏感键不在 Credentials 里
	require.NotContains(t, got.Credentials, "access_token")
	require.NotContains(t, got.Credentials, "refresh_token")
	require.NotContains(t, got.Credentials, "id_token")
	require.NotContains(t, got.Credentials, "api_key")
	// 非敏感键保留
	require.Equal(t, "https://api.example.com", got.Credentials["base_url"])
	require.Equal(t, map[string]any{"foo": "bar"}, got.Credentials["model_mapping"])

	// 状态 map 标记敏感键存在
	require.True(t, got.CredentialsStatus["has_access_token"])
	require.True(t, got.CredentialsStatus["has_refresh_token"])
	require.True(t, got.CredentialsStatus["has_id_token"])
	require.True(t, got.CredentialsStatus["has_api_key"])

	// JSON 序列化校验：响应体里不会出现敏感子串
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "rt-secret")
	require.NotContains(t, string(raw), "at-secret")
	require.NotContains(t, string(raw), "sk-secret")
	require.NotContains(t, string(raw), "id-secret")
	// 状态标识应序列化进 JSON
	require.Contains(t, string(raw), "credentials_status")
	require.Contains(t, string(raw), "has_refresh_token")

	// 原始 service.Account 不应被改动
	require.Equal(t, "rt-secret", src.Credentials["refresh_token"])
}

func TestAccountFromServiceShallow_NilCredentialsOmitsStatus(t *testing.T) {
	src := &service.Account{ID: 1, Name: "n", Platform: "anthropic", Type: "oauth"}
	got := AccountFromServiceShallow(src)
	require.NotNil(t, got)
	require.Nil(t, got.Credentials)
	require.Nil(t, got.CredentialsStatus)
}

func TestAccountFromServiceShallow_DropsDeprecatedUpstreamWarningExtra(t *testing.T) {
	src := &service.Account{
		ID:       43,
		Name:     "legacy-extra",
		Platform: "anthropic",
		Type:     "api_key",
		Extra: map[string]any{
			"upstream_prepaid_amount": 25.5,
			"upstream_warning_amount": 5.0,
			"upstream_notify_enabled": true,
		},
	}

	got := AccountFromServiceShallow(src)
	require.NotNil(t, got)
	require.Equal(t, 25.5, got.Extra["upstream_prepaid_amount"])
	require.NotContains(t, got.Extra, "upstream_warning_amount")
	require.NotContains(t, got.Extra, "upstream_notify_enabled")

	// 原始 service.Account 不应被响应层清理逻辑改动。
	require.Contains(t, src.Extra, "upstream_warning_amount")
	require.Contains(t, src.Extra, "upstream_notify_enabled")
}

func TestAccountFromServiceShallow_RedactsSensitiveExtra(t *testing.T) {
	src := &service.Account{
		ID:       44,
		Name:     "extra-secret",
		Platform: "openai",
		Type:     "apikey",
		Extra: map[string]any{
			"private_token":         "sentinel-private-token",
			"client_secret":         "sentinel-client-secret",
			"session_token_present": true,
			"access_token_sha256":   "fingerprint",
			"quota_daily_reset_at":  "2026-08-31T00:00:00Z",
		},
	}

	got := AccountFromServiceShallow(src)
	require.NotNil(t, got)
	require.NotContains(t, got.Extra, "private_token")
	require.NotContains(t, got.Extra, "client_secret")
	require.Equal(t, true, got.Extra["session_token_present"])
	require.Equal(t, "fingerprint", got.Extra["access_token_sha256"])
	require.Equal(t, "2026-08-31T00:00:00Z", got.Extra["quota_daily_reset_at"])

	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "sentinel-private-token")
	require.NotContains(t, string(raw), "sentinel-client-secret")
	require.Contains(t, string(raw), "session_token_present")
}
