//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestFinanceCostCalculatorPreservesHistoricalFXEvidence(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	versionID := int64(88)
	multiplier := decimal.NewFromInt(1)
	quote := FinancePriceQuote{
		VersionID: 1, Source: FinancePricingSourceUpstreamExact, BillingMode: "token", Currency: "CNY",
		USDExchangeRate: decimal.RequireFromString("0.14"), FXRateVersionID: &versionID, FXSource: "provider_snapshot", FXObservedAt: &observedAt,
		Detail: FinancePriceDetail{Standard: FinanceRateCard{Input: financeDecimal("1")}},
	}
	result := NewFinanceCostCalculator().Calculate(FinanceCostCalculatorInput{
		Attempt:     UsageUpstreamAttempt{AttemptNo: 1, AccountID: 1, InputTokens: 1_000_000, UpstreamCostMultiplier: &multiplier, Billable: true},
		BillingMode: "token", Price: &quote,
	})
	require.Equal(t, FinanceCostStatusExact, result.Status)
	require.Equal(t, int64(88), *result.FXRateVersionID)
	require.Equal(t, "CNY", result.SourceCurrency)
	require.Equal(t, "0.14", result.FXRateToUSD.String())
	require.Equal(t, "provider_snapshot", result.FXSource)
	require.Equal(t, observedAt, *result.FXObservedAt)
}

func TestFinanceCostCalculatorUsesFrozenRequestChargeEvidence(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	original := decimal.RequireFromString("2.50")
	actualUSD := decimal.RequireFromString("0.35")
	result := NewFinanceCostCalculator().Calculate(FinanceCostCalculatorInput{
		Attempt: UsageUpstreamAttempt{
			AttemptNo:               1,
			AccountID:               9,
			Billable:                true,
			UpstreamActualCharge:    &original,
			UpstreamActualChargeUSD: &actualUSD,
			UpstreamChargeCurrency:  "CNY",
			BillingObservedAt:       &observedAt,
		},
		BillingMode: "token",
	})
	require.Equal(t, FinanceCostStatusExact, result.Status)
	require.Equal(t, "0.3500000000", result.Amount.StringFixed(10))
	require.Equal(t, "0.14", result.FXRateToUSD.String())
	require.Equal(t, "upstream_request_charge", result.FXSource)
	require.Equal(t, observedAt, *result.FXObservedAt)
}

func TestFinanceCostCalculatorPreservesLocalFXVersionForRequestCharge(t *testing.T) {
	original := decimal.RequireFromString("2.5")
	actualUSD := decimal.RequireFromString("0.35")
	result := NewFinanceCostCalculator().Calculate(FinanceCostCalculatorInput{
		Attempt: UsageUpstreamAttempt{
			AccountID: 1, Billable: true, UpstreamActualCharge: &original, UpstreamActualChargeUSD: &actualUSD,
			UpstreamChargeCurrency: "CNY", UpstreamChargeSnapshot: map[string]any{"fx_rate_version_id": int64(77)},
		},
		BillingMode: "token",
	})
	require.Equal(t, FinanceCostStatusExact, result.Status)
	require.NotNil(t, result.FXRateVersionID)
	require.Equal(t, int64(77), *result.FXRateVersionID)
}

func TestFinanceCostCalculatorMarksMissingRequestChargeEvidence(t *testing.T) {
	result := NewFinanceCostCalculator().Calculate(FinanceCostCalculatorInput{
		Attempt: UsageUpstreamAttempt{AccountID: 9, InputTokens: 1, Billable: true}, BillingMode: "token", RequestChargeExpected: true,
	})
	require.Equal(t, FinanceCostStatusMissingPrice, result.Status)
	require.Equal(t, "request_charge_missing", result.Detail["reason"])
}

