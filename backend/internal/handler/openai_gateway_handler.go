package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

type openAIHTTPFallbackRecorder struct {
	code int
	head http.Header
	body bytes.Buffer
}

func newOpenAIHTTPFallbackRecorder() *openAIHTTPFallbackRecorder {
	return &openAIHTTPFallbackRecorder{
		head: make(http.Header),
	}
}

func (r *openAIHTTPFallbackRecorder) Header() http.Header {
	if r.head == nil {
		r.head = make(http.Header)
	}
	return r.head
}

func (r *openAIHTTPFallbackRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *openAIHTTPFallbackRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

func (r *openAIHTTPFallbackRecorder) Code() int {
	if r == nil || r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

func (r *openAIHTTPFallbackRecorder) BodyBytes() []byte {
	if r == nil {
		return nil
	}
	return r.body.Bytes()
}

func (r *openAIHTTPFallbackRecorder) Flush() {}

// OpenAIGatewayHandler handles OpenAI API gateway requests
type OpenAIGatewayHandler struct {
	gatewayService             *service.OpenAIGatewayService
	billingCacheService        *service.BillingCacheService
	apiKeyService              *service.APIKeyService
	usageRecordWorkerPool      *service.UsageRecordWorkerPool
	errorPassthroughService    *service.ErrorPassthroughService
	contentModerationService   *service.ContentModerationService
	grokMediaEligibilityProber grokMediaEligibilityProber
	concurrencyHelper          *ConcurrencyHelper
	imageLimiter               *imageConcurrencyLimiter
	maxAccountSwitches         int
	cfg                        *config.Config
}

type grokMediaEligibilityProber interface {
	ProbeMediaEligibility(ctx context.Context, accountID int64) (bool, string, error)
}

func openAICompatibleRequestPlatform(apiKey *service.APIKey) string {
	if apiKey != nil && apiKey.Group != nil {
		switch apiKey.Group.Platform {
		case service.PlatformGrok, service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepSeek:
			return apiKey.Group.Platform
		}
	}
	return service.PlatformOpenAI
}

func (h *OpenAIGatewayHandler) SetGrokMediaEligibilityProber(prober grokMediaEligibilityProber) {
	if h != nil {
		h.grokMediaEligibilityProber = prober
	}
}

func resolveOpenAIMessagesDispatchMappedModel(apiKey *service.APIKey, requestedModel string) string {
	if apiKey == nil || apiKey.Group == nil {
		return ""
	}
	return strings.TrimSpace(apiKey.Group.ResolveMessagesDispatchModel(requestedModel))
}

func resolveChannelMappedAccountSelectionModel(fallbackModel string, channelMapping service.ChannelMappingResult, applyChannelMapping bool) string {
	if applyChannelMapping && channelMapping.Mapped {
		if mappedModel := strings.TrimSpace(channelMapping.MappedModel); mappedModel != "" {
			return mappedModel
		}
	}
	return fallbackModel
}

// NewOpenAIGatewayHandler creates a new OpenAIGatewayHandler
func NewOpenAIGatewayHandler(
	gatewayService *service.OpenAIGatewayService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	errorPassthroughService *service.ErrorPassthroughService,
	contentModerationService *service.ContentModerationService,
	cfg *config.Config,
) *OpenAIGatewayHandler {
	pingInterval := time.Duration(0)
	maxAccountSwitches := 3
	if cfg != nil {
		pingInterval = time.Duration(cfg.Concurrency.PingInterval) * time.Second
		if cfg.Gateway.MaxAccountSwitches > 0 {
			maxAccountSwitches = cfg.Gateway.MaxAccountSwitches
		}
	}
	return &OpenAIGatewayHandler{
		gatewayService:           gatewayService,
		billingCacheService:      billingCacheService,
		apiKeyService:            apiKeyService,
		usageRecordWorkerPool:    usageRecordWorkerPool,
		errorPassthroughService:  errorPassthroughService,
		contentModerationService: contentModerationService,
		concurrencyHelper:        NewConcurrencyHelper(concurrencyService, SSEPingFormatComment, pingInterval),
		imageLimiter:             &imageConcurrencyLimiter{},
		maxAccountSwitches:       maxAccountSwitches,
		cfg:                      cfg,
	}
}

// Responses handles OpenAI Responses API endpoint
// POST /openai/v1/responses
func (h *OpenAIGatewayHandler) Responses(c *gin.Context) {
	// 局部兜底：确保该 handler 内部任何 panic 都不会击穿到进程级。
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)
	compactStartedAt := time.Now()
	defer h.logOpenAIRemoteCompactOutcome(c, compactStartedAt)
	setOpenAIClientTransportHTTP(c)

	requestStart := time.Now()

	// Get apiKey and user from context (set by ApiKeyAuth middleware)
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	// Read request body
	body, err := readRequestBodyWithDiagnostics(c)
	if err != nil {
		writeRequestBodyReadError(c, http.StatusBadRequest, "invalid_request_error", err, func(status int, errType string, message string) {
			h.errorResponse(c, status, errType, message)
		})
		return
	}

	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	setOpsRequestContext(c, "", false)
	sessionHashBody := body
	if service.IsOpenAIResponsesCompactPathForTest(c) {
		if compactSeed := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); compactSeed != "" {
			c.Set(service.OpenAICompactSessionSeedKeyForTest(), compactSeed)
		}
		normalizedCompactBody, normalizedCompact, compactErr := service.NormalizeOpenAICompactRequestBodyForTest(body)
		if compactErr != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to normalize compact request body")
			return
		}
		if normalizedCompact {
			body = normalizedCompactBody
		}
	}

	// 校验请求体 JSON 合法性
	if !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	// Capture the protocol signal once and reuse it across account selection and
	// forwarding. Native Remote Compaction V2 stays on /responses; legacy
	// clients continue to use the explicit /responses/compact route.
	compactionContext := service.ParseOpenAICompactionContext(body, c.Request.Header, c.Request.URL.Path)
	c.Set("openai_compaction_context", compactionContext)
	if compactionContext.NativeResponses && !h.gatewayService.IsOpenAIRemoteCompactionV2Enabled(c.Request.Context()) {
		h.errorResponse(c, http.StatusServiceUnavailable, "remote_compaction_v2_disabled", "OpenAI Remote Compaction V2 is temporarily disabled")
		return
	}
	if compactionContext.NativeResponses {
		reqCtx := service.WithOpenAICompactionContext(c.Request.Context(), compactionContext)
		c.Request = c.Request.WithContext(reqCtx)
	}

	// 使用 gjson 只读提取字段做校验，避免完整 Unmarshal
	modelResult := gjson.GetBytes(body, "model")
	reqModel, validModelField := extractOpenAIRequestModel(body)
	if !validModelField {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	requestPlatform := openAICompatibleRequestPlatform(apiKey)
	if requestPlatform == service.PlatformOpenAI && shouldFallbackOpenAIClientModel(reqModel) {
		fallbackModel := ""
		if h.gatewayService != nil {
			fallbackModel = h.gatewayService.ResolveOpenAIPlatformFallbackModel(c.Request.Context())
		}
		if fallbackModel == "" {
			fallbackModel = service.OpenAIDefaultFallbackModel()
		}
		updatedBody, fallbackReqModel, fallbackApplied, fallbackErr := applyOpenAIClientDefaultModelFallback(body, reqModel, fallbackModel)
		if fallbackErr != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
			return
		}
		if fallbackApplied {
			reqLog.Info("openai.model_default_fallback_applied",
				zap.String("requested_model", modelResult.String()),
				zap.String("fallback_model", fallbackReqModel),
			)
			body = updatedBody
			sessionHashBody = body
			reqModel = fallbackReqModel
		}
	}

	streamResult := gjson.GetBytes(body, "stream")
	if streamResult.Exists() && streamResult.Type != gjson.True && streamResult.Type != gjson.False {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "invalid stream field type")
		return
	}
	reqStream := streamResult.Bool()
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))
	previousResponseID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	previousResponseIDKind := ""
	if previousResponseID != "" {
		previousResponseIDKind = service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
		reqLog = reqLog.With(
			zap.Bool("has_previous_response_id", true),
			zap.String("previous_response_id_kind", previousResponseIDKind),
			zap.Int("previous_response_id_len", len(previousResponseID)),
		)
	}

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}

	imagePermissionIntent := service.IsImageGenerationPermissionIntentForPlatform("/v1/responses", reqModel, body, requestPlatform)

	// Resolve the channel policy once, then classify the stable Codex request
	// role separately from the client image capability and transport.
	codexRoute := h.gatewayService.ResolveCodexImageGenerationRoute(c.Request.Context(), apiKey.GroupID, reqModel)
	if codexRoute.ConfigurationError != "" {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", codexRoute.ConfigurationError)
		return
	}
	channelMapping := codexRoute.Mapping
	codexDecision := service.CodexImageRequestDecision{Execution: service.CodexImageExecutionOrdinary}
	if codexRoute.Enabled {
		codexDecision = service.ClassifyCodexImageRequest(reqModel, codexRoute.Mapping.MappedModel, body)
		reqLog.Info("openai.codex_image_route_classified",
			zap.String("request_role", string(codexDecision.Role)),
			zap.String("execution", string(codexDecision.Execution)),
			zap.Bool("has_metadata", codexDecision.HasMetadata),
			zap.Bool("has_extension", codexDecision.HasExtension),
			zap.Bool("has_hosted_image", codexDecision.HasHostedImage),
			zap.Bool("legacy_fallback", codexDecision.LegacyFallback),
			zap.String("requested_model", reqModel),
			zap.String("mapped_model", codexRoute.Mapping.MappedModel),
		)
	}
	codexImageExtensionCandidate := codexDecision.Execution == service.CodexImageExecutionExtension
	codexHostedImageTurn := codexDecision.Execution == service.CodexImageExecutionHostedImage
	codexTextBypassTurn := codexDecision.Execution == service.CodexImageExecutionTextBypass
	codexSystemBackgroundTurn := codexTextBypassTurn && service.IsCodexSystemBackgroundTurn(body)
	if codexImageExtensionCandidate && service.IsSuccessfulCodexImageGenerationExtensionContinuation(body) {
		reqLog.Info("openai.codex_image_success_continuation_completed_locally",
			zap.Int("body_bytes", len(body)),
			zap.String("requested_model", reqModel),
		)
		writeCodexImageContinuationCompleted(c, reqStream)
		return
	}
	if previousResponseID != "" {
		if previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
			reqLog.Warn("openai.request_validation_failed",
				zap.String("reason", "previous_response_id_looks_like_message_id"),
			)
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id must be a response.id (resp_*), not a message id")
			return
		}
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "previous_response_id_requires_wsv2"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id is only supported on Responses WebSocket v2")
		return
	}

	if (imagePermissionIntent || codexDecision.IsImageExecution()) && !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	imageIntent := service.IsImageGenerationIntentForPlatform("/v1/responses", reqModel, body, requestPlatform) || codexHostedImageTurn
	if imageIntent && !imagePermissionIntent && !codexHostedImageTurn && !service.GroupAllowsImageGeneration(apiKey.Group) {
		imageIntent = false
	}
	var imageReleaseFunc func()
	if imageIntent {
		var imageAcquired bool
		imageReleaseFunc, imageAcquired = h.acquireImageGenerationSlot(c, streamStarted)
		if !imageAcquired {
			return
		}
		if imageReleaseFunc != nil {
			defer imageReleaseFunc()
		}
	}

	selectionGroupID := apiKey.GroupID
	if codexDecision.UsesOrchestratorGroup() {
		selectionGroupID = codexRoute.OrchestratorGroupID
	}
	selectionModel := resolveChannelMappedAccountSelectionModel(reqModel, channelMapping, !codexDecision.UsesOrchestratorGroup())
	c.Set(service.OpenAICodexSystemBackgroundContextKey, codexSystemBackgroundTurn)
	usageChannelMapping := channelMapping
	if codexTextBypassTurn {
		usageChannelMapping.ChannelID = 0
		usageChannelMapping.Mapped = false
		usageChannelMapping.MappedModel = reqModel
		usageChannelMapping.BillingModelSource = service.BillingModelSourceRequested
	}

	// 提前校验 function_call_output 是否具备可关联上下文，避免上游 400。
	if !h.validateFunctionCallOutputRequest(c, body, reqLog) {
		return
	}

	// 绑定错误透传服务，允许 service 层在非 failover 错误场景复用规则。
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	// Get subscription info (may be nil)
	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	// 确保请求取消时也会释放槽位，避免长连接被动中断造成泄漏
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	// 2. Re-check billing eligibility after wait
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	// HTTP Responses requests without an explicit client session use a stable
	// prompt-prefix affinity key. This keeps later turns on the account whose
	// upstream prompt cache was populated by the first turn.
	sessionHash := h.gatewayService.GenerateHTTPStableSessionHash(c, sessionHashBody)
	requireCompact := isOpenAIRemoteCompactPath(c) || compactionContext.NativeResponses

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	teamWorkspaceSwitchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState

	for {
		// Select account supporting the requested model
		reqLog.Debug("openai.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			selectionGroupID,
			previousResponseID,
			sessionHash,
			selectionModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityResponses,
			requireCompact,
			false,
			true,
			requestPlatform,
		)
		if err != nil {
			reqLog.Warn("openai.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				if errors.Is(err, service.ErrNoAvailableCompactAccounts) {
					h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "compact_not_supported", "No available OpenAI accounts support /responses/compact", streamStarted)
					return
				}
				h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "Service temporarily unavailable", streamStarted)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleFailoverExhaustedSimple(c, 502, streamStarted)
			}
			return
		}
		if selection == nil || selection.Account == nil {
			markOpsRoutingCapacityLimited(c)
			h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", streamStarted)
			return
		}
		if previousResponseID != "" && selection != nil && selection.Account != nil {
			reqLog.Debug("openai.account_selected_with_previous_response_id", zap.Int64("account_id", selection.Account.ID))
		}
		reqLog.Debug("openai.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_previous_hit", scheduleDecision.StickyPreviousHit),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)
		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		setOpsSelectedAccount(c, account.ID, account.Platform)
		c.Set(openAITeamWorkspacePlatformContextKey, account.Platform)

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, selectionGroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		// Forward request
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		writerSizeBeforeForward := c.Writer.Size()
		// Current Codex clients display generated images only after executing the
		// local image_gen extension. Keep the channel mapping as the image-capable
		// routing signal, but let the text orchestrator decide whether the user's
		// actual intent requires that tool.
		forwardBody := body
		useCodexImageExtension := codexImageExtensionCandidate
		c.Set(service.OpenAICodexImageGenerationExtensionContextKey, useCodexImageExtension)
		c.Set(service.OpenAICodexImageGenerationToolCalledContextKey, false)
		if codexDecision.Execution != service.CodexImageExecutionOrdinary {
			preparedBody, prepareErr := service.PrepareCodexImageRouteRequest(body, reqModel, channelMapping.MappedModel, codexDecision)
			if prepareErr != nil {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				h.handleStreamingAwareError(c, http.StatusBadRequest, "invalid_request_error", "Invalid Codex image routing request", streamStarted)
				return
			}
			forwardBody = preparedBody
		} else if channelMapping.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
		}
		if !bytes.Equal(forwardBody, body) {
			// function_call_output 预校验会缓存映射前的请求体。渠道映射或图片路由
			// 修改 body 后必须让 service 重新解析，否则会上报错误的上游模型，
			// 完整重编码分支还可能把旧模型重新写回实际请求。
			c.Set(service.OpenAIParsedRequestBodyKey, nil)
		}
		if codexTextBypassTurn {
			reqLog.Info("openai.codex_non_user_mapping_bypassed",
				zap.String("request_role", string(codexDecision.Role)),
				zap.String("requested_model", reqModel),
				zap.String("image_model", channelMapping.MappedModel),
			)
		}
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.Forward(c.Request.Context(), c, account, forwardBody)
		}()
		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					appendBillableUsageAttemptFromFailover(c, account, reqModel, failoverErr)
					if c.Writer.Size() != writerSizeBeforeForward {
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
					// 池模式：同账号重试
					if failoverErr.RetryableOnSameAccount {
						retryLimit := account.GetPoolModeRetryCount()
						if sameAccountRetryCount[account.ID] < retryLimit {
							sameAccountRetryCount[account.ID]++
							reqLog.Warn("openai.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(sameAccountRetryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					teamWorkspaceFailure := service.IsOpenAITeamWorkspaceDeactivated(failoverErr.StatusCode, failoverErr.ResponseBody) && strings.TrimSpace(account.GetChatGPTAccountID()) != ""
					if teamWorkspaceFailure && !h.gatewayService.IsOpenAITeamLinkedResolverEnabled(c.Request.Context()) {
						// Rollback mode preserves the legacy single-account 402 contract;
						// never perform the Team-wide cross-account switch.
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					if teamWorkspaceSwitchCount >= 1 {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					if teamWorkspaceFailure {
						teamWorkspaceSwitchCount++
					}
					if switchCount >= maxAccountSwitches && !teamWorkspaceFailure {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
				wroteFallback := h.ensureForwardErrorResponse(c, streamStarted)
				fields := []zap.Field{
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Error(err),
				}
				if shouldLogOpenAIForwardFailureAsWarn(c, wroteFallback) {
					reqLog.Warn("openai.forward_failed", fields...)
					return
				}
				reqLog.Error("openai.forward_failed", fields...)
				return
			}
		}
		if result != nil {
			if account.Type == service.AccountTypeOAuth {
				h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(c.Request.Context(), account.ID, result.ResponseHeaders)
			}
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)
		}
		if result != nil && result.DuplicateSuppressed {
			reqLog.Info("openai.image_bridge_duplicate_suppressed",
				zap.Int64("account_id", account.ID),
				zap.Int("switch_count", switchCount),
			)
			return
		}
		if codexSystemBackgroundTurn {
			reqLog.Info("openai.codex_system_background_completed", zap.Int64("account_id", account.ID))
			return
		}
		if useCodexImageExtension && service.CodexImageGenerationToolCalled(c) {
			reqLog.Info("openai.codex_image_generation_extension_dispatched",
				zap.Int64("account_id", account.ID),
				zap.String("requested_model", reqModel),
				zap.String("image_model", channelMapping.MappedModel),
			)
			return
		}
		if useCodexImageExtension {
			reqLog.Info("openai.codex_image_orchestrator_text_completed",
				zap.Int64("account_id", account.ID),
				zap.String("requested_model", reqModel),
			)
			// The channel's text-to-image mapping describes the optional tool, not
			// the billing model for a turn where the orchestrator returned text.
			usageChannelMapping.ChannelID = 0
			usageChannelMapping.Mapped = false
			usageChannelMapping.MappedModel = reqModel
			usageChannelMapping.BillingModelSource = service.BillingModelSourceRequested
		}
		// 捕获请求信息（用于异步记录，避免在 goroutine 中访问 gin.Context）
		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)

		// 使用量记录通过有界 worker 池提交，避免请求热路径创建无界 goroutine。
		upstreamAttempts := usageUpstreamAttemptsSnapshot(c)
		fixedRequestPrice := service.CloneDecimalSnapshot(openAIXSearchFixedPrice(c))
		h.submitOpenAIUsageRecordTask(result, func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:                 result,
				APIKey:                 apiKey,
				User:                   apiKey.User,
				Account:                account,
				UpstreamCostMultiplier: service.CloneDecimalSnapshot(account.UpstreamCostMultiplier),
				Subscription:           subscription,
				InboundEndpoint:        inboundEndpoint,
				UpstreamEndpoint:       upstreamEndpoint,
				UserAgent:              userAgent,
				IPAddress:              clientIP,
				RequestPayloadHash:     requestPayloadHash,
				APIKeyService:          h.apiKeyService,
				ChannelUsageFields:     usageChannelMapping.ToUsageFields(reqModel, result.UpstreamModel),
				UpstreamAttempts:       service.CloneUsageUpstreamAttempts(upstreamAttempts),
				FixedRequestPrice:      fixedRequestPrice,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.responses"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func isOpenAIRemoteCompactPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(normalizedPath, "/responses/compact")
}

func (h *OpenAIGatewayHandler) logOpenAIRemoteCompactOutcome(c *gin.Context, startedAt time.Time) {
	if !isOpenAIRemoteCompactPath(c) {
		return
	}

	var (
		ctx    = context.Background()
		path   string
		status int
	)
	if c != nil {
		if c.Request != nil {
			ctx = c.Request.Context()
			if c.Request.URL != nil {
				path = strings.TrimSpace(c.Request.URL.Path)
			}
		}
		if c.Writer != nil {
			status = c.Writer.Status()
		}
	}

	outcome := "failed"
	if status >= 200 && status < 300 {
		outcome = "succeeded"
	}
	latencyMs := time.Since(startedAt).Milliseconds()
	if latencyMs < 0 {
		latencyMs = 0
	}

	fields := []zap.Field{
		zap.String("component", "handler.openai_gateway.responses"),
		zap.Bool("remote_compact", true),
		zap.String("compact_outcome", outcome),
		zap.Int("status_code", status),
		zap.Int64("latency_ms", latencyMs),
		zap.String("path", path),
		zap.Bool("force_codex_cli", h != nil && h.cfg != nil && h.cfg.Gateway.ForceCodexCLI),
	}

	if c != nil {
		if userAgent := strings.TrimSpace(c.GetHeader("User-Agent")); userAgent != "" {
			fields = append(fields, zap.String("request_user_agent", userAgent))
		}
		if v, ok := c.Get(opsModelKey); ok {
			if model, ok := v.(string); ok && strings.TrimSpace(model) != "" {
				fields = append(fields, zap.String("request_model", strings.TrimSpace(model)))
			}
		}
		if v, ok := c.Get(opsAccountIDKey); ok {
			if accountID, ok := v.(int64); ok && accountID > 0 {
				fields = append(fields, zap.Int64("account_id", accountID))
			}
		}
		if c.Writer != nil {
			if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("x-request-id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			} else if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("X-Request-Id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			}
		}
	}

	log := logger.FromContext(ctx).With(fields...)
	if outcome == "succeeded" {
		log.Info("codex.remote_compact.succeeded")
		return
	}
	log.Warn("codex.remote_compact.failed")
}

// Messages handles Anthropic Messages API requests routed to OpenAI platform.
// POST /v1/messages (when group platform is OpenAI)
func (h *OpenAIGatewayHandler) Messages(c *gin.Context) {
	streamStarted := false
	defer h.recoverAnthropicMessagesPanic(c, &streamStarted)

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.messages",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	// 检查分组是否允许 /v1/messages 调度
	if apiKey.Group != nil && !apiKey.Group.AllowMessagesDispatch {
		h.anthropicErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group does not allow /v1/messages dispatch")
		return
	}

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readRequestBodyWithDiagnostics(c)
	if err != nil {
		writeRequestBodyReadError(c, http.StatusBadRequest, "invalid_request_error", err, func(status int, errType string, message string) {
			h.anthropicErrorResponse(c, status, errType, message)
		})
		return
	}
	if len(body) == 0 {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	if !gjson.ValidBytes(body) {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	requestPlatform := openAICompatibleRequestPlatform(apiKey)
	routingModel := service.NormalizeOpenAICompatRequestedModel(reqModel)
	preferredMappedModel := resolveOpenAIMessagesDispatchMappedModel(apiKey, reqModel)
	reqStream := gjson.GetBytes(body, "stream").Bool()

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolAnthropicMessages, reqModel, body); decision != nil && decision.Blocked {
		h.anthropicErrorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}

	// 解析渠道级模型映射
	channelMappingMsg, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)

	// 绑定错误透传服务，允许 service 层在非 failover 错误场景复用规则。
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_messages.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.anthropicStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	promptCacheKey := h.gatewayService.ExtractSessionID(c, body)
	sessionHash, promptCacheKey = resolveOpenAIMessagesMetadataSession(sessionHash, promptCacheKey, reqModel, body)

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	teamWorkspaceSwitchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	effectiveMappedModel := preferredMappedModel

	for {
		currentRoutingModel := routingModel
		if effectiveMappedModel != "" {
			currentRoutingModel = effectiveMappedModel
		}
		currentRoutingModel = resolveChannelMappedAccountSelectionModel(currentRoutingModel, channelMappingMsg, true)
		reqLog.Debug("openai_messages.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			"", // no previous_response_id
			sessionHash,
			currentRoutingModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			false,
			true,
			requestPlatform,
		)
		if err != nil {
			reqLog.Warn("openai_messages.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				if err != nil {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					h.anthropicStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "Service temporarily unavailable", streamStarted)
					return
				}
			} else {
				if lastFailoverErr != nil {
					h.handleAnthropicFailoverExhausted(c, lastFailoverErr, streamStarted)
				} else {
					h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			markOpsRoutingCapacityLimited(c)
			h.anthropicStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", streamStarted)
			return
		}
		account := selection.Account
		c.Set(openAITeamWorkspacePlatformContextKey, account.Platform)
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_messages.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		writerSizeBeforeForward := c.Writer.Size()

		defaultMappedModel := strings.TrimSpace(effectiveMappedModel)
		// 应用渠道模型映射到请求体
		forwardBody := body
		if channelMappingMsg.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMappingMsg.MappedModel)
		}
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardAsAnthropic(c.Request.Context(), c, account, forwardBody, promptCacheKey, defaultMappedModel)
		}()

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai_messages.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					appendBillableUsageAttemptFromFailover(c, account, reqModel, failoverErr)
					if c.Writer.Size() != writerSizeBeforeForward {
						h.handleAnthropicFailoverExhausted(c, failoverErr, true)
						return
					}
					h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
					// 池模式：同账号重试
					if failoverErr.RetryableOnSameAccount {
						retryLimit := account.GetPoolModeRetryCount()
						if sameAccountRetryCount[account.ID] < retryLimit {
							sameAccountRetryCount[account.ID]++
							reqLog.Warn("openai_messages.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(sameAccountRetryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					teamWorkspaceFailure := service.IsOpenAITeamWorkspaceDeactivated(failoverErr.StatusCode, failoverErr.ResponseBody) && strings.TrimSpace(account.GetChatGPTAccountID()) != ""
					if teamWorkspaceFailure && !h.gatewayService.IsOpenAITeamLinkedResolverEnabled(c.Request.Context()) {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					if teamWorkspaceSwitchCount >= 1 {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					if teamWorkspaceFailure {
						teamWorkspaceSwitchCount++
					}
					if switchCount >= maxAccountSwitches && !teamWorkspaceFailure {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai_messages.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
				wroteFallback := h.ensureAnthropicErrorResponse(c, streamStarted)
				reqLog.Warn("openai_messages.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Error(err),
				)
				return
			}
		}
		if result != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)
		}
		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)

		upstreamAttempts := usageUpstreamAttemptsSnapshot(c)
		fixedRequestPrice := service.CloneDecimalSnapshot(openAIXSearchFixedPrice(c))
		h.submitOpenAIUsageRecordTask(result, func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:                 result,
				APIKey:                 apiKey,
				User:                   apiKey.User,
				Account:                account,
				UpstreamCostMultiplier: service.CloneDecimalSnapshot(account.UpstreamCostMultiplier),
				Subscription:           subscription,
				InboundEndpoint:        inboundEndpoint,
				UpstreamEndpoint:       upstreamEndpoint,
				UserAgent:              userAgent,
				IPAddress:              clientIP,
				RequestPayloadHash:     requestPayloadHash,
				APIKeyService:          h.apiKeyService,
				ChannelUsageFields:     channelMappingMsg.ToUsageFields(reqModel, result.UpstreamModel),
				UpstreamAttempts:       service.CloneUsageUpstreamAttempts(upstreamAttempts),
				FixedRequestPrice:      fixedRequestPrice,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.messages"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_messages.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_messages.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func resolveOpenAIMessagesMetadataSession(sessionHash, promptCacheKey, reqModel string, body []byte) (string, string) {
	// Anthropic metadata.user_id 只作为账号粘性信号。上游 GPT/Codex 缓存键
	// 交给 ForwardAsAnthropic 从 cache_control 或完整消息 digest 派生，避免
	// 固定 metadata key 压住后续 turn 的缓存滚动。
	if sessionHash != "" {
		return sessionHash, promptCacheKey
	}
	if userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String()); userID != "" {
		seed := reqModel + "-" + userID
		sessionHash = service.DeriveSessionHashFromSeed(seed)
	}
	return sessionHash, promptCacheKey
}

// anthropicErrorResponse writes an error in Anthropic Messages API format.
func (h *OpenAIGatewayHandler) anthropicErrorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// anthropicStreamingAwareError handles errors that may occur during streaming,
// using Anthropic SSE error format.
func (h *OpenAIGatewayHandler) anthropicStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if streamStarted {
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			errPayload, _ := json.Marshal(gin.H{
				"type": "error",
				"error": gin.H{
					"type":    errType,
					"message": message,
				},
			})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errPayload) //nolint:errcheck
			flusher.Flush()
		}
		return
	}
	h.anthropicErrorResponse(c, status, errType, message)
}

