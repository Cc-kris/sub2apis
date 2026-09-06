package service

import (
	"context"
	"database/sql"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
)

// BuildInfo contains build information
type BuildInfo struct {
	Version   string
	BuildType string
}

// ProvidePricingService creates and initializes PricingService
func ProvidePricingService(cfg *config.Config, remoteClient PricingRemoteClient) (*PricingService, error) {
	svc := NewPricingService(cfg, remoteClient)
	if err := svc.Initialize(); err != nil {
		// Pricing service initialization failure should not block startup, use fallback prices
		println("[Service] Warning: Pricing service initialization failed:", err.Error())
	}
	return svc, nil
}

// ProvideCNProviderQuotaService wires the domestic provider quota resolver.
// It is intentionally optional at runtime: absent quota_url/balance_url values
// yield an explicit unknown/failed snapshot and never a fabricated zero.
func ProvideCNProviderQuotaService(accountRepo AccountRepository, httpUpstream HTTPUpstream, cfg *config.Config, tlsProfiles *TLSFingerprintProfileService) *CNProviderQuotaService {
	fetcher := NewHTTPCNProviderQuotaFetcher(httpUpstream, cfg, tlsProfiles)
	return NewCNProviderQuotaService(accountRepo, fetcher)
}

func ProvideCNProviderBalanceService(quota *CNProviderQuotaService) *CNProviderBalanceService {
	return NewCNProviderBalanceService(quota)
}

// ProvideUpdateService creates UpdateService with BuildInfo
func ProvideUpdateService(cache UpdateCache, githubClient GitHubReleaseClient, buildInfo BuildInfo) *UpdateService {
	return NewUpdateService(cache, githubClient, buildInfo.Version, buildInfo.BuildType)
}

func ProvideCacheStatsService(repo OpsRepository, cache GatewayCache, settingService *SettingService) *CacheStatsService {
	return NewCacheStatsService(repo).SetAdvancedDependencies(repo, cache, settingService)
}

func ProvideChannelService(repo ChannelRepository, groupRepo GroupRepository, authCacheInvalidator APIKeyAuthCacheInvalidator, pricingService *PricingService, settingService *SettingService) *ChannelService {
	svc := NewChannelService(repo, groupRepo, authCacheInvalidator, pricingService)
	svc.SetSettingService(settingService)
	return svc
}

func ProvideSeedaceVideoService(accountRepo AccountRepository, usageLogRepo UsageLogRepository, usageBillingRepo UsageBillingRepository, userRepo UserRepository, userSubRepo UserSubscriptionRepository, channelService *ChannelService, billingService *BillingService, resolver *ModelPricingResolver, apiKeyService *APIKeyService, billingCacheService *BillingCacheService, deferredService *DeferredService, balanceNotifyService *BalanceNotifyService, quotaRepo UserPlatformQuotaRepository, httpUpstream HTTPUpstream, cfg *config.Config, settingService *SettingService) *SeedaceVideoService {
	return NewSeedaceVideoService(accountRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo, channelService, billingService, resolver, apiKeyService, billingCacheService, deferredService, balanceNotifyService, quotaRepo, httpUpstream, cfg, settingService)
}

func ProvideUpstreamFundService(wallets *UpstreamWalletService, repo UpstreamFundRepository, balances UpstreamFundBalanceRecorder) *UpstreamFundService {
	return NewUpstreamFundService(wallets, repo, balances)
}

func BindFinanceInitializationAccountService(admin AdminService) FinanceInitializationAccountService {
	return admin
}

func BindFinanceInitializationUpstreamService(upstreams *UpstreamService) FinanceInitializationUpstreamService {
	return upstreams
}

func BindFinanceInitializationWalletService(wallets *UpstreamWalletService) FinanceInitializationWalletService {
	return wallets
}

func BindFinanceInitializationFundService(funds *UpstreamFundService) FinanceInitializationFundService {
	return funds
}

func BindFinanceInitializationProfileService(profiles *AccountFinanceProfileService) FinanceInitializationProfileService {
	return profiles
}

// ProvideEmailQueueService creates EmailQueueService with default worker count
func ProvideEmailQueueService(emailService *EmailService) *EmailQueueService {
	return NewEmailQueueService(emailService, 3)
}

// ProvideOAuthRefreshAPI creates OAuthRefreshAPI with the default lock TTL.
func ProvideOAuthRefreshAPI(accountRepo AccountRepository, tokenCache GeminiTokenCache) *OAuthRefreshAPI {
	return NewOAuthRefreshAPI(accountRepo, tokenCache)
}

