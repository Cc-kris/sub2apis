package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

type financeReportRepository struct{ db *sql.DB }

func NewFinanceReportRepository(db *sql.DB) service.FinanceReportRepository {
	return &financeReportRepository{db: db}
}

func (r *financeReportRepository) SummarizeFinance(ctx context.Context, filter service.FinanceReportFilter) (*service.FinanceSummaryFacts, error) {
	where, args := financeReportWhere(filter, "ufr")
	costStatuses := []string{"exact", "estimated"}
	if filter.DataScope == "exact_only" {
		costStatuses = []string{"exact"}
	}
	args = append(args, pq.Array(costStatuses))
	costStatusArg := len(args)
	query := fmt.Sprintf(`
WITH base AS (
  SELECT ufr.id,
         CASE WHEN ufr.cost_status='excluded' THEN 0 ELSE COALESCE((
           SELECT SUM(ura.allocated_amount)
           FROM usage_revenue_allocations ura
           WHERE ura.usage_log_id=ufr.usage_log_id AND ura.invalidated_at IS NULL
         ), CASE WHEN ufr.business_type='subscription' THEN 0 ELSE ufr.usage_list_value END, 0) END::numeric AS revenue,
         CASE WHEN ufr.cost_status = ANY($%d) THEN ufr.upstream_cost ELSE NULL END::numeric AS included_cost,
         ufr.cost_status,
         CASE WHEN ufr.cost_status='estimated' AND COALESCE(segment_costs.estimated_count,0)=0
              THEN COALESCE(ufr.upstream_cost,0) ELSE COALESCE(segment_costs.estimated_cost,0) END::numeric AS estimated_cost,
         CASE WHEN ufr.cost_status='estimated' AND COALESCE(segment_costs.estimated_count,0)=0
                   AND COALESCE(ufr.calculation_detail->>'finance_cost_mode','') IN ('cumulative_list_and_actual','cumulative_actual')
              THEN COALESCE(ufr.upstream_cost,0) ELSE COALESCE(segment_costs.pending_settlement_cost,0) END::numeric AS pending_settlement_cost,
         CASE WHEN ufr.cost_status <> 'exact' THEN COALESCE(segment_costs.exact_cost,0) ELSE 0 END::numeric AS unconfirmed_exact_cost
  FROM usage_finance_records ufr
  LEFT JOIN LATERAL (
    SELECT COALESCE(SUM(seg.cost_amount) FILTER (WHERE seg.cost_status='estimated'),0) AS estimated_cost,
           COALESCE(SUM(seg.cost_amount) FILTER (
             WHERE seg.cost_status='estimated'
               AND COALESCE(seg.calculation_detail->>'finance_cost_mode','') IN ('cumulative_list_and_actual','cumulative_actual')
           ),0) AS pending_settlement_cost,
           COUNT(*) FILTER (WHERE seg.cost_status='estimated') AS estimated_count,
           COALESCE(SUM(seg.cost_amount) FILTER (WHERE seg.cost_status='exact'),0) AS exact_cost
    FROM usage_finance_cost_segments seg
    WHERE seg.usage_finance_record_id=ufr.id
  ) segment_costs ON TRUE
  WHERE %s
), usage_summary AS (
  SELECT
    COALESCE(SUM(revenue),0)::text AS revenue,
	COALESCE(SUM(revenue) FILTER (WHERE included_cost IS NOT NULL),0)::text AS covered_revenue,
    COALESCE(SUM(included_cost),0)::text AS upstream_cost,
    COALESCE(SUM(CASE WHEN included_cost IS NOT NULL AND included_cost > revenue THEN included_cost-revenue ELSE 0 END),0)::text AS loss_amount,
    COUNT(*) FILTER (WHERE included_cost IS NOT NULL AND included_cost > revenue)::bigint AS loss_request_count,
    COUNT(*)::bigint AS request_count,
    COUNT(*) FILTER (WHERE cost_status='exact')::bigint AS exact_count,
    COUNT(*) FILTER (WHERE cost_status='estimated')::bigint AS estimated_count,
	COUNT(*) FILTER (WHERE cost_status='missing_profile')::bigint AS missing_profile_count,
    COUNT(*) FILTER (WHERE cost_status='missing_price')::bigint AS missing_price_count,
    COUNT(*) FILTER (WHERE cost_status='missing_multiplier')::bigint AS missing_multiplier_count,
    COUNT(*) FILTER (WHERE cost_status='missing_usage')::bigint AS missing_usage_count,
	COUNT(*) FILTER (WHERE cost_status='unsupported_usage')::bigint AS unsupported_usage_count,
    COUNT(*) FILTER (WHERE cost_status='non_billable')::bigint AS non_billable_count,
	COUNT(*) FILTER (WHERE cost_status='excluded')::bigint AS excluded_count,
	COALESCE(SUM(revenue) FILTER (WHERE cost_status IN ('missing_profile','missing_price','missing_multiplier','missing_usage','unsupported_usage')),0)::text AS unpriced_revenue,
	COALESCE(SUM(estimated_cost),0)::text AS estimated_cost,
	COALESCE(SUM(pending_settlement_cost),0)::text AS pending_settlement_cost,
	COALESCE(SUM(unconfirmed_exact_cost),0)::text AS unconfirmed_exact_cost
  FROM base
)
SELECT revenue,covered_revenue,upstream_cost,loss_amount,loss_request_count,request_count,
	   exact_count,estimated_count,missing_profile_count,missing_price_count,missing_multiplier_count,missing_usage_count,
	   unsupported_usage_count,non_billable_count,excluded_count,unpriced_revenue,estimated_cost,pending_settlement_cost,unconfirmed_exact_cost
FROM usage_summary`, costStatusArg, where)
	facts := &service.FinanceSummaryFacts{}
	var revenue, coveredRevenue, cost, loss, unpriced, estimatedCost, pendingSettlementCost, unconfirmedExactCost string
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&revenue, &coveredRevenue, &cost, &loss, &facts.LossRequestCount, &facts.RequestCount,
		&facts.ExactCount, &facts.EstimatedCount, &facts.MissingProfileCount, &facts.MissingPriceCount,
		&facts.MissingMultiplierCount, &facts.MissingUsageCount, &facts.UnsupportedUsageCount,
		&facts.NonBillableCount, &facts.ExcludedCount, &unpriced, &estimatedCost, &pendingSettlementCost, &unconfirmedExactCost,
	)
	if err != nil {
		return nil, fmt.Errorf("summarize finance usage ledger: %w", err)
	}
	if facts.Revenue, err = decimal.NewFromString(revenue); err != nil {
		return nil, err
	}
	if facts.CoveredRevenue, err = decimal.NewFromString(coveredRevenue); err != nil {
		return nil, err
	}
	if facts.UpstreamCost, err = decimal.NewFromString(cost); err != nil {
		return nil, err
	}
	if facts.LossAmount, err = decimal.NewFromString(loss); err != nil {
		return nil, err
	}
	if facts.UnpricedRevenue, err = decimal.NewFromString(unpriced); err != nil {
		return nil, err
	}
	if facts.EstimatedCost, err = decimal.NewFromString(estimatedCost); err != nil {
		return nil, err
	}
	if facts.PendingSettlementCost, err = decimal.NewFromString(pendingSettlementCost); err != nil {
		return nil, err
	}
	if facts.UnconfirmedExactCost, err = decimal.NewFromString(unconfirmedExactCost); err != nil {
		return nil, err
	}
	if err = r.attachUnallocatedSubscriptionRevenue(ctx, filter, facts); err != nil {
		return nil, err
	}
	if err = r.attachFinancePaymentFees(ctx, filter, facts); err != nil {
		return nil, err
	}
	if !filter.SkipCurrentSnapshot {
		if err = r.attachFinanceCashSummary(ctx, filter, facts); err != nil {
			return nil, err
		}
	}
	if err = r.attachRechargeBonusIncome(ctx, filter, facts); err != nil {
		return nil, err
	}
	return facts, nil
}

