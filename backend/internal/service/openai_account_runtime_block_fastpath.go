package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	openAIAccountStateUpdateTimeout       = 5 * time.Second
	openAIOAuth429FallbackCooldown        = 5 * time.Second
	openAIStopSchedulingBridgeCooldown    = 2 * time.Minute
	openAIOAuth429StormWindow             = 10 * time.Second
	openAIOAuth429StormThreshold          = 20
	openAIOAuth429StormMaxAccountSwitches = 1
	openAITeamRuntimeBlockRecheckInterval = 30 * time.Second
)

func openAIAccountStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIAccountStateUpdateTimeout)
}

func isOpenAIOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth
}

// OpenAIOAuth429FailoverState gives a Grok OAuth 429 exactly one follow-up
// account attempt, even when the next selected account uses API-key auth.
type OpenAIOAuth429FailoverState struct {
	grokOAuth429FollowupPending bool
}

func isOpenAIAccount(account *Account) bool {
	return account != nil && (account.Platform == PlatformOpenAI || account.Platform == PlatformGrok)
}

func (s *OpenAIGatewayService) handleOpenAIAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, _ ...string) bool {
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(statusCode, responseBody) {
		return false
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()

	if statusCode == http.StatusTooManyRequests {
		s.markOpenAIOAuth429RateLimited(stateCtx, account, headers, responseBody)
	}
	if s == nil || account == nil || s.rateLimitService == nil {
		return false
	}
	shouldDisable := s.rateLimitService.HandleUpstreamError(stateCtx, account, statusCode, headers, responseBody)
	if shouldDisable {
		if (s.rateLimitService.settingService == nil || s.rateLimitService.settingService.IsOpenAITeamLinkedResolverEnabled(stateCtx)) && account.Platform == PlatformOpenAI && IsOpenAITeamWorkspaceDeactivated(statusCode, responseBody) && strings.TrimSpace(account.GetChatGPTAccountID()) != "" {
			teamID := account.GetChatGPTAccountID()
			active, err := s.rateLimitService.HasActiveOpenAITeamBlockForTeam(stateCtx, teamID)
			if err != nil {
				slog.Error("openai_team_workspace_durable_state_check_failed", "account_id", account.ID, "team_id", teamID, "error", err)
				s.BlockAccountScheduling(account, time.Now().Add(openAITeamRuntimeBlockRecheckInterval), "team_workspace_persistence_failed")
			} else if active {
				until := time.Now().Add(openAITeamBlockTTL)
				s.BlockOpenAITeamScheduling(teamID, until)
				s.BlockAccountScheduling(account, until, "team_workspace_deactivated")
			} else {
				// A duplicate response ID can point at an already-cleared event.
				// Do not recreate an unbounded Team cache entry from stale evidence.
				s.BlockAccountScheduling(account, time.Now().Add(openAITeamRuntimeBlockRecheckInterval), "team_workspace_stale_duplicate")
			}
		} else {
			s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
		}
	}
	return shouldDisable
}

func shouldCooldownOpenAITransientUpstreamError(statusCode int, responseBody []byte) bool {
	switch statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		return true
	case http.StatusBadRequest:
		return isOpenAITransientProcessingError(statusCode, "", responseBody)
	default:
		return false
	}
}

func canonicalOpenAIAccountSchedulingModel(account *Account, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
		return mapped
	}
	return model
}

func (s *OpenAIGatewayService) markOpenAIOAuth429RateLimited(ctx context.Context, account *Account, headers http.Header, responseBody []byte) {
	if s == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	s.recordOpenAIOAuth429()

	cooldownUntil := time.Now().Add(openAIOAuth429FallbackCooldown)
	if s.rateLimitService != nil {
		if resetAt := s.rateLimitService.calculateOpenAI429ResetTime(headers); resetAt != nil && resetAt.After(time.Now()) {
			cooldownUntil = *resetAt
		} else if resetUnix := parseOpenAIRateLimitResetTime(responseBody); resetUnix != nil {
			if resetAt := time.Unix(*resetUnix, 0); resetAt.After(time.Now()) {
				cooldownUntil = resetAt
			}
		} else if cooldown, ok := s.rateLimitService.get429FallbackCooldown(ctx, account); ok && cooldown > 0 {
			cooldownUntil = time.Now().Add(cooldown)
		}
	}
	s.BlockAccountScheduling(account, cooldownUntil, "429")
}

func (s *OpenAIGatewayService) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if s == nil || !isOpenAIAccount(account) {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	_, _ = s.blockAccountSchedulingLocked(account, until, reason)
}