// handleAnthropicFailoverExhausted maps upstream failover errors to Anthropic format.
func (h *OpenAIGatewayHandler) handleAnthropicFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	if isOpenAITeamWorkspaceFailoverForContext(c, failoverErr, h.gatewayService.IsOpenAITeamLinkedResolverEnabled(c.Request.Context())) {
		h.anthropicStreamingAwareError(c, http.StatusServiceUnavailable, "team_workspace_deactivated", "OpenAI Team workspace is temporarily unavailable", streamStarted)
		return
	}
	status, errType, errMsg := h.mapUpstreamError(failoverErr.StatusCode)
	h.anthropicStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

// ensureAnthropicErrorResponse writes a fallback Anthropic error if no response was written.
func (h *OpenAIGatewayHandler) ensureAnthropicErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	// Once a transformed upstream event has been emitted, HTTP status cannot be
	// changed.  End the Anthropic SSE stream with its protocol error event rather
	// than silently returning a partial 200 response.
	if c.Writer.Written() {
		streamStarted = true
	}
	h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
	return true
}

func (h *OpenAIGatewayHandler) validateFunctionCallOutputRequest(c *gin.Context, body []byte, reqLog *zap.Logger) bool {
	if !gjson.GetBytes(body, `input.#(type=="function_call_output")`).Exists() {
		return true
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		// 保持原有容错语义：解析失败时跳过预校验，沿用后续上游校验结果。
		return true
	}

	c.Set(service.OpenAIParsedRequestBodyKey, reqBody)
	validation := service.ValidateFunctionCallOutputContext(reqBody)
	if !validation.HasFunctionCallOutput {
		return true
	}

	previousResponseID, _ := reqBody["previous_response_id"].(string)
	if strings.TrimSpace(previousResponseID) != "" || validation.HasToolCallContext {
		return true
	}

	if validation.HasFunctionCallOutputMissingCallID {
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "function_call_output_missing_call_id"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
		return false
	}
	if validation.HasItemReferenceForAllCallIDs {
		return true
	}

	reqLog.Warn("openai.request_validation_failed",
		zap.String("reason", "function_call_output_missing_item_reference"),
	)
	h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires item_reference ids matching each call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
	return false
}