func (r *financeReportRepository) ListFinanceTrend(ctx context.Context, filter service.FinanceReportFilter) ([]service.FinanceTrendFact, error) {
	where, args := financeReportWhere(filter, "ufr")
	costStatuses := []string{"exact", "estimated"}
	if filter.DataScope == "exact_only" {
		costStatuses = []string{"exact"}
	}
	args = append(args, pq.Array(costStatuses), filter.Granularity, filter.Timezone)
	costArg, granularityArg, timezoneArg := len(args)-2, len(args)-1, len(args)
	query := fmt.Sprintf(`
WITH base AS (
 SELECT (date_trunc($%d, ufr.usage_created_at AT TIME ZONE $%d) AT TIME ZONE $%d) AS bucket_start,
        CASE WHEN ufr.cost_status='excluded' THEN 0 ELSE COALESCE((SELECT SUM(ura.allocated_amount) FROM usage_revenue_allocations ura WHERE ura.usage_log_id=ufr.usage_log_id AND ura.invalidated_at IS NULL),CASE WHEN ufr.business_type='subscription' THEN 0 ELSE ufr.usage_list_value END,0) END::numeric AS revenue,
        CASE WHEN ufr.cost_status=ANY($%d) THEN ufr.upstream_cost ELSE NULL END::numeric AS included_cost,
        ufr.cost_status
 FROM usage_finance_records ufr WHERE %s
)
SELECT bucket_start,
       COALESCE(SUM(revenue),0)::text,
       COALESCE(SUM(revenue) FILTER (WHERE included_cost IS NOT NULL),0)::text,
       COALESCE(SUM(included_cost),0)::text,
       COALESCE(SUM(CASE WHEN included_cost>revenue THEN included_cost-revenue ELSE 0 END),0)::text,
       COUNT(*)::bigint,
       COUNT(*) FILTER (WHERE cost_status='exact')::bigint,
       COUNT(*) FILTER (WHERE cost_status='estimated')::bigint,
	   COUNT(*) FILTER (WHERE cost_status='missing_profile')::bigint,
       COUNT(*) FILTER (WHERE cost_status='missing_price')::bigint,
       COUNT(*) FILTER (WHERE cost_status='missing_multiplier')::bigint,
	   COUNT(*) FILTER (WHERE cost_status='missing_usage')::bigint,
	   COUNT(*) FILTER (WHERE cost_status='unsupported_usage')::bigint,
	   COUNT(*) FILTER (WHERE cost_status='excluded')::bigint
FROM base GROUP BY bucket_start ORDER BY bucket_start`, granularityArg, timezoneArg, timezoneArg, costArg, where)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list finance trend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.FinanceTrendFact, 0)
	for rows.Next() {
		var item service.FinanceTrendFact
		var revenue, covered, cost, loss string
		if err = rows.Scan(&item.BucketStart, &revenue, &covered, &cost, &loss, &item.RequestCount, &item.ExactCount, &item.EstimatedCount, &item.MissingProfile, &item.MissingPrice, &item.MissingMultiplier, &item.MissingUsage, &item.UnsupportedUsage, &item.ExcludedCount); err != nil {
			return nil, err
		}
		if item.Revenue, err = decimal.NewFromString(revenue); err != nil {
			return nil, err
		}
		if item.CoveredRevenue, err = decimal.NewFromString(covered); err != nil {
			return nil, err
		}
		if item.UpstreamCost, err = decimal.NewFromString(cost); err != nil {
			return nil, err
		}
		if item.LossAmount, err = decimal.NewFromString(loss); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = r.attachUnallocatedSubscriptionTrend(ctx, filter, &items); err != nil {
		return nil, err
	}
	if err = r.attachPaymentFeeTrend(ctx, filter, &items); err != nil {
		return nil, err
	}
	if err = r.attachRechargeBonusTrend(ctx, filter, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *financeReportRepository) attachPaymentFeeTrend(ctx context.Context, filter service.FinanceReportFilter, items *[]service.FinanceTrendFact) error {
	if !financeCashApplies(filter) {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT (date_trunc($3, fee.occurred_at AT TIME ZONE $4) AT TIME ZONE $4) AS bucket_start,
       COALESCE(SUM(fee.fee_usd_amount),0)::text
FROM payment_provider_fee_events fee
WHERE fee.occurred_at >= $1 AND fee.occurred_at < $2 AND fee.fee_status='confirmed'
  AND EXISTS (
    SELECT 1 FROM payment_orders po
    WHERE po.id=fee.payment_order_id
      AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$5)
  )
GROUP BY bucket_start`, filter.StartAt, filter.EndBefore, filter.Granularity, filter.Timezone, service.RoleAdmin)
	if err != nil {
		return fmt.Errorf("list payment fee trend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byBucket := make(map[int64]int, len(*items))
	for index, item := range *items {
		byBucket[item.BucketStart.Unix()] = index
	}
	for rows.Next() {
		var bucket time.Time
		var raw string
		if err = rows.Scan(&bucket, &raw); err != nil {
			return err
		}
		amount, parseErr := decimal.NewFromString(raw)
		if parseErr != nil {
			return parseErr
		}
		key := bucket.UTC().Unix()
		if index, ok := byBucket[key]; ok {
			(*items)[index].PaymentFees = amount
			continue
		}
		*items = append(*items, service.FinanceTrendFact{BucketStart: bucket.UTC(), PaymentFees: amount})
		byBucket[key] = len(*items) - 1
	}
	return rows.Err()
}

func (r *financeReportRepository) ListFinanceBreakdown(ctx context.Context, filter service.FinanceReportFilter, request service.FinanceBreakdownRequest) ([]service.FinanceBreakdownFact, int64, error) {
	dimension, ok := financeBreakdownDimensions[request.Dimension]
	if !ok {
		return nil, 0, errors.New("unsupported finance breakdown dimension")
	}
	sortColumn, ok := financeBreakdownSortColumns[request.SortBy]
	if !ok {
		return nil, 0, errors.New("unsupported finance breakdown sort")
	}
	sortOrder := strings.ToUpper(request.SortOrder)
	if sortOrder != "ASC" && sortOrder != "DESC" {
		return nil, 0, errors.New("unsupported finance breakdown sort order")
	}
	where, args := financeReportWhere(filter, "ufr")
	costStatuses := []string{"exact", "estimated"}
	if filter.DataScope == "exact_only" {
		costStatuses = []string{"exact"}
	}
	args = append(args, pq.Array(costStatuses))
	costArg := len(args)
	args = append(args, request.PageSize, (request.Page-1)*request.PageSize)
	query := fmt.Sprintf(`
WITH base AS (
 SELECT %s AS dimension_key,%s AS dimension_name,
        CASE WHEN ufr.cost_status='excluded' THEN 0 ELSE COALESCE((SELECT SUM(ura.allocated_amount) FROM usage_revenue_allocations ura WHERE ura.usage_log_id=ufr.usage_log_id AND ura.invalidated_at IS NULL),CASE WHEN ufr.business_type='subscription' THEN 0 ELSE ufr.usage_list_value END,0) END::numeric AS revenue,
        CASE WHEN ufr.cost_status=ANY($%d) THEN ufr.upstream_cost ELSE NULL END::numeric AS included_cost,
        ufr.cost_status,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.input_cost,0) ELSE 0 END AS input_cost,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.output_cost,0) ELSE 0 END AS output_cost,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.cache_cost,0) ELSE 0 END AS cache_cost,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.fast_cost,0) ELSE 0 END AS fast_cost,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.image_cost,0) ELSE 0 END AS image_cost,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.video_cost,0) ELSE 0 END AS video_cost,
        CASE WHEN ufr.cost_status=ANY($%d) THEN COALESCE(components.other_cost,0) ELSE 0 END AS other_cost
 FROM usage_finance_records ufr %s
 LEFT JOIN LATERAL (
   SELECT
     COALESCE(SUM(amount) FILTER (WHERE NOT is_fast AND item_name='input'),0) AS input_cost,
     COALESCE(SUM(amount) FILTER (WHERE NOT is_fast AND item_name='output'),0) AS output_cost,
     COALESCE(SUM(amount) FILTER (WHERE NOT is_fast AND item_name IN ('cache_read','cache_write_5m','cache_write_1h')),0) AS cache_cost,
     COALESCE(SUM(amount) FILTER (WHERE is_fast),0) AS fast_cost,
     COALESCE(SUM(amount) FILTER (WHERE NOT is_fast AND item_name IN ('image_output','image','per_image','image_fallback')),0) AS image_cost,
     COALESCE(SUM(amount) FILTER (WHERE NOT is_fast AND item_name IN ('video_second','per_second','video_fallback')),0) AS video_cost,
     COALESCE(SUM(amount) FILTER (WHERE NOT is_fast AND item_name NOT IN ('input','output','cache_read','cache_write_5m','cache_write_1h','image_output','image','per_image','image_fallback','video_second','per_second','video_fallback')),0) AS other_cost
   FROM (
     SELECT LOWER(COALESCE(seg.service_tier,'')) IN ('fast','priority') AS is_fast,
            item->>'item' AS item_name,
            COALESCE(NULLIF(item->>'amount','')::numeric,0) AS amount
     FROM usage_finance_cost_segments seg
     CROSS JOIN LATERAL jsonb_array_elements(COALESCE(seg.calculation_detail->'items','[]'::jsonb)) item
     WHERE seg.usage_finance_record_id=ufr.id
     UNION ALL
     SELECT LOWER(COALESCE(seg.service_tier,'')) IN ('fast','priority') AS is_fast,
            CASE WHEN ufr.billing_type='image' THEN 'image_fallback'
                 WHEN ufr.billing_type='per_second' THEN 'video_fallback'
                 ELSE 'other_fallback' END AS item_name,
            COALESCE(seg.cost_amount,0) AS amount
     FROM usage_finance_cost_segments seg
     WHERE seg.usage_finance_record_id=ufr.id
       AND jsonb_array_length(COALESCE(seg.calculation_detail->'items','[]'::jsonb))=0
   ) component_items
 ) components ON TRUE
 WHERE %s
), grouped AS (
 SELECT dimension_key,dimension_name,
        COALESCE(SUM(revenue),0) AS revenue,
        COALESCE(SUM(revenue) FILTER (WHERE included_cost IS NOT NULL),0) AS covered_revenue,
        COALESCE(SUM(included_cost),0) AS upstream_cost,
        COALESCE(SUM(CASE WHEN included_cost>revenue THEN included_cost-revenue ELSE 0 END),0) AS loss_amount,
        COUNT(*)::bigint AS request_count,
        COUNT(*) FILTER (WHERE cost_status='exact')::bigint AS exact_count,
        COUNT(*) FILTER (WHERE cost_status='estimated')::bigint AS estimated_count,
		COUNT(*) FILTER (WHERE cost_status IN ('missing_profile','missing_price','missing_multiplier','missing_usage','unsupported_usage'))::bigint AS missing_count,
        COALESCE(SUM(input_cost),0) AS input_cost,COALESCE(SUM(output_cost),0) AS output_cost,
        COALESCE(SUM(cache_cost),0) AS cache_cost,COALESCE(SUM(fast_cost),0) AS fast_cost,
        COALESCE(SUM(image_cost),0) AS image_cost,COALESCE(SUM(video_cost),0) AS video_cost,
        COALESCE(SUM(other_cost),0) AS other_cost
 FROM base GROUP BY dimension_key,dimension_name
)
SELECT dimension_key,dimension_name,revenue::text,covered_revenue::text,upstream_cost::text,loss_amount::text,
       request_count,exact_count,estimated_count,missing_count,
       input_cost::text,output_cost::text,cache_cost::text,fast_cost::text,image_cost::text,video_cost::text,other_cost::text,
       COUNT(*) OVER()::bigint,
       (covered_revenue-upstream_cost) AS profit,
       CASE WHEN covered_revenue<>0 THEN (covered_revenue-upstream_cost)/covered_revenue ELSE NULL END AS margin_rate
