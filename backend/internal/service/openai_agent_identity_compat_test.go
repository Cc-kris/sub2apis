package service

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func openAIAgentIdentityCompatAccount(mode any) *Account {
	return &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"user_agent":    "codex_cli_rs/0.125.0",
			"access_token":  "must-not-appear",
			"refresh_token": "must-not-appear-either",
		},
		Extra: map[string]any{
			CodexIdentityModeExtraKey: mode,
			"openai_device_id":        "device-account-42",
			"openai_session_id":       "session-account-42",
		},
	}
}

func TestGetCodexIdentityModeDefaultsToDisabled(t *testing.T) {
	tests := []struct {
		name string
		acct *Account
		want OpenAICodexIdentityMode
	}{
		{name: "nil account", acct: nil, want: OpenAICodexIdentityModeDisabled},
		{name: "missing extra", acct: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, want: OpenAICodexIdentityModeDisabled},
		{name: "invalid string", acct: openAIAgentIdentityCompatAccount("unknown"), want: OpenAICodexIdentityModeDisabled},
		{name: "non string", acct: openAIAgentIdentityCompatAccount(true), want: OpenAICodexIdentityModeDisabled},
		{name: "api key ignores setting", acct: func() *Account {
			account := openAIAgentIdentityCompatAccount("full")
			account.Type = AccountTypeAPIKey
			return account
		}(), want: OpenAICodexIdentityModeDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.acct == nil {
				require.Equal(t, tt.want, OpenAICodexIdentityModeDisabled)
				return
			}
			require.Equal(t, tt.want, tt.acct.GetCodexIdentityMode())
		})
	}
}

func TestResolveOpenAIAgentIdentityCompatModes(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		wantHeaders  map[string]string
		wantMetadata map[string]any
	}{
		{
			name:         "disabled",
			mode:         "disabled",
			wantHeaders:  map[string]string{},
			wantMetadata: map[string]any{},
		},
		{
			name: "device",
			mode: "device",
			wantHeaders: map[string]string{
				"x-codex-installation-id": "device-account-42",
			},
			wantMetadata: map[string]any{
				"x-codex-installation-id": "device-account-42",
			},
		},
		{
			name: "session",
			mode: "session",
			wantHeaders: map[string]string{
				"x-codex-installation-id": "device-account-42",
				"session_id":              "session-account-42",
			},
			wantMetadata: map[string]any{
				"x-codex-installation-id": "device-account-42",
			},
		},
		{
			name: "full",
			mode: "full",
			wantHeaders: map[string]string{
				"x-codex-installation-id": "device-account-42",
				"session_id":              "session-account-42",
				"user-agent":              "codex_cli_rs/0.125.0",
			},
			wantMetadata: map[string]any{
				"x-codex-installation-id": "device-account-42",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := ResolveOpenAIAgentIdentityCompat(openAIAgentIdentityCompatAccount(tt.mode))
			require.Equal(t, OpenAICodexIdentityMode(tt.mode), identity.Mode)
			require.Equal(t, tt.wantHeaders, flattenIdentityHeaders(identity.Headers))
			require.Equal(t, tt.wantMetadata, identity.Metadata)
			require.NotContains(t, identity.Headers, "Authorization")
			require.NotContains(t, identity.Headers, "access_token")
		})
	}
}

func TestResolveOpenAIAgentIdentityCompatLeavesUnconfiguredAccountUntouched(t *testing.T) {
	account := openAIAgentIdentityCompatAccount("full")
	delete(account.Extra, CodexIdentityModeExtraKey)

	identity := ResolveOpenAIAgentIdentityCompat(account)
	require.Equal(t, OpenAICodexIdentityModeDisabled, identity.Mode)
	require.Empty(t, identity.Headers)
	require.Empty(t, identity.Metadata)
}

func TestResolveOpenAIAgentIdentityCompatReusesOnlyConfiguredValues(t *testing.T) {
	account := openAIAgentIdentityCompatAccount("full")
	account.Extra["openai_device_id"] = ""
	account.Extra["openai_session_id"] = ""
	account.Credentials["user_agent"] = ""

	identity := ResolveOpenAIAgentIdentity(account)
	require.Equal(t, OpenAICodexIdentityModeFull, identity.Mode)
	require.Empty(t, identity.Headers)
	require.Empty(t, identity.Metadata)

	account.Extra["openai_device_id"] = "device-only"
	identity = ResolveOpenAIAgentIdentity(account)
	require.Equal(t, "device-only", identity.Headers.Get("x-codex-installation-id"))
	require.Empty(t, identity.Headers.Get("session_id"))
	require.Empty(t, identity.Headers.Get("User-Agent"))
}

func TestOpenAIAgentIdentityCompatApplyToPreservesUnrelatedValues(t *testing.T) {
	identity := ResolveOpenAIAgentIdentityCompat(openAIAgentIdentityCompatAccount("session"))
	headers := http.Header{
		"X-Existing":              []string{"keep"},
		"X-Codex-Installation-Id": []string{"stale-client-value"},
	}
	metadata := map[string]any{
		"trace_id":                "trace-42",
		"x-codex-installation-id": "stale-client-value",
	}

	identity.ApplyTo(headers, metadata)
	require.Equal(t, "keep", headers.Get("X-Existing"))
	require.Equal(t, "device-account-42", headers.Get("x-codex-installation-id"))
	require.Equal(t, "session-account-42", headers.Get("session_id"))
	require.Equal(t, "trace-42", metadata["trace_id"])
	require.Equal(t, "device-account-42", metadata["x-codex-installation-id"])
}

func flattenIdentityHeaders(headers http.Header) map[string]string {
	result := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) > 0 {
			result[strings.ToLower(key)] = values[0]
		}
	}
	return result
}