func (s *OpenAIGatewayService) ClearAccountSchedulingBlock(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
}

func (s *OpenAIGatewayService) isOpenAIAccountRuntimeBlocked(account *Account) bool {
	if s == nil || !isOpenAIAccount(account) {
		return false
	}
	if s.isOpenAITeamRuntimeBlocked(account) {
		return true
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return false
	}
	cooldownUntil, ok := value.(time.Time)
	if !ok || cooldownUntil.IsZero() {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return false
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
	return false
}

// BlockOpenAITeamScheduling immediately protects all local scheduler
// snapshots while the database transaction and outbox propagate cross-instance.
func (s *OpenAIGatewayService) BlockOpenAITeamScheduling(teamID string, until time.Time) {
	if s == nil || strings.TrimSpace(teamID) == "" || until.IsZero() {
		return
	}
	teamID = strings.TrimSpace(teamID)
	recheckAt := time.Now().Add(openAITeamRuntimeBlockRecheckInterval)
	if current, ok := s.openaiTeamRuntimeBlockUntil.Load(teamID); ok {
		if currentUntil, ok := current.(time.Time); ok && currentUntil.After(recheckAt) {
			return
		}
	}
	s.openaiTeamRuntimeBlockUntil.Store(teamID, recheckAt)
}

// ClearOpenAITeamScheduling removes a durable Team block that has just passed
// a successful probe. The durable transaction is the source of truth; this
// only releases the local fast-path cache after that transaction committed.
func (s *OpenAIGatewayService) ClearOpenAITeamScheduling(teamID string) {
	if s == nil || strings.TrimSpace(teamID) == "" {
		return
	}
	s.openaiTeamRuntimeBlockUntil.Delete(strings.TrimSpace(teamID))
}

func (s *OpenAIGatewayService) isOpenAITeamRuntimeBlocked(account *Account) bool {
	if s == nil || account == nil {
		return false
	}
	teamID := strings.TrimSpace(account.GetChatGPTAccountID())
	if teamID == "" {
		return false
	}
	value, ok := s.openaiTeamRuntimeBlockUntil.Load(teamID)
	if !ok {
		return false
	}
	nextCheckAt, ok := value.(time.Time)
	if !ok {
		return true
	}
	if time.Now().Before(nextCheckAt) {
		return true
	}
	if s.rateLimitService == nil {
		return true
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), openAIAccountStateUpdateTimeout)
	defer cancel()
	active, err := s.rateLimitService.HasActiveOpenAITeamBlockForTeam(checkCtx, teamID)
	if err != nil {
		slog.Warn("openai_team_runtime_block_recheck_failed", "team_id", teamID, "error", err)
		s.openaiTeamRuntimeBlockUntil.Store(teamID, time.Now().Add(openAITeamRuntimeBlockRecheckInterval))
		return true
	}
	if !active {
		s.openaiTeamRuntimeBlockUntil.Delete(teamID)
		return false
	}
	s.openaiTeamRuntimeBlockUntil.Store(teamID, time.Now().Add(openAITeamRuntimeBlockRecheckInterval))
	return true
}

func (s *OpenAIGatewayService) recordOpenAIOAuth429() {
	if s == nil {
		return
	}
	now := time.Now()
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || now.Sub(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		if s.openaiOAuth429WindowStartUnixNano.CompareAndSwap(windowStart, now.UnixNano()) {
			s.openaiOAuth429WindowCount.Store(1)
			return
		}
	}
	s.openaiOAuth429WindowCount.Add(1)
}

func (s *OpenAIGatewayService) isOpenAIOAuth429Storm() bool {
	if s == nil {
		return false
	}
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || time.Since(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		return false
	}
	return s.openaiOAuth429WindowCount.Load() >= openAIOAuth429StormThreshold
}

func (s *OpenAIGatewayService) ShouldStopOpenAIOAuth429Failover(
	account *Account,
	statusCode int,
	failedSwitches int,
	state *OpenAIOAuth429FailoverState,
) bool {
	if failedSwitches < openAIOAuth429StormMaxAccountSwitches {
		return false
	}
	if state != nil && state.grokOAuth429FollowupPending {
		return true
	}
	if isGrokOAuthAccount(account) {
		if state == nil {
			return statusCode == http.StatusTooManyRequests && failedSwitches >= 2
		}
		if statusCode == http.StatusTooManyRequests {
			state.grokOAuth429FollowupPending = true
		}
		return false
	}
	if statusCode != http.StatusTooManyRequests || !isOpenAIOAuthAccount(account) {
		return false
	}
	return s.isOpenAIOAuth429Storm()
}
