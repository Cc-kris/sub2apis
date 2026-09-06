package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestXSearchPriceRequest_AllowsEmptyPriceToDisableFeature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPut, "/settings/x-search-price", strings.NewReader(`{"price_per_request":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	var req xSearchPriceRequest
	require.NoError(t, c.ShouldBindJSON(&req))
	require.Empty(t, req.PricePerRequest)
}
