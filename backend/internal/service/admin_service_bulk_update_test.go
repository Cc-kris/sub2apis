//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type accountRepoStubForBulkUpdate struct {
	accountRepoStub
	bulkUpdateErr     error
	bulkUpdateIDs     []int64
	bindGroupErrByID  map[int64]error
	bindGroupsCalls   []int64
	getByIDsAccounts  []*Account
	getByIDsErr       error
	getByIDsCalled    bool
	getByIDsIDs       []int64
	getByIDAccounts   map[int64]*Account
	getByIDErrByID    map[int64]error
	getByIDCalled     []int64
	listByGroupData   map[int64][]Account
	listByGroupErr    map[int64]error
	listData          []Account
	listResult        *pagination.PaginationResult
	listErr           error
	listCalled        bool
	lastListParams    pagination.PaginationParams
	lastBulkUpdate    AccountBulkUpdate
	upstreamAuditIDs  []int64
	updateCalls       int
	atomicUpdateCalls int
	lastListFilters   struct {
		platform    string
		accountType string
		status      string
		search      string
		groupID     int64
		privacyMode string
	}
}

type atomicAccountRepoStubForBulkUpdate struct {
	*accountRepoStubForBulkUpdate
	updatedIDs []int64
	err        error
}

func (s *atomicAccountRepoStubForBulkUpdate) BulkUpdateWithGroupsValidated(_ context.Context, ids []int64, _ AccountBulkUpdate, _ []int64, _ []AccountBulkUpdateTarget, _ bool) ([]int64, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.updatedIDs) == 0 {
		return append([]int64(nil), ids...), nil
	}
	return append([]int64(nil), s.updatedIDs...), nil
}

func TestAdminService_BulkUpdateAccounts_AtomicTargetConflictPropagates422(t *testing.T) {
	repo := &atomicAccountRepoStubForBulkUpdate{
		accountRepoStubForBulkUpdate: &accountRepoStubForBulkUpdate{},
		err:                          ErrBulkUpdateTargetInvalid,
	}
	svc := &adminServiceImpl{accountRepo: repo}
	priority := 8
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Priority:   &priority,
	})
	require.Nil(t, result)
	require.ErrorIs(t, err, ErrBulkUpdateTargetInvalid)
}

func (s *accountRepoStubForBulkUpdate) Update(_ context.Context, _ *Account) error {
	s.updateCalls++
	return nil
}

func (s *accountRepoStubForBulkUpdate) CreateWithUpstreamMultiplierAudit(_ context.Context, _ *Account, _ *int64, _ string) error {
	return nil
}

func (s *accountRepoStubForBulkUpdate) UpdateAccountWithUpstreamMultiplierAudit(_ context.Context, account *Account, _ *decimal.Decimal, newMultiplier decimal.Decimal, effectiveAt time.Time, _ *int64, _ string) error {
	if account == nil {
		return ErrAccountNilInput
	}
	s.atomicUpdateCalls++
	return s.UpdateUpstreamMultiplierWithAudit(context.Background(), account.ID, nil, newMultiplier, effectiveAt, nil, "")
}

func (s *accountRepoStubForBulkUpdate) UpdateUpstreamMultiplierWithAudit(_ context.Context, accountID int64, _ *decimal.Decimal, newMultiplier decimal.Decimal, effectiveAt time.Time, _ *int64, _ string) error {
	s.upstreamAuditIDs = append(s.upstreamAuditIDs, accountID)
	if account := s.getByIDAccounts[accountID]; account != nil {
		value := newMultiplier
		account.UpstreamCostMultiplier = &value
		account.UpstreamCostMultiplierUpdatedAt = &effectiveAt
	}
	return nil
}

func (s *accountRepoStubForBulkUpdate) ListUpstreamMultiplierChanges(_ context.Context, _ int64, _ pagination.PaginationParams) ([]AccountUpstreamMultiplierChange, *pagination.PaginationResult, error) {
	return nil, &pagination.PaginationResult{}, nil
}

func (s *accountRepoStubForBulkUpdate) BulkUpdate(_ context.Context, ids []int64, update AccountBulkUpdate) (int64, error) {
	s.bulkUpdateIDs = append([]int64{}, ids...)
	s.lastBulkUpdate = update
	if s.bulkUpdateErr != nil {
		return 0, s.bulkUpdateErr
	}
	return int64(len(ids)), nil
}

func (s *accountRepoStubForBulkUpdate) BulkUpdateWithGroupsValidated(_ context.Context, ids []int64, update AccountBulkUpdate, _ []int64, _ []AccountBulkUpdateTarget, _ bool) ([]int64, error) {
	s.bulkUpdateIDs = append([]int64{}, ids...)
	s.lastBulkUpdate = update
	if s.bulkUpdateErr != nil {
		return nil, s.bulkUpdateErr
	}
	for _, accountID := range ids {
		if err := s.bindGroupErrByID[accountID]; err != nil {
			return nil, err
		}
	}
	return append([]int64(nil), ids...), nil
}

