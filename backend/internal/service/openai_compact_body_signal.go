package service

import (
	"encoding/json"
	"github.com/tidwall/gjson"
)

// HasCompactionTriggerInInput detects the native Responses compaction signal.
func HasCompactionTriggerInInput(body []byte) bool {
	if len(body) == 0 || !gjson.GetBytes(body, "input").IsArray() {
		return false
	}
	found := false
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "compaction_trigger" {
			found = true
			return false
		}
		return true
	})
	return found
}

// forEachInputItem avoids exposing a JSON representation in the context API.
func forEachInputItem(body []byte, fn func(map[string]any) bool) {
	var envelope struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return
	}
	for _, item := range envelope.Input {
		if !fn(item) {
			return
		}
	}
}
