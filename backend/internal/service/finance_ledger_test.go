//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestFinanceUsageScannerBuildsMultiAttemptProjection(t *testing.T) {
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	multiplier := decimal.RequireFromString("1.2500")
	mode := "per_request"
	log := UsageLog{ID: 100, UserID: 8, AccountID: 12, RequestID: "req-1", Model: "model-a", RequestedModel: "model-a", BillingMode: &mode, UsageListValue: financeDecimal("2.50"), CreatedAt: now}
	ledger := &financeLedgerStub{
		launchAt: now.Add(-time.Hour),
		logs:     []UsageLog{log},
		attempts: map[int64][]UsageUpstreamAttempt{
			100: {
				{AttemptNo: 1, AccountID: 11, UpstreamModel: "model-a", RequestCount: 1, UpstreamCostMultiplier: &multiplier, UpstreamMultiplierChangeID: int64PointerForFinanceTest(11), AccountFinanceProfileID: int64PointerForFinanceTest(21), Billable: true, CreatedAt: now},
				{AttemptNo: 2, AccountID: 12, UpstreamModel: "model-a", RequestCount: 2, UpstreamCostMultiplier: &multiplier, UpstreamMultiplierChangeID: int64PointerForFinanceTest(12), AccountFinanceProfileID: int64PointerForFinanceTest(22), Billable: true, CreatedAt: now},
			},
		},
		acquired: true,
	}
	quote := &FinancePriceQuote{
		VersionID: 7, Source: FinancePricingSourceUpstreamExact, BillingMode: mode,
		Currency: "CNY", USDExchangeRate: decimal.RequireFromString("0.14"), FXRateVersionID: int64PointerForFinanceTest(99), FXSource: "provider_snapshot",
		FXObservedAt: timePointerForFinanceTest(now.Add(-time.Minute)),
		Detail:       FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("0.40")}},
	}
	priceRepo := &financePriceLookupStub{
		wallet: &FinanceWalletAssignment{WalletID: 3, UpstreamID: 4}, upstream: quote,
		profiles: map[int64]*AccountFinanceProfile{
			21: {ID: 21, AccountID: 11, WalletID: int64Snapshot(3), CostMode: FinanceCostModeManual, AccountMultiplierSnapshot: &multiplier},
			22: {ID: 22, AccountID: 12, WalletID: int64Snapshot(3), CostMode: FinanceCostModeManual, AccountMultiplierSnapshot: &multiplier},
		},
	}
	scanner := NewFinanceUsageScanner(ledger, NewFinancePriceSelector(priceRepo), NewFinanceCostCalculator())
	scanner.now = func() time.Time { return now.Add(time.Second) }

	result, err := scanner.RunBatch(context.Background())
	require.NoError(t, err)
	require.True(t, result.Acquired)
	require.Equal(t, 1, result.Succeeded)
	require.Equal(t, 0, result.Failed)
	require.Equal(t, []time.Time{now}, result.SucceededAt)
	require.Len(t, ledger.created, 1)
	projection := ledger.created[0]
	require.Equal(t, FinanceCostStatusExact, projection.CostStatus)
	require.Equal(t, "0.1680000000", projection.UpstreamCost.StringFixed(10))
	require.Len(t, projection.Segments, 2)
	require.Equal(t, int64(3), *projection.WalletID)
	require.Equal(t, int64(4), *projection.UpstreamID)
	require.Equal(t, "2.5", projection.UsageListValue.String())
	require.Equal(t, int64(99), *projection.FXRateVersionID)
	require.Equal(t, "CNY", projection.SourceCurrency)
	require.Equal(t, int64(12), *projection.Segments[1].UpstreamMultiplierChangeID)
	require.Equal(t, int64(99), *projection.Segments[1].FXRateVersionID)
}

func TestFinanceUsageScannerPreservesXSearchUsageListValue(t *testing.T) {
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	value := decimal.RequireFromString("0.0123456789")
	mode := string(BillingModePerRequest)
	log := &UsageLog{
		ID: 200, UserID: 8, AccountID: 12, RequestID: "x-search-finance",
		Model: "x_search", RequestedModel: "x_search", BillingMode: &mode,
		ActualCost: value.InexactFloat64(), UsageListValue: &value, CreatedAt: now,
	}
	scanner := NewFinanceUsageScanner(
		&financeLedgerStub{launchAt: now, acquired: true},
		NewFinancePriceSelector(&financePriceLookupStub{}),
		NewFinanceCostCalculator(),
	)
	projection, err := scanner.buildProjection(context.Background(), log, nil, now)
	require.NoError(t, err)
	require.NotNil(t, projection.UsageListValue)
	require.Equal(t, "0.0123456789", projection.UsageListValue.StringFixed(10))
}

func int64PointerForFinanceTest(value int64) *int64        { return &value }
func timePointerForFinanceTest(value time.Time) *time.Time { return &value }

