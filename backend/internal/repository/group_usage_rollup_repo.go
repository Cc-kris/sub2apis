package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GroupUsageRollupRepository reads and rebuilds the group daily rollup tables.
type GroupUsageRollupRepository struct{ db *sql.DB }

const defaultGroupUsageTimezone = "Asia/Shanghai"

const groupUsageRollupLeaseDuration = 90 * time.Second

var groupUsageRollupLeaseOwner = func() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}()

func NewGroupUsageRollupRepository(db *sql.DB) *GroupUsageRollupRepository {
	return &GroupUsageRollupRepository{db: db}
}

func (r *GroupUsageRollupRepository) GetGroupUsageSummary(ctx context.Context, todayStart time.Time) ([]usagestats.GroupUsageSummary, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("group usage rollup database is nil")
	}
	tz := todayStart.Location().String()
	if tz == "Local" || tz == "" {
		tz = defaultGroupUsageTimezone
	}
	var state string
	err := r.db.QueryRowContext(ctx, `SELECT bootstrap_state FROM usage_group_rollup_timezones WHERE timezone=$1`, tz).Scan(&state)
	if err == sql.ErrNoRows {
		return r.listGroupUsageSource(ctx, "realtime_bootstrapping")
	}
	if err != nil {
		return nil, err
	}
	if state == "pending" || state == "rebuilding" {
		return r.listGroupUsageSource(ctx, "realtime_bootstrapping")
	}
	if state != "ready" {
		return nil, fmt.Errorf("group usage rollup timezone %s is %s", tz, state)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("load rollup timezone %q: %w", tz, err)
	}
	bucketDate := todayStart.In(loc).Format("2006-01-02")
	rows, err := r.db.QueryContext(ctx, `
		SELECT g.id, COALESCE(SUM(CASE WHEN r.bucket_date=$2::date THEN r.actual_cost ELSE 0 END),0), COALESCE(SUM(r.actual_cost),0)
		FROM groups g LEFT JOIN usage_group_daily_rollups r ON r.group_id=g.id AND r.timezone=$1
		GROUP BY g.id ORDER BY g.id`, tz, bucketDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usagestats.GroupUsageSummary
	for rows.Next() {
		var s usagestats.GroupUsageSummary
		if err := rows.Scan(&s.GroupID, &s.TodayCost, &s.TotalCost); err != nil {
			return nil, err
		}
		s.GroupUsageSource = "rollup"
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *GroupUsageRollupRepository) listGroupUsageSource(ctx context.Context, source string) ([]usagestats.GroupUsageSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usagestats.GroupUsageSummary
	for rows.Next() {
		var item usagestats.GroupUsageSummary
		if err := rows.Scan(&item.GroupID); err != nil {
			return nil, err
		}
		item.GroupUsageSource = source
		out = append(out, item)
	}
	return out, rows.Err()
}

// RegisterTimezone atomically registers a timezone and captures the bootstrap boundary.
func (r *GroupUsageRollupRepository) RegisterTimezone(ctx context.Context, timezone string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("group usage rollup database is nil")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO usage_group_rollup_timezones(timezone, bootstrap_from) SELECT $1, MIN(created_at) FROM usage_logs ON CONFLICT (timezone) DO NOTHING`, timezone)
	return err
}

func (r *GroupUsageRollupRepository) ListRegisteredTimezones(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT timezone FROM usage_group_rollup_timezones ORDER BY timezone`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var timezone string
		if err := rows.Scan(&timezone); err != nil {
			return nil, err
		}
		out = append(out, timezone)
	}
	return out, rows.Err()
}

// Refresh performs a one-time backfill, then only rebuilds invalidated buckets
// and the current local day. This keeps the worker incremental while retaining
// idempotent recovery after source-log corrections.
func (r *GroupUsageRollupRepository) Refresh(ctx context.Context, timezone string, now time.Time) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("group usage rollup database is nil")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("load rollup timezone %q: %w", timezone, err)
	}
	if err := r.RegisterTimezone(ctx, timezone); err != nil {
		return err
	}
	acquired, err := r.acquireLease(ctx, timezone, groupUsageRollupLeaseOwner)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer func() { _ = r.releaseLease(context.Background(), timezone, groupUsageRollupLeaseOwner) }()
	var bootstrapFrom *time.Time
	var state string
	if err := r.db.QueryRowContext(ctx, `SELECT bootstrap_from, bootstrap_state FROM usage_group_rollup_timezones WHERE timezone=$1`, timezone).Scan(&bootstrapFrom, &state); err != nil {
		return err
	}
	loc, _ := time.LoadLocation(timezone)
	// Truncate works on the absolute instant and therefore does not produce
	// local midnight outside UTC. Build the bucket boundary in the target
	// location so Asia/Shanghai and DST zones use the correct calendar day.
	end := localDayStart(now, loc)
	var groupIDs []int64
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM groups ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		groupIDs = append(groupIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if state != "ready" {
		start := end
		if bootstrapFrom != nil {
			start = time.Date(bootstrapFrom.In(loc).Year(), bootstrapFrom.In(loc).Month(), bootstrapFrom.In(loc).Day(), 0, 0, 0, 0, loc)
		}
		for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := r.renewLease(ctx, timezone, groupUsageRollupLeaseOwner); err != nil {
				return err
			}
			for _, groupID := range groupIDs {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := r.RecalculateBucket(ctx, groupID, timezone, day); err != nil {
					_, _ = r.db.ExecContext(ctx, `UPDATE usage_group_rollup_timezones SET bootstrap_state='degraded' WHERE timezone=$1`, timezone)
					return err
				}
			}
		}
	} else {
		// Always refresh today, while historical corrections are driven by the
		// trigger-maintained invalidation table.
		invalidated, err := r.listInvalidatedBuckets(ctx, timezone)
		if err != nil {
			return err
		}
		seen := make(map[string]struct{}, len(invalidated))
		for _, item := range invalidated {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := fmt.Sprintf("%d:%s", item.groupID, item.bucket.Format("2006-01-02"))
			seen[key] = struct{}{}
			if err := r.RecalculateBucket(ctx, item.groupID, timezone, item.bucket); err != nil {
				return err
			}
		}
		for _, groupID := range groupIDs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := r.renewLease(ctx, timezone, groupUsageRollupLeaseOwner); err != nil {
				return err
			}
			key := fmt.Sprintf("%d:%s", groupID, end.Format("2006-01-02"))
			if _, ok := seen[key]; ok {
				continue
			}
			if err := r.RecalculateBucket(ctx, groupID, timezone, end); err != nil {
				return err
			}
		}
	}
	_, err = r.db.ExecContext(ctx, `UPDATE usage_group_rollup_timezones SET bootstrap_state='ready', ready_at=NOW() WHERE timezone=$1`, timezone)
	return err
}

