package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestOpenAIXSearchFixedPrice(t *testing.T) {
	c, _ := ginTestContext(httptest.NewRecorder(), httptest.NewRequest("POST", "/x_search", nil))
	price := decimal.RequireFromString("0.0123456789")
	c.Set(xSearchPriceContextKey, &price)
	got := openAIXSearchFixedPrice(c)
	require.NotNil(t, got)
	require.True(t, got.Equal(price))
}

func TestOpenAIXSearchFixedPriceMissing(t *testing.T) {
	c, _ := ginTestContext(httptest.NewRecorder(), httptest.NewRequest("POST", "/x_search", nil))
	require.Nil(t, openAIXSearchFixedPrice(c))
}

func ginTestContext(w *httptest.ResponseRecorder, r *http.Request) (*gin.Context, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	c, cancel := gin.CreateTestContext(w)
	c.Request = r
	return c, cancel
}