func TestFinanceCostCalculatorTokenItemsMultiplierAndPrecision(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.RequireFromString("1.2500")
	quote := FinancePriceQuote{
		VersionID:       11,
		Source:          FinancePricingSourceUpstreamExact,
		BillingMode:     "token",
		Currency:        "USD",
		USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{Standard: FinanceRateCard{
			Input:        financeDecimal("4"),
			Output:       financeDecimal("12"),
			CacheRead:    financeDecimal("1"),
			CacheWrite5m: financeDecimal("5"),
			CacheWrite1h: financeDecimal("8"),
		}},
	}
	result := calculator.Calculate(FinanceCostCalculatorInput{
		Attempt: UsageUpstreamAttempt{
			AttemptNo:              1,
			AccountID:              9,
			UpstreamModel:          "gpt-test",
			InputTokens:            1_000,
			OutputTokens:           500,
			CacheReadTokens:        200,
			CacheCreation5mTokens:  100,
			CacheCreation1hTokens:  50,
			UpstreamCostMultiplier: &multiplier,
			Billable:               true,
		},
		BillingMode: "token",
		Price:       &quote,
	})

	require.Equal(t, FinanceCostStatusExact, result.Status)
	require.Equal(t, "0.0111000000", result.Amount.StringFixed(10))
	require.Len(t, result.Items, 5)
	require.Equal(t, "input", result.Items[0].Item)
	require.Equal(t, "0.0040000000", result.Items[0].Amount)
	require.Equal(t, "output", result.Items[1].Item)
	require.Equal(t, "0.0060000000", result.Items[1].Amount)
	require.Equal(t, "cache_read", result.Items[2].Item)
	require.Equal(t, "0.0002000000", result.Items[2].Amount)
	require.Equal(t, "cache_write_5m", result.Items[3].Item)
	require.Equal(t, "0.0005000000", result.Items[3].Amount)
	require.Equal(t, "cache_write_1h", result.Items[4].Item)
	require.Equal(t, "0.0004000000", result.Items[4].Amount)
	require.Equal(t, "1.0000", result.Items[0].UpstreamMultiplier)
	require.Equal(t, false, result.Detail["multiplier_applied"])
}

func TestFinanceCostCalculatorUsesGenericCacheWriteWithoutTTLBreakdown(t *testing.T) {
	multiplier := decimal.NewFromInt(1)
	quote := FinancePriceQuote{
		VersionID: 12, Source: FinancePricingSourceUpstreamExact, BillingMode: "token", Currency: "USD",
		USDExchangeRate: decimal.NewFromInt(1),
		Detail:          FinancePriceDetail{Standard: FinanceRateCard{CacheWrite5m: financeDecimal("5")}},
	}
	result := NewFinanceCostCalculator().Calculate(FinanceCostCalculatorInput{
		Attempt: UsageUpstreamAttempt{
			AccountID: 9, UpstreamModel: "gpt-test", CacheCreationTokens: 100,
			UpstreamCostMultiplier: &multiplier, Billable: true,
		},
		BillingMode: "token",
		Price:       &quote,
	})

	require.Equal(t, FinanceCostStatusExact, result.Status)
	require.Equal(t, "0.0005000000", result.Amount.StringFixed(10))
	require.Len(t, result.Items, 1)
	require.Equal(t, "cache_write", result.Items[0].Item)
	require.Equal(t, int64(100), result.Detail["usage"].(map[string]any)["cache_creation_tokens"])
}

func TestFinanceCostCalculatorFastUsesFastCardWithoutStandardStacking(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.NewFromInt(1)
	quote := FinancePriceQuote{
		VersionID:       12,
		Source:          FinancePricingSourceUpstreamExact,
		Currency:        "USD",
		USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{
			Standard: FinanceRateCard{Input: financeDecimal("2")},
			Fast:     &FinanceRateCard{Input: financeDecimal("7")},
		},
	}
	result := calculator.Calculate(FinanceCostCalculatorInput{
		Attempt:     UsageUpstreamAttempt{InputTokens: 1_000_000, UpstreamCostMultiplier: &multiplier, Billable: true},
		BillingMode: "token",
		ServiceTier: "fast",
		Price:       &quote,
	})
	require.Equal(t, "7.0000000000", result.Amount.StringFixed(10))
	require.Equal(t, "7", result.Items[0].SourceUnitPrice)
}

