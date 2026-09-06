package admin

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type OpsHandler struct {
	opsService *service.OpsService
}

// GetErrorLogByID returns ops error log detail.
// GET /api/v1/admin/ops/errors/:id
func (h *OpsHandler) GetErrorLogByID(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid error id")
		return
	}

	detail, err := h.opsService.GetErrorLogByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, detail)
}

const (
	opsListViewErrors   = "errors"
	opsListViewExcluded = "excluded"
	opsListViewAll      = "all"
)

func parseOpsViewParam(c *gin.Context) string {
	if c == nil {
		return ""
	}
	v := strings.ToLower(strings.TrimSpace(c.Query("view")))
	switch v {
	case "", opsListViewErrors:
		return opsListViewErrors
	case opsListViewExcluded:
		return opsListViewExcluded
	case opsListViewAll:
		return opsListViewAll
	default:
		return opsListViewErrors
	}
}

func applyOpsErrorExtraFilters(c *gin.Context, filter *service.OpsErrorLogFilter) bool {
	if c == nil || filter == nil {
		return true
	}
	filter.Category = strings.TrimSpace(c.Query("category"))
	if v := strings.TrimSpace(c.Query("impact_platform_sla")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			b := true
			filter.ImpactPlatformSLA = &b
		case "0", "false", "no":
			b := false
			filter.ImpactPlatformSLA = &b
		default:
			response.BadRequest(c, "Invalid impact_platform_sla")
			return false
		}
	}
	if v := strings.TrimSpace(c.Query("client_failed")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			b := true
			filter.ClientFailed = &b
		case "0", "false", "no":
			b := false
			filter.ClientFailed = &b
		default:
			response.BadRequest(c, "Invalid client_failed")
			return false
		}
	}
	return true
}

func NewOpsHandler(opsService *service.OpsService) *OpsHandler {
	return &OpsHandler{opsService: opsService}
}