FROM grouped ORDER BY %s %s NULLS LAST,dimension_key ASC LIMIT $%d OFFSET $%d`,
		dimension.Key, dimension.Name, costArg, costArg, costArg, costArg, costArg, costArg, costArg, costArg,
		dimension.Joins, where, sortColumn, sortOrder, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list finance breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.FinanceBreakdownFact, 0)
	var total int64
	for rows.Next() {
		var item service.FinanceBreakdownFact
		var revenue, covered, cost, loss string
		var inputCost, outputCost, cacheCost, fastCost, imageCost, videoCost, otherCost string
		var ignoredProfit decimal.Decimal
		var ignoredMargin *decimal.Decimal
		if err = rows.Scan(&item.DimensionKey, &item.DimensionName, &revenue, &covered, &cost, &loss,
			&item.RequestCount, &item.ExactCount, &item.EstimatedCount, &item.MissingCount,
			&inputCost, &outputCost, &cacheCost, &fastCost, &imageCost, &videoCost, &otherCost,
			&total, &ignoredProfit, &ignoredMargin); err != nil {
			return nil, 0, err
		}
		if item.Revenue, err = decimal.NewFromString(revenue); err != nil {
			return nil, 0, err
		}
		if item.CoveredRevenue, err = decimal.NewFromString(covered); err != nil {
			return nil, 0, err
		}
		if item.UpstreamCost, err = decimal.NewFromString(cost); err != nil {
			return nil, 0, err
		}
		if item.LossAmount, err = decimal.NewFromString(loss); err != nil {
			return nil, 0, err
		}
		for _, component := range []struct {
			raw    string
			target *decimal.Decimal
		}{
			{inputCost, &item.InputCost}, {outputCost, &item.OutputCost}, {cacheCost, &item.CacheCost},
			{fastCost, &item.FastCost}, {imageCost, &item.ImageCost}, {videoCost, &item.VideoCost}, {otherCost, &item.OtherCost},
		} {
			if *component.target, err = decimal.NewFromString(component.raw); err != nil {
				return nil, 0, err
			}
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (r *financeReportRepository) ListFinanceDetails(ctx context.Context, filter service.FinanceReportFilter, request service.FinanceDetailsRequest) ([]service.FinanceDetailFact, int64, error) {
	where, args := financeReportWhere(filter, "ufr")
	if request.RequestID != "" {
		args = append(args, request.RequestID)
		where += fmt.Sprintf(" AND ul.request_id=$%d", len(args))
	}
	costStatuses := []string{"exact", "estimated"}
	if filter.DataScope == "exact_only" {
		costStatuses = []string{"exact"}
	}
	args = append(args, pq.Array(costStatuses))
	costArg := len(args)
	profitCondition := "TRUE"
	switch request.ProfitDirection {
	case "profit":
		profitCondition = "included_cost IS NOT NULL AND revenue > included_cost"
	case "loss":
		profitCondition = "included_cost IS NOT NULL AND revenue < included_cost"
	case "zero":
		profitCondition = "included_cost IS NOT NULL AND revenue = included_cost"
	case "unknown":
		profitCondition = "included_cost IS NULL"
	}
	sortColumn := map[string]string{
		"usage_created_at": "usage_created_at", "revenue": "revenue", "upstream_cost": "included_cost",
		"profit": "profit", "margin_rate": "margin_rate",
	}[request.SortBy]
	if sortColumn == "" {
		return nil, 0, errors.New("unsupported finance detail sort")
	}
	sortOrder := strings.ToUpper(request.SortOrder)
	if sortOrder != "ASC" && sortOrder != "DESC" {
		return nil, 0, errors.New("unsupported finance detail sort order")
	}
	lossReasonCondition := "TRUE"
	if request.LossReason != "" {
		args = append(args, request.LossReason)
		lossReasonCondition = fmt.Sprintf("loss_reason=$%d", len(args))
	}
	alertStatusCondition := "TRUE"
	if request.LossStatus != "" {
		if request.LossStatus == "untracked" {
			alertStatusCondition = "alert_id IS NULL"
		} else {
			args = append(args, request.LossStatus)
			alertStatusCondition = fmt.Sprintf("alert_status=$%d", len(args))
		}
	}
	args = append(args, request.PageSize, (request.Page-1)*request.PageSize)
	query := fmt.Sprintf(`
