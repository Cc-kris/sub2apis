package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func TestChannelMonitorModeValidation(t *testing.T) {
	for _, mode := range []string{MonitorModeActive, MonitorModePassive, MonitorModeQuota} {
		if err := validateMonitorMode(mode); err != nil {
			t.Fatalf("mode %q should be accepted: %v", mode, err)
		}
	}
	if got := defaultMonitorMode(""); got != MonitorModeActive {
		t.Fatalf("empty mode should default to active, got %q", got)
	}
	if err := validateMonitorMode("unknown"); !infraerrors.IsBadRequest(err) {
		t.Fatalf("unknown mode should return bad request, got %v", err)
	}
}

func TestChannelMonitorProviderModeValidation(t *testing.T) {
	for _, mode := range []string{MonitorModeActive, MonitorModePassive} {
		if err := validateProviderMode(MonitorProviderAntigravity, mode); err != ErrChannelMonitorProviderMode {
			t.Fatalf("antigravity mode %q should be rejected with provider-mode error, got %v", mode, err)
		}
		if got := infraerrors.Code(validateProviderMode(MonitorProviderAntigravity, mode)); got != http.StatusUnprocessableEntity {
			t.Fatalf("antigravity mode %q should return 422, got %d", mode, got)
		}
	}
	if err := validateProviderMode(MonitorProviderAntigravity, MonitorModeQuota); err != nil {
		t.Fatalf("antigravity quota mode should be accepted: %v", err)
	}
	if err := validateProviderMode(MonitorProviderKimi, MonitorModeActive); err != nil {
		t.Fatalf("kimi active mode should be accepted: %v", err)
	}
}

func TestChannelMonitorProviderValidationUsesUnprocessableEntity(t *testing.T) {
	err := validateProvider("unsupported")
	if err != ErrChannelMonitorInvalidProvider {
		t.Fatalf("unsupported provider should return the canonical error, got %v", err)
	}
	if got := infraerrors.Code(err); got != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported provider should return 422, got %d", got)
	}
}

func TestChannelMonitorRunnerSkipsPassiveAndQuotaProbes(t *testing.T) {
	for _, mode := range []string{MonitorModePassive, MonitorModeQuota} {
		t.Run(mode, func(t *testing.T) {
			svc := &modeTestMonitorSvc{runCalled: make(chan struct{}, 1)}
			runner := newChannelMonitorRunner(svc, nil)
			runner.Start()
			runner.Schedule(&ChannelMonitor{ID: 1, Name: "mode-test", Mode: mode, Enabled: true, IntervalSeconds: 15})
			defer runner.Stop()

			select {
			case <-svc.runCalled:
				t.Fatalf("mode %q must not trigger an active probe", mode)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

type modeTestMonitorSvc struct {
	runCalled chan struct{}
}

func (s *modeTestMonitorSvc) ListEnabledMonitors(context.Context) ([]*ChannelMonitor, error) {
	return nil, nil
}

func (s *modeTestMonitorSvc) RunCheck(context.Context, int64) ([]*CheckResult, error) {
	s.runCalled <- struct{}{}
	return nil, nil
}
