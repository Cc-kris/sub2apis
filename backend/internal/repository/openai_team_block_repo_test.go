package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAITeamBlockRepositoryBlockTeamAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewOpenAITeamBlockRepository(db)
	until := time.Now().Add(15 * time.Minute)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WithArgs("team-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO openai_team_block_events")).WithArgs("team-a", "req-a", int64(1), until).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE accounts SET temp_unschedulable_until")).WithArgs(until, "team-a").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WithArgs(service.SchedulerOutboxEventAccountChanged, int64(1), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WithArgs(service.SchedulerOutboxEventAccountChanged, int64(2), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	created, err := repo.BlockTeamAtomically(context.Background(), "team-a", "req-a", 1, until)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAITeamBlockRepositoryClaimDueProbe(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewOpenAITeamBlockRepository(db)
	probeUntil := time.Now().Add(time.Minute)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WithArgs("team-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("WITH candidate AS (SELECT event.id FROM openai_team_block_events AS event")).WithArgs("team-a", "worker-a", probeUntil).WillReturnRows(sqlmock.NewRows([]string{"id", "max_event_id"}).AddRow(1, 3))
	mock.ExpectCommit()
	claimed, err := repo.ClaimDueProbe(context.Background(), "team-a", "worker-a", probeUntil)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, int64(1), claimed.EventID)
	require.Equal(t, int64(3), claimed.MaxEventID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAITeamBlockRepositoryListDueProbeTargets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewOpenAITeamBlockRepository(db)
	mock.ExpectQuery(regexp.QuoteMeta("WITH transitioned AS (")).WithArgs(10).WillReturnRows(sqlmock.NewRows([]string{"team_id", "id"}).AddRow("team-a", 7))
	targets, err := repo.ListDueProbeTargets(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, []service.OpenAITeamProbeTarget{{TeamID: "team-a", AccountID: 7}}, targets)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAITeamBlockRepositoryClearTeamAfterProbe(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewOpenAITeamBlockRepository(db)
	mock.ExpectBegin()
	lease := service.OpenAITeamProbeLease{EventID: 1, MaxEventID: 3, TeamID: "team-a", Owner: "worker-a"}
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WithArgs("team-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE openai_team_block_events SET state='cleared'")).WithArgs(int64(1), "team-a", "worker-a").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE openai_team_block_events SET state='cleared'")).WithArgs("team-a", int64(3)).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM openai_team_block_events")).WithArgs("team-a").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE accounts SET temp_unschedulable_until=NULL")).WithArgs("team-a").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WithArgs(service.SchedulerOutboxEventAccountChanged, int64(1), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	cleared, err := repo.ClearTeamAfterProbe(context.Background(), lease)
	require.NoError(t, err)
	require.True(t, cleared)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAITeamBlockRepositoryClearTeamAfterProbeKeepsNewerActiveBlock(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewOpenAITeamBlockRepository(db)
	lease := service.OpenAITeamProbeLease{EventID: 1, MaxEventID: 1, TeamID: "team-a", Owner: "worker-a"}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WithArgs("team-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE openai_team_block_events SET state='cleared'")).WithArgs(int64(1), "team-a", "worker-a").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE openai_team_block_events SET state='cleared'")).WithArgs("team-a", int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM openai_team_block_events")).WithArgs("team-a").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectCommit()
	cleared, err := repo.ClearTeamAfterProbe(context.Background(), lease)
	require.NoError(t, err)
	require.False(t, cleared)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAITeamBlockRepositoryReblockTeamAfterProbe(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewOpenAITeamBlockRepository(db)
	until := time.Now().Add(15 * time.Minute)
	mock.ExpectBegin()
	lease := service.OpenAITeamProbeLease{EventID: 1, MaxEventID: 1, TeamID: "team-a", Owner: "worker-a"}
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WithArgs("team-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE openai_team_block_events SET state='blocked'")).WithArgs(int64(1), "team-a", "worker-a", until).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE accounts SET temp_unschedulable_until=$1")).WithArgs(until, "team-a").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WithArgs(service.SchedulerOutboxEventAccountChanged, int64(1), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	reblocked, err := repo.ReblockTeamAfterProbe(context.Background(), lease, until)
	require.NoError(t, err)
	require.True(t, reblocked)
	require.NoError(t, mock.ExpectationsWereMet())
}
