package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIChatFailingWriter struct {
	gin.ResponseWriter
	failAfter int
	writes    int
}

func (w *openAIChatFailingWriter) Write(p []byte) (int, error) {
	if w.writes >= w.failAfter {
		return 0, errors.New("write failed: client disconnected")
	}
	w.writes++
	return w.ResponseWriter.Write(p)
}

func TestNormalizeResponsesRequestServiceTier(t *testing.T) {
	t.Parallel()

	req := &apicompat.ResponsesRequest{ServiceTier: " fast "}
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "priority", req.ServiceTier)

	req.ServiceTier = "flex"
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "flex", req.ServiceTier)

	// OpenAI 官方合法 tier 应被透传保留。
	req.ServiceTier = "auto"
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "auto", req.ServiceTier)

	req.ServiceTier = "default"
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "default", req.ServiceTier)

	req.ServiceTier = "scale"
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "scale", req.ServiceTier)

	// 真未知值仍被剥离。
	req.ServiceTier = "turbo"
	normalizeResponsesRequestServiceTier(req)
	require.Empty(t, req.ServiceTier)
}

func TestNormalizeResponsesBodyServiceTier(t *testing.T) {
	t.Parallel()

	body, tier, err := normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"fast"}`))
	require.NoError(t, err)
	require.Equal(t, "priority", tier)
	require.Equal(t, "priority", gjson.GetBytes(body, "service_tier").String())

	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"flex"}`))
	require.NoError(t, err)
	require.Equal(t, "flex", tier)
	require.Equal(t, "flex", gjson.GetBytes(body, "service_tier").String())

	// OpenAI 官方 tier 直接保留在 body 中（透传上游）。
	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"auto"}`))
	require.NoError(t, err)
	require.Equal(t, "auto", tier)
	require.Equal(t, "auto", gjson.GetBytes(body, "service_tier").String())

	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"default"}`))
	require.NoError(t, err)
	require.Equal(t, "default", tier)
	require.Equal(t, "default", gjson.GetBytes(body, "service_tier").String())

	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"scale"}`))
	require.NoError(t, err)
	require.Equal(t, "scale", tier)
	require.Equal(t, "scale", gjson.GetBytes(body, "service_tier").String())

	// 真未知值才会被删除。
	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"turbo"}`))
	require.NoError(t, err)
	require.Empty(t, tier)
	require.False(t, gjson.GetBytes(body, "service_tier").Exists())
}

func TestGetOpenAIStructuredOutputMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		account *Account
		want    string
	}{
		{name: "nil account", want: OpenAIStructuredOutputModeNative},
		{name: "missing extra", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, want: OpenAIStructuredOutputModeNative},
		{name: "force non strict", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIStructuredOutputModeExtraKey: " force_non_strict "}}, want: OpenAIStructuredOutputModeForceNonStrict},
		{name: "unknown mode", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIStructuredOutputModeExtraKey: "unknown"}}, want: OpenAIStructuredOutputModeNative},
		{name: "oauth ignores mode", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAIStructuredOutputModeExtraKey: OpenAIStructuredOutputModeForceNonStrict}}, want: OpenAIStructuredOutputModeNative},
		{name: "other platform ignores mode", account: &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{OpenAIStructuredOutputModeExtraKey: OpenAIStructuredOutputModeForceNonStrict}}, want: OpenAIStructuredOutputModeNative},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.account.GetOpenAIStructuredOutputMode())
		})
	}
}

