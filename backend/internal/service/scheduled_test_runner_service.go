package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const scheduledTestDefaultMaxWorkers = 10

// ScheduledTestRunnerService periodically scans due test plans and executes them.
type ScheduledTestRunnerService struct {
	planRepo       ScheduledTestPlanRepository
	scheduledSvc   *ScheduledTestService
	accountTestSvc *AccountTestService
	rateLimitSvc   *RateLimitService
	cfg            *config.Config

	cron      *cron.Cron
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewScheduledTestRunnerService creates a new runner.
func NewScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
) *ScheduledTestRunnerService {
	return &ScheduledTestRunnerService{
		planRepo:       planRepo,
		scheduledSvc:   scheduledSvc,
		accountTestSvc: accountTestSvc,
		rateLimitSvc:   rateLimitSvc,
		cfg:            cfg,
	}
}

// Start begins the cron ticker (every minute).
func (s *ScheduledTestRunnerService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		loc := time.Local
		if s.cfg != nil {
			if parsed, err := time.LoadLocation(s.cfg.Timezone); err == nil && parsed != nil {
				loc = parsed
			}
		}

		c := cron.New(cron.WithParser(scheduledTestCronParser), cron.WithLocation(loc))
		_, err := c.AddFunc("* * * * *", func() { s.runScheduled() })
		if err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] not started (invalid schedule): %v", err)
			return
		}
		s.cron = c
		s.cron.Start()
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] started (tick=every minute)")
	})
}

// Stop gracefully shuts down the cron scheduler.
func (s *ScheduledTestRunnerService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cron != nil {
			ctx := s.cron.Stop()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
				logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] cron stop timed out")
			}
		}
	})
}

func (s *ScheduledTestRunnerService) runScheduled() {
	// Delay 10s so execution lands at ~:10 of each minute instead of :00.
	time.Sleep(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s.runDueTeamProbes(ctx)

	now := time.Now()
	plans, err := s.planRepo.ListDue(ctx, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ListDue error: %v", err)
		return
	}
	if len(plans) == 0 {
		return
	}

	logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] found %d due plans", len(plans))

	sem := make(chan struct{}, scheduledTestDefaultMaxWorkers)
	var wg sync.WaitGroup

	for _, plan := range plans {
		sem <- struct{}{}
		wg.Add(1)
		go func(p *ScheduledTestPlan) {
			defer wg.Done()
			defer func() { <-sem }()
			s.runOnePlan(ctx, p)
		}(plan)
	}

	wg.Wait()
}

// runDueTeamProbes is a plan-independent recovery lane for OpenAI Team
// workspace blocks. A due workspace must be retried even when an operator has
// not created a scheduled test plan for any of its accounts.
func (s *ScheduledTestRunnerService) runDueTeamProbes(ctx context.Context) {
	if s == nil || s.rateLimitSvc == nil || s.accountTestSvc == nil {
		return
	}
	targets, err := s.rateLimitSvc.ListDueOpenAITeamProbeTargets(ctx, scheduledTestDefaultMaxWorkers)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ListDueOpenAITeamProbeTargets error: %v", err)
		return
	}
	for _, target := range targets {
		s.runOneTeamProbe(ctx, target)
	}
}

func (s *ScheduledTestRunnerService) runOneTeamProbe(ctx context.Context, target OpenAITeamProbeTarget) {
	if s.rateLimitSvc != nil && s.rateLimitSvc.settingService != nil && !s.rateLimitSvc.settingService.IsOpenAITeamLinkedResolverEnabled(ctx) {
		return
	}
	lease, err := s.rateLimitSvc.ClaimOpenAITeamProbe(ctx, target.AccountID, "team-ttl-probe")
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] team=%s claim failed: %v", target.TeamID, err)
		return
	}
	if lease == nil {
		return
	}
	result, err := s.accountTestSvc.RunTestBackground(ctx, target.AccountID, "")
	succeeded := err == nil && result != nil && result.Status == "success"
	if completeErr := s.rateLimitSvc.CompleteOpenAITeamProbe(ctx, lease, succeeded); completeErr != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] team=%s completion failed: %v", target.TeamID, completeErr)
	}
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] team=%s probe failed: %v", target.TeamID, err)
	}
}

func (s *ScheduledTestRunnerService) runOnePlan(ctx context.Context, plan *ScheduledTestPlan) {
	var teamProbe *OpenAITeamProbeLease
	if s.rateLimitSvc != nil && (s.rateLimitSvc.settingService == nil || s.rateLimitSvc.settingService.IsOpenAITeamLinkedResolverEnabled(ctx)) {
		var err error
		teamProbe, err = s.rateLimitSvc.ClaimOpenAITeamProbe(ctx, plan.AccountID, fmt.Sprintf("scheduled-test-%d", plan.ID))
		if err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d Team probe claim failed: %v", plan.ID, err)
			return
		}
		if teamProbe == nil {
			active, err := s.rateLimitSvc.HasActiveOpenAITeamBlock(ctx, plan.AccountID)
			if err != nil {
				logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d Team state check failed: %v", plan.ID, err)
				return
			}
			if shouldSkipScheduledTeamTest(active, teamProbe) {
				// A normal scheduled test is not a Team recovery probe. Do not let
				// an early successful test (especially auto_recover) clear only one
				// account before the Team TTL and CAS probe have completed.
				s.advancePlanAfterTeamBlock(ctx, plan)
				return
			}
		}
	}
	result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
	if err != nil {
		if teamProbe != nil {
			_ = s.rateLimitSvc.CompleteOpenAITeamProbe(ctx, teamProbe, false)
		}
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d RunTestBackground error: %v", plan.ID, err)
		return
	}
	if teamProbe != nil {
		if err := s.rateLimitSvc.CompleteOpenAITeamProbe(ctx, teamProbe, result.Status == "success"); err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d Team probe completion failed: %v", plan.ID, err)
		}
	}

	if err := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, err)
	}

	// Auto-recover account if test succeeded and auto_recover is enabled.
	if teamProbe == nil && result.Status == "success" && plan.AutoRecover {
		s.tryRecoverAccount(ctx, plan.AccountID, plan.ID)
	}

	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun error: %v", plan.ID, err)
		return
	}

	if err := s.planRepo.UpdateAfterRun(ctx, plan.ID, time.Now(), nextRun); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d UpdateAfterRun error: %v", plan.ID, err)
	}
}

func shouldSkipScheduledTeamTest(teamBlocked bool, teamProbe *OpenAITeamProbeLease) bool {
	return teamBlocked && teamProbe == nil
}

func (s *ScheduledTestRunnerService) advancePlanAfterTeamBlock(ctx context.Context, plan *ScheduledTestPlan) {
	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun after Team block error: %v", plan.ID, err)
		return
	}
	if err := s.planRepo.UpdateAfterRun(ctx, plan.ID, time.Now(), nextRun); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d advance after Team block error: %v", plan.ID, err)
	}
}

// tryRecoverAccount attempts to recover an account from recoverable runtime state.
func (s *ScheduledTestRunnerService) tryRecoverAccount(ctx context.Context, accountID int64, planID int64) {
	if s.rateLimitSvc == nil {
		return
	}

	recovery, err := s.rateLimitSvc.RecoverAccountAfterSuccessfulTest(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover failed: %v", planID, err)
		return
	}
	if recovery == nil {
		return
	}

	if recovery.ClearedError {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d recovered from error status", planID, accountID)
	}
	if recovery.ClearedRateLimit {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d cleared rate-limit/runtime state", planID, accountID)
	}
}