WITH detail_base AS (
 SELECT ufr.usage_log_id,ul.request_id,ufr.usage_created_at,
        ufr.user_id,COALESCE(NULLIF(u.email,''),NULLIF(u.username,''),ufr.user_id::text) AS user_name,
		ufr.group_id,COALESCE(g.name,'') AS group_name,ufr.channel_id,COALESCE(c.name,'') AS channel_name,
		ufr.account_id,COALESCE(a.name,'') AS account_name,ufr.wallet_id,COALESCE(w.name,'') AS wallet_name,
		ufr.upstream_id,COALESCE(up.name,'') AS upstream_name,ufr.requested_model,COALESCE(ufr.upstream_model,'') AS upstream_model,
		COALESCE(ufr.service_tier,'') AS service_tier,COALESCE(ufr.pricing_source,'') AS pricing_source,COALESCE(ul.sales_pricing_version,'') AS sales_pricing_version,
        CASE WHEN ufr.cost_status='excluded' THEN 0 ELSE COALESCE((SELECT SUM(ura.allocated_amount) FROM usage_revenue_allocations ura WHERE ura.usage_log_id=ufr.usage_log_id AND ura.invalidated_at IS NULL),CASE WHEN ufr.business_type='subscription' THEN 0 ELSE ufr.usage_list_value END,0) END::numeric AS revenue,
        CASE WHEN ufr.cost_status=ANY($%d) THEN ufr.upstream_cost ELSE NULL END::numeric AS included_cost,
        ufr.cost_status,
		COALESCE(seg.segment_count,0)::bigint AS segment_count,COALESCE(seg.cache_tokens,0)::bigint AS cache_tokens,
		loss_alert.id AS alert_id,COALESCE(loss_alert.status,'') AS alert_status,loss_alert.assignee_id,
		loss_alert.handled_by,COALESCE(loss_alert.handled_note,'') AS handled_note,loss_alert.handled_at
 FROM usage_finance_records ufr
 JOIN usage_logs ul ON ul.id=ufr.usage_log_id
 LEFT JOIN users u ON u.id=ufr.user_id
 LEFT JOIN groups g ON g.id=ufr.group_id
 LEFT JOIN channels c ON c.id=ufr.channel_id
 LEFT JOIN accounts a ON a.id=ufr.account_id
 LEFT JOIN upstream_wallets w ON w.id=ufr.wallet_id
 LEFT JOIN upstreams up ON up.id=ufr.upstream_id
 LEFT JOIN LATERAL (
   SELECT COUNT(*)::bigint AS segment_count,
          COALESCE(SUM(
			COALESCE((usage_detail->>'cache_read_tokens')::bigint,0) +
			CASE
				WHEN COALESCE((usage_detail->>'cache_creation_5m_tokens')::bigint,0) > 0
				  OR COALESCE((usage_detail->>'cache_creation_1h_tokens')::bigint,0) > 0
				THEN COALESCE((usage_detail->>'cache_creation_5m_tokens')::bigint,0)+COALESCE((usage_detail->>'cache_creation_1h_tokens')::bigint,0)
				ELSE COALESCE((usage_detail->>'cache_creation_tokens')::bigint,0)
			END
		  ),0)::bigint AS cache_tokens
   FROM usage_finance_cost_segments WHERE usage_finance_record_id=ufr.id
 ) seg ON TRUE
 LEFT JOIN LATERAL (
   SELECT fa.id,fa.status,fa.assignee_id,fa.handled_by,fa.handled_note,fa.handled_at
   FROM finance_alerts fa
   WHERE fa.alert_type='negative_profit' AND fa.dimension_type='usage_log' AND fa.dimension_id=ufr.usage_log_id
   ORDER BY fa.last_occurred_at DESC,fa.id DESC LIMIT 1
 ) loss_alert ON TRUE
 WHERE %s
), filtered AS (
 SELECT *,CASE WHEN included_cost IS NOT NULL THEN revenue-included_cost ELSE NULL END AS profit,
          CASE WHEN included_cost IS NOT NULL AND revenue<>0 THEN (revenue-included_cost)/revenue ELSE NULL END AS margin_rate
 FROM detail_base WHERE %s
), classified AS (
 SELECT *,CASE
   WHEN segment_count>1 THEN 'multi_attempt_cost'
   WHEN LOWER(service_tier) IN ('fast','priority') THEN 'fast_cost_not_covered'
   WHEN cache_tokens>0 THEN 'cache_cost_not_covered'
   WHEN pricing_source='channel' THEN 'channel_price_mismatch'
   WHEN pricing_source='estimated_system' THEN 'other'
   WHEN revenue>0 THEN 'sales_multiplier_too_low'
   ELSE 'other' END AS loss_reason
 FROM filtered
)
SELECT usage_log_id,request_id,usage_created_at,user_id,user_name,
		group_id,group_name,channel_id,channel_name,account_id,account_name,wallet_id,wallet_name,upstream_id,upstream_name,
       requested_model,upstream_model,service_tier,pricing_source,sales_pricing_version,
       revenue::text,included_cost::text,cost_status,segment_count,cache_tokens,
	   alert_id,alert_status,assignee_id,handled_by,handled_note,handled_at,COUNT(*) OVER()::bigint
FROM classified WHERE %s AND %s ORDER BY %s %s NULLS LAST,usage_created_at DESC,usage_log_id DESC LIMIT $%d OFFSET $%d`,
		costArg, where, profitCondition, lossReasonCondition, alertStatusCondition, sortColumn, sortOrder, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list finance details: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.FinanceDetailFact, 0)
	var total int64
	for rows.Next() {
		var item service.FinanceDetailFact
		var groupID, channelID, accountID, walletID, upstreamID sql.NullInt64
		var alertID, assigneeID, handledBy sql.NullInt64
		var handledAt sql.NullTime
		var revenue string
		var cost sql.NullString
		if err = rows.Scan(
			&item.UsageLogID, &item.RequestID, &item.UsageCreatedAt, &item.UserID, &item.UserName,
			&groupID, &item.GroupName, &channelID, &item.ChannelName, &accountID, &item.AccountName,
			&walletID, &item.WalletName, &upstreamID, &item.UpstreamName,
			&item.RequestedModel, &item.UpstreamModel, &item.ServiceTier, &item.PricingSource, &item.SalesVersion,
			&revenue, &cost, &item.CostStatus, &item.SegmentCount, &item.CacheTokenCount,
			&alertID, &item.AlertStatus, &assigneeID, &handledBy, &item.HandledNote, &handledAt, &total,
		); err != nil {
			return nil, 0, err
		}
		item.GroupID = nullableInt64Pointer(groupID)
		item.ChannelID = nullableInt64Pointer(channelID)
		item.AccountID = nullableInt64Pointer(accountID)
		item.WalletID = nullableInt64Pointer(walletID)
		item.UpstreamID = nullableInt64Pointer(upstreamID)
		item.AlertID = nullableInt64Pointer(alertID)
		item.AssigneeID = nullableInt64Pointer(assigneeID)
		item.HandledBy = nullableInt64Pointer(handledBy)
		if handledAt.Valid {
			value := handledAt.Time
			item.HandledAt = &value
		}
		if item.Revenue, err = decimal.NewFromString(revenue); err != nil {
			return nil, 0, err
		}
		if cost.Valid {
			parsed, parseErr := decimal.NewFromString(cost.String)
			if parseErr != nil {
				return nil, 0, parseErr
			}
			item.UpstreamCost = &parsed
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (r *financeReportRepository) GetFinanceFunds(ctx context.Context, filter service.FinanceReportFilter) (*service.FinanceFundsFacts, error) {
	facts := &service.FinanceFundsFacts{}
	if !financeCashApplies(filter) {
		return facts, nil
	}
	rows, err := r.db.QueryContext(ctx, `
