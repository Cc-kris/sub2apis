package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type FinanceCostStatus string

const (
	FinanceCostStatusExact             FinanceCostStatus = "exact"
	FinanceCostStatusEstimated         FinanceCostStatus = "estimated"
	FinanceCostStatusMissingProfile    FinanceCostStatus = "missing_profile"
	FinanceCostStatusMissingPrice      FinanceCostStatus = "missing_price"
	FinanceCostStatusMissingMultiplier FinanceCostStatus = "missing_multiplier"
	FinanceCostStatusMissingUsage      FinanceCostStatus = "missing_usage"
	FinanceCostStatusUnsupportedUsage  FinanceCostStatus = "unsupported_usage"
	FinanceCostStatusNonBillable       FinanceCostStatus = "non_billable"
	FinanceCostStatusExcluded          FinanceCostStatus = "excluded"
)

const (
	FinancePricingSourceSystem          = "system"
	FinancePricingSourceChannel         = "channel"
	FinancePricingSourceUpstreamCatalog = "upstream_catalog"
	FinancePricingSourceUpstreamExact   = "upstream_exact"
	FinancePricingSourceManual          = "manual"
	FinancePricingSourceEstimatedSystem = "estimated_system"
)

const financeAmountScale int32 = 10

var financeMillion = decimal.NewFromInt(1_000_000)

type FinanceRateCard struct {
	Input        *decimal.Decimal
	Output       *decimal.Decimal
	CacheRead    *decimal.Decimal
	CacheWrite5m *decimal.Decimal
	CacheWrite1h *decimal.Decimal
	ImageOutput  *decimal.Decimal
	PerRequest   *decimal.Decimal
	PerImage     *decimal.Decimal
	PerSecond    *decimal.Decimal
}

type FinancePriceTier struct {
	MinQuantity int64
	MaxQuantity *int64
	Label       string
	Prices      FinanceRateCard
}

type FinancePriceDetail struct {
	Standard FinanceRateCard
	Fast     *FinanceRateCard
	Tiers    []FinancePriceTier
}

type FinancePriceQuote struct {
	VersionID       int64
	Source          string
	BillingMode     string
	Currency        string
	USDExchangeRate decimal.Decimal
	FXRateVersionID *int64
	FXSource        string
	FXObservedAt    *time.Time
	Detail          FinancePriceDetail
}

type FinanceCostCalculatorInput struct {
	Attempt               UsageUpstreamAttempt
	BillingMode           string
	ServiceTier           string
	ImageOutputTokens     int64
	Price                 *FinancePriceQuote
	MissingProfile        bool
	RequestChargeExpected bool
	Excluded              bool
}

type FinanceCostItem struct {
	Item                   string `json:"item"`
	Quantity               int64  `json:"quantity"`
	Unit                   string `json:"unit"`
	SourceCurrency         string `json:"source_currency"`
	SourceUnitPrice        string `json:"source_unit_price"`
	USDExchangeRate        string `json:"usd_exchange_rate"`
	USDUnitPrice           string `json:"usd_unit_price"`
	UpstreamMultiplier     string `json:"upstream_multiplier"`
	AmountBeforeMultiplier string `json:"amount_before_multiplier"`
	AmountBeforeRounding   string `json:"amount_before_rounding"`
	Amount                 string `json:"amount"`
}

type FinanceCostCalculation struct {
	Status                     FinanceCostStatus `json:"status"`
	Amount                     *decimal.Decimal  `json:"-"`
	PricingSource              string            `json:"pricing_source,omitempty"`
	PriceVersionID             *int64            `json:"price_version_id,omitempty"`
	FXRateVersionID            *int64            `json:"fx_rate_version_id,omitempty"`
	SourceCurrency             string            `json:"source_currency,omitempty"`
	FXRateToUSD                *decimal.Decimal  `json:"-"`
	FXSource                   string            `json:"fx_source,omitempty"`
	FXObservedAt               *time.Time        `json:"fx_observed_at,omitempty"`
	UpstreamMultiplierSnapshot *decimal.Decimal  `json:"-"`
	Items                      []FinanceCostItem `json:"items"`
	Detail                     map[string]any    `json:"detail"`
}

