package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/repository" //nolint:depguard // integration tests exercise the real upstream adapter
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOpenAIHandleStreamingAwareError_JSONEscaping(t *testing.T) {
	tests := []struct {
		name    string
		errType string
		message string
	}{
		{
			name:    "包含双引号的消息",
			errType: "server_error",
			message: `upstream returned "invalid" response`,
		},
		{
			name:    "包含反斜杠的消息",
			errType: "server_error",
			message: `path C:\Users\test\file.txt not found`,
		},
		{
			name:    "包含双引号和反斜杠的消息",
			errType: "upstream_error",
			message: `error parsing "key\value": unexpected token`,
		},
		{
			name:    "包含换行符的消息",
			errType: "server_error",
			message: "line1\nline2\ttab",
		},
		{
			name:    "普通消息",
			errType: "upstream_error",
			message: "Upstream service temporarily unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

			h := &OpenAIGatewayHandler{}
			h.handleStreamingAwareError(c, http.StatusBadGateway, tt.errType, tt.message, true)

			body := w.Body.String()

			// 验证 SSE 格式：event: error\ndata: {JSON}\n\n
			assert.True(t, strings.HasPrefix(body, "event: error\n"), "应以 'event: error\\n' 开头")
			assert.True(t, strings.HasSuffix(body, "\n\n"), "应以 '\\n\\n' 结尾")

			// 提取 data 部分
			lines := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n")
			require.Len(t, lines, 2, "应有 event 行和 data 行")
			dataLine := lines[1]
			require.True(t, strings.HasPrefix(dataLine, "data: "), "第二行应以 'data: ' 开头")
			jsonStr := strings.TrimPrefix(dataLine, "data: ")

			// 验证 JSON 合法性
			var parsed map[string]any
			err := json.Unmarshal([]byte(jsonStr), &parsed)
			require.NoError(t, err, "JSON 应能被成功解析，原始 JSON: %s", jsonStr)

			// 验证结构
			errorObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok, "应包含 error 对象")
			assert.Equal(t, tt.errType, errorObj["type"])
			assert.Equal(t, tt.message, errorObj["message"])
		})
	}
}

func TestResolveOpenAIMessagesMetadataSession_DoesNotDerivePromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"claude-code-session"},"messages":[{"role":"user","content":"hello"}]}`)

	sessionHash, promptCacheKey := resolveOpenAIMessagesMetadataSession("", "", "claude-sonnet-4-5", body)

	require.NotEmpty(t, sessionHash)
	require.Empty(t, promptCacheKey)
}

func TestResolveOpenAIMessagesMetadataSession_PreservesExplicitPromptCacheKey(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"claude-code-session"}}`)

	sessionHash, promptCacheKey := resolveOpenAIMessagesMetadataSession("", "explicit-cache", "claude-sonnet-4-5", body)

	require.NotEmpty(t, sessionHash)
	require.Equal(t, "explicit-cache", promptCacheKey)
}

func TestOpenAIHandleStreamingAwareError_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h := &OpenAIGatewayHandler{}
	h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "test error", false)

	// 非流式应返回 JSON 响应
	assert.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "test error", errorObj["message"])
}

func TestReadRequestBodyWithPrealloc(t *testing.T) {
	payload := `{"model":"gpt-5","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
	req.ContentLength = int64(len(payload))

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(req)
	require.NoError(t, err)
	require.Equal(t, payload, string(body))
}

func TestReadRequestBodyWithPrealloc_MaxBytesError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("x", 8)))
	req.Body = http.MaxBytesReader(rec, req.Body, 4)

	_, err := pkghttputil.ReadRequestBodyWithPrealloc(req)
	require.Error(t, err)
	var maxErr *http.MaxBytesError
	require.ErrorAs(t, err, &maxErr)
}

func TestOpenAIEnsureForwardErrorResponse_WritesFallbackWhenNotWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, false)

	require.True(t, wrote)
	require.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "Upstream request failed", errorObj["message"])
}

// Writer 已写后 ensureForwardErrorResponse 必须仍然把错误信息以 SSE
// 形式追加给客户端（streamStarted 强制 true）。
// 这是 case B 修复：旧实现遇到 Writer.Written 直接 return false，
// 客户端只能拿到 silent EOF；Codex CLI 报 "stream closed before response.completed"。
func TestOpenAIEnsureForwardErrorResponse_AppendsSSEAfterWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.String(http.StatusTeapot, "already written")

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, false)

	require.True(t, wrote, "must attempt to communicate the failure to the client via SSE")
	// 状态码改不了（headers 已 flush），但 body 应该追加 SSE 错误事件。
	require.Equal(t, http.StatusTeapot, w.Code)
	assert.Contains(t, w.Body.String(), "already written")
	// 非 /responses 路径走 legacy event: error 分支。
	assert.Contains(t, w.Body.String(), "event: error\n")
}

// case B 回归测试：/responses 路径，Writer 已被写过（模拟 ping flushed），
// ensureForwardErrorResponse 必须发 response.failed，让 Codex 收到合规终止事件。
func TestOpenAIEnsureForwardErrorResponse_ResponsesRouteAfterWrittenEmitsResponseFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
	// 模拟 ping 已 flush 的状态：Writer 已写过 1 个字节
	_, _ = c.Writer.WriteString(":\n\n")

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, false)

	require.True(t, wrote)
	body := w.Body.String()
	assert.Contains(t, body, ":\n\n", "earlier ping bytes preserved")
	assert.Contains(t, body, "event: response.failed\n", "appended a Responses terminal event")
	assert.Contains(t, body, `"type":"response.failed"`)
	assert.Contains(t, body, `"code":"upstream_error"`)
	assert.Contains(t, body, "Upstream request failed")
}

func TestOpenAIEnsureAnthropicErrorResponse_AfterWrittenEmitsSSEError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, EndpointMessages, nil)
	_, _ = c.Writer.WriteString("event: message_start\ndata: {}\n\n")

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureAnthropicErrorResponse(c, false)

	require.True(t, wrote)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "event: message_start")
	assert.Contains(t, w.Body.String(), "event: error\n")
	assert.Contains(t, w.Body.String(), `"type":"api_error"`)
}

func TestShouldLogOpenAIForwardFailureAsWarn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("fallback_written_should_not_downgrade", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		require.False(t, shouldLogOpenAIForwardFailureAsWarn(c, true))
	})

	t.Run("context_nil_should_not_downgrade", func(t *testing.T) {
		require.False(t, shouldLogOpenAIForwardFailureAsWarn(nil, false))
	})

	t.Run("response_not_written_should_not_downgrade", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		require.False(t, shouldLogOpenAIForwardFailureAsWarn(c, false))
	})

	t.Run("response_already_written_should_downgrade", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.String(http.StatusForbidden, "already written")
		require.True(t, shouldLogOpenAIForwardFailureAsWarn(c, false))
	})
}

func TestOpenAIRecoverResponsesPanic_WritesFallbackResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	h := &OpenAIGatewayHandler{}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
			panic("test panic")
		}()
	})

	require.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)

	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "Upstream request failed", errorObj["message"])
}

func TestOpenAIRecoverResponsesPanic_NoPanicNoWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	h := &OpenAIGatewayHandler{}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
		}()
	})

	require.False(t, c.Writer.Written())
	assert.Equal(t, "", w.Body.String())
}

// Panic 在已 flush 的 /v1/responses 流中：状态码无法改（已 written），
// 但 body 应追加 response.failed 让客户端识别为合规截断而不是 silent EOF。
func TestOpenAIRecoverResponsesPanic_AppendsResponseFailedAfterWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.String(http.StatusTeapot, "already written")

	h := &OpenAIGatewayHandler{}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
			panic("test panic")
		}()
	})

	require.Equal(t, http.StatusTeapot, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "already written")
	assert.Contains(t, body, "event: response.failed\n")
}

func TestOpenAIMissingResponsesDependencies(t *testing.T) {
	t.Run("nil_handler", func(t *testing.T) {
		var h *OpenAIGatewayHandler
		require.Equal(t, []string{"handler"}, h.missingResponsesDependencies())
	})

	t.Run("all_dependencies_missing", func(t *testing.T) {
		h := &OpenAIGatewayHandler{}
		require.Equal(t,
			[]string{"gatewayService", "billingCacheService", "apiKeyService", "concurrencyHelper"},
			h.missingResponsesDependencies(),
		)
	})

	t.Run("all_dependencies_present", func(t *testing.T) {
		h := &OpenAIGatewayHandler{
			gatewayService:      &service.OpenAIGatewayService{},
			billingCacheService: &service.BillingCacheService{},
			apiKeyService:       &service.APIKeyService{},
			concurrencyHelper: &ConcurrencyHelper{
				concurrencyService: &service.ConcurrencyService{},
			},
		}
		require.Empty(t, h.missingResponsesDependencies())
	})
}

func TestOpenAIEnsureResponsesDependencies(t *testing.T) {
	t.Run("missing_dependencies_returns_503", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		h := &OpenAIGatewayHandler{}
		ok := h.ensureResponsesDependencies(c, nil)

		require.False(t, ok)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		var parsed map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &parsed)
		require.NoError(t, err)
		errorObj, exists := parsed["error"].(map[string]any)
		require.True(t, exists)
		assert.Equal(t, "api_error", errorObj["type"])
		assert.Equal(t, "Service temporarily unavailable", errorObj["message"])
	})

	t.Run("already_written_response_not_overridden", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.String(http.StatusTeapot, "already written")

		h := &OpenAIGatewayHandler{}
		ok := h.ensureResponsesDependencies(c, nil)

		require.False(t, ok)
		require.Equal(t, http.StatusTeapot, w.Code)
		assert.Equal(t, "already written", w.Body.String())
	})

	t.Run("dependencies_ready_returns_true_and_no_write", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		h := &OpenAIGatewayHandler{
			gatewayService:      &service.OpenAIGatewayService{},
			billingCacheService: &service.BillingCacheService{},
			apiKeyService:       &service.APIKeyService{},
			concurrencyHelper: &ConcurrencyHelper{
				concurrencyService: &service.ConcurrencyService{},
			},
		}
		ok := h.ensureResponsesDependencies(c, nil)

		require.True(t, ok)
		require.False(t, c.Writer.Written())
		assert.Equal(t, "", w.Body.String())
	})
}

func TestResolveOpenAIMessagesDispatchMappedModel(t *testing.T) {
	t.Run("exact_claude_model_override_wins", func(t *testing.T) {
		apiKey := &service.APIKey{
			Group: &service.Group{
				MessagesDispatchModelConfig: service.OpenAIMessagesDispatchModelConfig{
					SonnetMappedModel: "gpt-5.2",
					ExactModelMappings: map[string]string{
						"claude-sonnet-4-5-20250929": "gpt-5.4-mini-high",
					},
				},
			},
		}
		require.Equal(t, "gpt-5.4-mini", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-sonnet-4-5-20250929"))
	})

	t.Run("uses_family_default_when_no_override", func(t *testing.T) {
		apiKey := &service.APIKey{Group: &service.Group{}}
		require.Equal(t, "gpt-5.4", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-opus-4-6"))
		require.Equal(t, "gpt-5.3-codex", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-sonnet-4-5-20250929"))
		require.Equal(t, "gpt-5.4-mini", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-haiku-4-5-20251001"))
	})

	t.Run("returns_empty_for_non_claude_or_missing_group", func(t *testing.T) {
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(nil, "claude-sonnet-4-5-20250929"))
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(&service.APIKey{}, "claude-sonnet-4-5-20250929"))
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(&service.APIKey{Group: &service.Group{}}, "gpt-5.4"))
	})

	t.Run("does_not_fall_back_to_group_default_mapped_model", func(t *testing.T) {
		apiKey := &service.APIKey{
			Group: &service.Group{
				DefaultMappedModel: "gpt-5.4",
			},
		}
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(apiKey, "gpt-5.4"))
		require.Equal(t, "gpt-5.3-codex", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-sonnet-4-5-20250929"))
	})
}

func TestResolveChannelMappedAccountSelectionModel(t *testing.T) {
	tests := []struct {
		name                string
		fallbackModel       string
		channelMapping      service.ChannelMappingResult
		applyChannelMapping bool
		want                string
	}{
		{
			name:                "uses channel mapped model for account selection",
			fallbackModel:       "gpt-5.6-luna",
			channelMapping:      service.ChannelMappingResult{Mapped: true, MappedModel: "grok-4.5"},
			applyChannelMapping: true,
			want:                "grok-4.5",
		},
		{
			name:                "keeps requested model for orchestrator routing",
			fallbackModel:       "gpt-5.6-luna",
			channelMapping:      service.ChannelMappingResult{Mapped: true, MappedModel: "gpt-image-2"},
			applyChannelMapping: false,
			want:                "gpt-5.6-luna",
		},
		{
			name:                "keeps fallback when mapped model is empty",
			fallbackModel:       "gpt-5.6-luna",
			channelMapping:      service.ChannelMappingResult{Mapped: true},
			applyChannelMapping: true,
			want:                "gpt-5.6-luna",
		},
		{
			name:                "keeps fallback without channel mapping",
			fallbackModel:       "gpt-5.6-luna",
			channelMapping:      service.ChannelMappingResult{MappedModel: "gpt-5.6-luna"},
			applyChannelMapping: true,
			want:                "gpt-5.6-luna",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, resolveChannelMappedAccountSelectionModel(tt.fallbackModel, tt.channelMapping, tt.applyChannelMapping))
		})
	}
}

func TestChannelMappedAccountSelectionAcrossTextModels(t *testing.T) {
	accounts := []service.Account{
		newChannelMappedTextAccount(8101, "grok-account", "grok-4.5"),
		newChannelMappedTextAccount(8102, "deepseek-account", "deepseek-v4-pro"),
		newChannelMappedTextAccount(8103, "kimi-account", "kimi-k3"),
		newChannelMappedTextAccount(8104, "glm-account", "glm-5.2"),
		newChannelMappedTextAccount(8105, "openai-account", "gpt-5.6-terra"),
	}
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{
			{ID: 8201, Name: "grok-channel", Status: service.StatusActive, GroupIDs: []int64{8301}, ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {"*": "grok-4.5"}}},
			{ID: 8202, Name: "deepseek-channel", Status: service.StatusActive, GroupIDs: []int64{8302}, ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {"gpt-5.5": "deepseek-v4-pro"}}},
			{ID: 8203, Name: "kimi-channel", Status: service.StatusActive, GroupIDs: []int64{8303}, ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {"*": "kimi-k3"}}},
			{ID: 8204, Name: "glm-channel", Status: service.StatusActive, GroupIDs: []int64{8304}},
		},
		groupPlatforms: map[int64]string{
			8301: service.PlatformOpenAI,
			8302: service.PlatformOpenAI,
			8303: service.PlatformOpenAI,
			8304: service.PlatformOpenAI,
		},
	}, nil, nil, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSSilentRetryAccountRepoStub{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, nil, repository.NewHTTPUpstream(cfg),
		&service.DeferredService{}, nil, nil, channelSvc, nil, nil, nil, nil,
	)

	tests := []struct {
		name           string
		groupID        int64
		requestedModel string
		wantAccountID  int64
	}{
		{
			name:           "grok channel",
			groupID:        8301,
			requestedModel: "gpt-5.6-luna",
			wantAccountID:  8101,
		},
		{
			name:           "deepseek channel",
			groupID:        8302,
			requestedModel: "gpt-5.5",
			wantAccountID:  8102,
		},
		{
			name:           "kimi channel",
			groupID:        8303,
			requestedModel: "gpt-5.6-sol",
			wantAccountID:  8103,
		},
		{
			name:           "unmapped glm channel",
			groupID:        8304,
			requestedModel: "glm-5.2",
			wantAccountID:  8104,
		},
		{
			name:           "unmapped openai channel",
			groupID:        8399,
			requestedModel: "gpt-5.6-terra",
			wantAccountID:  8105,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := channelSvc.ResolveChannelMapping(context.Background(), tt.groupID, tt.requestedModel)
			selectionModel := resolveChannelMappedAccountSelectionModel(tt.requestedModel, mapping, true)
			account, err := gatewaySvc.SelectAccountForModelWithExclusions(context.Background(), &tt.groupID, "", selectionModel, nil)
			require.NoError(t, err)
			require.Equal(t, tt.wantAccountID, account.ID)
		})
	}
}

func newChannelMappedTextAccount(id int64, name, model string) service.Account {
	return service.Account{
		ID:          id,
		Name:        name,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"model_mapping": map[string]any{model: model},
		},
	}
}

func TestOpenAIResponses_MissingDependencies_ReturnsServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":false}`))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      10,
		GroupID: &groupID,
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	// 故意使用未初始化依赖，验证快速失败而不是崩溃。
	h := &OpenAIGatewayHandler{}
	require.NotPanics(t, func() {
		h.Responses(c)
	})

	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)

	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api_error", errorObj["type"])
	assert.Equal(t, "Service temporarily unavailable", errorObj["message"])
}

