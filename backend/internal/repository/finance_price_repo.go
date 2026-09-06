package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/financefxrateversion"
	"github.com/Wei-Shaw/sub2api/ent/systemmodelpriceversion"
	"github.com/Wei-Shaw/sub2api/ent/upstreammodelpriceversion"
	"github.com/Wei-Shaw/sub2api/ent/upstreamwalletaccount"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

type financePriceLookupRepository struct {
	client *dbent.Client
}

func NewFinancePriceLookupRepository(client *dbent.Client) service.FinancePriceLookupRepository {
	return &financePriceLookupRepository{client: client}
}

func (r *financePriceLookupRepository) FindAccountFinanceProfileByID(ctx context.Context, profileID int64) (*service.AccountFinanceProfile, error) {
	profile, err := r.client.AccountFinanceProfile.Get(ctx, profileID)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find account finance profile: %w", err)
	}
	return &service.AccountFinanceProfile{
		ID: profile.ID, AccountID: profile.AccountID, WalletID: cloneRepositoryInt64(profile.WalletID),
		ProtocolVersionID: cloneRepositoryInt64(profile.ProtocolVersionID), CostMode: profile.CostMode,
		AccountMultiplierSnapshot: cloneRepositoryDecimal(profile.AccountMultiplierSnapshot),
		ContractMultiplier:        cloneRepositoryDecimal(profile.ContractMultiplier), ReadinessStatus: profile.ReadinessStatus,
		Version: profile.Version, EffectiveFrom: profile.EffectiveFrom, EffectiveTo: cloneRepositoryTime(profile.EffectiveTo),
	}, nil
}

func (r *financePriceLookupRepository) FindWalletByID(ctx context.Context, walletID int64) (*service.FinanceWalletAssignment, error) {
	wallet, err := r.client.UpstreamWallet.Get(ctx, walletID)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find wallet: %w", err)
	}
	return &service.FinanceWalletAssignment{WalletID: wallet.ID, UpstreamID: wallet.UpstreamID, Currency: wallet.Currency}, nil
}

func (r *financePriceLookupRepository) FindWalletAssignmentAt(ctx context.Context, accountID int64, at time.Time) (*service.FinanceWalletAssignment, error) {
	assignment, err := r.client.UpstreamWalletAccount.Query().
		Where(
			upstreamwalletaccount.AccountIDEQ(accountID),
			upstreamwalletaccount.EffectiveFromLTE(at),
			upstreamwalletaccount.Or(
				upstreamwalletaccount.EffectiveToIsNil(),
				upstreamwalletaccount.EffectiveToGT(at),
			),
		).
		Order(dbent.Desc(upstreamwalletaccount.FieldEffectiveFrom), dbent.Desc(upstreamwalletaccount.FieldID)).
		First(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find wallet assignment: %w", err)
	}
	wallet, err := r.client.UpstreamWallet.Get(ctx, assignment.WalletID)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find wallet: %w", err)
	}
	return &service.FinanceWalletAssignment{
		WalletID:   wallet.ID,
		UpstreamID: wallet.UpstreamID,
		Currency:   wallet.Currency,
	}, nil
}