func (h *OpenAIGatewayHandler) acquireResponsesUserSlot(
	c *gin.Context,
	userID int64,
	userConcurrency int,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), bool) {
	ctx := c.Request.Context()
	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, userID, userConcurrency)
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}
	if userAcquired {
		return wrapReleaseOnDone(ctx, userReleaseFunc), true
	}

	maxWait := service.CalculateMaxWait(userConcurrency)
	canWait, waitErr := h.concurrencyHelper.IncrementWaitCount(ctx, userID, maxWait)
	if waitErr != nil {
		reqLog.Warn("openai.user_wait_counter_increment_failed", zap.Error(waitErr))
		// 按现有降级语义：等待计数异常时放行后续抢槽流程
	} else if !canWait {
		reqLog.Info("openai.user_wait_queue_full", zap.Int("max_wait", maxWait))
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later")
		return nil, false
	}

	waitCounted := waitErr == nil && canWait
	defer func() {
		if waitCounted {
			h.concurrencyHelper.DecrementWaitCount(ctx, userID)
		}
	}()

	userReleaseFunc, err = h.concurrencyHelper.AcquireUserSlotWithWait(c, userID, userConcurrency, reqStream, streamStarted)
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed_after_wait", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}

	// 槽位获取成功后，立刻退出等待计数。
	if waitCounted {
		h.concurrencyHelper.DecrementWaitCount(ctx, userID)
		waitCounted = false
	}
	return wrapReleaseOnDone(ctx, userReleaseFunc), true
}