func TestOpenAIResponses_SetsClientTransportHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := &OpenAIGatewayHandler{}
	h.Responses(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, service.OpenAIClientTransportHTTP, service.GetOpenAIClientTransport(c))
}

func TestOpenAIResponses_RejectsMessageIDAsPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-5.1","stream":false,"previous_response_id":"msg_123456","input":[{"type":"input_text","text":"hello"}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: 1},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.Responses(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "previous_response_id must be a response.id")
}

func TestOpenAIResponses_RejectsHTTPContinuationPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-5.1","stream":false,"previous_response_id":"resp_123456","input":[{"type":"input_text","text":"hello"}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: 1},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.Responses(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "Responses WebSocket v2")
	require.Contains(t, w.Body.String(), "previous_response_id")
}

func TestOpenAIResponses_FunctionCallOutputHTTPGuidanceDoesNotSuggestPreviousResponseReuse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-5.1","stream":false,"input":[{"type":"function_call_output","output":"{}"}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: 1},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.Responses(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "Responses WebSocket v2")
	require.NotContains(t, w.Body.String(), "reuse previous_response_id")
}

func TestOpenAIResponsesWebSocket_SetsClientTransportWSWhenUpgradeValid(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Connection", "Upgrade")

	h := &OpenAIGatewayHandler{}
	h.ResponsesWebSocket(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, service.OpenAIClientTransportWS, service.GetOpenAIClientTransport(c))
}

func TestOpenAIResponsesWebSocket_InvalidUpgradeDoesNotSetTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)

	h := &OpenAIGatewayHandler{}
	h.ResponsesWebSocket(c)

	require.Equal(t, http.StatusUpgradeRequired, w.Code)
	require.Equal(t, service.OpenAIClientTransportUnknown, service.GetOpenAIClientTransport(c))
}

func TestOpenAIResponsesWebSocket_DisablesDownstreamCompressionNegotiation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 101, User: &service.User{ID: 1}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})
		c.Next()
	})
	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	conn, resp, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/openai/v1/responses", &coderws.DialOptions{
		CompressionMode: coderws.CompressionContextTakeover,
	})
	cancel()
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	require.NotNil(t, resp)
	require.Empty(t, resp.Header.Get("Sec-WebSocket-Extensions"))
}

func TestOpenAIResponsesWebSocket_RejectsMessageIDAsPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(
		`{"type":"response.create","model":"gpt-5.1","stream":false,"previous_response_id":"msg_abc123"}`,
	))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	require.Error(t, err)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
	require.Contains(t, strings.ToLower(closeErr.Reason), "previous_response_id")
}

func TestOpenAIResponsesWebSocket_PreviousResponseIDKindLoggedBeforeAcquireFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return false, errors.New("user slot unavailable")
		},
	}
	h := newOpenAIHandlerForPreviousResponseIDValidation(t, cache)
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(
		`{"type":"response.create","model":"gpt-5.1","stream":false,"previous_response_id":"resp_prev_123"}`,
	))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	require.Error(t, err)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusInternalError, closeErr.Code)
	require.Contains(t, strings.ToLower(closeErr.Reason), "failed to acquire user concurrency slot")
}

type contentModerationHandlerSettingRepo struct {
	values map[string]string
}

func (r *contentModerationHandlerSettingRepo) Get(ctx context.Context, key string) (*service.Setting, error) {
	if value, ok := r.values[key]; ok {
		return &service.Setting{Key: key, Value: value}, nil
	}
	return nil, service.ErrSettingNotFound
}

func (r *contentModerationHandlerSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *contentModerationHandlerSettingRepo) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *contentModerationHandlerSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *contentModerationHandlerSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *contentModerationHandlerSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *contentModerationHandlerSettingRepo) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

type contentModerationHandlerTestRepo struct {
	logs []service.ContentModerationLog
}

func (r *contentModerationHandlerTestRepo) CreateLog(ctx context.Context, log *service.ContentModerationLog) error {
	if log != nil {
		r.logs = append(r.logs, *log)
	}
	return nil
}

func (r *contentModerationHandlerTestRepo) ListLogs(ctx context.Context, filter service.ContentModerationLogFilter) ([]service.ContentModerationLog, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (r *contentModerationHandlerTestRepo) CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	return 0, nil
}

func (r *contentModerationHandlerTestRepo) CleanupExpiredLogs(ctx context.Context, hitBefore time.Time, nonHitBefore time.Time) (*service.ContentModerationCleanupResult, error) {
	return &service.ContentModerationCleanupResult{}, nil
}

func TestOpenAIResponsesWebSocket_ContentModerationBlocksFirstFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)

	moderationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/moderations", r.URL.Path)
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"sexual":0.9}}]}`))
	}))
	defer moderationServer.Close()

	cfg := &service.ContentModerationConfig{
		Enabled:      true,
		Mode:         service.ContentModerationModePreBlock,
		BaseURL:      moderationServer.URL,
		Model:        "omni-moderation-latest",
		APIKeys:      []string{"sk-test"},
		SampleRate:   100,
		AllGroups:    true,
		BlockMessage: "内容审计测试阻断",
	}
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)

	repo := &contentModerationHandlerTestRepo{}
	settingRepo := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:      "true",
		service.SettingKeyContentModerationConfig: string(rawCfg),
	}}
	moderationSvc := service.NewContentModerationService(
		settingRepo,
		repo,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	decision, err := moderationSvc.Check(context.Background(), service.ContentModerationCheckInput{
		UserID:   1,
		Endpoint: "/v1/responses",
		Provider: "openai",
		Model:    "gpt-5.5",
		Protocol: service.ContentModerationProtocolOpenAIResponses,
		Body:     []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"bad prompt"}]}]}`),
	})
	require.NoError(t, err)
	require.True(t, decision.Blocked)
	repo.logs = nil
	h := &OpenAIGatewayHandler{
		gatewayService:           &service.OpenAIGatewayService{},
		billingCacheService:      &service.BillingCacheService{},
		apiKeyService:            &service.APIKeyService{},
		contentModerationService: moderationSvc,
		concurrencyHelper:        NewConcurrencyHelper(service.NewConcurrencyService(&concurrencyCacheMock{}), SSEPingFormatNone, time.Second),
	}
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{
		"type":"response.create",
		"model":"gpt-5.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"bad prompt"}]}]
	}`))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, payload, readErr := clientConn.Read(readCtx)
	cancelRead()
	if readErr == nil {
		require.Contains(t, string(payload), "content_policy_violation")
		require.Contains(t, string(payload), "内容审计测试阻断")
	} else {
		var closeErr coderws.CloseError
		require.ErrorAs(t, readErr, &closeErr)
		require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
		require.Contains(t, closeErr.Reason, "内容审计测试阻断")
	}
	require.Len(t, repo.logs, 1)
	require.True(t, repo.logs[0].Flagged)
	require.Equal(t, service.ContentModerationActionBlock, repo.logs[0].Action)
	require.Equal(t, "bad prompt", repo.logs[0].InputExcerpt)
}

func TestOpenAIResponsesWebSocket_PassthroughUsageLogPersistsUserAgentAndReasoningEffort(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":false,"reasoning":{"effort":"HIGH"}}`,
		userAgent:    testStringPtr("codex_cli_rs/0.125.0 test"),
	})

	require.NotNil(t, got.log.UserAgent)
	require.Equal(t, "codex_cli_rs/0.125.0 test", *got.log.UserAgent)
	require.NotNil(t, got.log.ReasoningEffort)
	require.Equal(t, "high", *got.log.ReasoningEffort)
	require.True(t, got.log.OpenAIWSMode)
}

