package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	BillingTypeBalance      int8 = 0 // 钱包余额
	BillingTypeSubscription int8 = 1 // 订阅套餐
)

type RequestType int16

const (
	RequestTypeUnknown RequestType = 0
	RequestTypeSync    RequestType = 1
	RequestTypeStream  RequestType = 2
	RequestTypeWSV2    RequestType = 3
)

func (t RequestType) IsValid() bool {
	switch t {
	case RequestTypeUnknown, RequestTypeSync, RequestTypeStream, RequestTypeWSV2:
		return true
	default:
		return false
	}
}

func (t RequestType) Normalize() RequestType {
	if t.IsValid() {
		return t
	}
	return RequestTypeUnknown
}

func (t RequestType) String() string {
	switch t.Normalize() {
	case RequestTypeSync:
		return "sync"
	case RequestTypeStream:
		return "stream"
	case RequestTypeWSV2:
		return "ws_v2"
	default:
		return "unknown"
	}
}

func RequestTypeFromInt16(v int16) RequestType {
	return RequestType(v).Normalize()
}

func ParseUsageRequestType(value string) (RequestType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unknown":
		return RequestTypeUnknown, nil
	case "sync":
		return RequestTypeSync, nil
	case "stream":
		return RequestTypeStream, nil
	case "ws_v2":
		return RequestTypeWSV2, nil
	default:
		return RequestTypeUnknown, fmt.Errorf("invalid request_type, allowed values: unknown, sync, stream, ws_v2")
	}
}

func RequestTypeFromLegacy(stream bool, openAIWSMode bool) RequestType {
	if openAIWSMode {
		return RequestTypeWSV2
	}
	if stream {
		return RequestTypeStream
	}
	return RequestTypeSync
}

func ApplyLegacyRequestFields(requestType RequestType, fallbackStream bool, fallbackOpenAIWSMode bool) (stream bool, openAIWSMode bool) {
	switch requestType.Normalize() {
	case RequestTypeSync:
		return false, false
	case RequestTypeStream:
		return true, false
	case RequestTypeWSV2:
		return true, true
	default:
		return fallbackStream, fallbackOpenAIWSMode
	}
}