type FinanceCostSegmentResult struct {
	AttemptNo          int
	AccountID          int64
	ChannelID          *int64
	UpstreamModel      string
	ServiceTier        *string
	Billable           bool
	UsageDetail        map[string]any
	UpstreamMultiplier *decimal.Decimal
	CostStatus         FinanceCostStatus
	CostAmount         *decimal.Decimal
	PricingSource      string
	PriceVersionID     *int64
	FXRateVersionID    *int64
	SourceCurrency     string
	FXRateToUSD        *decimal.Decimal
	FXSource           string
	FXObservedAt       *time.Time
	CalculationDetail  map[string]any
}

type FinanceCostAggregate struct {
	Status   FinanceCostStatus
	Amount   *decimal.Decimal
	Segments []FinanceCostSegmentResult
	Detail   map[string]any
}

type FinanceCostCalculator struct{}

func NewFinanceCostCalculator() *FinanceCostCalculator {
	return &FinanceCostCalculator{}
}

func (c *FinanceCostCalculator) Calculate(input FinanceCostCalculatorInput) FinanceCostCalculation {
	attempt := input.Attempt
	usageDetail := financeUsageDetail(attempt, input.ImageOutputTokens)
	baseDetail := map[string]any{
		"billing_mode": strings.TrimSpace(input.BillingMode),
		"service_tier": strings.TrimSpace(input.ServiceTier),
		"usage":        usageDetail,
	}
	if input.Excluded {
		zero := decimal.Zero
		baseDetail["reason"] = "usage_excluded_from_finance"
		baseDetail["total_before_rounding"] = zero.String()
		baseDetail["total"] = zero.StringFixed(financeAmountScale)
		return FinanceCostCalculation{Status: FinanceCostStatusExcluded, Amount: &zero, Items: []FinanceCostItem{}, Detail: baseDetail}
	}
	if !attempt.Billable {
		zero := decimal.Zero
		baseDetail["reason"] = "attempt_marked_non_billable"
		baseDetail["total_before_rounding"] = zero.String()
		baseDetail["total"] = zero.StringFixed(financeAmountScale)
		return FinanceCostCalculation{
			Status: FinanceCostStatusNonBillable,
			Amount: &zero,
			Items:  []FinanceCostItem{},
			Detail: baseDetail,
		}
	}
	if input.MissingProfile {
		baseDetail["reason"] = "wallet_assignment_missing"
		return FinanceCostCalculation{Status: FinanceCostStatusMissingProfile, Detail: baseDetail}
	}
	if attempt.UpstreamActualChargeUSD != nil {
		amount := attempt.UpstreamActualChargeUSD.Round(financeAmountScale)
		fxRate := decimal.NewFromInt(1)
		if attempt.UpstreamActualCharge != nil && !attempt.UpstreamActualCharge.IsZero() {
			fxRate = attempt.UpstreamActualChargeUSD.Div(*attempt.UpstreamActualCharge)
		}
		baseDetail["pricing_source"] = FinancePricingSourceUpstreamExact
		baseDetail["request_charge_original"] = decimalPointerString(attempt.UpstreamActualCharge)
		baseDetail["request_charge_usd"] = amount.StringFixed(financeAmountScale)
		baseDetail["request_charge_currency"] = attempt.UpstreamChargeCurrency
		baseDetail["request_charge_unit_semantics"] = attempt.UpstreamChargeUnitSemantics
		baseDetail["fx_rate_to_usd"] = fxRate.String()
		baseDetail["fx_source"] = "upstream_request_charge"
		baseDetail["upstream_billing_request_id"] = attempt.UpstreamBillingRequestID
		baseDetail["request_charge_snapshot"] = attempt.UpstreamChargeSnapshot
		var fxVersionID *int64
		if raw, ok := attempt.UpstreamChargeSnapshot["fx_rate_version_id"]; ok {
			if parsed, err := decimal.NewFromString(strings.TrimSpace(fmt.Sprint(raw))); err == nil && parsed.IsInteger() && parsed.IsPositive() {
				value := parsed.IntPart()
				fxVersionID = &value
			}
		}
		baseDetail["total_before_rounding"] = attempt.UpstreamActualChargeUSD.String()
		baseDetail["total"] = amount.StringFixed(financeAmountScale)
		return FinanceCostCalculation{
			Status: FinanceCostStatusExact, Amount: &amount, PricingSource: FinancePricingSourceUpstreamExact, FXRateVersionID: fxVersionID,
			SourceCurrency: attempt.UpstreamChargeCurrency, UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier),
			FXRateToUSD: cloneDecimal(&fxRate), FXSource: "upstream_request_charge", FXObservedAt: cloneFinanceTime(attempt.BillingObservedAt),
			Items: []FinanceCostItem{}, Detail: baseDetail,
		}
	}
	if input.Price == nil {
		if input.RequestChargeExpected {
			baseDetail["reason"] = "request_charge_missing"
			baseDetail["pricing_source"] = FinancePricingSourceUpstreamExact
		} else {
			baseDetail["reason"] = "price_version_missing"
		}
		return FinanceCostCalculation{Status: FinanceCostStatusMissingPrice, UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier), Detail: baseDetail}
	}

	quote := *input.Price
	applyMultiplier := financePricingSourceUsesMultiplier(quote.Source)
	if applyMultiplier && attempt.UpstreamCostMultiplier == nil {
		baseDetail["reason"] = "upstream_multiplier_snapshot_missing"
		return FinanceCostCalculation{
			Status:         FinanceCostStatusMissingMultiplier,
			PricingSource:  quote.Source,
			PriceVersionID: positiveInt64Pointer(quote.VersionID),
			Detail:         baseDetail,
		}
	}
	appliedMultiplier := decimal.NewFromInt(1)
	if applyMultiplier {
		appliedMultiplier = *attempt.UpstreamCostMultiplier
	}
	currency := strings.ToUpper(strings.TrimSpace(quote.Currency))
	if currency == "" {
		currency = "USD"
	}
	exchangeRate := quote.USDExchangeRate
	if currency == "USD" && exchangeRate.IsZero() {
		exchangeRate = decimal.NewFromInt(1)
	}
	if exchangeRate.LessThanOrEqual(decimal.Zero) {
		baseDetail["reason"] = "usd_exchange_rate_missing"
		baseDetail["source_currency"] = currency
		return FinanceCostCalculation{Status: FinanceCostStatusMissingPrice, UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier), Detail: baseDetail}
	}

	mode := normalizeFinanceBillingMode(input.BillingMode)
	if !financeBillingModeSupported(mode) {
		baseDetail["reason"] = "billing_mode_unsupported"
		return FinanceCostCalculation{Status: FinanceCostStatusUnsupportedUsage, UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier), Detail: baseDetail}
	}
	if !financeUsagePresent(mode, attempt, input.ImageOutputTokens) {
		baseDetail["reason"] = "billable_usage_missing"
		return FinanceCostCalculation{Status: FinanceCostStatusMissingUsage, UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier), Detail: baseDetail}
	}

	card, tierLabel := financeRateCardFor(quote.Detail, input.ServiceTier, financeUsageQuantity(mode, attempt, input.ImageOutputTokens))
	items, missingItem := calculateFinanceItems(mode, attempt, input.ImageOutputTokens, card, currency, exchangeRate, appliedMultiplier)
	if missingItem != "" {
		baseDetail["reason"] = "unit_price_missing"
		baseDetail["missing_item"] = missingItem
		baseDetail["source_currency"] = currency
		return FinanceCostCalculation{
			Status:                     FinanceCostStatusMissingPrice,
			PricingSource:              quote.Source,
			PriceVersionID:             positiveInt64Pointer(quote.VersionID),
			UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier),
			Detail:                     baseDetail,
		}
	}

	totalBeforeRounding := decimal.Zero
	for _, item := range items {
		amount, err := decimal.NewFromString(item.AmountBeforeRounding)
		if err == nil {
			totalBeforeRounding = totalBeforeRounding.Add(amount)
		}
	}
	total := totalBeforeRounding.Round(financeAmountScale)
	status := FinanceCostStatusExact
	pricingSource := quote.Source
	if !isExactFinancePricingSource(quote.Source) {
		status = FinanceCostStatusEstimated
		if quote.Source == FinancePricingSourceChannel {
			pricingSource = FinancePricingSourceEstimatedSystem
		}
	}
	baseDetail["price_version_id"] = quote.VersionID
	baseDetail["pricing_source"] = pricingSource
	baseDetail["source_currency"] = currency
	baseDetail["usd_exchange_rate"] = exchangeRate.String()
	if input.Price.FXRateVersionID != nil {
		baseDetail["fx_rate_version_id"] = *input.Price.FXRateVersionID
	}
	if strings.TrimSpace(input.Price.FXSource) != "" {
		baseDetail["fx_source"] = strings.TrimSpace(input.Price.FXSource)
	}
	baseDetail["multiplier_applied"] = applyMultiplier
	if attempt.UpstreamCostMultiplier != nil {
		baseDetail["upstream_multiplier_snapshot"] = attempt.UpstreamCostMultiplier.StringFixed(4)
	}
	baseDetail["selected_tier"] = tierLabel
	baseDetail["items"] = items
	baseDetail["total_before_rounding"] = totalBeforeRounding.String()
	baseDetail["total"] = total.StringFixed(financeAmountScale)

	return FinanceCostCalculation{
		Status:                     status,
		Amount:                     &total,
		PricingSource:              pricingSource,
		PriceVersionID:             positiveInt64Pointer(quote.VersionID),
		FXRateVersionID:            cloneInt64Pointer(quote.FXRateVersionID),
		SourceCurrency:             currency,
		FXRateToUSD:                cloneDecimal(&exchangeRate),
		FXSource:                   strings.TrimSpace(quote.FXSource),
		FXObservedAt:               cloneFinanceTime(quote.FXObservedAt),
		UpstreamMultiplierSnapshot: cloneDecimal(attempt.UpstreamCostMultiplier),
		Items:                      items,
		Detail:                     baseDetail,
	}
}

