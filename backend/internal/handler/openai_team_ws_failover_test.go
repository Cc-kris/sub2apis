package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAITeamWorkspaceFailoverClassification(t *testing.T) {
	err := &service.UpstreamFailoverError{
		StatusCode:   http.StatusPaymentRequired,
		ResponseBody: []byte(`{"detail":{"code":"deactivated_workspace"}}`),
	}
	require.True(t, isOpenAITeamWorkspaceFailover(err))
	require.True(t, shouldSilentlySwitchOpenAIWSAccount(true, 0, 1, err, false, true))
	require.False(t, shouldSilentlySwitchOpenAIWSAccount(true, 0, 2, err, false, true), "Team 402 仅允许首轮切换一次")
	require.False(t, shouldSilentlySwitchOpenAIWSAccount(true, 1, 1, err, false, true), "已有完成的 WS turn 后不得切换")
	require.False(t, shouldSilentlySwitchOpenAIWSAccount(true, 0, 2, errors.New("ordinary websocket failure"), true, true), "Team 已触发过切换后不得再切到第三个账号")

	wrapped := errors.New("ordinary websocket failure")
	require.False(t, isOpenAITeamWorkspaceFailover(wrapped))
	require.False(t, shouldSilentlySwitchOpenAIWSAccount(true, 0, 1, wrapped, false, true))
	require.False(t, shouldSilentlySwitchOpenAIWSAccount(true, 0, 1, err, false, true, service.PlatformKimi))
}

func TestOpenAITeamWorkspaceFailoverRequiresExactPayload(t *testing.T) {
	for _, body := range []string{
		`{"detail":{"code":"workspace_deactivated"}}`,
		`{"error":{"code":"deactivated_workspace"}}`,
	} {
		err := &service.UpstreamFailoverError{StatusCode: http.StatusPaymentRequired, ResponseBody: []byte(body)}
		require.False(t, isOpenAITeamWorkspaceFailover(err))
	}
}