func TestFinanceUsageScannerMissingAttemptsBoundary(t *testing.T) {
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	multiplier := decimal.NewFromInt(1)
	quote := &FinancePriceQuote{
		VersionID: 1, Source: FinancePricingSourceUpstreamExact, Currency: "USD", USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{Standard: FinanceRateCard{Input: financeDecimal("1")}},
	}
	tests := []struct {
		name              string
		createdAt         time.Time
		wantStatus        FinanceCostStatus
		wantAttemptSource any
	}{
		{name: "legacy usage log fallback", createdAt: now.Add(-time.Second), wantStatus: FinanceCostStatusExact, wantAttemptSource: "legacy_usage_log"},
		{name: "new usage missing immutable attempt", createdAt: now.Add(time.Second), wantStatus: FinanceCostStatusMissingUsage, wantAttemptSource: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := &financeLedgerStub{
				launchAt: now,
				logs:     []UsageLog{{ID: 1, UserID: 1, AccountID: 2, RequestID: "req", Model: "model-a", InputTokens: 1_000_000, UpstreamCostMultiplier: &multiplier, CreatedAt: tt.createdAt}},
				attempts: map[int64][]UsageUpstreamAttempt{}, acquired: true,
			}
			scanner := NewFinanceUsageScanner(ledger, NewFinancePriceSelector(&financePriceLookupStub{
				wallet:   &FinanceWalletAssignment{WalletID: 1, UpstreamID: 1},
				upstream: quote, system: quote,
			}), NewFinanceCostCalculator())
			_, err := scanner.RunBatch(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, ledger.created[0].CostStatus)
			require.Equal(t, tt.wantAttemptSource, ledger.created[0].CalculationDetail["attempt_source"])
		})
	}
}

func TestFinanceUsageScannerContinuesAfterSingleFailureAndHonorsLease(t *testing.T) {
	now := time.Now().UTC()
	ledger := &financeLedgerStub{
		launchAt: now.Add(-time.Hour),
		logs: []UsageLog{
			{ID: 1, UserID: 1, AccountID: 1, RequestID: "a", Model: "x", CreatedAt: now},
			{ID: 2, UserID: 1, AccountID: 1, RequestID: "b", Model: "x", CreatedAt: now.Add(time.Second)},
		},
		attempts: map[int64][]UsageUpstreamAttempt{}, acquired: true,
		createErrors: map[int64]error{1: errors.New("write failed")},
	}
	scanner := NewFinanceUsageScanner(ledger, NewFinancePriceSelector(&financePriceLookupStub{}), NewFinanceCostCalculator())
	result, err := scanner.RunBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, result.Processed)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, 1, result.Succeeded)
	require.Equal(t, int64(2), result.Cursor.ID)
	require.Equal(t, []time.Time{now.Add(time.Second)}, result.SucceededAt)
	require.Contains(t, ledger.retries[1], "write failed")
	require.Contains(t, ledger.resolved, int64(2))

	ledger.acquired = false
	result, err = scanner.RunBatch(context.Background())
	require.NoError(t, err)
	require.False(t, result.Acquired)
}

func TestFinanceBusinessTypeUsesImmutableUsageFacts(t *testing.T) {
	require.Equal(t, "balance", financeBusinessTypeForUsage(&UsageLog{BillingType: BillingTypeBalance}))
	require.Equal(t, "subscription", financeBusinessTypeForUsage(&UsageLog{BillingType: BillingTypeSubscription}))
	subscriptionID := int64(10)
	require.Equal(t, "subscription", financeBusinessTypeForUsage(&UsageLog{SubscriptionID: &subscriptionID}))
	require.Equal(t, "admin", financeBusinessTypeForUsage(&UsageLog{User: &User{Role: "admin"}}))
	require.Equal(t, "admin", financeBusinessTypeForUsage(&UsageLog{BillingType: BillingTypeSubscription, User: &User{Role: "admin"}}))
	require.Equal(t, "admin", financeBusinessTypeForUsage(&UsageLog{FinanceBusinessTypeSnapshot: "admin", User: &User{Role: "user"}}))
}

func TestFinanceAdminUsageIsExcludedFromProfit(t *testing.T) {
	selector := NewFinancePriceSelector(&financePriceLookupStub{wallet: &FinanceWalletAssignment{WalletID: 3, UpstreamID: 4}, upstream: &FinancePriceQuote{
		VersionID: 1, Source: FinancePricingSourceUpstreamExact, Currency: "USD", USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("5")}},
	}})
	scanner := NewFinanceUsageScanner(nil, selector, NewFinanceCostCalculator())
	requestCount := int64(1)
	projection, err := scanner.buildProjection(context.Background(), &UsageLog{
		ID: 9, UserID: 1, User: &User{ID: 1, Role: RoleAdmin}, BillingMode: stringSnapshot("per_request"),
		UsageListValue: financeDecimal("10"), CreatedAt: time.Now().UTC(),
	}, []UsageUpstreamAttempt{{AttemptNo: 1, AccountID: 2, UpstreamModel: "gpt-test", RequestCount: requestCount, Billable: true}}, time.Time{})
	require.NoError(t, err)
	require.Equal(t, "admin", projection.BusinessType)
	require.Equal(t, FinanceCostStatusExcluded, projection.CostStatus)
	require.NotNil(t, projection.UpstreamCost)
	require.True(t, projection.UpstreamCost.IsZero())
}

