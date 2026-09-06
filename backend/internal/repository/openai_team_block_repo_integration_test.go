//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestOpenAITeamBlockRepositoryPostgresTransaction verifies the UPDATE ...
// RETURNING rows are fully consumed before scheduler outbox inserts. This must
// execute against lib/pq/PostgreSQL: sqlmock cannot expose protocol failures
// caused by interleaving a second statement with unread query rows.
func TestOpenAITeamBlockRepositoryPostgresTransaction(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	teamID := fmt.Sprintf("team-postgres-%d", suffix)
	requestID := fmt.Sprintf("request-postgres-%d", suffix)
	accountIDs := make([]int64, 0, 2)
	for index := 0; index < 2; index++ {
		var accountID int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			INSERT INTO accounts(name,platform,type,credentials)
			VALUES($1,'openai','oauth',jsonb_build_object('chatgpt_account_id',$2::text))
			RETURNING id`, fmt.Sprintf("team-postgres-%d-%d", suffix, index), teamID).Scan(&accountID))
		accountIDs = append(accountIDs, accountID)
	}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=ANY($1)`, pq.Array(accountIDs))
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM openai_team_block_events WHERE team_id=$1`, teamID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=ANY($1)`, pq.Array(accountIDs))
	})

	repo := NewOpenAITeamBlockRepository(integrationDB)
	created, err := repo.BlockTeamAtomically(ctx, teamID, requestID, accountIDs[0], time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, created)

	var blockedAccounts, outboxEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM accounts
		WHERE id=ANY($1) AND temp_unschedulable_reason='team_workspace_deactivated'`, pq.Array(accountIDs)).Scan(&blockedAccounts))
	require.Equal(t, 2, blockedAccounts)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=ANY($1) AND event_type=$2`, pq.Array(accountIDs), service.SchedulerOutboxEventAccountChanged).Scan(&outboxEvents))
	require.Equal(t, 2, outboxEvents)

	var eventID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `UPDATE openai_team_block_events SET block_until=NOW()-INTERVAL '1 second' WHERE team_id=$1 RETURNING id`, teamID).Scan(&eventID))
	lease, err := repo.ClaimDueProbe(ctx, teamID, "postgres-integration", time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, eventID, lease.EventID)

	// account_changed events are deliberately deduplicated for one second;
	// wait beyond that window so this test proves the recovery outbox writes
	// are sent after the RETURNING rows have been closed.
	time.Sleep(schedulerOutboxDedupWindow + 20*time.Millisecond)
	cleared, err := repo.ClearTeamAfterProbe(ctx, *lease)
	require.NoError(t, err)
	require.True(t, cleared)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM accounts
		WHERE id=ANY($1) AND temp_unschedulable_reason IS NOT NULL`, pq.Array(accountIDs)).Scan(&blockedAccounts))
	require.Zero(t, blockedAccounts)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=ANY($1) AND event_type=$2`, pq.Array(accountIDs), service.SchedulerOutboxEventAccountChanged).Scan(&outboxEvents))
	require.Equal(t, 4, outboxEvents)
}

// TestOpenAITeamBlockRepositoryDoesNotClaimOlderGeneration proves that a
// recent 402 event cannot be cleared by a probe that was scheduled for an
// older, already-expired event. This is the interleaving between listing due
// targets and claiming a probe on a different application instance.
func TestOpenAITeamBlockRepositoryDoesNotClaimOlderGeneration(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	teamID := fmt.Sprintf("team-generation-%d", suffix)
	var accountID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO accounts(name,platform,type,credentials)
		VALUES($1,'openai','oauth',jsonb_build_object('chatgpt_account_id',$2::text))
		RETURNING id`, fmt.Sprintf("team-generation-%d", suffix), teamID).Scan(&accountID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=$1`, accountID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM openai_team_block_events WHERE team_id=$1`, teamID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	})

	repo := NewOpenAITeamBlockRepository(integrationDB)
	created, err := repo.BlockTeamAtomically(ctx, teamID, fmt.Sprintf("request-old-%d", suffix), accountID, time.Now().Add(-time.Second))
	require.NoError(t, err)
	require.True(t, created)
	created, err = repo.BlockTeamAtomically(ctx, teamID, fmt.Sprintf("request-new-%d", suffix), accountID, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, created)

	lease, err := repo.ClaimDueProbe(ctx, teamID, "generation-worker", time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Nil(t, lease)

	var active, probing int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE state<>'cleared'), COUNT(*) FILTER (WHERE state='probing')
		FROM openai_team_block_events WHERE team_id=$1`, teamID).Scan(&active, &probing))
	require.Equal(t, 2, active)
	require.Zero(t, probing)
}