func (h *OpenAIGatewayHandler) acquireResponsesAccountSlot(
	c *gin.Context,
	groupID *int64,
	sessionHash string,
	selection *service.AccountSelectionResult,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), bool) {
	if selection == nil || selection.Account == nil {
		markOpsRoutingCapacityLimited(c)
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", *streamStarted)
		return nil, false
	}

	ctx := c.Request.Context()
	account := selection.Account
	if selection.Acquired {
		return wrapReleaseOnDone(ctx, selection.ReleaseFunc), true
	}
	if selection.WaitPlan == nil {
		markOpsRoutingCapacityLimited(c)
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", *streamStarted)
		return nil, false
	}

	fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
		ctx,
		account.ID,
		selection.WaitPlan.MaxConcurrency,
	)
	if err != nil {
		reqLog.Warn("openai.account_slot_quick_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.handleConcurrencyError(c, err, "account", *streamStarted)
		return nil, false
	}
	if fastAcquired {
		if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
		return wrapReleaseOnDone(ctx, fastReleaseFunc), true
	}

	canWait, waitErr := h.concurrencyHelper.IncrementAccountWaitCount(ctx, account.ID, selection.WaitPlan.MaxWaiting)
	if waitErr != nil {
		reqLog.Warn("openai.account_wait_counter_increment_failed", zap.Int64("account_id", account.ID), zap.Error(waitErr))
	} else if !canWait {
		reqLog.Info("openai.account_wait_queue_full",
			zap.Int64("account_id", account.ID),
			zap.Int("max_waiting", selection.WaitPlan.MaxWaiting),
		)
		h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later", *streamStarted)
		return nil, false
	}

	accountWaitCounted := waitErr == nil && canWait
	releaseWait := func() {
		if accountWaitCounted {
			h.concurrencyHelper.DecrementAccountWaitCount(ctx, account.ID)
			accountWaitCounted = false
		}
	}
	defer releaseWait()

	accountReleaseFunc, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(
		c,
		account.ID,
		selection.WaitPlan.MaxConcurrency,
		selection.WaitPlan.Timeout,
		reqStream,
		streamStarted,
	)
	if err != nil {
		reqLog.Warn("openai.account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.handleConcurrencyError(c, err, "account", *streamStarted)
		return nil, false
	}

	// Slot acquired: no longer waiting in queue.
	releaseWait()
	if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil {
		reqLog.Warn("openai.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
	return wrapReleaseOnDone(ctx, accountReleaseFunc), true
}

// ResponsesWebSocket handles OpenAI Responses API WebSocket ingress endpoint
// GET /openai/v1/responses (Upgrade: websocket)
func (h *OpenAIGatewayHandler) ResponsesWebSocket(c *gin.Context) {
	if !isOpenAIWSUpgradeRequest(c.Request) {
		h.errorResponse(c, http.StatusUpgradeRequired, "invalid_request_error", "WebSocket upgrade required (Upgrade: websocket)")
		return
	}
	setOpenAIClientTransportWS(c)

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses_ws",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.Bool("openai_ws_mode", true),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	bridgeGroup, bridgeLookupErr := h.gatewayService.IsCodexImageGenerationBridgeGroup(c.Request.Context(), apiKey.GroupID)
	if bridgeLookupErr != nil {
		reqLog.Warn("openai.websocket_image_bridge_group_lookup_failed", zap.Error(bridgeLookupErr))
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Codex image channel configuration is unavailable")
		return
	}
	if bridgeGroup {
		// Reject the upgrade before it is accepted. Codex natively retries the same
		// request through an HTTP Responses stream, which preserves incremental SSE
		// delivery during long image generations. Accepting WS first and buffering an
		// internal HTTP stream leaves the client idle and causes it to cancel/retry.
		reqLog.Info("openai.websocket_image_bridge_http_transport_required")
		h.errorResponse(c, http.StatusUpgradeRequired, "websocket_transport_unsupported", "Codex image channels require HTTPS Responses transport")
		return
	}
	reqLog.Info("openai.websocket_ingress_started")
	clientIP := ip.GetClientIP(c)
	userAgent := strings.TrimSpace(c.GetHeader("User-Agent"))

	// Keep downstream Codex WebSocket frames uncompressed. Some entry proxies and
	// clients advertise permessage-deflate but close the tunnel before the first
	// response.create frame is delivered. Disabling negotiation avoids that
	// fragile hop while preserving the WebSocket protocol itself.
	wsConn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{
		CompressionMode: coderws.CompressionDisabled,
	})
	if err != nil {
		reqLog.Warn("openai.websocket_accept_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("request_user_agent", userAgent),
			zap.String("upgrade_header", strings.TrimSpace(c.GetHeader("Upgrade"))),
			zap.String("connection_header", strings.TrimSpace(c.GetHeader("Connection"))),
			zap.String("sec_websocket_version", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Version"))),
			zap.Bool("has_sec_websocket_key", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Key")) != ""),
		)
		return
	}
	defer func() {
		_ = wsConn.CloseNow()
	}()
	wsConn.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(h.cfg))

	ctx := c.Request.Context()
	firstMessageTimeout := service.ResolveOpenAIWSClientFirstMessageTimeout(h.cfg)
	msgType, firstMessage, err := service.ReadOpenAIWSClientMessage(
		ctx,
		wsConn,
		firstMessageTimeout,
		coderws.StatusPolicyViolation,
		"missing first response.create message",
	)
	if err != nil {
		closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
		reqLog.Warn("openai.websocket_read_first_message_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("close_status", closeStatus),
			zap.String("close_reason", closeReason),
			zap.Duration("read_timeout", firstMessageTimeout),
		)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "missing first response.create message")
		return
	}
	if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "unsupported websocket message type")
		return
	}
	if !gjson.ValidBytes(firstMessage) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "invalid JSON payload")
		return
	}
	if turnMetadata := strings.TrimSpace(c.GetHeader("x-codex-turn-metadata")); turnMetadata != "" {
		firstMessage, err = service.AttachCodexTurnMetadata(firstMessage, turnMetadata)
		if err != nil {
			closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "invalid Codex turn metadata")
			return
		}
	}
	if compaction := service.ParseOpenAICompactionContext(firstMessage, c.Request.Header, "/v1/responses"); compaction.NativeResponses && !h.gatewayService.IsOpenAIRemoteCompactionV2Enabled(ctx) {
		writeOpenAIRemoteCompactionV2DisabledWSError(ctx, wsConn)
		closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "remote_compaction_v2_disabled")
		return
	}

	reqModel := strings.TrimSpace(gjson.GetBytes(firstMessage, "model").String())
	if reqModel == "" {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "model is required in first response.create payload")
		return
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(firstMessage, "previous_response_id").String())
	previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
	if previousResponseID != "" && previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "previous_response_id must be a response.id (resp_*), not a message id")
		return
	}
	reqLog = reqLog.With(
		zap.Bool("ws_ingress", true),
		zap.String("model", reqModel),
		zap.Bool("has_previous_response_id", previousResponseID != ""),
		zap.String("previous_response_id_kind", previousResponseIDKind),
	)
	setOpsRequestContext(c, reqModel, true)
	setOpsEndpointContext(c, "", int16(service.RequestTypeWSV2))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, firstMessage); decision != nil && decision.Blocked {
		writeContentModerationWSError(ctx, wsConn, decision)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, decision.Message)
		return
	}

	if service.IsImageGenerationPermissionIntentForPlatform("/v1/responses", reqModel, firstMessage, openAICompatibleRequestPlatform(apiKey)) && !service.GroupAllowsImageGeneration(apiKey.Group) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, service.ImageGenerationPermissionMessage())
		return
	}

	// Use the same role/capability/execution classifier as HTTP. WebSocket is a
	// transport detail and must not turn a mapped background request into image
	// generation or reject a valid hosted-image user turn.
	codexRouteWS := h.gatewayService.ResolveCodexImageGenerationRoute(ctx, apiKey.GroupID, reqModel)
	if codexRouteWS.ConfigurationError != "" {
		closeOpenAIClientWS(wsConn, coderws.StatusInternalError, codexRouteWS.ConfigurationError)
		return
	}
	channelMappingWS := codexRouteWS.Mapping
	codexDecisionWS := service.CodexImageRequestDecision{Execution: service.CodexImageExecutionOrdinary}
	if codexRouteWS.Enabled {
		codexDecisionWS = service.ClassifyCodexImageRequest(reqModel, codexRouteWS.Mapping.MappedModel, firstMessage)
		reqLog.Info("openai.codex_image_route_classified",
			zap.String("request_role", string(codexDecisionWS.Role)),
			zap.String("execution", string(codexDecisionWS.Execution)),
			zap.Bool("has_metadata", codexDecisionWS.HasMetadata),
			zap.Bool("has_extension", codexDecisionWS.HasExtension),
			zap.Bool("has_hosted_image", codexDecisionWS.HasHostedImage),
			zap.Bool("legacy_fallback", codexDecisionWS.LegacyFallback),
			zap.String("requested_model", reqModel),
			zap.String("mapped_model", codexRouteWS.Mapping.MappedModel),
		)
	}
	if codexDecisionWS.IsImageExecution() && !service.GroupAllowsImageGeneration(apiKey.Group) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, service.ImageGenerationPermissionMessage())
		return
	}
	selectionGroupIDWS := apiKey.GroupID
	if codexDecisionWS.UsesOrchestratorGroup() {
		selectionGroupIDWS = codexRouteWS.OrchestratorGroupID
	}
	selectionModelWS := resolveChannelMappedAccountSelectionModel(reqModel, channelMappingWS, !codexDecisionWS.UsesOrchestratorGroup())
	codexImageExtensionCandidateWS := codexDecisionWS.Execution == service.CodexImageExecutionExtension
	codexSystemBackgroundTurnWS := codexDecisionWS.Execution == service.CodexImageExecutionTextBypass && service.IsCodexSystemBackgroundTurn(firstMessage)
	wsRouteFamily := string(codexDecisionWS.Execution)
	usageChannelMappingWS := channelMappingWS
	if codexDecisionWS.Execution == service.CodexImageExecutionTextBypass {
		usageChannelMappingWS.ChannelID = 0
		usageChannelMappingWS.Mapped = false
		usageChannelMappingWS.MappedModel = reqModel
		usageChannelMappingWS.BillingModelSource = service.BillingModelSourceRequested
	}
	c.Set(service.OpenAICodexImageGenerationExtensionContextKey, false)
	c.Set(service.OpenAICodexSystemBackgroundContextKey, codexSystemBackgroundTurnWS)
	isOpenAIImageModel := func(model string) bool {
		return service.IsImageGenerationIntent("/v1/responses", model, []byte(`{}`))
	}
	isOrdinaryNativeImage := func(payload []byte, model string, route service.CodexImageGenerationRoute) bool {
		if route.Enabled || !service.GroupAllowsImageGeneration(apiKey.Group) || isOpenAIImageModel(model) {
			return false
		}
		if route.Mapping.Mapped && isOpenAIImageModel(route.Mapping.MappedModel) {
			return false
		}
		return service.IsImageGenerationIntent("/v1/responses", model, payload)
	}
	isUnsupportedOrdinaryWSImage := func(payload []byte, model string, route service.CodexImageGenerationRoute) bool {
		if route.Enabled {
			decision := service.ClassifyCodexImageRequest(model, route.Mapping.MappedModel, payload)
			if decision.Execution != service.CodexImageExecutionOrdinary {
				return false
			}
		}
		if !service.GroupAllowsImageGeneration(apiKey.Group) {
			requestPlatform := openAICompatibleRequestPlatform(apiKey)
			if service.IsImageGenerationPermissionIntentForPlatform("/v1/responses", model, payload, requestPlatform) {
				return true
			}
			return route.Mapping.Mapped && service.IsImageGenerationPermissionIntentForPlatform("/v1/responses", route.Mapping.MappedModel, payload, requestPlatform)
		}
		if isOrdinaryNativeImage(payload, model, route) {
			return false
		}
		if service.IsImageGenerationIntent("/v1/responses", model, payload) {
			return true
		}
		return route.Mapping.Mapped && service.IsImageGenerationIntent("/v1/responses", route.Mapping.MappedModel, payload)
	}
	if isUnsupportedOrdinaryWSImage(firstMessage, reqModel, codexRouteWS) {
		reqLog.Info("openai.websocket_image_generation_unsupported",
			zap.Int("turn", 1),
			zap.String("mapped_model", channelMappingWS.MappedModel),
			zap.Bool("channel_mapped", channelMappingWS.Mapped),
		)
		writeOpenAIWSImageGenerationUnsupported(ctx, wsConn, reqModel)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, openAIWSImageGenerationUnsupportedMessage)
		return
	}
	classifyWSRouteFamily := func(payload []byte, model string) string {
		route := h.gatewayService.ResolveCodexImageGenerationRoute(ctx, apiKey.GroupID, model)
		if route.ConfigurationError != "" {
			return "invalid"
		}
		if !route.Enabled {
			if isOrdinaryNativeImage(payload, model, route) {
				return "native_hosted_image"
			}
			return string(service.CodexImageExecutionOrdinary)
		}
		return string(service.ClassifyCodexImageRequest(model, route.Mapping.MappedModel, payload).Execution)
	}

	var currentUserRelease func()
	var currentAccountRelease func()
	releaseTurnSlots := func() {
		if currentAccountRelease != nil {
			currentAccountRelease()
			currentAccountRelease = nil
		}
		if currentUserRelease != nil {
			currentUserRelease()
			currentUserRelease = nil
		}
	}
	// 必须尽早注册，确保任何 early return 都能释放已获取的并发槽位。
	defer releaseTurnSlots()

	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, subject.UserID, subject.Concurrency)
	if err != nil {
		reqLog.Warn("openai.websocket_user_slot_acquire_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
		return
	}
	if !userAcquired {
		closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
		return
	}
	currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(apiKey)
	if err := h.billingCacheService.CheckBillingEligibility(ctx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai.websocket_billing_eligibility_check_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "billing check failed")
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHashWithFallback(
		c,
		firstMessage,
		openAIWSIngressFallbackSessionSeed(subject.UserID, apiKey.ID, apiKey.GroupID),
	)
	applyWSChannelMapping := func(payload []byte, model string) ([]byte, error) {
		route := h.gatewayService.ResolveCodexImageGenerationRoute(ctx, apiKey.GroupID, model)
		if route.ConfigurationError != "" {
			return nil, errors.New(route.ConfigurationError)
		}
		mapping := route.Mapping
		if !mapping.Mapped {
			return payload, nil
		}
		decision := service.CodexImageRequestDecision{Execution: service.CodexImageExecutionOrdinary}
		if route.Enabled {
			decision = service.ClassifyCodexImageRequest(model, mapping.MappedModel, payload)
		}
		if decision.Execution != service.CodexImageExecutionOrdinary {
			return service.PrepareCodexImageRouteRequest(payload, model, mapping.MappedModel, decision)
		}
		return h.gatewayService.ReplaceModelInBody(payload, mapping.MappedModel), nil
	}
	excludedAccountIDs := map[int64]struct{}{}
	canSwitchAccountSilently := previousResponseID == ""
	ordinaryNativeImageWS := isOrdinaryNativeImage(firstMessage, reqModel, codexRouteWS)
	selectionTransportWS := service.OpenAIUpstreamTransportResponsesWebsocketV2
	if requestPlatform == service.PlatformGrok {
		selectionTransportWS = service.OpenAIUpstreamTransportHTTPSSE
	}
	if previousResponseID == "" && (codexImageExtensionCandidateWS || codexDecisionWS.Execution == service.CodexImageExecutionHostedImage || ordinaryNativeImageWS) {
		selectionTransportWS = service.OpenAIUpstreamTransportAny
	}
	requiredCapabilityWS := service.OpenAIEndpointCapabilityChatCompletions
	if requestPlatform == service.PlatformOpenAI && service.IsExplicitImageGenerationIntent("/v1/responses", reqModel, firstMessage) {
		requiredCapabilityWS = service.OpenAIEndpointCapabilityResponses
	}

	for attempt := 1; attempt <= maxOpenAIWSSilentSwitchAttempts; attempt++ {
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			ctx,
			selectionGroupIDWS,
			previousResponseID,
			sessionHash,
			selectionModelWS,
			excludedAccountIDs,
			selectionTransportWS,
			requiredCapabilityWS,
			false,
			previousResponseID == "",
			true,
			requestPlatform,
		)
		if err != nil && previousResponseID == "" && requestPlatform == service.PlatformOpenAI && selectionTransportWS == service.OpenAIUpstreamTransportResponsesWebsocketV2 {
			// Prefer a reusable upstream WebSocket. If the group only has an
			// OpenAI-compatible Chat Completions account, allow the first turn to
			// select it and bridge the downstream Codex WebSocket through HTTP.
			selection, scheduleDecision, err = h.gatewayService.SelectAccountWithSchedulerForCapability(
				ctx,
				selectionGroupIDWS,
				previousResponseID,
				sessionHash,
				selectionModelWS,
				excludedAccountIDs,
				service.OpenAIUpstreamTransportAny,
				requiredCapabilityWS,
				false,
				true,
				true,
				requestPlatform,
			)
			if err == nil {
				reqLog.Info("openai.websocket_account_http_fallback_selection")
			}
		}
		if err != nil {
			reqLog.Warn("openai.websocket_account_select_failed", zap.Error(err), zap.Int("silent_retry_attempt", attempt))
			if wsTeamBlocked(c) {
				writeOpenAITeamWorkspaceWSError(ctx, wsConn)
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "team_workspace_deactivated")
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			return
		}
		if selection == nil || selection.Account == nil {
			if wsTeamBlocked(c) {
				writeOpenAITeamWorkspaceWSError(ctx, wsConn)
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "team_workspace_deactivated")
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			return
		}

		account := selection.Account
		useCodexImageExtensionWS := codexImageExtensionCandidateWS
		c.Set(service.OpenAICodexImageGenerationExtensionContextKey, useCodexImageExtensionWS)
		wsFirstMessage := firstMessage
		if channelMappingWS.Mapped {
			mappedBody, mapErr := applyWSChannelMapping(firstMessage, reqModel)
			if mapErr != nil {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "invalid websocket request payload")
				return
			}
			wsFirstMessage = mappedBody
		}
		accountMaxConcurrency := account.Concurrency
		if selection.WaitPlan != nil && selection.WaitPlan.MaxConcurrency > 0 {
			accountMaxConcurrency = selection.WaitPlan.MaxConcurrency
		}
		accountReleaseFunc := selection.ReleaseFunc
		if !selection.Acquired {
			if selection.WaitPlan == nil {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
				ctx,
				account.ID,
				selection.WaitPlan.MaxConcurrency,
			)
			if err != nil {
				reqLog.Warn("openai.websocket_account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
				closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire account concurrency slot")
				return
			}
			if !fastAcquired {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			accountReleaseFunc = fastReleaseFunc
		}
		currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
		if err := h.gatewayService.BindStickySession(ctx, selectionGroupIDWS, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.websocket_bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}

		token, _, err := h.gatewayService.GetAccessToken(ctx, account)
		if err != nil {
			reqLog.Warn("openai.websocket_get_access_token_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to get access token")
			return
		}

		reqLog.Debug("openai.websocket_account_selected",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("schedule_layer", scheduleDecision.Layer),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("silent_retry_attempt", attempt),
		)

		wsProtocolDecision := h.gatewayService.ResolveOpenAIWSProtocol(account)
		if previousResponseID == "" && wsProtocolDecision.Transport == service.OpenAIUpstreamTransportHTTPSSE {
			reqLog.Info("openai.websocket_account_http_transport_selected",
				zap.Int64("account_id", account.ID),
				zap.String("reason", wsProtocolDecision.Reason),
			)
			handled := h.tryFallbackOpenAIWebSocketIngressToHTTP(c, wsConn, reqLog, apiKey, account, reqModel, wsFirstMessage, usageChannelMappingWS)
			if currentAccountRelease != nil {
				currentAccountRelease()
				currentAccountRelease = nil
			}
			if !handled && wsTeamBlocked(c) && attempt == 1 {
				excludedAccountIDs[account.ID] = struct{}{}
				continue
			}
			if !handled && wsTeamBlocked(c) {
				writeOpenAITeamWorkspaceWSError(ctx, wsConn)
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "team_workspace_deactivated")
				return
			}
			if !handled {
				closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "http upstream fallback failed")
			}
			return
		}

		completedWSTurns := 0
		hooks := &service.OpenAIWSIngressHooks{
			InitialRequestModel: reqModel,
			BeforeRequest: func(turn int, payload []byte, originalModel string) ([]byte, error) {
				if turn == 1 {
					return payload, nil
				}
				if !gjson.ValidBytes(payload) {
					return nil, service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid websocket request payload", errors.New("invalid json"))
				}
				model := strings.TrimSpace(originalModel)
				if model == "" {
					model = strings.TrimSpace(gjson.GetBytes(payload, "model").String())
				}
				if model == "" {
					model = reqModel
				}
				if turnFamily := classifyWSRouteFamily(payload, model); turnFamily != wsRouteFamily {
					reqLog.Info("openai.websocket_route_family_changed",
						zap.Int("turn", turn),
						zap.String("connection_family", wsRouteFamily),
						zap.String("turn_family", turnFamily),
					)
					return nil, service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "request route changed; reconnect and retry", nil)
				}
				route := h.gatewayService.ResolveCodexImageGenerationRoute(ctx, apiKey.GroupID, model)
				if isUnsupportedOrdinaryWSImage(payload, model, route) {
					reqLog.Info("openai.websocket_image_generation_unsupported",
						zap.Int("turn", turn),
						zap.String("mapped_model", route.Mapping.MappedModel),
						zap.Bool("channel_mapped", route.Mapping.Mapped),
					)
					writeOpenAIWSImageGenerationUnsupported(ctx, wsConn, model)
					return nil, service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, openAIWSImageGenerationUnsupportedMessage, nil)
				}
				mappedPayload, mapErr := applyWSChannelMapping(payload, model)
				if mapErr != nil {
					return nil, service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid websocket request payload", mapErr)
				}
				if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, model, payload); decision != nil && decision.Blocked {
					writeContentModerationWSError(ctx, wsConn, decision)
					return nil, service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, decision.Message, nil)
				}
				return mappedPayload, nil
			},
			BeforeTurn: func(turn int) error {
				if turn == 1 {
					return nil
				}
				// 防御式清理：避免异常路径下旧槽位覆盖导致泄漏。
				releaseTurnSlots()
				// 非首轮 turn 需要重新抢占并发槽位，避免长连接空闲占槽。
				userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, subject.UserID, subject.Concurrency)
				if err != nil {
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire user concurrency slot", err)
				}
				if !userAcquired {
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "too many concurrent requests, please retry later", nil)
				}
				accountReleaseFunc, accountAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, accountMaxConcurrency)
				if err != nil {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire account concurrency slot", err)
				}
				if !accountAcquired {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "account is busy, please retry later", nil)
				}
				currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
				currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
				return nil
			},
			AfterTurn: func(turn int, result *service.OpenAIForwardResult, turnErr error) {
				releaseTurnSlots()
				if turnErr != nil {
					if result == nil || result.ImageCount <= 0 {
						return
					}
					reqLog.Warn("openai.websocket_partial_error_with_image_result",
						zap.Int64("account_id", account.ID),
						zap.Int("image_count", result.ImageCount),
						zap.Error(turnErr),
					)
				}
				if result == nil {
					return
				}
				completedWSTurns++
				if useCodexImageExtensionWS || codexSystemBackgroundTurnWS {
					return
				}
				if account.Type == service.AccountTypeOAuth {
					h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, result.ResponseHeaders)
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
				inboundEndpoint := GetInboundEndpoint(c)
				upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
				upstreamAttempts := usageUpstreamAttemptsSnapshot(c)
				h.submitOpenAIUsageRecordTask(result, func(taskCtx context.Context) {
					if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
						Result:                 result,
						APIKey:                 apiKey,
						User:                   apiKey.User,
						Account:                account,
						UpstreamCostMultiplier: service.CloneDecimalSnapshot(account.UpstreamCostMultiplier),
						Subscription:           subscription,
						InboundEndpoint:        inboundEndpoint,
						UpstreamEndpoint:       upstreamEndpoint,
						UserAgent:              userAgent,
						IPAddress:              clientIP,
						RequestPayloadHash:     service.HashUsageRequestPayload(firstMessage),
						APIKeyService:          h.apiKeyService,
						ChannelUsageFields:     usageChannelMappingWS.ToUsageFields(reqModel, result.UpstreamModel),
						UpstreamAttempts:       service.CloneUsageUpstreamAttempts(upstreamAttempts),
					}); err != nil {
						reqLog.Error("openai.websocket_record_usage_failed",
							zap.Int64("account_id", account.ID),
							zap.String("request_id", result.RequestID),
							zap.Error(err),
						)
					}
				})
			},
		}

		// Image-producing Responses turns use the HTTP upstream path even when the
		// Codex client connected over WebSocket. Extension turns need namespace
		// restoration, and hosted image turns are not portable across WS providers.
		if (useCodexImageExtensionWS || codexDecisionWS.Execution == service.CodexImageExecutionHostedImage || ordinaryNativeImageWS) && previousResponseID == "" {
			handled := h.tryFallbackOpenAIWebSocketIngressToHTTP(c, wsConn, reqLog, apiKey, account, reqModel, wsFirstMessage, usageChannelMappingWS)
			if currentAccountRelease != nil {
				currentAccountRelease()
				currentAccountRelease = nil
			}
			if !handled {
				// These request families deliberately use an HTTP upstream. Falling
				// through to the WebSocket proxy would call the same image account a
				// second time and could duplicate a successful-but-undelivered image.
				closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "image http upstream failed")
			}
			return
		}

		if err := h.gatewayService.ProxyResponsesWebSocketFromClient(ctx, c, wsConn, account, token, wsFirstMessage, hooks); err != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
			closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
			reqLog.Warn("openai.websocket_proxy_failed",
				zap.Int64("account_id", account.ID),
				zap.Int("silent_retry_attempt", attempt),
				zap.Error(err),
				zap.String("close_status", closeStatus),
				zap.String("close_reason", closeReason),
			)

			// Record WS upstream error so OpsErrorLoggerMiddleware can persist it.
			// WS requests have HTTP 101 status, which bypasses the normal error path.
			if closeStatus != "" && closeStatus != "-" {
				service.AppendOpsUpstreamError(c, service.OpsUpstreamErrorEvent{
					AccountID: account.ID,
					Platform:  account.Platform,
					Kind:      "ws_error",
					Message:   closeReason,
					Detail:    closeStatus + ": " + err.Error(),
				})
			}

			if currentAccountRelease != nil {
				currentAccountRelease()
				currentAccountRelease = nil
			}

			teamWorkspaceResolverEnabled := h.gatewayService.IsOpenAITeamLinkedResolverEnabled(c.Request.Context())
			teamWorkspaceBlocked := isOpenAITeamWorkspaceFailoverForAccount(account, err, teamWorkspaceResolverEnabled)
			teamSwitchAlreadyUsed := wsTeamBlocked(c)
			if teamWorkspaceBlocked {
				c.Set(openAITeamWorkspaceBlockedContextKey, true)
			}
			if shouldSilentlySwitchOpenAIWSAccount(canSwitchAccountSilently, completedWSTurns, attempt, err, teamSwitchAlreadyUsed, teamWorkspaceResolverEnabled, account.Platform) {
				excludedAccountIDs[account.ID] = struct{}{}
				reqLog.Info("openai.websocket_silent_retry_switch_account",
					zap.Int64("failed_account_id", account.ID),
					zap.Int("next_attempt", attempt+1),
					zap.String("reason", closeReason),
				)
				continue
			}
			if teamWorkspaceBlocked {
				writeOpenAITeamWorkspaceWSError(ctx, wsConn)
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "team_workspace_deactivated")
				return
			}

			var closeErr *service.OpenAIWSClientCloseError
			if errors.As(err, &closeErr) {
				closeOpenAIClientWS(wsConn, closeErr.StatusCode(), closeErr.Reason())
				return
			}
			if service.IsOpenAIWSHTTPFallbackSafe(err) && h.tryFallbackOpenAIWebSocketIngressToHTTP(c, wsConn, reqLog, apiKey, account, reqModel, wsFirstMessage, usageChannelMappingWS) {
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "upstream websocket proxy failed")
			return
		}
		reqLog.Info("openai.websocket_ingress_closed", zap.Int64("account_id", account.ID))
		return
	}

	closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "upstream websocket unavailable")
}