func TestApplyOpenAIStructuredOutputCompatibility(t *testing.T) {
	t.Parallel()

	compatAccount := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			OpenAIStructuredOutputModeExtraKey: OpenAIStructuredOutputModeForceNonStrict,
		},
	}
	nativeAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	recursiveSchema := `{"type":"object","properties":{"children":{"type":"array","items":{"$ref":"#/$defs/node"}}},"$defs":{"node":{"type":"object","properties":{"children":{"type":"array","items":{"$ref":"#/$defs/node"}}}}}}`

	tests := []struct {
		name          string
		account       *Account
		body          string
		wantChanged   bool
		wantRequested string
		wantStrict    string
		wantSchema    string
	}{
		{
			name:       "native mode preserves true",
			account:    nativeAccount,
			body:       `{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":true}}}`,
			wantStrict: "true",
		},
		{
			name:          "compat mode adds false when omitted",
			account:       compatAccount,
			body:          `{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}}`,
			wantChanged:   true,
			wantRequested: "omitted",
			wantStrict:    "false",
		},
		{
			name:          "compat mode downgrades true and preserves recursive schema",
			account:       compatAccount,
			body:          `{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + recursiveSchema + `,"strict":true}}}`,
			wantChanged:   true,
			wantRequested: "true",
			wantStrict:    "false",
			wantSchema:    recursiveSchema,
		},
		{
			name:       "compat mode preserves explicit false",
			account:    compatAccount,
			body:       `{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":false}}}`,
			wantStrict: "false",
		},
		{
			name:    "compat mode ignores json object",
			account: compatAccount,
			body:    `{"response_format":{"type":"json_object"}}`,
		},
		{
			name:       "compat mode does not repair malformed strict",
			account:    compatAccount,
			body:       `{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":"true"}}}`,
			wantStrict: `"true"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, changed, requested, err := applyOpenAIStructuredOutputCompatibility([]byte(tt.body), tt.account)
			require.NoError(t, err)
			require.Equal(t, tt.wantChanged, changed)
			require.Equal(t, tt.wantRequested, requested)
			require.Equal(t, tt.wantStrict, gjson.GetBytes(out, "response_format.json_schema.strict").Raw)
			if tt.wantSchema != "" {
				require.JSONEq(t, tt.wantSchema, gjson.GetBytes(out, "response_format.json_schema.schema").Raw)
			}
		})
	}
}

func TestForwardAsChatCompletions_ForceNonStrictStructuredOutput(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"Extract the document"}],
		"stream":false,
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"structured_output",
				"schema":{
					"type":"object",
					"properties":{"children":{"type":"array","items":{"$ref":"#/$defs/node"}}},
					"$defs":{"node":{"type":"object","properties":{"children":{"type":"array","items":{"$ref":"#/$defs/node"}}}}}
				},
				"strict":true
			}
		}
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop after capture"}}`)),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          73,
		Name:        "sag-compatible-upstream",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.com"},
		Extra: map[string]any{
			OpenAIStructuredOutputModeExtraKey:       OpenAIStructuredOutputModeForceNonStrict,
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, "false", gjson.GetBytes(upstream.lastBody, "text.format.strict").Raw)
	require.Equal(t, "structured_output", gjson.GetBytes(upstream.lastBody, "text.format.name").String())
	require.Equal(t, "#/$defs/node", gjson.GetBytes(upstream.lastBody, "text.format.schema.properties.children.items.$ref").String())
	require.Equal(t, "#/$defs/node", gjson.GetBytes(upstream.lastBody, "text.format.schema.$defs.node.properties.children.items.$ref").String())
}

func TestForwardAsChatCompletions_ForceNonStrictStructuredOutputRawChat(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"Extract the document"}],
		"stream":false,
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"structured_output",
				"schema":{"type":"object"},
				"strict":true
			}
		}
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop after capture"}}`)),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          73,
		Name:        "sag-compatible-raw-upstream",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.com"},
		Extra: map[string]any{
			OpenAIStructuredOutputModeExtraKey:  OpenAIStructuredOutputModeForceNonStrict,
			openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, "json_schema", gjson.GetBytes(upstream.lastBody, "response_format.type").String())
	require.Equal(t, "false", gjson.GetBytes(upstream.lastBody, "response_format.json_schema.strict").Raw)
}