// ProvideTokenRefreshService creates and starts TokenRefreshService
func ProvideTokenRefreshService(
	accountRepo AccountRepository,
	oauthService *OAuthService,
	openaiOAuthService *OpenAIOAuthService,
	geminiOAuthService *GeminiOAuthService,
	antigravityOAuthService *AntigravityOAuthService,
	grokOAuthService *GrokOAuthService,
	cacheInvalidator TokenCacheInvalidator,
	schedulerCache SchedulerCache,
	cfg *config.Config,
	tempUnschedCache TempUnschedCache,
	privacyClientFactory PrivacyClientFactory,
	proxyRepo ProxyRepository,
	refreshAPI *OAuthRefreshAPI,
	runtimeBlocker AccountRuntimeBlocker,
) *TokenRefreshService {
	svc := NewTokenRefreshService(accountRepo, oauthService, openaiOAuthService, geminiOAuthService, antigravityOAuthService, cacheInvalidator, schedulerCache, cfg, tempUnschedCache, grokOAuthService)
	// 注入 OpenAI privacy opt-out 依赖
	svc.SetPrivacyDeps(privacyClientFactory, proxyRepo)
	// 注入统一 OAuth 刷新 API（消除 TokenRefreshService 与 TokenProvider 之间的竞争条件）
	svc.SetRefreshAPI(refreshAPI)
	// 调用侧显式注入后台刷新策略，避免策略漂移
	svc.SetRefreshPolicy(DefaultBackgroundRefreshPolicy())
	svc.SetAccountRuntimeBlocker(runtimeBlocker)
	svc.Start()
	return svc
}

// ProvideClaudeTokenProvider creates ClaudeTokenProvider with OAuthRefreshAPI injection
func ProvideClaudeTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	oauthService *OAuthService,
	refreshAPI *OAuthRefreshAPI,
) *ClaudeTokenProvider {
	p := NewClaudeTokenProvider(accountRepo, tokenCache, oauthService)
	executor := NewClaudeTokenRefresher(oauthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(ClaudeProviderRefreshPolicy())
	return p
}

func ProvideGrokTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	grokOAuthService *GrokOAuthService,
	refreshAPI *OAuthRefreshAPI,
	tempUnschedCache TempUnschedCache,
) *GrokTokenProvider {
	provider := NewGrokTokenProvider(accountRepo, tokenCache)
	refresher := NewGrokTokenRefresher(grokOAuthService)
	provider.SetRefreshAPI(refreshAPI, refresher)
	provider.SetRefreshPolicy(GrokProviderRefreshPolicy())
	provider.SetTempUnschedCache(tempUnschedCache)
	return provider
}

func ProvideGrokQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *GrokTokenProvider,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	usageLogRepo UsageLogRepository,
) *GrokQuotaService {
	return NewGrokQuotaService(accountRepo, proxyRepo, tokenProvider, httpUpstream, cfg, usageLogRepo)
}

// ProvideOpenAITokenProvider creates OpenAITokenProvider with OAuthRefreshAPI injection
func ProvideOpenAITokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	openaiOAuthService *OpenAIOAuthService,
	refreshAPI *OAuthRefreshAPI,
) *OpenAITokenProvider {
	p := NewOpenAITokenProvider(accountRepo, tokenCache, openaiOAuthService)
	executor := NewOpenAITokenRefresher(openaiOAuthService, accountRepo)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(OpenAIProviderRefreshPolicy())
	return p
}

