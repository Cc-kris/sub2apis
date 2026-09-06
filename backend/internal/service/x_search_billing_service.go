package service

import (
	"errors"

	"github.com/shopspring/decimal"
)

// xSearchBillingSnapshot is the immutable per-request price captured before
// x_search is forwarded. It keeps the customer charge and finance source
// independent from token pricing or later settings changes.
type xSearchBillingSnapshot struct {
	price decimal.Decimal
}

func newXSearchBillingSnapshot(price *decimal.Decimal) (*xSearchBillingSnapshot, error) {
	if price == nil {
		return nil, nil
	}
	snapshot := price.Round(10)
	if !snapshot.IsPositive() {
		return nil, errors.New("x_search fixed request price must be positive")
	}
	// Downstream legacy billing fields are float64. Reject values that cannot
	// round-trip through that representation at the contract's 10-decimal
	// scale; silently truncating a large decimal would break three-table
	// reconciliation and could charge a different amount.
	if decimal.NewFromFloat(snapshot.InexactFloat64()).Round(10).Cmp(snapshot) != 0 {
		return nil, errors.New("x_search fixed request price exceeds lossless billing precision")
	}
	return &xSearchBillingSnapshot{price: snapshot}, nil
}

func (s *xSearchBillingSnapshot) cost() *CostBreakdown {
	if s == nil {
		return nil
	}
	return &CostBreakdown{
		TotalCost:   s.price.InexactFloat64(),
		ActualCost:  s.price.InexactFloat64(),
		BillingMode: string(BillingModePerRequest),
	}
}

func (s *xSearchBillingSnapshot) apply(usageLog *UsageLog) {
	if s == nil || usageLog == nil {
		return
	}
	usageLog.BillingMode = stringSnapshot(string(BillingModePerRequest))
	usageLog.TotalCost = s.price.InexactFloat64()
	usageLog.ActualCost = s.price.InexactFloat64()
	usageLog.UsageListValue = cloneDecimal(&s.price)
	usageLog.SalesPricingVersion = stringSnapshot(string(SalesPricingVersionV2))
	usageLog.SalesPricingSource = stringSnapshot("x_search")
	usageLog.SalesPricingEffectiveModel = stringSnapshot("x_search")
	usageLog.SalesPricingSnapshot = map[string]any{
		"requested_model": "x_search",
		"effective_model": "x_search",
		"version":         string(SalesPricingVersionV2),
		"pricing_source":  "x_search",
		"billing_mode":    string(BillingModePerRequest),
		"amounts": map[string]string{
			"original_total":   s.price.StringFixed(10),
			"multiplier_total": s.price.StringFixed(10),
		},
	}
}