// GetUnifiedErrors lists unified ops errors.
// GET /api/v1/admin/ops/unified-errors
func (h *OpsHandler) GetUnifiedErrors(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	filter, err := parseOpsUnifiedErrorListFilter(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	role, _ := middleware.GetUserRoleFromContext(c)
	filter.ViewerRole = role
	result, err := h.opsService.GetUnifiedErrors(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// ExportUnifiedErrors exports unified ops errors as CSV.
// GET /api/v1/admin/ops/unified-errors/export
func (h *OpsHandler) ExportUnifiedErrors(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if !canExportOpsUnifiedErrors(c) {
		response.Forbidden(c, "无权限导出错误列表")
		return
	}

	filter, err := parseOpsUnifiedErrorListFilter(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	filter.Page = 1
	filter.PageSize = 100
	role, _ := middleware.GetUserRoleFromContext(c)
	filter.ViewerRole = role
	if filter.StartTime == nil || filter.EndTime == nil || filter.EndTime.Sub(*filter.StartTime) > 7*24*time.Hour {
		response.BadRequest(c, "时间范围超出允许范围")
		return
	}
	result, err := h.opsService.ExportUnifiedErrors(c.Request.Context(), filter, 100000)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result.Total == 0 || len(result.Rows) == 0 {
		response.BadRequest(c, "当前筛选条件下暂无可导出数据")
		return
	}
	if result.Truncated {
		logOpsUnifiedErrorExportAudit(c, filter, result.Total, false, "row_limit_exceeded")
		response.BadRequest(c, "导出行数超过 100000，请缩小范围")
		return
	}

	var buf bytes.Buffer
	_, _ = buf.WriteString("\xEF\xBB\xBF")
	writer := csv.NewWriter(&buf)
	for _, row := range result.Rows {
		if err := writer.Write(row); err != nil {
			response.InternalError(c, "导出失败")
			return
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		response.InternalError(c, "导出失败")
		return
	}
	filename := "ops_unified_errors_" + time.Now().Format("20060102150405") + ".csv"
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename="+filename)
	logOpsUnifiedErrorExportAudit(c, filter, result.Total, true, "")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}

// GetUnifiedErrorByID returns unified ops error detail.
// GET /api/v1/admin/ops/unified-errors/:id
func (h *OpsHandler) GetUnifiedErrorByID(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid error id")
		return
	}
	role, _ := middleware.GetUserRoleFromContext(c)
	detail, err := h.opsService.GetUnifiedErrorDetailForRole(c.Request.Context(), id, role)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, detail)
}

// GetErrorLogs lists ops error logs.
// GET /api/v1/admin/ops/errors
func (h *OpsHandler) GetErrorLogs(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	page, pageSize := response.ParsePagination(c)
	// Ops list can be larger than standard admin tables.
	if pageSize > 500 {
		pageSize = 500
	}

	startTime, endTime, err := parseOpsTimeRange(c, "1h")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	filter := &service.OpsErrorLogFilter{Page: page, PageSize: pageSize}

	if !startTime.IsZero() {
		filter.StartTime = &startTime
	}
	if !endTime.IsZero() {
		filter.EndTime = &endTime
	}
	filter.View = parseOpsViewParam(c)
	filter.Phase = strings.TrimSpace(c.Query("phase"))
	filter.Owner = strings.TrimSpace(c.Query("error_owner"))
	filter.Source = strings.TrimSpace(c.Query("error_source"))
	filter.Query = strings.TrimSpace(c.Query("q"))
	filter.UserQuery = strings.TrimSpace(c.Query("user_query"))
	filter.Model = strings.TrimSpace(c.Query("model"))
	if !applyOpsErrorExtraFilters(c, filter) {
		return
	}

	// Force request errors: client-visible status >= 400.
	// buildOpsErrorLogsWhere already applies this for non-upstream phase.
	if strings.EqualFold(strings.TrimSpace(filter.Phase), "upstream") {
		filter.Phase = ""
	}

	if platform := strings.TrimSpace(c.Query("platform")); platform != "" {
		filter.Platform = platform
	}
	if v := strings.TrimSpace(c.Query("group_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &id
	}
	if v := strings.TrimSpace(c.Query("account_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid account_id")
			return
		}
		filter.AccountID = &id
	}
	if v := strings.TrimSpace(c.Query("user_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		filter.UserID = &id
	}

	if v := strings.TrimSpace(c.Query("resolved")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			b := true
			filter.Resolved = &b
		case "0", "false", "no":
			b := false
			filter.Resolved = &b
		default:
			response.BadRequest(c, "Invalid resolved")
			return
		}
	}
	if statusCodesStr := strings.TrimSpace(c.Query("status_codes")); statusCodesStr != "" {
		parts := strings.Split(statusCodesStr, ",")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				response.BadRequest(c, "Invalid status_codes")
				return
			}
			out = append(out, n)
		}
		filter.StatusCodes = out
	}

	result, err := h.opsService.GetErrorLogs(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, result.Errors, int64(result.Total), result.Page, result.PageSize)
}

// ListRequestErrors lists client-visible request errors.
// GET /api/v1/admin/ops/request-errors
func (h *OpsHandler) ListRequestErrors(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	page, pageSize := response.ParsePagination(c)
	if pageSize > 500 {
		pageSize = 500
	}
	startTime, endTime, err := parseOpsTimeRange(c, "1h")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	filter := &service.OpsErrorLogFilter{Page: page, PageSize: pageSize}
	if !startTime.IsZero() {
		filter.StartTime = &startTime
	}
	if !endTime.IsZero() {
		filter.EndTime = &endTime
	}
	filter.View = parseOpsViewParam(c)
	filter.Phase = strings.TrimSpace(c.Query("phase"))
	filter.Owner = strings.TrimSpace(c.Query("error_owner"))
	filter.Source = strings.TrimSpace(c.Query("error_source"))
	filter.Query = strings.TrimSpace(c.Query("q"))
	filter.UserQuery = strings.TrimSpace(c.Query("user_query"))
	if !applyOpsErrorExtraFilters(c, filter) {
		return
	}

	// Force request errors: client-visible status >= 400.
	// buildOpsErrorLogsWhere already applies this for non-upstream phase.
	if strings.EqualFold(strings.TrimSpace(filter.Phase), "upstream") {
		filter.Phase = ""
	}

	if platform := strings.TrimSpace(c.Query("platform")); platform != "" {
		filter.Platform = platform
	}
	if v := strings.TrimSpace(c.Query("group_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &id
	}
	if v := strings.TrimSpace(c.Query("account_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid account_id")
			return
		}
		filter.AccountID = &id
	}

	if v := strings.TrimSpace(c.Query("resolved")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			b := true
			filter.Resolved = &b
		case "0", "false", "no":
			b := false
			filter.Resolved = &b
		default:
			response.BadRequest(c, "Invalid resolved")
			return
		}
	}
	if statusCodesStr := strings.TrimSpace(c.Query("status_codes")); statusCodesStr != "" {
		parts := strings.Split(statusCodesStr, ",")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				response.BadRequest(c, "Invalid status_codes")
				return
			}
			out = append(out, n)
		}
		filter.StatusCodes = out
	}

	result, err := h.opsService.GetErrorLogs(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, result.Errors, int64(result.Total), result.Page, result.PageSize)
}

