package service

import "testing"

func TestChineseProviderPlatformContracts(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepSeek} {
		if !IsAllowedQuotaPlatform(platform) {
			t.Fatalf("%s must be available to the platform quota contract", platform)
		}
	}
}

func TestChineseProviderOpenAICompatibilityContract(t *testing.T) {
	wantBase := map[string]string{
		PlatformKimi:     "https://api.moonshot.cn",
		PlatformZhipu:    "https://open.bigmodel.cn/api/paas",
		PlatformDeepSeek: "https://api.deepseek.com",
	}
	for platform, base := range wantBase {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key"}}
		if !account.IsOpenAICompatible() || !account.IsOpenAICompatibleAPIKey() {
			t.Fatalf("%s must be an OpenAI-compatible API-key account", platform)
		}
		if got := account.GetOpenAIBaseURL(); got != base {
			t.Fatalf("%s default base URL = %q, want %q", platform, got, base)
		}
		if got := account.GetOpenAIApiKey(); got != "key" {
			t.Fatalf("%s API key = %q", platform, got)
		}
		if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions) ||
			!account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) {
			t.Fatalf("%s must support chat/responses gateway capabilities", platform)
		}
		account.Extra = map[string]any{"openai_capabilities": []string{string(OpenAIEndpointCapabilityChatCompletions)}}
		if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) {
			t.Fatalf("%s responses must use the declared chat_completions capability", platform)
		}
	}
}
