package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type seedaceAccountRepoStub struct {
	AccountRepository

	list []Account
	byID map[int64]*Account
}

func (s *seedaceAccountRepoStub) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return append([]Account(nil), s.list...), nil
}

func (s *seedaceAccountRepoStub) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	return append([]Account(nil), s.list...), nil
}

func (s *seedaceAccountRepoStub) ListSchedulableByPlatform(context.Context, string) ([]Account, error) {
	return append([]Account(nil), s.list...), nil
}

func (s *seedaceAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if account := s.byID[id]; account != nil {
		copy := *account
		return &copy, nil
	}
	return nil, ErrAccountNotFound
}

type seedaceUsageLogRepoStub struct {
	UsageLogRepository

	taskLog         *UsageLog
	taskErr         error
	bestEffortCalls int
	lastLog         *UsageLog
}

func (s *seedaceUsageLogRepoStub) GetSeedaceVideoByTaskID(context.Context, int64, string) (*UsageLog, error) {
	if s.taskErr != nil {
		return nil, s.taskErr
	}
	if s.taskLog != nil {
		copy := *s.taskLog
		return &copy, nil
	}
	return nil, ErrUsageLogNotFound
}

func (s *seedaceUsageLogRepoStub) CreateBestEffort(_ context.Context, log *UsageLog) error {
	s.bestEffortCalls++
	s.lastLog = log
	return nil
}

type seedaceBillingRepoStub struct {
	UsageBillingRepository

	err error
}

type seedaceChannelRepoStub struct {
	ChannelRepository

	channels       []Channel
	groupPlatforms map[int64]string
}

func (s *seedaceChannelRepoStub) ListAll(context.Context) ([]Channel, error) {
	return append([]Channel(nil), s.channels...), nil
}

func (s *seedaceChannelRepoStub) GetGroupPlatforms(context.Context, []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(s.groupPlatforms))
	for groupID, platform := range s.groupPlatforms {
		result[groupID] = platform
	}
	return result, nil
}

func (s *seedaceChannelRepoStub) List(context.Context, pagination.PaginationParams, string, string) ([]Channel, *pagination.PaginationResult, error) {
	return append([]Channel(nil), s.channels...), &pagination.PaginationResult{}, nil
}

func (s *seedaceBillingRepoStub) Apply(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &UsageBillingApplyResult{Applied: true}, nil
}

type seedaceUpstreamStub struct {
	calls      []seedaceUpstreamCall
	bodyByPath map[string]string
}

type seedaceUpstreamCall struct {
	method        string
	url           string
	accountID     int64
	authorization string
}

