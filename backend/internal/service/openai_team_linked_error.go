package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const openAITeamBlockTTL = 15 * time.Minute

// isOpenAITeamWorkspaceDeactivated matches only the documented OpenAI Team
// workspace deactivation response. Other 402 payloads remain payment errors.
func IsOpenAITeamWorkspaceDeactivated(statusCode int, responseBody []byte) bool {
	return statusCode == http.StatusPaymentRequired &&
		gjson.GetBytes(responseBody, "detail.code").String() == "deactivated_workspace"
}

func openAITeamBlockRequestID(headers http.Header, teamID string, accountID int64, responseBody []byte) string {
	if requestID := strings.TrimSpace(headers.Get("x-request-id")); requestID != "" {
		return requestID
	}
	// Some upstream 402s lack a request ID. Keep retries within the block window
	// idempotent while allowing a later, independent outage to create new audit.
	sum := sha256.Sum256(append([]byte(teamID), responseBody...))
	window := time.Now().UTC().Unix() / int64(openAITeamBlockTTL.Seconds())
	return fmt.Sprintf("local-%d-%d-%x", accountID, window, sum[:8])
}