func TestOpenAIResponsesWebSocket_PassthroughUsageLogInfersReasoningFromInitialRequestModel(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4-xhigh","stream":false}`,
		userAgent:    testStringPtr("codex_cli_rs/0.125.0 mapped"),
		channelMapping: map[string]string{
			"gpt-5.4-xhigh": "gpt-5.4",
		},
	})

	require.Equal(t, "gpt-5.4", gjson.GetBytes(got.upstreamFirstPayload, "model").String(),
		"上游首帧应使用渠道映射后的模型")
	require.NotNil(t, got.log.ReasoningEffort)
	require.Equal(t, "xhigh", *got.log.ReasoningEffort,
		"usage log reasoning effort 必须使用渠道映射前首帧模型后缀推导")
}

func TestOpenAIResponsesWebSocket_ChannelMappedModelSelectsMappedAccount(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:        `{"model":"gpt-5.6-luna","input":"reply OK"}`,
		channelMapping:      map[string]string{"*": "grok-4.5"},
		accountModelMapping: map[string]any{"grok-4.5": "grok-4.5"},
	})

	require.Equal(t, "grok-4.5", gjson.GetBytes(got.upstreamFirstPayload, "model").String())
	require.Equal(t, int64(9901), got.log.AccountID)
}

func TestOpenAIResponsesWebSocket_OpenAICompatibleGrokWithoutResponsesUsesHTTPChatFallback(t *testing.T) {
	responsesSupported := false
	allowImageGeneration := false
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:         `{"type":"response.create","model":"gpt-5.6-luna","input":"reply OK","stream":false,"tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]},{"type":"function","name":"shell","description":"Run shell","parameters":{"type":"object"}}],"tool_choice":"auto"}`,
		channelMapping:       map[string]string{"*": "grok-4.5"},
		accountModelMapping:  map[string]any{"grok-4.5": "grok-4.5"},
		responsesSupported:   &responsesSupported,
		allowImageGeneration: &allowImageGeneration,
	})

	require.Equal(t, int32(1), got.upstreamHTTPHits)
	require.Equal(t, int32(0), got.upstreamWSHits)
	require.Equal(t, "grok-4.5", gjson.GetBytes(got.upstreamFirstPayload, "model").String())
	require.True(t, gjson.GetBytes(got.upstreamFirstPayload, "messages").Exists())
	require.Equal(t, "shell", gjson.GetBytes(got.upstreamFirstPayload, "tools.0.function.name").String())
	require.NotContains(t, string(got.upstreamFirstPayload), "image_gen")
	require.NotContains(t, string(got.upstreamFirstPayload), "imagegen")
	require.Equal(t, int64(9901), got.log.AccountID)
}

func TestOpenAIResponses_ChannelMappedModelSelectsMappedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type capturedRequest struct {
		path              string
		authorization     string
		grokClientVersion string
		payload           []byte
	}
	upstreamRequest := make(chan capturedRequest, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		upstreamRequest <- capturedRequest{
			path:              r.URL.Path,
			authorization:     r.Header.Get("Authorization"),
			grokClientVersion: r.Header.Get("X-Grok-Client-Version"),
			payload:           payload,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_grok_mapping","object":"response","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	}))
	defer upstreamServer.Close()

	groupID := int64(4203)
	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: service.Account{
		ID:          9903,
		Name:        "grok-channel-mapped-account",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"base_url":      upstreamServer.URL,
			"model_mapping": map[string]any{"grok-4.5": "grok-4.5"},
		},
		Extra: map[string]any{"openai_passthrough": true},
	}}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{
			ID:           7703,
			Name:         "grok-text-channel",
			Status:       service.StatusActive,
			GroupIDs:     []int64{groupID},
			ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {"*": "grok-4.5"}},
		}},
		groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
	}, nil, nil, nil)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, repository.NewHTTPUpstream(cfg),
		&service.DeferredService{}, nil, nil, channelSvc, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := &OpenAIGatewayHandler{
		gatewayService: gatewaySvc, billingCacheService: billingCacheSvc, apiKeyService: &service.APIKeyService{},
		concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}
	apiKey := &service.APIKey{
		ID: 1803, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
		User:  &service.User{ID: 1703, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.POST("/openai/v1/responses", h.Responses)

	testCases := []struct {
		name         string
		body         string
		accountExtra map[string]any
	}{
		{
			name:         "ordinary passthrough request",
			body:         `{"model":"gpt-5.6-luna","input":"reply OK","stream":false}`,
			accountExtra: map[string]any{"openai_passthrough": true},
		},
		{
			name:         "function call output continuation",
			body:         `{"model":"gpt-5.6-luna","stream":false,"input":[{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
			accountExtra: map[string]any{"openai_responses_supported": true},
		},
		{
			name:         "function call output continuation with full body reserialization",
			body:         `{"model":"gpt-5.6-luna","stream":false,"reasoning":{"effort":"minimal"},"input":[{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
			accountExtra: map[string]any{"openai_responses_supported": true},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			accountRepo.account.Extra = testCase.accountExtra
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(testCase.body))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			select {
			case upstream := <-upstreamRequest:
				require.Equal(t, "/v1/responses", upstream.path)
				require.Equal(t, "Bearer sk-test", upstream.authorization)
				require.Empty(t, upstream.grokClientVersion)
				require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.payload, "model").String())
			case <-time.After(3 * time.Second):
				t.Fatal("等待渠道映射后的 HTTP 上游请求超时")
			}
			select {
			case usageLog := <-usageRepo.created:
				require.Equal(t, int64(9903), usageLog.AccountID)
				require.Equal(t, "gpt-5.6-luna", usageLog.RequestedModel)
				require.NotNil(t, usageLog.ModelMappingChain)
				require.Equal(t, "gpt-5.6-luna→grok-4.5", *usageLog.ModelMappingChain)
			case <-time.After(3 * time.Second):
				t.Fatal("等待渠道映射后的 HTTP usage log 超时")
			}
		})
	}
}

func TestOpenAIResponsesWebSocket_TextModelNativeImageToolUsesHTTPFallbackWhenGroupAllows(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":true,"input":"draw a green square","tools":[{"type":"image_generation","model":"gpt-image-2"}],"tool_choice":{"type":"image_generation"}}`,
		userAgent:    testStringPtr("Codex Desktop/0.144.0-alpha.4 Windows"),
	})

	require.Equal(t, "gpt-5.4", gjson.GetBytes(got.upstreamFirstPayload, "model").String())
	require.Equal(t, "image_generation", gjson.GetBytes(got.upstreamFirstPayload, "tools.0.type").String())
	require.Equal(t, "gpt-image-2", gjson.GetBytes(got.upstreamFirstPayload, "tools.0.model").String())
	require.Equal(t, 1, got.log.ImageCount)
	require.NotNil(t, got.log.BillingMode)
	require.Equal(t, string(service.BillingModeImage), *got.log.BillingMode)
}

func TestOpenAIResponsesWebSocket_NativeImageHTTPFailureDoesNotRetryWebSocket(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:       `{"type":"response.create","model":"gpt-5.4","stream":true,"input":"draw a green square","tools":[{"type":"image_generation","model":"gpt-image-2"}],"tool_choice":{"type":"image_generation"}}`,
		userAgent:          testStringPtr("Codex Desktop/0.144.0-alpha.4 Windows"),
		upstreamHTTPStatus: http.StatusBadGateway,
	})

	require.NotNil(t, got.closeErr)
	require.Equal(t, coderws.StatusInternalError, got.closeErr.Code)
	require.Equal(t, int32(1), got.upstreamHTTPHits, "原生生图失败时只能调用一次 HTTP 上游")
	require.Zero(t, got.upstreamWSHits, "HTTP 生图失败后不得再次请求 WebSocket 上游")
}

func TestOpenAIResponsesWebSocket_HostedImageCanSelectHTTPOnlyAccount(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:    `{"type":"response.create","model":"gpt-5.4","stream":true,"input":"draw","tools":[{"type":"image_generation","model":"gpt-image-2"}],"tool_choice":{"type":"image_generation"}}`,
		httpOnlyAccount: true,
	})

	require.Equal(t, "image_generation", gjson.GetBytes(got.upstreamFirstPayload, "tool_choice.type").String())
	require.Equal(t, 1, got.log.ImageCount)
}

func TestOpenAIResponsesWebSocket_PreviousResponseDoesNotSelectHTTPOnlyAccount(t *testing.T) {
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:           `{"type":"response.create","model":"gpt-5.4","stream":true,"previous_response_id":"resp_previous","input":"edit","tools":[{"type":"image_generation","model":"gpt-image-2"}],"tool_choice":{"type":"image_generation"}}`,
		httpOnlyAccount:        true,
		expectSelectionFailure: true,
	})
}