func (s *accountRepoStubForBulkUpdate) BindGroups(_ context.Context, accountID int64, _ []int64) error {
	s.bindGroupsCalls = append(s.bindGroupsCalls, accountID)
	if err, ok := s.bindGroupErrByID[accountID]; ok {
		return err
	}
	return nil
}

func (s *accountRepoStubForBulkUpdate) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	s.getByIDsCalled = true
	s.getByIDsIDs = append([]int64{}, ids...)
	if s.getByIDsErr != nil {
		return nil, s.getByIDsErr
	}
	return s.getByIDsAccounts, nil
}

func (s *accountRepoStubForBulkUpdate) GetByID(_ context.Context, id int64) (*Account, error) {
	s.getByIDCalled = append(s.getByIDCalled, id)
	if err, ok := s.getByIDErrByID[id]; ok {
		return nil, err
	}
	if account, ok := s.getByIDAccounts[id]; ok {
		return account, nil
	}
	return nil, errors.New("account not found")
}

func (s *accountRepoStubForBulkUpdate) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	if err, ok := s.listByGroupErr[groupID]; ok {
		return nil, err
	}
	if rows, ok := s.listByGroupData[groupID]; ok {
		return rows, nil
	}
	return nil, nil
}

func (s *accountRepoStubForBulkUpdate) ListWithFilters(_ context.Context, params pagination.PaginationParams, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, *pagination.PaginationResult, error) {
	s.listCalled = true
	s.lastListParams = params
	s.lastListFilters.platform = platform
	s.lastListFilters.accountType = accountType
	s.lastListFilters.status = status
	s.lastListFilters.search = search
	s.lastListFilters.groupID = groupID
	s.lastListFilters.privacyMode = privacyMode
	if s.listErr != nil {
		return nil, nil, s.listErr
	}
	if s.listResult != nil {
		return s.listData, s.listResult, nil
	}
	return s.listData, &pagination.PaginationResult{Total: int64(len(s.listData))}, nil
}

// TestAdminService_BulkUpdateAccounts_AllSuccessIDs 验证批量更新成功时返回 success_ids/failed_ids。
func TestAdminService_BulkUpdateAccounts_AllSuccessIDs(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	schedulable := true
	input := &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1, 2, 3},
		Schedulable: &schedulable,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, 3, result.Success)
	require.Equal(t, 0, result.Failed)
	require.ElementsMatch(t, []int64{1, 2, 3}, result.SuccessIDs)
	require.Empty(t, result.FailedIDs)
	require.Len(t, result.Results, 3)
}

func TestAdminService_BulkUpdateAccounts_UpstreamMultiplierReportsSuccessUnchangedAndFailure(t *testing.T) {
	one := decimal.RequireFromString("1.0000")
	oneTwo := decimal.RequireFromString("1.2000")
	repo := &accountRepoStubForBulkUpdate{
		getByIDAccounts: map[int64]*Account{
			1: {ID: 1, UpstreamCostMultiplier: &one},
			2: {ID: 2, UpstreamCostMultiplier: &oneTwo},
		},
		getByIDErrByID: map[int64]error{3: errors.New("read failed")},
	}
	svc := &adminServiceImpl{accountRepo: repo}
	operatorID := int64(9)

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:                         []int64{1, 2, 3},
		UpstreamCostMultiplier:             &oneTwo,
		UpstreamCostMultiplierChangeReason: "supplier contract updated",
		OperatorID:                         &operatorID,
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, 1, result.Unchanged)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, []int64{1}, repo.upstreamAuditIDs)
	require.Equal(t, []string{"success", "unchanged", "failed"}, []string{
		result.Results[0].Status, result.Results[1].Status, result.Results[2].Status,
	})
}

func TestAdminService_UpdateAccountPersistsAccountAndMultiplierAuditAtomically(t *testing.T) {
	oldMultiplier := decimal.RequireFromString("1.0000")
	newMultiplier := decimal.RequireFromString("1.2500")
	account := &Account{
		ID: 77, Name: "before", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Credentials: map[string]any{}, Extra: map[string]any{},
		UpstreamCostMultiplier: &oldMultiplier,
	}
	repo := &accountRepoStubForBulkUpdate{getByIDAccounts: map[int64]*Account{77: account}}
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), 77, &UpdateAccountInput{
		Name:                               "after",
		UpstreamCostMultiplier:             &newMultiplier,
		UpstreamCostMultiplierChangeReason: "上游采购倍率调整",
	})

	require.NoError(t, err)
	require.Equal(t, 1, repo.atomicUpdateCalls)
	require.Equal(t, 0, repo.updateCalls, "must not persist the account before the audit transaction")
	require.Equal(t, "after", updated.Name)
	require.True(t, updated.UpstreamCostMultiplier.Equal(newMultiplier))
}

