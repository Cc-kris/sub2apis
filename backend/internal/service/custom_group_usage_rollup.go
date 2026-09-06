package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GroupUsageRollupStore is the worker-facing subset of the rollup repository.
type GroupUsageRollupStore interface {
	GroupUsageSummaryProvider
	RegisterTimezone(context.Context, string) error
	ListRegisteredTimezones(context.Context) ([]string, error)
	Refresh(context.Context, string, time.Time) error
}

const defaultGroupUsageTimezone = "Asia/Shanghai"

const groupUsageRollupRuntimePollInterval = time.Second

var errGroupUsageRollupPaused = errors.New("group usage rollup is paused")

// CustomGroupUsageRollup owns the optional background rollup lifecycle. It is
// deliberately inert until Start is called, making tests and server shutdown safe.
type CustomGroupUsageRollup struct {
	store          GroupUsageRollupStore
	interval       time.Duration
	settingService *SettingService
	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	refreshMu      sync.Mutex
	refreshCancel  context.CancelFunc
	refreshDone    chan struct{}
	refreshEpoch   uint64
	pausedEpoch    uint64
	healthy        atomic.Bool
}

func NewCustomGroupUsageRollup(store GroupUsageRollupStore) *CustomGroupUsageRollup {
	worker := &CustomGroupUsageRollup{store: store, interval: time.Minute}
	worker.healthy.Store(false)
	return worker
}

func (w *CustomGroupUsageRollup) SetSettingService(settingService *SettingService) {
	if w != nil {
		w.settingService = settingService
		if settingService != nil {
			settingService.RegisterGroupUsageRollupListener(func(enabled bool) {
				if !enabled {
					w.cancelActiveRefreshAndWait()
				}
			})
		}
	}
}

func (w *CustomGroupUsageRollup) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil || w.store == nil {
		return
	}
	c, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		w.logRefreshError(w.refresh(c))
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.logRefreshError(w.refresh(c))
			case <-c.Done():
				return
			}
		}
	}()
}

func (w *CustomGroupUsageRollup) refresh(ctx context.Context) error {
	if w == nil || w.store == nil {
		return nil
	}
	refreshCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	epoch := w.setActiveRefresh(cancel, done)
	defer w.clearActiveRefresh(done)
	defer cancel()
	if w.settingService != nil && !w.settingService.IsGroupUsageRollupEnabled(refreshCtx) {
		w.healthy.Store(false)
		return errGroupUsageRollupPaused
	}

	stopPolling := make(chan struct{})
	defer close(stopPolling)
	if w.settingService != nil {
		go w.cancelRefreshWhenDisabled(refreshCtx, stopPolling)
	}
	if err := refreshCtx.Err(); err != nil {
		return err
	}

	if err := w.store.RegisterTimezone(refreshCtx, defaultGroupUsageTimezone); err != nil {
		return err
	}
	timezones, err := w.store.ListRegisteredTimezones(refreshCtx)
	if err != nil {
		return err
	}
	if len(timezones) == 0 {
		timezones = []string{defaultGroupUsageTimezone}
	}
	for _, timezone := range timezones {
		if err := w.store.Refresh(refreshCtx, timezone, time.Now()); err != nil {
			return err
		}
	}
	if err := refreshCtx.Err(); err != nil {
		return err
	}
	if w.settingService != nil {
		runtime := w.settingService.GetGroupUsageRollupRuntime(refreshCtx)
		if !runtime.Known || !runtime.Enabled {
			w.healthy.Store(false)
			return errGroupUsageRollupPaused
		}
	}
	// Commit a healthy read under the same lock used by the disable listener.
	// Once a disable operation acquires this lock, no older refresh can restore
	// health after that operation has marked the worker paused.
	w.markRefreshHealthy(done, epoch)
	return nil
}

// GetGroupUsageSummary prevents stale rollup data from being presented as
// healthy after a worker refresh failure. DashboardService then uses its
// existing realtime/degraded fallback.
func (w *CustomGroupUsageRollup) GetGroupUsageSummary(ctx context.Context, todayStart time.Time) ([]usagestats.GroupUsageSummary, error) {
	if w == nil || w.store == nil {
		return nil, errors.New("group usage rollup worker is unavailable")
	}
	if !w.healthy.Load() {
		return nil, errors.New("group usage rollup worker is unhealthy")
	}
	return w.store.GetGroupUsageSummary(ctx, todayStart)
}

func (w *CustomGroupUsageRollup) cancelRefreshWhenDisabled(ctx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(groupUsageRollupRuntimePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !w.settingService.IsGroupUsageRollupEnabled(ctx) {
				w.pauseActiveRefresh()
				return
			}
		case <-ctx.Done():
			return
		case <-stop:
			return
		}
	}
}

func (w *CustomGroupUsageRollup) setActiveRefresh(cancel context.CancelFunc, done chan struct{}) uint64 {
	w.refreshMu.Lock()
	w.refreshEpoch++
	w.refreshCancel = cancel
	w.refreshDone = done
	epoch := w.refreshEpoch
	w.refreshMu.Unlock()
	return epoch
}

func (w *CustomGroupUsageRollup) clearActiveRefresh(done chan struct{}) {
	w.refreshMu.Lock()
	if w.refreshDone == done {
		w.refreshCancel = nil
		w.refreshDone = nil
		close(done)
	}
	w.refreshMu.Unlock()
}

func (w *CustomGroupUsageRollup) cancelActiveRefreshAndWait() {
	_, done := w.pauseActiveRefresh()
	if done != nil {
		<-done
	}
}

// pauseActiveRefresh makes pausing and declaring the worker unhealthy one
// ordered operation. It is safe for the in-flight refresh's polling goroutine:
// callers that own that goroutine must not wait for done, because the refresh
// itself waits for polling to stop during its deferred cleanup.
func (w *CustomGroupUsageRollup) pauseActiveRefresh() (context.CancelFunc, chan struct{}) {
	w.refreshMu.Lock()
	w.healthy.Store(false)
	if w.refreshDone != nil {
		w.pausedEpoch = w.refreshEpoch
	}
	cancel, done := w.refreshCancel, w.refreshDone
	w.refreshMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return cancel, done
}

func (w *CustomGroupUsageRollup) markRefreshHealthy(done chan struct{}, epoch uint64) {
	w.refreshMu.Lock()
	if w.refreshDone == done && w.refreshEpoch == epoch && w.pausedEpoch != epoch {
		w.healthy.Store(true)
	}
	w.refreshMu.Unlock()
}

func (w *CustomGroupUsageRollup) logRefreshError(err error) {
	if err == nil {
		return
	}
	if err == errGroupUsageRollupPaused {
		w.healthy.Store(false)
		return
	}
	if err == context.Canceled {
		return
	}
	w.healthy.Store(false)
	slog.Error("group usage rollup refresh failed", "error", err)
}

func (w *CustomGroupUsageRollup) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel = nil
	w.done = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
