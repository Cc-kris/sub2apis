package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

func BuildBillableUsageAttemptFromResponse(
	responseBody []byte,
	requestID string,
	attemptNo int,
	accountID int64,
	channelID *int64,
	upstreamModel string,
	serviceTier *string,
	multiplier *decimal.Decimal,
	createdAt time.Time,
) (UsageUpstreamAttempt, bool) {
	var payload map[string]any
	if len(responseBody) == 0 || json.Unmarshal(responseBody, &payload) != nil {
		return UsageUpstreamAttempt{}, false
	}
	usage := financeUsageObject(payload)
	if usage == nil {
		return UsageUpstreamAttempt{}, false
	}
	inputTokens := usageInt64(usage, "input_tokens", "prompt_tokens", "promptTokenCount")
	outputTokens := usageInt64(usage, "output_tokens", "completion_tokens", "candidatesTokenCount")
	cacheReadTokens := usageInt64(usage, "cache_read_input_tokens", "cache_read_tokens", "cachedContentTokenCount")
	cacheCreationTokens := usageInt64(usage, "cache_creation_input_tokens", "cache_creation_tokens", "cache_write_tokens")
	cacheCreation5mTokens := usageInt64(usage, "cache_creation_5m_input_tokens", "cache_creation_5m_tokens")
	cacheCreation1hTokens := usageInt64(usage, "cache_creation_1h_input_tokens", "cache_creation_1h_tokens")
	if details := usageMap(usage, "cache_creation"); details != nil {
		cacheCreation5mTokens += usageInt64(details, "ephemeral_5m_input_tokens", "cache_creation_5m_tokens")
		cacheCreation1hTokens += usageInt64(details, "ephemeral_1h_input_tokens", "cache_creation_1h_tokens")
	}
	if details := usageMap(usage, "prompt_tokens_details", "input_tokens_details"); details != nil {
		cacheReadTokens += usageInt64(details, "cached_tokens")
		if cacheCreationTokens == 0 {
			cacheCreationTokens = usageInt64(details, "cache_write_tokens", "cache_creation_tokens")
		}
	}
	requestCount := usageInt64(usage, "request_count")
	imageCount := usageInt64(usage, "image_count")
	videoSeconds := usageInt64(usage, "video_seconds", "duration_seconds")
	billable := inputTokens > 0 || outputTokens > 0 || cacheReadTokens > 0 || cacheCreationTokens > 0 || cacheCreation5mTokens > 0 || cacheCreation1hTokens > 0 || requestCount > 0 || imageCount > 0 || videoSeconds > 0
	if !billable || accountID <= 0 || strings.TrimSpace(upstreamModel) == "" {
		return UsageUpstreamAttempt{}, false
	}
	if attemptNo <= 0 {
		attemptNo = 1
	}
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	return UsageUpstreamAttempt{
		RequestID:              strings.TrimSpace(requestID),
		AttemptNo:              attemptNo,
		AccountID:              accountID,
		ChannelID:              cloneInt64Pointer(channelID),
		UpstreamModel:          strings.TrimSpace(upstreamModel),
		ServiceTier:            cloneStringPointer(serviceTier),
		InputTokens:            inputTokens,
		OutputTokens:           outputTokens,
		CacheReadTokens:        cacheReadTokens,
		CacheCreationTokens:    cacheCreationTokens,
		CacheCreation5mTokens:  cacheCreation5mTokens,
		CacheCreation1hTokens:  cacheCreation1hTokens,
		RequestCount:           requestCount,
		ImageCount:             imageCount,
		VideoSeconds:           videoSeconds,
		UpstreamCostMultiplier: cloneDecimal(multiplier),
		Billable:               true,
		CompletedAt:            createdAt,
		CreatedAt:              createdAt,
	}, true
}

func financeUsageObject(payload map[string]any) map[string]any {
	if usage := usageMap(payload, "usage", "usageMetadata", "usage_metadata"); usage != nil {
		return usage
	}
	if response := usageMap(payload, "response", "result"); response != nil {
		return usageMap(response, "usage", "usageMetadata", "usage_metadata")
	}
	return nil
}

func usageMap(raw map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		if typed, ok := value.(map[string]any); ok {
			return typed
		}
	}
	return nil
}

func usageInt64(raw map[string]any, keys ...string) int64 {
	value, ok := financeAnyValue(raw, keys...)
	if !ok {
		return 0
	}
	parsed, ok := financeInt64Value(map[string]any{"value": value}, "value")
	if !ok || parsed < 0 {
		return 0
	}
	return parsed
}