type UsageLog struct {
	ID        int64
	UserID    int64
	APIKeyID  int64
	AccountID int64
	RequestID string
	Model     string
	// RequestedModel is the client-requested model name recorded for stable user/admin display.
	// Empty should be treated as Model for backward compatibility with historical rows.
	RequestedModel string
	// UpstreamModel is the actual model sent to the upstream provider after mapping.
	// Nil means no mapping was applied (requested model was used as-is).
	UpstreamModel *string
	// ChannelID 渠道 ID
	ChannelID *int64
	// ModelMappingChain 模型映射链，如 "a→b→c"
	ModelMappingChain *string
	// BillingTier 计费层级标签（per_request/image 模式）
	BillingTier *string
	// BillingMode 计费模式：token/per_request/image/per_second
	BillingMode *string
	// ServiceTier records the OpenAI service tier used for billing, e.g. "priority" / "flex".
	ServiceTier *string
	// ReasoningEffort is the request's reasoning effort level.
	// OpenAI: "low" / "medium" / "high" / "xhigh"; Claude: "low" / "medium" / "high" / "max".
	// Nil means not provided / not applicable.
	ReasoningEffort *string
	// InboundEndpoint is the client-facing API endpoint path, e.g. /v1/chat/completions.
	InboundEndpoint *string
	// UpstreamEndpoint is the normalized upstream endpoint path, e.g. /v1/responses.
	UpstreamEndpoint *string

	GroupID        *int64
	SubscriptionID *int64

	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int

	CacheCreation5mTokens int `gorm:"column:cache_creation_5m_tokens"`
	CacheCreation1hTokens int `gorm:"column:cache_creation_1h_tokens"`

	ImageOutputTokens int
	ImageOutputCost   float64

	InputCost         float64
	OutputCost        float64
	CacheCreationCost float64
	CacheReadCost     float64
	TotalCost         float64
	ActualCost        float64
	RateMultiplier    float64
	// AccountRateMultiplier 账号计费倍率快照（nil 表示历史数据，按 1.0 处理）
	AccountRateMultiplier *float64
	// AccountStatsCost 账号统计定价预计算费用（nil = 使用默认公式 total_cost × account_rate_multiplier）
	AccountStatsCost *float64
	// UpstreamCostMultiplier is the immutable procurement multiplier copied
	// from the selected account for this request. It must never be recomputed
	// from the account's current value when historical finance data is read.
	UpstreamCostMultiplier        *decimal.Decimal
	UpstreamMultiplierChangeID    *int64
	UpstreamMultiplierSource      string
	UpstreamMultiplierEffectiveAt *time.Time
	AccountFinanceProfileID       *int64
	// Sales pricing fields are immutable request-time facts used by the finance
	// ledger. Nil values identify historical rows created before snapshotting.
	SalesModel                 *string
	SalesPricingEffectiveModel *string
	SalesPricingLegacySource   *string
	SalesPricingVersion        *string
	SalesPricingSource         *string
	SalesPricingChecksum       *string
	SalesPricingSnapshot       map[string]any
	SalesPricingShadowSnapshot map[string]any
	SalesPricingShadowDelta    *decimal.Decimal
	UsageListValue             *decimal.Decimal
	// FinanceBusinessTypeSnapshot is populated from an existing finance record
	// during recalculation so historical classification never uses a current role.
	FinanceBusinessTypeSnapshot string
	PromotionCreditUsed         *decimal.Decimal
	FinanceExcluded             bool
	FinanceExclusionReason      string

	BillingType  int8
	RequestType  RequestType
	Stream       bool
	OpenAIWSMode bool
	DurationMs   *int
	FirstTokenMs *int
	UserAgent    *string
	IPAddress    *string

	// Cache TTL Override 标记（管理员强制替换了缓存 TTL 计费）
	CacheTTLOverridden bool

	// 图片生成字段
	ImageCount             int
	ImageSize              *string
	ImageInputSize         *string
	ImageOutputSize        *string
	ImageSizeSource        *string
	ImageSizeBreakdown     map[string]int
	VideoCount             int
	VideoResolution        *string
	VideoDurationSeconds   *int
	VideoTaskID            *string
	MediaType              *string
	UpstreamAttempts       []UsageUpstreamAttempt
	FinanceCostMode        string                 `json:"-"`
	FinanceProtocolConfig  *FinanceProtocolConfig `json:"-"`
	UpstreamBillingPayload []byte                 `json:"-"`

	CreatedAt time.Time

	User         *User
	APIKey       *APIKey
	Account      *Account
	Group        *Group
	Subscription *UserSubscription
}

type UsageUpstreamAttempt struct {
	ID                            int64
	UsageLogID                    int64
	RequestID                     string
	AttemptNo                     int
	AccountID                     int64
	ChannelID                     *int64
	UpstreamModel                 string
	ServiceTier                   *string
	InputTokens                   int64
	OutputTokens                  int64
	CacheReadTokens               int64
	CacheCreationTokens           int64
	CacheCreation5mTokens         int64
	CacheCreation1hTokens         int64
	RequestCount                  int64
	ImageCount                    int64
	VideoSeconds                  int64
	UpstreamCostMultiplier        *decimal.Decimal
	UpstreamMultiplierChangeID    *int64
	UpstreamMultiplierSource      string
	UpstreamMultiplierEffectiveAt *time.Time
	AccountFinanceProfileID       *int64
	Billable                      bool
	BillingObservedAt             *time.Time
	UpstreamActualCharge          *decimal.Decimal
	UpstreamActualChargeUSD       *decimal.Decimal
	UpstreamStandardCharge        *decimal.Decimal
	UpstreamChargeCurrency        string
	UpstreamChargeUnitSemantics   string
	UpstreamBillingRequestID      string
	UpstreamChargeSnapshot        map[string]any
	CompletedAt                   time.Time
	CreatedAt                     time.Time
}

