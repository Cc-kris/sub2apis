//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBulkUpdateWithGroupsRollsBackAccountFieldsWhenBindingFails(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     "bulk-atomic-account",
		Priority: 10,
		Extra:    map[string]any{"before": true},
	})
	group, err := integrationEntClient.Group.Create().SetName("bulk-atomic-group").Save(ctx)
	require.NoError(t, err)

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	newPriority := 99
	_, err = repo.BulkUpdateWithGroups(ctx, []int64{account.ID}, service.AccountBulkUpdate{
		Priority: &newPriority,
		Extra:    map[string]any{"after": true},
	}, []int64{group.ID, 1 << 60})
	require.Error(t, err, "invalid group binding must abort the whole transaction")

	got, err := integrationEntClient.Account.Query().Where(dbaccount.IDEQ(account.ID)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, 10, got.Priority)
	require.Equal(t, true, got.Extra["before"])
	require.NotContains(t, got.Extra, "after")
	bindings, err := integrationEntClient.AccountGroup.Query().Where(accountgroup.AccountIDEQ(account.ID)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, bindings, "failed transaction must not leave a group binding")
}

func TestBulkUpdateWithGroupsRejectsMissingTargetAndRollsBack(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-missing-target", Priority: 1})
	group, err := integrationEntClient.Group.Create().SetName("bulk-live-target-group").Save(ctx)
	require.NoError(t, err)

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	priority := 7
	_, err = repo.BulkUpdateWithGroups(ctx, []int64{1 << 60, account.ID}, service.AccountBulkUpdate{Priority: &priority}, []int64{group.ID})
	require.ErrorIs(t, err, service.ErrBulkUpdateTargetInvalid)

	got, err := integrationEntClient.Account.Query().Where(dbaccount.IDEQ(account.ID)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, got.Priority)
}

func TestBulkUpdateWithGroupsRejectsInapplicableField(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-inapplicable", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Priority: 2})
	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	mode := service.OpenAIWSIngressModePassthrough
	_, err := repo.BulkUpdateWithGroups(ctx, []int64{account.ID}, service.AccountBulkUpdate{Extra: map[string]any{"openai_oauth_responses_websockets_v2_mode": mode}}, nil)
	require.ErrorIs(t, err, service.ErrBulkUpdateFieldNotApplicable)

	got, err := integrationEntClient.Account.Query().Where(dbaccount.IDEQ(account.ID)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, got.Priority)
}

func TestBulkUpdateWithGroupsWritesExpectedOutboxEvents(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-outbox", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Priority: 3})
	group, err := integrationEntClient.Group.Create().SetName("bulk-outbox-group").Save(ctx)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
	require.NoError(t, err)
	var before int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM scheduler_outbox").Scan(&before))

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	priority := 8
	updatedIDs, err := repo.BulkUpdateWithGroups(ctx, []int64{account.ID}, service.AccountBulkUpdate{Priority: &priority}, []int64{group.ID})
	require.NoError(t, err)
	require.Equal(t, []int64{account.ID}, updatedIDs)

	rows, err := integrationDB.QueryContext(ctx, `SELECT event_type, account_id, payload FROM scheduler_outbox WHERE id > $1 ORDER BY id`, before)
	require.NoError(t, err)
	defer rows.Close()
	var groupEvents, bulkEvents int
	for rows.Next() {
		var eventType string
		var accountID sql.NullInt64
		var payload []byte
		require.NoError(t, rows.Scan(&eventType, &accountID, &payload))
		switch eventType {
		case service.SchedulerOutboxEventAccountGroupsChanged:
			groupEvents++
			require.True(t, accountID.Valid)
			var body map[string]any
			require.NoError(t, json.Unmarshal(payload, &body))
			require.Equal(t, float64(group.ID), body["group_ids"].([]any)[0])
		case service.SchedulerOutboxEventAccountBulkChanged:
			bulkEvents++
		}
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 1, groupEvents)
	require.Equal(t, 1, bulkEvents)
}

