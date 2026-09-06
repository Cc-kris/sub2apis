package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type upstreamContextTestKey string

func TestGatewayService_StreamingReusesScannerBufferAndStillParsesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}

	svc := &GatewayService{
		cfg:              cfg,
		rateLimitService: &RateLimitService{},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}

	go func() {
		defer func() { _ = pw.Close() }()
		// Minimal SSE event to trigger parseSSEUsage
		_, _ = pw.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n"))
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	}()

	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "model", "model", false)
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 3, result.usage.InputTokens)
	require.Equal(t, 7, result.usage.OutputTokens)
}

func TestDetachUpstreamContextCancelsBeforeResponseStarts(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), upstreamContextTestKey("test-key"), "test-value"))
	upstreamCtx, _ := detachUpstreamContext(parent)
	defer ReleaseDetachedUpstreamContext(upstreamCtx)

	cancel()

	select {
	case <-upstreamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("upstream context was not canceled with the client before response start")
	}
	require.ErrorIs(t, upstreamCtx.Err(), context.Canceled)
	require.Equal(t, "test-value", upstreamCtx.Value(upstreamContextTestKey("test-key")))
}

func TestDetachUpstreamContextAllowsBoundedDrainAfterResponseStarts(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), upstreamContextTestKey("test-key"), "test-value"))
	upstreamCtx, _ := detachUpstreamContext(parent)
	defer ReleaseDetachedUpstreamContext(upstreamCtx)

	trace := httptrace.ContextClientTrace(upstreamCtx)
	require.NotNil(t, trace)
	require.NotNil(t, trace.GotFirstResponseByte)
	trace.GotFirstResponseByte()
	cancel()

	require.NoError(t, upstreamCtx.Err())
	require.Equal(t, "test-value", upstreamCtx.Value(upstreamContextTestKey("test-key")))
}

func TestDetachUpstreamContextHasHardLifetimeLimit(t *testing.T) {
	upstreamCtx, _ := detachUpstreamContextWithTimeout(context.Background(), 20*time.Millisecond)
	defer ReleaseDetachedUpstreamContext(upstreamCtx)

	select {
	case <-upstreamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("detached upstream context exceeded its hard lifetime limit")
	}
	require.ErrorIs(t, upstreamCtx.Err(), context.DeadlineExceeded)
}