func BuildFinalUsageUpstreamAttempt(log *UsageLog) (UsageUpstreamAttempt, bool) {
	if log == nil || log.AccountID <= 0 || strings.TrimSpace(log.RequestID) == "" {
		return UsageUpstreamAttempt{}, false
	}
	upstreamModel := strings.TrimSpace(log.Model)
	if log.UpstreamModel != nil && strings.TrimSpace(*log.UpstreamModel) != "" {
		upstreamModel = strings.TrimSpace(*log.UpstreamModel)
	}
	if upstreamModel == "" {
		return UsageUpstreamAttempt{}, false
	}
	requestCount := int64(0)
	if log.BillingMode != nil && strings.TrimSpace(*log.BillingMode) == "per_request" {
		requestCount = 1
	}
	videoSeconds := int64(0)
	if log.VideoDurationSeconds != nil && *log.VideoDurationSeconds > 0 {
		videoSeconds = int64(*log.VideoDurationSeconds)
	}
	attempt := UsageUpstreamAttempt{
		RequestID:                     log.RequestID,
		AttemptNo:                     1,
		AccountID:                     log.AccountID,
		ChannelID:                     log.ChannelID,
		UpstreamModel:                 upstreamModel,
		ServiceTier:                   log.ServiceTier,
		InputTokens:                   int64(max(log.InputTokens, 0)),
		OutputTokens:                  int64(max(log.OutputTokens, 0)),
		CacheReadTokens:               int64(max(log.CacheReadTokens, 0)),
		CacheCreationTokens:           int64(max(log.CacheCreationTokens, 0)),
		CacheCreation5mTokens:         int64(max(log.CacheCreation5mTokens, 0)),
		CacheCreation1hTokens:         int64(max(log.CacheCreation1hTokens, 0)),
		RequestCount:                  requestCount,
		ImageCount:                    int64(max(log.ImageCount, 0)),
		VideoSeconds:                  videoSeconds,
		UpstreamCostMultiplier:        CloneDecimalSnapshot(log.UpstreamCostMultiplier),
		UpstreamMultiplierChangeID:    cloneInt64Pointer(log.UpstreamMultiplierChangeID),
		UpstreamMultiplierSource:      log.UpstreamMultiplierSource,
		UpstreamMultiplierEffectiveAt: cloneFinanceTime(log.UpstreamMultiplierEffectiveAt),
		AccountFinanceProfileID:       cloneInt64Pointer(log.AccountFinanceProfileID),
		CompletedAt:                   log.CreatedAt,
		CreatedAt:                     log.CreatedAt,
	}
	attempt.Billable = attempt.InputTokens > 0 || attempt.OutputTokens > 0 || attempt.CacheReadTokens > 0 || attempt.CacheCreationTokens > 0 ||
		attempt.CacheCreation5mTokens > 0 || attempt.CacheCreation1hTokens > 0 || attempt.RequestCount > 0 ||
		attempt.ImageCount > 0 || attempt.VideoSeconds > 0
	return attempt, attempt.Billable
}

func ApplyAccountFinanceEvidenceToUsageLog(log *UsageLog, account *Account) {
	if log == nil || account == nil {
		return
	}
	log.UpstreamMultiplierChangeID = cloneInt64Pointer(account.UpstreamCostMultiplierChangeID)
	log.UpstreamMultiplierSource = strings.TrimSpace(account.UpstreamCostMultiplierSource)
	log.UpstreamMultiplierEffectiveAt = cloneFinanceTime(account.UpstreamCostMultiplierUpdatedAt)
	log.AccountFinanceProfileID = cloneInt64Pointer(account.CurrentFinanceProfileID)
	log.FinanceCostMode = account.FinanceCostMode
	log.FinanceProtocolConfig = account.FinanceProtocolConfig
	for index := range log.UpstreamAttempts {
		attempt := &log.UpstreamAttempts[index]
		if attempt.AccountID != account.ID {
			continue
		}
		ApplyAccountFinanceEvidenceToAttempt(attempt, account)
	}
}

func ApplyAccountFinanceEvidenceToAttempt(attempt *UsageUpstreamAttempt, account *Account) {
	if attempt == nil || account == nil || attempt.AccountID != account.ID {
		return
	}
	attempt.UpstreamMultiplierChangeID = cloneInt64Pointer(account.UpstreamCostMultiplierChangeID)
	attempt.UpstreamMultiplierSource = strings.TrimSpace(account.UpstreamCostMultiplierSource)
	attempt.UpstreamMultiplierEffectiveAt = cloneFinanceTime(account.UpstreamCostMultiplierUpdatedAt)
	attempt.AccountFinanceProfileID = cloneInt64Pointer(account.CurrentFinanceProfileID)
}

