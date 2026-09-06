package service

import (
	"context"
	"fmt"
	"time"
)

// OllamaUsageService provides a bounded local usage snapshot for Ollama Cloud
// accounts. Ollama Cloud does not expose the same provider quota contract as
// Anthropic/OpenAI, so the UI receives an explicit passive/local state rather
// than a fabricated remote quota.
type OllamaUsageService struct {
	usageLogRepo UsageLogRepository
}

func NewOllamaUsageService(usageLogRepo UsageLogRepository) *OllamaUsageService {
	return &OllamaUsageService{usageLogRepo: usageLogRepo}
}

func (s *OllamaUsageService) GetUsage(ctx context.Context, accountID int64, now time.Time) (*UsageInfo, error) {
	if s == nil || s.usageLogRepo == nil || accountID <= 0 {
		return nil, fmt.Errorf("ollama usage service is unavailable")
	}
	if now.IsZero() {
		now = time.Now()
	}
	start := now.Add(-5 * time.Hour)
	stats, err := s.usageLogRepo.GetAccountWindowStats(ctx, accountID, start)
	if err != nil {
		return nil, fmt.Errorf("get ollama local usage: %w", err)
	}
	return &UsageInfo{Source: "passive", UpdatedAt: &now, FiveHour: &UsageProgress{WindowStats: &WindowStats{Requests: stats.Requests, Tokens: stats.Tokens, Cost: stats.Cost, StandardCost: stats.StandardCost, UserCost: stats.UserCost}}}, nil
}