func TestForwardAsChatCompletions_APIKeyAutoDerivesStableIsolatedPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := func() *http.Response {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop before response parsing"}}`))}
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response(), response(), response()}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 2, Name: "openai-compatible", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "sk-compatible"}, Extra: map[string]any{"openai_responses_supported": true}}
	firstBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hello"}],"stream":false}`)
	appendedBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"continue"}],"stream":false}`)
	forward := func(apiKeyID int64, body []byte) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: apiKeyID})
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")
		require.Error(t, err)
		require.Nil(t, result)
	}
	forward(99, firstBody)
	forward(99, appendedBody)
	forward(100, appendedBody)
	firstKey := gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String()
	appendedKey := gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String()
	otherTenantKey := gjson.GetBytes(upstream.bodies[2], "prompt_cache_key").String()
	require.NotEmpty(t, firstKey)
	require.Equal(t, firstKey, appendedKey)
	require.NotEqual(t, firstKey, otherTenantKey)
	require.Equal(t, generateSessionUUID(firstKey), upstream.requests[0].Header.Get("session_id"))
	require.Equal(t, upstream.requests[0].Header.Get("session_id"), upstream.requests[1].Header.Get("session_id"))
	require.Equal(t, generateSessionUUID(otherTenantKey), upstream.requests[2].Header.Get("session_id"))
	require.NotEqual(t, upstream.requests[1].Header.Get("session_id"), upstream.requests[2].Header.Get("session_id"))
}

func TestForwardAsChatCompletions_ResponsesShapeDoesNotAutoDerivePromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := func() *http.Response {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop before response parsing"}}`))}
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{response(), response(), response()}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 2, Name: "openai-compatible", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "sk-compatible"}, Extra: map[string]any{"openai_responses_supported": true}}
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_text","text":"input"}]}],"stream":false}`)
	forward := func(body []byte, key string) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: 99})
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, key, "gpt-5.4")
		require.Error(t, err)
	}
	forward(body, "")
	forward(body, "explicit-responses-key")
	require.False(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").Exists())
	require.Empty(t, upstream.requests[0].Header.Get("session_id"))
	require.Equal(t, "explicit-responses-key", gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String())
	require.Equal(t, generateSessionUUID(isolateOpenAISessionID(99, "explicit-responses-key")), upstream.requests[1].Header.Get("session_id"))
}

func TestForwardAsChatCompletions_UnknownModelDoesNotUseDefaultMappedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt6","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_chat_unknown_model"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"model not found"}}`)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.4")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, "gpt6", gjson.GetBytes(upstream.lastBody, "model").String())
	require.NotEqual(t, "gpt-5.4", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestForwardAsChatCompletions_ClientDisconnectDrainsUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAIChatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16,"input_tokens_details":{"cached_tokens":4}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_disconnect"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
}

func TestExtractOpenAIUsageParsesGPT56CacheWriteTokens(t *testing.T) {
	body := []byte(`{
		"response": {
			"usage": {
				"input_tokens": 300,
				"output_tokens": 40,
				"input_tokens_details": {
					"cached_tokens": 50,
					"cache_write_tokens": 70
				}
			}
		}
	}`)

	usage, ok := ExtractOpenAIUsageFromJSONBytes(body)
	require.True(t, ok)
	require.Equal(t, 300, usage.InputTokens)
	require.Equal(t, 40, usage.OutputTokens)
	require.Equal(t, 70, usage.CacheCreationInputTokens)
	require.Equal(t, 50, usage.CacheReadInputTokens)
}

func TestForwardAsChatCompletions_TerminalUsageWithoutUpstreamCloseReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAIChatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":17,"output_tokens":8,"total_tokens":25,"input_tokens_details":{"cached_tokens":6}}}}` + "\n\n")
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_terminal_no_close"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 17, got.result.Usage.InputTokens)
		require.Equal(t, 8, got.result.Usage.OutputTokens)
		require.Equal(t, 6, got.result.Usage.CacheReadInputTokens)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsChatCompletions should return after terminal usage event even if upstream keeps the connection open")
	}
}