func (r *financePriceLookupRepository) FindUpstreamPriceAt(ctx context.Context, walletID int64, model, billingMode, serviceTier string, at time.Time) (*service.FinancePriceQuote, error) {
	versions, err := r.client.UpstreamModelPriceVersion.Query().
		Where(
			upstreammodelpriceversion.WalletIDEQ(walletID),
			upstreammodelpriceversion.BillingModeEQ(strings.TrimSpace(billingMode)),
			upstreammodelpriceversion.EffectiveFromLTE(at),
			upstreammodelpriceversion.Or(
				upstreammodelpriceversion.EffectiveToIsNil(),
				upstreammodelpriceversion.EffectiveToGT(at),
			),
		).
		Order(dbent.Desc(upstreammodelpriceversion.FieldEffectiveFrom), dbent.Desc(upstreammodelpriceversion.FieldID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("find upstream price versions: %w", err)
	}
	selected := selectHistoricalUpstreamPrice(versions, model, serviceTier)
	if selected == nil {
		return nil, nil
	}
	detail, err := service.FinancePriceDetailFromMap(selected.PriceDetail)
	if err != nil {
		return nil, fmt.Errorf("parse upstream price version %d: %w", selected.ID, err)
	}
	exchangeRate := financeExchangeRate(selected.Currency, selected.PriceDetail, selected.SourceSnapshot)
	fxSource, fxObservedAt := financeFXEvidence(selected.Currency, selected.Source, selected.EffectiveFrom, selected.PriceDetail, selected.SourceSnapshot)
	var fxRateVersionID *int64
	if !exchangeRate.IsPositive() && !strings.EqualFold(strings.TrimSpace(selected.Currency), "USD") {
		fxVersion, fxErr := r.findFXRateAt(ctx, selected.Currency, at)
		if fxErr != nil {
			return nil, fmt.Errorf("find upstream fx rate: %w", fxErr)
		}
		if fxVersion != nil {
			exchangeRate = fxVersion.RateToUsd
			fxRateVersionID = &fxVersion.ID
			fxSource = fxVersion.Source
			fxObservedAt = fxVersion.ObservedAt
		}
	}
	if fxRateVersionID == nil {
		fxRateVersionID, err = r.ensureFXRateVersion(ctx, selected.Currency, exchangeRate, fxSource, fxObservedAt, selected.EffectiveFrom)
		if err != nil {
			return nil, fmt.Errorf("freeze upstream fx rate: %w", err)
		}
	}
	source := service.FinancePricingSourceUpstreamCatalog
	if strings.EqualFold(strings.TrimSpace(selected.Source), service.FinancePricingSourceManual) {
		source = service.FinancePricingSourceManual
	}
	return &service.FinancePriceQuote{
		VersionID:       selected.ID,
		Source:          source,
		BillingMode:     selected.BillingMode,
		Currency:        selected.Currency,
		USDExchangeRate: exchangeRate,
		FXRateVersionID: fxRateVersionID,
		FXSource:        fxSource,
		FXObservedAt:    &fxObservedAt,
		Detail:          detail,
	}, nil
}

func (r *financePriceLookupRepository) findFXRateAt(ctx context.Context, currency string, at time.Time) (*dbent.FinanceFXRateVersion, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" || currency == "USD" {
		return nil, nil
	}
	version, err := r.client.FinanceFXRateVersion.Query().Where(
		financefxrateversion.CurrencyEQ(currency),
		financefxrateversion.EffectiveFromLTE(at),
		financefxrateversion.Or(
			financefxrateversion.EffectiveToIsNil(),
			financefxrateversion.EffectiveToGT(at),
		),
	).Order(dbent.Desc(financefxrateversion.FieldEffectiveFrom), dbent.Desc(financefxrateversion.FieldID)).First(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	return version, err
}

func (r *financePriceLookupRepository) FindFinanceFXRateAt(ctx context.Context, currency string, at time.Time) (*service.FinanceFXRateVersion, error) {
	version, err := r.findFXRateAt(ctx, currency, at)
	if err != nil || version == nil {
		return nil, err
	}
	return &service.FinanceFXRateVersion{
		ID: version.ID, Currency: version.Currency, RateToUSD: version.RateToUsd.String(), Source: version.Source,
		ObservedAt: version.ObservedAt, EffectiveFrom: version.EffectiveFrom, EffectiveTo: cloneRepositoryTime(version.EffectiveTo), Checksum: version.Checksum, CreatedAt: version.CreatedAt,
	}, nil
}

func (r *financePriceLookupRepository) FindSystemPriceAt(ctx context.Context, model, billingMode string, at time.Time) (*service.FinancePriceQuote, error) {
	version, err := r.client.SystemModelPriceVersion.Query().
		Where(
			systemmodelpriceversion.ModelNameEqualFold(strings.TrimSpace(model)),
			systemmodelpriceversion.BillingModeEQ(strings.TrimSpace(billingMode)),
			systemmodelpriceversion.EffectiveFromLTE(at),
			systemmodelpriceversion.Or(
				systemmodelpriceversion.EffectiveToIsNil(),
				systemmodelpriceversion.EffectiveToGT(at),
			),
		).
		Order(dbent.Desc(systemmodelpriceversion.FieldEffectiveFrom), dbent.Desc(systemmodelpriceversion.FieldID)).
		First(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find system price version: %w", err)
	}
	detail, err := service.FinancePriceDetailFromMap(version.PriceDetail)
	if err != nil {
		return nil, fmt.Errorf("parse system price version %d: %w", version.ID, err)
	}
	identityRate := decimal.NewFromInt(1)
	fxRateVersionID, err := r.ensureFXRateVersion(ctx, "USD", identityRate, "currency_identity", version.EffectiveFrom, version.EffectiveFrom)
	if err != nil {
		return nil, fmt.Errorf("freeze system price fx rate: %w", err)
	}
	return &service.FinancePriceQuote{
		VersionID:       version.ID,
		Source:          service.FinancePricingSourceSystem,
		BillingMode:     version.BillingMode,
		Currency:        "USD",
		USDExchangeRate: identityRate,
		FXRateVersionID: fxRateVersionID,
		FXSource:        "currency_identity",
		FXObservedAt:    &version.EffectiveFrom,
		Detail:          detail,
	}, nil
}

func (r *financePriceLookupRepository) ensureFXRateVersion(ctx context.Context, currency string, rate decimal.Decimal, source string, observedAt, effectiveFrom time.Time) (*int64, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = "USD"
	}
	if len(currency) != 3 || rate.LessThanOrEqual(decimal.Zero) {
		return nil, nil
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "price_version_snapshot"
	}
	if observedAt.IsZero() {
		observedAt = effectiveFrom
	}
	if effectiveFrom.IsZero() {
		effectiveFrom = observedAt
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(currency+"|"+rate.String()+"|"+source+"|"+effectiveFrom.UTC().Format(time.RFC3339Nano))))
	existing, err := r.client.FinanceFXRateVersion.Query().Where(
		financefxrateversion.CurrencyEQ(currency),
		financefxrateversion.RateToUsdEQ(rate),
		financefxrateversion.SourceEQ(source),
		financefxrateversion.EffectiveFromEQ(effectiveFrom),
	).First(ctx)
	if err == nil {
		return &existing.ID, nil
	}
	if !dbent.IsNotFound(err) {
		return nil, err
	}
	// FX intervals are exclusive per currency, so an existing version at the
	// same effective timestamp must be reused even when it was recorded by a
	// different source (for example, finance_initialization).
	existing, err = r.client.FinanceFXRateVersion.Query().Where(
		financefxrateversion.CurrencyEQ(currency),
		financefxrateversion.EffectiveFromEQ(effectiveFrom),
	).Order(dbent.Desc(financefxrateversion.FieldID)).First(ctx)
	if err == nil {
		return &existing.ID, nil
	}
	if !dbent.IsNotFound(err) {
		return nil, err
	}
	created, err := r.client.FinanceFXRateVersion.Create().
		SetCurrency(currency).
		SetRateToUsd(rate).
		SetSource(source).
		SetObservedAt(observedAt).
		SetEffectiveFrom(effectiveFrom).
		SetChecksum(checksum).
		Save(ctx)
	if err == nil {
		return &created.ID, nil
	}
	existing, queryErr := r.client.FinanceFXRateVersion.Query().Where(
		financefxrateversion.CurrencyEQ(currency),
		financefxrateversion.RateToUsdEQ(rate),
		financefxrateversion.SourceEQ(source),
		financefxrateversion.EffectiveFromEQ(effectiveFrom),
	).First(ctx)
	if queryErr != nil {
		return nil, err
	}
	return &existing.ID, nil
}

