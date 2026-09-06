//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestBuildBillableUsageAttemptFromResponseFormats(t *testing.T) {
	multiplier := decimal.RequireFromString("1.2500")
	tests := []struct {
		name       string
		body       string
		wantInput  int64
		wantOutput int64
		wantCache  int64
		wantWrite  int64
		wantWrite5 int64
	}{
		{name: "anthropic", body: `{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":40}}}`, wantInput: 100, wantOutput: 20, wantCache: 30, wantWrite5: 40},
		{name: "openai", body: `{"usage":{"prompt_tokens":200,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":70}}}`, wantInput: 200, wantOutput: 50, wantCache: 60, wantWrite: 70},
		{name: "gemini", body: `{"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":70,"cachedContentTokenCount":80}}`, wantInput: 300, wantOutput: 70, wantCache: 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt, ok := BuildBillableUsageAttemptFromResponse([]byte(tt.body), "req", 2, 9, nil, "model-a", nil, &multiplier, time.Now())
			require.True(t, ok)
			require.Equal(t, tt.wantInput, attempt.InputTokens)
			require.Equal(t, tt.wantOutput, attempt.OutputTokens)
			require.Equal(t, tt.wantCache, attempt.CacheReadTokens)
			require.Equal(t, tt.wantWrite, attempt.CacheCreationTokens)
			require.Equal(t, tt.wantWrite5, attempt.CacheCreation5mTokens)
			require.Equal(t, 2, attempt.AttemptNo)
			require.True(t, attempt.UpstreamCostMultiplier.Equal(multiplier))
		})
	}
}

func TestBuildBillableUsageAttemptFromResponseRejectsErrorWithoutUsage(t *testing.T) {
	_, ok := BuildBillableUsageAttemptFromResponse([]byte(`{"error":{"message":"rate limited"}}`), "", 1, 9, nil, "model-a", nil, financeDecimal("1"), time.Now())
	require.False(t, ok)
}
