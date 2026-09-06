package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAITeamLinkedResolverSwitchDefaultsAndStickyFailure(t *testing.T) {
	repo := &settingUpdateRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	require.True(t, svc.IsOpenAITeamLinkedResolverEnabled(context.Background()))

	repo.values[SettingKeyOpenAITeamLinkedResolverEnabled] = "false"
	require.False(t, svc.IsOpenAITeamLinkedResolverEnabled(context.Background()))
	repo.getErr = errors.New("settings unavailable")
	require.False(t, svc.IsOpenAITeamLinkedResolverEnabled(context.Background()), "confirmed disabled state must remain sticky")

	first := NewSettingService(&settingUpdateRepoStub{getErr: errors.New("settings unavailable")}, &config.Config{})
	require.False(t, first.IsOpenAITeamLinkedResolverEnabled(context.Background()), "first unreadable state fails closed")
}

func TestOpenAITeamLinkedResolverChangeRequiresReason(t *testing.T) {
	repo := &settingUpdateRepoStub{values: map[string]string{SettingKeyOpenAITeamLinkedResolverEnabled: "true"}}
	svc := NewSettingService(repo, &config.Config{})
	current := true
	settings := &SystemSettings{CurrentOpenAITeamLinkedResolverEnabled: &current, OpenAITeamLinkedResolverEnabled: false}
	_, err := svc.buildSystemSettingsUpdates(context.Background(), settings)
	require.Error(t, err)
	require.Contains(t, err.Error(), "INVALID_OPENAI_TEAM_LINKED_CHANGE_REASON")
	settings.OpenAITeamLinkedChangeReason = "rollback Team linkage"
	_, err = svc.buildSystemSettingsUpdates(context.Background(), settings)
	require.NoError(t, err)
}
