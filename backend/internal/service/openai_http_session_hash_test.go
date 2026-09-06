package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newOpenAIHTTPHashTestContext(apiKeyID int64) *gin.Context {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Set("api_key", &APIKey{ID: apiKeyID})
	return c
}

func TestGenerateHTTPStableSessionHash_NoReusablePrefixUsesAPIKeyModelAffinity(t *testing.T) {
	svc := &OpenAIGatewayService{}
	first := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"first turn"}]}`)
	second := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"later turn"}]}`)

	h1 := svc.GenerateHTTPStableSessionHash(newOpenAIHTTPHashTestContext(353), first)
	h2 := svc.GenerateHTTPStableSessionHash(newOpenAIHTTPHashTestContext(353), second)

	require.NotEmpty(t, h1)
	require.Equal(t, h1, h2, "content-only requests must stay on one API-key/model affinity route")
}

func TestGenerateHTTPStableSessionHash_AffinityIsolatedByAPIKeyAndModel(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"turn"}]}`)

	base := svc.GenerateHTTPStableSessionHash(newOpenAIHTTPHashTestContext(353), body)
	differentKey := svc.GenerateHTTPStableSessionHash(newOpenAIHTTPHashTestContext(354), body)
	differentModel := svc.GenerateHTTPStableSessionHash(newOpenAIHTTPHashTestContext(353), []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"turn"}]}`))

	require.NotEmpty(t, base)
	require.NotEqual(t, base, differentKey)
	require.NotEqual(t, base, differentModel)
}

func TestGenerateHTTPStableSessionHash_NoAPIKeyContextFailsClosed(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"turn"}]}`)

	require.Empty(t, svc.GenerateHTTPStableSessionHash(newOpenAIHTTPHashTestContext(0), body))
}