// ProvideGeminiTokenProvider creates GeminiTokenProvider with OAuthRefreshAPI injection
func ProvideGeminiTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	geminiOAuthService *GeminiOAuthService,
	refreshAPI *OAuthRefreshAPI,
) *GeminiTokenProvider {
	p := NewGeminiTokenProvider(accountRepo, tokenCache, geminiOAuthService)
	executor := NewGeminiTokenRefresher(geminiOAuthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(GeminiProviderRefreshPolicy())
	return p
}

// ProvideAntigravityTokenProvider creates AntigravityTokenProvider with OAuthRefreshAPI injection
func ProvideAntigravityTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	antigravityOAuthService *AntigravityOAuthService,
	refreshAPI *OAuthRefreshAPI,
	tempUnschedCache TempUnschedCache,
) *AntigravityTokenProvider {
	p := NewAntigravityTokenProvider(accountRepo, tokenCache, antigravityOAuthService)
	executor := NewAntigravityTokenRefresher(antigravityOAuthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(AntigravityProviderRefreshPolicy())
	p.SetTempUnschedCache(tempUnschedCache)
	return p
}

// ProvideDashboardAggregationService 创建并启动仪表盘聚合服务
func ProvideDashboardAggregationService(repo DashboardAggregationRepository, timingWheel *TimingWheelService, cfg *config.Config) *DashboardAggregationService {
	svc := NewDashboardAggregationService(repo, timingWheel, cfg)
	svc.Start()
	return svc
}

// ProvideUsageCleanupService 创建并启动使用记录清理任务服务
func ProvideUsageCleanupService(repo UsageCleanupRepository, timingWheel *TimingWheelService, dashboardAgg *DashboardAggregationService, cfg *config.Config) *UsageCleanupService {
	svc := NewUsageCleanupService(repo, timingWheel, dashboardAgg, cfg)
	svc.Start()
	return svc
}

// ProvideAccountExpiryService creates and starts AccountExpiryService.
func ProvideAccountExpiryService(accountRepo AccountRepository) *AccountExpiryService {
	svc := NewAccountExpiryService(accountRepo, time.Minute)
	svc.Start()
	return svc
}

// ProvideSubscriptionExpiryService creates and starts SubscriptionExpiryService.
func ProvideSubscriptionExpiryService(userSubRepo UserSubscriptionRepository, settingRepo SettingRepository, notificationEmailService *NotificationEmailService) *SubscriptionExpiryService {
	svc := NewSubscriptionExpiryService(userSubRepo, time.Minute)
	svc.SetSettingRepository(settingRepo)
	svc.SetNotificationEmailService(notificationEmailService)
	svc.Start()
	return svc
}

// ProvideTimingWheelService creates and starts TimingWheelService
func ProvideTimingWheelService() (*TimingWheelService, error) {
	svc, err := NewTimingWheelService()
	if err != nil {
		return nil, err
	}
	svc.Start()
	return svc, nil
}

// ProvideDeferredService creates and starts DeferredService
func ProvideDeferredService(accountRepo AccountRepository, timingWheel *TimingWheelService) *DeferredService {
	svc := NewDeferredService(accountRepo, timingWheel, 10*time.Second)
	svc.Start()
	return svc
}

// ProvideConcurrencyService creates ConcurrencyService and starts slot cleanup worker.
func ProvideConcurrencyService(cache ConcurrencyCache, accountRepo AccountRepository, cfg *config.Config) *ConcurrencyService {
	svc := NewConcurrencyService(cache)
	if err := svc.CleanupStaleProcessSlots(context.Background()); err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: startup cleanup stale process slots failed: %v", err)
	}
	if cfg != nil {
		svc.SetAccountLoadBatchCacheTTL(time.Duration(cfg.Gateway.Scheduling.LoadBatchCacheTTLMS) * time.Millisecond)
		svc.StartSlotCleanupWorker(accountRepo, cfg.Gateway.Scheduling.SlotCleanupInterval)
	}
	return svc
}

// ProvideUserMessageQueueService 创建用户消息串行队列服务并启动清理 worker
func ProvideUserMessageQueueService(cache UserMsgQueueCache, rpmCache RPMCache, cfg *config.Config) *UserMessageQueueService {
	svc := NewUserMessageQueueService(cache, rpmCache, &cfg.Gateway.UserMessageQueue)
	if cfg.Gateway.UserMessageQueue.CleanupIntervalSeconds > 0 {
		svc.StartCleanupWorker(time.Duration(cfg.Gateway.UserMessageQueue.CleanupIntervalSeconds) * time.Second)
	}
	return svc
}

// ProvideSchedulerSnapshotService creates and starts SchedulerSnapshotService.
func ProvideSchedulerSnapshotService(
	cache SchedulerCache,
	outboxRepo SchedulerOutboxRepository,
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	cfg *config.Config,
) *SchedulerSnapshotService {
	svc := NewSchedulerSnapshotService(cache, outboxRepo, accountRepo, groupRepo, cfg)
	svc.Start()
	return svc
}

// ProvideRateLimitService creates RateLimitService with optional dependencies.
func ProvideRateLimitService(
	accountRepo AccountRepository,
	usageRepo UsageLogRepository,
	cfg *config.Config,
	geminiQuotaService *GeminiQuotaService,
	tempUnschedCache TempUnschedCache,
	timeoutCounterCache TimeoutCounterCache,
	openAI403CounterCache OpenAI403CounterCache,
	settingService *SettingService,
	tokenCacheInvalidator TokenCacheInvalidator,
	teamBlockStore OpenAITeamBlockStore,
) *RateLimitService {
	svc := NewRateLimitService(accountRepo, usageRepo, cfg, geminiQuotaService, tempUnschedCache)
	svc.SetTimeoutCounterCache(timeoutCounterCache)
	svc.SetOpenAI403CounterCache(openAI403CounterCache)
	svc.SetSettingService(settingService)
	svc.SetTokenCacheInvalidator(tokenCacheInvalidator)
	svc.SetOpenAITeamBlockStore(teamBlockStore)
	return svc
}

