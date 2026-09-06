package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

const xSearchPriceContextKey = "_x_search_fixed_price"

type xSearchRequest struct {
	Query  string `json:"query"`
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// XSearch exposes an independent capability endpoint while reusing the
// Responses account selection, failover and response streaming path.
func (h *OpenAIGatewayHandler) XSearch(c *gin.Context) {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformGrok {
		h.errorResponse(c, http.StatusForbidden, "permission_denied", "x_search is not enabled for this API key")
		return
	}
	priceText, err := h.gatewayService.XSearchPricePerRequest(c.Request.Context())
	if err != nil || strings.TrimSpace(priceText) == "" {
		h.errorResponse(c, http.StatusForbidden, "permission_denied", "x_search is not configured")
		return
	}
	price, err := decimal.NewFromString(priceText)
	if err != nil || !price.IsPositive() {
		h.errorResponse(c, http.StatusForbidden, "permission_denied", "x_search is not configured")
		return
	}
	var req xSearchRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Query) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "query is required")
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "grok-4-1-fast"
	}
	body, err := json.Marshal(map[string]any{
		"model":  model,
		"input":  req.Query,
		"stream": req.Stream,
		"tools":  []map[string]string{{"type": "x_search"}},
	})
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "failed to build x_search request")
		return
	}
	c.Set(xSearchPriceContextKey, &price)
	c.Set(ctxKeyInboundEndpoint, EndpointXSearch)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.URL.Path = EndpointResponses
	h.Responses(c)
}

func openAIXSearchFixedPrice(c *gin.Context) *decimal.Decimal {
	if c == nil {
		return nil
	}
	v, ok := c.Get(xSearchPriceContextKey)
	if !ok {
		return nil
	}
	price, ok := v.(*decimal.Decimal)
	if !ok || price == nil || !price.IsPositive() {
		return nil
	}
	copy := price.Round(10)
	return &copy
}