func (s *seedaceUpstreamStub) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	s.calls = append(s.calls, seedaceUpstreamCall{
		method:        req.Method,
		url:           req.URL.String(),
		accountID:     accountID,
		authorization: req.Header.Get("Authorization"),
	})
	body := `{"data":{"task_id":"task-create"}}`
	if s.bodyByPath != nil {
		if configured, ok := s.bodyByPath[req.URL.Path]; ok {
			body = configured
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *seedaceUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestSeedaceVideoServicePollUsesCreateAccountFromTaskUsage(t *testing.T) {
	accountA := seedaceAccountForTest(101, "https://upstream-a.example/v1", "key-a")
	accountB := seedaceAccountForTest(202, "https://upstream-b.example/v1", "key-b")
	upstream := &seedaceUpstreamStub{bodyByPath: map[string]string{"/v1/video/generations/task-123": `{"data":{"video_url":"https://upstream-b.example/video.mp4"}}`}}
	svc := &SeedaceVideoService{
		accountRepo: &seedaceAccountRepoStub{
			list: []Account{accountA, accountB},
			byID: map[int64]*Account{
				accountA.ID: &accountA,
				accountB.ID: &accountB,
			},
		},
		usageLogRepo: &seedaceUsageLogRepoStub{taskLog: &UsageLog{AccountID: accountB.ID}},
		httpUpstream: upstream,
	}

	result, err := svc.Poll(context.Background(), SeedaceVideoPollInput{
		APIKey: &APIKey{ID: 11},
		TaskID: "task-123",
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.Len(t, upstream.calls, 1)
	require.Equal(t, accountB.ID, upstream.calls[0].accountID)
	require.Equal(t, "https://upstream-b.example/v1/video/generations/task-123", upstream.calls[0].url)
	require.Equal(t, "Bearer key-b", upstream.calls[0].authorization)
}

func TestSeedaceVideoServicePollFailsWhenStickyAccountUnavailable(t *testing.T) {
	accountA := seedaceAccountForTest(101, "https://upstream-a.example/v1", "key-a")
	accountB := seedaceAccountForTest(202, "https://upstream-b.example/v1", "key-b")
	accountB.Extra = map[string]any{"url_relay_enabled": false}
	upstream := &seedaceUpstreamStub{}
	svc := &SeedaceVideoService{
		accountRepo: &seedaceAccountRepoStub{
			list: []Account{accountA, accountB},
			byID: map[int64]*Account{
				accountA.ID: &accountA,
				accountB.ID: &accountB,
			},
		},
		usageLogRepo: &seedaceUsageLogRepoStub{taskLog: &UsageLog{AccountID: accountB.ID}},
		httpUpstream: upstream,
	}

	result, err := svc.Poll(context.Background(), SeedaceVideoPollInput{
		APIKey: &APIKey{ID: 11},
		TaskID: "task-123",
	})

	require.Nil(t, result)
	require.ErrorContains(t, err, "upstream account is no longer available")
	require.Empty(t, upstream.calls)
}

func TestSeedaceVideoServiceCreateRecordsUsageWhenBillingSucceeds(t *testing.T) {
	account := seedaceAccountForTest(303, "https://seedace.example/v1", "seedace-key")
	usageRepo := &seedaceUsageLogRepoStub{}
	upstream := &seedaceUpstreamStub{}
	groupID := int64(7)
	group := &Group{ID: groupID, Platform: PlatformSeedace, RateMultiplier: 1}
	perSecondPrice := 0.028
	apiKey := &APIKey{ID: 22, User: &User{ID: 33}, GroupID: &groupID, Group: group}
	svc := &SeedaceVideoService{
		accountRepo:      &seedaceAccountRepoStub{list: []Account{account}},
		usageLogRepo:     usageRepo,
		usageBillingRepo: &seedaceBillingRepoStub{},
		channelService: NewChannelService(&seedaceChannelRepoStub{
			groupPlatforms: map[int64]string{groupID: PlatformSeedace},
			channels: []Channel{{
				ID:       9,
				Status:   StatusActive,
				GroupIDs: []int64{groupID},
				ModelPricing: []ChannelModelPricing{{
					Platform:        PlatformSeedace,
					Models:          []string{"seedance-2.0"},
					BillingMode:     BillingModePerSecond,
					PerRequestPrice: &perSecondPrice,
				}},
			}},
		}, nil, nil, nil),
		billingCacheService: NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
		deferredService:     NewDeferredService(nil, nil, time.Second),
		httpUpstream:        upstream,
	}

	result, err := svc.Create(context.Background(), SeedaceVideoCreateInput{
		APIKey: apiKey,
		Body:   []byte(`{"model":"seedance-2.0","duration":4}`),
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.Equal(t, 1, usageRepo.bestEffortCalls)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, account.ID, usageRepo.lastLog.AccountID)
	require.Equal(t, apiKey.ID, usageRepo.lastLog.APIKeyID)
	require.Equal(t, "seedance-2.0", usageRepo.lastLog.Model)
	require.Equal(t, 0.112, usageRepo.lastLog.TotalCost)
	require.Equal(t, 0.112, usageRepo.lastLog.ActualCost)
	require.NotNil(t, usageRepo.lastLog.VideoTaskID)
	require.Equal(t, "task-create", *usageRepo.lastLog.VideoTaskID)
}

func TestSeedaceVideoServiceResolverDisabledUsesLegacyGroupVideoPrice(t *testing.T) {
	account := seedaceAccountForTest(303, "https://seedace.example/v1", "seedace-key")
	usageRepo := &seedaceUsageLogRepoStub{}
	groupID := int64(7)
	price := 0.03
	group := &Group{ID: groupID, Platform: PlatformSeedace, RateMultiplier: 2, VideoPrice720P: &price}
	settings := NewSettingService(&settingUpdateRepoStub{values: map[string]string{SettingKeySalesPricingResolverEnabled: "false"}}, nil)
	svc := &SeedaceVideoService{accountRepo: &seedaceAccountRepoStub{list: []Account{account}}, usageLogRepo: usageRepo, usageBillingRepo: &seedaceBillingRepoStub{}, billingService: NewBillingService(&config.Config{}, nil), modelPricingResolver: &ModelPricingResolver{}, settingService: settings, billingCacheService: NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil), deferredService: NewDeferredService(nil, nil, time.Second), httpUpstream: &seedaceUpstreamStub{}}

	_, err := svc.Create(context.Background(), SeedaceVideoCreateInput{APIKey: &APIKey{ID: 22, User: &User{ID: 33}, GroupID: &groupID, Group: group}, Body: []byte(`{"model":"seedance-2.0-720","duration":4}`)})
	require.NoError(t, err)
	require.InDelta(t, 0.12, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, 0.24, usageRepo.lastLog.ActualCost, 1e-12)
}

func TestSeedaceVideoServiceCreateReturnsUpstreamResultWhenBillingFails(t *testing.T) {
	account := seedaceAccountForTest(303, "https://seedace.example/v1", "seedace-key")
	usageRepo := &seedaceUsageLogRepoStub{}
	upstream := &seedaceUpstreamStub{}
	groupID := int64(7)
	group := &Group{ID: groupID, Platform: PlatformSeedace, RateMultiplier: 1}
	perSecondPrice := 0.028
	apiKey := &APIKey{ID: 22, User: &User{ID: 33}, GroupID: &groupID, Group: group}
	svc := &SeedaceVideoService{
		accountRepo:      &seedaceAccountRepoStub{list: []Account{account}},
		usageLogRepo:     usageRepo,
		usageBillingRepo: &seedaceBillingRepoStub{err: errors.New("billing database down")},
		channelService: NewChannelService(&seedaceChannelRepoStub{
			groupPlatforms: map[int64]string{groupID: PlatformSeedace},
			channels: []Channel{{
				ID:       9,
				Status:   StatusActive,
				GroupIDs: []int64{groupID},
				ModelPricing: []ChannelModelPricing{{
					Platform:        PlatformSeedace,
					Models:          []string{"seedance-2.0"},
					BillingMode:     BillingModePerSecond,
					PerRequestPrice: &perSecondPrice,
				}},
			}},
		}, nil, nil, nil),
		httpUpstream: upstream,
	}

	result, err := svc.Create(context.Background(), SeedaceVideoCreateInput{
		APIKey: apiKey,
		Body:   []byte(`{"model":"seedance-2.0","duration":4}`),
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.Contains(t, string(result.Body), "task-create")
	require.Len(t, upstream.calls, 1)
	require.Equal(t, account.ID, upstream.calls[0].accountID)
	require.Equal(t, 1, usageRepo.bestEffortCalls)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, account.ID, usageRepo.lastLog.AccountID)
	require.NotNil(t, usageRepo.lastLog.VideoTaskID)
	require.Equal(t, "task-create", *usageRepo.lastLog.VideoTaskID)
	require.Equal(t, 0.0, usageRepo.lastLog.ActualCost)
}

func TestParseSeedaceVideoRequestUsesOfficialSecondsField(t *testing.T) {
	req, err := parseSeedaceVideoRequest([]byte(`{"model":"seedance-2.0-720","seconds":"10"}`))

	require.NoError(t, err)
	model, durationSeconds := normalizeSeedaceVideoMeter(req)
	require.Equal(t, "seedance-2.0-720", model)
	require.Equal(t, 10, durationSeconds)
}

func TestParseSeedaceVideoRequestAcceptsNumericSeconds(t *testing.T) {
	req, err := parseSeedaceVideoRequest([]byte(`{"model":"seedance-2.0-720","seconds":10}`))

	require.NoError(t, err)
	_, durationSeconds := normalizeSeedaceVideoMeter(req)
	require.Equal(t, 10, durationSeconds)
}

func TestParseSeedaceVideoRequestSecondsOverridesDuration(t *testing.T) {
	req, err := parseSeedaceVideoRequest([]byte(`{"model":"seedance-2.0-720","duration":4,"seconds":"10"}`))

	require.NoError(t, err)
	_, durationSeconds := normalizeSeedaceVideoMeter(req)
	require.Equal(t, 10, durationSeconds)
}

func TestParseSeedaceVideoRequestEmptySecondsDoesNotOverrideDuration(t *testing.T) {
	for _, body := range []string{
		`{"model":"seedance-2.0-720","duration":10,"seconds":""}`,
		`{"model":"seedance-2.0-720","duration":10,"seconds":null}`,
	} {
		t.Run(body, func(t *testing.T) {
			req, err := parseSeedaceVideoRequest([]byte(body))

			require.NoError(t, err)
			_, durationSeconds := normalizeSeedaceVideoMeter(req)
			require.Equal(t, 10, durationSeconds)
		})
	}
}

func TestParseSeedaceVideoRequestRejectsInvalidSeconds(t *testing.T) {
	_, err := parseSeedaceVideoRequest([]byte(`{"model":"seedance-2.0-720","seconds":"bad"}`))

	require.ErrorContains(t, err, "seconds must be an integer string")
}

func seedaceAccountForTest(id int64, baseURL, apiKey string) Account {
	return Account{
		ID:       id,
		Platform: PlatformSeedace,
		Type:     domain.AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": baseURL,
			"api_key":  apiKey,
		},
		Extra: map[string]any{"url_relay_enabled": true},
	}
}