// ProvideOpsMetricsCollector creates and starts OpsMetricsCollector.
func ProvideOpsMetricsCollector(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	accountRepo AccountRepository,
	concurrencyService *ConcurrencyService,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsMetricsCollector {
	collector := NewOpsMetricsCollector(opsRepo, settingRepo, accountRepo, concurrencyService, db, redisClient, cfg)
	collector.Start()
	return collector
}

// ProvideOpsAggregationService creates and starts OpsAggregationService (hourly/daily pre-aggregation).
func ProvideOpsAggregationService(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsAggregationService {
	svc := NewOpsAggregationService(opsRepo, settingRepo, db, redisClient, cfg)
	svc.Start()
	return svc
}

// ProvideOpsAlertEvaluatorService creates and starts OpsAlertEvaluatorService.
func ProvideOpsAlertEvaluatorService(
	opsService *OpsService,
	opsRepo OpsRepository,
	emailService *EmailService,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsAlertEvaluatorService {
	svc := NewOpsAlertEvaluatorService(opsService, opsRepo, emailService, redisClient, cfg)
	svc.Start()
	return svc
}

// ProvideOpsCleanupService creates and starts OpsCleanupService (cron scheduled).
// channelMonitorSvc 让维护任务（聚合 + 历史/聚合软删）跟随 ops 清理 cron 一起跑，
// 共享 leader lock + heartbeat。
// settingRepo 让 cleanup service 自己读 ops_advanced_settings.data_retention 覆盖 cfg；
// opsService 用来反向注入 cleanup hook，以便 UI 改清理设置时能 Reload cron。
func ProvideOpsCleanupService(
	opsRepo OpsRepository,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
	channelMonitorSvc *ChannelMonitorService,
	settingRepo SettingRepository,
	opsService *OpsService,
) *OpsCleanupService {
	svc := NewOpsCleanupService(opsRepo, db, redisClient, cfg, channelMonitorSvc, settingRepo)
	svc.Start()
	if opsService != nil {
		opsService.SetCleanupReloader(svc)
	}
	return svc
}

func ProvideOpsSystemLogSink(opsRepo OpsRepository) *OpsSystemLogSink {
	sink := NewOpsSystemLogSink(opsRepo)
	sink.Start()
	logger.SetSink(sink)
	return sink
}

func buildIdempotencyConfig(cfg *config.Config) IdempotencyConfig {
	idempotencyCfg := DefaultIdempotencyConfig()
	if cfg != nil {
		if cfg.Idempotency.DefaultTTLSeconds > 0 {
			idempotencyCfg.DefaultTTL = time.Duration(cfg.Idempotency.DefaultTTLSeconds) * time.Second
		}
		if cfg.Idempotency.SystemOperationTTLSeconds > 0 {
			idempotencyCfg.SystemOperationTTL = time.Duration(cfg.Idempotency.SystemOperationTTLSeconds) * time.Second
		}
		if cfg.Idempotency.ProcessingTimeoutSeconds > 0 {
			idempotencyCfg.ProcessingTimeout = time.Duration(cfg.Idempotency.ProcessingTimeoutSeconds) * time.Second
		}
		if cfg.Idempotency.FailedRetryBackoffSeconds > 0 {
			idempotencyCfg.FailedRetryBackoff = time.Duration(cfg.Idempotency.FailedRetryBackoffSeconds) * time.Second
		}
		if cfg.Idempotency.MaxStoredResponseLen > 0 {
			idempotencyCfg.MaxStoredResponseLen = cfg.Idempotency.MaxStoredResponseLen
		}
		idempotencyCfg.ObserveOnly = cfg.Idempotency.ObserveOnly
	}
	return idempotencyCfg
}

func ProvideIdempotencyCoordinator(repo IdempotencyRepository, cfg *config.Config) *IdempotencyCoordinator {
	coordinator := NewIdempotencyCoordinator(repo, buildIdempotencyConfig(cfg))
	SetDefaultIdempotencyCoordinator(coordinator)
	return coordinator
}

func ProvideSystemOperationLockService(repo IdempotencyRepository, cfg *config.Config) *SystemOperationLockService {
	return NewSystemOperationLockService(repo, buildIdempotencyConfig(cfg))
}

func ProvideIdempotencyCleanupService(repo IdempotencyRepository, cfg *config.Config) *IdempotencyCleanupService {
	svc := NewIdempotencyCleanupService(repo, cfg)
	svc.Start()
	return svc
}

// ProvideScheduledTestService creates ScheduledTestService.
func ProvideScheduledTestService(
	planRepo ScheduledTestPlanRepository,
	resultRepo ScheduledTestResultRepository,
) *ScheduledTestService {
	return NewScheduledTestService(planRepo, resultRepo)
}

// ProvideScheduledTestRunnerService creates and starts ScheduledTestRunnerService.
func ProvideScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
) *ScheduledTestRunnerService {
	svc := NewScheduledTestRunnerService(planRepo, scheduledSvc, accountTestSvc, rateLimitSvc, cfg)
	svc.Start()
	return svc
}

func ProvideSemanticCacheAsyncWriter(cache GatewayCache, settingService *SettingService) *SemanticCacheAsyncWriter {
	return NewSemanticCacheAsyncWriter(cache, settingService)
}

func ProvideGatewayService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	identityService *IdentityService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	claudeTokenProvider *ClaudeTokenProvider,
	sessionLimitCache SessionLimitCache,
	rpmCache RPMCache,
	digestStore *DigestSessionStore,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
	channelService *ChannelService,
	resolver *ModelPricingResolver,
	balanceNotifyService *BalanceNotifyService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
	semanticCacheWriter *SemanticCacheAsyncWriter,
) *GatewayService {
	svc := NewGatewayService(accountRepo, groupRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo, userGroupRateRepo, cache, cfg, schedulerSnapshot, concurrencyService, billingService, rateLimitService, billingCacheService, identityService, httpUpstream, deferredService, claudeTokenProvider, sessionLimitCache, rpmCache, digestStore, settingService, tlsFPProfileService, channelService, resolver, balanceNotifyService, userPlatformQuotaRepo)
	svc.SetSemanticCacheWriter(semanticCacheWriter)
	return svc
}

func ProvideOpenAIGatewayService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider,
	grokTokenProvider *GrokTokenProvider,
	resolver *ModelPricingResolver,
	channelService *ChannelService,
	balanceNotifyService *BalanceNotifyService,
	settingService *SettingService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
	idempotencyRepo IdempotencyRepository,
	semanticCacheWriter *SemanticCacheAsyncWriter,
	cnQuotaService *CNProviderQuotaService,
	channelMonitorService *ChannelMonitorService,
) *OpenAIGatewayService {
	svc := NewOpenAIGatewayService(accountRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo, userGroupRateRepo, cache, cfg, schedulerSnapshot, concurrencyService, billingService, rateLimitService, billingCacheService, httpUpstream, deferredService, openAITokenProvider, resolver, channelService, balanceNotifyService, settingService, userPlatformQuotaRepo, idempotencyRepo)
	svc.SetGrokTokenProvider(grokTokenProvider)
	svc.SetSemanticCacheWriter(semanticCacheWriter)
	svc.SetCNProviderQuotaService(cnQuotaService)
	svc.SetGatewayTelemetryRecorder(channelMonitorService)
	return svc
}