// GetRequestError returns request error detail.
// GET /api/v1/admin/ops/request-errors/:id
func (h *OpsHandler) GetRequestError(c *gin.Context) {
	// same storage; just proxy to existing detail
	h.GetErrorLogByID(c)
}

// ListRequestErrorUpstreamErrors lists upstream error logs correlated to a request error.
// GET /api/v1/admin/ops/request-errors/:id/upstream-errors
func (h *OpsHandler) ListRequestErrorUpstreamErrors(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid error id")
		return
	}

	// Load request error to get correlation keys.
	detail, err := h.opsService.GetErrorLogByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Correlate by request_id/client_request_id.
	requestID := strings.TrimSpace(detail.RequestID)
	clientRequestID := strings.TrimSpace(detail.ClientRequestID)
	if requestID == "" && clientRequestID == "" {
		response.Paginated(c, []*service.OpsErrorLog{}, 0, 1, 10)
		return
	}

	page, pageSize := response.ParsePagination(c)
	if pageSize > 500 {
		pageSize = 500
	}

	// Keep correlation window wide enough so linked upstream errors
	// are discoverable even when UI defaults to 1h elsewhere.
	startTime, endTime, err := parseOpsTimeRange(c, "30d")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	filter := &service.OpsErrorLogFilter{Page: page, PageSize: pageSize}
	if !startTime.IsZero() {
		filter.StartTime = &startTime
	}
	if !endTime.IsZero() {
		filter.EndTime = &endTime
	}
	filter.View = "all"
	filter.Phase = "upstream"
	filter.Owner = "provider"
	filter.Source = strings.TrimSpace(c.Query("error_source"))
	filter.Query = strings.TrimSpace(c.Query("q"))
	if !applyOpsErrorExtraFilters(c, filter) {
		return
	}

	if platform := strings.TrimSpace(c.Query("platform")); platform != "" {
		filter.Platform = platform
	}

	// Prefer exact match on request_id; if missing, fall back to client_request_id.
	if requestID != "" {
		filter.RequestID = requestID
	} else {
		filter.ClientRequestID = clientRequestID
	}

	result, err := h.opsService.GetErrorLogs(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// If client asks for details, expand each upstream error log to include upstream response fields.
	includeDetail := strings.TrimSpace(c.Query("include_detail"))
	if includeDetail == "1" || strings.EqualFold(includeDetail, "true") || strings.EqualFold(includeDetail, "yes") {
		details := make([]*service.OpsErrorLogDetail, 0, len(result.Errors))
		for _, item := range result.Errors {
			if item == nil {
				continue
			}
			d, err := h.opsService.GetErrorLogByID(c.Request.Context(), item.ID)
			if err != nil || d == nil {
				continue
			}
			details = append(details, d)
		}
		response.Paginated(c, details, int64(result.Total), result.Page, result.PageSize)
		return
	}

	response.Paginated(c, result.Errors, int64(result.Total), result.Page, result.PageSize)
}

