package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/shopspring/decimal"
)

type SalesPricingVersion string

const (
	SalesPricingVersionLegacy SalesPricingVersion = "legacy"
	SalesPricingVersionShadow SalesPricingVersion = "shadow"
	SalesPricingVersionV2     SalesPricingVersion = "v2"
)

func (v SalesPricingVersion) IsValid() bool {
	switch v {
	case SalesPricingVersionLegacy, SalesPricingVersionShadow, SalesPricingVersionV2:
		return true
	default:
		return false
	}
}

type SalesPricingContext struct {
	RequestedModel string              `json:"requested_model"`
	EffectiveModel string              `json:"effective_model"`
	LegacySource   string              `json:"legacy_source,omitempty"`
	Version        SalesPricingVersion `json:"version"`
	PricingSource  string              `json:"pricing_source"`
	Checksum       string              `json:"checksum,omitempty"`
	Multiplier     decimal.Decimal     `json:"-"`
	Prices         *ModelPriceView     `json:"prices"`
	Shadow         *SalesPricingShadow `json:"shadow,omitempty"`
	RuleLayer      string              `json:"rule_layer,omitempty"`
	TimeMultiplier decimal.Decimal     `json:"-"`
	ServiceTier    string              `json:"service_tier,omitempty"`
}

type SalesPricingShadow struct {
	EffectiveModel string          `json:"effective_model"`
	PricingSource  string          `json:"pricing_source"`
	Checksum       string          `json:"checksum,omitempty"`
	Multiplier     decimal.Decimal `json:"-"`
	Prices         *ModelPriceView `json:"prices"`
}