// ProvideAccountUsageService wires domestic quota and balance snapshots into
// the existing account usage service while preserving the public constructor
// used by focused tests and integrations.
func ProvideAccountUsageService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageFetcher ClaudeUsageFetcher,
	geminiQuotaService *GeminiQuotaService,
	antigravityQuotaFetcher *AntigravityQuotaFetcher,
	grokQuotaFetcher *GrokQuotaFetcher,
	grokQuotaService *GrokQuotaService,
	cache *UsageCache,
	identityCache IdentityCache,
	tlsFPProfileService *TLSFingerprintProfileService,
	cnQuotaService *CNProviderQuotaService,
	cnBalanceService *CNProviderBalanceService,
) *AccountUsageService {
	svc := NewAccountUsageService(accountRepo, usageLogRepo, usageFetcher, geminiQuotaService, antigravityQuotaFetcher, grokQuotaFetcher, grokQuotaService, cache, identityCache, tlsFPProfileService)
	svc.SetCNQuotaService(cnQuotaService)
	svc.SetCNBalanceService(cnBalanceService)
	return svc
}

func ProvideAccountTestService(
	accountRepo AccountRepository,
	geminiTokenProvider *GeminiTokenProvider,
	claudeTokenProvider *ClaudeTokenProvider,
	grokTokenProvider *GrokTokenProvider,
	antigravityGatewayService *AntigravityGatewayService,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	tlsFPProfileService *TLSFingerprintProfileService,
	settingService *SettingService,
) *AccountTestService {
	svc := NewAccountTestService(accountRepo, geminiTokenProvider, claudeTokenProvider, grokTokenProvider, antigravityGatewayService, httpUpstream, cfg, tlsFPProfileService)
	svc.SetSettingService(settingService)
	return svc
}

func ProvideDashboardService(
	usageRepo UsageLogRepository,
	aggRepo DashboardAggregationRepository,
	cache DashboardStatsCache,
	cfg *config.Config,
	providers []GroupUsageSummaryProvider,
	settingService *SettingService,
) *DashboardService {
	svc := NewDashboardService(usageRepo, aggRepo, cache, cfg, providers...)
	svc.SetSettingService(settingService)
	return svc
}

func ProvideCustomGroupUsageRollup(store GroupUsageRollupStore, settingService *SettingService) *CustomGroupUsageRollup {
	worker := NewCustomGroupUsageRollup(store)
	worker.SetSettingService(settingService)
	worker.Start(context.Background())
	return worker
}

