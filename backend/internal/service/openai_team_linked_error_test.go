package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAITeamBlockStoreStub struct {
	calls     int
	teamID    string
	requestID string
	accountID int64
	until     time.Time
	cleared   bool
	active    bool
	activeErr error
	blockErr  error
}

func (s *openAITeamBlockStoreStub) BlockTeamAtomically(_ context.Context, teamID, requestID string, accountID int64, until time.Time) (bool, error) {
	s.calls++
	s.teamID = teamID
	s.requestID = requestID
	s.accountID = accountID
	s.until = until
	return true, s.blockErr
}

func (s *openAITeamBlockStoreStub) ListDueProbeTargets(context.Context, int) ([]OpenAITeamProbeTarget, error) {
	return nil, nil
}

func (s *openAITeamBlockStoreStub) HasActiveBlock(context.Context, string) (bool, error) {
	return s.active, s.activeErr
}

func (s *openAITeamBlockStoreStub) GetActiveBlockStatus(context.Context, string) (*OpenAITeamBlockStatus, error) {
	return nil, nil
}

func (s *openAITeamBlockStoreStub) ClaimDueProbe(_ context.Context, teamID, owner string, _ time.Time) (*OpenAITeamProbeLease, error) {
	return &OpenAITeamProbeLease{EventID: 1, MaxEventID: 1, TeamID: teamID, Owner: owner}, nil
}

func (s *openAITeamBlockStoreStub) ClaimProbeNow(_ context.Context, teamID, owner string, _ time.Time) (*OpenAITeamProbeLease, error) {
	return &OpenAITeamProbeLease{EventID: 1, MaxEventID: 1, TeamID: teamID, Owner: owner}, nil
}

func (s *openAITeamBlockStoreStub) ClearTeamAfterProbe(context.Context, OpenAITeamProbeLease) (bool, error) {
	return s.cleared, nil
}

func (s *openAITeamBlockStoreStub) ReblockTeamAfterProbe(context.Context, OpenAITeamProbeLease, time.Time) (bool, error) {
	return true, nil
}

type openAITeamRuntimeBlockerStub struct{ clearedTeamID string }

func (*openAITeamRuntimeBlockerStub) BlockAccountScheduling(*Account, time.Time, string) {}
func (*openAITeamRuntimeBlockerStub) ClearAccountSchedulingBlock(int64)                  {}
func (s *openAITeamRuntimeBlockerStub) ClearOpenAITeamScheduling(teamID string) {
	s.clearedTeamID = teamID
}

func TestIsOpenAITeamWorkspaceDeactivated(t *testing.T) {
	require.True(t, IsOpenAITeamWorkspaceDeactivated(http.StatusPaymentRequired, []byte(`{"detail":{"code":"deactivated_workspace"}}`)))
	require.False(t, IsOpenAITeamWorkspaceDeactivated(http.StatusBadRequest, []byte(`{"detail":{"code":"deactivated_workspace"}}`)))
	require.False(t, IsOpenAITeamWorkspaceDeactivated(http.StatusPaymentRequired, []byte(`{"detail":{"code":"insufficient_quota"}}`)))
}

func TestOpenAIWSCtxPoolHandshakeTeamFailureReturnsFailoverContract(t *testing.T) {
	account := &Account{
		ID:       73,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "team-ctx-pool",
		},
	}
	dialErr := &openAIWSDialError{
		StatusCode:      http.StatusPaymentRequired,
		ResponseHeaders: http.Header{"X-Request-Id": []string{"ctx-pool-team-402"}},
		ResponseBody:    []byte(`{"detail":{"code":"deactivated_workspace"}}`),
		Err:             errors.New("handshake rejected"),
	}

	// The ctx_pool acquire boundary must preserve this error as an
	// UpstreamFailoverError. The outer gateway handler relies on this contract
	// for the one-time Team switch.
	gateway := &OpenAIGatewayService{}
	failover := gateway.openAIWSTeamDialFailover(context.Background(), account, dialErr)
	require.NotNil(t, failover)
	require.True(t, IsOpenAITeamWorkspaceDeactivated(failover.StatusCode, failover.ResponseBody))
	require.Equal(t, "ctx-pool-team-402", failover.ResponseHeaders.Get("X-Request-Id"))
	require.True(t, failover.ShouldRetryNextAccount())
	require.Equal(t, "team-ctx-pool", account.GetChatGPTAccountID())
}

func TestOpenAITeamBlockRequestID(t *testing.T) {
	h := make(http.Header)
	h.Set("X-Request-Id", " req-123 ")
	require.Equal(t, "req-123", openAITeamBlockRequestID(h, "team-a", 1, nil))
	require.NotEmpty(t, openAITeamBlockRequestID(http.Header{}, "team-a", 1, []byte(`{"detail":{"code":"deactivated_workspace"}}`)))
}

func TestRateLimitService_HandlesLinkedOpenAITeamWorkspaceBlock(t *testing.T) {
	store := &openAITeamBlockStoreStub{}
	svc := NewRateLimitService(nil, nil, nil, nil, nil)
	svc.SetOpenAITeamBlockStore(store)
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "team-42",
		},
	}
	headers := make(http.Header)
	headers.Set("x-request-id", "req-42")
	before := time.Now()
	require.True(t, svc.HandleUpstreamError(context.Background(), account, http.StatusPaymentRequired, headers, []byte(`{"detail":{"code":"deactivated_workspace"}}`)))
	require.Equal(t, 1, store.calls)
	require.Equal(t, "team-42", store.teamID)
	require.Equal(t, "req-42", store.requestID)
	require.Equal(t, int64(42), store.accountID)
	require.WithinDuration(t, before.Add(openAITeamBlockTTL), store.until, time.Second)
}

