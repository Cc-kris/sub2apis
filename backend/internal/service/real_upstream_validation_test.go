//go:build integration

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestRealOpenAIResponsesStream exercises the local Responses SSE parser with
// a real API-key upstream. It is opt-in so normal unit runs never require
// credentials or network access.
func TestRealOpenAIResponsesStream(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("REAL_UPSTREAM_KEY"))
	if key == "" {
		t.Skip("REAL_UPSTREAM_KEY is not set")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("REAL_UPSTREAM_URL")), "/")
	if endpoint == "" {
		endpoint = "https://api.dmxcode.cc/v1/responses"
	} else if !strings.HasSuffix(endpoint, "/responses") {
		endpoint += "/responses"
	}
	model := strings.TrimSpace(os.Getenv("REAL_UPSTREAM_MODEL"))
	if model == "" {
		model = "gpt-5.6-sol"
	}

	payload, err := json.Marshal(map[string]any{
		"model":       model,
		"instructions": "Reply with exactly OK.",
		"input":       "ping",
		"stream":      true,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		require.Failf(t, "real upstream returned non-200", "status=%d body=%s", resp.StatusCode, body)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamDataIntervalTimeout: 45,
		MaxLineSize:               defaultMaxLineSize,
	}}}
	started := time.Now()
	result, err := svc.handleStreamingResponse(ctx, resp, c, &Account{ID: 146, Name: "real-upstream-validation"}, started, model, model)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.firstTokenMs)
	require.GreaterOrEqual(t, *result.firstTokenMs, 0)
	require.Contains(t, recorder.Body.String(), "response.completed")
	require.Less(t, time.Since(started), 90*time.Second)
}

func TestRealOpenAIChatCompatibilityStream(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("REAL_UPSTREAM_KEY"))
	if key == "" {
		t.Skip("REAL_UPSTREAM_KEY is not set")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("REAL_UPSTREAM_URL")), "/")
	if endpoint == "" {
		endpoint = "https://api.dmxcode.cc/v1/responses"
	}
	model := strings.TrimSpace(os.Getenv("REAL_UPSTREAM_MODEL"))
	if model == "" {
		model = "gpt-5.6-sol"
	}
	payload, err := json.Marshal(map[string]any{
		"model":        model,
		"instructions": "Reply with exactly OK.",
		"input":        "ping",
		"stream":       true,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		require.Failf(t, "real upstream returned non-200", "status=%d body=%s", resp.StatusCode, body)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	svc := &OpenAIGatewayService{}
	started := time.Now()
	result, err := svc.handleChatStreamingResponse(resp, c, &Account{ID: 146, Name: "real-upstream-validation"}, model, model, model, true, started)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.FirstTokenMs)
	require.GreaterOrEqual(t, *result.FirstTokenMs, 0)
	require.Contains(t, recorder.Body.String(), "data: ")
	require.Contains(t, recorder.Body.String(), "[DONE]")
}
