//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPopulateOpenAIUsageFromResponseJSONParsesGenericCacheWrite(t *testing.T) {
	usage := &OpenAIUsage{}
	populateOpenAIUsageFromResponseJSON([]byte(`{"response":{"usage":{"input_tokens":9,"output_tokens":2,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":4}}}}`), usage)

	require.Equal(t, 9, usage.InputTokens)
	require.Equal(t, 2, usage.OutputTokens)
	require.Equal(t, 3, usage.CacheReadInputTokens)
	require.Equal(t, 4, usage.CacheCreationInputTokens)
}
