package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type ollamaUsageRepoStub struct{ UsageLogRepository }

func (ollamaUsageRepoStub) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
	return &usagestats.AccountStats{Requests: 3, Tokens: 120}, nil
}

func TestOllamaUsageServiceReturnsExplicitLocalSnapshot(t *testing.T) {
	usage, err := NewOllamaUsageService(ollamaUsageRepoStub{}).GetUsage(context.Background(), 7, time.Unix(1, 0))
	require.NoError(t, err)
	require.Equal(t, "passive", usage.Source)
	require.EqualValues(t, 3, usage.FiveHour.WindowStats.Requests)
	require.EqualValues(t, 120, usage.FiveHour.WindowStats.Tokens)
}
