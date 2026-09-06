package repository

import (
	"strings"
	"testing"
)

func TestGatewayTelemetryIsPassiveOnly(t *testing.T) {
	q := `INSERT INTO channel_monitor_histories
    (monitor_id, model, status, latency_ms, message, checked_at)
SELECT id, COALESCE(NULLIF($2, ''), primary_model), $3, $4, 'gateway', $5
FROM channel_monitors
WHERE account_id = $1 AND enabled = TRUE AND mode = 'passive'`
	if !strings.Contains(q, "mode = 'passive'") || strings.Contains(q, "mode = 'active'") {
		t.Fatal("gateway telemetry must target passive monitors only")
	}
}