type SalesPricingUsageSnapshot struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CacheCreationTokens   int `json:"cache_creation_tokens"`
	CacheReadTokens       int `json:"cache_read_tokens"`
	CacheCreation5mTokens int `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int `json:"cache_creation_1h_tokens"`
	ImageOutputTokens     int `json:"image_output_tokens"`
	ImageCount            int `json:"image_count"`
	VideoCount            int `json:"video_count"`
	VideoDurationSeconds  int `json:"video_duration_seconds"`
}

type SalesPricingAmountSnapshot struct {
	Input           string `json:"input"`
	Output          string `json:"output"`
	CacheCreation   string `json:"cache_creation"`
	CacheRead       string `json:"cache_read"`
	ImageOutput     string `json:"image_output"`
	OriginalTotal   string `json:"original_total"`
	MultiplierTotal string `json:"multiplier_total"`
}

type SalesPricingSnapshot struct {
	RequestedModel string                     `json:"requested_model"`
	EffectiveModel string                     `json:"effective_model"`
	LegacySource   string                     `json:"legacy_source,omitempty"`
	Version        SalesPricingVersion        `json:"version"`
	PricingSource  string                     `json:"pricing_source"`
	Checksum       string                     `json:"checksum"`
	Multiplier     string                     `json:"multiplier"`
	BillingMode    string                     `json:"billing_mode"`
	ServiceTier    string                     `json:"service_tier,omitempty"`
	BillingTier    string                     `json:"billing_tier,omitempty"`
	Rule           map[string]string          `json:"rule,omitempty"`
	Prices         *ModelPriceView            `json:"prices"`
	Usage          SalesPricingUsageSnapshot  `json:"usage"`
	Amounts        SalesPricingAmountSnapshot `json:"amounts"`
}

func BuildV2SalesPricingContext(requestedModel, checksum string, resolved *ResolvedPricing, multiplier decimal.Decimal) (*SalesPricingContext, error) {
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		return nil, errors.New("requested model is required")
	}
	prices, err := BuildModelPriceView(resolved, multiplier)
	if err != nil {
		return nil, err
	}
	ctx := &SalesPricingContext{
		RequestedModel: model,
		EffectiveModel: model,
		Version:        SalesPricingVersionV2,
		PricingSource:  normalizeSalesPricingSource(resolved.Source),
		Checksum:       strings.TrimSpace(checksum),
		Multiplier:     multiplier,
		Prices:         prices,
		RuleLayer:      resolved.RuleLayer,
		TimeMultiplier: decimal.NewFromFloat(maxFloat(resolved.TimeMultiplier, 1)),
		ServiceTier:    resolved.ServiceTier,
	}
	if ctx.Checksum == "" {
		ctx.Checksum = salesPricingChecksum(ctx.EffectiveModel, ctx.PricingSource, ctx.Prices)
	}
	return ctx, nil
}

func maxFloat(v, fallback float64) float64 {
	if v <= 0 {
		return fallback
	}
	return v
}

func ResolveUnifiedSalesPricing(ctx context.Context, resolver *ModelPricingResolver, channels *ChannelService, groupID *int64, requestedModel string) (*ResolvedPricing, error) {
	return resolveUnifiedSalesPricing(ctx, resolver, channels, groupID, PricingInput{Model: requestedModel})
}

func resolveUnifiedSalesPricing(ctx context.Context, resolver *ModelPricingResolver, channels *ChannelService, groupID *int64, input PricingInput) (*ResolvedPricing, error) {
	if resolver == nil {
		return nil, errors.New("model pricing resolver is required")
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return nil, errors.New("requested model is required")
	}
	input.Model = model
	resolved := resolver.Resolve(ctx, input)
	if channels != nil && groupID != nil {
		if channelPricing := channels.GetChannelModelPricing(ctx, *groupID, model); channelPricing != nil {
			if IsCompleteChannelSalesPricing(*channelPricing) {
				input.GroupID = groupID
				resolved = resolver.Resolve(ctx, input)
			}
		}
	}
	if resolved == nil {
		return nil, errors.New("sales pricing is unavailable")
	}
	return resolved, nil
}

func CalculateUnifiedSalesPricingCost(
	ctx context.Context,
	billingService *BillingService,
	resolver *ModelPricingResolver,
	resolved *ResolvedPricing,
	groupID *int64,
	model string,
	log *UsageLog,
	multiplier decimal.Decimal,
) (*CostBreakdown, error) {
	if billingService == nil || resolver == nil || resolved == nil || log == nil {
		return nil, errors.New("billing service, resolver, resolved pricing and usage log are required")
	}
	requestCount := 1
	sizeTier := ""
	switch resolved.Mode {
	case BillingModeImage:
		requestCount = max(log.ImageCount, 1)
		if log.ImageSize != nil {
			sizeTier = NormalizeImageBillingTierOrDefault(*log.ImageSize)
		}
	case BillingModeVideo, BillingModePerSecond:
		requestCount = max(log.VideoCount, 1) * max(intSnapshotValue(log.VideoDurationSeconds), 1)
	}
	serviceTier := ""
	if log.ServiceTier != nil {
		serviceTier = strings.TrimSpace(*log.ServiceTier)
	}
	// A channel-specific fast/flex multiplier is already frozen into the
	// resolved prices. Do not apply the generic service-tier multiplier again.
	if resolved.ExplicitServiceTier {
		serviceTier = ""
	}
	return billingService.CalculateCostUnified(CostInput{
		Ctx:     ctx,
		Model:   model,
		GroupID: groupID,
		Tokens: UsageTokens{
			InputTokens:           log.InputTokens,
			OutputTokens:          log.OutputTokens,
			CacheCreationTokens:   log.CacheCreationTokens,
			CacheReadTokens:       log.CacheReadTokens,
			CacheCreation5mTokens: log.CacheCreation5mTokens,
			CacheCreation1hTokens: log.CacheCreation1hTokens,
			ImageOutputTokens:     log.ImageOutputTokens,
		},
		RequestCount:   requestCount,
		SizeTier:       sizeTier,
		RateMultiplier: multiplier.InexactFloat64(),
		ServiceTier:    serviceTier,
		Resolver:       resolver,
		Resolved:       resolved,
	})
}

func ResolveV2SalesPricingContextAndCost(
	ctx context.Context,
	resolver *ModelPricingResolver,
	channels *ChannelService,
	billingService *BillingService,
	groupID *int64,
	requestedModel string,
	log *UsageLog,
	multiplier decimal.Decimal,
) (*SalesPricingContext, *CostBreakdown, error) {
	input := PricingInput{Model: requestedModel, GroupID: groupID}
	if log != nil {
		input.RequestStartedAt = log.CreatedAt
		if log.ServiceTier != nil {
			input.ServiceTier = *log.ServiceTier
		}
	}
	resolved, err := resolveUnifiedSalesPricing(ctx, resolver, channels, groupID, input)
	if err != nil {
		return nil, nil, err
	}
	pricingContext, err := BuildV2SalesPricingContext(requestedModel, "", resolved, multiplier)
	if err != nil {
		return nil, nil, err
	}
	cost, err := CalculateUnifiedSalesPricingCost(ctx, billingService, resolver, resolved, groupID, requestedModel, log, multiplier)
	if err != nil {
		return nil, nil, err
	}
	return pricingContext, cost, nil
}

func ApplyCostBreakdownToUsageLog(log *UsageLog, cost *CostBreakdown) {
	if log == nil || cost == nil {
		return
	}
	log.InputCost = cost.InputCost
	log.OutputCost = cost.OutputCost
	log.ImageOutputCost = cost.ImageOutputCost
	log.CacheCreationCost = cost.CacheCreationCost
	log.CacheReadCost = cost.CacheReadCost
	log.TotalCost = cost.TotalCost
	log.ActualCost = cost.ActualCost
	if mode := strings.TrimSpace(cost.BillingMode); mode != "" {
		log.BillingMode = &mode
	}
}

// ApplyConfiguredSalesPricing applies the rollout state to one completed usage
// fact. It never changes scheduling or persistence. If v2 resolution fails, the
// caller receives the legacy cost plus an error and can preserve the existing
// charge rather than losing the usage record.
func ApplyConfiguredSalesPricing(
	ctx context.Context,
	settingService *SettingService,
	resolver *ModelPricingResolver,
	channels *ChannelService,
	billingService *BillingService,
	groupID *int64,
	requestedModel string,
	log *UsageLog,
	legacyContext *SalesPricingContext,
	legacyCost *CostBreakdown,
	multiplier decimal.Decimal,
) (*CostBreakdown, error) {
	if settingService != nil && !settingService.IsSalesPricingResolverEnabled(ctx) {
		if err := ApplySalesPricingContextToUsageLog(log, legacyContext, legacyCost); err != nil {
			return legacyCost, err
		}
		return legacyCost, nil
	}
	version := SalesPricingVersionLegacy
	if settingService != nil {
		version = settingService.GetSalesPricingVersion(ctx)
	}
	switch version {
	case SalesPricingVersionShadow:
		v2Context, v2Cost, err := ResolveV2SalesPricingContextAndCost(ctx, resolver, channels, billingService, groupID, requestedModel, log, multiplier)
		if err != nil {
			_ = ApplySalesPricingContextToUsageLog(log, legacyContext, legacyCost)
			return legacyCost, err
		}
		shadowContext, err := AttachV2Shadow(legacyContext, v2Context)
		if err != nil {
			return legacyCost, err
		}
		if err = ApplyShadowSalesPricingContextToUsageLog(log, shadowContext, legacyCost, v2Cost); err != nil {
			return legacyCost, err
		}
		return legacyCost, nil
	case SalesPricingVersionV2:
		v2Context, v2Cost, err := ResolveV2SalesPricingContextAndCost(ctx, resolver, channels, billingService, groupID, requestedModel, log, multiplier)
		if err != nil {
			_ = ApplySalesPricingContextToUsageLog(log, legacyContext, legacyCost)
			return legacyCost, err
		}
		ApplyCostBreakdownToUsageLog(log, v2Cost)
		log.RateMultiplier = multiplier.InexactFloat64()
		if err = ApplySalesPricingContextToUsageLog(log, v2Context, v2Cost); err != nil {
			return legacyCost, err
		}
		return v2Cost, nil
	default:
		if err := ApplySalesPricingContextToUsageLog(log, legacyContext, legacyCost); err != nil {
			return legacyCost, err
		}
		return legacyCost, nil
	}
}

func BuildLegacySalesPricingContext(requestedModel, effectiveModel, legacySource, checksum string, resolved *ResolvedPricing, multiplier decimal.Decimal) (*SalesPricingContext, error) {
	requested := strings.TrimSpace(requestedModel)
	effective := strings.TrimSpace(effectiveModel)
	if requested == "" || effective == "" {
		return nil, errors.New("requested and effective models are required")
	}
	prices, err := BuildModelPriceView(resolved, multiplier)
	if err != nil {
		return nil, err
	}
	ctx := &SalesPricingContext{
		RequestedModel: requested,
		EffectiveModel: effective,
		LegacySource:   strings.TrimSpace(legacySource),
		Version:        SalesPricingVersionLegacy,
		PricingSource:  normalizeSalesPricingSource(resolved.Source),
		Checksum:       strings.TrimSpace(checksum),
		Multiplier:     multiplier,
		Prices:         prices,
	}
	if ctx.Checksum == "" {
		ctx.Checksum = salesPricingChecksum(ctx.EffectiveModel, ctx.PricingSource, ctx.Prices)
	}
	return ctx, nil
}

// ResolveLegacySalesPricingContext reconstructs the exact request-time unit
// price surface without changing the legacy billing calculation. Non-token
// modes use the realized pre-multiplier amount to preserve group image/video
// prices that are not represented in the system token catalog.
func ResolveLegacySalesPricingContext(
	ctx context.Context,
	resolver *ModelPricingResolver,
	billingService *BillingService,
	requestedModel string,
	effectiveModel string,
	legacySource string,
	groupID *int64,
	billingMode BillingMode,
	requestCount int,
	videoSeconds int,
	cost *CostBreakdown,
	multiplier decimal.Decimal,
	settingServices ...*SettingService,
) (*SalesPricingContext, error) {
	if billingMode == "" {
		billingMode = BillingModeToken
	}
	if len(settingServices) > 0 && settingServices[0] != nil && !settingServices[0].IsSalesPricingResolverEnabled(ctx) {
		resolved := &ResolvedPricing{Mode: billingMode, Source: "system"}
		if billingMode == BillingModeToken && billingService != nil {
			if pricing, err := billingService.GetModelPricing(effectiveModel); err == nil {
				resolved.BasePricing = pricing
			}
		} else if billingMode != BillingModeToken {
			units := requestCount
			if units <= 0 {
				units = 1
			}
			if billingMode == BillingModeVideo || billingMode == BillingModePerSecond {
				if videoSeconds <= 0 {
					videoSeconds = 1
				}
				units *= videoSeconds
			}
			if cost == nil {
				return nil, errors.New("cost is required for realized non-token pricing")
			}
			resolved.DefaultPerRequestPrice = cost.TotalCost / float64(units)
			resolved.DefaultPerRequestPricePresent = true
		}
		return BuildLegacySalesPricingContext(requestedModel, effectiveModel, legacySource, "", resolved, multiplier)
	}
	var resolved *ResolvedPricing
	if billingMode == BillingModeToken {
		if resolver != nil {
			resolved = resolver.Resolve(ctx, PricingInput{Model: effectiveModel, GroupID: groupID})
		} else if billingService != nil {
			pricing, err := billingService.GetModelPricing(effectiveModel)
			if err == nil {
				resolved = &ResolvedPricing{Mode: BillingModeToken, BasePricing: pricing, Source: "system"}
			}
		}
		if resolved == nil {
			resolved = &ResolvedPricing{Mode: BillingModeToken, Source: "system"}
		}
	} else if resolver != nil {
		candidate := resolver.Resolve(ctx, PricingInput{Model: effectiveModel, GroupID: groupID})
		if candidate != nil && candidate.Mode == billingMode {
			resolved = candidate
		}
	}
	if resolved == nil {
		if cost == nil {
			return nil, errors.New("cost is required for realized non-token pricing")
		}
		units := requestCount
		if units <= 0 {
			units = 1
		}
		if billingMode == BillingModeVideo || billingMode == BillingModePerSecond {
			if videoSeconds <= 0 {
				videoSeconds = 1
			}
			units *= videoSeconds
		}
		resolved = &ResolvedPricing{
			Mode:                          billingMode,
			DefaultPerRequestPrice:        cost.TotalCost / float64(units),
			DefaultPerRequestPricePresent: true,
			Source:                        "system",
		}
	}
	return BuildLegacySalesPricingContext(requestedModel, effectiveModel, legacySource, "", resolved, multiplier)
}

func ApplySalesPricingContextToUsageLog(log *UsageLog, pricing *SalesPricingContext, cost *CostBreakdown) error {
	if log == nil || pricing == nil || cost == nil {
		return errors.New("usage log, pricing context and cost are required")
	}
	wired, err := buildSalesPricingUsageSnapshot(log, pricing, cost)
	if err != nil {
		return err
	}
	version := string(pricing.Version)
	log.SalesModel = stringSnapshot(pricing.RequestedModel)
	log.SalesPricingEffectiveModel = stringSnapshot(pricing.EffectiveModel)
	log.SalesPricingLegacySource = stringSnapshot(pricing.LegacySource)
	log.SalesPricingVersion = &version
	log.SalesPricingSource = stringSnapshot(pricing.PricingSource)
	log.SalesPricingChecksum = stringSnapshot(pricing.Checksum)
	log.SalesPricingSnapshot = wired
	usageListValue := decimal.NewFromFloat(cost.ActualCost)
	log.UsageListValue = &usageListValue
	return nil
}

func ApplyShadowSalesPricingContextToUsageLog(log *UsageLog, pricing *SalesPricingContext, legacyCost, v2Cost *CostBreakdown) error {
	if pricing == nil || pricing.Version != SalesPricingVersionShadow || pricing.Shadow == nil {
		return errors.New("shadow sales pricing context is required")
	}
	if err := ApplySalesPricingContextToUsageLog(log, pricing, legacyCost); err != nil {
		return err
	}
	v2Context := &SalesPricingContext{
		RequestedModel: pricing.RequestedModel,
		EffectiveModel: pricing.Shadow.EffectiveModel,
		Version:        SalesPricingVersionV2,
		PricingSource:  pricing.Shadow.PricingSource,
		Checksum:       pricing.Shadow.Checksum,
		Multiplier:     pricing.Shadow.Multiplier,
		Prices:         pricing.Shadow.Prices,
	}
	shadowSnapshot, err := buildSalesPricingUsageSnapshot(log, v2Context, v2Cost)
	if err != nil {
		return err
	}
	log.SalesPricingShadowSnapshot = shadowSnapshot
	delta := decimal.NewFromFloat(v2Cost.ActualCost).Sub(decimal.NewFromFloat(legacyCost.ActualCost))
	log.SalesPricingShadowDelta = &delta
	return nil
}

func buildSalesPricingUsageSnapshot(log *UsageLog, pricing *SalesPricingContext, cost *CostBreakdown) (map[string]any, error) {
	snapshot := SalesPricingSnapshot{
		RequestedModel: pricing.RequestedModel,
		EffectiveModel: pricing.EffectiveModel,
		LegacySource:   pricing.LegacySource,
		Version:        pricing.Version,
		PricingSource:  pricing.PricingSource,
		Checksum:       pricing.Checksum,
		Multiplier:     pricing.Multiplier.StringFixed(4),
		BillingMode:    effectiveUsageBillingMode(log),
		Prices:         pricing.Prices,
		Usage: SalesPricingUsageSnapshot{
			InputTokens:           log.InputTokens,
			OutputTokens:          log.OutputTokens,
			CacheCreationTokens:   log.CacheCreationTokens,
			CacheReadTokens:       log.CacheReadTokens,
			CacheCreation5mTokens: log.CacheCreation5mTokens,
			CacheCreation1hTokens: log.CacheCreation1hTokens,
			ImageOutputTokens:     log.ImageOutputTokens,
			ImageCount:            log.ImageCount,
			VideoCount:            log.VideoCount,
			VideoDurationSeconds:  intSnapshotValue(log.VideoDurationSeconds),
		},
		Amounts: SalesPricingAmountSnapshot{
			Input:           decimal.NewFromFloat(cost.InputCost).StringFixed(10),
			Output:          decimal.NewFromFloat(cost.OutputCost).StringFixed(10),
			CacheCreation:   decimal.NewFromFloat(cost.CacheCreationCost).StringFixed(10),
			CacheRead:       decimal.NewFromFloat(cost.CacheReadCost).StringFixed(10),
			ImageOutput:     decimal.NewFromFloat(cost.ImageOutputCost).StringFixed(10),
			OriginalTotal:   decimal.NewFromFloat(cost.TotalCost).StringFixed(10),
			MultiplierTotal: decimal.NewFromFloat(cost.ActualCost).StringFixed(10),
		},
		Rule: map[string]string{"layer": pricing.RuleLayer, "service_tier": pricing.ServiceTier, "time_multiplier": pricing.TimeMultiplier.String()},
	}
	if log.ServiceTier != nil {
		snapshot.ServiceTier = strings.TrimSpace(*log.ServiceTier)
	}
	if log.BillingTier != nil {
		snapshot.BillingTier = strings.TrimSpace(*log.BillingTier)
	}
	return snapshotMap(snapshot)
}

func normalizeSalesPricingSource(source string) string {
	if strings.EqualFold(strings.TrimSpace(source), PricingSourceChannel) {
		return PricingSourceChannel
	}
	if strings.EqualFold(strings.TrimSpace(source), PricingSourceMixed) {
		return PricingSourceMixed
	}
	return "system"
}

func salesPricingChecksum(model, source string, prices *ModelPriceView) string {
	payload, _ := json.Marshal(struct {
		Model  string          `json:"model"`
		Source string          `json:"source"`
		Prices *ModelPriceView `json:"prices"`
	}{Model: model, Source: source, Prices: prices})
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func snapshotMap(snapshot SalesPricingSnapshot) (map[string]any, error) {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func effectiveUsageBillingMode(log *UsageLog) string {
	if log != nil && log.BillingMode != nil && strings.TrimSpace(*log.BillingMode) != "" {
		return strings.TrimSpace(*log.BillingMode)
	}
	return string(BillingModeToken)
}

func intSnapshotValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func stringSnapshot(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func ResolveLegacySalesPricingModelSource(configured, requestedModel, effectiveModel, channelMappedModel, upstreamModel string) string {
	configured = strings.TrimSpace(configured)
	switch configured {
	case BillingModelSourceRequested, BillingModelSourceChannelMapped, BillingModelSourceUpstream:
		return configured
	}
	effective := strings.TrimSpace(effectiveModel)
	if effective != "" && effective == strings.TrimSpace(requestedModel) {
		return BillingModelSourceRequested
	}
	if effective != "" && effective == strings.TrimSpace(channelMappedModel) {
		return BillingModelSourceChannelMapped
	}
	if effective != "" && effective == strings.TrimSpace(upstreamModel) {
		return BillingModelSourceUpstream
	}
	return BillingModelSourceUpstream
}

func AttachV2Shadow(legacy *SalesPricingContext, v2 *SalesPricingContext) (*SalesPricingContext, error) {
	if legacy == nil || v2 == nil {
		return nil, errors.New("legacy and v2 pricing contexts are required")
	}
	copyContext := *legacy
	copyContext.Version = SalesPricingVersionShadow
	copyContext.Shadow = &SalesPricingShadow{
		EffectiveModel: v2.EffectiveModel,
		PricingSource:  v2.PricingSource,
		Checksum:       v2.Checksum,
		Multiplier:     v2.Multiplier,
		Prices:         v2.Prices,
	}
	return &copyContext, nil
}

func ValidateSalesPricingTransition(from, to SalesPricingVersion) error {
	if !from.IsValid() || !to.IsValid() {
		return errors.New("invalid sales pricing version")
	}
	if from == to {
		return nil
	}
	if (from == SalesPricingVersionLegacy && to == SalesPricingVersionShadow) ||
		(from == SalesPricingVersionShadow && (to == SalesPricingVersionLegacy || to == SalesPricingVersionV2)) ||
		(from == SalesPricingVersionV2 && to == SalesPricingVersionShadow) {
		return nil
	}
	return errors.New("invalid sales pricing version transition")
}