func financeFXEvidence(currency, priceSource string, fallback time.Time, maps ...map[string]any) (string, time.Time) {
	if strings.EqualFold(strings.TrimSpace(currency), "USD") || strings.TrimSpace(currency) == "" {
		return "currency_identity", fallback
	}
	source := strings.TrimSpace(priceSource)
	if source == "" {
		source = "price_version_snapshot"
	}
	observedAt := fallback
	for _, raw := range maps {
		if raw == nil {
			continue
		}
		if value := strings.TrimSpace(fmt.Sprint(raw["fx_source"])); value != "" && value != "<nil>" {
			source = value
		}
		if value := strings.TrimSpace(fmt.Sprint(raw["fx_observed_at"])); value != "" && value != "<nil>" {
			if parsed, err := time.Parse(time.RFC3339, value); err == nil {
				observedAt = parsed
			}
		}
	}
	return source, observedAt
}

func selectHistoricalUpstreamPrice(versions []*dbent.UpstreamModelPriceVersion, model, serviceTier string) *dbent.UpstreamModelPriceVersion {
	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	normalizedTier := strings.ToLower(strings.TrimSpace(serviceTier))
	candidates := make([]*dbent.UpstreamModelPriceVersion, 0, len(versions))
	for _, version := range versions {
		if version == nil || !financeServiceTierMatches(version.ServiceTier, normalizedTier) {
			continue
		}
		pattern := strings.ToLower(strings.TrimSpace(version.ModelPattern))
		if version.IsWildcard {
			prefix := strings.TrimSuffix(pattern, "*")
			if prefix == "" || !strings.HasPrefix(normalizedModel, prefix) {
				continue
			}
		} else if pattern != normalizedModel {
			continue
		}
		candidates = append(candidates, version)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.IsWildcard != right.IsWildcard {
			return !left.IsWildcard
		}
		leftTierExact := left.ServiceTier != nil && strings.EqualFold(strings.TrimSpace(*left.ServiceTier), normalizedTier)
		rightTierExact := right.ServiceTier != nil && strings.EqualFold(strings.TrimSpace(*right.ServiceTier), normalizedTier)
		if leftTierExact != rightTierExact {
			return leftTierExact
		}
		if left.IsWildcard && len(left.ModelPattern) != len(right.ModelPattern) {
			return len(left.ModelPattern) > len(right.ModelPattern)
		}
		if !left.EffectiveFrom.Equal(right.EffectiveFrom) {
			return left.EffectiveFrom.After(right.EffectiveFrom)
		}
		return left.ID > right.ID
	})
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

func financeServiceTierMatches(versionTier *string, requestedTier string) bool {
	if versionTier == nil || strings.TrimSpace(*versionTier) == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(*versionTier), requestedTier)
}

func financeExchangeRate(currency string, maps ...map[string]any) decimal.Decimal {
	if strings.EqualFold(strings.TrimSpace(currency), "USD") || strings.TrimSpace(currency) == "" {
		return decimal.NewFromInt(1)
	}
	for _, raw := range maps {
		if raw == nil {
			continue
		}
		for _, key := range []string{"usd_exchange_rate", "exchange_rate_to_usd", "usd_rate"} {
			value, ok := raw[key]
			if !ok {
				continue
			}
			parsed, err := service.ParseFinanceDecimal(value)
			if err == nil && parsed != nil && parsed.GreaterThan(decimal.Zero) {
				return *parsed
			}
		}
	}
	return decimal.Zero
}
