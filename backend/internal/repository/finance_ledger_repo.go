package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/usagefinancecostsegment"
	"github.com/Wei-Shaw/sub2api/ent/usagefinancerecord"
	"github.com/Wei-Shaw/sub2api/ent/usageupstreamattempt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

const (
	financeMigrationFilename = "172_finance_cost_ledger_and_upstream_sync.sql"
	financeScannerLockID     = int64(734729842987345123)
)

type financeLedgerRepository struct {
	client *dbent.Client
	sql    *sql.DB
}

func NewFinanceLedgerRepository(client *dbent.Client, sqlDB *sql.DB) service.FinanceLedgerRepository {
	return &financeLedgerRepository{client: client, sql: sqlDB}
}

func (r *financeLedgerRepository) FinanceLaunchAt(ctx context.Context) (time.Time, error) {
	var appliedAt time.Time
	err := r.sql.QueryRowContext(ctx,
		"SELECT applied_at FROM schema_migrations WHERE filename = $1",
		financeMigrationFilename,
	).Scan(&appliedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("read finance migration boundary: %w", err)
	}
	return appliedAt, nil
}

func (r *financeLedgerRepository) TryAcquireScannerLease(ctx context.Context) (func(), bool, error) {
	conn, err := r.sql.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", financeScannerLockID).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("acquire finance scanner lease: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		return func() {}, false, nil
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", financeScannerLockID)
		_ = conn.Close()
	}
	return release, true, nil
}