func TestFinanceCostCalculatorFastAndPriorityIncludeEveryCacheAndImageComponent(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.NewFromInt(1)
	quote := FinancePriceQuote{
		VersionID:       13,
		Source:          FinancePricingSourceUpstreamExact,
		Currency:        "USD",
		USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{
			Standard: FinanceRateCard{
				Input: financeDecimal("101"), Output: financeDecimal("102"), CacheRead: financeDecimal("103"),
				CacheWrite5m: financeDecimal("104"), CacheWrite1h: financeDecimal("105"), ImageOutput: financeDecimal("106"),
			},
			Fast: &FinanceRateCard{
				Input: financeDecimal("1"), Output: financeDecimal("2"), CacheRead: financeDecimal("3"),
				CacheWrite5m: financeDecimal("4"), CacheWrite1h: financeDecimal("5"), ImageOutput: financeDecimal("6"),
			},
		},
	}
	for _, serviceTier := range []string{"fast", "priority"} {
		t.Run(serviceTier, func(t *testing.T) {
			result := calculator.Calculate(FinanceCostCalculatorInput{
				Attempt: UsageUpstreamAttempt{
					InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000,
					CacheCreation5mTokens: 1_000_000, CacheCreation1hTokens: 1_000_000,
					UpstreamCostMultiplier: &multiplier, Billable: true,
				},
				BillingMode: "token", ServiceTier: serviceTier, ImageOutputTokens: 1_000_000, Price: &quote,
			})
			require.Equal(t, FinanceCostStatusExact, result.Status)
			require.Equal(t, "21.0000000000", result.Amount.StringFixed(10))
			require.Equal(t, []string{"input", "output", "cache_read", "cache_write_5m", "cache_write_1h", "image_output"}, []string{
				result.Items[0].Item, result.Items[1].Item, result.Items[2].Item,
				result.Items[3].Item, result.Items[4].Item, result.Items[5].Item,
			})
		})
	}
}

func TestFinanceCostCalculatorDirectModesAndTier(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.NewFromInt(1)
	max := int64(10)
	tests := []struct {
		name     string
		mode     string
		attempt  UsageUpstreamAttempt
		detail   FinancePriceDetail
		expected string
	}{
		{
			name:     "per request",
			mode:     "per_request",
			attempt:  UsageUpstreamAttempt{RequestCount: 2},
			detail:   FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("0.03")}},
			expected: "0.0600000000",
		},
		{
			name:     "per image",
			mode:     "image",
			attempt:  UsageUpstreamAttempt{ImageCount: 3},
			detail:   FinancePriceDetail{Standard: FinanceRateCard{PerImage: financeDecimal("0.08")}},
			expected: "0.2400000000",
		},
		{
			name:    "video second tier",
			mode:    "per_second",
			attempt: UsageUpstreamAttempt{VideoSeconds: 5},
			detail: FinancePriceDetail{
				Standard: FinanceRateCard{PerSecond: financeDecimal("0.10")},
				Tiers:    []FinancePriceTier{{MinQuantity: 1, MaxQuantity: &max, Prices: FinanceRateCard{PerSecond: financeDecimal("0.06")}}},
			},
			expected: "0.3000000000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := tt.attempt
			attempt.UpstreamCostMultiplier = &multiplier
			attempt.Billable = true
			result := calculator.Calculate(FinanceCostCalculatorInput{
				Attempt:     attempt,
				BillingMode: tt.mode,
				Price: &FinancePriceQuote{
					VersionID:       1,
					Source:          FinancePricingSourceUpstreamExact,
					Currency:        "USD",
					USDExchangeRate: decimal.NewFromInt(1),
					Detail:          tt.detail,
				},
			})
			require.Equal(t, tt.expected, result.Amount.StringFixed(10))
			if tt.mode == "image" {
				require.Equal(t, "per_image", result.Items[0].Item)
			}
			if tt.mode == "per_second" {
				require.Equal(t, "per_second", result.Items[0].Item)
			}
		})
	}
}