WITH latest AS (
 SELECT DISTINCT ON (s.wallet_id) s.wallet_id,s.balance_amount,s.currency,s.collected_at,s.sync_status,
        COALESCE(NULLIF(w.balance_scope_key,''),'wallet:'||s.wallet_id::text) AS balance_scope_key,
        w.name
 FROM upstream_balance_snapshots s JOIN upstream_wallets w ON w.id=s.wallet_id
 WHERE s.balance_kind='wallet_cash'
 ORDER BY s.wallet_id,s.collected_at DESC,s.id DESC
), ranked AS (
 SELECT latest.*,ROW_NUMBER() OVER(PARTITION BY balance_scope_key ORDER BY collected_at DESC,wallet_id) AS scope_rank
 FROM latest
), wallet_scopes AS (
 SELECT id AS wallet_id,COALESCE(NULLIF(balance_scope_key,''),'wallet:'||id::text) AS balance_scope_key
 FROM upstream_wallets WHERE deleted_at IS NULL
), costs AS (
 SELECT ws.balance_scope_key,COALESCE(SUM(ufr.upstream_cost),0) AS seven_day_cost
 FROM usage_finance_records ufr JOIN wallet_scopes ws ON ws.wallet_id=ufr.wallet_id
 WHERE ufr.usage_created_at >= NOW()-INTERVAL '7 days' AND ufr.cost_status='exact'
   AND COALESCE(ufr.business_type,'') <> $1
   AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=ufr.user_id AND finance_admin.role=$1)
 GROUP BY ws.balance_scope_key
)
SELECT l.wallet_id,l.name,l.balance_amount::text,l.currency,l.collected_at,l.sync_status,COALESCE(c.seven_day_cost,0)::text,
       l.balance_scope_key,
       (l.scope_rank=1 AND l.sync_status='success' AND l.collected_at>=NOW()-INTERVAL '20 minutes') AS included_in_total,
       (l.collected_at<NOW()-INTERVAL '20 minutes') AS stale
FROM ranked l LEFT JOIN costs c ON c.balance_scope_key=l.balance_scope_key
ORDER BY l.name,l.wallet_id`, service.RoleAdmin)
	if err != nil {
		return nil, fmt.Errorf("list wallet cash funds: %w", err)
	}
	for rows.Next() {
		var item service.FinanceWalletCashFact
		var balance, cost string
		if err = rows.Scan(&item.WalletID, &item.WalletName, &balance, &item.Currency, &item.CollectedAt, &item.SyncStatus, &cost, &item.BalanceScopeKey, &item.IncludedInTotal, &item.Stale); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if item.Balance, err = decimal.NewFromString(balance); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if item.SevenDayCost, err = decimal.NewFromString(cost); err != nil {
			_ = rows.Close()
			return nil, err
		}
		facts.WalletCash = append(facts.WalletCash, item)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var payment, refund, fees, topup, upstreamRefund, adjustment, rechargeBonus string
	err = r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(pay_amount) FILTER (WHERE paid_at >= $1 AND paid_at < $2),0)::text,
       COALESCE(SUM(refund_amount) FILTER (WHERE refund_amount>0 AND refund_at >= $1 AND refund_at < $2),0)::text
FROM payment_orders po
WHERE NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3)`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&payment, &refund)
	if err != nil {
		return nil, err
	}
	err = r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(fee.fee_usd_amount),0)::text FROM payment_provider_fee_events fee WHERE fee.occurred_at >= $1 AND fee.occurred_at < $2 AND fee.fee_status='confirmed' AND EXISTS (SELECT 1 FROM payment_orders po WHERE po.id=fee.payment_order_id AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3))`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&fees)
	if err != nil {
		return nil, err
	}
	err = r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(usd_amount) FILTER (WHERE event_type='topup'),0)::text,
       COALESCE(SUM(usd_amount) FILTER (WHERE event_type='refund'),0)::text,
       COALESCE(SUM(usd_amount) FILTER (WHERE event_type='adjustment'),0)::text,
       COALESCE(SUM(bonus_income_usd) FILTER (WHERE bonus_status IN ('confirmed','reversed')),0)::text,
       COUNT(*) FILTER (WHERE event_type='topup')::bigint,
       COUNT(*) FILTER (WHERE event_type <> 'opening_balance')::bigint
FROM upstream_fund_events WHERE occurred_at >= $1 AND occurred_at < $2`, filter.StartAt, filter.EndBefore).Scan(&topup, &upstreamRefund, &adjustment, &rechargeBonus, &facts.UpstreamTopupCount, &facts.UpstreamEventCount)
	if err != nil {
		return nil, err
	}
	for raw, target := range map[string]*decimal.Decimal{
		payment: &facts.CustomerPayment, refund: &facts.CustomerRefund, fees: &facts.PaymentFees,
		topup: &facts.UpstreamTopup, upstreamRefund: &facts.UpstreamRefund, adjustment: &facts.UpstreamAdjust, rechargeBonus: &facts.RechargeBonusIncome,
	} {
		if *target, err = decimal.NewFromString(raw); err != nil {
			return nil, err
		}
	}
	var customerBalance string
	if err = r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(balance),0)::text FROM users WHERE role<>$1 AND deleted_at IS NULL`, service.RoleAdmin).Scan(&customerBalance); err != nil {
		return nil, err
	}
	if facts.CustomerBalance, err = decimal.NewFromString(customerBalance); err != nil {
		return nil, err
	}
	if err = r.db.QueryRowContext(ctx, `
WITH latest AS (SELECT DISTINCT ON (wallet_id) wallet_id,collected_at FROM upstream_balance_snapshots WHERE balance_kind='wallet_cash' ORDER BY wallet_id,collected_at DESC,id DESC)
SELECT COUNT(*)::bigint FROM latest WHERE collected_at < NOW()-INTERVAL '20 minutes'`).Scan(&facts.StaleWalletCount); err != nil {
		return nil, err
	}
	if err = r.db.QueryRowContext(ctx, `SELECT COUNT(*)::bigint FROM upstream_wallets WHERE deleted_at IS NULL AND (pricing_sync_status='failed' OR balance_sync_status='failed' OR quota_sync_status='failed')`).Scan(&facts.FailedSyncCount); err != nil {
		return nil, err
	}
	return facts, nil
}

func (r *financeReportRepository) ListFinanceQualityIssues(ctx context.Context, filter service.FinanceReportFilter, issueType string, page, pageSize int) ([]service.FinanceQualityIssueFact, int64, error) {
	where, args := financeReportWhere(filter, "ufr")
	where += " AND ufr.cost_status IN ('missing_profile','missing_price','missing_multiplier','missing_usage','unsupported_usage')"
	if issueType != "" {
		args = append(args, issueType)
		where += fmt.Sprintf(" AND ufr.cost_status=$%d", len(args))
	}
	args = append(args, pageSize, (page-1)*pageSize)
	query := fmt.Sprintf(`
SELECT ufr.usage_log_id,ufr.cost_status,
       CASE WHEN ufr.cost_status IN ('missing_profile','missing_multiplier') THEN 'account' WHEN ufr.cost_status='missing_price' THEN 'wallet' ELSE 'usage' END,
	   CASE WHEN ufr.cost_status IN ('missing_profile','missing_multiplier') THEN ufr.account_id WHEN ufr.cost_status='missing_price' THEN ufr.wallet_id ELSE ufr.usage_log_id END,
       (CASE WHEN ufr.cost_status='excluded' THEN 0 ELSE COALESCE((SELECT SUM(ura.allocated_amount) FROM usage_revenue_allocations ura WHERE ura.usage_log_id=ufr.usage_log_id AND ura.invalidated_at IS NULL),CASE WHEN ufr.business_type='subscription' THEN 0 ELSE ufr.usage_list_value END,0) END)::text,
       ufr.created_at,ufr.calculated_at,COUNT(*) OVER()::bigint