func (c *FinanceCostCalculator) Aggregate(inputs []FinanceCostCalculatorInput) FinanceCostAggregate {
	segments := make([]FinanceCostSegmentResult, 0, len(inputs))
	total := decimal.Zero
	overall := FinanceCostStatusNonBillable
	allAmountsKnown := true
	for _, input := range inputs {
		calculation := c.Calculate(input)
		segment := FinanceCostSegmentResult{
			AttemptNo:          input.Attempt.AttemptNo,
			AccountID:          input.Attempt.AccountID,
			ChannelID:          input.Attempt.ChannelID,
			UpstreamModel:      input.Attempt.UpstreamModel,
			ServiceTier:        input.Attempt.ServiceTier,
			Billable:           input.Attempt.Billable,
			UsageDetail:        financeUsageDetail(input.Attempt, input.ImageOutputTokens),
			UpstreamMultiplier: cloneDecimal(input.Attempt.UpstreamCostMultiplier),
			CostStatus:         calculation.Status,
			CostAmount:         cloneDecimal(calculation.Amount),
			PricingSource:      calculation.PricingSource,
			PriceVersionID:     cloneInt64Pointer(calculation.PriceVersionID),
			FXRateVersionID:    cloneInt64Pointer(calculation.FXRateVersionID),
			SourceCurrency:     calculation.SourceCurrency,
			FXRateToUSD:        cloneDecimal(calculation.FXRateToUSD),
			FXSource:           calculation.FXSource,
			FXObservedAt:       cloneFinanceTime(calculation.FXObservedAt),
			CalculationDetail:  calculation.Detail,
		}
		segments = append(segments, segment)
		if calculation.Amount == nil {
			allAmountsKnown = false
		} else {
			total = total.Add(*calculation.Amount)
		}
		overall = mergeFinanceCostStatus(overall, calculation.Status)
	}
	if len(inputs) == 0 {
		allAmountsKnown = false
		overall = FinanceCostStatusMissingUsage
	}
	var amount *decimal.Decimal
	if allAmountsKnown {
		rounded := total.Round(financeAmountScale)
		amount = &rounded
	}
	return FinanceCostAggregate{
		Status:   overall,
		Amount:   amount,
		Segments: segments,
		Detail: map[string]any{
			"segment_count": len(segments),
			"total":         decimalPointerString(amount),
			"status":        overall,
		},
	}
}