func TestAdminService_BulkUpdateAccounts_DropsDeprecatedUpstreamWarningExtra(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Extra: map[string]any{
			"upstream_prepaid_amount": 25.5,
			"upstream_warning_amount": 5.0,
			"upstream_notify_enabled": true,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Equal(t, 25.5, repo.lastBulkUpdate.Extra["upstream_prepaid_amount"])
	require.NotContains(t, repo.lastBulkUpdate.Extra, "upstream_warning_amount")
	require.NotContains(t, repo.lastBulkUpdate.Extra, "upstream_notify_enabled")
}

// TestAdminService_BulkUpdateAccounts_BindingFailureIsAtomic verifies that a
// failed binding aborts the entire request instead of returning partial IDs.
func TestAdminService_BulkUpdateAccounts_BindingFailureIsAtomic(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		bindGroupErrByID: map[int64]error{
			2: errors.New("bind failed"),
		},
	}
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: 10, Name: "g10"}},
	}

	groupIDs := []int64{10}
	schedulable := false
	input := &BulkUpdateAccountsInput{
		AccountIDs:            []int64{1, 2, 3},
		GroupIDs:              &groupIDs,
		Schedulable:           &schedulable,
		SkipMixedChannelCheck: true,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.Nil(t, result)
	require.EqualError(t, err, "bind failed")
}

func TestAdminService_BulkUpdateAccounts_AtomicPathReturnsAllTargetsInRequestOrder(t *testing.T) {
	base := &accountRepoStubForBulkUpdate{}
	repo := &atomicAccountRepoStubForBulkUpdate{accountRepoStubForBulkUpdate: base}
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: 10, Name: "g10"}},
	}
	groupIDs := []int64{10}
	priority := 8
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:            []int64{9, 7, 8},
		Priority:              &priority,
		GroupIDs:              &groupIDs,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.Equal(t, 3, result.Success)
	require.Equal(t, 0, result.Failed)
	require.Equal(t, []int64{9, 7, 8}, result.SuccessIDs)
	require.Empty(t, result.FailedIDs)
	require.Equal(t, []string{"success", "success", "success"}, []string{
		result.Results[0].Status, result.Results[1].Status, result.Results[2].Status,
	})
}

func TestAdminService_BulkUpdateAccounts_NilGroupRepoReturnsError(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	groupIDs := []int64{10}
	input := &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		GroupIDs:   &groupIDs,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "group repository not configured")
}

// TestAdminService_BulkUpdateAccounts_MixedChannelPreCheckBlocksOnExistingConflict verifies
// that the global pre-check detects a conflict with existing group members and returns an
// error before any DB write is performed.
func TestAdminService_BulkUpdateAccounts_DelegatesMixedChannelCheckToAtomicRepository(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		getByIDsAccounts: []*Account{
			{ID: 1, Platform: PlatformAntigravity},
		},
		// Group 10 already contains an Anthropic account.
		listByGroupData: map[int64][]Account{
			10: {{ID: 99, Platform: PlatformAnthropic}},
		},
	}
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo:   &groupRepoStubForAdmin{getByID: &Group{ID: 10, Name: "target-group"}},
	}

	groupIDs := []int64{10}
	input := &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		GroupIDs:   &groupIDs,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, result.SuccessIDs)
	// The concrete repository validates the post-lock group state; the service
	// must not revive an unsafe preflight write path.
	require.Empty(t, repo.bindGroupsCalls)
}

func TestAdminServiceBulkUpdateAccounts_ResolvesIDsFromFilters(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		listData: []Account{
			{ID: 7},
			{ID: 11},
		},
		listResult: &pagination.PaginationResult{Total: 2},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	schedulable := true
	input := &BulkUpdateAccountsInput{
		Schedulable: &schedulable,
	}

	filtersField := reflect.ValueOf(input).Elem().FieldByName("Filters")
	require.True(t, filtersField.IsValid(), "BulkUpdateAccountsInput should expose Filters for filter-target bulk update")
	require.Equal(t, reflect.Ptr, filtersField.Kind(), "BulkUpdateAccountsInput.Filters should be a pointer field")

	filtersValue := reflect.New(filtersField.Type().Elem())
	filtersValue.Elem().FieldByName("Platform").SetString(PlatformOpenAI)
	filtersValue.Elem().FieldByName("Type").SetString(AccountTypeOAuth)
	filtersValue.Elem().FieldByName("Status").SetString(StatusActive)
	filtersValue.Elem().FieldByName("Group").SetString("12")
	filtersValue.Elem().FieldByName("PrivacyMode").SetString(PrivacyModeCFBlocked)
	filtersValue.Elem().FieldByName("Search").SetString("bulk-target")
	filtersField.Set(filtersValue)

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err)
	require.True(t, repo.listCalled, "expected filter-target bulk update to resolve matching IDs via account list filters")
	require.Equal(t, PlatformOpenAI, repo.lastListFilters.platform)
	require.Equal(t, AccountTypeOAuth, repo.lastListFilters.accountType)
	require.Equal(t, StatusActive, repo.lastListFilters.status)
	require.Equal(t, "bulk-target", repo.lastListFilters.search)
	require.Equal(t, int64(12), repo.lastListFilters.groupID)
	require.Equal(t, PrivacyModeCFBlocked, repo.lastListFilters.privacyMode)
	require.Equal(t, []int64{7, 11}, repo.bulkUpdateIDs)
	require.Equal(t, 2, result.Success)
	require.Equal(t, 0, result.Failed)
	require.Equal(t, []int64{7, 11}, result.SuccessIDs)
}
