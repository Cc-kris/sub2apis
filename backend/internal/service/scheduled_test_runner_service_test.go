package service

import "testing"

func TestShouldSkipScheduledTeamTestBeforeDueProbe(t *testing.T) {
	if !shouldSkipScheduledTeamTest(true, nil) {
		t.Fatal("an active Team block without a claimed probe must skip ordinary scheduled testing")
	}
	if shouldSkipScheduledTeamTest(true, &OpenAITeamProbeLease{EventID: 1, TeamID: "team-a"}) {
		t.Fatal("a claimed Team probe must run")
	}
	if shouldSkipScheduledTeamTest(false, nil) {
		t.Fatal("an account without an active Team block must keep ordinary testing")
	}
}