func EnsureFinalUsageUpstreamAttempt(log *UsageLog) {
	if log == nil {
		return
	}
	attempt, ok := BuildFinalUsageUpstreamAttempt(log)
	if !ok {
		return
	}
	maxAttemptNo := 0
	for index := range log.UpstreamAttempts {
		existing := &log.UpstreamAttempts[index]
		if existing.AttemptNo > maxAttemptNo {
			maxAttemptNo = existing.AttemptNo
		}
		if sameUsageUpstreamAttempt(*existing, attempt) {
			if existing.UpstreamActualCharge == nil && log.FinanceCostMode == FinanceCostModeRequestCharge && log.FinanceProtocolConfig != nil && len(log.UpstreamBillingPayload) > 0 {
				if charge, err := ExtractFinanceRequestCharge(log.UpstreamBillingPayload, *log.FinanceProtocolConfig); err == nil {
					ApplyFinanceRequestChargeToUsageAttempt(existing, charge)
				}
			}
			return
		}
	}
	attempt.AttemptNo = maxAttemptNo + 1
	if log.FinanceCostMode == FinanceCostModeRequestCharge && log.FinanceProtocolConfig != nil && len(log.UpstreamBillingPayload) > 0 {
		charge, err := ExtractFinanceRequestCharge(log.UpstreamBillingPayload, *log.FinanceProtocolConfig)
		if err == nil {
			ApplyFinanceRequestChargeToUsageAttempt(&attempt, charge)
		}
	}
	log.UpstreamAttempts = append(log.UpstreamAttempts, attempt)
}

func sameUsageUpstreamAttempt(left, right UsageUpstreamAttempt) bool {
	return left.AccountID == right.AccountID &&
		left.UpstreamModel == right.UpstreamModel &&
		left.InputTokens == right.InputTokens &&
		left.OutputTokens == right.OutputTokens &&
		left.CacheReadTokens == right.CacheReadTokens &&
		left.CacheCreationTokens == right.CacheCreationTokens &&
		left.CacheCreation5mTokens == right.CacheCreation5mTokens &&
		left.CacheCreation1hTokens == right.CacheCreation1hTokens &&
		left.RequestCount == right.RequestCount &&
		left.ImageCount == right.ImageCount &&
		left.VideoSeconds == right.VideoSeconds &&
		left.Billable == right.Billable
}

func CloneUsageUpstreamAttempts(attempts []UsageUpstreamAttempt) []UsageUpstreamAttempt {
	if len(attempts) == 0 {
		return nil
	}
	cloned := make([]UsageUpstreamAttempt, len(attempts))
	copy(cloned, attempts)
	for index := range cloned {
		cloned[index].UpstreamCostMultiplier = CloneDecimalSnapshot(attempts[index].UpstreamCostMultiplier)
		cloned[index].UpstreamMultiplierChangeID = cloneInt64Pointer(attempts[index].UpstreamMultiplierChangeID)
		cloned[index].UpstreamMultiplierEffectiveAt = cloneFinanceTime(attempts[index].UpstreamMultiplierEffectiveAt)
		cloned[index].AccountFinanceProfileID = cloneInt64Pointer(attempts[index].AccountFinanceProfileID)
		cloned[index].BillingObservedAt = cloneFinanceTime(attempts[index].BillingObservedAt)
		cloned[index].UpstreamActualCharge = CloneDecimalSnapshot(attempts[index].UpstreamActualCharge)
		cloned[index].UpstreamActualChargeUSD = CloneDecimalSnapshot(attempts[index].UpstreamActualChargeUSD)
		cloned[index].UpstreamStandardCharge = CloneDecimalSnapshot(attempts[index].UpstreamStandardCharge)
		cloned[index].UpstreamChargeSnapshot = cloneFinanceSnapshot(attempts[index].UpstreamChargeSnapshot)
	}
	return cloned
}

func (u *UsageLog) TotalTokens() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

func (u *UsageLog) EffectiveRequestType() RequestType {
	if u == nil {
		return RequestTypeUnknown
	}
	if normalized := u.RequestType.Normalize(); normalized != RequestTypeUnknown {
		return normalized
	}
	return RequestTypeFromLegacy(u.Stream, u.OpenAIWSMode)
}

func (u *UsageLog) SyncRequestTypeAndLegacyFields() {
	if u == nil {
		return
	}
	requestType := u.EffectiveRequestType()
	u.RequestType = requestType
	u.Stream, u.OpenAIWSMode = ApplyLegacyRequestFields(requestType, u.Stream, u.OpenAIWSMode)
}