func calculateFinanceItems(mode string, attempt UsageUpstreamAttempt, imageOutputTokens int64, card FinanceRateCard, currency string, exchangeRate, multiplier decimal.Decimal) ([]FinanceCostItem, string) {
	items := make([]FinanceCostItem, 0, 6)
	add := func(name string, quantity int64, unit string, unitPrice *decimal.Decimal, divisor decimal.Decimal) string {
		if quantity <= 0 {
			return ""
		}
		if unitPrice == nil {
			return name
		}
		quantityDecimal := decimal.NewFromInt(quantity)
		beforeMultiplier := quantityDecimal.Div(divisor).Mul(*unitPrice).Mul(exchangeRate)
		beforeRounding := beforeMultiplier.Mul(multiplier)
		amount := beforeRounding.Round(financeAmountScale)
		items = append(items, FinanceCostItem{
			Item:                   name,
			Quantity:               quantity,
			Unit:                   unit,
			SourceCurrency:         currency,
			SourceUnitPrice:        unitPrice.String(),
			USDExchangeRate:        exchangeRate.String(),
			USDUnitPrice:           unitPrice.Mul(exchangeRate).String(),
			UpstreamMultiplier:     multiplier.StringFixed(4),
			AmountBeforeMultiplier: beforeMultiplier.String(),
			AmountBeforeRounding:   beforeRounding.String(),
			Amount:                 amount.StringFixed(financeAmountScale),
		})
		return ""
	}

	switch mode {
	case "token":
		if missing := add("input", attempt.InputTokens, string(PriceUnitPerMillionTokens), card.Input, financeMillion); missing != "" {
			return nil, missing
		}
		if missing := add("output", attempt.OutputTokens, string(PriceUnitPerMillionTokens), card.Output, financeMillion); missing != "" {
			return nil, missing
		}
		if missing := add("cache_read", attempt.CacheReadTokens, string(PriceUnitPerMillionCacheTokens), card.CacheRead, financeMillion); missing != "" {
			return nil, missing
		}
		if attempt.CacheCreation5mTokens == 0 && attempt.CacheCreation1hTokens == 0 {
			if missing := add("cache_write", attempt.CacheCreationTokens, string(PriceUnitPerMillionCacheTokens), card.CacheWrite5m, financeMillion); missing != "" {
				return nil, missing
			}
		}
		if missing := add("cache_write_5m", attempt.CacheCreation5mTokens, string(PriceUnitPerMillionCacheTokens), card.CacheWrite5m, financeMillion); missing != "" {
			return nil, missing
		}
		if missing := add("cache_write_1h", attempt.CacheCreation1hTokens, string(PriceUnitPerMillionCacheTokens), card.CacheWrite1h, financeMillion); missing != "" {
			return nil, missing
		}
		if missing := add("image_output", imageOutputTokens, string(PriceUnitPerMillionTokens), card.ImageOutput, financeMillion); missing != "" {
			return nil, missing
		}
	case "per_request":
		if missing := add("per_request", attempt.RequestCount, string(PriceUnitPerRequest), card.PerRequest, decimal.NewFromInt(1)); missing != "" {
			return nil, missing
		}
	case "image":
		if missing := add("per_image", attempt.ImageCount, string(PriceUnitPerImage), card.PerImage, decimal.NewFromInt(1)); missing != "" {
			return nil, missing
		}
	case "per_second":
		if missing := add("per_second", attempt.VideoSeconds, string(PriceUnitPerSecond), card.PerSecond, decimal.NewFromInt(1)); missing != "" {
			return nil, missing
		}
	default:
		return nil, "billing_mode"
	}
	return items, ""
}