func (h *OpenAIGatewayHandler) tryFallbackOpenAIWebSocketIngressToHTTP(
	c *gin.Context,
	wsConn *coderws.Conn,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	account *service.Account,
	requestedModel string,
	body []byte,
	channelMappingWS service.ChannelMappingResult,
) bool {
	if h == nil || h.gatewayService == nil || c == nil || wsConn == nil || account == nil {
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return false
	}
	forwardBody := body
	if strings.TrimSpace(gjson.GetBytes(body, "type").String()) == "response.create" {
		var normalizeErr error
		forwardBody, normalizeErr = sjson.DeleteBytes(body, "type")
		if normalizeErr != nil {
			if reqLog != nil {
				reqLog.Warn("openai.websocket_http_fallback_request_normalize_failed", zap.Error(normalizeErr))
			}
			return false
		}
	}
	fallbackRecorder := newOpenAIHTTPFallbackRecorder()
	fallbackCtx, _ := gin.CreateTestContext(fallbackRecorder)
	fallbackCtx.Request = c.Request.Clone(c.Request.Context())
	fallbackCtx.Request.Body = nil
	for key, value := range c.Keys {
		if key == service.OpenAIParsedRequestBodyKey {
			continue
		}
		fallbackCtx.Set(key, value)
	}
	fallbackCtx.Set(service.OpenAICodexImageGenerationToolCalledContextKey, false)
	setOpenAIClientTransportHTTP(fallbackCtx)
	result, err := h.gatewayService.Forward(fallbackCtx.Request.Context(), fallbackCtx, account, forwardBody)
	if err != nil {
		if isOpenAITeamWorkspaceFailover(err, h.gatewayService.IsOpenAITeamLinkedResolverEnabled(c.Request.Context())) {
			c.Set(openAITeamWorkspaceBlockedContextKey, true)
		}
		// Forward normally writes the upstream error before returning it. Relay
		// that structured failure to Codex when available; the caller still treats
		// the HTTP attempt as terminal for image request families.
		if len(fallbackRecorder.BodyBytes()) > 0 {
			_ = writeOpenAIHTTPFallbackBodyToWS(c.Request.Context(), wsConn, fallbackRecorder)
		}
		if reqLog != nil {
			reqLog.Warn("openai.websocket_http_fallback_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
		return false
	}
	if result == nil {
		return false
	}
	codexImageToolCalled := service.CodexImageGenerationToolCalled(fallbackCtx)
	if codexImageToolCalled {
		c.Set(service.OpenAICodexImageGenerationToolCalledContextKey, true)
	}
	if !writeOpenAIHTTPFallbackBodyToWS(c.Request.Context(), wsConn, fallbackRecorder) {
		return false
	}
	if reqLog != nil {
		reqLog.Info("openai.websocket_http_fallback_succeeded",
			zap.Int64("account_id", account.ID),
			zap.String("request_id", result.RequestID),
		)
	}
	h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
	codexImageExtension, _ := c.Get(service.OpenAICodexImageGenerationExtensionContextKey)
	codexImageExtensionEnabled, _ := codexImageExtension.(bool)
	codexSystemBackground, _ := c.Get(service.OpenAICodexSystemBackgroundContextKey)
	codexSystemBackgroundEnabled, _ := codexSystemBackground.(bool)
	if apiKey != nil && (!codexImageExtensionEnabled || !codexImageToolCalled) && !codexSystemBackgroundEnabled {
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
		usageChannelMapping := channelMappingWS
		if codexImageExtensionEnabled && !codexImageToolCalled {
			usageChannelMapping.ChannelID = 0
			usageChannelMapping.Mapped = false
			usageChannelMapping.MappedModel = requestedModel
			usageChannelMapping.BillingModelSource = service.BillingModelSourceRequested
		}
		upstreamAttempts := usageUpstreamAttemptsSnapshot(c)
		h.submitOpenAIUsageRecordTask(result, func(taskCtx context.Context) {
			if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
				Result:                 result,
				APIKey:                 apiKey,
				User:                   apiKey.User,
				Account:                account,
				UpstreamCostMultiplier: service.CloneDecimalSnapshot(account.UpstreamCostMultiplier),
				InboundEndpoint:        inboundEndpoint,
				UpstreamEndpoint:       upstreamEndpoint,
				UserAgent:              strings.TrimSpace(c.GetHeader("User-Agent")),
				IPAddress:              ip.GetClientIP(c),
				RequestPayloadHash:     service.HashUsageRequestPayload(body),
				APIKeyService:          h.apiKeyService,
				ChannelUsageFields:     usageChannelMapping.ToUsageFields(requestedModel, result.UpstreamModel),
				UpstreamAttempts:       service.CloneUsageUpstreamAttempts(upstreamAttempts),
			}); err != nil && reqLog != nil {
				reqLog.Error("openai.websocket_http_fallback_record_usage_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			}
		})
	}
	closeOpenAIClientWS(wsConn, coderws.StatusNormalClosure, "fallback to http completed")
	return true
}

const openAITeamWorkspaceBlockedContextKey = "openai_ws_team_workspace_blocked"
const openAITeamWorkspacePlatformContextKey = "openai_team_workspace_platform"
const maxOpenAIWSSilentSwitchAttempts = 3

func isOpenAITeamWorkspaceFailover(err error, resolverEnabled ...bool) bool {
	if len(resolverEnabled) > 0 && !resolverEnabled[0] {
		return false
	}
	var failoverErr *service.UpstreamFailoverError
	return errors.As(err, &failoverErr) && failoverErr != nil && service.IsOpenAITeamWorkspaceDeactivated(failoverErr.StatusCode, failoverErr.ResponseBody)
}

func isOpenAITeamWorkspaceFailoverForAccount(account *service.Account, err error, resolverEnabled bool) bool {
	if account == nil || account.Platform != service.PlatformOpenAI {
		return false
	}
	return isOpenAITeamWorkspaceFailover(err, resolverEnabled)
}

func isOpenAITeamWorkspaceFailoverForContext(c *gin.Context, err error, resolverEnabled bool) bool {
	if c == nil || !resolverEnabled {
		return false
	}
	platform, _ := c.Get(openAITeamWorkspacePlatformContextKey)
	if p, ok := platform.(string); ok && p != "" && p != service.PlatformOpenAI {
		return false
	}
	return isOpenAITeamWorkspaceFailover(err, true)
}

func shouldSilentlySwitchOpenAIWSAccount(canSwitch bool, completedTurns, attempt int, err error, teamSwitchAlreadyUsed bool, resolverEnabled bool, platform ...string) bool {
	if !canSwitch || completedTurns != 0 || attempt >= maxOpenAIWSSilentSwitchAttempts {
		return false
	}
	if teamSwitchAlreadyUsed {
		return false
	}
	if isOpenAITeamWorkspaceFailover(err, resolverEnabled) {
		if len(platform) > 0 && platform[0] != string(service.PlatformOpenAI) {
			return false
		}
		return attempt == 1
	}
	return service.IsOpenAIWSSilentRetrySafe(err)
}

func wsTeamBlocked(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(openAITeamWorkspaceBlockedContextKey)
	blocked, _ := value.(bool)
	return ok && blocked
}

func writeOpenAITeamWorkspaceWSError(ctx context.Context, conn *coderws.Conn) {
	if conn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload := []byte(`{"event_id":"evt_team_workspace_deactivated","type":"error","error":{"type":"api_error","code":"team_workspace_deactivated","message":"OpenAI Team workspace is temporarily unavailable"}}`)
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

func writeOpenAIHTTPFallbackBodyToWS(ctx context.Context, wsConn *coderws.Conn, recorder *openAIHTTPFallbackRecorder) bool {
	if wsConn == nil || recorder == nil {
		return false
	}
	body := recorder.BodyBytes()
	if recorder.Code() >= 400 {
		msg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
		if msg == "" {
			msg = http.StatusText(recorder.Code())
		}
		payload := fmt.Sprintf(`{"type":"response.failed","error":{"type":"upstream_error","message":%q}}`, msg)
		return wsConn.Write(ctx, coderws.MessageText, []byte(payload)) == nil
	}
	contentType := strings.ToLower(recorder.Header().Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") || bytes.Contains(body, []byte("data:")) {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 16*1024*1024)
		wrote := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			if err := wsConn.Write(ctx, coderws.MessageText, []byte(payload)); err != nil {
				return false
			}
			wrote = true
		}
		return wrote && scanner.Err() == nil
	}
	trimmedBody := bytes.TrimSpace(body)
	if len(trimmedBody) == 0 || !gjson.ValidBytes(trimmedBody) {
		return false
	}
	if eventType := strings.TrimSpace(gjson.GetBytes(trimmedBody, "type").String()); strings.HasPrefix(eventType, "response.") {
		return wsConn.Write(ctx, coderws.MessageText, trimmedBody) == nil
	}
	payload := []byte(`{"type":"response.completed","response":` + string(trimmedBody) + `}`)
	return wsConn.Write(ctx, coderws.MessageText, payload) == nil
}

func (h *OpenAIGatewayHandler) recoverResponsesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := false
	if streamStarted != nil {
		started = *streamStarted
	}
	wroteFallback := h.ensureForwardErrorResponse(c, started)
	requestLogger(c, "handler.openai_gateway.responses").Error(
		"openai.responses_panic_recovered",
		zap.Bool("fallback_error_response_written", wroteFallback),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
}

// recoverAnthropicMessagesPanic recovers from panics in the Anthropic Messages
// handler and returns an Anthropic-formatted error response.
func (h *OpenAIGatewayHandler) recoverAnthropicMessagesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := streamStarted != nil && *streamStarted
	requestLogger(c, "handler.openai_gateway.messages").Error(
		"openai.messages_panic_recovered",
		zap.Bool("stream_started", started),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
	if !started {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "Internal server error")
	}
}

func (h *OpenAIGatewayHandler) ensureResponsesDependencies(c *gin.Context, reqLog *zap.Logger) bool {
	missing := h.missingResponsesDependencies()
	if len(missing) == 0 {
		return true
	}

	if reqLog == nil {
		reqLog = requestLogger(c, "handler.openai_gateway.responses")
	}
	reqLog.Error("openai.handler_dependencies_missing", zap.Strings("missing_dependencies", missing))

	if c != nil && c.Writer != nil && !c.Writer.Written() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"type":    "api_error",
				"message": "Service temporarily unavailable",
			},
		})
	}
	return false
}