FROM usage_finance_records ufr WHERE %s
ORDER BY ufr.usage_created_at DESC,ufr.id DESC LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.FinanceQualityIssueFact, 0)
	var total int64
	for rows.Next() {
		var item service.FinanceQualityIssueFact
		var relatedID sql.NullInt64
		var exposed string
		if err = rows.Scan(&item.UsageLogID, &item.IssueType, &item.RelatedType, &relatedID, &exposed, &item.FirstDetectedAt, &item.LastScannedAt, &total); err != nil {
			return nil, 0, err
		}
		item.RelatedID = nullableInt64Pointer(relatedID)
		if item.ExposedRevenue, err = decimal.NewFromString(exposed); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (r *financeReportRepository) GetFinanceCashFlow(ctx context.Context, filter service.FinanceReportFilter, request service.FinanceCashFlowRequest) (*service.FinanceCashFlowFacts, error) {
	facts := &service.FinanceCashFlowFacts{}
	var payments, refunds, surcharge, fees, topups, upstreamRefunds, adjustments string
	err := r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(pay_amount) FILTER (WHERE paid_at >= $1 AND paid_at < $2),0)::text,
       COALESCE(SUM(refund_amount) FILTER (WHERE refund_amount>0 AND refund_at >= $1 AND refund_at < $2),0)::text,
       COALESCE(SUM(GREATEST(pay_amount-amount,0)) FILTER (WHERE paid_at >= $1 AND paid_at < $2),0)::text
FROM payment_orders po
WHERE NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3)`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&payments, &refunds, &surcharge)
	if err != nil {
		return nil, err
	}
	err = r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(fee.fee_usd_amount),0)::text FROM payment_provider_fee_events fee WHERE fee.occurred_at >= $1 AND fee.occurred_at < $2 AND fee.fee_status='confirmed' AND EXISTS (SELECT 1 FROM payment_orders po WHERE po.id=fee.payment_order_id AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3))`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&fees)
	if err != nil {
		return nil, err
	}
	err = r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(usd_amount) FILTER (WHERE event_type='topup'),0)::text,
       COALESCE(SUM(usd_amount) FILTER (WHERE event_type='refund'),0)::text,
       COALESCE(SUM(usd_amount) FILTER (WHERE event_type='adjustment'),0)::text
FROM upstream_fund_events WHERE occurred_at >= $1 AND occurred_at < $2`, filter.StartAt, filter.EndBefore).Scan(&topups, &upstreamRefunds, &adjustments)
	if err != nil {
		return nil, err
	}
	for raw, target := range map[string]*decimal.Decimal{
		payments: &facts.CustomerPayments, refunds: &facts.CustomerRefunds, surcharge: &facts.PaymentSurcharges,
		fees: &facts.PaymentFees, topups: &facts.UpstreamTopups, upstreamRefunds: &facts.UpstreamRefunds, adjustments: &facts.UpstreamAdjustments,
	} {
		if *target, err = decimal.NewFromString(raw); err != nil {
			return nil, err
		}
	}
	rows, err := r.db.QueryContext(ctx, `
WITH events AS (
 SELECT 'customer_payment'::text event_type,'payment_order'::text source_type,id source_id,pay_amount::text original_amount,'USD'::text currency,'1'::text fx_rate_to_usd,pay_amount::text usd_amount,paid_at occurred_at,out_trade_no reference_no
 FROM payment_orders po WHERE paid_at >= $1 AND paid_at < $2
   AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$7)
 UNION ALL
 SELECT 'customer_refund','payment_order',id,refund_amount::text,'USD','1',refund_amount::text,refund_at,out_trade_no
 FROM payment_orders po WHERE refund_amount>0 AND refund_at >= $1 AND refund_at < $2
   AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$7)
 UNION ALL
	SELECT 'payment_surcharge','payment_order',id,GREATEST(pay_amount-amount,0)::text,'USD','1',GREATEST(pay_amount-amount,0)::text,paid_at,out_trade_no
	FROM payment_orders po WHERE pay_amount>amount AND paid_at >= $1 AND paid_at < $2
	  AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$7)
	UNION ALL
	SELECT 'payment_fee','payment_fee_event',id,COALESCE(fee_amount,0)::text,currency,COALESCE(fx_rate_to_usd,0)::text,COALESCE(fee_usd_amount,0)::text,occurred_at,bill_event_id
	FROM payment_provider_fee_events fee WHERE fee_status='confirmed' AND occurred_at >= $1 AND occurred_at < $2
	  AND EXISTS (SELECT 1 FROM payment_orders po WHERE po.id=fee.payment_order_id AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$7))
 UNION ALL
	SELECT CASE WHEN event_type='topup' THEN 'upstream_topup' WHEN event_type='refund' THEN 'upstream_refund' ELSE 'upstream_adjustment' END,
	       'upstream_fund_event',id,original_amount::text,currency,fx_rate_to_usd::text,usd_amount::text,occurred_at,COALESCE(reference_no,'')
 FROM upstream_fund_events WHERE event_type <> 'opening_balance' AND occurred_at >= $1 AND occurred_at < $2
)
SELECT event_type,source_type,source_id,original_amount,currency,fx_rate_to_usd,usd_amount,occurred_at,reference_no,COUNT(*) OVER()::bigint
FROM events
WHERE ($3='' OR event_type=$3) AND ($4='' OR currency=$4)
ORDER BY occurred_at DESC,source_type,source_id DESC LIMIT $5 OFFSET $6`,
		filter.StartAt, filter.EndBefore, request.EventType, request.Currency, request.PageSize, (request.Page-1)*request.PageSize, service.RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item service.FinanceCashFlowItem
		if err = rows.Scan(&item.EventType, &item.SourceType, &item.SourceID, &item.OriginalAmount, &item.Currency, &item.FXRateToUSD, &item.USDAmount, &item.OccurredAt, &item.ReferenceNo, &facts.Total); err != nil {
			return nil, err
		}
		facts.Items = append(facts.Items, item)
	}
	return facts, rows.Err()
}

func nullableInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

type financeBreakdownDimension struct{ Key, Name, Joins string }

var financeBreakdownDimensions = map[string]financeBreakdownDimension{
	"user":            {"ufr.user_id::text", "COALESCE(NULLIF(u.email,''),NULLIF(u.username,''),ufr.user_id::text)", "LEFT JOIN users u ON u.id=ufr.user_id"},
	"group":           {"COALESCE(ufr.group_id::text,'unknown')", "COALESCE(g.name,'Unknown')", "LEFT JOIN groups g ON g.id=ufr.group_id"},
	"channel":         {"COALESCE(ufr.channel_id::text,'unknown')", "COALESCE(c.name,'Unknown')", "LEFT JOIN channels c ON c.id=ufr.channel_id"},
	"upstream":        {"COALESCE(ufr.upstream_id::text,'unknown')", "COALESCE(up.name,'Unknown')", "LEFT JOIN upstreams up ON up.id=ufr.upstream_id"},
	"wallet":          {"COALESCE(ufr.wallet_id::text,'unknown')", "COALESCE(w.name,'Unknown')", "LEFT JOIN upstream_wallets w ON w.id=ufr.wallet_id"},
	"account":         {"COALESCE(ufr.account_id::text,'unknown')", "COALESCE(a.name,'Unknown')", "LEFT JOIN accounts a ON a.id=ufr.account_id"},
	"requested_model": {"COALESCE(NULLIF(ufr.requested_model,''),'unknown')", "COALESCE(NULLIF(ufr.requested_model,''),'Unknown')", ""},
	"upstream_model":  {"COALESCE(NULLIF(ufr.upstream_model,''),'unknown')", "COALESCE(NULLIF(ufr.upstream_model,''),'Unknown')", ""},
	"billing_type":    {"COALESCE(NULLIF(ufr.billing_type,''),'unknown')", "COALESCE(NULLIF(ufr.billing_type,''),'Unknown')", ""},
	"business_type":   {"COALESCE(NULLIF(ufr.business_type,''),'unknown')", "COALESCE(NULLIF(ufr.business_type,''),'Unknown')", ""},
}

var financeBreakdownSortColumns = map[string]string{
	"revenue": "revenue", "upstream_cost": "upstream_cost", "profit": "profit", "loss_amount": "loss_amount",
	"margin_rate": "margin_rate", "request_count": "request_count",
}

func (r *financeReportRepository) attachFinanceCashSummary(ctx context.Context, filter service.FinanceReportFilter, facts *service.FinanceSummaryFacts) error {
	if !financeCashApplies(filter) {
		return nil
	}
	var paymentCash, upstreamCash, walletCash string
	err := r.db.QueryRowContext(ctx, `