func (r *financeLedgerRepository) ListPendingUsage(ctx context.Context, cursor service.FinanceUsageCursor, limit int) ([]service.UsageLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := `SELECT ` + usageLogSelectColumns + `
		FROM usage_logs u
		WHERE NOT EXISTS (
		      SELECT 1 FROM usage_finance_records f WHERE f.usage_log_id = u.id
		  )
		  AND (
		    u.created_at > $1 OR (u.created_at = $1 AND u.id > $2)
		    OR EXISTS (
		      SELECT 1 FROM finance_ledger_retries r
		      WHERE r.usage_log_id=u.id AND r.status='pending' AND r.next_retry_at<=NOW()
		    )
		  )
		ORDER BY CASE WHEN EXISTS (
		  SELECT 1 FROM finance_ledger_retries r
		  WHERE r.usage_log_id=u.id AND r.status='pending' AND r.next_retry_at<=NOW()
		) THEN 0 ELSE 1 END, u.created_at ASC, u.id ASC
		LIMIT $3`
	rows, err := r.sql.QueryContext(ctx, query, cursor.CreatedAt, cursor.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending finance usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	logs := make([]service.UsageLog, 0, limit)
	for rows.Next() {
		log, scanErr := scanUsageLog(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		logs = append(logs, *log)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = attachFinanceUsageUsers(ctx, r.sql, logs); err != nil {
		return nil, err
	}
	if err = attachFinanceUsageClassifications(ctx, r.sql, logs); err != nil {
		return nil, err
	}
	return logs, nil
}

func attachFinanceUsageClassifications(ctx context.Context, db *sql.DB, logs []service.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(logs))
	byID := make(map[int64]*service.UsageLog, len(logs))
	for index := range logs {
		ids = append(ids, logs[index].ID)
		byID[logs[index].ID] = &logs[index]
	}
	rows, err := db.QueryContext(ctx, `
SELECT u.id,
       CASE WHEN class.finance_classification_recorded THEN class.finance_business_type ELSE 'legacy_unknown' END,
       CASE WHEN class.finance_classification_recorded THEN class.promotion_credit_used ELSE 0 END,
       CASE WHEN class.finance_classification_recorded THEN class.finance_excluded ELSE TRUE END,
       CASE WHEN class.finance_classification_recorded THEN COALESCE(class.finance_exclusion_reason,'') ELSE 'legacy_unclassified' END
FROM usage_logs u
JOIN LATERAL (
  SELECT finance_business_type,promotion_credit_used,finance_excluded,finance_exclusion_reason,
         finance_classification_recorded
  FROM (
    SELECT finance_business_type,promotion_credit_used,finance_excluded,finance_exclusion_reason,
           finance_classification_recorded,0 AS priority
    FROM usage_billing_dedup WHERE request_id=u.request_id AND api_key_id=u.api_key_id
    UNION ALL
    SELECT finance_business_type,promotion_credit_used,finance_excluded,finance_exclusion_reason,
           finance_classification_recorded,1 AS priority
    FROM usage_billing_dedup_archive WHERE request_id=u.request_id AND api_key_id=u.api_key_id
  ) candidates ORDER BY priority LIMIT 1
) class ON TRUE
WHERE u.id=ANY($1)`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("attach finance usage classifications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var businessType, exclusionReason string
		var promotionUsed decimal.Decimal
		var excluded bool
		if err = rows.Scan(&id, &businessType, &promotionUsed, &excluded, &exclusionReason); err != nil {
			return err
		}
		if log := byID[id]; log != nil {
			log.FinanceBusinessTypeSnapshot = businessType
			log.PromotionCreditUsed = &promotionUsed
			log.FinanceExcluded = excluded
			log.FinanceExclusionReason = exclusionReason
		}
	}
	return rows.Err()
}

func attachFinanceUsageUsers(ctx context.Context, db *sql.DB, logs []service.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(logs))
	seen := make(map[int64]struct{}, len(logs))
	for i := range logs {
		if logs[i].UserID <= 0 {
			continue
		}
		if _, exists := seen[logs[i].UserID]; exists {
			continue
		}
		seen[logs[i].UserID] = struct{}{}
		ids = append(ids, logs[i].UserID)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id,role FROM users WHERE id=ANY($1)`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("load finance usage user roles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	roles := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var role string
		if err = rows.Scan(&id, &role); err != nil {
			return err
		}
		roles[id] = role
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range logs {
		if role, exists := roles[logs[i].UserID]; exists {
			logs[i].User = &service.User{ID: logs[i].UserID, Role: role}
		}
	}
	return nil
}

func (r *financeLedgerRepository) RecordFinanceProjectionFailure(ctx context.Context, usageLogID int64, message string, failedAt time.Time) error {
	_, err := r.sql.ExecContext(ctx, `
INSERT INTO finance_ledger_retries(usage_log_id,attempt_count,last_error,next_retry_at,status,first_failed_at,last_failed_at)
VALUES($1,1,$2,$3::timestamptz+INTERVAL '30 seconds','pending',$3::timestamptz,$3::timestamptz)
ON CONFLICT(usage_log_id) DO UPDATE SET
 attempt_count=finance_ledger_retries.attempt_count+1,
 last_error=EXCLUDED.last_error,
 last_failed_at=EXCLUDED.last_failed_at,
 next_retry_at=EXCLUDED.last_failed_at + LEAST(INTERVAL '6 hours', INTERVAL '30 seconds' * POWER(2, LEAST(finance_ledger_retries.attempt_count,10))),
 status=CASE WHEN finance_ledger_retries.attempt_count+1 >= 8 THEN 'exhausted' ELSE 'pending' END,
 resolved_at=NULL`, usageLogID, message, failedAt)
	if err != nil {
		return fmt.Errorf("record finance projection failure: %w", err)
	}
	return nil
}

func (r *financeLedgerRepository) ResolveFinanceProjectionFailure(ctx context.Context, usageLogID int64, resolvedAt time.Time) error {
	_, err := r.sql.ExecContext(ctx, `
UPDATE finance_ledger_retries
SET status='resolved',resolved_at=$2
WHERE usage_log_id=$1 AND status<>'resolved'`, usageLogID, resolvedAt)
	if err != nil {
		return fmt.Errorf("resolve finance projection failure: %w", err)
	}
	return nil
}

func (r *financeLedgerRepository) LoadUsageAttempts(ctx context.Context, usageLogIDs []int64) (map[int64][]service.UsageUpstreamAttempt, error) {
	result := make(map[int64][]service.UsageUpstreamAttempt, len(usageLogIDs))
	if len(usageLogIDs) == 0 {
		return result, nil
	}
	rows, err := r.client.UsageUpstreamAttempt.Query().
		Where(usageupstreamattempt.UsageLogIDIn(usageLogIDs...)).
		Order(dbent.Asc(usageupstreamattempt.FieldUsageLogID), dbent.Asc(usageupstreamattempt.FieldAttemptNo)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load usage upstream attempts: %w", err)
	}
	for _, row := range rows {
		result[row.UsageLogID] = append(result[row.UsageLogID], service.UsageUpstreamAttempt{
			ID:                            row.ID,
			UsageLogID:                    row.UsageLogID,
			RequestID:                     row.RequestID,
			AttemptNo:                     row.AttemptNo,
			AccountID:                     row.AccountID,
			ChannelID:                     cloneRepositoryInt64(row.ChannelID),
			UpstreamModel:                 row.UpstreamModel,
			ServiceTier:                   cloneRepositoryString(row.ServiceTier),
			InputTokens:                   row.InputTokens,
			OutputTokens:                  row.OutputTokens,
			CacheReadTokens:               row.CacheReadTokens,
			CacheCreationTokens:           row.CacheCreationTokens,
			CacheCreation5mTokens:         row.CacheCreation5mTokens,
			CacheCreation1hTokens:         row.CacheCreation1hTokens,
			RequestCount:                  row.RequestCount,
			ImageCount:                    row.ImageCount,
			VideoSeconds:                  row.VideoSeconds,
			UpstreamCostMultiplier:        cloneRepositoryDecimal(row.UpstreamCostMultiplier),
			UpstreamMultiplierChangeID:    cloneRepositoryInt64(row.UpstreamMultiplierChangeID),
			UpstreamMultiplierSource:      dereferenceRepositoryString(row.UpstreamMultiplierSource),
			UpstreamMultiplierEffectiveAt: cloneRepositoryTime(row.UpstreamMultiplierEffectiveAt),
			AccountFinanceProfileID:       cloneRepositoryInt64(row.AccountFinanceProfileID),
			Billable:                      row.Billable,
			BillingObservedAt:             cloneRepositoryTime(row.BillingObservedAt),
			UpstreamActualCharge:          cloneRepositoryDecimal(row.UpstreamActualCharge),
			UpstreamActualChargeUSD:       cloneRepositoryDecimal(row.UpstreamActualChargeUsd),
			UpstreamStandardCharge:        cloneRepositoryDecimal(row.UpstreamStandardCharge),
			UpstreamChargeCurrency:        dereferenceRepositoryString(row.UpstreamChargeCurrency),
			UpstreamChargeUnitSemantics:   dereferenceRepositoryString(row.UpstreamChargeUnitSemantics),
			UpstreamBillingRequestID:      dereferenceRepositoryString(row.UpstreamBillingRequestID),
			UpstreamChargeSnapshot:        row.UpstreamChargeSnapshot,
			CompletedAt:                   row.CompletedAt,
			CreatedAt:                     row.CreatedAt,
		})
	}
	return result, nil
}

func (r *financeLedgerRepository) CreateFinanceProjection(ctx context.Context, projection *service.UsageFinanceProjection) (bool, error) {
	if projection == nil {
		return false, fmt.Errorf("finance projection is required")
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	exists, err := tx.UsageFinanceRecord.Query().
		Where(usagefinancerecord.UsageLogIDEQ(projection.UsageLogID)).
		Exist(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	entity, err := createFinanceProjectionEntity(ctx, tx, projection)
	if err != nil {
		if dbent.IsConstraintError(err) {
			return false, nil
		}
		return false, err
	}
	if err = createFinanceSegmentEntities(ctx, tx, entity.ID, projection.Segments); err != nil {
		return false, err
	}
	if err = createBalanceRevenueAllocation(ctx, tx, projection); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	projection.ID = entity.ID
	projection.CurrentRevision = 1
	return true, nil
}

func (r *financeLedgerRepository) ReviseFinanceProjection(ctx context.Context, projection *service.UsageFinanceProjection, metadata service.FinanceRevisionMetadata) (bool, error) {
	if projection == nil {
		return false, fmt.Errorf("finance projection is required")
	}
	if strings.TrimSpace(metadata.Reason) == "" {
		return false, fmt.Errorf("finance revision reason is required")
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	currentEntity, err := tx.UsageFinanceRecord.Query().
		Where(usagefinancerecord.UsageLogIDEQ(projection.UsageLogID)).
		Only(ctx)
	if err != nil {
		return false, err
	}
	currentSegments, err := tx.UsageFinanceCostSegment.Query().
		Where(usagefinancecostsegment.UsageFinanceRecordIDEQ(currentEntity.ID)).
		Order(dbent.Asc(usagefinancecostsegment.FieldAttemptNo)).
		All(ctx)
	if err != nil {
		return false, err
	}
	current := financeEntityToProjection(currentEntity, currentSegments)
	projection.ID = currentEntity.ID
	if financeProjectionSame(current, projection) {
		return false, nil
	}
	nextRevision := currentEntity.CurrentRevision + 1
	oldResult := financeProjectionResultMap(current)
	newResult := financeProjectionResultMap(projection)
	revision := tx.FinanceCalculationRevision.Create().
		SetEntityType("usage_finance_record").
		SetEntityID(currentEntity.ID).
		SetRevision(nextRevision).
		SetOldResult(oldResult).
		SetNewResult(newResult).
		SetReason(strings.TrimSpace(metadata.Reason)).
		SetNillableJobID(metadata.JobID).
		SetNillableOperatorID(metadata.OperatorID)
	if _, err = revision.Save(ctx); err != nil {
		return false, err
	}
	update := tx.UsageFinanceRecord.UpdateOneID(currentEntity.ID).
		SetNillableGroupID(projection.GroupID).
		SetNillableChannelID(projection.ChannelID).
		SetNillableAccountID(projection.AccountID).
		SetNillableWalletID(projection.WalletID).
		SetNillableUpstreamID(projection.UpstreamID).
		SetNillableUpstreamModel(projection.UpstreamModel).
		SetNillableServiceTier(projection.ServiceTier).
		SetBillingType(projection.BillingType).
		SetBusinessType(projection.BusinessType).
		SetNillableUsageListValue(projection.UsageListValue).
		SetNillableUpstreamCost(projection.UpstreamCost).
		SetCostStatus(string(projection.CostStatus)).
		SetNillablePriceVersionID(projection.PriceVersionID).
		SetNillableUpstreamCostMultiplierSnapshot(projection.UpstreamCostMultiplierSnapshot).
		SetNillableUpstreamMultiplierChangeID(projection.UpstreamMultiplierChangeID).
		SetNillableUpstreamMultiplierEffectiveAt(projection.UpstreamMultiplierEffectiveAt).
		SetNillableAccountFinanceProfileID(projection.AccountFinanceProfileID).
		SetNillableFxRateVersionID(projection.FXRateVersionID).
		SetNillableFxRateToUsd(projection.FXRateToUSD).
		SetNillableFxObservedAt(projection.FXObservedAt).
		SetCurrentRevision(nextRevision).
		SetCalculationDetail(projection.CalculationDetail).
		SetCalculatedAt(projection.CalculatedAt)
	if strings.TrimSpace(projection.PricingSource) == "" {
		update.ClearPricingSource()
	} else {
		update.SetPricingSource(projection.PricingSource)
	}
	if strings.TrimSpace(projection.UpstreamMultiplierSource) == "" {
		update.ClearUpstreamMultiplierSource()
	} else {
		update.SetUpstreamMultiplierSource(projection.UpstreamMultiplierSource)
	}
	if strings.TrimSpace(projection.SourceCurrency) == "" {
		update.ClearSourceCurrency()
	} else {
		update.SetSourceCurrency(projection.SourceCurrency)
	}
	if strings.TrimSpace(projection.FXSource) == "" {
		update.ClearFxSource()
	} else {
		update.SetFxSource(projection.FXSource)
	}
	if projection.UpstreamMultiplierChangeID == nil {
		update.ClearUpstreamMultiplierChangeID()
	}
	if projection.UpstreamMultiplierEffectiveAt == nil {
		update.ClearUpstreamMultiplierEffectiveAt()
	}
	if projection.AccountFinanceProfileID == nil {
		update.ClearAccountFinanceProfileID()
	}
	if projection.FXRateVersionID == nil {
		update.ClearFxRateVersionID()
	}
	if projection.FXRateToUSD == nil {
		update.ClearFxRateToUsd()
	}
	if projection.FXObservedAt == nil {
		update.ClearFxObservedAt()
	}
	if _, err = update.Save(ctx); err != nil {
		return false, err
	}
	if _, err = tx.UsageFinanceCostSegment.Delete().
		Where(usagefinancecostsegment.UsageFinanceRecordIDEQ(currentEntity.ID)).
		Exec(ctx); err != nil {
		return false, err
	}
	if err = createFinanceSegmentEntities(ctx, tx, currentEntity.ID, projection.Segments); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	projection.CurrentRevision = nextRevision
	return true, nil
}

func createFinanceProjectionEntity(ctx context.Context, tx *dbent.Tx, projection *service.UsageFinanceProjection) (*dbent.UsageFinanceRecord, error) {
	builder := tx.UsageFinanceRecord.Create().
		SetUsageLogID(projection.UsageLogID).
		SetUserID(projection.UserID).
		SetNillableGroupID(projection.GroupID).
		SetNillableChannelID(projection.ChannelID).
		SetNillableAccountID(projection.AccountID).
		SetNillableWalletID(projection.WalletID).
		SetNillableUpstreamID(projection.UpstreamID).
		SetUsageCreatedAt(projection.UsageCreatedAt).
		SetRequestedModel(projection.RequestedModel).
		SetNillableUpstreamModel(projection.UpstreamModel).
		SetNillableServiceTier(projection.ServiceTier).
		SetBillingType(projection.BillingType).
		SetBusinessType(projection.BusinessType).
		SetNillableUsageListValue(projection.UsageListValue).
		SetNillableUpstreamCost(projection.UpstreamCost).
		SetCostStatus(string(projection.CostStatus)).
		SetNillablePriceVersionID(projection.PriceVersionID).
		SetNillableUpstreamCostMultiplierSnapshot(projection.UpstreamCostMultiplierSnapshot).
		SetNillableUpstreamMultiplierChangeID(projection.UpstreamMultiplierChangeID).
		SetNillableUpstreamMultiplierEffectiveAt(projection.UpstreamMultiplierEffectiveAt).
		SetNillableAccountFinanceProfileID(projection.AccountFinanceProfileID).
		SetNillableFxRateVersionID(projection.FXRateVersionID).
		SetNillableFxRateToUsd(projection.FXRateToUSD).
		SetNillableFxObservedAt(projection.FXObservedAt).
		SetCurrentRevision(1).
		SetCalculationDetail(projection.CalculationDetail).
		SetCalculatedAt(projection.CalculatedAt)
	if strings.TrimSpace(projection.PricingSource) != "" {
		builder.SetPricingSource(projection.PricingSource)
	}
	if strings.TrimSpace(projection.UpstreamMultiplierSource) != "" {
		builder.SetUpstreamMultiplierSource(projection.UpstreamMultiplierSource)
	}
	if strings.TrimSpace(projection.SourceCurrency) != "" {
		builder.SetSourceCurrency(projection.SourceCurrency)
	}
	if strings.TrimSpace(projection.FXSource) != "" {
		builder.SetFxSource(projection.FXSource)
	}
	return builder.Save(ctx)
}

func createFinanceSegmentEntities(ctx context.Context, tx *dbent.Tx, recordID int64, segments []service.UsageFinanceCostSegment) error {
	if len(segments) == 0 {
		return nil
	}
	builders := make([]*dbent.UsageFinanceCostSegmentCreate, 0, len(segments))
	for _, segment := range segments {
		builder := tx.UsageFinanceCostSegment.Create().
			SetUsageFinanceRecordID(recordID).
			SetAttemptNo(segment.AttemptNo).
			SetAccountID(segment.AccountID).
			SetNillableWalletID(segment.WalletID).
			SetNillableUpstreamID(segment.UpstreamID).
			SetNillableChannelID(segment.ChannelID).
			SetUpstreamModel(segment.UpstreamModel).
			SetNillableServiceTier(segment.ServiceTier).
			SetUsageDetail(segment.UsageDetail).
			SetNillableUpstreamCostMultiplierSnapshot(segment.UpstreamCostMultiplier).
			SetNillableUpstreamMultiplierChangeID(segment.UpstreamMultiplierChangeID).
			SetNillableUpstreamMultiplierEffectiveAt(segment.UpstreamMultiplierEffectiveAt).
			SetNillableAccountFinanceProfileID(segment.AccountFinanceProfileID).
			SetNillableFxRateVersionID(segment.FXRateVersionID).
			SetNillableFxRateToUsd(segment.FXRateToUSD).
			SetNillableFxObservedAt(segment.FXObservedAt).
			SetNillablePriceVersionID(segment.PriceVersionID).
			SetCostStatus(string(segment.CostStatus)).
			SetNillableCostAmount(segment.CostAmount).
			SetCalculationDetail(segment.CalculationDetail)
		if strings.TrimSpace(segment.PricingSource) != "" {
			builder.SetPricingSource(segment.PricingSource)
		}
		if strings.TrimSpace(segment.UpstreamMultiplierSource) != "" {
			builder.SetUpstreamMultiplierSource(segment.UpstreamMultiplierSource)
		}
		if strings.TrimSpace(segment.SourceCurrency) != "" {
			builder.SetSourceCurrency(segment.SourceCurrency)
		}
		if strings.TrimSpace(segment.FXSource) != "" {
			builder.SetFxSource(segment.FXSource)
		}
		builders = append(builders, builder)
	}
	_, err := tx.UsageFinanceCostSegment.CreateBulk(builders...).Save(ctx)
	return err
}

func createBalanceRevenueAllocation(ctx context.Context, tx *dbent.Tx, projection *service.UsageFinanceProjection) error {
	if projection == nil || projection.CustomerBillingType != service.BillingTypeBalance {
		return nil
	}
	if projection.UsageListValue == nil {
		return fmt.Errorf("balance usage revenue requires usage list value")
	}
	recognitionDate := time.Date(
		projection.UsageCreatedAt.UTC().Year(),
		projection.UsageCreatedAt.UTC().Month(),
		projection.UsageCreatedAt.UTC().Day(),
		0, 0, 0, 0, time.UTC,
	)
	_, err := tx.UsageRevenueAllocation.Create().
		SetUsageLogID(projection.UsageLogID).
		SetSourceType("balance_usage").
		SetAllocatedAmount(*projection.UsageListValue).
		SetAllocationMethod("actual_usage_charge").
		SetRecognitionDate(recognitionDate).
		SetRevision(1).
		SetAuditDetail(map[string]any{
			"usage_list_value": projection.UsageListValue.StringFixed(10),
			"source":           "usage_logs.usage_list_value",
		}).
		Save(ctx)
	return err
}

func financeEntityToProjection(entity *dbent.UsageFinanceRecord, segments []*dbent.UsageFinanceCostSegment) *service.UsageFinanceProjection {
	projection := &service.UsageFinanceProjection{
		ID:                             entity.ID,
		UsageLogID:                     entity.UsageLogID,
		UserID:                         entity.UserID,
		GroupID:                        cloneRepositoryInt64(entity.GroupID),
		ChannelID:                      cloneRepositoryInt64(entity.ChannelID),
		AccountID:                      cloneRepositoryInt64(entity.AccountID),
		WalletID:                       cloneRepositoryInt64(entity.WalletID),
		UpstreamID:                     cloneRepositoryInt64(entity.UpstreamID),
		UsageCreatedAt:                 entity.UsageCreatedAt,
		RequestedModel:                 entity.RequestedModel,
		UpstreamModel:                  cloneRepositoryString(entity.UpstreamModel),
		ServiceTier:                    cloneRepositoryString(entity.ServiceTier),
		BillingType:                    entity.BillingType,
		BusinessType:                   entity.BusinessType,
		UsageListValue:                 cloneRepositoryDecimal(entity.UsageListValue),
		UpstreamCost:                   cloneRepositoryDecimal(entity.UpstreamCost),
		CostStatus:                     service.FinanceCostStatus(entity.CostStatus),
		PricingSource:                  dereferenceRepositoryString(entity.PricingSource),
		PriceVersionID:                 cloneRepositoryInt64(entity.PriceVersionID),
		UpstreamCostMultiplierSnapshot: cloneRepositoryDecimal(entity.UpstreamCostMultiplierSnapshot),
		UpstreamMultiplierChangeID:     cloneRepositoryInt64(entity.UpstreamMultiplierChangeID),
		UpstreamMultiplierSource:       dereferenceRepositoryString(entity.UpstreamMultiplierSource),
		UpstreamMultiplierEffectiveAt:  cloneRepositoryTime(entity.UpstreamMultiplierEffectiveAt),
		AccountFinanceProfileID:        cloneRepositoryInt64(entity.AccountFinanceProfileID),
		FXRateVersionID:                cloneRepositoryInt64(entity.FxRateVersionID),
		SourceCurrency:                 dereferenceRepositoryString(entity.SourceCurrency),
		FXRateToUSD:                    cloneRepositoryDecimal(entity.FxRateToUsd),
		FXSource:                       dereferenceRepositoryString(entity.FxSource),
		FXObservedAt:                   cloneRepositoryTime(entity.FxObservedAt),
		CurrentRevision:                entity.CurrentRevision,
		CalculationDetail:              entity.CalculationDetail,
		CalculatedAt:                   entity.CalculatedAt,
		Segments:                       make([]service.UsageFinanceCostSegment, 0, len(segments)),
	}
	for _, segment := range segments {
		projection.Segments = append(projection.Segments, service.UsageFinanceCostSegment{
			AttemptNo:                     segment.AttemptNo,
			AccountID:                     segment.AccountID,
			WalletID:                      cloneRepositoryInt64(segment.WalletID),
			UpstreamID:                    cloneRepositoryInt64(segment.UpstreamID),
			ChannelID:                     cloneRepositoryInt64(segment.ChannelID),
			UpstreamModel:                 segment.UpstreamModel,
			ServiceTier:                   cloneRepositoryString(segment.ServiceTier),
			UsageDetail:                   segment.UsageDetail,
			UpstreamCostMultiplier:        cloneRepositoryDecimal(segment.UpstreamCostMultiplierSnapshot),
			UpstreamMultiplierChangeID:    cloneRepositoryInt64(segment.UpstreamMultiplierChangeID),
			UpstreamMultiplierSource:      dereferenceRepositoryString(segment.UpstreamMultiplierSource),
			UpstreamMultiplierEffectiveAt: cloneRepositoryTime(segment.UpstreamMultiplierEffectiveAt),
			AccountFinanceProfileID:       cloneRepositoryInt64(segment.AccountFinanceProfileID),
			PriceVersionID:                cloneRepositoryInt64(segment.PriceVersionID),
			FXRateVersionID:               cloneRepositoryInt64(segment.FxRateVersionID),
			SourceCurrency:                dereferenceRepositoryString(segment.SourceCurrency),
			FXRateToUSD:                   cloneRepositoryDecimal(segment.FxRateToUsd),
			FXSource:                      dereferenceRepositoryString(segment.FxSource),
			FXObservedAt:                  cloneRepositoryTime(segment.FxObservedAt),
			PricingSource:                 dereferenceRepositoryString(segment.PricingSource),
			CostStatus:                    service.FinanceCostStatus(segment.CostStatus),
			CostAmount:                    cloneRepositoryDecimal(segment.CostAmount),
			CalculationDetail:             segment.CalculationDetail,
		})
	}
	return projection
}

func financeProjectionResultMap(projection *service.UsageFinanceProjection) map[string]any {
	segments := make([]map[string]any, 0, len(projection.Segments))
	for _, segment := range projection.Segments {
		segments = append(segments, map[string]any{
			"attempt_no":         segment.AttemptNo,
			"account_id":         segment.AccountID,
			"wallet_id":          segment.WalletID,
			"upstream_id":        segment.UpstreamID,
			"cost_status":        segment.CostStatus,
			"cost_amount":        repositoryDecimalString(segment.CostAmount),
			"price_version_id":   segment.PriceVersionID,
			"pricing_source":     segment.PricingSource,
			"calculation_detail": segment.CalculationDetail,
		})
	}
	return map[string]any{
		"usage_log_id":                      projection.UsageLogID,
		"upstream_cost":                     repositoryDecimalString(projection.UpstreamCost),
		"cost_status":                       projection.CostStatus,
		"pricing_source":                    projection.PricingSource,
		"price_version_id":                  projection.PriceVersionID,
		"upstream_cost_multiplier_snapshot": repositoryDecimalString(projection.UpstreamCostMultiplierSnapshot),
		"calculation_detail":                projection.CalculationDetail,
		"segments":                          segments,
	}
}

func financeProjectionSame(left, right *service.UsageFinanceProjection) bool {
	leftJSON, _ := jsonMarshalStable(financeProjectionResultMap(left))
	rightJSON, _ := jsonMarshalStable(financeProjectionResultMap(right))
	return string(leftJSON) == string(rightJSON)
}

func jsonMarshalStable(value any) ([]byte, error) {
	return json.Marshal(value)
}

func cloneRepositoryInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneRepositoryString(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneRepositoryDecimal(value *decimal.Decimal) *decimal.Decimal {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneRepositoryTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func dereferenceRepositoryString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func repositoryDecimalString(value *decimal.Decimal) any {
	if value == nil {
		return nil
	}
	return value.StringFixed(10)
}