// ResolveRequestError toggles resolved status.
// PUT /api/v1/admin/ops/request-errors/:id/resolve
func (h *OpsHandler) ResolveRequestError(c *gin.Context) {
	h.UpdateErrorResolution(c)
}

// ListUpstreamErrors lists independent upstream errors.
// GET /api/v1/admin/ops/upstream-errors
func (h *OpsHandler) ListUpstreamErrors(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	page, pageSize := response.ParsePagination(c)
	if pageSize > 500 {
		pageSize = 500
	}
	startTime, endTime, err := parseOpsTimeRange(c, "1h")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	filter := &service.OpsErrorLogFilter{Page: page, PageSize: pageSize}
	if !startTime.IsZero() {
		filter.StartTime = &startTime
	}
	if !endTime.IsZero() {
		filter.EndTime = &endTime
	}

	filter.View = parseOpsViewParam(c)
	filter.Phase = strings.TrimSpace(c.Query("phase"))
	filter.Owner = "provider"
	filter.Source = strings.TrimSpace(c.Query("error_source"))
	filter.Query = strings.TrimSpace(c.Query("q"))
	if !applyOpsErrorExtraFilters(c, filter) {
		return
	}

	if platform := strings.TrimSpace(c.Query("platform")); platform != "" {
		filter.Platform = platform
	}
	if v := strings.TrimSpace(c.Query("group_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &id
	}
	if v := strings.TrimSpace(c.Query("account_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid account_id")
			return
		}
		filter.AccountID = &id
	}

	if v := strings.TrimSpace(c.Query("resolved")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			b := true
			filter.Resolved = &b
		case "0", "false", "no":
			b := false
			filter.Resolved = &b
		default:
			response.BadRequest(c, "Invalid resolved")
			return
		}
	}
	if statusCodesStr := strings.TrimSpace(c.Query("status_codes")); statusCodesStr != "" {
		parts := strings.Split(statusCodesStr, ",")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				response.BadRequest(c, "Invalid status_codes")
				return
			}
			out = append(out, n)
		}
		filter.StatusCodes = out
	}

	result, err := h.opsService.GetErrorLogs(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, result.Errors, int64(result.Total), result.Page, result.PageSize)
}

// GetUpstreamError returns upstream error detail.
// GET /api/v1/admin/ops/upstream-errors/:id
func (h *OpsHandler) GetUpstreamError(c *gin.Context) {
	h.GetErrorLogByID(c)
}

// ResolveUpstreamError toggles resolved status.
// PUT /api/v1/admin/ops/upstream-errors/:id/resolve
func (h *OpsHandler) ResolveUpstreamError(c *gin.Context) {
	h.UpdateErrorResolution(c)
}

// ==================== Existing endpoints ====================

