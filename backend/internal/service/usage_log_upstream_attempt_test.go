//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestBuildFinalUsageUpstreamAttemptCopiesImmutableBillingFacts(t *testing.T) {
	multiplier := decimal.RequireFromString("1.2500")
	upstreamModel := "gpt-5.5-2026-07-01"
	serviceTier := "priority"
	createdAt := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	changeID, profileID := int64(41), int64(51)
	log := &UsageLog{
		AccountID: 9, RequestID: "req-attempt", Model: "gpt-5.5", UpstreamModel: &upstreamModel,
		ServiceTier: &serviceTier, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheCreationTokens: 6,
		CacheCreation5mTokens: 4, CacheCreation1hTokens: 5,
		UpstreamCostMultiplier: &multiplier, CreatedAt: createdAt,
		UpstreamMultiplierChangeID: &changeID, UpstreamMultiplierSource: "account_config",
		UpstreamMultiplierEffectiveAt: &createdAt, AccountFinanceProfileID: &profileID,
	}

	attempt, ok := BuildFinalUsageUpstreamAttempt(log)
	require.True(t, ok)
	require.True(t, attempt.Billable)
	require.Equal(t, "gpt-5.5-2026-07-01", attempt.UpstreamModel)
	require.Equal(t, int64(3), attempt.CacheReadTokens)
	require.Equal(t, int64(6), attempt.CacheCreationTokens)
	require.Equal(t, "1.2500", attempt.UpstreamCostMultiplier.StringFixed(4))
	require.Equal(t, int64(41), *attempt.UpstreamMultiplierChangeID)
	require.Equal(t, "account_config", attempt.UpstreamMultiplierSource)
	require.Equal(t, int64(51), *attempt.AccountFinanceProfileID)

	multiplier = decimal.RequireFromString("9.9999")
	require.Equal(t, "1.2500", attempt.UpstreamCostMultiplier.StringFixed(4))
}

func TestBuildFinalUsageUpstreamAttemptSupportsImageVideoAndPerRequest(t *testing.T) {
	perRequest := "per_request"
	seconds := 12
	log := &UsageLog{
		AccountID: 10, RequestID: "req-media", Model: "media-model", BillingMode: &perRequest,
		ImageCount: 2, VideoDurationSeconds: &seconds, CreatedAt: time.Now(),
	}

	attempt, ok := BuildFinalUsageUpstreamAttempt(log)
	require.True(t, ok)
	require.Equal(t, int64(1), attempt.RequestCount)
	require.Equal(t, int64(2), attempt.ImageCount)
	require.Equal(t, int64(12), attempt.VideoSeconds)
}

func TestEnsureFinalUsageUpstreamAttemptAppendsAfterPriorBillableRetry(t *testing.T) {
	multiplier := decimal.RequireFromString("1.2500")
	log := &UsageLog{
		AccountID: 2, RequestID: "req-1", Model: "model-final", InputTokens: 200,
		UpstreamCostMultiplier: &multiplier,
		UpstreamAttempts: []UsageUpstreamAttempt{{
			AttemptNo: 1, AccountID: 1, UpstreamModel: "model-first", InputTokens: 100,
			UpstreamCostMultiplier: &multiplier, Billable: true,
		}},
	}
	EnsureFinalUsageUpstreamAttempt(log)
	require.Len(t, log.UpstreamAttempts, 2)
	require.Equal(t, 2, log.UpstreamAttempts[1].AttemptNo)
	require.Equal(t, int64(2), log.UpstreamAttempts[1].AccountID)
	EnsureFinalUsageUpstreamAttempt(log)
	require.Len(t, log.UpstreamAttempts, 2, "final attempt must remain idempotent")
}

func TestEnsureFinalUsageUpstreamAttemptCapturesRequestChargeAndRedactsSnapshot(t *testing.T) {
	config := FinanceProtocolConfig{
		Capabilities:  []string{FinanceCapabilityRequestCharge},
		CostMode:      FinanceCostModeRequestCharge,
		UnitSemantics: FinanceUnitFiatCurrency,
		Operations: map[string]FinanceProtocolOperation{FinanceCapabilityRequestCharge: {
			Mapping: map[string]string{"actual_cost": "$.amount", "currency": "$.currency", "fx_rate_to_usd": "$.fx", "billing_request_id": "$.id"},
		}},
		RedactPaths: []string{"$.token"},
	}
	log := &UsageLog{
		AccountID: 3, RequestID: "req-charge", Model: "gpt-5", InputTokens: 10, CreatedAt: time.Now().UTC(),
		FinanceCostMode: FinanceCostModeRequestCharge, FinanceProtocolConfig: &config,
		UpstreamBillingPayload: []byte(`{"amount":"2.5","currency":"CNY","fx":"0.14","id":"bill-1","token":"secret"}`),
	}
	EnsureFinalUsageUpstreamAttempt(log)
	require.Len(t, log.UpstreamAttempts, 1)
	attempt := log.UpstreamAttempts[0]
	require.Equal(t, "2.5", attempt.UpstreamActualCharge.String())
	require.Equal(t, "0.35", attempt.UpstreamActualChargeUSD.String())
	require.Equal(t, "CNY", attempt.UpstreamChargeCurrency)
	require.Equal(t, "bill-1", attempt.UpstreamBillingRequestID)
	require.NotContains(t, attempt.UpstreamChargeSnapshot, "token")
}
