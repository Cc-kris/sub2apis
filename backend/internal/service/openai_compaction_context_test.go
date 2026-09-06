package service

import (
	"net/http"
	"testing"
)

func TestParseOpenAICompactionContext(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		headers     http.Header
		path        string
		remote      bool
		trigger     string
		native      bool
		compact     bool
		sessionBeta string
	}{
		{
			name:   "body capability and trigger",
			body:   `{"remote_compaction_v2":true,"input":[{"type":"compaction_trigger","trigger":"threshold"}]}`,
			remote: true, trigger: "threshold", native: true, compact: true,
			sessionBeta: "responses=experimental,remote_compaction_v2",
		},
		{
			name:    "header capability",
			body:    `{"input":[{"type":"compaction_trigger"}]}`,
			headers: http.Header{"X-Codex-Beta-Features": []string{"foo, remote_compaction_v2=true"}},
			path:    "/v1/responses", remote: true, trigger: "compaction_trigger", native: true, compact: true,
			sessionBeta: "responses=experimental,remote_compaction_v2",
		},
		{
			name: "legacy compact path",
			path: "/v1/responses/compact/", compact: true,
			sessionBeta: "responses=experimental",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseOpenAICompactionContext([]byte(tt.body), tt.headers, tt.path)
			if got.RemoteCompactionV2 != tt.remote || got.CompactionTrigger != tt.trigger || got.NativeResponses != tt.native || got.Compact != tt.compact || got.SessionBeta != tt.sessionBeta {
				t.Fatalf("context = %+v", got)
			}
		})
	}
}

func TestHasCompactionTriggerInInput(t *testing.T) {
	if !HasCompactionTriggerInInput([]byte(`{"input":[{"type":"message"},{"type":"compaction_trigger"}]}`)) {
		t.Fatal("expected trigger")
	}
	if HasCompactionTriggerInInput([]byte(`{"input":[{"type":"message"}]}`)) {
		t.Fatal("did not expect trigger")
	}
}