func TestOpenAIResponses_CodexImageBridgeFallsBackToHTTPAndRoutesRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamPayload := make(chan []byte, 8)
	streamProbeStarted := make(chan struct{})
	streamProbeRelease := make(chan struct{})
	var streamProbeReleaseOnce sync.Once
	releaseStreamProbe := func() { streamProbeReleaseOnce.Do(func() { close(streamProbeRelease) }) }
	defer releaseStreamProbe()
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		upstreamPayload <- payload
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(gjson.GetBytes(payload, "input").String(), "stream-probe") {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"ig_stream_probe\",\"type\":\"image_generation_call\",\"status\":\"in_progress\"}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(streamProbeStarted)
			<-streamProbeRelease
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_stream_probe\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_probe\",\"model\":\"gpt-image-2\",\"output\":[{\"id\":\"ig_stream_probe\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"output_tokens_details\":{\"image_tokens\":1}}}}\n\n")
			return
		}
		turnMetadata := gjson.GetBytes(payload, "client_metadata.x-codex-turn-metadata").String()
		if gjson.Get(turnMetadata, "request_kind").String() == "prewarm" {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_prewarm_e2e\",\"model\":\"gpt-5.6-terra\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ready\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			return
		}
		if strings.Contains(gjson.GetBytes(payload, "input").String(), "legacy text request") {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_legacy_text_e2e\",\"model\":\"gpt-5.5\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"legacy text answer\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
			return
		}
		if strings.Contains(string(payload), "image generation failed: invalid response") ||
			strings.Contains(string(payload), "change the previous image to green") {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_continuation_text_e2e\",\"model\":\"gpt-5.6-terra\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"handled by text orchestrator\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
			return
		}
		if gjson.GetBytes(payload, "tools.0.name").String() == "imagegen" {
			if strings.Contains(string(payload), "explain image APIs without generating") ||
				strings.Contains(string(payload), "nonstream text request") {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_text_e2e\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"text-only answer\"}]}}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_text_e2e\",\"model\":\"gpt-5.4\",\"output\":[{\"id\":\"msg_text_e2e\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"text-only answer\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
				return
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_http_e2e\",\"type\":\"function_call\",\"name\":\"imagegen\",\"call_id\":\"call_http_e2e\",\"arguments\":\"{}\"}}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_extension_e2e\",\"model\":\"gpt-5.4\",\"output\":[{\"id\":\"fc_http_e2e\",\"type\":\"function_call\",\"name\":\"imagegen\",\"call_id\":\"call_http_e2e\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_http_e2e\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http_e2e\",\"model\":\"gpt-5.4\",\"output\":[{\"id\":\"ig_http_e2e\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"output_tokens_details\":{\"image_tokens\":1}}}}\n\n")
	}))
	defer upstreamServer.Close()

	groupID := int64(4202)
	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: service.Account{
		ID: 9902, Name: "openai-http-hosted-image-e2e", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": upstreamServer.URL},
		Extra:       map[string]any{"openai_passthrough": true},
	}}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{
			ID: 7702, Name: "openai-http-image-channel", Status: service.StatusActive,
			GroupIDs: []int64{groupID},
			ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {
				"gpt-5.6-terra": "gpt-image-2",
				"gpt-5.5":       "gpt-image-2",
			}},
			FeaturesConfig: map[string]any{"codex_image_generation_bridge": map[string]any{
				service.PlatformOpenAI: true, "orchestrator_group_id": int64(20),
			}},
		}},
		groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
	}, nil, nil, nil)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, repository.NewHTTPUpstream(cfg),
		&service.DeferredService{}, nil, nil, channelSvc, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := &OpenAIGatewayHandler{
		gatewayService: gatewaySvc, billingCacheService: billingCacheSvc, apiKeyService: &service.APIKeyService{},
		concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}
	apiKey := &service.APIKey{
		ID: 1802, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowImageGeneration: true},
		User:  &service.User{ID: 1702, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	router.POST("/openai/v1/responses", h.Responses)

	// Codex first attempts WebSocket. A channel-managed image group must reject
	// the upgrade before accepting it so the client can natively retry over HTTP.
	upgradeReq := httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	upgradeReq.Header.Set("Connection", "Upgrade")
	upgradeReq.Header.Set("Upgrade", "websocket")
	upgradeReq.Header.Set("User-Agent", "Codex Desktop/0.144.0-alpha.4 Windows")
	upgradeRecorder := httptest.NewRecorder()
	router.ServeHTTP(upgradeRecorder, upgradeReq)
	require.Equal(t, http.StatusUpgradeRequired, upgradeRecorder.Code)
	require.Contains(t, upgradeRecorder.Body.String(), "Codex image channels require HTTPS Responses transport")
	select {
	case <-upstreamPayload:
		require.Fail(t, "WebSocket handshake rejection must not call an upstream account")
	default:
	}

	metadata := `{"request_kind":"turn","thread_source":"user"}`
	body := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"draw a blue paper airplane","tools":[{"type":"image_generation"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Codex Desktop/0.144.0-alpha.4 Windows")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"type":"image_generation_call"`)
	forwarded := <-upstreamPayload
	require.Equal(t, "gpt-image-2", gjson.GetBytes(forwarded, "model").String())
	require.Equal(t, "image_generation", gjson.GetBytes(forwarded, "tools.0.type").String())
	usageLog := <-usageRepo.created
	require.Equal(t, 1, usageLog.ImageCount)
	require.Equal(t, int64(9902), usageLog.AccountID, "真实生图必须使用 API Key 原图片分组账号")
	require.NotNil(t, usageLog.BillingMode)
	require.Equal(t, string(service.BillingModeImage), *usageLog.BillingMode)

	extensionBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"draw another airplane","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`)
	extensionReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(extensionBody))
	extensionReq.Header.Set("Content-Type", "application/json")
	extensionReq.Header.Set("User-Agent", "Codex Desktop/0.144.0-alpha.4 Windows")
	extensionRecorder := httptest.NewRecorder()
	router.ServeHTTP(extensionRecorder, extensionReq)
	require.Equal(t, http.StatusOK, extensionRecorder.Code)
	require.Contains(t, extensionRecorder.Body.String(), `"namespace":"image_gen"`)
	require.Contains(t, extensionRecorder.Body.String(), `"name":"imagegen"`)
	extensionForwarded := <-upstreamPayload
	require.Equal(t, "gpt-5.6-terra", gjson.GetBytes(extensionForwarded, "model").String())
	require.Equal(t, "function", gjson.GetBytes(extensionForwarded, "tools.0.type").String())
	require.Equal(t, "imagegen", gjson.GetBytes(extensionForwarded, "tools.0.name").String())
	require.Equal(t, "auto", gjson.GetBytes(extensionForwarded, "tool_choice").String())

	liteBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"draw via latest metadata-only request"}]}]}`)
	liteReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(liteBody))
	liteReq.Header.Set("Content-Type", "application/json")
	liteRecorder := httptest.NewRecorder()
	router.ServeHTTP(liteRecorder, liteReq)
	require.Equal(t, http.StatusOK, liteRecorder.Code)
	require.Contains(t, liteRecorder.Body.String(), `"namespace":"image_gen"`)
	require.Contains(t, liteRecorder.Body.String(), `"name":"imagegen"`)
	liteForwarded := <-upstreamPayload
	require.Equal(t, "gpt-5.6-terra", gjson.GetBytes(liteForwarded, "model").String())
	require.Equal(t, "function", gjson.GetBytes(liteForwarded, "tools.0.type").String())
	require.Equal(t, "imagegen", gjson.GetBytes(liteForwarded, "tools.0.name").String())
	require.Equal(t, "auto", gjson.GetBytes(liteForwarded, "tool_choice").String())
	require.False(t, gjson.GetBytes(liteForwarded, `input.#(type=="additional_tools")`).Exists())

	// Once Codex has received a generated image, its local extension sends the
	// image back in a continuation request. Completing this turn locally avoids
	// uploading several megabytes to a text model and creating a second charge.
	largeImage := strings.Repeat("A", 3*1024*1024)
	continuationInput := `[{"type":"function_call","namespace":"image_gen","name":"imagegen","call_id":"call_completed"},{"type":"function_call_output","call_id":"call_completed","output":[{"type":"input_image","image_url":"data:image/png;base64,` + largeImage + `"},{"type":"input_text","text":"saved"}]}]`
	continuationBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":` + continuationInput + `}`)
	continuationReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(continuationBody))
	continuationReq.Header.Set("Content-Type", "application/json")
	continuationReq.Header.Set("User-Agent", "Codex Desktop/0.144.5 Windows")
	continuationRecorder := httptest.NewRecorder()
	router.ServeHTTP(continuationRecorder, continuationReq)
	require.Equal(t, http.StatusOK, continuationRecorder.Code)
	require.Contains(t, continuationRecorder.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, continuationRecorder.Body.String(), "event: response.completed")
	require.Contains(t, continuationRecorder.Body.String(), `"status":"completed"`)
	require.Contains(t, continuationRecorder.Body.String(), `"output":[]`)
	require.Contains(t, continuationRecorder.Body.String(), `"total_tokens":0`)
	require.Less(t, continuationRecorder.Body.Len(), 2048, "local completion must not echo image bytes")
	select {
	case unexpectedPayload := <-upstreamPayload:
		require.Fail(t, "successful image continuation must not call text upstream", "payload_bytes=%d", len(unexpectedPayload))
	default:
	}
	select {
	case unexpectedUsage := <-usageRepo.created:
		require.Fail(t, "successful image continuation must not create token usage", "usage=%+v", unexpectedUsage)
	default:
	}

	nonStreamContinuationBody := []byte(`{"model":"gpt-5.6-terra","stream":false,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":` + continuationInput + `}`)
	nonStreamContinuationReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(nonStreamContinuationBody))
	nonStreamContinuationReq.Header.Set("Content-Type", "application/json")
	nonStreamContinuationRecorder := httptest.NewRecorder()
	router.ServeHTTP(nonStreamContinuationRecorder, nonStreamContinuationReq)
	require.Equal(t, http.StatusOK, nonStreamContinuationRecorder.Code)
	require.Contains(t, nonStreamContinuationRecorder.Header().Get("Content-Type"), "application/json")
	require.Equal(t, "response", gjson.GetBytes(nonStreamContinuationRecorder.Body.Bytes(), "object").String())
	require.Equal(t, "completed", gjson.GetBytes(nonStreamContinuationRecorder.Body.Bytes(), "status").String())
	require.Empty(t, gjson.GetBytes(nonStreamContinuationRecorder.Body.Bytes(), "output").Array())
	require.Zero(t, gjson.GetBytes(nonStreamContinuationRecorder.Body.Bytes(), "usage.total_tokens").Int())
	select {
	case unexpectedPayload := <-upstreamPayload:
		require.Fail(t, "non-stream image continuation must not call text upstream", "payload_bytes=%d", len(unexpectedPayload))
	default:
	}

	previousResponseContinuationBody := []byte(`{"model":"gpt-5.6-terra","stream":false,"previous_response_id":"resp_completed","client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":[{"type":"function_call_output","call_id":"call_completed","output":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)
	previousResponseContinuationReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(previousResponseContinuationBody))
	previousResponseContinuationReq.Header.Set("Content-Type", "application/json")
	previousResponseContinuationRecorder := httptest.NewRecorder()
	router.ServeHTTP(previousResponseContinuationRecorder, previousResponseContinuationReq)
	require.Equal(t, http.StatusOK, previousResponseContinuationRecorder.Code)
	require.Equal(t, "completed", gjson.GetBytes(previousResponseContinuationRecorder.Body.Bytes(), "status").String())
	select {
	case unexpectedPayload := <-upstreamPayload:
		require.Fail(t, "successful previous-response image continuation must not call text upstream", "payload_bytes=%d", len(unexpectedPayload))
	default:
	}

	failedContinuationBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":[{"type":"function_call","namespace":"image_gen","name":"imagegen","call_id":"call_failed"},{"type":"function_call_output","call_id":"call_failed","output":"image generation failed: invalid response"}]}`)
	failedContinuationReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(failedContinuationBody))
	failedContinuationReq.Header.Set("Content-Type", "application/json")
	failedContinuationRecorder := httptest.NewRecorder()
	router.ServeHTTP(failedContinuationRecorder, failedContinuationReq)
	require.Equal(t, http.StatusOK, failedContinuationRecorder.Code)
	failedContinuationForwarded := <-upstreamPayload
	require.Contains(t, string(failedContinuationForwarded), "image generation failed: invalid response")
	failedContinuationUsage := <-usageRepo.created
	require.Zero(t, failedContinuationUsage.ImageCount)

	laterEditBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":[{"type":"function_call","namespace":"image_gen","name":"imagegen","call_id":"call_edit"},{"type":"function_call_output","call_id":"call_edit","output":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"change the previous image to green"}]}]}`)
	laterEditReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(laterEditBody))
	laterEditReq.Header.Set("Content-Type", "application/json")
	laterEditRecorder := httptest.NewRecorder()
	router.ServeHTTP(laterEditRecorder, laterEditReq)
	require.Equal(t, http.StatusOK, laterEditRecorder.Code)
	laterEditForwarded := <-upstreamPayload
	require.Contains(t, string(laterEditForwarded), "change the previous image to green")
	laterEditUsage := <-usageRepo.created
	require.Zero(t, laterEditUsage.ImageCount)

	missingCapabilityBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"draw without a declared image capability"}`)
	missingCapabilityReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(missingCapabilityBody))
	missingCapabilityReq.Header.Set("Content-Type", "application/json")
	missingCapabilityRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingCapabilityRecorder, missingCapabilityReq)
	require.Equal(t, http.StatusOK, missingCapabilityRecorder.Code)
	require.Contains(t, missingCapabilityRecorder.Body.String(), `"namespace":"image_gen"`)
	require.Contains(t, missingCapabilityRecorder.Body.String(), `"name":"imagegen"`)
	missingCapabilityForwarded := <-upstreamPayload
	require.Equal(t, "gpt-5.6-terra", gjson.GetBytes(missingCapabilityForwarded, "model").String())
	require.Equal(t, "function", gjson.GetBytes(missingCapabilityForwarded, "tools.0.type").String())
	require.Equal(t, "imagegen", gjson.GetBytes(missingCapabilityForwarded, "tools.0.name").String())
	select {
	case unexpectedUsage := <-usageRepo.created:
		require.Fail(t, "local image_gen orchestration must not be billed as a generated image", "usage=%+v", unexpectedUsage)
	default:
	}

	// An image-capable Codex turn that has no generation intent must remain a
	// normal text response and must be recorded through the ordinary text path.
	textBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"explain image APIs without generating","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`)
	textReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(textBody))
	textReq.Header.Set("Content-Type", "application/json")
	textRecorder := httptest.NewRecorder()
	router.ServeHTTP(textRecorder, textReq)
	require.Equal(t, http.StatusOK, textRecorder.Code)
	require.Contains(t, textRecorder.Body.String(), "text-only answer")
	require.NotContains(t, textRecorder.Body.String(), `"name":"imagegen"`)
	textForwarded := <-upstreamPayload
	require.Equal(t, "auto", gjson.GetBytes(textForwarded, "tool_choice").String())
	require.Contains(t, gjson.GetBytes(textForwarded, "instructions").String(), "only when the user's actual intent")
	textUsage := <-usageRepo.created
	require.Zero(t, textUsage.ImageCount)
	require.NotNil(t, textUsage.BillingMode)
	require.Equal(t, string(service.BillingModeToken), *textUsage.BillingMode)
	require.Nil(t, textUsage.ChannelID)
	require.Nil(t, textUsage.ModelMappingChain)

	// The stream=false path aggregates upstream SSE into one JSON response. It
	// must make the same intent and billing decision as the streaming path.
	nonStreamTextBody := []byte(`{"model":"gpt-5.6-terra","stream":false,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"nonstream text request","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`)
	nonStreamTextReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(nonStreamTextBody))
	nonStreamTextReq.Header.Set("Content-Type", "application/json")
	nonStreamTextRecorder := httptest.NewRecorder()
	router.ServeHTTP(nonStreamTextRecorder, nonStreamTextReq)
	require.Equal(t, http.StatusOK, nonStreamTextRecorder.Code)
	require.Contains(t, nonStreamTextRecorder.Body.String(), "text-only answer")
	require.NotContains(t, nonStreamTextRecorder.Body.String(), `"name":"imagegen"`)
	nonStreamTextForwarded := <-upstreamPayload
	require.False(t, gjson.GetBytes(nonStreamTextForwarded, "stream").Bool())
	nonStreamTextUsage := <-usageRepo.created
	require.Zero(t, nonStreamTextUsage.ImageCount)
	require.NotNil(t, nonStreamTextUsage.BillingMode)
	require.Equal(t, string(service.BillingModeToken), *nonStreamTextUsage.BillingMode)
	require.Nil(t, nonStreamTextUsage.ChannelID)

	nonStreamImageBody := []byte(`{"model":"gpt-5.6-terra","stream":false,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"generate a nonstream blue circle","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`)
	nonStreamImageReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(nonStreamImageBody))
	nonStreamImageReq.Header.Set("Content-Type", "application/json")
	nonStreamImageRecorder := httptest.NewRecorder()
	router.ServeHTTP(nonStreamImageRecorder, nonStreamImageReq)
	require.Equal(t, http.StatusOK, nonStreamImageRecorder.Code)
	require.Equal(t, "image_gen", gjson.GetBytes(nonStreamImageRecorder.Body.Bytes(), "output.0.namespace").String())
	require.Equal(t, "imagegen", gjson.GetBytes(nonStreamImageRecorder.Body.Bytes(), "output.0.name").String())
	<-upstreamPayload
	select {
	case unexpectedUsage := <-usageRepo.created:
		require.Fail(t, "nonstream imagegen orchestration must not create token usage", "usage=%+v", unexpectedUsage)
	case <-time.After(100 * time.Millisecond):
	}

	// Background/prewarm turns still use their original text model after the
	// client has fallen back to HTTP; they must not be converted into image work.
	prewarmMetadata := `{"request_kind":"prewarm","thread_source":"user"}`
	prewarmBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(prewarmMetadata) + `},"input":"warm"}`)
	prewarmReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(prewarmBody))
	prewarmReq.Header.Set("Content-Type", "application/json")
	prewarmRecorder := httptest.NewRecorder()
	router.ServeHTTP(prewarmRecorder, prewarmReq)
	require.Equal(t, http.StatusOK, prewarmRecorder.Code)
	require.NotContains(t, prewarmRecorder.Body.String(), `"type":"image_generation_call"`)
	prewarmForwarded := <-upstreamPayload
	require.Equal(t, "gpt-5.6-terra", gjson.GetBytes(prewarmForwarded, "model").String())
	require.Empty(t, gjson.GetBytes(prewarmForwarded, `tools.#(type=="image_generation")#`).Array())
	prewarmUsage := <-usageRepo.created
	require.Zero(t, prewarmUsage.ImageCount)
	require.Nil(t, prewarmUsage.ChannelID)
	require.Nil(t, prewarmUsage.ModelMappingChain)

	// Older Codex image turns explicitly advertise image_generation and retain
	// the hosted image contract.
	legacyBody := []byte(`{"model":"gpt-5.5","stream":true,"input":"draw a red circle","tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	legacyReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(legacyBody))
	legacyReq.Header.Set("Content-Type", "application/json")
	legacyReq.Header.Set("User-Agent", "codex_cli_rs/0.90.0")
	legacyRecorder := httptest.NewRecorder()
	router.ServeHTTP(legacyRecorder, legacyReq)
	require.Equal(t, http.StatusOK, legacyRecorder.Code)
	require.Contains(t, legacyRecorder.Body.String(), `"type":"image_generation_call"`)
	legacyForwarded := <-upstreamPayload
	require.Equal(t, "gpt-image-2", gjson.GetBytes(legacyForwarded, "model").String())
	require.Equal(t, "image_generation", gjson.GetBytes(legacyForwarded, "tools.0.type").String())
	legacyUsage := <-usageRepo.created
	require.Equal(t, int64(9902), legacyUsage.AccountID)
	require.Equal(t, 1, legacyUsage.ImageCount)
	require.NotNil(t, legacyUsage.BillingMode)
	require.Equal(t, string(service.BillingModeImage), *legacyUsage.BillingMode)

	// Older Codex text turns do not carry canonical metadata or an image tool.
	// They must preserve the requested text model and normal text accounting.
	legacyTextBody := []byte(`{"model":"gpt-5.5","stream":true,"input":"legacy text request"}`)
	legacyTextReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(legacyTextBody))
	legacyTextReq.Header.Set("Content-Type", "application/json")
	legacyTextReq.Header.Set("User-Agent", "codex_cli_rs/0.90.0")
	legacyTextRecorder := httptest.NewRecorder()
	router.ServeHTTP(legacyTextRecorder, legacyTextReq)
	require.Equal(t, http.StatusOK, legacyTextRecorder.Code)
	require.Contains(t, legacyTextRecorder.Body.String(), "legacy text answer")
	require.NotContains(t, legacyTextRecorder.Body.String(), `"type":"image_generation_call"`)
	legacyTextForwarded := <-upstreamPayload
	require.Equal(t, "gpt-5.5", gjson.GetBytes(legacyTextForwarded, "model").String())
	require.Empty(t, gjson.GetBytes(legacyTextForwarded, "tools").Array())
	legacyTextUsage := <-usageRepo.created
	require.Zero(t, legacyTextUsage.ImageCount)
	require.NotNil(t, legacyTextUsage.BillingMode)
	require.Equal(t, string(service.BillingModeToken), *legacyTextUsage.BillingMode)
	require.Nil(t, legacyTextUsage.ChannelID)
	require.Nil(t, legacyTextUsage.ModelMappingChain)

	// The HTTP fallback must expose upstream SSE incrementally. This guards
	// against reintroducing the buffered HTTP-to-WebSocket adapter that caused
	// Codex to sit idle and cancel long-running image requests.
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()
	streamBody := []byte(`{"model":"gpt-5.6-terra","stream":true,"client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(metadata) + `},"input":"stream-probe","tools":[{"type":"image_generation"}]}`)
	streamReq, err := http.NewRequest(http.MethodPost, handlerServer.URL+"/openai/v1/responses", bytes.NewReader(streamBody))
	require.NoError(t, err)
	streamReq.Header.Set("Content-Type", "application/json")
	streamReq.Header.Set("User-Agent", "Codex Desktop/0.144.0-alpha.4 Windows")
	streamResp, err := handlerServer.Client().Do(streamReq)
	require.NoError(t, err)
	defer func() { _ = streamResp.Body.Close() }()
	require.Equal(t, http.StatusOK, streamResp.StatusCode)
	select {
	case <-streamProbeStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "upstream did not start the streaming probe")
	}
	reader := bufio.NewReader(streamResp.Body)
	firstLine, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, firstLine, `"type":"response.output_item.added"`, "first SSE event must reach Codex before image completion")
	releaseStreamProbe()
	remainder, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(remainder), `"type":"image_generation_call"`)
	streamForwarded := <-upstreamPayload
	require.Equal(t, "gpt-image-2", gjson.GetBytes(streamForwarded, "model").String())
	streamUsage := <-usageRepo.created
	require.Equal(t, int64(9902), streamUsage.AccountID)
	require.Equal(t, 1, streamUsage.ImageCount)
}

