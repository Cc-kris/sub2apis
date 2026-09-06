//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type groupUsageRollupRuntimeStore struct {
	mu                  sync.Mutex
	registerCalls       int
	refreshCalls        int
	refreshStarted      chan struct{}
	refreshCanceled     chan struct{}
	refreshBeforeReturn chan struct{}
	allowRefreshReturn  chan struct{}
	blockFirstRefresh   bool
	blockOnce           sync.Once
	returnGateOnce      sync.Once
}

type rollupRuntimeSettingRepo struct {
	mu               sync.Mutex
	values           map[string]string
	getCalls         int
	finalReadStarted chan struct{}
	allowFinalRead   chan struct{}
}

func (r *rollupRuntimeSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (r *rollupRuntimeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	value, ok := r.values[key]
	r.getCalls++
	blockFinalRead := r.getCalls == 2 && r.finalReadStarted != nil && r.allowFinalRead != nil
	r.mu.Unlock()
	if blockFinalRead {
		close(r.finalReadStarted)
		<-r.allowFinalRead
	}
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *rollupRuntimeSettingRepo) Set(context.Context, string, string) error { return nil }
func (r *rollupRuntimeSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *rollupRuntimeSettingRepo) SetMultiple(_ context.Context, updates map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range updates {
		r.values[key] = value
	}
	return nil
}
func (r *rollupRuntimeSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *rollupRuntimeSettingRepo) Delete(context.Context, string) error { return nil }

func (s *groupUsageRollupRuntimeStore) GetGroupUsageSummary(context.Context, time.Time) ([]usagestats.GroupUsageSummary, error) {
	return nil, nil
}
func (s *groupUsageRollupRuntimeStore) RegisterTimezone(context.Context, string) error {
	s.mu.Lock()
	s.registerCalls++
	s.mu.Unlock()
	return nil
}
func (s *groupUsageRollupRuntimeStore) ListRegisteredTimezones(context.Context) ([]string, error) {
	return []string{defaultGroupUsageTimezone}, nil
}
func (s *groupUsageRollupRuntimeStore) Refresh(ctx context.Context, _ string, _ time.Time) error {
	s.mu.Lock()
	s.refreshCalls++
	s.mu.Unlock()
	if s.blockFirstRefresh {
		blocked := false
		s.blockOnce.Do(func() {
			blocked = true
			close(s.refreshStarted)
		})
		if blocked {
			<-ctx.Done()
			close(s.refreshCanceled)
			return ctx.Err()
		}
	}
	if s.refreshBeforeReturn != nil {
		waitForReturn := false
		s.returnGateOnce.Do(func() {
			waitForReturn = true
			close(s.refreshBeforeReturn)
		})
		if waitForReturn {
			<-s.allowRefreshReturn
		}
	}
	return nil
}

func (s *groupUsageRollupRuntimeStore) callCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registerCalls, s.refreshCalls
}

func TestCustomGroupUsageRollupRefreshSkipsWhenRuntimeDisabled(t *testing.T) {
	store := &groupUsageRollupRuntimeStore{}
	settings := NewSettingService(&compactTestSettingRepo{values: map[string]string{
		SettingKeyGroupUsageRollupEnabled: "false",
	}}, &config.Config{})
	worker := NewCustomGroupUsageRollup(store)
	worker.SetSettingService(settings)

	require.ErrorIs(t, worker.refresh(context.Background()), errGroupUsageRollupPaused)
	registerCalls, refreshCalls := store.callCounts()
	require.Zero(t, registerCalls)
	require.Zero(t, refreshCalls)
}

func TestCustomGroupUsageRollupRefreshResumesWhenRuntimeEnabled(t *testing.T) {
	store := &groupUsageRollupRuntimeStore{}
	settingsRepo := &compactTestSettingRepo{values: map[string]string{
		SettingKeyGroupUsageRollupEnabled: "false",
	}}
	worker := NewCustomGroupUsageRollup(store)
	worker.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	require.ErrorIs(t, worker.refresh(context.Background()), errGroupUsageRollupPaused)
	settingsRepo.values[SettingKeyGroupUsageRollupEnabled] = "true"
	require.NoError(t, worker.refresh(context.Background()))
	registerCalls, refreshCalls := store.callCounts()
	require.Equal(t, 1, registerCalls)
	require.Equal(t, 1, refreshCalls)
}

