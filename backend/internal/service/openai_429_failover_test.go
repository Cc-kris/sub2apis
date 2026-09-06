package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TASK-004 locks the shared HTTP/WS retry contract: one same-account retry is
// allowed for a retryable upstream failure, then the state is consumed.
func TestOpenAIHTTPAndWS429FailoverIsSingleShot(t *testing.T) {
	svc := &OpenAIGatewayService{}
	err := &UpstreamFailoverError{StatusCode: 429, RetryableOnSameAccount: true}
	require.True(t, svc.shouldRetryOpenAIHTTPStreamFailover(7, err))
	require.False(t, svc.shouldRetryOpenAIHTTPStreamFailover(7, err))
	require.False(t, svc.shouldRetryOpenAIHTTPStreamFailover(7, errors.New("plain error")))
}
