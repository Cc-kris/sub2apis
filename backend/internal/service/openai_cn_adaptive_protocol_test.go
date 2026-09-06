package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChineseProviderMessagesUsesNativeChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"cn-message"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"id":"chat-cn","model":"kimi-k2","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 17, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "kimi-key"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, []byte(`{"model":"kimi-k2","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hello"}]}`), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer kimi-key", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"type":"message"`)
}

func TestChineseProviderFixedAnthropicProtocolUsesConfiguredEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"cn-native-anthropic"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"id":"msg-cn","type":"message","role":"assistant","content":[],"model":"kimi-k2","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 22, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "kimi-key", "api_protocol": "anthropic", "api_base_urls": map[string]any{"anthropic": "https://anthropic.kimi.example/v1"},
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, []byte(`{"model":"kimi-k2","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hello"}]}`), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "anthropic.kimi.example", upstream.lastReq.URL.Hostname())
	require.Equal(t, "/v1/messages", upstream.lastReq.URL.Path)
	require.Equal(t, "kimi-key", upstream.lastReq.Header.Get("x-api-key"))
	require.Equal(t, 3, result.Usage.InputTokens)
}

func TestChineseProviderResponsesUsesChatCompletionsBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"cn-response"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"id":"chat-cn","model":"deepseek-chat","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 18, Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "deepseek-key"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"deepseek-chat","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer deepseek-key", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestDeepSeekResponsesUsesConfiguredNativeEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"id":"resp-cn","object":"response","status":"completed","model":"deepseek-reasoner","output":[],"usage":{"input_tokens":3,"output_tokens":2}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 21, Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key":       "deepseek-key",
		"api_base_urls": map[string]any{"responses": "https://deepseek.example/v1"},
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"deepseek-reasoner","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
	require.Equal(t, "deepseek.example", upstream.lastReq.URL.Hostname())
}

func TestDeepSeekFixedChatProtocolDoesNotUseResponsesEndpoint(t *testing.T) {
	account := &Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_protocol": "chat_completions", "api_base_urls": map[string]any{"responses": "https://deepseek.example/v1"},
	}}
	require.False(t, account.UsesNativeResponsesForDomesticProvider())
}

func TestDomesticOpenAIAccountValidation(t *testing.T) {
	require.Error(t, ValidateDomesticOpenAIAccountCredentials(PlatformKimi, AccountTypeOAuth, map[string]any{}))
	require.Error(t, ValidateDomesticOpenAIAccountCredentials(PlatformDeepSeek, AccountTypeAPIKey, map[string]any{"api_protocol": "responses"}))
	require.NoError(t, ValidateDomesticOpenAIAccountCredentials(PlatformDeepSeek, AccountTypeAPIKey, map[string]any{
		"api_protocol": "responses", "api_base_urls": map[string]any{"responses": "https://deepseek.example/v1"},
	}))
	require.Error(t, ValidateDomesticOpenAIAccountCredentials(PlatformKimi, AccountTypeAPIKey, map[string]any{
		"api_protocol": "responses", "api_base_urls": map[string]any{"responses": "https://kimi.example/v1"},
	}))
	require.NoError(t, ValidateDomesticOpenAIAccountCredentials(PlatformKimi, AccountTypeAPIKey, map[string]any{
		"api_protocol": "responses", "api_base_urls": map[string]any{"chat_completions": "https://kimi.example/v1"},
	}))
}

func TestChineseProviderResponsesRejectsPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 19, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "zhipu-key"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"glm-4.5","input":"continue","previous_response_id":"resp_1"}`))
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "previous_response_id is not supported")
	require.Nil(t, upstream.lastReq)
}

func TestChineseProviderMessagesStreamReturnsErrorForMalformedUpstreamEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString("data: {not-json}\\n\\n")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 20, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "kimi-key"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, []byte(`{"model":"kimi-k2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`), "", "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "parse chat completions stream event")
	require.NotContains(t, recorder.Body.String(), "message_stop")
}