func (r *GroupUsageRollupRepository) acquireLease(ctx context.Context, timezone, owner string) (bool, error) {
	var acquiredOwner string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO usage_group_rollup_leases(timezone, lease_owner, lease_until, updated_at)
		VALUES ($1, $2, NOW() + ($3 * INTERVAL '1 second'), NOW())
		ON CONFLICT (timezone) DO UPDATE
		SET lease_owner=EXCLUDED.lease_owner,
		    lease_until=EXCLUDED.lease_until,
		    revision=usage_group_rollup_leases.revision + 1,
		    updated_at=NOW()
		WHERE usage_group_rollup_leases.lease_until IS NULL
		   OR usage_group_rollup_leases.lease_until < NOW()
		   OR usage_group_rollup_leases.lease_owner = EXCLUDED.lease_owner
		RETURNING lease_owner`, timezone, owner, int64(groupUsageRollupLeaseDuration/time.Second)).Scan(&acquiredOwner)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && acquiredOwner == owner, err
}

func (r *GroupUsageRollupRepository) renewLease(ctx context.Context, timezone, owner string) (bool, error) {
	var renewed bool
	err := r.db.QueryRowContext(ctx, `
		UPDATE usage_group_rollup_leases
		SET lease_until=NOW() + ($3 * INTERVAL '1 second'), updated_at=NOW(), revision=revision+1
		WHERE timezone=$1 AND lease_owner=$2 AND lease_until > NOW()
		RETURNING TRUE`, timezone, owner, int64(groupUsageRollupLeaseDuration/time.Second)).Scan(&renewed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return renewed, err
}

func (r *GroupUsageRollupRepository) releaseLease(ctx context.Context, timezone, owner string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE usage_group_rollup_leases
		SET lease_owner=NULL, lease_until=NULL, updated_at=NOW(), revision=revision+1
		WHERE timezone=$1 AND lease_owner=$2`, timezone, owner)
	return err
}

func localDayStart(now time.Time, loc *time.Location) time.Time {
	localNow := now.In(loc)
	return time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
}

type invalidatedBucket struct {
	groupID int64
	bucket  time.Time
}

func (r *GroupUsageRollupRepository) listInvalidatedBuckets(ctx context.Context, timezone string) ([]invalidatedBucket, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT group_id, bucket_date FROM usage_group_rollup_invalidations WHERE timezone=$1 ORDER BY bucket_date, group_id`, timezone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []invalidatedBucket
	for rows.Next() {
		var item invalidatedBucket
		if err := rows.Scan(&item.groupID, &item.bucket); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RecalculateBucket rebuilds both rollup levels for one group/date in a transaction.
func (r *GroupUsageRollupRepository) RecalculateBucket(ctx context.Context, groupID int64, timezone string, bucketDate time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return fmt.Errorf("load rollup timezone %q: %w", timezone, err)
	}
	localDate := bucketDate.In(loc)
	startLocal := time.Date(localDate.Year(), localDate.Month(), localDate.Day(), 0, 0, 0, 0, loc)
	start := startLocal.UTC()
	end := startLocal.AddDate(0, 0, 1).UTC()
	args := []any{groupID, timezone, bucketDate, start, end}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_group_daily_rollups WHERE group_id=$1 AND timezone=$2 AND bucket_date=$3`, groupID, timezone, bucketDate); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_group_daily_rollups(group_id,timezone,bucket_date,request_count,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,source_max_created_at,revision)
		SELECT $1,$2,$3,COUNT(*),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_creation_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(total_cost),0),COALESCE(SUM(actual_cost),0),MAX(created_at),1 FROM usage_logs WHERE group_id=$1 AND created_at >= $4 AND created_at < $5`, args...)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_group_model_daily_rollups WHERE group_id=$1 AND timezone=$2 AND bucket_date=$3`, groupID, timezone, bucketDate); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_group_model_daily_rollups(group_id,model,timezone,bucket_date,request_count,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,source_max_created_at,revision)
		SELECT $1,model,$2,$3,COUNT(*),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_creation_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(total_cost),0),COALESCE(SUM(actual_cost),0),MAX(created_at),1 FROM usage_logs WHERE group_id=$1 AND created_at >= $4 AND created_at < $5 GROUP BY model`, args...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_group_rollup_invalidations WHERE group_id=$1 AND timezone=$2 AND bucket_date=$3`, groupID, timezone, bucketDate)
	if err != nil {
		return err
	}
	return tx.Commit()
}