func TestForwardAsChatCompletions_EventNamedTerminalWithoutUpstreamCloseReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(strings.Join([]string{
		`event: response.created`,
		`data: {"response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"delta":"ok"}`,
		``,
		`event: response.completed`,
		`data: {"response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":17,"output_tokens":8,"total_tokens":25,"input_tokens_details":{"cached_tokens":6}}}}`,
		``,
		``,
	}, "\n"))
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_event_named_terminal"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 17, got.result.Usage.InputTokens)
		require.Equal(t, 8, got.result.Usage.OutputTokens)
		require.Equal(t, 6, got.result.Usage.CacheReadInputTokens)
		require.Contains(t, rec.Body.String(), `"content":"ok"`)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsChatCompletions should use SSE event names when data payloads omit type")
	}
}

func TestForwardAsChatCompletions_EventTypeDoesNotLeakAcrossFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`event: response.created`,
		`data: {"response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		``,
		`event: response.completed`,
		`data: {"response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":17,"output_tokens":8,"total_tokens":25,"input_tokens_details":{"cached_tokens":6}}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_event_boundary"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"content":"ok"`)
	require.Contains(t, rec.Body.String(), `data: [DONE]`)
}

func TestForwardAsChatCompletions_BufferedTerminalWithoutUpstreamCloseReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":17,"output_tokens":8,"total_tokens":25,"input_tokens_details":{"cached_tokens":6}}}}` + "\n\n")
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_buffered_terminal_no_close"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{
		httpUpstream:         upstream,
		responseHeaderFilter: compileResponseHeaderFilter(&config.Config{}),
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 17, got.result.Usage.InputTokens)
		require.Equal(t, 8, got.result.Usage.OutputTokens)
		require.Equal(t, 6, got.result.Usage.CacheReadInputTokens)
		require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
		require.Contains(t, rec.Body.String(), `"finish_reason":"stop"`)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsChatCompletions buffered response should return after terminal usage event even if upstream keeps the connection open")
	}
}

func TestHandleAnthropicBufferedStreamingResponseOverridesSSEContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	upstreamBody := `data: {"type":"response.completed","response":{"id":"resp_messages","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_messages_json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	svc := &OpenAIGatewayService{responseHeaderFilter: compileResponseHeaderFilter(&config.Config{})}

	result, err := svc.handleAnthropicBufferedStreamingResponse(resp, c, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}

func TestHandleChatStreamingResponse_SmallSilentRefusalTriggersRetryableFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_silent","model":"grok-4.5"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_silent","object":"response","model":"grok-4.5","status":"completed","output":[]}}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_grok_silent_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	account := &Account{ID: 120, Name: "grok", Platform: PlatformOpenAI}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleChatStreamingResponse(resp, c, account, "grok-4.5", "grok-4.5", "grok-4.5", false, time.Now())
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, IsOpenAISilentRefusalErrorBody(failoverErr.ResponseBody))
	require.False(t, c.Writer.Written())
}

func TestHandleChatBufferedStreamingResponse_SilentRefusalTriggersRetryableFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	upstreamBody := `data: {"type":"response.completed","response":{"id":"resp_silent_json","object":"response","model":"grok-4.5","status":"completed","output":[]}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_grok_silent_json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	account := &Account{ID: 120, Name: "grok", Platform: PlatformOpenAI}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleChatBufferedStreamingResponse(resp, c, account, "grok-4.5", "grok-4.5", "grok-4.5", time.Now())
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, IsOpenAISilentRefusalErrorBody(failoverErr.ResponseBody))
	require.False(t, c.Writer.Written())
}

func TestForwardAsChatCompletions_DoneSentinelWithoutTerminalReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := "data: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_missing_terminal"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result)
	require.Zero(t, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
}

func TestForwardAsChatCompletions_UpstreamRequestCancelsBeforeResponseStarts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqCtx, cancel := context.WithCancel(context.Background())
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(reqCtx)
	c.Request.Header.Set("Content-Type", "application/json")
	cancel()

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_chat_ctx"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsChatCompletions(reqCtx, c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.ErrorIs(t, upstream.lastReq.Context().Err(), context.Canceled)
}
