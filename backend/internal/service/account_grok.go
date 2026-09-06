package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

type OpenAIEndpointCapability string

const (
	OpenAIEndpointCapabilityChatCompletions     OpenAIEndpointCapability = "chat_completions"
	OpenAIEndpointCapabilityEmbeddings          OpenAIEndpointCapability = "embeddings"
	OpenAIEndpointCapabilityAlphaSearch         OpenAIEndpointCapability = "alpha_search"
	OpenAIEndpointCapabilityGrokMediaGeneration OpenAIEndpointCapability = "grok_media_generation"
	OpenAIEndpointCapabilityResponses           OpenAIEndpointCapability = "responses"
)

const openAIEndpointCapabilitiesCredentialKey = "openai_capabilities"

// GrokMediaEligibleExtraKey allows an operator to override media eligibility.
const GrokMediaEligibleExtraKey = "grok_media_eligible"

func (a *Account) IsGrok() bool {
	return a != nil && a.Platform == PlatformGrok
}

func (a *Account) IsGrokOAuth() bool {
	return a.IsGrok() && a.Type == AccountTypeOAuth
}

func (a *Account) IsOpenAICompatible() bool {
	return a != nil && (a.Platform == PlatformOpenAI || a.Platform == PlatformGrok ||
		isDomesticOpenAICompatiblePlatform(a.Platform))
}

func isDomesticOpenAICompatiblePlatform(platform string) bool {
	return platform == PlatformKimi || platform == PlatformZhipu || platform == PlatformDeepSeek
}

// IsOpenAICompatibleAPIKey identifies API-key accounts that can use the
// OpenAI-compatible gateway paths (Chat Completions and Responses fallback).
func (a *Account) IsOpenAICompatibleAPIKey() bool {
	return a != nil && a.Type == AccountTypeAPIKey && a.IsOpenAICompatible()
}

func (a *Account) GetGrokBaseURL() string {
	if !a.IsGrok() {
		return ""
	}
	baseURL := strings.TrimSpace(a.GetCredential("base_url"))
	if a.IsGrokOAuth() {
		if baseURL == "" || !xai.IsParseableBaseURL(baseURL) {
			return xai.DefaultCLIBaseURL
		}
		return baseURL
	}
	if baseURL != "" {
		return baseURL
	}
	return xai.DefaultBaseURL
}

func (a *Account) GetGrokMediaBaseURL() string {
	if !a.IsGrok() {
		return ""
	}
	baseURL := a.GetGrokBaseURL()
	if a.IsGrokOAuth() && isGrokCLIProxyTarget(baseURL) {
		return xai.DefaultBaseURL
	}
	return baseURL
}

func (a *Account) GetGrokAccessToken() string {
	if !a.IsGrok() {
		return ""
	}
	return a.GetCredential("access_token")
}

func (a *Account) GetGrokRefreshToken() string {
	if !a.IsGrokOAuth() {
		return ""
	}
	return a.GetCredential("refresh_token")
}

func (a *Account) SupportsOpenAIEndpointCapability(capability OpenAIEndpointCapability) bool {
	if a == nil {
		return false
	}
	if capability == "" {
		return true
	}
	if !a.IsOpenAICompatible() {
		return false
	}
	if a.IsGrok() {
		switch capability {
		case OpenAIEndpointCapabilityChatCompletions, OpenAIEndpointCapabilityResponses:
			return true
		case OpenAIEndpointCapabilityGrokMediaGeneration:
			eligible, reason := a.GrokMediaGenerationEligibility()
			return eligible || reason == "billing_unobserved"
		default:
			return false
		}
	}

	switch capability {
	case OpenAIEndpointCapabilityChatCompletions:
	case OpenAIEndpointCapabilityResponses:
		// Domestic OpenAI-compatible API-key providers commonly expose only
		// /v1/chat/completions. The gateway transparently converts Responses
		// requests to that endpoint, so both capabilities are schedulable.
		if isDomesticOpenAICompatiblePlatform(a.Platform) {
			capability = OpenAIEndpointCapabilityChatCompletions
		} else {
			if a.Type == AccountTypeAPIKey && !openai_compat.ShouldUseResponsesAPI(a.Extra) {
				return false
			}
			capability = OpenAIEndpointCapabilityChatCompletions
		}
	case OpenAIEndpointCapabilityAlphaSearch:
		if a.Type != AccountTypeOAuth && a.Type != AccountTypeAPIKey {
			return false
		}
	case OpenAIEndpointCapabilityEmbeddings:
		if a.Type != AccountTypeAPIKey {
			return false
		}
	default:
		return false
	}

	configured, found := a.openAIEndpointCapabilitySet()
	if !found {
		return true
	}
	if capability == OpenAIEndpointCapabilityAlphaSearch && configured[string(OpenAIEndpointCapabilityChatCompletions)] {
		return true
	}
	return configured[string(capability)]
}

func (a *Account) GrokMediaGenerationEligibility() (bool, string) {
	if a == nil || !a.IsGrok() {
		return false, "not_grok"
	}
	if override, ok := grokMediaEligibilityOverride(a.Extra); ok {
		if override {
			return true, "override_enabled"
		}
		return false, "override_disabled"
	}
	if a.Type != AccountTypeOAuth {
		return true, "non_oauth"
	}
	billing, err := grokBillingSnapshotFromExtra(a.Extra)
	if err != nil || billing == nil {
		return false, "billing_unobserved"
	}
	if billing.StatusCode == 403 || billing.WeeklyStatusCode == 403 || billing.MonthlyStatusCode == 403 {
		return false, "billing_forbidden"
	}
	if isKnownGrokFreeAccount(a) {
		return false, "billing_free_tier"
	}
	if !grokBillingHasAuthoritativeQuota(billing) {
		return false, "billing_inconclusive"
	}
	return true, "eligible"
}

func grokMediaEligibilityOverride(extra map[string]any) (bool, bool) {
	if extra == nil {
		return false, false
	}
	raw, exists := extra[GrokMediaEligibleExtraKey]
	if !exists || raw == nil {
		return false, false
	}
	value, ok := raw.(bool)
	return value, ok
}

func (a *Account) openAIEndpointCapabilitySet() (map[string]bool, bool) {
	if a == nil || a.Credentials == nil {
		return nil, false
	}
	raw, found := a.Credentials[openAIEndpointCapabilitiesCredentialKey]
	if !found || raw == nil {
		return nil, false
	}
	result := make(map[string]bool)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			result[value] = true
		}
	}
	switch capabilities := raw.(type) {
	case []any:
		for _, item := range capabilities {
			if value, ok := item.(string); ok {
				add(value)
			}
		}
	case []string:
		for _, value := range capabilities {
			add(value)
		}
	case map[string]any:
		for key, value := range capabilities {
			if enabled, ok := value.(bool); ok && enabled {
				add(key)
			}
		}
	case map[string]bool:
		for key, enabled := range capabilities {
			if enabled {
				add(key)
			}
		}
	}
	return result, true
}