func (h *OpenAIGatewayHandler) missingResponsesDependencies() []string {
	missing := make([]string, 0, 5)
	if h == nil {
		return append(missing, "handler")
	}
	if h.gatewayService == nil {
		missing = append(missing, "gatewayService")
	}
	if h.billingCacheService == nil {
		missing = append(missing, "billingCacheService")
	}
	if h.apiKeyService == nil {
		missing = append(missing, "apiKeyService")
	}
	if h.concurrencyHelper == nil || h.concurrencyHelper.concurrencyService == nil {
		missing = append(missing, "concurrencyHelper")
	}
	return missing
}

func getContextInt64(c *gin.Context, key string) (int64, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	v, ok := c.Get(key)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}

func (h *OpenAIGatewayHandler) submitUsageRecordTask(task service.UsageRecordTask) {
	if task == nil {
		return
	}
	if h.usageRecordWorkerPool != nil {
		h.usageRecordWorkerPool.Submit(task)
		return
	}
	// 回退路径：worker 池未注入时同步执行，避免退回到无界 goroutine 模式。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.responses"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) submitOpenAIUsageRecordTask(result *service.OpenAIForwardResult, task service.UsageRecordTask) {
	if result != nil && result.ImageCount > 0 {
		h.submitMandatoryUsageRecordTask(task)
		return
	}
	h.submitUsageRecordTask(task)
}

