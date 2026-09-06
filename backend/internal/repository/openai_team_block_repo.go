package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// OpenAITeamBlockRepository persists the cross-instance Team block truth.
type OpenAITeamBlockRepository struct{ db *sql.DB }

func NewOpenAITeamBlockRepository(db *sql.DB) *OpenAITeamBlockRepository {
	return &OpenAITeamBlockRepository{db: db}
}

// lockOpenAITeam serializes every state transition for one Team across all
// application instances. The advisory lock is scoped to the surrounding
// transaction and releases automatically on commit or rollback.
func lockOpenAITeam(ctx context.Context, tx *sql.Tx, teamID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, teamID)
	return err
}

// BlockTeamAtomically creates the idempotent trigger event, pauses every
// active account with the same ChatGPT team key, and emits one scheduler event
// for each changed account in one transaction.
func (r *OpenAITeamBlockRepository) BlockTeamAtomically(ctx context.Context, teamID, requestID string, triggerAccountID int64, until time.Time) (bool, error) {
	if r == nil || r.db == nil || teamID == "" || requestID == "" {
		return false, fmt.Errorf("team_id and request_id are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockOpenAITeam(ctx, tx, teamID); err != nil {
		return false, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO openai_team_block_events(team_id,trigger_request_id,trigger_account_id,block_until) VALUES($1,$2,$3,$4) ON CONFLICT(team_id,trigger_request_id) DO NOTHING RETURNING id`, teamID, requestID, triggerAccountID, until).Scan(&id)
	if err == sql.ErrNoRows {
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE accounts SET temp_unschedulable_until=$1,temp_unschedulable_reason='team_workspace_deactivated',updated_at=NOW() WHERE deleted_at IS NULL AND credentials->>'chatgpt_account_id'=$2 AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<$1) RETURNING id`, until, teamID)
	if err != nil {
		return false, err
	}
	accountIDs, err := collectReturnedAccountIDs(rows)
	if err != nil {
		return false, err
	}
	if err := enqueueAccountChangedOutbox(ctx, tx, accountIDs); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListDueProbeTargets advances expired Team blocks to probe_due and returns
// one linked account per due Team. It is intentionally independent of account
// test plans so every blocked workspace gets an automatic TTL recovery probe.
func (r *OpenAITeamBlockRepository) ListDueProbeTargets(ctx context.Context, limit int) ([]service.OpenAITeamProbeTarget, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("team block repository is unavailable")
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := r.db.QueryContext(ctx, `WITH transitioned AS (
		UPDATE openai_team_block_events
		SET state='probe_due', updated_at=NOW()
		WHERE state='blocked' AND block_until<=NOW()
		RETURNING team_id
	), due_teams AS (
		SELECT DISTINCT ON (event.team_id) event.team_id
		FROM openai_team_block_events AS event
		WHERE event.state='probe_due' OR (event.state='probing' AND event.probe_until<NOW())
		ORDER BY event.team_id, event.updated_at, event.id
	)
	SELECT due.team_id, account.id
	FROM due_teams AS due
	JOIN LATERAL (
		SELECT id FROM accounts
		WHERE deleted_at IS NULL AND credentials->>'chatgpt_account_id'=due.team_id
		ORDER BY id
		LIMIT 1
	) AS account ON TRUE
	ORDER BY due.team_id
	LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]service.OpenAITeamProbeTarget, 0)
	for rows.Next() {
		var target service.OpenAITeamProbeTarget
		if err := rows.Scan(&target.TeamID, &target.AccountID); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// HasActiveBlock reports whether a Team still has durable block state. It is
// used to reject unsafe single-account recovery attempts.
func (r *OpenAITeamBlockRepository) HasActiveBlock(ctx context.Context, teamID string) (bool, error) {
	if r == nil || r.db == nil || strings.TrimSpace(teamID) == "" {
		return false, nil
	}
	var active bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openai_team_block_events WHERE team_id=$1 AND state<>'cleared')`, teamID).Scan(&active); err != nil {
		return false, err
	}
	return active, nil
}

// GetActiveBlockStatus returns the current Team state together with the linked
// account IDs that remain protected by it.
func (r *OpenAITeamBlockRepository) GetActiveBlockStatus(ctx context.Context, teamID string) (*service.OpenAITeamBlockStatus, error) {
	if r == nil || r.db == nil || strings.TrimSpace(teamID) == "" {
		return nil, nil
	}
	status := &service.OpenAITeamBlockStatus{TeamID: strings.TrimSpace(teamID)}
	if err := r.db.QueryRowContext(ctx, `SELECT state,block_until FROM openai_team_block_events WHERE team_id=$1 AND state<>'cleared' ORDER BY id DESC LIMIT 1`, status.TeamID).Scan(&status.State, &status.BlockUntil); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM accounts WHERE deleted_at IS NULL AND credentials->>'chatgpt_account_id'=$1 ORDER BY id`, status.TeamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, err
		}
		status.AffectedAccountIDs = append(status.AffectedAccountIDs, accountID)
	}
	return status, rows.Err()
}

// ClaimDueProbe performs the cross-instance CAS that grants one worker the
// right to probe a Team after its block TTL has elapsed.
func (r *OpenAITeamBlockRepository) ClaimDueProbe(ctx context.Context, teamID, owner string, probeUntil time.Time) (*service.OpenAITeamProbeLease, error) {
	return r.claimProbe(ctx, teamID, owner, probeUntil, false)
}

// ClaimProbeNow is the administrator-controlled recovery action. It still
// acquires the same CAS lease and never clears an account without probe success.
func (r *OpenAITeamBlockRepository) ClaimProbeNow(ctx context.Context, teamID, owner string, probeUntil time.Time) (*service.OpenAITeamProbeLease, error) {
	return r.claimProbe(ctx, teamID, owner, probeUntil, true)
}

func (r *OpenAITeamBlockRepository) claimProbe(ctx context.Context, teamID, owner string, probeUntil time.Time, force bool) (*service.OpenAITeamProbeLease, error) {
	if r == nil || r.db == nil || teamID == "" || owner == "" {
		return nil, fmt.Errorf("team_id and owner are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockOpenAITeam(ctx, tx, teamID); err != nil {
		return nil, err
	}
	statePredicate := `(state='probe_due' OR (state='blocked' AND block_until<=NOW()) OR (state='probing' AND probe_until<NOW()))`
	if force {
		statePredicate = `(state IN ('blocked','probe_due') OR (state='probing' AND probe_until<NOW()))`
	}
	// A successful probe may clear every event in its captured generation. Do
	// not claim an older due event when a newer active 402 event already exists:
	// that newer event represents a later workspace deactivation and must remain
	// blocked until its own recovery probe succeeds.
	query := `WITH candidate AS (SELECT event.id FROM openai_team_block_events AS event WHERE event.team_id=$1 AND ` + statePredicate + ` AND NOT EXISTS (SELECT 1 FROM openai_team_block_events AS active_probe WHERE active_probe.team_id=$1 AND active_probe.state='probing' AND active_probe.probe_until>=NOW()) AND NOT EXISTS (SELECT 1 FROM openai_team_block_events AS newer_event WHERE newer_event.team_id=$1 AND newer_event.id>event.id AND newer_event.state<>'cleared') ORDER BY event.block_until,event.id FOR UPDATE SKIP LOCKED LIMIT 1), generation AS (SELECT COALESCE(MAX(id),0) AS max_event_id FROM openai_team_block_events WHERE team_id=$1 AND state<>'cleared'), claimed AS (UPDATE openai_team_block_events AS event SET state='probing',probe_owner=$2,probe_until=$3,updated_at=NOW() FROM candidate WHERE event.id=candidate.id RETURNING event.id) SELECT claimed.id,generation.max_event_id FROM claimed CROSS JOIN generation`
	var id, maxEventID int64
	err = tx.QueryRowContext(ctx, query, teamID, owner, probeUntil).Scan(&id, &maxEventID)
	if err == sql.ErrNoRows {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &service.OpenAITeamProbeLease{EventID: id, MaxEventID: maxEventID, TeamID: teamID, Owner: owner}, nil
}

// ClearTeamAfterProbe clears a successfully probed Team and all of its
// account-level scheduling pauses in one transaction.
func (r *OpenAITeamBlockRepository) ClearTeamAfterProbe(ctx context.Context, lease service.OpenAITeamProbeLease) (bool, error) {
	if r == nil || r.db == nil || lease.EventID <= 0 || lease.MaxEventID <= 0 || lease.TeamID == "" || lease.Owner == "" {
		return false, fmt.Errorf("team_id and owner are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockOpenAITeam(ctx, tx, lease.TeamID); err != nil {
		return false, err
	}
	var eventID int64
	if err := tx.QueryRowContext(ctx, `UPDATE openai_team_block_events SET state='cleared',cleared_at=NOW(),probe_owner=NULL,probe_until=NULL,updated_at=NOW() WHERE id=$1 AND team_id=$2 AND state='probing' AND probe_owner=$3 RETURNING id`, lease.EventID, lease.TeamID, lease.Owner).Scan(&eventID); err == sql.ErrNoRows {
		return false, tx.Commit()
	} else if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE openai_team_block_events SET state='cleared',cleared_at=NOW(),probe_owner=NULL,probe_until=NULL,updated_at=NOW() WHERE team_id=$1 AND id<=$2 AND state<>'cleared'`, lease.TeamID, lease.MaxEventID); err != nil {
		return false, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM openai_team_block_events WHERE team_id=$1 AND state<>'cleared'`, lease.TeamID).Scan(&active); err != nil {
		return false, err
	}
	if active > 0 {
		return false, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `UPDATE accounts SET temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,updated_at=NOW() WHERE deleted_at IS NULL AND credentials->>'chatgpt_account_id'=$1 AND temp_unschedulable_reason='team_workspace_deactivated' RETURNING id`, lease.TeamID)
	if err != nil {
		return false, err
	}
	accountIDs, err := collectReturnedAccountIDs(rows)
	if err != nil {
		return false, err
	}
	if err := enqueueAccountChangedOutbox(ctx, tx, accountIDs); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ReblockTeamAfterProbe returns a failed probe to the blocked state and
// extends every linked account's pause atomically.
func (r *OpenAITeamBlockRepository) ReblockTeamAfterProbe(ctx context.Context, lease service.OpenAITeamProbeLease, until time.Time) (bool, error) {
	if r == nil || r.db == nil || lease.EventID <= 0 || lease.MaxEventID <= 0 || lease.TeamID == "" || lease.Owner == "" {
		return false, fmt.Errorf("team_id and owner are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockOpenAITeam(ctx, tx, lease.TeamID); err != nil {
		return false, err
	}
	var eventID int64
	if err := tx.QueryRowContext(ctx, `UPDATE openai_team_block_events SET state='blocked',block_until=$4,probe_owner=NULL,probe_until=NULL,updated_at=NOW() WHERE id=$1 AND team_id=$2 AND state='probing' AND probe_owner=$3 RETURNING id`, lease.EventID, lease.TeamID, lease.Owner, until).Scan(&eventID); err == sql.ErrNoRows {
		return false, tx.Commit()
	} else if err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE accounts SET temp_unschedulable_until=$1,temp_unschedulable_reason='team_workspace_deactivated',updated_at=NOW() WHERE deleted_at IS NULL AND credentials->>'chatgpt_account_id'=$2 AND temp_unschedulable_reason='team_workspace_deactivated' RETURNING id`, until, lease.TeamID)
	if err != nil {
		return false, err
	}
	accountIDs, err := collectReturnedAccountIDs(rows)
	if err != nil {
		return false, err
	}
	if err := enqueueAccountChangedOutbox(ctx, tx, accountIDs); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// collectReturnedAccountIDs consumes and closes UPDATE ... RETURNING rows
// before another statement is sent through the transaction. lib/pq streams
// rows on the PostgreSQL connection, so interleaving an outbox INSERT while
// those rows are still open corrupts the transaction protocol.
func collectReturnedAccountIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	accountIDs := make([]int64, 0)
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return accountIDs, nil
}

func enqueueAccountChangedOutbox(ctx context.Context, tx *sql.Tx, accountIDs []int64) error {
	for _, accountID := range accountIDs {
		if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