func TestOpenAIResponsesWebSocket_RejectsImageOnlyModel(t *testing.T) {
	event, closeErr := runOpenAIResponsesWebSocketUnsupportedImageCase(t, openAIResponsesWSUnsupportedImageCase{
		firstPayload: `{"type":"response.create","model":"gpt-image-2","stream":true,"input":"draw a candy on white background"}`,
		userAgent:    testStringPtr("Codex Desktop test"),
	})

	assertOpenAIWSImageUnsupportedEvent(t, event, "gpt-image-2")
	require.NotNil(t, closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
}

func TestOpenAIResponsesWebSocket_RejectsChannelMappedImageModelWithoutExplicitTool(t *testing.T) {
	event, closeErr := runOpenAIResponsesWebSocketUnsupportedImageCase(t, openAIResponsesWSUnsupportedImageCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.5","stream":true,"input":"draw a candy on white background"}`,
		userAgent:    testStringPtr("Codex Desktop test"),
		channelMapping: map[string]string{
			"gpt-5.5": "gpt-image-2",
		},
	})

	assertOpenAIWSImageUnsupportedEvent(t, event, "gpt-5.5")
	require.NotNil(t, closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
}

func TestOpenAIResponsesWebSocket_ImageGenerationOnFollowupTurnRequiresReconnect(t *testing.T) {
	got := runOpenAIResponsesWebSocketFollowupUnsupportedImageCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":true,"input":"first text turn"}`,
		nextPayload:  `{"type":"response.create","model":"gpt-5.4","stream":true,"previous_response_id":"resp_usage_e2e","tools":[{"type":"image_generation"}],"input":"draw another candy"}`,
		userAgent:    testStringPtr("Codex Desktop test"),
	})

	require.Empty(t, got.event)
	require.Error(t, got.readErr)
	if got.closeErr != nil {
		require.Equal(t, coderws.StatusTryAgainLater, got.closeErr.Code)
		require.Equal(t, "request route changed; reconnect and retry", got.closeErr.Reason)
	} else {
		require.ErrorIs(t, got.readErr, io.EOF)
	}
	require.Equal(t, int32(1), got.upstreamHits, "第二轮图片请求切换 HTTP 路径前不得复用原 WS 上游")
}

func TestOpenAIResponsesWebSocket_PassthroughUsageLogLeavesUserAgentNilWhenMissing(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":false,"reasoning":{"effort":"medium"}}`,
		userAgent:    testStringPtr(""),
	})

	require.Nil(t, got.log.UserAgent, "空入站 User-Agent 不应由上游握手 UA 或默认 UA 兜底")
	require.NotNil(t, got.log.ReasoningEffort)
	require.Equal(t, "medium", *got.log.ReasoningEffort)
}

func TestSetOpenAIClientTransportHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	setOpenAIClientTransportHTTP(c)
	require.Equal(t, service.OpenAIClientTransportHTTP, service.GetOpenAIClientTransport(c))
}

func TestSetOpenAIClientTransportWS(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	setOpenAIClientTransportWS(c)
	require.Equal(t, service.OpenAIClientTransportWS, service.GetOpenAIClientTransport(c))
}