func TestFinanceCostCalculatorStatuses(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.NewFromInt(1)
	baseAttempt := UsageUpstreamAttempt{InputTokens: 100, Billable: true, UpstreamCostMultiplier: &multiplier}
	baseQuote := FinancePriceQuote{VersionID: 1, Source: FinancePricingSourceUpstreamExact, Currency: "USD", USDExchangeRate: decimal.NewFromInt(1), Detail: FinancePriceDetail{Standard: FinanceRateCard{Input: financeDecimal("1")}}}

	nonBillable := baseAttempt
	nonBillable.Billable = false
	require.Equal(t, FinanceCostStatusNonBillable, calculator.Calculate(FinanceCostCalculatorInput{Attempt: nonBillable, BillingMode: "token"}).Status)

	missingMultiplier := baseAttempt
	missingMultiplier.UpstreamCostMultiplier = nil
	exactWithoutMultiplier := calculator.Calculate(FinanceCostCalculatorInput{Attempt: missingMultiplier, BillingMode: "token", Price: &baseQuote})
	require.Equal(t, FinanceCostStatusExact, exactWithoutMultiplier.Status)
	require.NotNil(t, exactWithoutMultiplier.Amount)

	require.Equal(t, FinanceCostStatusMissingPrice, calculator.Calculate(FinanceCostCalculatorInput{Attempt: baseAttempt, BillingMode: "token"}).Status)

	missingUsage := baseAttempt
	missingUsage.InputTokens = 0
	require.Equal(t, FinanceCostStatusMissingUsage, calculator.Calculate(FinanceCostCalculatorInput{Attempt: missingUsage, BillingMode: "token", Price: &baseQuote}).Status)

	missingOutputPrice := baseAttempt
	missingOutputPrice.OutputTokens = 1
	require.Equal(t, FinanceCostStatusMissingPrice, calculator.Calculate(FinanceCostCalculatorInput{Attempt: missingOutputPrice, BillingMode: "token", Price: &baseQuote}).Status)

	estimatedQuote := baseQuote
	estimatedQuote.Source = FinancePricingSourceSystem
	estimated := calculator.Calculate(FinanceCostCalculatorInput{Attempt: baseAttempt, BillingMode: "token", Price: &estimatedQuote})
	require.Equal(t, FinanceCostStatusEstimated, estimated.Status)
	require.Equal(t, FinancePricingSourceSystem, estimated.PricingSource)
	require.Equal(t, true, estimated.Detail["multiplier_applied"])

	missingSystemMultiplier := baseAttempt
	missingSystemMultiplier.UpstreamCostMultiplier = nil
	require.Equal(t, FinanceCostStatusMissingMultiplier, calculator.Calculate(FinanceCostCalculatorInput{Attempt: missingSystemMultiplier, BillingMode: "token", Price: &estimatedQuote}).Status)

	require.Equal(t, FinanceCostStatusMissingProfile, calculator.Calculate(FinanceCostCalculatorInput{Attempt: baseAttempt, BillingMode: "token", MissingProfile: true}).Status)
	require.Equal(t, FinanceCostStatusUnsupportedUsage, calculator.Calculate(FinanceCostCalculatorInput{Attempt: baseAttempt, BillingMode: "unsupported", Price: &baseQuote}).Status)
	excluded := calculator.Calculate(FinanceCostCalculatorInput{Attempt: baseAttempt, BillingMode: "token", Excluded: true})
	require.Equal(t, FinanceCostStatusExcluded, excluded.Status)
	require.True(t, excluded.Amount.IsZero())
}