func TestRateLimitService_TeamBlockPrecedesLocalErrorPolicy(t *testing.T) {
	store := &openAITeamBlockStoreStub{}
	svc := NewRateLimitService(nil, nil, nil, nil, nil)
	svc.SetOpenAITeamBlockStore(store)
	account := &Account{
		ID:       43,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id":         "team-43",
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(http.StatusTooManyRequests)},
		},
	}

	require.True(t, svc.HandleUpstreamError(context.Background(), account, http.StatusPaymentRequired, http.Header{}, []byte(`{"detail":{"code":"deactivated_workspace"}}`)))
	require.Equal(t, 1, store.calls)
	require.Equal(t, "team-43", store.teamID)
}

func TestRateLimitService_HandledTeamBlockDoesNotFallThroughToLocalError(t *testing.T) {
	store := &openAITeamBlockStoreStub{}
	// A nil account repo is intentional: a handled Team event must return
	// before the normal 402 branch attempts a local SetError write.
	svc := NewRateLimitService(nil, nil, nil, nil, nil)
	svc.SetOpenAITeamBlockStore(store)
	account := &Account{
		ID:       44,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "team-44",
		},
	}
	body := []byte(`{"detail":{"code":"deactivated_workspace"}}`)
	require.True(t, svc.HandleOpenAITeamWorkspaceDeactivation(context.Background(), account, http.StatusPaymentRequired, http.Header{}, body))
	require.True(t, svc.HandleUpstreamError(MarkOpenAITeamWorkspaceDeactivationHandled(context.Background()), account, http.StatusPaymentRequired, http.Header{}, body))
	require.Equal(t, 1, store.calls)
}

func TestRateLimitService_CompleteOpenAITeamProbeClearsRuntimeTeamBlock(t *testing.T) {
	store := &openAITeamBlockStoreStub{cleared: true}
	runtimeBlocker := &openAITeamRuntimeBlockerStub{}
	svc := NewRateLimitService(nil, nil, nil, nil, nil)
	svc.SetOpenAITeamBlockStore(store)
	svc.SetAccountRuntimeBlocker(runtimeBlocker)
	require.NoError(t, svc.CompleteOpenAITeamProbe(context.Background(), &OpenAITeamProbeLease{EventID: 1, MaxEventID: 1, TeamID: "team-42", Owner: "worker"}, true))
	require.Equal(t, "team-42", runtimeBlocker.clearedTeamID)
}

func TestOpenAIGatewayService_TeamRuntimeBlockStaysClosedUntilSuccessfulProbe(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-42"}}
	gateway.BlockOpenAITeamScheduling("team-42", time.Now().Add(-time.Minute))
	require.True(t, gateway.isOpenAITeamRuntimeBlocked(account))
	gateway.ClearOpenAITeamScheduling("team-42")
	require.False(t, gateway.isOpenAITeamRuntimeBlocked(account))
}

func TestOpenAIGatewayService_TeamRuntimeBlockRechecksDurableState(t *testing.T) {
	store := &openAITeamBlockStoreStub{}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAITeamBlockStore(store)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimit}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-42"}}
	gateway.openaiTeamRuntimeBlockUntil.Store("team-42", time.Now().Add(-time.Second))
	require.False(t, gateway.isOpenAITeamRuntimeBlocked(account))
}

func TestOpenAIGatewayService_RejectsDurablyBlockedTeamDuringFinalSelection(t *testing.T) {
	store := &openAITeamBlockStoreStub{active: true}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAITeamBlockStore(store)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimit}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"chatgpt_account_id": "team-42"},
	}

	require.Nil(t, gateway.recheckSelectedOpenAIAccountFromDB(context.Background(), account, "", false))
}

func TestOpenAIGatewayService_DoesNotCreateTeamRuntimeBlockWithoutDurableState(t *testing.T) {
	store := &openAITeamBlockStoreStub{blockErr: errors.New("database unavailable"), activeErr: errors.New("database unavailable")}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAITeamBlockStore(store)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimit}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-42"}}
	require.True(t, gateway.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusPaymentRequired, http.Header{}, []byte(`{"detail":{"code":"deactivated_workspace"}}`)))
	_, found := gateway.openaiTeamRuntimeBlockUntil.Load("team-42")
	require.False(t, found)
}

func TestOpenAIWSHTTPBridge_PersistsTeamBlockBeforeFailover(t *testing.T) {
	store := &openAITeamBlockStoreStub{active: true}
	rateLimit := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimit.SetOpenAITeamBlockStore(store)
	gateway := &OpenAIGatewayService{
		rateLimitService: rateLimit,
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusPaymentRequired,
			Header:     http.Header{"X-Request-Id": []string{"ws-bridge-team-402"}},
			Body:       io.NopCloser(strings.NewReader(`{"detail":{"code":"deactivated_workspace"}}`)),
		}},
	}
	account := &Account{
		ID:          71,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "test-token",
			"chatgpt_account_id": "team-ws-bridge",
		},
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)

	result, err := gateway.proxyOpenAIWSHTTPBridgeTurn(
		context.Background(), c, account, "test-token", payload, len(payload),
		"gpt-5", "", "", "", "", 1, func([]byte) error { return nil },
	)

	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusPaymentRequired, failover.StatusCode)
	require.Equal(t, 1, store.calls)
	require.Equal(t, "team-ws-bridge", store.teamID)
	require.Equal(t, "ws-bridge-team-402", store.requestID)
	require.True(t, gateway.isOpenAITeamRuntimeBlocked(account))
}