func financeRateCardFor(detail FinancePriceDetail, serviceTier string, quantity int64) (FinanceRateCard, string) {
	tierName := strings.ToLower(strings.TrimSpace(serviceTier))
	card := detail.Standard
	if (tierName == "fast" || tierName == "priority") && detail.Fast != nil {
		card = *detail.Fast
	}
	for _, tier := range detail.Tiers {
		labelMatches := tier.Label != "" && strings.EqualFold(strings.TrimSpace(tier.Label), strings.TrimSpace(serviceTier))
		quantityMatches := quantity >= tier.MinQuantity && (tier.MaxQuantity == nil || quantity <= *tier.MaxQuantity)
		if labelMatches || (tier.Label == "" && quantityMatches) {
			return mergeFinanceRateCards(card, tier.Prices), tier.Label
		}
	}
	return card, tierName
}

func mergeFinanceRateCards(base, override FinanceRateCard) FinanceRateCard {
	result := base
	if override.Input != nil {
		result.Input = override.Input
	}
	if override.Output != nil {
		result.Output = override.Output
	}
	if override.CacheRead != nil {
		result.CacheRead = override.CacheRead
	}
	if override.CacheWrite5m != nil {
		result.CacheWrite5m = override.CacheWrite5m
	}
	if override.CacheWrite1h != nil {
		result.CacheWrite1h = override.CacheWrite1h
	}
	if override.ImageOutput != nil {
		result.ImageOutput = override.ImageOutput
	}
	if override.PerRequest != nil {
		result.PerRequest = override.PerRequest
	}
	if override.PerImage != nil {
		result.PerImage = override.PerImage
	}
	if override.PerSecond != nil {
		result.PerSecond = override.PerSecond
	}
	return result
}

func normalizeFinanceBillingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "token", "tokens":
		return "token"
	case "per_request", "request":
		return "per_request"
	case "image", "per_image":
		return "image"
	case "per_second", "video":
		return "per_second"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func financeBillingModeSupported(mode string) bool {
	switch mode {
	case "token", "per_request", "image", "per_second":
		return true
	default:
		return false
	}
}