func TestFinanceCostCalculatorMultiCurrency(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.RequireFromString("1.5000")
	result := calculator.Calculate(FinanceCostCalculatorInput{
		Attempt:     UsageUpstreamAttempt{RequestCount: 2, Billable: true, UpstreamCostMultiplier: &multiplier},
		BillingMode: "per_request",
		Price: &FinancePriceQuote{
			VersionID:       7,
			Source:          FinancePricingSourceManual,
			Currency:        "CNY",
			USDExchangeRate: decimal.RequireFromString("0.14"),
			Detail:          FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("10")}},
		},
	})
	require.Equal(t, FinanceCostStatusEstimated, result.Status)
	require.Equal(t, "2.8000000000", result.Amount.StringFixed(10))
	require.Equal(t, "CNY", result.Items[0].SourceCurrency)
	require.Equal(t, "0.14", result.Items[0].USDExchangeRate)
}

func TestFinanceCostCalculatorAggregate(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.NewFromInt(1)
	quote := &FinancePriceQuote{
		VersionID:       1,
		Source:          FinancePricingSourceUpstreamExact,
		Currency:        "USD",
		USDExchangeRate: decimal.NewFromInt(1),
		Detail:          FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("0.5")}},
	}
	result := calculator.Aggregate([]FinanceCostCalculatorInput{
		{Attempt: UsageUpstreamAttempt{AttemptNo: 1, AccountID: 10, RequestCount: 1, Billable: true, UpstreamCostMultiplier: &multiplier}, BillingMode: "per_request", Price: quote},
		{Attempt: UsageUpstreamAttempt{AttemptNo: 2, AccountID: 11, RequestCount: 2, Billable: true, UpstreamCostMultiplier: &multiplier}, BillingMode: "per_request", Price: quote},
	})
	require.Equal(t, FinanceCostStatusExact, result.Status)
	require.Equal(t, "1.5000000000", result.Amount.StringFixed(10))
	require.Len(t, result.Segments, 2)

	missing := calculator.Aggregate([]FinanceCostCalculatorInput{
		{Attempt: UsageUpstreamAttempt{AttemptNo: 1, RequestCount: 1, Billable: true, UpstreamCostMultiplier: &multiplier}, BillingMode: "per_request", Price: quote},
		{Attempt: UsageUpstreamAttempt{AttemptNo: 2, RequestCount: 1, Billable: true, UpstreamCostMultiplier: &multiplier}, BillingMode: "per_request"},
	})
	require.Nil(t, missing.Amount)
	require.Equal(t, FinanceCostStatusMissingPrice, missing.Status)
}

func TestFinanceCostCalculatorPreservesExtremeDecimalValues(t *testing.T) {
	calculator := NewFinanceCostCalculator()
	multiplier := decimal.RequireFromString("9999.9999")
	quote := &FinancePriceQuote{
		VersionID:       1,
		Source:          FinancePricingSourceSystem,
		Currency:        "USD",
		USDExchangeRate: decimal.NewFromInt(1),
		Detail:          FinancePriceDetail{Standard: FinanceRateCard{Input: financeDecimal("0.00000001")}},
	}
	small := calculator.Calculate(FinanceCostCalculatorInput{
		Attempt:     UsageUpstreamAttempt{InputTokens: 1, Billable: true, UpstreamCostMultiplier: financeDecimal("0.0001")},
		BillingMode: "token",
		Price:       quote,
	})
	require.Equal(t, "0", small.Items[0].AmountBeforeRounding[:1])
	require.NotEmpty(t, small.Items[0].AmountBeforeRounding)

	large := calculator.Calculate(FinanceCostCalculatorInput{
		Attempt:     UsageUpstreamAttempt{InputTokens: 9_000_000_000_000, Billable: true, UpstreamCostMultiplier: &multiplier},
		BillingMode: "token",
		Price:       quote,
	})
	require.NotNil(t, large.Amount)
	require.NotContains(t, large.Amount.String(), "e")
}

func financeDecimal(value string) *decimal.Decimal {
	parsed := decimal.RequireFromString(value)
	return &parsed
}
