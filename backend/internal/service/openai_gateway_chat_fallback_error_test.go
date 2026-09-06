package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAnthropicChatFallback_MalformedFirstEventDoesNotCommitSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	resp := &http.Response{
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader("data: not-json\\n\\n")),
	}

	_, err := (&OpenAIGatewayService{}).handleAnthropicFromRawChatCompletionsStream(
		resp, c, "claude-test", "claude-test", "provider-model", time.Now(),
	)

	require.ErrorContains(t, err, "parse chat completions stream event")
	require.False(t, c.Writer.Written(), "the handler must retain the ability to return a non-200 error")
	require.Empty(t, w.Body.String())
}

func TestResponsesChatFallback_MalformedEventDoesNotFinalizeSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	resp := &http.Response{
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader("data: not-json\\n\\n")),
	}

	_, err := (&OpenAIGatewayService{}).streamChatCompletionsAsResponses(
		c, resp, "gpt-test", "gpt-test", "provider-model", nil, nil, time.Now(),
	)

	require.ErrorContains(t, err, "parse chat completions stream event")
	require.False(t, c.Writer.Written(), "the handler must retain the ability to return an error")
	require.NotContains(t, w.Body.String(), "response.completed")
	require.NotContains(t, w.Body.String(), "data: [DONE]")
}