func financeUsagePresent(mode string, attempt UsageUpstreamAttempt, imageOutputTokens int64) bool {
	switch mode {
	case "token":
		return attempt.InputTokens > 0 || attempt.OutputTokens > 0 || attempt.CacheReadTokens > 0 || attempt.CacheCreationTokens > 0 || attempt.CacheCreation5mTokens > 0 || attempt.CacheCreation1hTokens > 0 || imageOutputTokens > 0
	case "per_request":
		return attempt.RequestCount > 0
	case "image":
		return attempt.ImageCount > 0
	case "per_second":
		return attempt.VideoSeconds > 0
	default:
		return false
	}
}

func financeUsageQuantity(mode string, attempt UsageUpstreamAttempt, imageOutputTokens int64) int64 {
	switch mode {
	case "token":
		return attempt.InputTokens + attempt.OutputTokens + attempt.CacheReadTokens + effectiveFinanceCacheCreationTokens(attempt) + imageOutputTokens
	case "per_request":
		return attempt.RequestCount
	case "image":
		return attempt.ImageCount
	case "per_second":
		return attempt.VideoSeconds
	default:
		return 0
	}
}

func financeUsageDetail(attempt UsageUpstreamAttempt, imageOutputTokens int64) map[string]any {
	return map[string]any{
		"input_tokens":             attempt.InputTokens,
		"output_tokens":            attempt.OutputTokens,
		"cache_read_tokens":        attempt.CacheReadTokens,
		"cache_creation_tokens":    attempt.CacheCreationTokens,
		"cache_creation_5m_tokens": attempt.CacheCreation5mTokens,
		"cache_creation_1h_tokens": attempt.CacheCreation1hTokens,
		"image_output_tokens":      imageOutputTokens,
		"request_count":            attempt.RequestCount,
		"image_count":              attempt.ImageCount,
		"video_seconds":            attempt.VideoSeconds,
	}
}

func effectiveFinanceCacheCreationTokens(attempt UsageUpstreamAttempt) int64 {
	if attempt.CacheCreation5mTokens > 0 || attempt.CacheCreation1hTokens > 0 {
		return attempt.CacheCreation5mTokens + attempt.CacheCreation1hTokens
	}
	return attempt.CacheCreationTokens
}

func mergeFinanceCostStatus(current, next FinanceCostStatus) FinanceCostStatus {
	rank := map[FinanceCostStatus]int{
		FinanceCostStatusNonBillable:       0,
		FinanceCostStatusExcluded:          1,
		FinanceCostStatusExact:             2,
		FinanceCostStatusEstimated:         3,
		FinanceCostStatusMissingPrice:      4,
		FinanceCostStatusMissingProfile:    5,
		FinanceCostStatusUnsupportedUsage:  6,
		FinanceCostStatusMissingUsage:      7,
		FinanceCostStatusMissingMultiplier: 8,
	}
	if rank[next] > rank[current] {
		return next
	}
	return current
}

func isExactFinancePricingSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case FinancePricingSourceUpstreamExact:
		return true
	default:
		return false
	}
}

func financePricingSourceUsesMultiplier(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case FinancePricingSourceUpstreamExact, FinancePricingSourceUpstreamCatalog, FinancePricingSourceManual:
		return false
	default:
		return true
	}
}

func positiveInt64Pointer(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneDecimal(value *decimal.Decimal) *decimal.Decimal {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func decimalPointerString(value *decimal.Decimal) any {
	if value == nil {
		return nil
	}
	return value.StringFixed(financeAmountScale)
}

func ParseFinanceDecimal(value any) (*decimal.Decimal, error) {
	if value == nil {
		return nil, nil
	}
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case fmt.Stringer:
		raw = typed.String()
	case float64:
		raw = decimal.NewFromFloat(typed).String()
	case float32:
		raw = decimal.NewFromFloat32(typed).String()
	case int:
		raw = decimal.NewFromInt(int64(typed)).String()
	case int64:
		raw = decimal.NewFromInt(typed).String()
	case int32:
		raw = decimal.NewFromInt32(typed).String()
	default:
		return nil, fmt.Errorf("unsupported decimal value type %T", value)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := decimal.NewFromString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid decimal %q: %w", raw, err)
	}
	if parsed.IsNegative() {
		return nil, fmt.Errorf("decimal must be non-negative")
	}
	return &parsed, nil
}