// ProvideOpsScheduledReportService creates and starts OpsScheduledReportService.
func ProvideOpsScheduledReportService(
	opsService *OpsService,
	userService *UserService,
	emailService *EmailService,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsScheduledReportService {
	svc := NewOpsScheduledReportService(opsService, userService, emailService, redisClient, cfg)
	svc.Start()
	return svc
}

// ProvideAPIKeyAuthCacheInvalidator 提供 API Key 认证缓存失效能力
func ProvideAPIKeyAuthCacheInvalidator(apiKeyService *APIKeyService) APIKeyAuthCacheInvalidator {
	// Start Pub/Sub subscriber for L1 cache invalidation across instances
	apiKeyService.StartAuthCacheInvalidationSubscriber(context.Background())
	return apiKeyService
}

// ProvideBackupService creates and starts BackupService
func ProvideBackupService(
	settingRepo SettingRepository,
	backupRepo BackupRepository,
	cfg *config.Config,
	encryptor SecretEncryptor,
	storeFactory BackupObjectStoreFactory,
	dumper DBDumper,
) *BackupService {
	svc := NewBackupService(settingRepo, cfg, encryptor, storeFactory, dumper, backupRepo)
	svc.Start()
	return svc
}

// ProvideSettingService wires SettingService with group reader and proxy repo.
func ProvideSettingService(settingRepo SettingRepository, groupRepo GroupRepository, proxyRepo ProxyRepository, cfg *config.Config, encryptor SecretEncryptor) *SettingService {
	svc := NewSettingService(settingRepo, cfg)
	svc.SetDefaultSubscriptionGroupReader(groupRepo)
	svc.SetProxyRepository(proxyRepo)
	svc.SetSecretEncryptor(encryptor)
	if err := svc.LoadAPIKeyACLTrustForwardedIPSetting(context.Background()); err != nil {
		logger.LegacyPrintf("service.setting", "Warning: load api key acl forwarded ip setting failed: %v", err)
	}
	antigravity.SetUserAgentVersionResolver(svc.GetAntigravityUserAgentVersion)
	return svc
}

// ProvideBillingCacheService wires BillingCacheService with its RPM dependencies.
func ProvideBillingCacheService(
	cache BillingCache,
	userRepo UserRepository,
	subRepo UserSubscriptionRepository,
	apiKeyRepo APIKeyRepository,
	rpmCache UserRPMCache,
	rateRepo UserGroupRateRepository,
	cfg *config.Config,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *BillingCacheService {
	return NewBillingCacheService(cache, userRepo, subRepo, apiKeyRepo, rpmCache, rateRepo, cfg, userPlatformQuotaRepo)
}

// ProvideAPIKeyService wires APIKeyService and connects rate-limit cache invalidation.
func ProvideAPIKeyService(
	apiKeyRepo APIKeyRepository,
	userRepo UserRepository,
	groupRepo GroupRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache APIKeyCache,
	cfg *config.Config,
	billingCacheService *BillingCacheService,
) *APIKeyService {
	svc := NewAPIKeyService(apiKeyRepo, userRepo, groupRepo, userSubRepo, userGroupRateRepo, cache, cfg)
	svc.SetRateLimitCacheInvalidator(billingCacheService)
	return svc
}

// ProvideOpsService creates OpsService and injects optional secret encryption dependencies.
func ProvideOpsService(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	cfg *config.Config,
	accountRepo AccountRepository,
	userRepo UserRepository,
	concurrencyService *ConcurrencyService,
	gatewayService *GatewayService,
	openAIGatewayService *OpenAIGatewayService,
	geminiCompatService *GeminiMessagesCompatService,
	antigravityGatewayService *AntigravityGatewayService,
	systemLogSink *OpsSystemLogSink,
	encryptor SecretEncryptor,
) *OpsService {
	svc := NewOpsService(opsRepo, settingRepo, cfg, accountRepo, userRepo, concurrencyService, gatewayService, openAIGatewayService, geminiCompatService, antigravityGatewayService, systemLogSink)
	svc.SetSecretEncryptor(encryptor)
	svc.StartAIAnalysisWorker()
	return svc
}

func ProvideGeminiMessagesCompatService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	cache GatewayCache,
	schedulerSnapshot *SchedulerSnapshotService,
	tokenProvider *GeminiTokenProvider,
	rateLimitService *RateLimitService,
	httpUpstream HTTPUpstream,
	antigravityGatewayService *AntigravityGatewayService,
	cfg *config.Config,
	settingService *SettingService,
	semanticCacheWriter *SemanticCacheAsyncWriter,
) *GeminiMessagesCompatService {
	svc := NewGeminiMessagesCompatService(accountRepo, groupRepo, cache, schedulerSnapshot, tokenProvider, rateLimitService, httpUpstream, antigravityGatewayService, cfg)
	svc.SetSettingService(settingService)
	svc.SetSemanticCacheWriter(semanticCacheWriter)
	return svc
}

func ProvideAnnouncementService(
	announcementRepo AnnouncementRepository,
	readRepo AnnouncementReadRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	emailService *EmailService,
	settingService *SettingService,
	cfg *config.Config,
) *AnnouncementService {
	svc := NewAnnouncementService(announcementRepo, readRepo, userRepo, userSubRepo, emailService, settingService)
	_ = cfg // translation settings are read dynamically from SettingService
	return svc
}

// ProviderSet is the Wire provider set for all services
var ProviderSet = wire.NewSet(
	// Core services
	NewAuthService,
	NewUserService,
	ProvideAPIKeyService,
	ProvideAPIKeyAuthCacheInvalidator,
	NewGroupService,
	NewAccountService,
	NewProxyService,
	NewRedeemService,
	NewPromoService,
	NewUsageService,
	ProvideDashboardService,
	ProvideCustomGroupUsageRollup,
	ProvideCNProviderQuotaService,
	ProvideCNProviderBalanceService,
	ProvidePricingService,
	NewBillingService,
	ProvideBillingCacheService,
	ProvideAnnouncementService,
	NewAdminService,
	ProvideSemanticCacheAsyncWriter,
	ProvideGatewayService,
	ProvideOpenAIGatewayService,
	ProvideSeedaceVideoService,
	NewSeedaceVideoHistoryService,
	wire.Bind(new(AccountRuntimeBlocker), new(*OpenAIGatewayService)),
	NewOAuthService,
	NewOpenAIOAuthService,
	NewGrokOAuthService,
	wire.Bind(new(GrokOAuthTokenService), new(*GrokOAuthService)),
	NewGeminiOAuthService,
	NewGeminiQuotaService,
	NewCompositeTokenCacheInvalidator,
	wire.Bind(new(TokenCacheInvalidator), new(*CompositeTokenCacheInvalidator)),
	NewAntigravityOAuthService,
	ProvideOAuthRefreshAPI,
	ProvideGeminiTokenProvider,
	ProvideGeminiMessagesCompatService,
	ProvideAntigravityTokenProvider,
	ProvideGrokTokenProvider,
	ProvideOpenAITokenProvider,
	ProvideGrokQuotaService,
	ProvideClaudeTokenProvider,
	NewAntigravityGatewayService,
	ProvideRateLimitService,
	ProvideAccountUsageService,
	ProvideAccountTestService,
	ProvideSettingService,
	NewDataManagementService,
	ProvideBackupService,
	ProvideOpsSystemLogSink,
	ProvideOpsService,
	ProvideOpsMetricsCollector,
	ProvideOpsAggregationService,
	ProvideOpsAlertEvaluatorService,
	ProvideOpsCleanupService,
	ProvideOpsScheduledReportService,
	ProvideCacheStatsService,
	NewEmailService,
	NewNotificationEmailService,
	ProvideEmailQueueService,
	NewTurnstileService,
	NewSubscriptionService,
	wire.Bind(new(DefaultSubscriptionAssigner), new(*SubscriptionService)),
	ProvideConcurrencyService,
	ProvideUserMessageQueueService,
	NewUsageRecordWorkerPool,
	ProvideSchedulerSnapshotService,
	NewIdentityService,
	NewCRSSyncService,
	ProvideUpdateService,
	ProvideTokenRefreshService,
	wire.Bind(new(GrokOAuthReconciler), new(*TokenRefreshService)),
	ProvideAccountExpiryService,
	ProvideSubscriptionExpiryService,
	ProvideTimingWheelService,
	ProvideDashboardAggregationService,
	ProvideUsageCleanupService,
	ProvideDeferredService,
	NewAntigravityQuotaFetcher,
	NewGrokQuotaFetcher,
	NewUserAttributeService,
	NewUsageCache,
	NewTotpService,
	NewErrorPassthroughService,
	NewTLSFingerprintProfileService,
	NewDigestSessionStore,
	ProvideIdempotencyCoordinator,
	ProvideSystemOperationLockService,
	ProvideIdempotencyCleanupService,
	ProvideScheduledTestService,
	ProvideScheduledTestRunnerService,
	NewGroupCapacityService,
	ProvideChannelService,
	NewModelPricingResolver,
	NewModelSquareService,
	NewFinanceCostCalculator,
	NewFinancePriceSelector,
	NewFinanceCatalogVersionSync,
	NewFinanceUsageScanner,
	NewFinanceBackfillService,
	NewFinanceDetailService,
	NewAccountFinanceSettlementService,
	NewAccountFinanceProfileService,
	wire.Bind(new(AccountFinanceSettlementProcessor), new(*AccountFinanceSettlementService)),
	NewAccountFinanceSnapshotService,
	NewUpstreamWalletService,
	NewUpstreamFinanceAdapterRegistry,
	NewUpstreamFinanceHTTPExecutor,
	NewUpstreamFinanceProtocolService,
	NewUpstreamFinanceProtocolDetector,
	NewUpstreamFinanceSyncService,
	ProvideUpstreamFinanceSyncRuntime,
	ProvideUpstreamFundService,
	NewFinanceReportService,
	NewFinanceAlertService,
	NewFinancePaymentFeeService,
	NewFinanceFXRateService,
	NewFinanceRevenueRecognitionService,
	NewFinanceReconciliationService,
	NewPromotionCreditReconciliationService,
	NewFinanceExportService,
	ProvideFinanceRuntime,
	NewContentModerationService,
	NewAffiliateService,
	ProvidePaymentConfigService,
	ProvidePaymentService,
	ProvidePaymentOrderExpiryService,
	ProvideBalanceNotifyService,
	NewUpstreamService,
	BindFinanceInitializationAccountService,
	BindFinanceInitializationUpstreamService,
	BindFinanceInitializationWalletService,
	BindFinanceInitializationFundService,
	BindFinanceInitializationProfileService,
	NewFinanceInitializationService,
	ProvideBalanceLowNotifyScanner,
	ProvideChannelMonitorService,
	ProvideChannelMonitorRunner,
	NewChannelMonitorRequestTemplateService,
)

// ProvidePaymentConfigService wraps NewPaymentConfigService to accept the named
// payment.EncryptionKey type instead of raw []byte, avoiding Wire ambiguity.
func ProvidePaymentConfigService(entClient *dbent.Client, settingRepo SettingRepository, key payment.EncryptionKey) *PaymentConfigService {
	return NewPaymentConfigService(entClient, settingRepo, []byte(key))
}

// ProvideBalanceNotifyService creates BalanceNotifyService.
func ProvideBalanceNotifyService(emailService *EmailService, settingRepo SettingRepository, accountRepo AccountRepository, balanceLowRepo BalanceLowNotifyRepository, notificationEmailService *NotificationEmailService) *BalanceNotifyService {
	svc := NewBalanceNotifyService(emailService, settingRepo, accountRepo, balanceLowRepo)
	svc.SetNotificationEmailService(notificationEmailService)
	return svc
}

// ProvideBalanceLowNotifyScanner creates and starts BalanceLowNotifyScanner.
func ProvideBalanceLowNotifyScanner(balanceNotify *BalanceNotifyService) *BalanceLowNotifyScanner {
	scanner := NewBalanceLowNotifyScanner(balanceNotify, 5*time.Minute)
	scanner.Start()
	return scanner
}

// ProvidePaymentService creates PaymentService and attaches notification email delivery.
func ProvidePaymentService(entClient *dbent.Client, registry *payment.Registry, loadBalancer payment.LoadBalancer, redeemService *RedeemService, subscriptionSvc *SubscriptionService, configService *PaymentConfigService, userRepo UserRepository, groupRepo GroupRepository, affiliateService *AffiliateService, notificationEmailService *NotificationEmailService) *PaymentService {
	svc := NewPaymentService(entClient, registry, loadBalancer, redeemService, subscriptionSvc, configService, userRepo, groupRepo, affiliateService)
	svc.SetNotificationEmailService(notificationEmailService)
	return svc
}

// ProvidePaymentOrderExpiryService creates and starts PaymentOrderExpiryService.
func ProvidePaymentOrderExpiryService(paymentSvc *PaymentService) *PaymentOrderExpiryService {
	svc := NewPaymentOrderExpiryService(paymentSvc, 60*time.Second)
	svc.Start()
	return svc
}

// ProvideChannelMonitorService 创建渠道监控服务（CRUD + RunCheck + 用户视图聚合）。
// 加密器复用 wire 中已注入的 SecretEncryptor（AES-256-GCM）。
func ProvideChannelMonitorService(
	repo ChannelMonitorRepository,
	encryptor SecretEncryptor,
	accountRepo AccountRepository,
	usageService *AccountUsageService,
) *ChannelMonitorService {
	return NewChannelMonitorService(repo, encryptor, accountRepo, usageService)
}

// ProvideChannelMonitorRunner 创建并启动渠道监控调度器。
// 通过 SetScheduler 注入回 service 后再 Start，确保启动时加载所有 enabled monitor，
// 后续 CRUD 也能即时同步任务表。Runner.Stop 由 cleanup function 调用。
// settingService 用于 runner 每次 fire 读取功能开关。
func ProvideChannelMonitorRunner(svc *ChannelMonitorService, settingService *SettingService) *ChannelMonitorRunner {
	r := NewChannelMonitorRunner(svc, settingService)
	svc.SetScheduler(r)
	r.Start()
	return r
}