SELECT (
  COALESCE(SUM(pay_amount) FILTER (WHERE paid_at >= $1 AND paid_at < $2),0)
  - COALESCE(SUM(refund_amount) FILTER (WHERE refund_amount>0 AND refund_at >= $1 AND refund_at < $2),0)
)::text
FROM payment_orders po
WHERE NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3)`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&paymentCash)
	if err != nil {
		return fmt.Errorf("summarize payment cash: %w", err)
	}
	err = r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(CASE
	WHEN event_type='topup' THEN -usd_amount
 WHEN event_type='refund' THEN usd_amount
 ELSE usd_amount END),0)::text
FROM upstream_fund_events WHERE event_type <> 'opening_balance' AND occurred_at >= $1 AND occurred_at < $2`, filter.StartAt, filter.EndBefore).Scan(&upstreamCash)
	if err != nil {
		return fmt.Errorf("summarize upstream cash: %w", err)
	}
	err = r.db.QueryRowContext(ctx, `
WITH wallet_latest AS (
 SELECT DISTINCT ON (s.wallet_id) s.wallet_id,s.balance_amount,s.collected_at,
        COALESCE(NULLIF(w.balance_scope_key,''),'wallet:'||s.wallet_id::text) AS balance_scope_key
 FROM upstream_balance_snapshots s JOIN upstream_wallets w ON w.id=s.wallet_id
	WHERE s.balance_kind='wallet_cash' AND s.sync_status='success' AND s.currency='USD'
 ORDER BY s.wallet_id,s.collected_at DESC,s.id DESC
), latest AS (
 SELECT DISTINCT ON (balance_scope_key) balance_scope_key,balance_amount,collected_at
 FROM wallet_latest ORDER BY balance_scope_key,collected_at DESC,wallet_id
)
SELECT COALESCE(SUM(balance_amount) FILTER (WHERE collected_at >= NOW()-INTERVAL '20 minutes'),0)::text
FROM latest`).Scan(&walletCash)
	if err != nil {
		return fmt.Errorf("summarize wallet cash: %w", err)
	}
	if err = r.db.QueryRowContext(ctx, `
SELECT COUNT(*)::bigint
FROM finance_alerts alert
WHERE alert.status IN ('open','acknowledged')
  AND NOT (alert.alert_type='negative_profit' AND COALESCE(alert.dimension_type,'')='global')
  AND (
    COALESCE(alert.dimension_type,'') <> 'usage_log'
    OR EXISTS (
      SELECT 1 FROM usage_finance_records ufr
      WHERE ufr.usage_log_id=alert.dimension_id
        AND COALESCE(ufr.business_type,'') <> $3
        AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=ufr.user_id AND finance_admin.role=$3)
    )
  )
  AND (
    alert.alert_type NOT IN ('missing_price','missing_multiplier','missing_usage')
    OR EXISTS (
      SELECT 1 FROM usage_finance_records ufr
      WHERE ufr.usage_created_at >= $1 AND ufr.usage_created_at < $2
        AND ufr.cost_status=alert.alert_type
        AND COALESCE(ufr.business_type,'') <> $3
        AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=ufr.user_id AND finance_admin.role=$3)
    )
  )
  AND (
    alert.alert_type <> 'payment_fee_uncollected'
    OR EXISTS (
      SELECT 1 FROM payment_orders po
      WHERE po.paid_at >= $1 AND po.paid_at < $2
        AND alert.aggregation_key='payment_fee_uncollected:'||COALESCE(NULLIF(po.provider_key,''),'unknown')
        AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3)
        AND NOT EXISTS (SELECT 1 FROM payment_provider_fee_events fee WHERE fee.payment_order_id=po.id AND fee.fee_status='confirmed')
    )
  )`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&facts.OpenAlertCount); err != nil {
		return fmt.Errorf("count finance alerts: %w", err)
	}
	if facts.PaymentNetCash, err = decimal.NewFromString(paymentCash); err != nil {
		return err
	}
	if facts.UpstreamNetCash, err = decimal.NewFromString(upstreamCash); err != nil {
		return err
	}
	if facts.WalletCashTotal, err = decimal.NewFromString(walletCash); err != nil {
		return err
	}
	return nil
}

func (r *financeReportRepository) attachFinancePaymentFees(ctx context.Context, filter service.FinanceReportFilter, facts *service.FinanceSummaryFacts) error {
	if !financeCashApplies(filter) {
		return nil
	}
	var paymentFees string
	if err := r.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(fee.fee_usd_amount),0)::text
