package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseCNProviderRateLimitHeadersSupportsUnixAndDeltaReset(t *testing.T) {
	now := time.Now()
	headers := make(http.Header)
	headers.Set("x-ratelimit-reset-requests", "60")
	headers.Set("x-ratelimit-reset-tokens", strconv.FormatInt(now.Add(time.Minute).Unix(), 10))
	parsed := ParseCNProviderRateLimitHeaders(headers)
	require.Contains(t, parsed, "requests")
	require.InDelta(t, 60, time.Until(*parsed["requests"].ResetAt).Seconds(), 2)
	require.Contains(t, parsed, "tokens")
	require.WithinDuration(t, now.Add(time.Minute), *parsed["tokens"].ResetAt, 2*time.Second)
}

type cnQuotaFetcherStub struct {
	mu    sync.Mutex
	calls int
	snap  *CNProviderQuotaSnapshot
	err   error
	delay time.Duration
}

func (f *cnQuotaFetcherStub) FetchQuota(ctx context.Context, _ *Account) (*CNProviderQuotaSnapshot, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.delay > 0 {
		t := time.NewTimer(f.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return cloneCNQuotaSnapshot(f.snap), f.err
}

func (f *cnQuotaFetcherStub) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestCNProviderQuotaServiceCachesSuccessAndNormalizesExpiry(t *testing.T) {
	now := time.Now()
	remaining := 7.0
	fetcher := &cnQuotaFetcherStub{snap: &CNProviderQuotaSnapshot{State: "fresh", Remaining: &remaining, ResetAt: cnQuotaPtrTime(now.Add(time.Hour))}}
	svc := NewCNProviderQuotaService(nil, fetcher)
	account := &Account{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey}
	first, err := svc.GetSnapshot(context.Background(), account, false)
	if err != nil || first.State != "fresh" || first.Remaining == nil || *first.Remaining != remaining {
		t.Fatalf("unexpected first snapshot: %#v, %v", first, err)
	}
	second, err := svc.GetSnapshot(context.Background(), account, false)
	if err != nil || second.State != "fresh" || fetcher.Calls() != 1 {
		t.Fatalf("cache miss: %#v, %v, calls=%d", second, err, fetcher.Calls())
	}
}

func TestCNProviderQuotaServiceNegativeCacheAndSingleflight(t *testing.T) {
	fetcher := &cnQuotaFetcherStub{err: errors.New("provider timeout"), delay: 20 * time.Millisecond}
	svc := NewCNProviderQuotaService(nil, fetcher)
	account := &Account{ID: 2, Platform: PlatformDeepSeek, Type: AccountTypeAPIKey}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := svc.GetSnapshot(context.Background(), account, false)
			if err == nil || snap == nil || snap.State != "failed" {
				t.Errorf("expected failed snapshot, got %#v, %v", snap, err)
			}
		}()
	}
	wg.Wait()
	if calls := fetcher.Calls(); calls != 1 {
		t.Fatalf("singleflight calls=%d, want 1", calls)
	}
	_, _ = svc.GetSnapshot(context.Background(), account, false)
	if calls := fetcher.Calls(); calls != 1 {
		t.Fatalf("negative cache calls=%d, want 1", calls)
	}
}

func TestSanitizeQuotaErrorRedactsURLCredentialsAndQuery(t *testing.T) {
	err := errors.New(`Get "https://quota-user:secret@example.com/v1/quota?token=secret-token&key=secret-key": dial tcp 203.0.113.10:443: i/o timeout`)
	got := sanitizeQuotaError(err)
	require.NotContains(t, got, "secret")
	require.Contains(t, got, "https://example.com/v1/quota")
}

func TestAccountIsCNQuotaExhaustedOnlyForFreshZero(t *testing.T) {
	for _, tc := range []struct {
		state     string
		remaining any
		want      bool
	}{
		{"fresh", float64(0), true}, {"fresh", float64(1), false}, {"failed", float64(0), false}, {"stale", float64(0), false},
	} {
		account := &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey, Extra: map[string]any{CNQuotaSnapshotExtraKey: map[string]any{"state": tc.state, "remaining": tc.remaining}}}
		if got := account.IsCNQuotaExhausted(); got != tc.want {
			t.Errorf("state=%s remaining=%v got=%v want=%v", tc.state, tc.remaining, got, tc.want)
		}
	}
}

func TestCNProviderSnapshotBlocksSchedulingByBalanceAndRateWindow(t *testing.T) {
	now := time.Now()
	zero := float64(0)
	reset := now.Add(time.Minute)
	require.True(t, cnProviderSnapshotBlocksScheduling(&CNProviderQuotaSnapshot{State: "fresh", Remaining: &zero, ResetAt: &reset}, now))
	remaining := int64(0)
	require.True(t, cnProviderSnapshotBlocksScheduling(&CNProviderQuotaSnapshot{State: "fresh", RateLimits: map[string]CNProviderRateLimitWindow{
		"requests": {Remaining: &remaining, ResetAt: &reset},
	}}, now))
	require.False(t, cnProviderSnapshotBlocksScheduling(&CNProviderQuotaSnapshot{State: "stale", Remaining: &zero}, now))
}

func cnQuotaPtrTime(t time.Time) *time.Time { return &t }