// ListRequestDetails returns a request-level list (success + error) for drill-down.
// GET /api/v1/admin/ops/requests
func (h *OpsHandler) ListRequestDetails(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	page, pageSize := response.ParsePagination(c)
	if pageSize > 100 {
		pageSize = 100
	}

	startTime, endTime, err := parseOpsTimeRange(c, "1h")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	filter := &service.OpsRequestDetailFilter{
		Page:      page,
		PageSize:  pageSize,
		StartTime: &startTime,
		EndTime:   &endTime,
	}

	filter.Kind = strings.TrimSpace(c.Query("kind"))
	filter.Platform = strings.TrimSpace(c.Query("platform"))
	filter.Model = strings.TrimSpace(c.Query("model"))
	filter.RequestID = strings.TrimSpace(c.Query("request_id"))
	filter.Query = strings.TrimSpace(c.Query("q"))
	filter.Sort = strings.TrimSpace(c.Query("sort"))

	if v := strings.TrimSpace(c.Query("user_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		filter.UserID = &id
	}
	if v := strings.TrimSpace(c.Query("api_key_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid api_key_id")
			return
		}
		filter.APIKeyID = &id
	}
	if v := strings.TrimSpace(c.Query("account_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid account_id")
			return
		}
		filter.AccountID = &id
	}
	if v := strings.TrimSpace(c.Query("group_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &id
	}

	if v := strings.TrimSpace(c.Query("min_duration_ms")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			response.BadRequest(c, "Invalid min_duration_ms")
			return
		}
		filter.MinDurationMs = &parsed
	}
	if v := strings.TrimSpace(c.Query("max_duration_ms")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			response.BadRequest(c, "Invalid max_duration_ms")
			return
		}
		filter.MaxDurationMs = &parsed
	}

	out, err := h.opsService.ListRequestDetails(c.Request.Context(), filter)
	if err != nil {
		// Invalid sort/kind/platform etc should be a bad request; keep it simple.
		if strings.Contains(strings.ToLower(err.Error()), "invalid") {
			response.BadRequest(c, err.Error())
			return
		}
		response.Error(c, http.StatusInternalServerError, "Failed to list request details")
		return
	}

	response.Paginated(c, out.Items, out.Total, out.Page, out.PageSize)
}

type opsResolveRequest struct {
	Resolved bool `json:"resolved"`
}