func (h *OpenAIGatewayHandler) submitMandatoryUsageRecordTask(task service.UsageRecordTask) {
	if task == nil {
		return
	}
	if h.usageRecordWorkerPool != nil {
		if mode := h.usageRecordWorkerPool.Submit(task); mode != service.UsageRecordSubmitModeDropped {
			return
		}
		logger.L().With(
			zap.String("component", "handler.openai_gateway.usage"),
		).Warn("openai.usage_record_task_mandatory_sync_fallback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.usage"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) acquireImageGenerationSlot(c *gin.Context, streamStarted bool) (func(), bool) {
	if h == nil || h.cfg == nil || h.imageLimiter == nil {
		return nil, true
	}
	imageConcurrency := h.cfg.Gateway.ImageConcurrency
	wait := strings.TrimSpace(imageConcurrency.OverflowMode) == config.ImageConcurrencyOverflowModeWait
	release, acquired := h.imageLimiter.Acquire(
		c.Request.Context(),
		imageConcurrency.Enabled,
		imageConcurrency.MaxConcurrentRequests,
		wait,
		time.Duration(imageConcurrency.WaitTimeoutSeconds)*time.Second,
		imageConcurrency.MaxWaitingRequests,
	)
	if acquired {
		return release, true
	}
	h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Image generation concurrency limit exceeded, please retry later", streamStarted)
	return nil, false
}

// handleConcurrencyError handles concurrency-related errors with proper 429 response
func (h *OpenAIGatewayHandler) handleConcurrencyError(c *gin.Context, err error, slotType string, streamStarted bool) {
	h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error",
		fmt.Sprintf("Concurrency limit exceeded for %s, please retry later", slotType), streamStarted)
}

func (h *OpenAIGatewayHandler) handleFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	statusCode := failoverErr.StatusCode
	responseBody := failoverErr.ResponseBody
	if isOpenAITeamWorkspaceFailoverForContext(c, failoverErr, h.gatewayService.IsOpenAITeamLinkedResolverEnabled(c.Request.Context())) {
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "team_workspace_deactivated", "OpenAI Team workspace is temporarily unavailable", streamStarted)
		return
	}
	if service.IsOpenAISilentRefusalErrorBody(responseBody) {
		service.SetOpsUpstreamError(c, statusCode, service.OpenAISilentRefusalClientMessage(), "")
		h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", service.OpenAISilentRefusalClientMessage(), streamStarted)
		return
	}

	// 先检查透传规则
	if h.errorPassthroughService != nil && len(responseBody) > 0 {
		if rule := h.errorPassthroughService.MatchRule("openai", statusCode, responseBody); rule != nil {
			// 确定响应状态码
			respCode := statusCode
			if !rule.PassthroughCode && rule.ResponseCode != nil {
				respCode = *rule.ResponseCode
			}

			// 确定响应消息
			msg := service.ExtractUpstreamErrorMessage(responseBody)
			if !rule.PassthroughBody && rule.CustomMessage != nil {
				msg = *rule.CustomMessage
			}

			if rule.SkipMonitoring {
				c.Set(service.OpsSkipPassthroughKey, true)
			}

			h.handleStreamingAwareError(c, respCode, "upstream_error", msg, streamStarted)
			return
		}
	}

	// 记录原始上游状态码，以便 ops 错误日志捕获真实的上游错误
	upstreamMsg := service.ExtractUpstreamErrorMessage(responseBody)
	service.SetOpsUpstreamError(c, statusCode, upstreamMsg, "")

	// 使用默认的错误映射
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