func TestCustomGroupUsageRollupDisableCancelsActiveRefreshAndTickerResumes(t *testing.T) {
	store := &groupUsageRollupRuntimeStore{
		refreshStarted:    make(chan struct{}),
		refreshCanceled:   make(chan struct{}),
		blockFirstRefresh: true,
	}
	settingsRepo := &rollupRuntimeSettingRepo{values: map[string]string{
		SettingKeyGroupUsageRollupEnabled: "true",
	}}
	settings := NewSettingService(settingsRepo, &config.Config{})
	worker := NewCustomGroupUsageRollup(store)
	worker.interval = 10 * time.Millisecond
	worker.SetSettingService(settings)
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)

	require.Eventually(t, func() bool {
		select {
		case <-store.refreshStarted:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)

	require.NoError(t, settings.UpdateSettings(context.Background(), &SystemSettings{
		GroupUsageRollupEnabled: false,
	}))
	select {
	case <-store.refreshCanceled:
	case <-time.After(time.Second):
		t.Fatal("active rollup refresh was not canceled")
	}
	_, refreshCallsAtDisable := store.callCounts()
	require.False(t, worker.healthy.Load())
	time.Sleep(30 * time.Millisecond)
	_, refreshCallsAfterWait := store.callCounts()
	require.Equal(t, refreshCallsAtDisable, refreshCallsAfterWait)

	require.NoError(t, settings.UpdateSettings(context.Background(), &SystemSettings{
		GroupUsageRollupEnabled: true,
	}))
	require.Eventually(t, func() bool {
		_, refreshCalls := store.callCounts()
		return refreshCalls > refreshCallsAtDisable && worker.healthy.Load()
	}, time.Second, time.Millisecond)
}

func TestCustomGroupUsageRollupDisableAtFinalRefreshBoundaryStaysUnhealthy(t *testing.T) {
	store := &groupUsageRollupRuntimeStore{
		refreshBeforeReturn: make(chan struct{}),
		allowRefreshReturn:  make(chan struct{}),
	}
	settingsRepo := &rollupRuntimeSettingRepo{values: map[string]string{
		SettingKeyGroupUsageRollupEnabled: "true",
	}}
	settings := NewSettingService(settingsRepo, &config.Config{})
	worker := NewCustomGroupUsageRollup(store)
	worker.interval = 10 * time.Millisecond
	worker.SetSettingService(settings)
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)

	select {
	case <-store.refreshBeforeReturn:
	case <-time.After(time.Second):
		t.Fatal("rollup refresh did not reach its return boundary")
	}
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- settings.UpdateSettings(context.Background(), &SystemSettings{
			GroupUsageRollupEnabled: false,
		})
	}()
	close(store.allowRefreshReturn)
	require.NoError(t, <-updateDone)
	require.Eventually(t, func() bool {
		return !worker.healthy.Load()
	}, time.Second, time.Millisecond)

	require.NoError(t, settings.UpdateSettings(context.Background(), &SystemSettings{
		GroupUsageRollupEnabled: true,
	}))
	require.Eventually(t, func() bool {
		return worker.healthy.Load()
	}, time.Second, time.Millisecond)
}

func TestCustomGroupUsageRollupPausedRefreshCannotRestoreHealth(t *testing.T) {
	store := &groupUsageRollupRuntimeStore{}
	settingsRepo := &rollupRuntimeSettingRepo{
		values: map[string]string{
			SettingKeyGroupUsageRollupEnabled: "true",
		},
		finalReadStarted: make(chan struct{}),
		allowFinalRead:   make(chan struct{}),
	}
	worker := NewCustomGroupUsageRollup(store)
	worker.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- worker.refresh(context.Background()) }()
	select {
	case <-settingsRepo.finalReadStarted:
	case <-time.After(time.Second):
		t.Fatal("rollup refresh did not reach its final runtime check")
	}

	pauseDone := make(chan struct{})
	go func() {
		worker.cancelActiveRefreshAndWait()
		close(pauseDone)
	}()
	require.Eventually(t, func() bool {
		worker.refreshMu.Lock()
		paused := worker.refreshDone != nil && worker.pausedEpoch == worker.refreshEpoch
		worker.refreshMu.Unlock()
		return paused
	}, time.Second, time.Millisecond)

	close(settingsRepo.allowFinalRead)
	require.NoError(t, <-refreshDone)
	select {
	case <-pauseDone:
	case <-time.After(time.Second):
		t.Fatal("pause did not wait for the active refresh")
	}
	require.False(t, worker.healthy.Load())
}
