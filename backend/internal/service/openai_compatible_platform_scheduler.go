package service

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

const (
	openCodeSessionAffinityHeader = "X-Session-Affinity"
	openCodeSessionIDHeader       = "X-Session-Id"
	openCodeNativeSessionHeader   = "X-OpenCode-Session"
	codeBuddyConversationHeader   = "X-Conversation-ID"
)

func isOpenAICompatibleAccountEligibleForRequest(
	ctx context.Context,
	account *Account,
	platform string,
	requestedModel string,
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
) bool {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if account == nil || account.Platform != platform || !account.IsOpenAICompatible() || !account.IsSchedulableForModelWithContext(ctx, requestedModel) {
		return false
	}
	if account.IsGrok() {
		if paused, reason := shouldAutoPauseGrokAccountByQuota(account); paused {
			slog.Debug("grok_account_auto_paused_by_quota", "account_id", account.ID, "window", reason.window)
			return false
		}
	}
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		return false
	}
	if !account.SupportsOpenAIEndpointCapability(requiredCapability) {
		return false
	}
	return !requireCompact || openAICompactSupportTier(account) > 0
}

type openAIQuotaAutoPauseDecision struct {
	window      string
	threshold   float64
	utilization float64
}

func shouldAutoPauseGrokAccountByQuota(account *Account) (bool, openAIQuotaAutoPauseDecision) {
	if account == nil || !account.IsGrok() || account.Type != AccountTypeOAuth {
		return false, openAIQuotaAutoPauseDecision{}
	}
	snapshot, err := grokQuotaSnapshotFromExtra(account.Extra)
	if err != nil || snapshot == nil {
		return false, openAIQuotaAutoPauseDecision{}
	}
	now := time.Now()
	if grokQuotaSnapshotStaleForPause(snapshot, now) {
		return false, openAIQuotaAutoPauseDecision{}
	}
	if grokQuotaRetryAfterActive(snapshot, now) {
		return true, openAIQuotaAutoPauseDecision{window: "retry_after", threshold: 1, utilization: 1}
	}
	if paused, decision := shouldAutoPauseGrokQuotaWindow("requests", snapshot.Requests, now); paused {
		return true, decision
	}
	if paused, decision := shouldAutoPauseGrokQuotaWindow("tokens", snapshot.Tokens, now); paused {
		return true, decision
	}
	return false, openAIQuotaAutoPauseDecision{}
}

func grokQuotaRetryAfterActive(snapshot *xai.QuotaSnapshot, now time.Time) bool {
	if snapshot == nil || snapshot.RetryAfterSeconds == nil || *snapshot.RetryAfterSeconds <= 0 {
		return false
	}
	if strings.TrimSpace(snapshot.UpdatedAt) == "" {
		return true
	}
	updatedAt, err := parseTime(snapshot.UpdatedAt)
	if err != nil {
		return true
	}
	return now.Before(updatedAt.Add(time.Duration(*snapshot.RetryAfterSeconds) * time.Second))
}

func shouldAutoPauseGrokQuotaWindow(name string, window *xai.QuotaWindow, now time.Time) (bool, openAIQuotaAutoPauseDecision) {
	if window == nil || window.Limit == nil || window.Remaining == nil || *window.Limit <= 0 {
		return false, openAIQuotaAutoPauseDecision{}
	}
	if window.ResetUnix != nil && *window.ResetUnix > 0 && !now.Before(time.Unix(*window.ResetUnix, 0)) {
		return false, openAIQuotaAutoPauseDecision{}
	}
	utilization := float64(*window.Limit-*window.Remaining) / float64(*window.Limit)
	if *window.Remaining <= 0 || utilization >= 1 {
		return true, openAIQuotaAutoPauseDecision{window: name, threshold: 1, utilization: utilization}
	}
	return false, openAIQuotaAutoPauseDecision{}
}

func grokQuotaSnapshotStaleForPause(snapshot *xai.QuotaSnapshot, now time.Time) bool {
	if snapshot == nil || strings.TrimSpace(snapshot.UpdatedAt) == "" {
		return false
	}
	updatedAt, err := parseTime(snapshot.UpdatedAt)
	if err != nil {
		return false
	}
	return now.Sub(updatedAt) >= 2*time.Hour
}

func (s *OpenAIGatewayService) selectCompatiblePlatformAccount(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIEndpointCapability,
	platform string,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: openAIAccountScheduleLayerLoadBalance}
	var accounts []Account
	var err error
	if s.schedulerSnapshot != nil {
		accounts, _, err = s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, platform, false)
	} else if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		accounts, err = s.accountRepo.ListSchedulableByPlatform(ctx, platform)
	} else if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, platform)
	} else {
		accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, platform)
	}
	if err != nil {
		return nil, decision, err
	}

	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if _, excluded := excludedIDs[account.ID]; excluded {
			continue
		}
		if s.isOpenAIAccountRuntimeBlocked(account) || !isOpenAICompatibleAccountEligibleForRequest(ctx, account, platform, requestedModel, false, requiredCapability) {
			continue
		}
		if isDomesticOpenAICompatiblePlatform(platform) && s.cnQuotaService != nil {
			snapshot, snapshotErr := s.cnQuotaService.GetSnapshot(ctx, account, false)
			if snapshotErr == nil && cnProviderSnapshotBlocksScheduling(snapshot, time.Now()) {
				continue
			}
		}
		candidates = append(candidates, account)
	}
	decision.CandidateCount = len(candidates)
	if len(candidates) == 0 {
		return nil, decision, ErrNoAvailableAccounts
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		if candidates[i].LastUsedAt == nil {
			return candidates[j].LastUsedAt != nil
		}
		if candidates[j].LastUsedAt == nil {
			return false
		}
		return candidates[i].LastUsedAt.Before(*candidates[j].LastUsedAt)
	})

	for _, account := range candidates {
		acquired, acquireErr := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if acquireErr != nil || acquired == nil || !acquired.Acquired {
			continue
		}
		selection, selectionErr := s.newAcquiredSelectionResult(ctx, account, acquired.ReleaseFunc)
		if selectionErr != nil {
			return nil, decision, selectionErr
		}
		decision.SelectedAccountID = account.ID
		decision.SelectedAccountType = account.Type
		if sessionHash != "" {
			_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, account.ID, openaiStickySessionTTL)
		}
		return selection, decision, nil
	}
	return nil, decision, ErrNoAvailableAccounts
}

func cnProviderSnapshotBlocksScheduling(snapshot *CNProviderQuotaSnapshot, now time.Time) bool {
	if snapshot == nil || snapshot.State != "fresh" {
		return false
	}
	if snapshot.ExpiresAt != nil && !now.Before(*snapshot.ExpiresAt) {
		return false
	}
	if snapshot.Remaining != nil && *snapshot.Remaining <= 0 {
		if snapshot.ResetAt == nil || now.Before(*snapshot.ResetAt) {
			return true
		}
	}
	for _, name := range []string{"requests", "tokens"} {
		window, ok := snapshot.RateLimits[name]
		if !ok || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		if window.ResetAt == nil || now.Before(*window.ResetAt) {
			return true
		}
	}
	return false
}