func TestBulkUpdateWithGroupsNotifiesRemovedAndAddedGroups(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-group-move", Priority: 3})
	oldGroup, err := integrationEntClient.Group.Create().SetName("bulk-group-old").Save(ctx)
	require.NoError(t, err)
	newGroup, err := integrationEntClient.Group.Create().SetName("bulk-group-new").Save(ctx)
	require.NoError(t, err)
	_, err = integrationEntClient.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(oldGroup.ID).SetPriority(1).Save(ctx)
	require.NoError(t, err)
	var before int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM scheduler_outbox").Scan(&before))

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	_, err = repo.BulkUpdateWithGroups(ctx, []int64{account.ID}, service.AccountBulkUpdate{}, []int64{newGroup.ID})
	require.NoError(t, err)

	var payload []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT payload FROM scheduler_outbox
		WHERE id > $1 AND event_type = $2 AND account_id = $3
		ORDER BY id DESC LIMIT 1`, before, service.SchedulerOutboxEventAccountGroupsChanged, account.ID).Scan(&payload))
	var body map[string]any
	require.NoError(t, json.Unmarshal(payload, &body))
	groupIDs := body["group_ids"].([]any)
	require.ElementsMatch(t, []any{float64(oldGroup.ID), float64(newGroup.ID)}, groupIDs)
}

func TestBulkUpdateWithGroupsRejectsMixedPlatformsInSameBatch(t *testing.T) {
	ctx := context.Background()
	antigravity := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-mixed-antigravity", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth})
	anthropic := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-mixed-anthropic", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth})
	group, err := integrationEntClient.Group.Create().SetName("bulk-mixed-empty-group").Save(ctx)
	require.NoError(t, err)

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	_, err = repo.BulkUpdateWithGroupsValidated(ctx,
		[]int64{antigravity.ID, anthropic.ID}, service.AccountBulkUpdate{}, []int64{group.ID}, nil, true)
	var mixedErr *service.MixedChannelError
	require.ErrorAs(t, err, &mixedErr)

	count, err := integrationEntClient.AccountGroup.Query().Where(accountgroup.GroupIDEQ(group.ID)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestBulkUpdateWithGroupsSerializesConcurrentMixedPlatformBindings(t *testing.T) {
	ctx := context.Background()
	antigravity := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-concurrent-mixed-antigravity", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth})
	anthropic := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-concurrent-mixed-anthropic", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth})
	group, err := integrationEntClient.Group.Create().SetName("bulk-concurrent-mixed-group").Save(ctx)
	require.NoError(t, err)
	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)

	start := make(chan struct{})
	result := make(chan error, 2)
	for _, accountID := range []int64{antigravity.ID, anthropic.ID} {
		accountID := accountID
		go func() {
			<-start
			_, callErr := repo.BulkUpdateWithGroupsValidated(ctx, []int64{accountID}, service.AccountBulkUpdate{}, []int64{group.ID}, nil, true)
			result <- callErr
		}()
	}
	close(start)
	first, second := <-result, <-result
	require.True(t, (first == nil) != (second == nil), "exactly one competing platform may join the group")
	if first != nil {
		var mixedErr *service.MixedChannelError
		require.ErrorAs(t, first, &mixedErr)
	}
	if second != nil {
		var mixedErr *service.MixedChannelError
		require.ErrorAs(t, second, &mixedErr)
	}
}

func TestBulkUpdateWithGroupsRollsBackWhenOutboxWriteFails(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-outbox-failure", Priority: 6})
	group, err := integrationEntClient.Group.Create().SetName("bulk-outbox-failure-group").Save(ctx)
	require.NoError(t, err)
	_, err = integrationEntClient.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(group.ID).SetPriority(1).Save(ctx)
	require.NoError(t, err)

	triggerName := "bulk_update_outbox_fail_trigger"
	functionName := "bulk_update_outbox_fail"
	_, err = integrationDB.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION `+functionName+`() RETURNS trigger AS $$
		BEGIN
			IF NEW.account_id = `+strconv.FormatInt(account.ID, 10)+` THEN
				RAISE EXCEPTION 'forced scheduler outbox failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, "CREATE TRIGGER "+triggerName+" BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION "+functionName+"()")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS "+triggerName+" ON scheduler_outbox")
		_, _ = integrationDB.ExecContext(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	})

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	priority := 9
	_, err = repo.BulkUpdateWithGroups(ctx, []int64{account.ID}, service.AccountBulkUpdate{Priority: &priority}, []int64{})
	require.Error(t, err)

	got, err := integrationEntClient.Account.Query().Where(dbaccount.IDEQ(account.ID)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, 6, got.Priority)
	bindings, err := integrationEntClient.AccountGroup.Query().Where(accountgroup.AccountIDEQ(account.ID), accountgroup.GroupIDEQ(group.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, bindings)
}

func TestBulkUpdateWithGroupsRejectsConcurrentSoftDelete(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "bulk-concurrent-delete", Priority: 4})
	group, err := integrationEntClient.Group.Create().SetName("bulk-concurrent-delete-group").Save(ctx)
	require.NoError(t, err)
	lockTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = lockTx.ExecContext(ctx, "SELECT id FROM accounts WHERE id = $1 FOR UPDATE", account.ID)
	require.NoError(t, err)

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	resultCh := make(chan error, 1)
	go func() {
		_, callErr := repo.BulkUpdateWithGroups(ctx, []int64{account.ID}, service.AccountBulkUpdate{Priority: func() *int { v := 9; return &v }()}, []int64{group.ID})
		resultCh <- callErr
	}()
	time.Sleep(50 * time.Millisecond)
	_, err = lockTx.ExecContext(ctx, "UPDATE accounts SET deleted_at = NOW() WHERE id = $1", account.ID)
	require.NoError(t, err)
	require.NoError(t, lockTx.Commit())
	select {
	case callErr := <-resultCh:
		require.ErrorIs(t, callErr, service.ErrBulkUpdateTargetInvalid)
	case <-time.After(5 * time.Second):
		t.Fatal("bulk update did not finish after concurrent delete committed")
	}
}

func TestBulkUpdateWithGroupsRejectsConcurrentTypeChange(t *testing.T) {
	ctx := context.Background()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     "bulk-concurrent-type",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Priority: 5,
	})

	// Simulate a concurrent writer committing a type change between the
	// service snapshot and the repository transaction.
	_, err := integrationDB.ExecContext(ctx, "UPDATE accounts SET type = $1 WHERE id = $2", service.AccountTypeAPIKey, account.ID)
	require.NoError(t, err)

	repo := NewAccountRepository(integrationEntClient, integrationDB, nil).(*accountRepository)
	mode := service.OpenAIWSIngressModePassthrough
	_, err = repo.BulkUpdateWithGroupsValidated(
		ctx,
		[]int64{account.ID},
		service.AccountBulkUpdate{Extra: map[string]any{"openai_oauth_responses_websockets_v2_mode": mode}},
		nil,
		[]service.AccountBulkUpdateTarget{{ID: account.ID, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}},
		false,
	)
	require.ErrorIs(t, err, service.ErrBulkUpdateTargetInvalid)

	var gotType string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT type FROM accounts WHERE id = $1", account.ID).Scan(&gotType))
	require.Equal(t, service.AccountTypeAPIKey, gotType)
}