// handleFailoverExhaustedSimple 简化版本，用于没有响应体的情况
func (h *OpenAIGatewayHandler) handleFailoverExhaustedSimple(c *gin.Context, statusCode int, streamStarted bool) {
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	service.SetOpsUpstreamError(c, statusCode, errMsg, "")
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

func (h *OpenAIGatewayHandler) mapUpstreamError(statusCode int) (int, string, string) {
	switch statusCode {
	case 401:
		return http.StatusBadGateway, "upstream_error", "Upstream authentication failed, please contact administrator"
	case 403:
		return http.StatusBadGateway, "upstream_error", "Upstream access forbidden, please contact administrator"
	case 429:
		return http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded, please retry later"
	case 529:
		return http.StatusServiceUnavailable, "upstream_error", "Upstream service overloaded, please retry later"
	case 500, 502, 503, 504:
		return http.StatusBadGateway, "upstream_error", "Upstream service temporarily unavailable"
	default:
		return http.StatusBadGateway, "upstream_error", "Upstream request failed"
	}
}

// handleStreamingAwareError handles errors that may occur after streaming has started
func (h *OpenAIGatewayHandler) handleStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if streamStarted {
		// /v1/responses 的严格 SDK（Codex CLI）要求终止事件必须属于
		// response.completed/failed/incomplete/cancelled 集合。
		// 通用 `event: error` 帧不被识别为终止事件，会导致
		// "stream closed before response.completed"。
		if inboundIsResponses(c) {
			if writeResponsesFailedSSE(c, errType, message) {
				return
			}
		}
		// Stream already started, send error as SSE event then close
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			// SSE 错误事件固定 schema，使用 Quote 直拼可避免额外 Marshal 分配。
			errorEvent := "event: error\ndata: " + `{"error":{"type":` + strconv.Quote(errType) + `,"message":` + strconv.Quote(message) + `}}` + "\n\n"
			if _, err := fmt.Fprint(c.Writer, errorEvent); err != nil {
				_ = c.Error(err)
			}
			flusher.Flush()
		}
		return
	}

	// Normal case: return JSON response with proper status code
	h.errorResponse(c, status, errType, message)
}

// ensureForwardErrorResponse 在 Forward 返回错误但尚未写响应时补写统一错误响应。
func (h *OpenAIGatewayHandler) ensureForwardErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	// 旧实现在 Writer.Written 时直接 return false，导致 ping 已 flush 之后的
	// 上游错误（http2 timeout、连接中断等）完全无法把错误传给客户端——
	// HTTP 200 已锁死，TCP 直接 EOF，Codex CLI 报 "stream closed before response.completed"。
	// 这里改成：Writer 已写过时强制走 streamStarted 分支，让
	// handleStreamingAwareError 通过 SSE 发协议合规的 response.failed。
	if c.Writer.Written() {
		streamStarted = true
	}
	h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed", streamStarted)
	return true
}

func shouldLogOpenAIForwardFailureAsWarn(c *gin.Context, wroteFallback bool) bool {
	if wroteFallback {
		return false
	}
	if c == nil || c.Writer == nil {
		return false
	}
	return c.Writer.Written()
}

// errorResponse returns OpenAI API format error response
func (h *OpenAIGatewayHandler) errorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func setOpenAIClientTransportHTTP(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportHTTP)
}

func setOpenAIClientTransportWS(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
}

func ensureOpenAIPoolModeSessionHash(sessionHash string, account *service.Account) string {
	if sessionHash != "" || account == nil || !account.IsPoolMode() {
		return sessionHash
	}
	// 为当前请求生成一次性粘性会话键，确保同账号重试不会重新负载均衡到其他账号。
	return "openai-pool-retry-" + uuid.NewString()
}

func openAIWSIngressFallbackSessionSeed(userID, apiKeyID int64, groupID *int64) string {
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}
	return fmt.Sprintf("openai_ws_ingress:%d:%d:%d", gid, userID, apiKeyID)
}

func isOpenAIWSUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Connection"))), "upgrade")
}

func closeOpenAIClientWS(conn *coderws.Conn, status coderws.StatusCode, reason string) {
	if conn == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = conn.Close(status, reason)
	_ = conn.CloseNow()
}

const openAIWSImageGenerationUnsupportedMessage = "生图不支持ws的方式"

func writeOpenAIRemoteCompactionV2DisabledWSError(ctx context.Context, conn *coderws.Conn) {
	if conn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload := []byte(`{"event_id":"evt_remote_compaction_v2_disabled","type":"error","error":{"type":"api_error","code":"remote_compaction_v2_disabled","message":"OpenAI Remote Compaction V2 is temporarily disabled"}}`)
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

func writeOpenAIWSImageGenerationUnsupported(ctx context.Context, conn *coderws.Conn, model string) {
	if conn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_image_generation_ws_unsupported",
		"type":     "response.failed",
		"response": gin.H{
			"object": "response",
			"status": "failed",
			"model":  model,
			"output": []any{},
			"error": gin.H{
				"type":    "invalid_request_error",
				"code":    "image_generation_ws_unsupported",
				"message": openAIWSImageGenerationUnsupportedMessage,
			},
		},
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    "image_generation_ws_unsupported",
			"message": openAIWSImageGenerationUnsupportedMessage,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_image_generation_ws_unsupported","type":"response.failed","response":{"object":"response","status":"failed","output":[],"error":{"type":"invalid_request_error","code":"image_generation_ws_unsupported","message":"生图不支持ws的方式"}},"error":{"type":"invalid_request_error","code":"image_generation_ws_unsupported","message":"生图不支持ws的方式"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

func writeContentModerationWSError(ctx context.Context, conn *coderws.Conn, decision *service.ContentModerationDecision) {
	if conn == nil || decision == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message := strings.TrimSpace(decision.Message)
	if message == "" {
		message = "content moderation blocked this request"
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_content_moderation_blocked",
		"type":     "error",
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    contentModerationErrorCode(decision),
			"message": message,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_content_moderation_blocked","type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"content moderation blocked this request"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

func summarizeWSCloseErrorForLog(err error) (string, string) {
	if err == nil {
		return "-", "-"
	}
	statusCode := coderws.CloseStatus(err)
	if statusCode == -1 {
		return "-", "-"
	}
	closeStatus := fmt.Sprintf("%d(%s)", int(statusCode), statusCode.String())
	closeReason := "-"
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		reason := strings.TrimSpace(closeErr.Reason)
		if reason != "" {
			closeReason = reason
		}
	}
	return closeStatus, closeReason
}