func TestFinancePromotionUsageRecognizesOnlyPaidRevenue(t *testing.T) {
	selector := NewFinancePriceSelector(&financePriceLookupStub{wallet: &FinanceWalletAssignment{WalletID: 3, UpstreamID: 4}, upstream: &FinancePriceQuote{
		VersionID: 1, Source: FinancePricingSourceUpstreamExact, Currency: "USD", USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("1")}},
	}})
	scanner := NewFinanceUsageScanner(nil, selector, NewFinanceCostCalculator())
	requestCount := int64(1)
	projection, err := scanner.buildProjection(context.Background(), &UsageLog{
		ID: 10, UserID: 2, FinanceBusinessTypeSnapshot: "promotion", PromotionCreditUsed: financeDecimal("2"),
		BillingMode: stringSnapshot("per_request"), UsageListValue: financeDecimal("3"), CreatedAt: time.Now().UTC(),
	}, []UsageUpstreamAttempt{{AttemptNo: 1, AccountID: 2, UpstreamModel: "gpt-test", RequestCount: requestCount, Billable: true}}, time.Time{})
	require.NoError(t, err)
	require.Equal(t, "promotion", projection.BusinessType)
	require.Equal(t, "1", projection.UsageListValue.String())
	require.Equal(t, "2.0000000000", projection.CalculationDetail["promotion_credit_used"])
}

func TestFinanceExplicitlyExcludedTrafficIsNeverIncludedInProfit(t *testing.T) {
	selector := NewFinancePriceSelector(&financePriceLookupStub{wallet: &FinanceWalletAssignment{WalletID: 3, UpstreamID: 4}, upstream: &FinancePriceQuote{
		VersionID: 1, Source: FinancePricingSourceUpstreamExact, Currency: "USD", USDExchangeRate: decimal.NewFromInt(1),
		Detail: FinancePriceDetail{Standard: FinanceRateCard{PerRequest: financeDecimal("5")}},
	}})
	scanner := NewFinanceUsageScanner(nil, selector, NewFinanceCostCalculator())
	requestCount := int64(1)
	projection, err := scanner.buildProjection(context.Background(), &UsageLog{
		ID: 11, UserID: 3, FinanceExcluded: true, FinanceExclusionReason: "campaign_test",
		BillingMode: stringSnapshot("per_request"), UsageListValue: financeDecimal("10"), CreatedAt: time.Now().UTC(),
	}, []UsageUpstreamAttempt{{AttemptNo: 1, AccountID: 2, UpstreamModel: "gpt-test", RequestCount: requestCount, Billable: true}}, time.Time{})
	require.NoError(t, err)
	require.Equal(t, FinanceCostStatusExcluded, projection.CostStatus)
	require.NotNil(t, projection.UpstreamCost)
	require.True(t, projection.UpstreamCost.IsZero())
	require.Equal(t, "campaign_test", projection.CalculationDetail["finance_exclusion_reason"])
}

type financeLedgerStub struct {
	launchAt     time.Time
	logs         []UsageLog
	attempts     map[int64][]UsageUpstreamAttempt
	created      []*UsageFinanceProjection
	createErrors map[int64]error
	acquired     bool
	retries      map[int64]string
	resolved     []int64
}

func (s *financeLedgerStub) FinanceLaunchAt(context.Context) (time.Time, error) {
	return s.launchAt, nil
}

func (s *financeLedgerStub) TryAcquireScannerLease(context.Context) (func(), bool, error) {
	return func() {}, s.acquired, nil
}

func (s *financeLedgerStub) ListPendingUsage(_ context.Context, cursor FinanceUsageCursor, _ int) ([]UsageLog, error) {
	result := make([]UsageLog, 0, len(s.logs))
	for _, log := range s.logs {
		if log.CreatedAt.After(cursor.CreatedAt) || (log.CreatedAt.Equal(cursor.CreatedAt) && log.ID > cursor.ID) {
			result = append(result, log)
		}
	}
	return result, nil
}

func (s *financeLedgerStub) LoadUsageAttempts(_ context.Context, _ []int64) (map[int64][]UsageUpstreamAttempt, error) {
	return s.attempts, nil
}

func (s *financeLedgerStub) CreateFinanceProjection(_ context.Context, projection *UsageFinanceProjection) (bool, error) {
	if err := s.createErrors[projection.UsageLogID]; err != nil {
		return false, err
	}
	s.created = append(s.created, projection)
	return true, nil
}

func (s *financeLedgerStub) ReviseFinanceProjection(context.Context, *UsageFinanceProjection, FinanceRevisionMetadata) (bool, error) {
	return false, nil
}

func (s *financeLedgerStub) RecordFinanceProjectionFailure(_ context.Context, usageLogID int64, message string, _ time.Time) error {
	if s.retries == nil {
		s.retries = map[int64]string{}
	}
	s.retries[usageLogID] = message
	return nil
}

func (s *financeLedgerStub) ResolveFinanceProjectionFailure(_ context.Context, usageLogID int64, _ time.Time) error {
	s.resolved = append(s.resolved, usageLogID)
	delete(s.retries, usageLogID)
	return nil
}