// UpdateErrorResolution allows manual resolve/unresolve.
// PUT /api/v1/admin/ops/errors/:id/resolve
func (h *OpsHandler) UpdateErrorResolution(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Error(c, http.StatusUnauthorized, "Unauthorized")
		return
	}

	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid error id")
		return
	}

	var req opsResolveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	uid := subject.UserID
	if err := h.opsService.UpdateErrorResolution(c.Request.Context(), id, req.Resolved, &uid); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func parseOpsTimeRange(c *gin.Context, defaultRange string) (time.Time, time.Time, error) {
	startStr := strings.TrimSpace(c.Query("start_time"))
	endStr := strings.TrimSpace(c.Query("end_time"))

	parseTS := func(s string) (time.Time, error) {
		if s == "" {
			return time.Time{}, nil
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		return time.Parse(time.RFC3339, s)
	}

	start, err := parseTS(startStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseTS(endStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	// start/end explicitly provided (even partially)
	if startStr != "" || endStr != "" {
		if end.IsZero() {
			end = time.Now()
		}
		if start.IsZero() {
			dur, _ := parseOpsDuration(defaultRange)
			start = end.Add(-dur)
		}
		if start.After(end) {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid time range: start_time must be <= end_time")
		}
		if end.Sub(start) > 30*24*time.Hour {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid time range: max window is 30 days")
		}
		return start, end, nil
	}

	// time_range fallback
	tr := strings.TrimSpace(c.Query("time_range"))
	if tr == "" {
		tr = defaultRange
	}
	dur, ok := parseOpsDuration(tr)
	if !ok {
		dur, _ = parseOpsDuration(defaultRange)
	}

	end = time.Now()
	start = end.Add(-dur)
	if end.Sub(start) > 30*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid time range: max window is 30 days")
	}
	return start, end, nil
}

func parseOpsDuration(v string) (time.Duration, bool) {
	switch strings.TrimSpace(v) {
	case "1m":
		return time.Minute, true
	case "5m":
		return 5 * time.Minute, true
	case "30m":
		return 30 * time.Minute, true
	case "1h":
		return time.Hour, true
	case "6h":
		return 6 * time.Hour, true
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func parseOpsUnifiedErrorListFilter(c *gin.Context) (*service.OpsUnifiedErrorListFilter, error) {
	page, pageSize := response.ParsePagination(c)
	if page <= 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize != 20 && pageSize != 50 && pageSize != 100 {
		return nil, fmt.Errorf("page_size must be one of 20, 50, 100")
	}
	start, end, err := parseOpsTimeRange(c, "30m")
	if err != nil {
		return nil, err
	}
	if end.Sub(start) > 7*24*time.Hour {
		return nil, fmt.Errorf("invalid time range: max window is 7 days")
	}

	filter := &service.OpsUnifiedErrorListFilter{StartTime: &start, EndTime: &end, Page: page, PageSize: pageSize}
	if filter.ErrorCategories, err = parseOpsCSVParam(c, "error_categories", service.IsValidOpsErrorCategory, "invalid error_categories"); err != nil {
		return nil, err
	}
	if filter.ErrorSubcategories, err = parseOpsCSVParam(c, "error_subcategories", nil, "invalid error_subcategories"); err != nil {
		return nil, err
	}
	if filter.ClientErrorSubcategories, err = parseOpsCSVParam(c, "client_error_subcategories", service.IsValidOpsClientErrorSubcategory, "invalid client_error_subcategories"); err != nil {
		return nil, err
	}
	if filter.ErrorResults, err = parseOpsCSVParam(c, "error_results", service.IsValidOpsUnifiedErrorResult, "invalid error_results"); err != nil {
		return nil, err
	}
	if filter.Severities, err = parseOpsCSVParam(c, "severity", service.IsValidOpsUnifiedSeverity, "invalid severity"); err != nil {
		return nil, err
	}
	if filter.StatusCodes, err = parseOpsStatusCodeFilter(c.Query("status_code")); err != nil {
		return nil, err
	}
	if filter.UserID, err = parsePositiveInt64Ptr(c.Query("user_id"), "user_id"); err != nil {
		return nil, err
	}
	if filter.APIKeyID, err = parsePositiveInt64Ptr(c.Query("api_key_id"), "api_key_id"); err != nil {
		return nil, err
	}
	if filter.GroupID, err = parsePositiveInt64Ptr(c.Query("group_id"), "group_id"); err != nil {
		return nil, err
	}
	if filter.UpstreamAccountID, err = parsePositiveInt64Ptr(c.Query("upstream_account_id"), "upstream_account_id"); err != nil {
		return nil, err
	}
	filter.Platform = strings.TrimSpace(c.Query("platform"))
	filter.Model = strings.TrimSpace(c.Query("model"))
	filter.RequestID = strings.TrimSpace(c.Query("request_id"))
	if len(filter.RequestID) > 128 {
		return nil, fmt.Errorf("request_id must be at most 128 characters")
	}
	filter.Keyword = strings.TrimSpace(c.Query("keyword"))
	if filter.Keyword != "" && (len([]rune(filter.Keyword)) < 2 || len([]rune(filter.Keyword)) > 100) {
		return nil, fmt.Errorf("keyword must be 2-100 characters")
	}
	filter.AIAnalysis = strings.TrimSpace(c.Query("ai_analysis"))
	if filter.AIAnalysis == "" {
		filter.AIAnalysis = service.OpsUnifiedAIAnalysisAll
	}
	if !service.IsValidOpsUnifiedAIAnalysis(filter.AIAnalysis) {
		return nil, fmt.Errorf("ai_analysis must be all/analyzed/not_analyzed")
	}
	filter.SortBy = strings.TrimSpace(c.Query("sort_by"))
	if filter.SortBy == "" {
		filter.SortBy = "occurred_at"
	}
	switch filter.SortBy {
	case "occurred_at", "status_code", "severity", "same_kind_count":
	default:
		return nil, fmt.Errorf("invalid sort_by")
	}
	filter.SortOrder = strings.ToLower(strings.TrimSpace(c.Query("sort_order")))
	if filter.SortOrder == "" {
		filter.SortOrder = "desc"
	}
	if filter.SortOrder != "asc" && filter.SortOrder != "desc" {
		return nil, fmt.Errorf("sort_order must be asc or desc")
	}
	return filter, nil
}

func parseOpsCSVParam(c *gin.Context, key string, validate func(string) bool, invalidMessage string) ([]string, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if validate != nil && !validate(value) {
			return nil, fmt.Errorf("%s", invalidMessage)
		}
		lower := strings.ToLower(value)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func parsePositiveInt64Ptr(raw string, field string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("invalid %s", field)
	}
	return &id, nil
}

func parseOpsStatusCodeFilter(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seen := map[int]struct{}{}
	out := []int{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			start, err1 := strconv.Atoi(strings.TrimSpace(bounds[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err1 != nil || err2 != nil || start < 100 || end > 599 || end < start {
				return nil, fmt.Errorf("invalid status_code")
			}
			for code := start; code <= end; code++ {
				if _, ok := seen[code]; !ok {
					seen[code] = struct{}{}
					out = append(out, code)
				}
			}
			continue
		}
		code, err := strconv.Atoi(part)
		if err != nil || code < 100 || code > 599 {
			return nil, fmt.Errorf("invalid status_code")
		}
		if _, ok := seen[code]; !ok {
			seen[code] = struct{}{}
			out = append(out, code)
		}
	}
	return out, nil
}

func canExportOpsUnifiedErrors(c *gin.Context) bool {
	role, ok := middleware.GetUserRoleFromContext(c)
	if !ok {
		return false
	}
	role = strings.TrimSpace(role)
	return role == service.RoleAdmin || strings.EqualFold(role, "ops") || strings.EqualFold(role, "operation") || strings.EqualFold(role, "operator")
}

func canOperateOpsAIAnalysis(c *gin.Context) bool {
	return canExportOpsUnifiedErrors(c)
}

func canFeedbackOpsAIAnalysis(c *gin.Context) bool {
	role, ok := middleware.GetUserRoleFromContext(c)
	if !ok {
		return false
	}
	return isOpsAIAnalysisFeedbackRole(role)
}

func isOpsAIAnalysisFeedbackRole(role string) bool {
	role = strings.TrimSpace(role)
	return role == service.RoleAdmin ||
		strings.EqualFold(role, "ops") ||
		strings.EqualFold(role, "operation") ||
		strings.EqualFold(role, "operator") ||
		strings.EqualFold(role, "operations") ||
		strings.EqualFold(role, "customer_service") ||
		strings.EqualFold(role, "customer-service") ||
		strings.EqualFold(role, "customerService") ||
		strings.EqualFold(role, "support") ||
		strings.EqualFold(role, "service") ||
		strings.EqualFold(role, "cs")
}

func opsJoinInts(values []int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func logOpsUnifiedErrorExportAudit(c *gin.Context, filter *service.OpsUnifiedErrorListFilter, total int, success bool, reason string) {
	if c == nil || filter == nil {
		return
	}
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	role, _ := middleware.GetUserRoleFromContext(c)
	startAt, endAt := "", ""
	if filter.StartTime != nil {
		startAt = filter.StartTime.Format(time.RFC3339)
	}
	if filter.EndTime != nil {
		endAt = filter.EndTime.Format(time.RFC3339)
	}
	slog.Info("ops unified errors exported",
		"audit", true,
		"operation", "ops_unified_errors_export",
		"user_id", subject.UserID,
		"role", role,
		"success", success,
		"reason", reason,
		"export_rows", total,
		"start_time", startAt,
		"end_time", endAt,
		"error_categories", strings.Join(filter.ErrorCategories, ","),
		"error_subcategories", strings.Join(filter.ErrorSubcategories, ","),
		"client_error_subcategories", strings.Join(filter.ClientErrorSubcategories, ","),
		"error_results", strings.Join(filter.ErrorResults, ","),
		"severities", strings.Join(filter.Severities, ","),
		"platform", filter.Platform,
		"model", filter.Model,
		"status_codes", opsJoinInts(filter.StatusCodes),
		"group_id", filter.GroupID,
		"user_id_filter", filter.UserID,
		"api_key_id", filter.APIKeyID,
		"upstream_account_id", filter.UpstreamAccountID,
	)
}