FROM payment_provider_fee_events fee
WHERE fee.occurred_at >= $1 AND fee.occurred_at < $2 AND fee.fee_status='confirmed'
  AND EXISTS (
    SELECT 1 FROM payment_orders po
    WHERE po.id=fee.payment_order_id
      AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=po.user_id AND finance_admin.role=$3)
  )`, filter.StartAt, filter.EndBefore, service.RoleAdmin).Scan(&paymentFees); err != nil {
		return fmt.Errorf("summarize payment fees: %w", err)
	}
	amount, err := decimal.NewFromString(paymentFees)
	if err != nil {
		return err
	}
	facts.PaymentFees = amount
	return nil
}

// Cash and wallet snapshots cannot be attributed to a user, group, account or
// model without a separately reconciled allocation. Returning a global balance
// under those filters would make the report look precise while being wrong.
func financeCashApplies(filter service.FinanceReportFilter) bool {
	return filter.UserID == nil && filter.GroupID == nil && filter.ChannelID == nil && filter.AccountID == nil &&
		filter.UpstreamID == nil && filter.WalletID == nil && filter.RequestedModel == "" && filter.UpstreamModel == "" &&
		filter.BillingType == "" && filter.BusinessType == ""
}

func financeRechargeBonusApplies(filter service.FinanceReportFilter) bool {
	return filter.UserID == nil && filter.GroupID == nil && filter.ChannelID == nil && filter.AccountID == nil &&
		filter.RequestedModel == "" && filter.UpstreamModel == "" && filter.BillingType == "" && filter.BusinessType == ""
}

func (r *financeReportRepository) attachRechargeBonusIncome(ctx context.Context, filter service.FinanceReportFilter, facts *service.FinanceSummaryFacts) error {
	if !financeRechargeBonusApplies(filter) {
		return nil
	}
	args := []any{filter.StartAt, filter.EndBefore}
	where := "event.occurred_at >= $1 AND event.occurred_at < $2 AND event.bonus_status IN ('confirmed','reversed')"
	joins := ""
	if filter.WalletID != nil {
		args = append(args, *filter.WalletID)
		where += fmt.Sprintf(" AND event.wallet_id=$%d", len(args))
	}
	if filter.UpstreamID != nil {
		joins = " JOIN upstream_wallets wallet ON wallet.id=event.wallet_id"
		args = append(args, *filter.UpstreamID)
		where += fmt.Sprintf(" AND wallet.upstream_id=$%d", len(args))
	}
	var raw string
	query := "SELECT COALESCE(SUM(event.bonus_income_usd),0)::text FROM upstream_fund_events event" + joins + " WHERE " + where
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		return fmt.Errorf("summarize recharge bonus income: %w", err)
	}
	amount, err := decimal.NewFromString(raw)
	if err != nil {
		return err
	}
	facts.RechargeBonusIncome = amount
	return nil
}

func (r *financeReportRepository) attachRechargeBonusTrend(ctx context.Context, filter service.FinanceReportFilter, items *[]service.FinanceTrendFact) error {
	if !financeRechargeBonusApplies(filter) {
		return nil
	}
	args := []any{filter.StartAt, filter.EndBefore, filter.Granularity, filter.Timezone}
	where := "event.occurred_at >= $1 AND event.occurred_at < $2 AND event.bonus_status IN ('confirmed','reversed')"
	joins := ""
	if filter.WalletID != nil {
		args = append(args, *filter.WalletID)
		where += fmt.Sprintf(" AND event.wallet_id=$%d", len(args))
	}
	if filter.UpstreamID != nil {
		joins = " JOIN upstream_wallets wallet ON wallet.id=event.wallet_id"
		args = append(args, *filter.UpstreamID)
		where += fmt.Sprintf(" AND wallet.upstream_id=$%d", len(args))
	}
	query := "SELECT (date_trunc($3, event.occurred_at AT TIME ZONE $4) AT TIME ZONE $4), COALESCE(SUM(event.bonus_income_usd),0)::text FROM upstream_fund_events event" + joins + " WHERE " + where + " GROUP BY 1"
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("list recharge bonus trend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byBucket := make(map[int64]int, len(*items))
	for index, item := range *items {
		byBucket[item.BucketStart.Unix()] = index
	}
	for rows.Next() {
		var bucket time.Time
		var raw string
		if err := rows.Scan(&bucket, &raw); err != nil {
			return err
		}
		amount, err := decimal.NewFromString(raw)
		if err != nil {
			return err
		}
		key := bucket.UTC().Unix()
		if index, ok := byBucket[key]; ok {
			(*items)[index].RechargeBonusIncome = amount
			continue
		}
		*items = append(*items, service.FinanceTrendFact{BucketStart: bucket.UTC(), RechargeBonusIncome: amount})
		byBucket[key] = len(*items) - 1
	}
	return rows.Err()
}

func (r *financeReportRepository) attachUnallocatedSubscriptionRevenue(ctx context.Context, filter service.FinanceReportFilter, facts *service.FinanceSummaryFacts) error {
	// Unallocated subscription revenue belongs to the business period, but cannot be
	// attributed to an upstream/request dimension until usage exists.
	if filter.AccountID != nil || filter.ChannelID != nil || filter.UpstreamID != nil || filter.WalletID != nil ||
		filter.RequestedModel != "" || filter.UpstreamModel != "" || filter.BillingType != "" ||
		(filter.BusinessType != "" && filter.BusinessType != "subscription") {
		return nil
	}
	startDate := filter.StartAt.In(filter.Location).Format("2006-01-02")
	endDate := filter.EndBefore.In(filter.Location).Format("2006-01-02")
	args := []any{startDate, endDate, service.RoleAdmin}
	where := "recognition_date >= $1::date AND recognition_date < $2::date AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=subscription_revenue_recognitions.user_id AND finance_admin.role=$3)"
	if filter.UserID != nil {
		args = append(args, *filter.UserID)
		where += fmt.Sprintf(" AND user_id=$%d", len(args))
	}
	if filter.GroupID != nil {
		args = append(args, *filter.GroupID)
		where += fmt.Sprintf(" AND group_id=$%d", len(args))
	}
	var raw string
	if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(unallocated_revenue),0)::text FROM subscription_revenue_recognitions WHERE `+where, args...).Scan(&raw); err != nil {
		return fmt.Errorf("summarize unallocated subscription revenue: %w", err)
	}
	amount, err := decimal.NewFromString(raw)
	if err != nil {
		return err
	}
	facts.Revenue = facts.Revenue.Add(amount)
	return nil
}

func (r *financeReportRepository) attachUnallocatedSubscriptionTrend(ctx context.Context, filter service.FinanceReportFilter, items *[]service.FinanceTrendFact) error {
	if filter.AccountID != nil || filter.ChannelID != nil || filter.UpstreamID != nil || filter.WalletID != nil ||
		filter.RequestedModel != "" || filter.UpstreamModel != "" || filter.BillingType != "" ||
		(filter.BusinessType != "" && filter.BusinessType != "subscription") {
		return nil
	}
	args := []any{
		filter.StartAt.In(filter.Location).Format("2006-01-02"),
		filter.EndBefore.In(filter.Location).Format("2006-01-02"),
		filter.Granularity,
		filter.Timezone,
		service.RoleAdmin,
	}
	where := "recognition_date >= $1::date AND recognition_date < $2::date AND NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=subscription_revenue_recognitions.user_id AND finance_admin.role=$5)"
	if filter.UserID != nil {
		args = append(args, *filter.UserID)
		where += fmt.Sprintf(" AND user_id=$%d", len(args))
	}
	if filter.GroupID != nil {
		args = append(args, *filter.GroupID)
		where += fmt.Sprintf(" AND group_id=$%d", len(args))
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT (date_trunc($3,recognition_date::timestamp) AT TIME ZONE $4) AS bucket_start,
       COALESCE(SUM(unallocated_revenue),0)::text
FROM subscription_revenue_recognitions
WHERE `+where+`
GROUP BY bucket_start`, args...)
	if err != nil {
		return fmt.Errorf("list unallocated subscription revenue trend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byBucket := make(map[time.Time]int, len(*items))
	for index := range *items {
		byBucket[(*items)[index].BucketStart] = index
	}
	for rows.Next() {
		var bucket time.Time
		var raw string
		if err = rows.Scan(&bucket, &raw); err != nil {
			return err
		}
		amount, parseErr := decimal.NewFromString(raw)
		if parseErr != nil {
			return parseErr
		}
		if index, ok := byBucket[bucket]; ok {
			(*items)[index].Revenue = (*items)[index].Revenue.Add(amount)
			continue
		}
		*items = append(*items, service.FinanceTrendFact{BucketStart: bucket, Revenue: amount})
		byBucket[bucket] = len(*items) - 1
	}
	if err = rows.Err(); err != nil {
		return err
	}
	sort.Slice(*items, func(i, j int) bool { return (*items)[i].BucketStart.Before((*items)[j].BucketStart) })
	return nil
}

func financeReportWhere(filter service.FinanceReportFilter, alias string) (string, []any) {
	args := []any{filter.StartAt, filter.EndBefore, service.RoleAdmin}
	conditions := []string{
		alias + ".usage_created_at >= $1",
		alias + ".usage_created_at < $2",
		"COALESCE(" + alias + ".business_type,'') <> $3",
		"NOT EXISTS (SELECT 1 FROM users finance_admin WHERE finance_admin.id=" + alias + ".user_id AND finance_admin.role=$3)",
	}
	addEqual := func(column string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf("%s.%s=$%d", alias, column, len(args)))
	}
	for _, item := range []struct {
		column string
		value  *int64
	}{
		{"user_id", filter.UserID}, {"group_id", filter.GroupID}, {"channel_id", filter.ChannelID},
		{"upstream_id", filter.UpstreamID}, {"wallet_id", filter.WalletID}, {"account_id", filter.AccountID},
	} {
		if item.value != nil {
			addEqual(item.column, *item.value)
		}
	}
	for _, item := range []struct{ column, value string }{
		{"requested_model", filter.RequestedModel}, {"upstream_model", filter.UpstreamModel},
		{"billing_type", filter.BillingType}, {"business_type", filter.BusinessType},
	} {
		if item.value != "" {
			addEqual(item.column, item.value)
		}
	}
	if len(filter.CostStatuses) > 0 {
		args = append(args, pq.Array(filter.CostStatuses))
		conditions = append(conditions, fmt.Sprintf("%s.cost_status=ANY($%d)", alias, len(args)))
	}
	return strings.Join(conditions, " AND "), args
}