// TestOpenAIHandler_GjsonExtraction 验证 gjson 从请求体中提取 model/stream 的正确性
func TestOpenAIHandler_GjsonExtraction(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModel  string
		wantStream bool
	}{
		{"正常提取", `{"model":"gpt-4","stream":true,"input":"hello"}`, "gpt-4", true},
		{"stream false", `{"model":"gpt-4","stream":false}`, "gpt-4", false},
		{"无 stream 字段", `{"model":"gpt-4"}`, "gpt-4", false},
		{"model 缺失", `{"stream":true}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			modelResult := gjson.GetBytes(body, "model")
			model := ""
			if modelResult.Type == gjson.String {
				model = modelResult.String()
			}
			stream := gjson.GetBytes(body, "stream").Bool()
			require.Equal(t, tt.wantModel, model)
			require.Equal(t, tt.wantStream, stream)
		})
	}
}

// TestOpenAIHandler_GjsonValidation 验证修复后的 JSON 合法性和类型校验
func TestOpenAIHandler_GjsonValidation(t *testing.T) {
	// 非法 JSON 被 gjson.ValidBytes 拦截
	require.False(t, gjson.ValidBytes([]byte(`{invalid json`)))

	// model 为数字 → 类型不是 gjson.String，应被拒绝
	body := []byte(`{"model":123}`)
	modelResult := gjson.GetBytes(body, "model")
	require.True(t, modelResult.Exists())
	require.NotEqual(t, gjson.String, modelResult.Type)

	// model 为 null → 类型不是 gjson.String，应被拒绝
	body2 := []byte(`{"model":null}`)
	modelResult2 := gjson.GetBytes(body2, "model")
	require.True(t, modelResult2.Exists())
	require.NotEqual(t, gjson.String, modelResult2.Type)

	// stream 为 string → 类型既不是 True 也不是 False，应被拒绝
	body3 := []byte(`{"model":"gpt-4","stream":"true"}`)
	streamResult := gjson.GetBytes(body3, "stream")
	require.True(t, streamResult.Exists())
	require.NotEqual(t, gjson.True, streamResult.Type)
	require.NotEqual(t, gjson.False, streamResult.Type)

	// stream 为 int → 同上
	body4 := []byte(`{"model":"gpt-4","stream":1}`)
	streamResult2 := gjson.GetBytes(body4, "stream")
	require.True(t, streamResult2.Exists())
	require.NotEqual(t, gjson.True, streamResult2.Type)
	require.NotEqual(t, gjson.False, streamResult2.Type)
}

// TestOpenAIHandler_InstructionsInjection 验证 instructions 的 gjson/sjson 注入逻辑
func TestOpenAIHandler_InstructionsInjection(t *testing.T) {
	// 测试 1：无 instructions → 注入
	body := []byte(`{"model":"gpt-4"}`)
	existing := gjson.GetBytes(body, "instructions").String()
	require.Empty(t, existing)
	newBody, err := sjson.SetBytes(body, "instructions", "test instruction")
	require.NoError(t, err)
	require.Equal(t, "test instruction", gjson.GetBytes(newBody, "instructions").String())

	// 测试 2：已有 instructions → 不覆盖
	body2 := []byte(`{"model":"gpt-4","instructions":"existing"}`)
	existing2 := gjson.GetBytes(body2, "instructions").String()
	require.Equal(t, "existing", existing2)

	// 测试 3：空白 instructions → 注入
	body3 := []byte(`{"model":"gpt-4","instructions":"   "}`)
	existing3 := strings.TrimSpace(gjson.GetBytes(body3, "instructions").String())
	require.Empty(t, existing3)

	// 测试 4：sjson.SetBytes 返回错误时不应 panic
	// 正常 JSON 不会产生 sjson 错误，验证返回值被正确处理
	validBody := []byte(`{"model":"gpt-4"}`)
	result, setErr := sjson.SetBytes(validBody, "instructions", "hello")
	require.NoError(t, setErr)
	require.True(t, gjson.ValidBytes(result))
}

func newOpenAIHandlerForPreviousResponseIDValidation(t *testing.T, cache *concurrencyCacheMock) *OpenAIGatewayHandler {
	t.Helper()
	if cache == nil {
		cache = &concurrencyCacheMock{
			acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
				return true, nil
			},
			acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
				return true, nil
			},
		}
	}
	return &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}
}

func newOpenAIWSHandlerTestServer(t *testing.T, h *OpenAIGatewayHandler, subject middleware.AuthSubject) *httptest.Server {
	t.Helper()
	groupID := int64(2)
	apiKey := &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: subject.UserID},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), subject)
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	return httptest.NewServer(router)
}

type openAIResponsesWSUsageLogCase struct {
	firstPayload           string
	nextPayload            string
	userAgent              *string
	turnMetadataHeader     *string
	channelMapping         map[string]string
	accountModelMapping    map[string]any
	responsesSupported     *bool
	allowImageGeneration   *bool
	codexBridge            bool
	httpOnlyAccount        bool
	upstreamHTTPStatus     int
	expectSelectionFailure bool
}

type openAIResponsesWSUsageLogResult struct {
	log                   *service.UsageLog
	upstreamFirstPayload  []byte
	upstreamSecondPayload []byte
	closeErr              *coderws.CloseError
	upstreamHTTPHits      int32
	upstreamWSHits        int32
}

type openAIResponsesWSUnsupportedImageCase struct {
	firstPayload   string
	userAgent      *string
	channelMapping map[string]string
	codexBridge    bool
}

type openAIResponsesWSFollowupUnsupportedImageResult struct {
	event        []byte
	readErr      error
	closeErr     *coderws.CloseError
	upstreamHits int32
}

type openAIWSUsageHandlerAccountRepoStub struct {
	service.AccountRepository
	account service.Account
}

func (s *openAIWSUsageHandlerAccountRepoStub) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	if s.account.Platform != platform {
		return nil, nil
	}
	return []service.Account{s.account}, nil
}

func (s *openAIWSUsageHandlerAccountRepoStub) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *openAIWSUsageHandlerAccountRepoStub) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	if s.account.ID != id {
		return nil, nil
	}
	account := s.account
	return &account, nil
}

type openAIWSSilentRetryAccountRepoStub struct {
	service.AccountRepository
	accounts []service.Account
}

func (s *openAIWSSilentRetryAccountRepoStub) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	accounts := make([]service.Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		if account.Platform == platform {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (s *openAIWSSilentRetryAccountRepoStub) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *openAIWSSilentRetryAccountRepoStub) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	for _, account := range s.accounts {
		if account.ID == id {
			accountCopy := account
			return &accountCopy, nil
		}
	}
	return nil, nil
}

type openAIWSUsageHandlerUsageLogRepoStub struct {
	service.UsageLogRepository
	created chan *service.UsageLog
}

func (s *openAIWSUsageHandlerUsageLogRepoStub) Create(ctx context.Context, log *service.UsageLog) (bool, error) {
	if s.created != nil {
		s.created <- log
	}
	return true, nil
}

func TestOpenAIResponsesWebSocket_SilentRetrySwitchesAccountBeforeDownstream(t *testing.T) {
	var failedHits int32
	failedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&failedHits, 1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer failedUpstream.Close()

	var successHits int32
	successUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&successHits, 1)
		conn, err := coderws.Accept(w, r, nil)
		require.NoError(t, err)
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		_, _, err = conn.Read(readCtx)
		cancelRead()
		require.NoError(t, err)

		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		err = conn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_switched","model":"gpt-5.4","usage":{"input_tokens":2,"output_tokens":1}}}`))
		cancelWrite()
		require.NoError(t, err)
		_ = conn.Close(coderws.StatusNormalClosure, "done")
	}))
	defer successUpstream.Close()

	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
	server := newOpenAIWSSilentRetryHandlerTestServer(t, []service.Account{
		newOpenAIWSSilentRetryAccount(9101, "ws-first-fails", failedUpstream.URL, 0),
		newOpenAIWSSilentRetryAccount(9102, "ws-second-succeeds", successUpstream.URL, 1),
	}, usageRepo)
	defer server.Close()

	clientConn := dialOpenAIWSTestClient(t, server.URL)
	defer func() { _ = clientConn.CloseNow() }()
	writeOpenAIWSTestPayload(t, clientConn, `{"type":"response.create","model":"gpt-5.4","input":"hello","stream":true}`)

	_, event, err := readOpenAIWSTestMessage(t, clientConn)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	require.Eventually(t, func() bool { return atomic.LoadInt32(&failedHits) == 1 }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&successHits) == 1 }, time.Second, 10*time.Millisecond)

	var usageLog *service.UsageLog
	select {
	case usageLog = <-usageRepo.created:
		require.NotNil(t, usageLog)
	case <-time.After(3 * time.Second):
		t.Fatal("等待 WebSocket usage log 写入超时")
	}
	require.Equal(t, int64(9102), usageLog.AccountID)
	select {
	case extra := <-usageRepo.created:
		t.Fatalf("unexpected duplicate usage log: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOpenAIImages_5xxSkipsSameAccountReplayAndFailsOver(t *testing.T) {
	var failedHits int32
	failedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&failedHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"synthetic bad gateway"}}`)
	}))
	defer failedUpstream.Close()

	var successHits int32
	successUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&successHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_second_image_account")
		_, _ = io.WriteString(w, `{"created":1710000000,"data":[{"b64_json":"aW1hZ2U="}]}`)
	}))
	defer successUpstream.Close()

	accounts := []service.Account{
		{
			ID: 9401, Name: "image-first-502", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
			Credentials: map[string]any{
				"api_key": "sk-first", "base_url": failedUpstream.URL + "/v1",
				"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"},
			},
		},
		{
			ID: 9402, Name: "image-second-success", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
			Credentials: map[string]any{
				"api_key": "sk-second", "base_url": successUpstream.URL + "/v1",
				"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"},
			},
		},
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.MaxAccountSwitches = 2
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSSilentRetryAccountRepoStub{accounts: accounts}, usageRepo,
		nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, repository.NewHTTPUpstream(cfg),
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := NewOpenAIGatewayHandler(
		gatewaySvc,
		service.NewConcurrencyService(cache),
		billingCacheSvc,
		&service.APIKeyService{},
		nil, nil, nil, cfg,
	)

	groupID := int64(4401)
	apiKey := &service.APIKey{
		ID: 2001, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowImageGeneration: true},
		User:  &service.User{ID: 2002, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.POST("/v1/images/generations", h.Images)

	body := []byte(`{"model":"gpt-image-2","prompt":"draw a cat","response_format":"b64_json"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "aW1hZ2U=", gjson.Get(recorder.Body.String(), "data.0.b64_json").String())
	require.Equal(t, int32(1), atomic.LoadInt32(&failedHits), "502 账号不得同账号重放")
	require.Equal(t, int32(1), atomic.LoadInt32(&successHits), "必须切换到第二个图片账号")
	select {
	case usageLog := <-usageRepo.created:
		require.Equal(t, int64(9402), usageLog.AccountID)
		require.Equal(t, 1, usageLog.ImageCount)
	case <-time.After(3 * time.Second):
		t.Fatal("等待图片故障切换 usage log 超时")
	}
}

type openAIImagesOAuthFailoverUpstream struct {
	failedAccountID  int64
	successAccountID int64
	failedHits       atomic.Int32
	successHits      atomic.Int32
}

func (u *openAIImagesOAuthFailoverUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	switch accountID {
	case u.failedAccountID:
		u.failedHits.Add(1)
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic bad gateway"}}`)),
		}, nil
	case u.successAccountID:
		u.successHits.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"X-Request-Id": []string{"req_second_oauth_image_account"},
			},
			Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1710000000,\"usage\":{\"input_tokens\":2,\"output_tokens\":1},\"tool_usage\":{\"image_gen\":{\"images\":1}},\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"output_format\":\"png\",\"size\":\"1024x1024\"}]}}\n\ndata: [DONE]\n\n")),
		}, nil
	default:
		return nil, errors.New("unexpected image OAuth account")
	}
}

func (u *openAIImagesOAuthFailoverUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestOpenAIImages_OAuth5xxSkipsSameAccountReplayAndFailsOver(t *testing.T) {
	const failedAccountID int64 = 9451
	const successAccountID int64 = 9452
	upstream := &openAIImagesOAuthFailoverUpstream{
		failedAccountID:  failedAccountID,
		successAccountID: successAccountID,
	}
	accounts := []service.Account{
		{
			ID: failedAccountID, Name: "image-oauth-first-502", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
			Credentials: map[string]any{"access_token": "token-first", "chatgpt_account_id": "acct-first"},
		},
		{
			ID: successAccountID, Name: "image-oauth-second-success", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
			Credentials: map[string]any{"access_token": "token-second", "chatgpt_account_id": "acct-second"},
		},
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 2
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSSilentRetryAccountRepoStub{accounts: accounts}, usageRepo,
		nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := NewOpenAIGatewayHandler(
		gatewaySvc, service.NewConcurrencyService(cache), billingCacheSvc,
		&service.APIKeyService{}, nil, nil, nil, cfg,
	)

	groupID := int64(4451)
	apiKey := &service.APIKey{
		ID: 2051, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowImageGeneration: true},
		User:  &service.User{ID: 2052, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.POST("/v1/images/generations", h.Images)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(`{"model":"gpt-image-2","prompt":"draw a cat","response_format":"b64_json"}`)))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "aW1hZ2U=", gjson.Get(recorder.Body.String(), "data.0.b64_json").String())
	require.Equal(t, int32(1), upstream.failedHits.Load(), "OAuth 502 账号不得同账号重放")
	require.Equal(t, int32(1), upstream.successHits.Load(), "OAuth 必须切换到第二个图片账号")
	select {
	case usageLog := <-usageRepo.created:
		require.Equal(t, successAccountID, usageLog.AccountID)
		require.Equal(t, 1, usageLog.ImageCount)
	case <-time.After(3 * time.Second):
		t.Fatal("等待 OAuth 图片故障切换 usage log 超时")
	}
}

