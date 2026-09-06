package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceTestCache struct{}

func (updateServiceTestCache) GetUpdateInfo(context.Context) (string, error) {
	return "", context.Canceled
}

func (updateServiceTestCache) SetUpdateInfo(context.Context, string, time.Duration) error {
	return nil
}

type updateServiceTestGitHubClient struct {
	repo string
}

func (c *updateServiceTestGitHubClient) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	c.repo = repo
	return &GitHubRelease{TagName: "v1.2.3", Name: "test release"}, nil
}

func (*updateServiceTestGitHubClient) DownloadFile(context.Context, string, string, int64) error {
	return nil
}

func (*updateServiceTestGitHubClient) FetchChecksumFile(context.Context, string) ([]byte, error) {
	return nil, nil
}

func TestUpdateServiceUsesProductionReleaseRepository(t *testing.T) {
	client := &updateServiceTestGitHubClient{}
	service := NewUpdateService(updateServiceTestCache{}, client, "1.0.0", "release")

	info, err := service.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.True(t, info.HasUpdate)
	require.Equal(t, "Cc-kris/cc2api", client.repo)
}
