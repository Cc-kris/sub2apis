//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTimePricingConfigValidationAndMultiplier(t *testing.T) {
	cfg := &TimePricingConfig{Timezone: "Asia/Shanghai", Periods: []TimePricingPeriod{{StartMinute: 0, EndMinute: 720, Multiplier: 1.5}, {StartMinute: 720, EndMinute: 1440, Multiplier: 1}}}
	require.NoError(t, cfg.Validate())
	p := &ChannelModelPricing{TimePricing: cfg, FastMultiplier: pricingV2PtrFloat(1.2)}
	at := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC) // 09:00 Shanghai
	require.InDelta(t, 1.8, p.MultiplierAt(at, "fast"), 1e-12)
	cfg.Periods[1].StartMinute = 600
	require.Error(t, cfg.Validate())
}

func TestGroupPricingOverridesChannelAndMarksMixed(t *testing.T) {
	input := 2.0
	base := &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceChannel, BasePricing: &ModelPricing{InputPricePerToken: 9e-6}}
	r := &ModelPricingResolver{}
	r.applyGroupOverrides("gpt-5", &Group{LongContextPricingEnabled: true, ModelPricing: map[string]GroupModelPricing{"gpt-5": {Input: "2e-6", Intervals: []GroupPricingInterval{{MinTokens: 0, MaxTokens: pricingV2PtrInt(1000), Input: "3e-6"}}}}}, base)
	require.Equal(t, PricingSourceMixed, base.Source)
	require.Equal(t, "group", base.RuleLayer)
	require.InDelta(t, input*1e-6, base.BasePricing.InputPricePerToken, 1e-12)
	require.Len(t, base.Intervals, 1)
}

func TestChannelModifiersFreezeRequestStartAndDoNotMutateCachedIntervals(t *testing.T) {
	base := &ModelPricing{InputPricePerToken: 2e-6}
	intervalPrice := 3e-6
	fast := 1.2
	pricing := &ChannelModelPricing{
		TimePricing:    &TimePricingConfig{Timezone: "Asia/Shanghai", Periods: []TimePricingPeriod{{StartMinute: 0, EndMinute: 1440, Multiplier: 1.5}}},
		FastMultiplier: &fast,
	}
	resolved := &ResolvedPricing{BasePricing: base, Intervals: []PricingInterval{{InputPrice: &intervalPrice}}}
	resolver := &ModelPricingResolver{}
	resolver.applyChannelModifiers(pricing, resolved, PricingInput{RequestStartedAt: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), ServiceTier: "fast"})
	require.InDelta(t, 1.8, resolved.TimeMultiplier, 1e-12)
	require.InDelta(t, 3.6e-6, resolved.BasePricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 5.4e-6, *resolved.Intervals[0].InputPrice, 1e-12)
	require.InDelta(t, 3e-6, intervalPrice, 1e-12)
}

func TestValidateModelPricingRejectsInvalidIntervals(t *testing.T) {
	err := ValidateModelPricing(map[string]GroupModelPricing{
		"gpt-5": {Intervals: []GroupPricingInterval{{MinTokens: 0, MaxTokens: pricingV2PtrInt(1000), Input: "1"}, {MinTokens: 999, MaxTokens: nil, Input: "2"}}},
	})
	require.Error(t, err)
	require.NoError(t, ValidateModelPricing(map[string]GroupModelPricing{
		"gpt-5": {Intervals: []GroupPricingInterval{{MinTokens: 0, MaxTokens: pricingV2PtrInt(1000), Input: "1"}, {MinTokens: 1000, MaxTokens: nil, Input: "2"}}},
	}))
}

func pricingV2PtrInt(v int) *int           { return &v }
func pricingV2PtrFloat(v float64) *float64 { return &v }