func TestOpenAIResponsesWebSocket_SilentRetrySwitchesOnUpstreamTemporaryClose(t *testing.T) {
	for _, closeCode := range []coderws.StatusCode{coderws.StatusTryAgainLater, coderws.StatusInternalError} {
		t.Run(strconv.Itoa(int(closeCode)), func(t *testing.T) {
			var upstreamHits int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := atomic.AddInt32(&upstreamHits, 1)
				conn, err := coderws.Accept(w, r, nil)
				require.NoError(t, err)
				defer func() { _ = conn.CloseNow() }()

				readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
				_, _, err = conn.Read(readCtx)
				cancelRead()
				require.NoError(t, err)
				if attempt <= 2 {
					require.NoError(t, conn.Close(closeCode, "no available account"))
					return
				}

				writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
				err = conn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_temporary_close_retry","model":"gpt-5.4","usage":{"input_tokens":2,"output_tokens":1}}}`))
				cancelWrite()
				require.NoError(t, err)
			}))
			defer upstream.Close()

			usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
			server := newOpenAIWSSilentRetryHandlerTestServerWithModeRouterV2(t, []service.Account{
				newOpenAIWSSilentRetryAccount(9401, "ws-temporary-close-first", upstream.URL, 0),
				newOpenAIWSSilentRetryAccount(9402, "ws-temporary-close-second", upstream.URL, 1),
			}, usageRepo, false)
			defer server.Close()

			clientConn := dialOpenAIWSTestClient(t, server.URL)
			defer func() { _ = clientConn.CloseNow() }()
			writeOpenAIWSTestPayload(t, clientConn, `{"type":"response.create","model":"gpt-5.4","input":"hello","stream":true}`)

			_, event, err := readOpenAIWSTestMessage(t, clientConn)
			require.NoError(t, err)
			require.Equal(t, "resp_temporary_close_retry", gjson.GetBytes(event, "response.id").String())
			_ = clientConn.Close(coderws.StatusNormalClosure, "done")

			require.Eventually(t, func() bool { return atomic.LoadInt32(&upstreamHits) == 3 }, time.Second, 10*time.Millisecond)
			select {
			case usageLog := <-usageRepo.created:
				require.Equal(t, int64(9402), usageLog.AccountID)
			case <-time.After(3 * time.Second):
				t.Fatal("等待 WebSocket usage log 写入超时")
			}
		})
	}
}

func TestOpenAIResponsesWebSocket_SilentRetryDoesNotSwitchWithPreviousResponseID(t *testing.T) {
	var failedHits int32
	failedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&failedHits, 1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer failedUpstream.Close()

	var secondHits int32
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		http.Error(w, "should not be selected", http.StatusInternalServerError)
	}))
	defer secondUpstream.Close()

	server := newOpenAIWSSilentRetryHandlerTestServer(t, []service.Account{
		newOpenAIWSSilentRetryAccount(9201, "ws-prev-first-fails", failedUpstream.URL, 0),
		newOpenAIWSSilentRetryAccount(9202, "ws-prev-second-blocked", secondUpstream.URL, 1),
	}, &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)})
	defer server.Close()

	clientConn := dialOpenAIWSTestClient(t, server.URL)
	defer func() { _ = clientConn.CloseNow() }()
	writeOpenAIWSTestPayload(t, clientConn, `{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_existing","input":"hello","stream":true}`)

	_, _, err := readOpenAIWSTestMessage(t, clientConn)
	require.Error(t, err)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&failedHits) == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&secondHits))
}

func TestOpenAIResponsesWebSocket_SilentRetryDoesNotSwitchAfterDownstreamWrite(t *testing.T) {
	var firstHits int32
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		conn, err := coderws.Accept(w, r, nil)
		require.NoError(t, err)
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		_, _, err = conn.Read(readCtx)
		cancelRead()
		require.NoError(t, err)

		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		err = conn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_partial","model":"gpt-5.4"}}`))
		cancelWrite()
		require.NoError(t, err)
	}))
	defer firstUpstream.Close()

	var secondHits int32
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		http.Error(w, "should not be selected", http.StatusInternalServerError)
	}))
	defer secondUpstream.Close()

	server := newOpenAIWSSilentRetryHandlerTestServer(t, []service.Account{
		newOpenAIWSSilentRetryAccount(9301, "ws-partial-first", firstUpstream.URL, 0),
		newOpenAIWSSilentRetryAccount(9302, "ws-partial-second-blocked", secondUpstream.URL, 1),
	}, &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)})
	defer server.Close()

	clientConn := dialOpenAIWSTestClient(t, server.URL)
	defer func() { _ = clientConn.CloseNow() }()
	writeOpenAIWSTestPayload(t, clientConn, `{"type":"response.create","model":"gpt-5.4","input":"hello","stream":true}`)

	_, event, err := readOpenAIWSTestMessage(t, clientConn)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.GetBytes(event, "type").String())

	_, _, err = readOpenAIWSTestMessage(t, clientConn)
	require.Error(t, err)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&firstHits) == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&secondHits))
}

func newOpenAIWSSilentRetryAccount(id int64, name string, upstreamBaseURL string, priority int) service.Account {
	return service.Account{
		ID:          id,
		Name:        name,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    priority,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": upstreamBaseURL,
		},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}
}

func newOpenAIWSSilentRetryHandlerTestServer(t *testing.T, accounts []service.Account, usageRepo *openAIWSUsageHandlerUsageLogRepoStub) *httptest.Server {
	return newOpenAIWSSilentRetryHandlerTestServerWithModeRouterV2(t, accounts, usageRepo, true)
}

func newOpenAIWSSilentRetryHandlerTestServerWithModeRouterV2(t *testing.T, accounts []service.Account, usageRepo *openAIWSUsageHandlerUsageLogRepoStub, modeRouterV2Enabled bool) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = modeRouterV2Enabled
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSSilentRetryAccountRepoStub{accounts: accounts},
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}

	groupID := int64(4301)
	apiKey := &service.APIKey{
		ID:      1901,
		GroupID: &groupID,
		User:    &service.User{ID: 1902, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	return httptest.NewServer(router)
}

func dialOpenAIWSTestClient(t *testing.T, serverURL string) *coderws.Conn {
	t.Helper()
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(serverURL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	return clientConn
}

func writeOpenAIWSTestPayload(t *testing.T, clientConn *coderws.Conn, payload string) {
	t.Helper()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err := clientConn.Write(writeCtx, coderws.MessageText, []byte(payload))
	cancelWrite()
	require.NoError(t, err)
}

func readOpenAIWSTestMessage(t *testing.T, clientConn *coderws.Conn) (coderws.MessageType, []byte, error) {
	t.Helper()
	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	msgType, event, err := clientConn.Read(readCtx)
	cancelRead()
	return msgType, event, err
}

type openAIWSUsageHandlerChannelRepoStub struct {
	service.ChannelRepository
	channels       []service.Channel
	groupPlatforms map[int64]string
}

func (s *openAIWSUsageHandlerChannelRepoStub) ListAll(ctx context.Context) ([]service.Channel, error) {
	return s.channels, nil
}

func (s *openAIWSUsageHandlerChannelRepoStub) GetGroupPlatforms(ctx context.Context, groupIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(groupIDs))
	for _, groupID := range groupIDs {
		if platform := strings.TrimSpace(s.groupPlatforms[groupID]); platform != "" {
			out[groupID] = platform
		}
	}
	return out, nil
}

func runOpenAIResponsesWebSocketFollowupUnsupportedImageCase(t *testing.T, tc openAIResponsesWSUsageLogCase) openAIResponsesWSFollowupUnsupportedImageResult {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var upstreamHits int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		_, _, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			return
		}
		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		_ = conn.Write(writeCtx, coderws.MessageText, []byte(
			`{"type":"response.completed","response":{"id":"resp_usage_e2e","model":"gpt-5.4","usage":{"input_tokens":2,"output_tokens":1}}}`,
		))
		cancelWrite()
		readCtx, cancelRead = context.WithTimeout(r.Context(), 600*time.Millisecond)
		_, _, _ = conn.Read(readCtx)
		cancelRead()
	}))
	defer upstreamServer.Close()

	groupID := int64(4201)
	account := service.Account{
		ID:          9901,
		Name:        "openai-ws-followup-image-unsupported-e2e",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": upstreamServer.URL,
		},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	var channelSvc *service.ChannelService
	if len(tc.channelMapping) > 0 {
		featuresConfig := map[string]any(nil)
		if tc.codexBridge {
			featuresConfig = map[string]any{"codex_image_generation_bridge": map[string]any{
				service.PlatformOpenAI:  true,
				"orchestrator_group_id": int64(20),
			}}
		}
		channelSvc = service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
			channels: []service.Channel{{
				ID:             7701,
				Name:           "openai-ws-followup-image-channel",
				Status:         service.StatusActive,
				GroupIDs:       []int64{groupID},
				ModelMapping:   map[string]map[string]string{service.PlatformOpenAI: tc.channelMapping},
				FeaturesConfig: featuresConfig,
			}},
			groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
		}, nil, nil, nil)
	}

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSUsageHandlerAccountRepoStub{account: account},
		&openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)},
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		channelSvc,
		nil,
		nil,
		nil,
		nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}

	apiKey := &service.APIKey{
		ID:      1801,
		GroupID: &groupID,
		Group: &service.Group{
			ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowImageGeneration: true,
		},
		User: &service.User{ID: 1701, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()

	headers := http.Header{}
	if tc.userAgent != nil {
		headers.Set("User-Agent", *tc.userAgent)
	}
	if tc.turnMetadataHeader != nil {
		headers.Set("x-codex-turn-metadata", *tc.turnMetadataHeader)
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{HTTPHeader: headers, CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeOpenAIWSTestPayload(t, clientConn, tc.firstPayload)
	_, firstEvent, err := readOpenAIWSTestMessage(t, clientConn)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(firstEvent, "type").String())

	writeOpenAIWSTestPayload(t, clientConn, tc.nextPayload)
	var closeErr coderws.CloseError
	var closeErrPtr *coderws.CloseError
	_, event, err := readOpenAIWSTestMessage(t, clientConn)
	if err != nil {
		if errors.As(err, &closeErr) {
			closeErrPtr = &closeErr
		} else {
			require.ErrorIs(t, err, io.EOF)
		}
		event = nil
	} else {
		_, _, err = readOpenAIWSTestMessage(t, clientConn)
		require.Error(t, err)
		if errors.As(err, &closeErr) {
			closeErrPtr = &closeErr
		} else {
			require.ErrorIs(t, err, io.EOF)
		}
	}

	require.Eventually(t, func() bool { return atomic.LoadInt32(&upstreamHits) == 1 }, time.Second, 10*time.Millisecond)
	return openAIResponsesWSFollowupUnsupportedImageResult{
		event:        event,
		readErr:      err,
		closeErr:     closeErrPtr,
		upstreamHits: atomic.LoadInt32(&upstreamHits),
	}
}

func runOpenAIResponsesWebSocketUnsupportedImageCase(t *testing.T, tc openAIResponsesWSUnsupportedImageCase) ([]byte, *coderws.CloseError) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var upstreamHits int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		http.Error(w, "ws image generation should not reach upstream", http.StatusInternalServerError)
	}))
	defer upstreamServer.Close()

	groupID := int64(4201)
	account := service.Account{
		ID:          9901,
		Name:        "openai-ws-image-unsupported-e2e",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": upstreamServer.URL,
		},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	var channelSvc *service.ChannelService
	if len(tc.channelMapping) > 0 {
		featuresConfig := map[string]any(nil)
		if tc.codexBridge {
			featuresConfig = map[string]any{"codex_image_generation_bridge": map[string]any{
				service.PlatformOpenAI: true, "orchestrator_group_id": int64(20),
			}}
		}
		channelSvc = service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
			channels: []service.Channel{{
				ID:             7701,
				Name:           "openai-ws-image-unsupported-channel",
				Status:         service.StatusActive,
				GroupIDs:       []int64{groupID},
				ModelMapping:   map[string]map[string]string{service.PlatformOpenAI: tc.channelMapping},
				FeaturesConfig: featuresConfig,
			}},
			groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
		}, nil, nil, nil)
	}

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSUsageHandlerAccountRepoStub{account: account},
		&openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)},
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		channelSvc,
		nil,
		nil,
		nil,
		nil,
	)

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}

	apiKey := &service.APIKey{
		ID:      1801,
		GroupID: &groupID,
		Group: &service.Group{
			ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowImageGeneration: true,
		},
		User: &service.User{ID: 1701, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()

	headers := http.Header{}
	if tc.userAgent != nil {
		headers.Set("User-Agent", *tc.userAgent)
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{HTTPHeader: headers, CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeOpenAIWSTestPayload(t, clientConn, tc.firstPayload)

	msgType, event, err := readOpenAIWSTestMessage(t, clientConn)
	require.NoError(t, err)
	require.Equal(t, coderws.MessageText, msgType)

	_, _, err = readOpenAIWSTestMessage(t, clientConn)
	require.Error(t, err)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, int32(0), atomic.LoadInt32(&upstreamHits), "WS 生图拒绝路径不得访问上游账号")
	return event, &closeErr
}

func assertOpenAIWSImageUnsupportedEvent(t *testing.T, event []byte, model string) {
	t.Helper()
	require.True(t, gjson.ValidBytes(event), "unsupported event must be valid JSON")
	require.Equal(t, "response.failed", gjson.GetBytes(event, "type").String())
	require.Equal(t, "failed", gjson.GetBytes(event, "response.status").String())
	require.Equal(t, strings.TrimSpace(model), gjson.GetBytes(event, "response.model").String())
	require.Equal(t, "image_generation_ws_unsupported", gjson.GetBytes(event, "response.error.code").String())
	require.Equal(t, openAIWSImageGenerationUnsupportedMessage, gjson.GetBytes(event, "response.error.message").String())
	require.Equal(t, "image_generation_ws_unsupported", gjson.GetBytes(event, "error.code").String())
	require.Equal(t, openAIWSImageGenerationUnsupportedMessage, gjson.GetBytes(event, "error.message").String())
}

func runOpenAIResponsesWebSocketUsageLogCase(t *testing.T, tc openAIResponsesWSUsageLogCase) openAIResponsesWSUsageLogResult {
	t.Helper()
	gin.SetMode(gin.TestMode)

	upstreamPayloadCh := make(chan []byte, 2)
	upstreamErrCh := make(chan error, 1)
	var upstreamHTTPHits int32
	var upstreamWSHits int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isOpenAIWSUpgradeRequest(r) {
			atomic.AddInt32(&upstreamHTTPHits, 1)
			payload, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				upstreamErrCh <- readErr
				return
			}
			upstreamPayloadCh <- payload
			if tc.upstreamHTTPStatus >= http.StatusBadRequest {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamHTTPStatus)
				_, _ = io.WriteString(w, `{"error":{"type":"upstream_error","message":"synthetic image upstream failure"}}`)
				upstreamErrCh <- nil
				return
			}
			if strings.HasSuffix(r.URL.Path, "/chat/completions") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl_usage_e2e","object":"chat.completion","model":"grok-4.5","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
				upstreamErrCh <- nil
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if gjson.GetBytes(payload, "tool_choice.type").String() == "image_generation" ||
				gjson.GetBytes(payload, "model").String() == "gpt-image-2" {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_usage_e2e\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage_e2e\",\"model\":\"gpt-image-2\",\"output\":[{\"id\":\"ig_usage_e2e\",\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\",\"size\":\"1024x1024\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"output_tokens_details\":{\"image_tokens\":1}}}}\n\n")
			} else {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage_e2e\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			}
			upstreamErrCh <- nil
			return
		}
		atomic.AddInt32(&upstreamWSHits, 1)
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{
			CompressionMode: coderws.CompressionContextTakeover,
		})
		if err != nil {
			upstreamErrCh <- err
			return
		}
		defer func() {
			_ = conn.CloseNow()
		}()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, payload, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			upstreamErrCh <- readErr
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			upstreamErrCh <- errors.New("unexpected upstream websocket message type")
			return
		}
		upstreamPayloadCh <- payload

		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		writeErr := conn.Write(writeCtx, coderws.MessageText, []byte(
			`{"type":"response.completed","response":{"id":"resp_usage_e2e","model":"gpt-5.4","usage":{"input_tokens":2,"output_tokens":1}}}`,
		))
		cancelWrite()
		if writeErr != nil {
			upstreamErrCh <- writeErr
			return
		}
		if strings.TrimSpace(tc.nextPayload) != "" {
			readCtx, cancelRead = context.WithTimeout(r.Context(), 3*time.Second)
			msgType, payload, readErr = conn.Read(readCtx)
			cancelRead()
			if readErr != nil {
				upstreamErrCh <- readErr
				return
			}
			if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
				upstreamErrCh <- errors.New("unexpected upstream websocket second message type")
				return
			}
			upstreamPayloadCh <- payload

			writeCtx, cancelWrite = context.WithTimeout(r.Context(), 3*time.Second)
			writeErr = conn.Write(writeCtx, coderws.MessageText, []byte(
				`{"type":"response.completed","response":{"id":"resp_usage_e2e_2","model":"gpt-5.4","usage":{"input_tokens":3,"output_tokens":1}}}`,
			))
			cancelWrite()
			if writeErr != nil {
				upstreamErrCh <- writeErr
				return
			}
		}
		_ = conn.Close(coderws.StatusNormalClosure, "done")
		upstreamErrCh <- nil
	}))
	defer upstreamServer.Close()

	groupID := int64(4201)
	account := service.Account{
		ID:          9901,
		Name:        "openai-ws-passthrough-usage-e2e",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": upstreamServer.URL,
		},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}
	if tc.responsesSupported != nil {
		account.Extra["openai_responses_supported"] = *tc.responsesSupported
	}
	if len(tc.accountModelMapping) > 0 {
		account.Credentials["model_mapping"] = tc.accountModelMapping
	}
	if tc.httpOnlyAccount {
		account.Extra = map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": false,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModeOff,
		}
	}
	if tc.codexBridge {
		account.Extra["openai_passthrough"] = true
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: account}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}

	var channelSvc *service.ChannelService
	if len(tc.channelMapping) > 0 {
		featuresConfig := map[string]any(nil)
		if tc.codexBridge {
			featuresConfig = map[string]any{"codex_image_generation_bridge": map[string]any{
				service.PlatformOpenAI:  true,
				"orchestrator_group_id": int64(20),
			}}
		}
		channelSvc = service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
			channels: []service.Channel{{
				ID:             7701,
				Name:           "openai-ws-e2e-channel",
				Status:         service.StatusActive,
				GroupIDs:       []int64{groupID},
				ModelMapping:   map[string]map[string]string{service.PlatformOpenAI: tc.channelMapping},
				FeaturesConfig: featuresConfig,
			}},
			groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
		}, nil, nil, nil)
	}

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		repository.NewHTTPUpstream(cfg),
		&service.DeferredService{},
		nil,
		nil,
		channelSvc,
		nil,
		nil,
		nil, // userPlatformQuotaRepo
		nil, // idempotencyRepo
	)

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}

	allowImageGeneration := true
	if tc.allowImageGeneration != nil {
		allowImageGeneration = *tc.allowImageGeneration
	}
	apiKey := &service.APIKey{
		ID:      1801,
		GroupID: &groupID,
		Group: &service.Group{
			ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowImageGeneration: allowImageGeneration,
		},
		User: &service.User{ID: 1701, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()

	headers := http.Header{}
	if tc.userAgent != nil {
		headers.Set("User-Agent", *tc.userAgent)
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{HTTPHeader: headers, CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(tc.firstPayload))
	cancelWrite()
	require.NoError(t, err)
	if tc.upstreamHTTPStatus >= http.StatusBadRequest {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, event, readErr := clientConn.Read(readCtx)
		cancelRead()
		var closeErr coderws.CloseError
		if readErr == nil {
			require.Equal(t, "response.failed", gjson.GetBytes(event, "type").String())
			readCtx, cancelRead = context.WithTimeout(context.Background(), 3*time.Second)
			_, _, readErr = clientConn.Read(readCtx)
			cancelRead()
			require.Error(t, readErr)
		}
		require.ErrorAs(t, readErr, &closeErr)

		select {
		case upstreamErr := <-upstreamErrCh:
			require.NoError(t, upstreamErr)
		case <-time.After(3 * time.Second):
			t.Fatal("等待 HTTP 图片上游失败结束超时")
		}
		return openAIResponsesWSUsageLogResult{
			closeErr:         &closeErr,
			upstreamHTTPHits: atomic.LoadInt32(&upstreamHTTPHits),
			upstreamWSHits:   atomic.LoadInt32(&upstreamWSHits),
		}
	}
	if tc.expectSelectionFailure {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, _, readErr := clientConn.Read(readCtx)
		cancelRead()
		require.Error(t, readErr)
		var closeErr coderws.CloseError
		require.ErrorAs(t, readErr, &closeErr)
		require.Equal(t, coderws.StatusTryAgainLater, closeErr.Code)
		require.Equal(t, "no available account", closeErr.Reason)
		return openAIResponsesWSUsageLogResult{}
	}

	var terminalEvent []byte
	for range 8 {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, event, readErr := clientConn.Read(readCtx)
		cancelRead()
		require.NoError(t, readErr)
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			terminalEvent = event
			break
		}
	}
	require.NotEmpty(t, terminalEvent, "WebSocket 客户端必须收到 response.completed")
	if strings.TrimSpace(tc.nextPayload) != "" {
		writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
		err = clientConn.Write(writeCtx, coderws.MessageText, []byte(tc.nextPayload))
		cancelWrite()
		require.NoError(t, err)
	}
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	var usageLog *service.UsageLog
	select {
	case usageLog = <-usageRepo.created:
		require.NotNil(t, usageLog)
	case <-time.After(3 * time.Second):
		t.Fatal("等待 WebSocket usage log 写入超时")
	}

	var upstreamFirstPayload []byte
	select {
	case upstreamFirstPayload = <-upstreamPayloadCh:
	case <-time.After(3 * time.Second):
		t.Fatal("等待上游 WebSocket 首帧超时")
	}
	var upstreamSecondPayload []byte
	if strings.TrimSpace(tc.nextPayload) != "" {
		select {
		case upstreamSecondPayload = <-upstreamPayloadCh:
		case <-time.After(3 * time.Second):
			t.Fatal("等待上游 WebSocket 第二帧超时")
		}
	}

	select {
	case upstreamErr := <-upstreamErrCh:
		if strings.TrimSpace(tc.nextPayload) == "" {
			require.NoError(t, upstreamErr)
		}
	case <-time.After(3 * time.Second):
		if strings.TrimSpace(tc.nextPayload) == "" {
			t.Fatal("等待上游 WebSocket 结束超时")
		}
	}

	return openAIResponsesWSUsageLogResult{
		log:                   usageLog,
		upstreamFirstPayload:  upstreamFirstPayload,
		upstreamSecondPayload: upstreamSecondPayload,
		upstreamHTTPHits:      atomic.LoadInt32(&upstreamHTTPHits),
		upstreamWSHits:        atomic.LoadInt32(&upstreamWSHits),
	}
}

func testStringPtr(v string) *string {
	return &v
}
